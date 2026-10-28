package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/openrouter"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
	"github.com/daniel-grinevich/cafe-gateway/internal/testdb"
)

const (
	decisionsPath  = "/api/alpha/decisions"
	embeddingsPath = "/api/v1/embeddings"
)

func expect(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	if code != "" {
		if got := r.errorCode(t); got != code {
			t.Fatalf("error code = %q, want %q: %s", got, code, r.body)
		}
	}
}

// --- happy paths ---

func TestDecideForwardsToOpenRouterAndLogsUsage(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	r := e.post("/v1/decide", decide, caller(key))
	expect(t, r, 200, "")
	if r.json(t)["id"] != "dec-1" {
		t.Fatalf("body not passed through: %s", r.body)
	}
	if got := r.header.Get("X-Cost-Usd"); got != "0.00040000" {
		t.Fatalf("X-Cost-Usd = %q", got)
	}
	if got := r.header.Get("X-Budget-Remaining-Usd"); got != "0.999600" {
		t.Fatalf("X-Budget-Remaining-Usd = %q", got)
	}

	sent := e.upstream.last(decisionsPath)
	if sent["model"] != "perplexity/pplx-decider-flash-v1.2" {
		t.Fatalf("model = %v", sent["model"])
	}
	if p, _ := sent["provider"].(map[string]any); len(p) != 1 || p["data_collection"] != "deny" {
		t.Fatalf("provider = %v", sent["provider"])
	}
	if u, _ := sent["user"].(string); !strings.HasPrefix(u, "barista:") {
		t.Fatalf("user = %v", sent["user"])
	}
	if _, ok := sent["questions"].(map[string]any)["ship"]; !ok {
		t.Fatalf("questions not forwarded: %v", sent)
	}
	h := e.upstream.headers[decisionsPath][0]
	if h.Get("Authorization") != "Bearer sk-or-test" || h.Get("X-Title") != "cafe-gateway" {
		t.Fatalf("upstream headers = %v", h)
	}

	surfaces := e.usageBy("surface", "")
	if surfaces["slack"]["requests"] != 1.0 || surfaces["slack"]["cost_usd"] != "0.00040000" {
		t.Fatalf("by surface = %v", surfaces)
	}
	if e.usageBy("agent", "")["alices-bot"]["input_tokens"] != 120.0 {
		t.Fatal("agent grouping lost input tokens")
	}
	if e.scalar("SELECT cost_source FROM usage") != "openrouter" {
		t.Fatal("cost_source should be openrouter")
	}
}

func TestEmbedForwardsToOpenRouter(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	r := e.post("/v1/embed", embed, caller(key, "internal", "internal"))
	expect(t, r, 200, "")
	sent := e.upstream.last(embeddingsPath)
	if sent["model"] != "qwen/qwen3-embedding-8b" || len(sent["input"].([]any)) != 2 {
		t.Fatalf("sent = %v", sent)
	}
	if sent["provider"].(map[string]any)["data_collection"] != "deny" {
		t.Fatal("data_collection not forced")
	}
	if e.usageBy("surface", "&service=grinder")["internal"]["input_tokens"] != 5.0 {
		t.Fatal("prompt_tokens not recorded")
	}
}

func TestCallerCannotTurnDataCollectionBackOn(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	body := map[string]any{"input": "x", "provider": map[string]any{"data_collection": "allow", "order": []string{"nebius"}}}
	expect(t, e.post("/v1/embed", body, caller(key)), 200, "")
	p := e.upstream.last(embeddingsPath)["provider"].(map[string]any)
	if p["data_collection"] != "deny" || len(p["order"].([]any)) != 1 {
		t.Fatalf("provider = %v", p)
	}
}

// --- service isolation and keys ---

