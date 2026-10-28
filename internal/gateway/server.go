package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
)

var monthRE = regexp.MustCompile(`^\d{4}-\d{2}$`)

// Handler is the whole HTTP API for this deployment's service. The other service's endpoint is not registered,
// so a barista deployment answers 404 on /v1/embed and vice versa.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", g.health)
	mux.HandleFunc("GET /ready", g.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.Metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /docs", g.docs)
	mux.HandleFunc("GET /openapi.json", g.openapi)
	mux.HandleFunc("POST "+g.service().Endpoint(), g.modelEndpoint)
	mux.HandleFunc("GET /v1/usage", g.ownUsage)
	mux.HandleFunc("POST /v1/admin/keys", g.admin(g.adminCreateKey))
	mux.HandleFunc("GET /v1/admin/keys", g.admin(g.adminListKeys))
	mux.HandleFunc("POST /v1/admin/keys/{key_id}/revoke", g.admin(g.adminRevokeKey))
	mux.HandleFunc("GET /v1/admin/usage", g.admin(g.adminUsage))
	mux.HandleFunc("GET /v1/admin/spend", g.admin(g.adminSpend))
	mux.HandleFunc("/", notFound)
	return g.recoverer(g.logRequests(g.hostGuard(mux)))
}

// --- middleware ---

type requestInfo struct {
	keyID, surface, agent, code string
}

type infoKey struct{}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(infoKey{}).(*requestInfo)
	return info
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests writes one JSON log line per request. Never bodies, keys or tokens.
func (g *Gateway) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		info := &requestInfo{}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
		elapsed := time.Since(started)
		if r.URL.Path == "/health" || r.URL.Path == "/ready" || r.URL.Path == "/metrics" {
			return
		}
		if r.URL.Path == g.service().Endpoint() {
			code := info.code
			if code == "" {
				code = "ok"
			}
			g.Metrics.Requests.WithLabelValues(string(g.service()), strconv.Itoa(sw.status), code).Inc()
			g.Metrics.Duration.WithLabelValues(string(g.service())).Observe(elapsed.Seconds())
		}
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration_ms", elapsed.Milliseconds(), "service", string(g.service()), "key_id", info.keyID,
			"surface", info.surface, "agent_id", info.agent, "error_code", info.code)
	})
}

func (g *Gateway) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				slog.ErrorContext(r.Context(), "panic", "value", v, "stack", string(debug.Stack()))
				writeError(w, r, apiError(http.StatusInternalServerError, "internal_error", "Internal error."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func hostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.Split(h, ",")[0]))
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

// hostGuard answers 404 unless Host (and X-Forwarded-Host, when present) is one of BARISTA_ALLOWED_HOSTS.
// X-Forwarded-Host can only narrow access. Probes and scrapes by pod IP still reach /health, /ready and /metrics.
func (g *Gateway) hostGuard(next http.Handler) http.Handler {
	allowed := g.Cfg.AllowedHosts
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/ready", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		ok := slices.Contains(allowed, hostname(r.Host))
		if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
			ok = ok && slices.Contains(allowed, hostname(fwd))
		}
		if !ok {
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- public endpoints ---

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": g.service()})
}

// ready fails while Postgres is unreachable, so Kubernetes stops routing to the pod.
func (g *Gateway) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := g.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "database": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "database": "ok", "service": g.service()})
}

func (g *Gateway) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *APIError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.Cfg.MaxBodyBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, apiError(http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
	}
	if err != nil {
		return nil, invalidRequest("Could not read the request body.")
	}
	return body, nil
}

