// Package store is the Postgres layer: keys, usage rows, monthly spend and cost reservations.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/keys"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Any constant works; it only has to be the same for every `cafe-gateway migrate`.
const migrateLockID = 0x63616665 // "cafe"

var ErrNotFound = errors.New("not found")

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrations returns [(version, sql)] sorted by file name.
func Migrations() ([][2]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, [2]string{strings.TrimSuffix(e.Name(), ".sql"), string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out, nil
}

// Migrate applies pending migrations, each in its own transaction, and returns their versions. A session advisory
// lock makes concurrent runs (two pods' init containers) safe: the second waits, then finds nothing to do.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	files, err := Migrations()
	if err != nil {
		return nil, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockID); err != nil {
		return nil, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockID) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	applied := []string{}
	for _, f := range files {
		version, sql := f[0], f[1]
		if contains(done, version) {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}

func contains(items []string, s string) bool {
	for _, i := range items {
		if i == s {
			return true
		}
	}
	return false
}

// --- keys ---

type APIKey struct {
	ID                  string
	KeyID               string
	Service             config.Service
	KeyHash             string
	Name                string
	Scopes              []string
	RPM                 int
	MaxTokensPerRequest int
	MonthlyBudgetUSD    decimal.Decimal
	CreatedAt           time.Time
	LastUsedAt          *time.Time
	RevokedAt           *time.Time
}

func (k *APIKey) HasScope(scope string) bool { return contains(k.Scopes, scope) }

// KeyJSON is the public view of a key: never the hash or the plaintext.
type KeyJSON struct {
	KeyID               string     `json:"key_id"`
	Prefix              string     `json:"prefix"`
	Service             string     `json:"service"`
	Name                string     `json:"name"`
	Scopes              []string   `json:"scopes"`
	RPM                 int        `json:"rpm"`
	MaxTokensPerRequest int        `json:"max_tokens_per_request"`
	MonthlyBudgetUSD    string     `json:"monthly_budget_usd"`
	CreatedAt           time.Time  `json:"created_at"`
	LastUsedAt          *time.Time `json:"last_used_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
}

func (k *APIKey) JSON() KeyJSON {
	return KeyJSON{
		KeyID: k.KeyID, Prefix: keys.DisplayPrefix(k.Service, k.KeyID), Service: string(k.Service), Name: k.Name,
		Scopes: k.Scopes, RPM: k.RPM, MaxTokensPerRequest: k.MaxTokensPerRequest,
		MonthlyBudgetUSD: k.MonthlyBudgetUSD.String(), CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
		RevokedAt: k.RevokedAt,
	}
}

const keyColumns = `id::text, key_id, service, key_hash, name, scopes, rpm, max_tokens_per_request,
	monthly_budget_usd::text, created_at, last_used_at, revoked_at`

func scanKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	var service, budget string
	err := row.Scan(&k.ID, &k.KeyID, &service, &k.KeyHash, &k.Name, &k.Scopes, &k.RPM, &k.MaxTokensPerRequest,
		&budget, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.Service = config.Service(service)
	k.MonthlyBudgetUSD = decimal.RequireFromString(budget)
	return &k, nil
}

type NewKey struct {
	Service             config.Service
	Name                string
	RPM                 int
	MaxTokensPerRequest int
	MonthlyBudgetUSD    decimal.Decimal
}

// CreateKey stores a new key and returns it with its plaintext, which is never stored.
func (s *Store) CreateKey(ctx context.Context, nk NewKey) (*APIKey, string, error) {
	nk.Name = strings.TrimSpace(nk.Name)
	switch {
	case nk.Name == "" || len(nk.Name) > 128:
		return nil, "", errors.New("name must be 1-128 characters")
	case nk.RPM < 1 || nk.MaxTokensPerRequest < 1:
		return nil, "", errors.New("rpm and max tokens per request must be at least 1")
	case nk.MonthlyBudgetUSD.IsNegative():
		return nil, "", errors.New("the monthly budget can't be negative")
	}
	keyID, plaintext := keys.Generate(nk.Service)
	row := s.Pool.QueryRow(ctx, `INSERT INTO api_keys
		(key_id, service, key_hash, name, scopes, rpm, max_tokens_per_request, monthly_budget_usd)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric) RETURNING `+keyColumns,
		keyID, string(nk.Service), keys.Hash(plaintext), nk.Name, nk.Service.Scopes(), nk.RPM,
		nk.MaxTokensPerRequest, nk.MonthlyBudgetUSD.String())
	key, err := scanKey(row)
	if err != nil {
		return nil, "", err
	}
	return key, plaintext, nil
}

func (s *Store) KeyByKeyID(ctx context.Context, keyID string) (*APIKey, error) {
	return scanKey(s.Pool.QueryRow(ctx, "SELECT "+keyColumns+" FROM api_keys WHERE key_id = $1", keys.LookupID(keyID)))
}

func (s *Store) ListKeys(ctx context.Context, service config.Service) ([]*APIKey, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+keyColumns+` FROM api_keys
		WHERE $1 = '' OR service = $1 ORDER BY created_at`, string(service))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey is idempotent: revoking twice keeps the first revoked_at.
func (s *Store) RevokeKey(ctx context.Context, keyID string) (*APIKey, error) {
	return scanKey(s.Pool.QueryRow(ctx, `UPDATE api_keys SET revoked_at = coalesce(revoked_at, now())
		WHERE key_id = $1 RETURNING `+keyColumns, keys.LookupID(keyID)))
}

// --- usage, spend and reservations ---

// MonthBounds returns (YYYY-MM, start, end) in UTC. An empty month means the month containing now.
func MonthBounds(month string, now time.Time) (string, time.Time, time.Time, error) {
	var start time.Time
	if month == "" {
		now = now.UTC()
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		t, err := time.Parse("2006-01", month)
		if err != nil || len(month) != 7 {
			return "", time.Time{}, time.Time{}, errors.New("month must be YYYY-MM")
		}
		start = t.UTC()
	}
	return start.Format("2006-01"), start, start.AddDate(0, 1, 0), nil
}

type UsageRow struct {
	APIKeyID     string
	Service      config.Service
	Endpoint     string
	Surface      string // "" = not sent
	AgentID      string
	Model        string
	InputTokens  int
	OutputTokens int
	CostUSD      decimal.Decimal
	CostSource   string
	LatencyMS    int
	StatusCode   int
	ErrorCode    string
	UpstreamID   string
	RequestBody  string // only with BARISTA_DEBUG_STORE_BODIES
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertUsage(ctx context.Context, db execer, u UsageRow) error {
	if u.CostSource == "" {
		u.CostSource = "none"
	}
	_, err := db.Exec(ctx, `INSERT INTO usage (api_key_id, service, endpoint, surface, agent_id, model, input_tokens,
		output_tokens, cost_usd, cost_source, latency_ms, status_code, error_code, upstream_id, request_body)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13, $14, $15)`,
		u.APIKeyID, string(u.Service), u.Endpoint, nullable(u.Surface), nullable(u.AgentID), nullable(u.Model),
		u.InputTokens, u.OutputTokens, u.CostUSD.String(), u.CostSource, u.LatencyMS, u.StatusCode,
		nullable(u.ErrorCode), nullable(u.UpstreamID), nullable(u.RequestBody))
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, "UPDATE api_keys SET last_used_at = now() WHERE id = $1::uuid", u.APIKeyID)
	return err
}

// RecordUsage logs a request that never reached OpenRouter (no reservation to settle).
func (s *Store) RecordUsage(ctx context.Context, u UsageRow) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return insertUsage(ctx, tx, u) })
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func sumDecimal(ctx context.Context, db querier, sql string, args ...any) (decimal.Decimal, error) {
	var s string
	if err := db.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(s)
}

// spend returns the recorded spend this month for a service (keyID "") or one key.
func spend(ctx context.Context, db querier, service config.Service, keyID string, now time.Time) (decimal.Decimal, error) {
	_, start, end, _ := MonthBounds("", now)
	if keyID != "" {
		return sumDecimal(ctx, db, `SELECT coalesce(sum(cost_usd), 0)::text FROM usage
			WHERE api_key_id = $1::uuid AND created_at >= $2 AND created_at < $3`, keyID, start, end)
	}
	return sumDecimal(ctx, db, `SELECT coalesce(sum(cost_usd), 0)::text FROM usage
		WHERE service = $1 AND created_at >= $2 AND created_at < $3`, string(service), start, end)
}

// reserved returns the unexpired reservations for a service (keyID "") or one key.
func reserved(ctx context.Context, db querier, service config.Service, keyID string, now time.Time) (decimal.Decimal, error) {
	if keyID != "" {
		return sumDecimal(ctx, db, `SELECT coalesce(sum(usd), 0)::text FROM reservations
			WHERE api_key_id = $1::uuid AND expires_at > $2`, keyID, now)
	}
	return sumDecimal(ctx, db, `SELECT coalesce(sum(usd), 0)::text FROM reservations
		WHERE service = $1 AND expires_at > $2`, string(service), now)
}

// serviceLockID is the transaction advisory lock that serializes budget checks and settlements for one service.
func serviceLockID(service config.Service) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("cafe-gateway/spend/" + string(service)))
	return int64(h.Sum32())
}

// Snapshot is a budget's state at the moment of a check.
type Snapshot struct {
	Limit    decimal.Decimal
	Spent    decimal.Decimal
	Reserved decimal.Decimal
}

func (s Snapshot) Remaining() decimal.Decimal { return s.Limit.Sub(s.Spent).Sub(s.Reserved) }

// OverLimitError says which limit a reservation would cross.
type OverLimitError struct {
	// "key" or "service".
	Scope    string
	Snapshot Snapshot
}

func (e *OverLimitError) Error() string { return e.Scope + " budget exceeded" }

type Reservation struct {
	ID      int64
	USD     decimal.Decimal
	Key     Snapshot
	Service Snapshot
}

// Reserve checks that spent + in-flight reservations + estimate stays within both the key's monthly budget and
// the service's monthly cap, then records a reservation for the estimate. The service-wide advisory lock means
// concurrent requests on any replica see each other's reservations.
func (s *Store) Reserve(ctx context.Context, key *APIKey, serviceCap, estimate decimal.Decimal,
	ttl time.Duration, now time.Time,
) (*Reservation, error) {
	var res *Reservation
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", serviceLockID(key.Service)); err != nil {
			return err
		}
		snap := func(limit decimal.Decimal, keyID string) (Snapshot, error) {
			spent, err := spend(ctx, tx, key.Service, keyID, now)
			if err != nil {
				return Snapshot{}, err
			}
			held, err := reserved(ctx, tx, key.Service, keyID, now)
			return Snapshot{Limit: limit, Spent: spent, Reserved: held}, err
		}
		keySnap, err := snap(key.MonthlyBudgetUSD, key.ID)
		if err != nil {
			return err
		}
		if estimate.GreaterThan(keySnap.Remaining()) {
			return &OverLimitError{Scope: "key", Snapshot: keySnap}
		}
		svcSnap, err := snap(serviceCap, "")
		if err != nil {
			return err
		}
		if estimate.GreaterThan(svcSnap.Remaining()) {
			return &OverLimitError{Scope: "service", Snapshot: svcSnap}
		}
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO reservations (service, api_key_id, usd, expires_at)
			VALUES ($1, $2::uuid, $3::numeric, $4) RETURNING id`,
			string(key.Service), key.ID, estimate.String(), now.Add(ttl)).Scan(&id)
		res = &Reservation{ID: id, USD: estimate, Key: keySnap, Service: svcSnap}
		return err
	})
	return res, err
}