func TestEachDeploymentServesOnlyItsEndpoint(t *testing.T) {
	st, up := testdb.New(t), newMockOpenRouter(t)
	bar := newEnvShared(t, config.Barista, st, up)
	grd := newEnvShared(t, config.Grinder, st, up)
	barKey, grdKey := bar.makeKey(config.Barista, nil), bar.makeKey(config.Grinder, nil)

	expect(t, bar.post("/v1/embed", embed, caller(grdKey)), 404, "not_found")
	expect(t, grd.post("/v1/decide", decide, caller(barKey)), 404, "not_found")
	expect(t, bar.post("/v1/decide", decide, caller(barKey)), 200, "")
	expect(t, grd.post("/v1/embed", embed, caller(grdKey)), 200, "")
	for _, e := range []*env{bar, grd} {
		expect(t, e.do("GET", "/health", nil, nil), 200, "")
		expect(t, e.do("GET", "/docs", nil, nil), 200, "")
	}
}

func TestKeysAreScopedPerService(t *testing.T) {
	st, up := testdb.New(t), newMockOpenRouter(t)
	bar := newEnvShared(t, config.Barista, st, up)
	grd := newEnvShared(t, config.Grinder, st, up)
	barKey, grdKey := bar.makeKey(config.Barista, nil), bar.makeKey(config.Grinder, nil)
	expect(t, bar.post("/v1/decide", decide, caller(grdKey)), 403, "wrong_service")
	expect(t, grd.post("/v1/embed", embed, caller(barKey)), 403, "wrong_service")
	// A barista key can't read its usage through grinder either.
	expect(t, grd.do("GET", "/v1/usage", nil, caller(barKey)), 403, "wrong_service")
	if up.callCount(decisionsPath)+up.callCount(embeddingsPath) != 0 {
		t.Fatal("upstream called with a wrong-service key")
	}
}

func TestBadKeysAre401(t *testing.T) {
	e := newEnv(t, config.Barista)
	headers := map[string]string{"X-AGENT-SURFACE": "internal", "X-AGENT-ID": "internal"}
	expect(t, e.post("/v1/decide", decide, headers), 401, "missing_api_key")
	expect(t, e.post("/v1/decide", decide, merge(headers, map[string]string{"Authorization": "Bearer nope"})), 401, "invalid_api_key")
	unknown := "Bearer bar_aaaaaaaaaaaa_" + strings.Repeat("x", 43)
	expect(t, e.post("/v1/decide", decide, merge(headers, map[string]string{"Authorization": unknown})), 401, "invalid_api_key")
}

func TestWrongSecretForRealKeyIDIs401(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	forged := key[:len("bar_")+12] + "_" + strings.Repeat("y", 43)
	expect(t, e.post("/v1/decide", decide, caller(forged)), 401, "invalid_api_key")
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	r := e.do("POST", "/v1/admin/keys/"+strings.Split(key, "_")[1]+"/revoke", nil, adminHeaders())
	expect(t, r, 200, "")
	if r.json(t)["revoked_at"] == nil {
		t.Fatal("revoked_at not set")
	}
	expect(t, e.post("/v1/decide", decide, caller(key)), 401, "revoked_api_key")
	expect(t, e.do("GET", "/v1/usage", nil, caller(key)), 401, "revoked_api_key")
	expect(t, e.do("POST", "/v1/admin/keys/nosuchkey/revoke", nil, adminHeaders()), 404, "not_found")
}

func TestKeyIsStoredHashed(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	if !strings.HasPrefix(key, "grd_") {
		t.Fatalf("key = %q", key)
	}
	r := e.do("GET", "/v1/admin/keys", nil, adminHeaders())
	if bytes.Contains(r.body, []byte(key)) || bytes.Contains(r.body, []byte(key[17:])) {
		t.Fatal("listing leaks the key")
	}
	keyID := strings.Split(key, "_")[1]
	if !bytes.Contains(r.body, []byte(`"prefix":"grd_`+keyID+`_…"`)) {
		t.Fatalf("listing = %s", r.body)
	}
	if e.scalar("SELECT key_hash FROM api_keys") == key || e.scalar("SELECT count(*)::text FROM api_keys WHERE key_hash LIKE '%' || $1 || '%'", key[17:]) != "0" {
		t.Fatal("plaintext stored")
	}
}

