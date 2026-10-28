package metrics

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestUnauthenticatedCallersShareOneSeries(t *testing.T) {
	u := NewUsers()
	for i := range 20 {
		// Caller-supplied values are ignored without a key: forged keys can't mint series.
		u.ObserveRequest(RequestLabels{Service: "barista", Surface: fmt.Sprint("s", i), AgentID: "a", Status: 401, ErrorCode: "invalid_api_key"}, 0.01, 0, 0, 0)
	}
	if n := testutil.CollectAndCount(u.Requests); n != 1 {
		t.Fatalf("series = %d", n)
	}
	v := testutil.ToFloat64(u.Errors.WithLabelValues("barista", Unauthenticated, Unauthenticated, Unauthenticated, Unauthenticated, None, "401", "invalid_api_key"))
	if v != 20 {
		t.Fatalf("errors = %v", v)
	}
}

func TestAttributionsPerKeyAreBounded(t *testing.T) {
	u := NewUsers()
	for i := range MaxAttributionsPerKey + 10 {
		u.ObserveRequest(RequestLabels{Service: "grinder", KeyName: "alice", KeyID: "k1", Surface: "cli", AgentID: fmt.Sprint("agent-", i), Model: "m", Status: 200}, 0.01, 1, 0, 0.001)
	}
	if n := testutil.CollectAndCount(u.Requests); n != MaxAttributionsPerKey+1 {
		t.Fatalf("series = %d, want %d", n, MaxAttributionsPerKey+1)
	}
	if v := testutil.ToFloat64(u.Requests.WithLabelValues("grinder", "alice", "k1", Other, Other, "m", "200")); v != 10 {
		t.Fatalf("other = %v", v)
	}
	// A pair seen before the limit keeps its own series.
	u.ObserveRequest(RequestLabels{Service: "grinder", KeyName: "alice", KeyID: "k1", Surface: "cli", AgentID: "agent-0", Model: "m", Status: 200}, 0.01, 0, 0, 0)
	if v := testutil.ToFloat64(u.Requests.WithLabelValues("grinder", "alice", "k1", "cli", "agent-0", "m", "200")); v != 2 {
		t.Fatalf("agent-0 = %v", v)
	}
	// Another key has its own budget of pairs.
	u.ObserveRequest(RequestLabels{Service: "grinder", KeyName: "bob", KeyID: "k2", Surface: "cli", AgentID: "x", Model: "m", Status: 200}, 0.01, 0, 0, 0)
	if v := testutil.ToFloat64(u.Requests.WithLabelValues("grinder", "bob", "k2", "cli", "x", "m", "200")); v != 1 {
		t.Fatal("second key folded")
	}
}