func (g *Gateway) modelEndpoint(w http.ResponseWriter, r *http.Request) {
	key, e := g.authenticate(r.Context(), r.Header)
	if e != nil {
		writeError(w, r, e)
		return
	}
	body, e := g.readBody(w, r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	res, e := g.handleModelCall(r.Context(), key, r.Header, body)
	if e != nil {
		writeError(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cost-Usd", res.cost.StringFixed(8))
	w.Header().Set("X-Budget-Remaining-Usd", res.budgetRemaining.StringFixed(6))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.body)
}

var ownUsageGroups = []string{"surface", "agent", "model", "day", "status"}

// ownUsage reports this month's (or ?month=) usage for the calling key.
func (g *Gateway) ownUsage(w http.ResponseWriter, r *http.Request) {
	key, e := g.authenticate(r.Context(), r.Header)
	if e != nil {
		writeError(w, r, e)
		return
	}
	if !key.HasScope("usage") {
		writeError(w, r, apiError(http.StatusForbidden, "insufficient_scope", "This key lacks the 'usage' scope."))
		return
	}
	q := r.URL.Query()
	if by := q.Get("by"); by != "" && !slices.Contains(ownUsageGroups, by) {
		writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", "by must be one of "+strings.Join(ownUsageGroups, ", ")))
		return
	}
	g.writeReport(w, r, store.UsageFilter{Month: q.Get("month"), APIKeyID: key.ID}, q.Get("by"), map[string]any{"key": key.JSON()})
}

func (g *Gateway) writeReport(w http.ResponseWriter, r *http.Request, f store.UsageFilter, by string, extra map[string]any) {
	if f.Month != "" && !monthRE.MatchString(f.Month) {
		writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", "month must be YYYY-MM"))
		return
	}
	if by != "" && !slices.Contains(store.GroupBys, by) {
		writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", "by must be one of "+strings.Join(store.GroupBys, ", ")))
		return
	}
	report, err := g.Store.Summarize(r.Context(), f, by, g.Now())
	if err != nil {
		if strings.HasPrefix(err.Error(), "month must") {
			writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", err.Error()))
			return
		}
		slog.ErrorContext(r.Context(), "usage summary failed", "error", err)
		writeError(w, r, storeUnavailable())
		return
	}
	out := map[string]any{}
	raw, _ := json.Marshal(report)
	_ = json.Unmarshal(raw, &out)
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// --- admin (Authorization: Bearer <BARISTA_ADMIN_TOKEN>) ---

func (g *Gateway) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.Cfg.AdminToken == "" {
			writeError(w, r, apiError(http.StatusServiceUnavailable, "admin_disabled", "BARISTA_ADMIN_TOKEN is not set, so the admin API is off."))
			return
		}
		token := bearer(r.Header)
		if subtle.ConstantTimeCompare([]byte(token), []byte(g.Cfg.AdminToken)) != 1 {
			writeError(w, r, apiError(http.StatusUnauthorized, "invalid_admin_token", "Send 'Authorization: Bearer <BARISTA_ADMIN_TOKEN>'.").
				header("WWW-Authenticate", "Bearer"))
			return
		}
		if info := requestInfoFrom(r.Context()); info != nil {
			info.keyID = "admin"
		}
		next(w, r)
	}
}

type createKeyRequest struct {
	Service             string           `json:"service"`
	Name                string           `json:"name"`
	RPM                 *int             `json:"rpm"`
	MaxTokensPerRequest *int             `json:"max_tokens_per_request"`
	MonthlyBudgetUSD    *decimal.Decimal `json:"monthly_budget_usd"`
}

// adminCreateKey returns the full key once; it is never retrievable again.
func (g *Gateway) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	body, e := g.readBody(w, r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var req createKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, r, invalidRequest("Body must be {service, name, rpm?, max_tokens_per_request?, monthly_budget_usd?}."))
		return
	}
	service, err := config.ParseService(req.Service)
	if err != nil {
		writeError(w, r, invalidRequest(err.Error()))
		return
	}
	nk := store.NewKey{
		Service: service, Name: req.Name, RPM: g.Cfg.DefaultRPM,
		MaxTokensPerRequest: g.Cfg.DefaultMaxTokensPerRequest, MonthlyBudgetUSD: g.Cfg.DefaultMonthlyBudget,
	}
	if req.RPM != nil {
		nk.RPM = *req.RPM
	}
	if req.MaxTokensPerRequest != nil {
		nk.MaxTokensPerRequest = *req.MaxTokensPerRequest
	}
	if req.MonthlyBudgetUSD != nil {
		nk.MonthlyBudgetUSD = *req.MonthlyBudgetUSD
	}
	key, plaintext, err := g.Store.CreateKey(r.Context(), nk)
	if err != nil {
		if strings.Contains(err.Error(), "must") || strings.Contains(err.Error(), "can't") {
			writeError(w, r, invalidRequest(err.Error()))
			return
		}
		slog.ErrorContext(r.Context(), "create key failed", "error", err)
		writeError(w, r, storeUnavailable())
		return
	}
	slog.InfoContext(r.Context(), "key created", "key_id", key.KeyID, "service", string(key.Service), "name", key.Name)
	out := map[string]any{"api_key": plaintext}
	raw, _ := json.Marshal(key.JSON())
	_ = json.Unmarshal(raw, &out)
	out["api_key"] = plaintext
	writeJSON(w, http.StatusCreated, out)
}