// --- attribution headers ---

func TestMissingAttributionHeaderIs400(t *testing.T) {
	for _, missing := range []string{"X-AGENT-SURFACE", "X-AGENT-ID"} {
		t.Run(missing, func(t *testing.T) {
			e := newEnv(t, config.Barista)
			key := e.makeKey(config.Barista, nil)
			h := caller(key)
			delete(h, missing)
			r := e.post("/v1/decide", decide, h)
			expect(t, r, 400, "missing_header")
			errBody := r.json(t)["error"].(map[string]any)
			if m := errBody["headers_missing"].([]any); len(m) != 1 || m[0] != missing {
				t.Fatalf("headers_missing = %v", m)
			}
			if !strings.Contains(errBody["message"].(string), missing) {
				t.Fatal("message doesn't name the header")
			}
			if e.upstream.callCount(decisionsPath) != 0 {
				t.Fatal("upstream called")
			}
			if e.scalar("SELECT error_code FROM usage") != "missing_header" {
				t.Fatal("rejection not logged")
			}
		})
	}
}

func TestInvalidAttributionHeaderIs400(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	for _, bad := range []string{"has spaces", strings.Repeat("a", 65), "émoji"} {
		r := e.post("/v1/embed", embed, caller(key, bad))
		expect(t, r, 400, "invalid_header")
		if h := r.json(t)["error"].(map[string]any)["headers_invalid"].([]any); h[0] != "X-AGENT-SURFACE" {
			t.Fatalf("headers_invalid = %v", h)
		}
	}
}

func TestAttributionIsLowercased(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	expect(t, e.post("/v1/embed", embed, caller(key, "Slack", "Bot-A")), 200, "")
	if _, ok := e.usageBy("surface", "")["slack"]; !ok {
		t.Fatal("surface not lowercased")
	}
	if _, ok := e.usageBy("agent", "")["bot-a"]; !ok {
		t.Fatal("agent not lowercased")
	}
}

// --- limits ---

func TestRateLimitIs429(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, map[string]any{"rpm": 1})
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	r := e.post("/v1/decide", decide, caller(key))
	expect(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" || r.header.Get("Retry-After") == "0" {
		t.Fatalf("Retry-After = %q", r.header.Get("Retry-After"))
	}
}

func TestMaxTokensPerRequestIs413(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, map[string]any{"max_tokens_per_request": 10})
	expect(t, e.post("/v1/embed", map[string]any{"input": strings.Repeat("x", 200)}, caller(key)), 413, "too_many_tokens")
	if e.upstream.callCount(embeddingsPath) != 0 {
		t.Fatal("upstream called")
	}
}

func TestTooManyEmbedInputs(t *testing.T) {
	e := newEnv(t, config.Grinder, with("MAX_EMBED_INPUTS", "2"))
	key := e.makeKey(config.Grinder, nil)
	expect(t, e.post("/v1/embed", map[string]any{"input": []string{"a", "b", "c"}}, caller(key)), 400, "too_many_inputs")
	expect(t, e.post("/v1/embed", map[string]any{"input": []any{"a", 1}}, caller(key)), 422, "invalid_request")
	expect(t, e.post("/v1/embed", map[string]any{"input": ""}, caller(key)), 422, "invalid_request")
}

