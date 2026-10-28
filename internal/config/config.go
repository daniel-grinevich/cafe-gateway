// Package config reads runtime configuration from environment variables (see .env.example).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Service is one of the two gateways served from this codebase. Same image, two deployments.
type Service string

const (
	Barista Service = "barista" // OpenRouter Decisions API, POST /v1/decide
	Grinder Service = "grinder" // OpenRouter embeddings, POST /v1/embed
)

var Services = []Service{Barista, Grinder}

func ParseService(s string) (Service, error) {
	switch Service(strings.ToLower(strings.TrimSpace(s))) {
	case Barista:
		return Barista, nil
	case Grinder:
		return Grinder, nil
	}
	return "", fmt.Errorf("service must be barista or grinder, got %q", s)
}

// KeyPrefix is the start of this service's API keys: bar_… or grd_….
func (s Service) KeyPrefix() string {
	if s == Barista {
		return "bar"
	}
	return "grd"
}

// Scope is the key scope that unlocks this service's model endpoint.
func (s Service) Scope() string {
	if s == Barista {
		return "decide"
	}
	return "embed"
}

// Scopes are the scopes a new key for this service gets.
func (s Service) Scopes() []string { return []string{s.Scope(), "usage"} }

// Endpoint is the model endpoint this service serves.
func (s Service) Endpoint() string { return "/v1/" + s.Scope() }

type Config struct {
	// Which gateway this process serves (BARISTA_SERVICE).
	Service Service
	Addr    string
	// Internal-only listener for per-key metrics ("" = off). Never route it through an ingress.
	InternalMetricsAddr string

	DatabaseURL   string
	OpenRouterKey string // never logged
	AdminToken    string // never logged
	// Hostnames this deployment answers on; empty = any. /health and /metrics are always reachable.
	AllowedHosts []string

	OpenRouterBaseURL      string
	OpenRouterDecisionsURL string
	OpenRouterAppURL       string
	OpenRouterAppTitle     string
	// Provider preference forced onto every upstream request; "" stops forcing it.
	OpenRouterDataCollection string

	// How often to poll OpenRouter's key endpoint for the shared key's real credit (0 = don't poll, no floor).
	OpenRouterPollInterval time.Duration
	// Below this much credit left on the key (last successful poll), upstream calls are refused with 402.
	OpenRouterMinRemaining decimal.Decimal

	UpstreamConnectTimeout time.Duration
	UpstreamReadTimeout    time.Duration
	UpstreamMaxRetries     int
	UpstreamBackoffBase    time.Duration

	// Model allow-lists; the first entry is the default model.
	BaristaModels []string
	GrinderModels []string

	// Global monthly caps per service (USD). Requests are refused once a cap would be crossed. 0 blocks everything.
	BaristaMonthlyCap decimal.Decimal
	GrinderMonthlyCap decimal.Decimal

	DefaultRPM                 int
	DefaultMaxTokensPerRequest int
	DefaultMonthlyBudget       decimal.Decimal

	// Price used when neither OpenRouter's usage.cost nor the model catalogue has one. Pessimistic on purpose.
	FallbackPricePerMTok decimal.Decimal
	PricingTTL           time.Duration
	// Output tokens assumed for a decision when reserving its cost before the call.
	DecideOutputAllowance int
	MaxEmbedInputs        int
	MaxBodyBytes          int64
	// How long an unreconciled reservation counts against a cap (a crashed pod's reservations expire).
	ReservationTTL time.Duration
	// Opt-in: store request bodies on usage rows. Off by default for privacy.
	DebugStoreBodies bool
	LogLevel         string
}

func (c *Config) Models(s Service) []string {
	if s == Barista {
		return c.BaristaModels
	}
	return c.GrinderModels
}

func (c *Config) MonthlyCap(s Service) decimal.Decimal {
	if s == Barista {
		return c.BaristaMonthlyCap
	}
	return c.GrinderMonthlyCap
}