// Settlement is the recorded spend just before and just after a settled request.
type Settlement struct {
	ServiceBefore, ServiceAfter decimal.Decimal
	KeyBefore, KeyAfter         decimal.Decimal
}

// Settle replaces a reservation with the request's usage row (its real cost) in one transaction, under the same
// lock as Reserve, so Before/After are exact and each threshold crossing is seen by exactly one request.
func (s *Store) Settle(ctx context.Context, reservationID int64, u UsageRow, now time.Time) (Settlement, error) {
	var out Settlement
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", serviceLockID(u.Service)); err != nil {
			return err
		}
		var err error
		if out.ServiceBefore, err = spend(ctx, tx, u.Service, "", now); err != nil {
			return err
		}
		if out.KeyBefore, err = spend(ctx, tx, u.Service, u.APIKeyID, now); err != nil {
			return err
		}
		if err := insertUsage(ctx, tx, u); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM reservations WHERE id = $1", reservationID)
		return err
	})
	out.ServiceAfter = out.ServiceBefore.Add(u.CostUSD)
	out.KeyAfter = out.KeyBefore.Add(u.CostUSD)
	return out, err
}

// ServiceSpend returns this month's recorded spend and live reservations for a service.
func (s *Store) ServiceSpend(ctx context.Context, service config.Service, now time.Time) (decimal.Decimal, decimal.Decimal, error) {
	spent, err := spend(ctx, s.Pool, service, "", now)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	held, err := reserved(ctx, s.Pool, service, "", now)
	return spent, held, err
}

// KeySpend returns this month's recorded spend for one key.
func (s *Store) KeySpend(ctx context.Context, key *APIKey, now time.Time) (decimal.Decimal, error) {
	return spend(ctx, s.Pool, key.Service, key.ID, now)
}

// PruneReservations deletes expired reservations (they no longer count anyway).
func (s *Store) PruneReservations(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, "DELETE FROM reservations WHERE expires_at <= $1", now)
	return tag.RowsAffected(), err
}