func (g *Gateway) adminListKeys(w http.ResponseWriter, r *http.Request) {
	var service config.Service
	if s := r.URL.Query().Get("service"); s != "" {
		var err error
		if service, err = config.ParseService(s); err != nil {
			writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", err.Error()))
			return
		}
	}
	list, err := g.Store.ListKeys(r.Context(), service)
	if err != nil {
		slog.ErrorContext(r.Context(), "list keys failed", "error", err)
		writeError(w, r, storeUnavailable())
		return
	}
	out := make([]store.KeyJSON, 0, len(list))
	for _, k := range list {
		out = append(out, k.JSON())
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	key, err := g.Store.RevokeKey(r.Context(), r.PathValue("key_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, apiError(http.StatusNotFound, "not_found", "No key '"+r.PathValue("key_id")+"'."))
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "revoke key failed", "error", err)
		writeError(w, r, storeUnavailable())
		return
	}
	slog.InfoContext(r.Context(), "key revoked", "key_id", key.KeyID, "service", string(key.Service), "name", key.Name)
	writeJSON(w, http.StatusOK, key.JSON())
}

func (g *Gateway) adminUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.UsageFilter{Month: q.Get("month"), Surface: q.Get("surface"), AgentID: q.Get("agent")}
	if s := q.Get("service"); s != "" {
		service, err := config.ParseService(s)
		if err != nil {
			writeError(w, r, apiError(http.StatusBadRequest, "invalid_request", err.Error()))
			return
		}
		f.Service = string(service)
	}
	if k := q.Get("key"); k != "" {
		key, err := g.Store.KeyByKeyID(r.Context(), k)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, apiError(http.StatusNotFound, "not_found", "No key '"+k+"'."))
			return
		}
		if err != nil {
			writeError(w, r, storeUnavailable())
			return
		}
		f.APIKeyID = key.ID
	}
	g.writeReport(w, r, f, q.Get("by"), nil)
}

// adminSpend shows each service's month spend against its cap.
func (g *Gateway) adminSpend(w http.ResponseWriter, r *http.Request) {
	now := g.Now()
	month, _, _, _ := store.MonthBounds("", now)
	services := map[string]any{}
	for _, s := range config.Services {
		spent, held, err := g.Store.ServiceSpend(r.Context(), s, now)
		if err != nil {
			slog.ErrorContext(r.Context(), "spend query failed", "error", err)
			writeError(w, r, storeUnavailable())
			return
		}
		cap := g.Cfg.MonthlyCap(s)
		services[string(s)] = map[string]any{
			"spent_usd": spent.StringFixed(8), "reserved_usd": held.StringFixed(8), "cap_usd": cap.String(),
			"remaining_usd": decimal.Max(decimal.Zero, cap.Sub(spent).Sub(held)).StringFixed(8),
			"status":        capStatus(spent, cap),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"month": month, "services": services})
}
