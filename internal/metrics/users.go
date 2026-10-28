package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Label values used when the real value isn't known or isn't trusted.
const (
	Unauthenticated = "unauthenticated" // no key, unknown/forged/revoked key, or another service's key
	None            = "none"            // header or model not given (or rejected before it was read)
	Other           = "other"           // over the per-key surface/agent limit
)

// MaxAttributionsPerKey bounds cardinality: a key's first N distinct (surface, agent_id) pairs get their own
// series; later ones are folded into surface="other", agent_id="other".
const MaxAttributionsPerKey = 50

// Users holds per-key ("per user") metrics. They carry friends' key names, so they live in their own registry,
// served only on the internal listener (BARISTA_INTERNAL_METRICS_ADDR), never on the tailnet-facing :8000.
type Users struct {
	Registry *prometheus.Registry

	Requests        *prometheus.CounterVec
	Errors          *prometheus.CounterVec
	Duration        *prometheus.HistogramVec
	Tokens          *prometheus.CounterVec
	SpendUSD        *prometheus.CounterVec
	BudgetUSD       *prometheus.GaugeVec
	BudgetRemaining *prometheus.GaugeVec
	CapRemaining    *prometheus.GaugeVec

	mu   sync.Mutex
	seen map[string]map[[2]string]bool // key_id -> (surface, agent_id) pairs with their own series
}

// RequestLabels describes one model-endpoint request.
type RequestLabels struct {
	Service, KeyName, KeyID, Surface, AgentID, Model string
	Status                                           int
	ErrorCode                                        string // "" on success
}

var requestLabelNames = []string{"service", "key_name", "key_id", "surface", "agent_id", "model", "status"}

func NewUsers() *Users {
	u := &Users{
		Registry: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_user_requests_total", Help: "Model endpoint requests per key, surface, agent, model and HTTP status.",
		}, requestLabelNames),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_user_errors_total", Help: "Model endpoint requests that failed (status >= 400), with the gateway's error code.",
		}, append(append([]string{}, requestLabelNames...), "error_code")),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cafe_gateway_user_request_duration_seconds", Help: "Model endpoint latency per key and model.",
			Buckets: []float64{0.25, 1, 2.5, 5, 10, 30},
		}, []string{"service", "key_name", "key_id", "model"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_user_tokens_total", Help: "Tokens reported by OpenRouter per key (direction = input | output).",
		}, []string{"service", "key_name", "key_id", "surface", "agent_id", "model", "direction"}),
		SpendUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_user_spend_usd_total", Help: "Settled OpenRouter cost per key since process start.",
		}, []string{"service", "key_name", "key_id", "surface", "agent_id", "model"}),
		BudgetUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_user_budget_usd", Help: "Each active key's monthly budget.",
		}, []string{"service", "key_name", "key_id"}),
		BudgetRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_user_budget_remaining_usd", Help: "What's left of each active key's monthly budget (UTC month).",
		}, []string{"service", "key_name", "key_id"}),
		CapRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_cap_remaining_usd", Help: "What's left of the service's global monthly cap (spend and in-flight reservations deducted).",
		}, []string{"service"}),
		seen: map[string]map[[2]string]bool{},
	}
	u.Registry.MustRegister(u.Requests, u.Errors, u.Duration, u.Tokens, u.SpendUSD, u.BudgetUSD, u.BudgetRemaining, u.CapRemaining)
	return u
}

// bound normalizes labels so a series set stays small: unauthenticated callers collapse into one series, and each
// key gets at most MaxAttributionsPerKey (surface, agent_id) pairs.
func (u *Users) bound(l RequestLabels) RequestLabels {
	if l.KeyID == "" {
		l.KeyName, l.KeyID, l.Surface, l.AgentID = Unauthenticated, Unauthenticated, Unauthenticated, Unauthenticated
	}
	if l.Model == "" {
		l.Model = None
	}
	if l.Surface == "" {
		l.Surface = None
	}
	if l.AgentID == "" {
		l.AgentID = None
	}
	if l.KeyID == Unauthenticated {
		return l
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	pairs := u.seen[l.KeyID]
	if pairs == nil {
		pairs = map[[2]string]bool{}
		u.seen[l.KeyID] = pairs
	}
	pair := [2]string{l.Surface, l.AgentID}
	if !pairs[pair] {
		if len(pairs) >= MaxAttributionsPerKey {
			l.Surface, l.AgentID = Other, Other
			return l
		}
		pairs[pair] = true
	}
	return l
}

// ObserveRequest records one finished model-endpoint request.
func (u *Users) ObserveRequest(l RequestLabels, seconds float64, inputTokens, outputTokens int, costUSD float64) {
	l = u.bound(l)
	status := strconv.Itoa(l.Status)
	u.Requests.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Surface, l.AgentID, l.Model, status).Inc()
	if l.Status >= 400 {
		code := l.ErrorCode
		if code == "" {
			code = "unknown"
		}
		u.Errors.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Surface, l.AgentID, l.Model, status, code).Inc()
	}
	u.Duration.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Model).Observe(seconds)
	if inputTokens > 0 {
		u.Tokens.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Surface, l.AgentID, l.Model, "input").Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		u.Tokens.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Surface, l.AgentID, l.Model, "output").Add(float64(outputTokens))
	}
	if costUSD > 0 {
		u.SpendUSD.WithLabelValues(l.Service, l.KeyName, l.KeyID, l.Surface, l.AgentID, l.Model).Add(costUSD)
	}
}
