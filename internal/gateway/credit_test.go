package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

// --- the shared OpenRouter key's real credit (GET /api/v1/key) ---

// keyInfo is OpenRouter's key endpoint body for a key with limit (nil = no limit) and usage.
func keyInfo(limit any, usage float64) upstreamReply {
	data := map[string]any{
		"label": "sk-or-v1-abc...xyz", "limit": limit, "limit_remaining": nil, "limit_reset": nil, "usage": usage,
		"usage_daily": 0.25, "usage_weekly": 1.5, "usage_monthly": usage, "is_free_tier": false,
	}
	if l, ok := limit.(float64); ok {
		data["limit_remaining"] = l - usage
	}
	return upstreamReply{200, map[string]any{"data": data}}
}

func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return capture
}

func (e *env) poll() error {
	e.t.Helper()
	return e.gw.PollCredit(context.Background())
}

func (e *env) keyCredit() map[string]any {
	e.t.Helper()
	r := e.do("GET", "/v1/admin/spend", nil, adminHeaders())
	expect(e.t, r, 200, "")
	return r.json(e.t)["openrouter_key"].(map[string]any)
}

func TestKeyCreditNormal(t *testing.T) {
	e := newEnv(t, config.Barista)
	e.upstream.reply(keyPath, keyInfo(10.0, 2.5))
	if err := e.poll(); err != nil {
		t.Fatal(err)
	}
	key := e.makeKey(config.Barista, nil)
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")

	for _, want := range []string{
		`cafe_gateway_openrouter_key_limit_usd 10`,
		`cafe_gateway_openrouter_key_usage_usd 2.5`,
		`cafe_gateway_openrouter_key_remaining_usd 7.5`,
		`cafe_gateway_openrouter_key_remaining_ratio 0.75`,
		`cafe_gateway_openrouter_key_usage_daily_usd 0.25`,
		`cafe_gateway_openrouter_key_usage_weekly_usd 1.5`,
		`cafe_gateway_openrouter_key_usage_monthly_usd 2.5`,
		`cafe_gateway_openrouter_key_min_remaining_usd 0.5`,
		`cafe_gateway_openrouter_key_poll_success 1`,
		`cafe_gateway_openrouter_key_blocked 0`,
		`cafe_gateway_openrouter_key_last_poll_timestamp_seconds `,
	} {
		for name, body := range map[string]string{"public": string(e.do("GET", "/metrics", nil, nil).body), "internal": scrape(t, e.gw.InternalHandler())} {
			if !strings.Contains(body, want) {
				t.Errorf("%s /metrics lacks %q", name, want)
			}
		}
	}
	c := e.keyCredit()
	if c["status"] != "ok" || c["limit_usd"] != "10.00000000" || c["remaining_usd"] != "7.50000000" ||
		c["remaining_ratio"] != 0.75 || c["usage_daily_usd"] != "0.25000000" || c["blocking"] != false || c["poll_ok"] != true {
		t.Fatalf("admin spend openrouter_key = %v", c)
	}
	if strings.Contains(string(e.do("GET", "/v1/admin/spend", nil, adminHeaders()).body), "sk-or") {
		t.Fatal("key label leaked")
	}
}

func TestKeyCreditNearLimitAlertsOnce(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	e.upstream.reply(keyPath, keyInfo(100.0, 50), keyInfo(100.0, 85), keyInfo(100.0, 86), keyInfo(100.0, 96))
	for range 4 {
		if err := e.poll(); err != nil {
			t.Fatal(err)
		}
	}
	alerts := logs.events("cafe_gateway_openrouter_key_threshold")
	if len(alerts) != 2 || alerts[0]["threshold_pct"] != int64(80) || alerts[1]["threshold_pct"] != int64(95) || alerts[0]["level"] != "WARN" {
		t.Fatalf("alerts = %v", alerts)
	}
	if v := promtest.ToFloat64(e.gw.Metrics.KeyAlerts.WithLabelValues("95")); v != 1 {
		t.Fatalf("95%% alerts = %v", v)
	}
	// $4 left is above the $0.50 floor: still serving.
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	if s := e.keyCredit()["status"]; s != "critical" {
		t.Fatalf("status = %v", s)
	}
}

