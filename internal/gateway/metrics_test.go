package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

// --- per-key metrics on the internal listener ---

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestPerKeyMetricsOnInternalListener(t *testing.T) {
	e := newEnv(t, config.Barista)
	alice := e.makeKey(config.Barista, nil)
	aliceID := strings.Split(alice, "_")[1]
	model := "perplexity/pplx-decider-flash-v1.2"
	expect(t, e.post("/v1/decide", decide, caller(alice, "slack", "bot-a")), 200, "")
	expect(t, e.post("/v1/decide", decide, map[string]string{"Authorization": "Bearer " + alice}), 400, "missing_header")
	expect(t, e.post("/v1/decide", decide, caller("bar_zzzzzzzzzzzz_"+strings.Repeat("q", 43), "evil", "evil")), 401, "invalid_api_key")

	u := e.gw.Users
	ok := u.Requests.WithLabelValues("barista", "alice", aliceID, "slack", "bot-a", model, "200")
	if v := promtest.ToFloat64(ok); v != 1 {
		t.Fatalf("alice 200 = %v", v)
	}
	if v := promtest.ToFloat64(u.Errors.WithLabelValues("barista", "alice", aliceID, "none", "none", "none", "400", "missing_header")); v != 1 {
		t.Fatalf("alice missing_header = %v", v)
	}
	if v := promtest.ToFloat64(u.Errors.WithLabelValues("barista", "unauthenticated", "unauthenticated", "unauthenticated", "unauthenticated", "none", "401", "invalid_api_key")); v != 1 {
		t.Fatalf("unauthenticated = %v", v)
	}
	if v := promtest.ToFloat64(u.Tokens.WithLabelValues("barista", "alice", aliceID, "slack", "bot-a", model, "input")); v != 120 {
		t.Fatalf("input tokens = %v", v)
	}
	if v := promtest.ToFloat64(u.Tokens.WithLabelValues("barista", "alice", aliceID, "slack", "bot-a", model, "output")); v != 8 {
		t.Fatalf("output tokens = %v", v)
	}
	if v := promtest.ToFloat64(u.SpendUSD.WithLabelValues("barista", "alice", aliceID, "slack", "bot-a", model)); v != 0.0004 {
		t.Fatalf("spend = %v", v)
	}
	if v := promtest.ToFloat64(u.BudgetRemaining.WithLabelValues("barista", "alice", aliceID)); v != 0.9996 {
		t.Fatalf("budget remaining = %v", v)
	}
	if v := promtest.ToFloat64(u.CapRemaining.WithLabelValues("barista")); v != 19.9996 {
		t.Fatalf("cap remaining = %v", v)
	}

	internal := scrape(t, e.gw.InternalHandler())
	for _, want := range []string{
		`cafe_gateway_user_requests_total{agent_id="bot-a",key_id="` + aliceID + `",key_name="alice",model="` + model + `",service="barista",status="200",surface="slack"} 1`,
		`cafe_gateway_user_request_duration_seconds_count{key_id="` + aliceID + `",key_name="alice",model="` + model + `",service="barista"} 1`,
		`cafe_gateway_user_request_duration_seconds_count{key_id="` + aliceID + `",key_name="alice",model="none",service="barista"} 1`,
		`cafe_gateway_cap_remaining_usd{service="barista"}`,
		`cafe_gateway_month_spend_usd{service="barista"} 0.0004`, // the public series are on the internal port too
	} {
		if !strings.Contains(internal, want) {
			t.Errorf("internal /metrics lacks %s", want)
		}
	}
	// The tailnet-facing /metrics never carries key names or ids.
	public := string(e.do("GET", "/metrics", nil, nil).body)
	if strings.Contains(public, "alice") || strings.Contains(public, aliceID) || strings.Contains(public, "cafe_gateway_user_") {
		t.Fatal("per-key series leaked onto :8000/metrics")
	}
	// The internal handler serves nothing else.
	r, _ := http.Get(httptest.NewServer(e.gw.InternalHandler()).URL + "/v1/decide")
	if r.StatusCode != 404 {
		t.Fatalf("internal listener serves %d on /v1/decide", r.StatusCode)
	}
	_ = r.Body.Close()
}

func TestBudgetGaugesRefreshAndDropRevokedKeys(t *testing.T) {
	e := newEnv(t, config.Grinder)
	a := strings.Split(e.makeKey(config.Grinder, map[string]any{"monthly_budget_usd": "3"}), "_")[1]
	b := strings.Split(e.makeKey(config.Grinder, nil), "_")[1]
	e.makeKey(config.Barista, nil) // other service: not this deployment's gauges
	if err := e.gw.RefreshSpend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := promtest.CollectAndCount(e.gw.Users.BudgetRemaining); n != 2 {
		t.Fatalf("series = %d", n)
	}
	if v := promtest.ToFloat64(e.gw.Users.BudgetUSD.WithLabelValues("grinder", "alice", a)); v != 3 {
		t.Fatalf("budget = %v", v)
	}
	expect(t, e.do("POST", "/v1/admin/keys/"+b+"/revoke", nil, adminHeaders()), 200, "")
	if err := e.gw.RefreshSpend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := promtest.CollectAndCount(e.gw.Users.BudgetRemaining); n != 1 {
		t.Fatalf("revoked key still has a series (%d)", n)
	}
}