func TestKeyBudgetIs402(t *testing.T) {
	// Reservation estimate is ~$0.00056 (fallback $1/Mtok on ~560 tokens incl. the output allowance), each call
	// costs $0.0004. Budget 0.001: call 1 fits; call 2 would need 0.0004 + 0.00056 <= 0.001 and fits; call 3 doesn't.
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "0.001"})
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	r := e.post("/v1/decide", decide, caller(key))
	expect(t, r, 402, "budget_exceeded")
	if r.json(t)["error"].(map[string]any)["spent_usd"] != "0.00080000" {
		t.Fatalf("spent_usd: %s", r.body)
	}
	statuses := e.usageBy("status", "")
	if statuses["200"]["requests"] != 2.0 || statuses["402"]["requests"] != 1.0 {
		t.Fatalf("statuses = %v", statuses)
	}
	if e.upstream.callCount(decisionsPath) != 2 {
		t.Fatal("refused call reached upstream")
	}
}

func TestServiceCapIs402(t *testing.T) {
	e := newEnv(t, config.Barista, with("BARISTA_MONTHLY_CAP_USD", "0.001"))
	alice := e.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "5"})
	bob := e.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "5"})
	expect(t, e.post("/v1/decide", decide, caller(alice)), 200, "")
	expect(t, e.post("/v1/decide", decide, caller(bob)), 200, "")
	expect(t, e.post("/v1/decide", decide, caller(bob)), 402, "service_budget_exceeded")
	expect(t, e.post("/v1/decide", decide, caller(alice)), 402, "service_budget_exceeded")
}

func TestZeroCapBlocksEverything(t *testing.T) {
	e := newEnv(t, config.Grinder, with("GRINDER_MONTHLY_CAP_USD", "0"))
	key := e.makeKey(config.Grinder, nil)
	expect(t, e.post("/v1/embed", embed, caller(key)), 402, "service_budget_exceeded")
	if e.upstream.callCount(embeddingsPath) != 0 {
		t.Fatal("upstream called with a zero cap")
	}
}

func TestCapsAreSeparatePerService(t *testing.T) {
	st, up := testdb.New(t), newMockOpenRouter(t)
	bar := newEnvShared(t, config.Barista, st, up, with("BARISTA_MONTHLY_CAP_USD", "0"))
	grd := newEnvShared(t, config.Grinder, st, up, with("BARISTA_MONTHLY_CAP_USD", "0"))
	expect(t, bar.post("/v1/decide", decide, caller(bar.makeKey(config.Barista, nil))), 402, "service_budget_exceeded")
	expect(t, grd.post("/v1/embed", embed, caller(bar.makeKey(config.Grinder, nil))), 200, "")
}

// Concurrent requests must not all pass the check on the same remaining budget: the in-flight reservation counts.
func TestReservationsStopConcurrentOvershoot(t *testing.T) {
	st, up := testdb.New(t), newMockOpenRouter(t)
	// Two replicas of the same service share the cap through Postgres.
	a := newEnvShared(t, config.Barista, st, up, with("BARISTA_MONTHLY_CAP_USD", "0.0008"))
	b := newEnvShared(t, config.Barista, st, up, with("BARISTA_MONTHLY_CAP_USD", "0.0008"))
	key := a.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "5", "rpm": 100})
	gate := make(chan struct{})
	up.mu.Lock()
	up.gate = gate
	up.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]response, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := a
			if i%2 == 1 {
				e = b
			}
			results[i] = e.post("/v1/decide", decide, caller(key))
		}()
	}
	// Refusals come back while the one admitted request is still held at the upstream.
	waitForStatus(t, st, 402, len(results)-1)
	close(gate)
	wg.Wait()
	var ok, refused int
	for _, r := range results {
		switch r.status {
		case 200:
			ok++
		case 402:
			refused++
		}
	}
	if ok != 1 || refused != len(results)-1 {
		t.Fatalf("ok=%d refused=%d, want 1 and %d", ok, refused, len(results)-1)
	}
	if up.callCount(decisionsPath) != 1 {
		t.Fatalf("upstream calls = %d", up.callCount(decisionsPath))
	}
}

// waitForStatus polls until n usage rows with this status exist.
func waitForStatus(t *testing.T, st *store.Store, status, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM usage WHERE status_code = $1", status).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d rows with status %d", n, status)
}

