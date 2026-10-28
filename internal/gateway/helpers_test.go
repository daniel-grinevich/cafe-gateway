package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/openrouter"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
	"github.com/daniel-grinevich/cafe-gateway/internal/testdb"
)

const adminToken = "test-admin-token"

func TestMain(m *testing.M) {
	// Request logs are noise in test output; tests that assert on logs install their own handler.
	flag.Parse()
	if !testing.Verbose() {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}
	os.Exit(m.Run())
}

var decisionBody = map[string]any{
	"id":      "dec-1",
	"model":   "perplexity/pplx-decider-flash-v1.2",
	"answers": map[string]any{"ship": map[string]any{"type": "noul", "noul": 0.96}},
	"usage":   map[string]any{"input_tokens": 120, "output_tokens": 8, "cost": 0.0004},
}

var embeddingBody = map[string]any{
	"id":    "emb-1",
	"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
	"usage": map[string]any{"prompt_tokens": 5, "total_tokens": 5, "cost": 0.00001},
}

var (
	decide = map[string]any{"state": "We have 3 open bugs and a demo tomorrow.", "questions": map[string]any{"ship": map[string]any{"type": "noul", "instructions": "Ship today?"}}}
	embed  = map[string]any{"input": []string{"espresso", "latte"}}
)

// upstreamReply is what the mock OpenRouter answers on one endpoint.
type upstreamReply struct {
	status int
	body   any
}

// mockOpenRouter records requests and answers from a per-path queue (the last reply repeats).
type mockOpenRouter struct {
	*httptest.Server
	mu      sync.Mutex
	replies map[string][]upstreamReply
	calls   map[string][]map[string]any
	headers map[string][]http.Header
	// gate, when set, blocks each POST until it is closed or receives a value.
	gate chan struct{}
}

func newMockOpenRouter(t *testing.T) *mockOpenRouter {
	m := &mockOpenRouter{
		replies: map[string][]upstreamReply{
			"/api/alpha/decisions": {{200, decisionBody}},
			"/api/v1/embeddings":   {{200, embeddingBody}},
		},
		calls:   map[string][]map[string]any{},
		headers: map[string][]http.Header{},
	}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data": []}`))
			return
		}
		m.mu.Lock()
		gate := m.gate
		m.mu.Unlock()
		if gate != nil {
			<-gate
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.calls[r.URL.Path] = append(m.calls[r.URL.Path], body)
		m.headers[r.URL.Path] = append(m.headers[r.URL.Path], r.Header.Clone())
		queue := m.replies[r.URL.Path]
		reply := upstreamReply{404, map[string]any{"error": map[string]any{"message": "no route"}}}
		if len(queue) > 0 {
			reply = queue[0]
			if len(queue) > 1 {
				m.replies[r.URL.Path] = queue[1:]
			}
		}
		m.mu.Unlock()
		w.WriteHeader(reply.status)
		_ = json.NewEncoder(w).Encode(reply.body)
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockOpenRouter) reply(path string, replies ...upstreamReply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies[path] = replies
}

func (m *mockOpenRouter) callCount(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls[path])
}

func (m *mockOpenRouter) last(path string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.calls[path]
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

// env is one deployment (barista or grinder) against a shared test database and mock OpenRouter.
type env struct {
	t        *testing.T
	gw       *Gateway
	srv      *httptest.Server
	upstream *mockOpenRouter
	store    *store.Store
}

type option func(map[string]string)

func with(k, v string) option { return func(m map[string]string) { m[k] = v } }

func newEnv(t *testing.T, service config.Service, opts ...option) *env {
	t.Helper()
	return newEnvShared(t, service, testdb.New(t), newMockOpenRouter(t), opts...)
}

// newEnvShared starts a deployment for service on an existing database and upstream.
func newEnvShared(t *testing.T, service config.Service, st *store.Store, up *mockOpenRouter, opts ...option) *env {
	t.Helper()
	vars := map[string]string{
		"BARISTA_SERVICE":          string(service),
		"OPEN_ROUTER_KEY":          "sk-or-test",
		"BARISTA_ADMIN_TOKEN":      adminToken,
		"OPENROUTER_BASE_URL":      up.URL + "/api/v1",
		"OPENROUTER_DECISIONS_URL": up.URL + "/api/alpha/decisions",
		"UPSTREAM_MAX_RETRIES":     "0",
		"UPSTREAM_BACKOFF_BASE_S":  "0",
		"BARISTA_MONTHLY_CAP_USD":  "20",
		"GRINDER_MONTHLY_CAP_USD":  "20",
	}
	for _, o := range opts {
		o(vars)
	}
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, st, openrouter.New(cfg, nil))
	gw.Client.Sleep = func(context.Context, time.Duration) error { return nil }
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, gw: gw, srv: srv, upstream: up, store: st}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return out
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (e *env) do(method, path string, body any, headers map[string]string) response {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rdr = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func adminHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + adminToken}
}

func caller(key string, surfaceAgent ...string) map[string]string {
	surface, agent := "slack", "alices-bot"
	if len(surfaceAgent) > 0 {
		surface = surfaceAgent[0]
	}
	if len(surfaceAgent) > 1 {
		agent = surfaceAgent[1]
	}
	return map[string]string{"Authorization": "Bearer " + key, "X-AGENT-SURFACE": surface, "X-AGENT-ID": agent}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// makeKey creates a key through the admin API.
func (e *env) makeKey(service config.Service, limits map[string]any) string {
	e.t.Helper()
	body := map[string]any{"service": string(service), "name": "alice"}
	for k, v := range limits {
		body[k] = v
	}
	r := e.do("POST", "/v1/admin/keys", body, adminHeaders())
	if r.status != http.StatusCreated {
		e.t.Fatalf("create key: %d %s", r.status, r.body)
	}
	return r.json(e.t)["api_key"].(string)
}

func (e *env) post(path string, body any, headers map[string]string) response {
	e.t.Helper()
	return e.do("POST", path, body, headers)
}

// usageBy returns admin usage groups keyed by group value.
func (e *env) usageBy(by string, query string) map[string]map[string]any {
	e.t.Helper()
	r := e.do("GET", "/v1/admin/usage?by="+by+query, nil, adminHeaders())
	if r.status != 200 {
		e.t.Fatalf("usage: %d %s", r.status, r.body)
	}
	out := map[string]map[string]any{}
	groups, _ := r.json(e.t)["groups"].([]any)
	for _, g := range groups {
		m := g.(map[string]any)
		name, _ := m["group"].(string)
		out[name] = m
	}
	return out
}

func (e *env) scalar(sql string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.store.Pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return s
}