func TestKeyCreditBelowFloorBlocksAndRecovers(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, config.Barista)
	key := e.makeKey(config.Barista, nil)
	e.upstream.reply(keyPath, keyInfo(10.0, 9.7), keyInfo(10.0, 9.8), keyInfo(15.0, 9.8))

	for range 2 {
		if err := e.poll(); err != nil {
			t.Fatal(err)
		}
	}
	r := e.post("/v1/decide", decide, caller(key))
	expect(t, r, 402, "upstream_credit_low")
	if body := r.json(t)["error"].(map[string]any); body["remaining_usd"] != "0.20000000" || body["min_remaining_usd"] != "0.5" {
		t.Fatalf("402 body = %v", body)
	}
	if n := e.upstream.callCount(decisionsPath); n != 0 {
		t.Fatalf("upstream called %d times while blocked", n)
	}
	if low := logs.events("cafe_gateway_openrouter_credit_low"); len(low) != 1 || low[0]["level"] != "WARN" {
		t.Fatalf("credit_low logs = %v (want one per crossing)", low)
	}
	if v := promtest.ToFloat64(e.gw.Metrics.KeyBlocked.WithLabelValues()); v != 1 {
		t.Fatalf("blocked = %v", v)
	}
	if c := e.keyCredit(); c["status"] != "below_floor" || c["blocking"] != true {
		t.Fatalf("admin = %v", c)
	}
	if got := e.usageBy("status", ""); got["402"]["requests"] != 1.0 {
		t.Fatalf("refusal not recorded: %v", got)
	}

	// Credit added: the next poll lifts the block.
	if err := e.poll(); err != nil {
		t.Fatal(err)
	}
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	if len(logs.events("cafe_gateway_openrouter_credit_ok")) != 1 {
		t.Fatal("recovery not logged")
	}
	if v := promtest.ToFloat64(e.gw.Metrics.KeyBlocked.WithLabelValues()); v != 0 {
		t.Fatalf("blocked = %v", v)
	}
}

func TestKeyCreditPollFailureFailsOpen(t *testing.T) {
	e := newEnv(t, config.Grinder)
	key := e.makeKey(config.Grinder, nil)
	e.upstream.reply(keyPath, keyInfo(10.0, 9.9), upstreamReply{500, map[string]any{"error": map[string]any{"message": "boom"}}})
	if err := e.poll(); err != nil {
		t.Fatal(err)
	}
	expect(t, e.post("/v1/embed", embed, caller(key)), 402, "upstream_credit_low")
	if err := e.poll(); err == nil {
		t.Fatal("500 should fail the poll")
	}
	expect(t, e.post("/v1/embed", embed, caller(key)), 200, "")
	if v := promtest.ToFloat64(e.gw.Metrics.KeyPollSuccess.WithLabelValues()); v != 0 {
		t.Fatalf("poll_success = %v", v)
	}
	if v := promtest.ToFloat64(e.gw.Metrics.KeyBlocked.WithLabelValues()); v != 0 {
		t.Fatalf("blocked = %v", v)
	}
	c := e.keyCredit()
	if c["poll_ok"] != false || c["blocking"] != false || c["poll_error"] == nil || c["remaining_usd"] != "0.10000000" {
		t.Fatalf("admin = %v", c)
	}
}

func TestKeyCreditNeverPolled(t *testing.T) {
	// No successful poll yet: no block, and admin spend fetches the credit live.
	e := newEnv(t, config.Barista, with("OPENROUTER_POLL_SECONDS", "0"))
	key := e.makeKey(config.Barista, nil)
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "")
	if m := string(e.do("GET", "/metrics", nil, nil).body); strings.Contains(m, "cafe_gateway_openrouter_key_") {
		t.Fatal("key gauges exported before any poll")
	}
	e.upstream.reply(keyPath, keyInfo(10.0, 9.9))
	c := e.keyCredit()
	if c["status"] != "below_floor" || c["polling"] != false || c["blocking"] != false {
		t.Fatalf("admin = %v", c)
	}
	expect(t, e.post("/v1/decide", decide, caller(key)), 200, "") // a live admin read never blocks
}

func TestKeyCreditUnlimitedKey(t *testing.T) {
	e := newEnv(t, config.Barista)
	e.upstream.reply(keyPath, keyInfo(nil, 42))
	if err := e.poll(); err != nil {
		t.Fatal(err)
	}
	m := string(e.do("GET", "/metrics", nil, nil).body)
	if !strings.Contains(m, "cafe_gateway_openrouter_key_usage_usd 42") || strings.Contains(m, "cafe_gateway_openrouter_key_limit_usd") ||
		strings.Contains(m, "cafe_gateway_openrouter_key_remaining_usd") {
		t.Fatalf("unlimited key metrics:\n%s", m)
	}
	if c := e.keyCredit(); c["status"] != "unlimited" || c["limit_usd"] != nil {
		t.Fatalf("admin = %v", c)
	}
	expect(t, e.post("/v1/decide", decide, caller(e.makeKey(config.Barista, nil))), 200, "")
}

func TestCreditPollerRunsAndBacksOff(t *testing.T) {
	e := newEnv(t, config.Barista, with("OPENROUTER_POLL_SECONDS", "0.01"))
	e.upstream.reply(keyPath, upstreamReply{503, map[string]any{}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	e.gw.RunCreditPoller(ctx)
	e.upstream.mu.Lock()
	polls := e.upstream.keyPolls
	e.upstream.mu.Unlock()
	// Without backoff 300ms / 10ms would be ~30 polls; doubling (20, 40, 80, 160ms ...) allows about 4.
	if polls < 2 || polls > 6 {
		t.Fatalf("polls = %d", polls)
	}
}