// --- reconciliation ---

func TestSettlementReplacesReservationWithRealCost(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	if n := e.scalar("SELECT count(*)::text FROM reservations"); n != "0" {
		t.Fatalf("%s reservations left open", n)
	}
	if c := e.scalar("SELECT cost_usd::text FROM usage"); c != "0.00040000" {
		t.Fatalf("recorded cost = %s", c)
	}
	r := e.do("GET", "/v1/admin/spend", nil, adminHeaders())
	expect(t, r, 200, "")
	svc := r.json(t)["services"].(map[string]any)["barista"].(map[string]any)
	if svc["spent_usd"] != "0.00040000" || svc["reserved_usd"] != "0.00000000" || svc["status"] != "ok" {
		t.Fatalf("spend = %v", svc)
	}
}

func TestCostFromCatalogueWhenNotReported(t *testing.T) {
	e := newEnv(t, config.Grinder)
	e.gw.Pricing.SetPrices(map[string]openrouter.Price{
		"qwen/qwen3-embedding-8b": {Prompt: decimal.RequireFromString("0.00000002")},
	})
	e.upstream.reply(embeddingsPath, upstreamReply{200, map[string]any{"id": "e", "data": []any{}, "usage": map[string]any{"prompt_tokens": 1000}}})
	key := e.makeKey(config.Grinder, nil)
	r := e.post("/v1/embed", embed, caller(key))
	expect(t, r, 200, "")
	if r.header.Get("X-Cost-Usd") != "0.00002000" || e.scalar("SELECT cost_source FROM usage") != "pricing" {
		t.Fatalf("cost = %s source = %s", r.header.Get("X-Cost-Usd"), e.scalar("SELECT cost_source FROM usage"))
	}
}

func TestFallbackPriceWhenNothingKnown(t *testing.T) {
	e := newEnv(t, config.Grinder)
	e.upstream.reply(embeddingsPath, upstreamReply{200, map[string]any{"id": "e", "data": []any{}, "usage": map[string]any{"prompt_tokens": 1000}}})
	key := e.makeKey(config.Grinder, nil)
	r := e.post("/v1/embed", embed, caller(key))
	expect(t, r, 200, "")
	if r.header.Get("X-Cost-Usd") != "0.00100000" || e.scalar("SELECT cost_source FROM usage") != "fallback" {
		t.Fatalf("cost = %s", r.header.Get("X-Cost-Usd"))
	}
}

func TestUpstreamFailureReleasesReservation(t *testing.T) {
	e := newEnv(t, config.Grinder)
	e.upstream.reply(embeddingsPath, upstreamReply{500, map[string]any{"error": map[string]any{"message": "boom"}}})
	key := e.makeKey(config.Grinder, nil)
	expect(t, e.post("/v1/embed", embed, caller(key)), 502, "upstream_error")
	if e.scalar("SELECT count(*)::text FROM reservations") != "0" || e.scalar("SELECT cost_usd::text FROM usage") != "0.00000000" {
		t.Fatal("failed call left a reservation or a cost")
	}
}

func TestUpstreamRetriesTransientErrors(t *testing.T) {
	e := newEnv(t, config.Grinder, with("UPSTREAM_MAX_RETRIES", "2"))
	e.upstream.reply(embeddingsPath,
		upstreamReply{503, map[string]any{"error": map[string]any{"message": "busy"}}},
		upstreamReply{200, embeddingBody})
	key := e.makeKey(config.Grinder, nil)
	expect(t, e.post("/v1/embed", embed, caller(key)), 200, "")
	if e.upstream.callCount(embeddingsPath) != 2 {
		t.Fatalf("calls = %d", e.upstream.callCount(embeddingsPath))
	}
}

func TestUpstreamErrorsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		upstream, status int
		code             string
	}{{400, 400, "upstream_rejected"}, {402, 502, "upstream_auth"}, {401, 502, "upstream_auth"}, {429, 503, "upstream_rate_limited"}, {500, 502, "upstream_error"}} {
		t.Run(tc.code, func(t *testing.T) {
			e := newEnv(t, config.Grinder)
			e.upstream.reply(embeddingsPath, upstreamReply{tc.upstream, map[string]any{"error": map[string]any{"message": "nope: secret account detail"}}})
			key := e.makeKey(config.Grinder, nil)
			r := e.post("/v1/embed", embed, caller(key))
			expect(t, r, tc.status, tc.code)
			if tc.code == "upstream_auth" && bytes.Contains(r.body, []byte("secret account detail")) {
				t.Fatal("upstream auth details leaked to the caller")
			}
		})
	}
}

func TestModelAllowList(t *testing.T) {
	e := newEnv(t, config.Barista, with("BARISTA_MODELS", "perplexity/pplx-decider-flash-v1.2, perplexity/other"))
	key := e.makeKey(config.Barista, map[string]any{"rpm": 100})
	body := map[string]any{"state": "s", "questions": map[string]any{"q": map[string]any{"type": "noul"}}, "model": "openai/gpt-5"}
	r := e.post("/v1/decide", body, caller(key))
	expect(t, r, 400, "model_not_allowed")
	if m := r.json(t)["error"].(map[string]any)["allowed_models"].([]any); len(m) != 2 {
		t.Fatalf("allowed_models = %v", m)
	}
	body["model"] = "perplexity/other"
	expect(t, e.post("/v1/decide", body, caller(key)), 200, "")
	if e.upstream.last(decisionsPath)["model"] != "perplexity/other" {
		t.Fatal("chosen model not forwarded")
	}
}

func TestDecideValidation(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, map[string]any{"rpm": 100})
	for _, body := range []any{"not json", map[string]any{"questions": map[string]any{"q": 1}}, map[string]any{"state": "s", "questions": map[string]any{}}} {
		expect(t, e.post("/v1/decide", body, caller(key)), 422, "invalid_request")
	}
}

// --- alerts and metrics ---

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) events(name string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, r := range h.records {
		attrs := map[string]any{"level": r.Level.String()}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
		if attrs["event"] == name {
			out = append(out, attrs)
		}
	}
	return out
}