// Load reads the environment. lookup is os.LookupEnv outside tests.
func Load(lookup func(string) (string, bool)) (*Config, error) {
	e := env{lookup: lookup}
	c := &Config{
		Addr:                       e.str("BARISTA_ADDR", ":8000"),
		InternalMetricsAddr:        offIsEmpty(e.str("BARISTA_INTERNAL_METRICS_ADDR", ":9090")),
		DatabaseURL:                NormalizeDatabaseURL(e.str("DATABASE_URL", "")),
		OpenRouterKey:              e.str("OPEN_ROUTER_KEY", ""),
		AdminToken:                 e.str("BARISTA_ADMIN_TOKEN", ""),
		AllowedHosts:               lower(e.list("BARISTA_ALLOWED_HOSTS", nil)),
		OpenRouterBaseURL:          strings.TrimRight(e.str("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"), "/"),
		OpenRouterDecisionsURL:     e.str("OPENROUTER_DECISIONS_URL", "https://openrouter.ai/api/alpha/decisions"),
		OpenRouterAppURL:           e.str("OPENROUTER_APP_URL", "https://github.com/daniel-grinevich/cafe-gateway"),
		OpenRouterAppTitle:         e.str("OPENROUTER_APP_TITLE", "cafe-gateway"),
		OpenRouterDataCollection:   e.str("OPENROUTER_DATA_COLLECTION", "deny"),
		OpenRouterPollInterval:     e.seconds("OPENROUTER_POLL_SECONDS", 60),
		OpenRouterMinRemaining:     e.money("OPENROUTER_MIN_REMAINING_USD", "0.50"),
		UpstreamConnectTimeout:     e.seconds("UPSTREAM_CONNECT_TIMEOUT_S", 5),
		UpstreamReadTimeout:        e.seconds("UPSTREAM_READ_TIMEOUT_S", 60),
		UpstreamMaxRetries:         e.int("UPSTREAM_MAX_RETRIES", 2),
		UpstreamBackoffBase:        e.seconds("UPSTREAM_BACKOFF_BASE_S", 0.5),
		BaristaModels:              e.list("BARISTA_MODELS", []string{"perplexity/pplx-decider-flash-v1.2"}),
		GrinderModels:              e.list("GRINDER_MODELS", []string{"qwen/qwen3-embedding-8b"}),
		BaristaMonthlyCap:          e.money("BARISTA_MONTHLY_CAP_USD", "10"),
		GrinderMonthlyCap:          e.money("GRINDER_MONTHLY_CAP_USD", "10"),
		DefaultRPM:                 e.int("DEFAULT_RPM", 30),
		DefaultMaxTokensPerRequest: e.int("DEFAULT_MAX_TOKENS_PER_REQUEST", 8000),
		DefaultMonthlyBudget:       e.money("DEFAULT_MONTHLY_BUDGET_USD", "1"),
		FallbackPricePerMTok:       e.money("FALLBACK_PRICE_PER_MTOK_USD", "1"),
		PricingTTL:                 e.seconds("PRICING_TTL_S", 6*3600),
		DecideOutputAllowance:      e.int("DECIDE_OUTPUT_TOKEN_ALLOWANCE", 512),
		MaxEmbedInputs:             e.int("MAX_EMBED_INPUTS", 256),
		MaxBodyBytes:               int64(e.int("MAX_BODY_BYTES", 4<<20)),
		ReservationTTL:             e.seconds("RESERVATION_TTL_S", 600),
		DebugStoreBodies:           e.bool("BARISTA_DEBUG_STORE_BODIES", false),
		LogLevel:                   e.str("LOG_LEVEL", "info"),
	}
	if raw, ok := lookup("BARISTA_SERVICE"); ok && strings.TrimSpace(raw) != "" {
		s, err := ParseService(raw)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("BARISTA_SERVICE: %w", err))
		}
		c.Service = s
	} else {
		c.Service = Barista
	}
	if len(c.BaristaModels) == 0 || len(c.GrinderModels) == 0 {
		e.errs = append(e.errs, errors.New("BARISTA_MODELS and GRINDER_MODELS must not be empty"))
	}
	for name, v := range map[string]decimal.Decimal{
		"BARISTA_MONTHLY_CAP_USD": c.BaristaMonthlyCap, "GRINDER_MONTHLY_CAP_USD": c.GrinderMonthlyCap,
		"DEFAULT_MONTHLY_BUDGET_USD": c.DefaultMonthlyBudget, "OPENROUTER_MIN_REMAINING_USD": c.OpenRouterMinRemaining,
	} {
		if v.IsNegative() {
			e.errs = append(e.errs, fmt.Errorf("%s must not be negative", name))
		}
	}
	if c.FallbackPricePerMTok.Sign() <= 0 {
		e.errs = append(e.errs, errors.New("FALLBACK_PRICE_PER_MTOK_USD must be positive"))
	}
	if c.DefaultRPM < 1 || c.DefaultMaxTokensPerRequest < 1 || c.MaxEmbedInputs < 1 || c.UpstreamMaxRetries < 0 {
		e.errs = append(e.errs, errors.New("DEFAULT_RPM, DEFAULT_MAX_TOKENS_PER_REQUEST and MAX_EMBED_INPUTS must be >= 1"))
	}
	return c, errors.Join(e.errs...)
}

// NormalizeDatabaseURL accepts SQLAlchemy-style URLs (postgresql+asyncpg://) too, so an old secret still works.
func NormalizeDatabaseURL(url string) string {
	for _, driver := range []string{"postgresql+asyncpg://", "postgresql+psycopg://", "postgres+asyncpg://"} {
		if rest, ok := strings.CutPrefix(url, driver); ok {
			return "postgresql://" + rest
		}
	}
	return url
}

type env struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (e *env) str(name, def string) string {
	if v, ok := e.lookup(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (e *env) list(name string, def []string) []string {
	v, ok := e.lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (e *env) int(name string, def int) int {
	raw := e.str(name, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not an integer: %q", name, raw))
		return def
	}
	return n
}

func (e *env) bool(name string, def bool) bool {
	raw := e.str(name, "")
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not a boolean: %q", name, raw))
		return def
	}
	return b
}

func (e *env) seconds(name string, def float64) time.Duration {
	raw := e.str(name, "")
	f := def
	if raw != "" {
		var err error
		if f, err = strconv.ParseFloat(raw, 64); err != nil || f < 0 {
			e.errs = append(e.errs, fmt.Errorf("%s: not a number of seconds: %q", name, raw))
			f = def
		}
	}
	return time.Duration(f * float64(time.Second))
}

func (e *env) money(name, def string) decimal.Decimal {
	raw := e.str(name, def)
	d, err := decimal.NewFromString(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not a number: %q", name, raw))
		return decimal.RequireFromString(def)
	}
	return d
}

// offIsEmpty lets an address be switched off with "off" (an empty env var means "use the default").
func offIsEmpty(addr string) string {
	if strings.EqualFold(addr, "off") {
		return ""
	}
	return addr
}

func lower(items []string) []string {
	for i, s := range items {
		items[i] = strings.ToLower(s)
	}
	return items
}

// FromEnv loads the process environment.
func FromEnv() (*Config, error) { return Load(os.LookupEnv) }