func TestCapThresholdAlertsAndMetrics(t *testing.T) {
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t, config.Barista, with("BARISTA_MONTHLY_CAP_USD", "0.001"))
	// A tiny catalogue price keeps the reservation small, so spend can reach 100%.
	e.gw.Pricing.SetPrices(map[string]openrouter.Price{"perplexity/pplx-decider-flash-v1.2": {Prompt: decimal.RequireFromString("0.000000001")}})
	key := e.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "5"})
	for range 3 { // spend: 0.0004, 0.0008 (80%), 0.0012 (100%)
		expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	}
	expect(t, e.post("/v1/decide", decide, caller(key)), 402, "service_budget_exceeded")

	alerts := capture.events("cafe_gateway_cap_threshold")
	if len(alerts) != 2 || alerts[0]["threshold_pct"] != int64(80) || alerts[1]["threshold_pct"] != int64(100) {
		t.Fatalf("alerts = %v", alerts)
	}
	if alerts[0]["level"] != "WARN" || alerts[0]["service"] != "barista" {
		t.Fatalf("alert = %v", alerts[0])
	}
	if len(capture.events("cafe_gateway_cap_refused")) != 1 {
		t.Fatal("refusal not logged")
	}

	m := string(e.do("GET", "/metrics", nil, nil).body)
	for _, want := range []string{
		`cafe_gateway_cap_threshold_alerts_total{service="barista",threshold_pct="80"} 1`,
		`cafe_gateway_cap_threshold_alerts_total{service="barista",threshold_pct="100"} 1`,
		`cafe_gateway_month_spend_usd{service="barista"} 0.0012`,
		`cafe_gateway_monthly_cap_usd{service="barista"} 0.001`,
		`cafe_gateway_requests_total{code="service_budget_exceeded",service="barista",status="402"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(m, "alice") || strings.Contains(m, key) {
		t.Fatal("metrics expose key names")
	}
	r := e.do("GET", "/v1/admin/spend", nil, adminHeaders())
	if s := r.json(t)["services"].(map[string]any)["barista"].(map[string]any)["status"]; s != "exceeded" {
		t.Fatalf("status = %v", s)
	}
}

func TestKeyBudgetAlerts(t *testing.T) {
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t, config.Barista)
	e.gw.Pricing.SetPrices(map[string]openrouter.Price{"perplexity/pplx-decider-flash-v1.2": {Prompt: decimal.RequireFromString("0.000000001")}})
	key := e.makeKey(config.Barista, map[string]any{"monthly_budget_usd": "0.0005"})
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "") // 0.0004 = 80%
	alerts := capture.events("cafe_gateway_key_budget_threshold")
	if len(alerts) != 1 || alerts[0]["threshold_pct"] != int64(80) || alerts[0]["key_name"] != "alice" {
		t.Fatalf("alerts = %v", alerts)
	}
}

// --- usage, admin, hosts ---

func TestOwnUsageReport(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	e.post("/v1/decide", decide, caller(key, "slack", "bot-a"))
	e.post("/v1/decide", decide, caller(key, "cli", "bot-a"))
	other := e.makeKey(config.Barista, nil)
	e.post("/v1/decide", decide, caller(other, "web", "bot-b"))

	r := e.do("GET", "/v1/usage?by=surface", nil, caller(key))
	expect(t, r, 200, "")
	report := r.json(t)
	if report["totals"].(map[string]any)["requests"] != 2.0 {
		t.Fatalf("report = %s", r.body)
	}
	var groups []string
	for _, g := range report["groups"].([]any) {
		groups = append(groups, g.(map[string]any)["group"].(string))
	}
	slices.Sort(groups)
	if !slices.Equal(groups, []string{"cli", "slack"}) {
		t.Fatalf("groups = %v", groups)
	}
	if report["key"].(map[string]any)["service"] != "barista" {
		t.Fatal("key missing from report")
	}
	expect(t, e.do("GET", "/v1/usage?by=key", nil, caller(key)), 400, "invalid_request")
	expect(t, e.do("GET", "/v1/usage?month=2026-13x", nil, caller(key)), 400, "invalid_request")
	// The usage endpoint doesn't need attribution headers.
	expect(t, e.do("GET", "/v1/usage", nil, map[string]string{"Authorization": "Bearer " + key}), 200, "")
}

func TestAdminUsageGroupsByKey(t *testing.T) {
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	e.post("/v1/decide", decide, caller(key))
	groups := e.usageBy("key", "")
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	for name := range groups {
		if !strings.HasPrefix(name, "alice (") {
			t.Fatalf("group = %q", name)
		}
	}
	expect(t, e.do("GET", "/v1/admin/usage?by=nope", nil, adminHeaders()), 400, "invalid_request")
}

func TestAdminRequiresToken(t *testing.T) {
	e := newEnv(t, config.Barista)
	expect(t, e.do("GET", "/v1/admin/keys", nil, nil), 401, "invalid_admin_token")
	expect(t, e.do("GET", "/v1/admin/keys", nil, map[string]string{"Authorization": "Bearer wrong"}), 401, "invalid_admin_token")
	// A gateway key is not an admin token.
	key := e.makeKey(config.Barista, nil)
	expect(t, e.do("GET", "/v1/admin/keys", nil, map[string]string{"Authorization": "Bearer " + key}), 401, "invalid_admin_token")
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	e := newEnv(t, config.Barista, with("BARISTA_ADMIN_TOKEN", ""))
	expect(t, e.do("GET", "/v1/admin/keys", nil, map[string]string{"Authorization": "Bearer "}), 503, "admin_disabled")
}

func TestAdminRejectsBadKeySettings(t *testing.T) {
	e := newEnv(t, config.Barista)
	for _, body := range []map[string]any{
		{"service": "barista", "name": "x", "rpm": 0},
		{"service": "barista", "name": "x", "monthly_budget_usd": "-1"},
		{"service": "espresso", "name": "x"},
		{"service": "barista", "name": " "},
	} {
		expect(t, e.do("POST", "/v1/admin/keys", body, adminHeaders()), 422, "invalid_request")
	}
}

func TestAllowedHosts(t *testing.T) {
	e := newEnv(t, config.Barista, with("BARISTA_ALLOWED_HOSTS", "barista.example.ts.net"))
	_, key, err := e.store.CreateKey(context.Background(), store.NewKey{
		Service: config.Barista, Name: "alice", RPM: 30, MaxTokensPerRequest: 8000, MonthlyBudgetUSD: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	host := map[string]string{"Host": "barista.example.ts.net"}
	expect(t, e.post("/v1/decide", decide, caller(key)), 404, "not_found") // test server's 127.0.0.1 host
	expect(t, e.post("/v1/decide", decide, merge(caller(key), host)), 200, "")
	expect(t, e.post("/v1/decide", decide, merge(caller(key), map[string]string{"Host": "barista.example.ts.net:443"})), 200, "")
	// X-Forwarded-Host can only narrow access.
	expect(t, e.post("/v1/decide", decide, merge(caller(key), merge(host, map[string]string{"X-Forwarded-Host": "evil.example"}))), 404, "not_found")
	// Probes and scrapes by pod IP still work.
	expect(t, e.do("GET", "/health", nil, nil), 200, "")
	expect(t, e.do("GET", "/metrics", nil, nil), 200, "")
}

func TestDebugStoreBodies(t *testing.T) {
	off := newEnv(t, config.Grinder)
	expect(t, off.post("/v1/embed", embed, caller(off.makeKey(config.Grinder, nil))), 200, "")
	if off.scalar("SELECT coalesce(request_body, 'NULL') FROM usage") != "NULL" {
		t.Fatal("body stored without BARISTA_DEBUG_STORE_BODIES")
	}
	on := newEnv(t, config.Grinder, with("BARISTA_DEBUG_STORE_BODIES", "true"))
	expect(t, on.post("/v1/embed", embed, caller(on.makeKey(config.Grinder, nil))), 200, "")
	if !strings.Contains(on.scalar("SELECT request_body FROM usage"), "espresso") {
		t.Fatal("body not stored with BARISTA_DEBUG_STORE_BODIES=true")
	}
}

func TestUpstreamNotConfigured(t *testing.T) {
	e := newEnv(t, config.Grinder, with("OPEN_ROUTER_KEY", ""))
	expect(t, e.post("/v1/embed", embed, caller(e.makeKey(config.Grinder, nil))), 503, "upstream_not_configured")
}

func TestHealthDocsAndOpenAPI(t *testing.T) {
	e := newEnv(t, config.Grinder)
	if r := e.do("GET", "/health", nil, nil); r.json(t)["service"] != "grinder" {
		t.Fatalf("health = %s", r.body)
	}
	expect(t, e.do("GET", "/ready", nil, nil), 200, "")
	spec := e.do("GET", "/openapi.json", nil, nil).json(t)
	paths := spec["paths"].(map[string]any)
	if _, ok := paths["/v1/embed"]; !ok {
		t.Fatal("grinder spec lacks /v1/embed")
	}
	if _, ok := paths["/v1/decide"]; ok {
		t.Fatal("grinder spec lists /v1/decide")
	}
	if !bytes.Contains(e.do("GET", "/docs", nil, nil).body, []byte("/v1/embed")) {
		t.Fatal("docs lack the example")
	}
	expect(t, e.do("GET", "/nope", nil, nil), http.StatusNotFound, "not_found")
}
