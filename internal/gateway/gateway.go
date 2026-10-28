// Package gateway is the request pipeline and HTTP API shared by barista (/v1/decide) and grinder (/v1/embed).
//
// Per request: auth (401, wrong service 403) -> attribution headers (400) -> body (422/400) -> scope (403)
// -> rate limit (429) -> model allow-list (400) -> max tokens (413) -> reserve estimated cost against the key's
// budget and the service cap (402) -> OpenRouter -> settle the reservation with the real cost -> 80%/100% alerts.
// Every step after auth writes a usage row, rejections included.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/keys"
	"github.com/daniel-grinevich/cafe-gateway/internal/metrics"
	"github.com/daniel-grinevich/cafe-gateway/internal/openrouter"
	"github.com/daniel-grinevich/cafe-gateway/internal/ratelimit"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
)

// Token estimate before the call, when real counts aren't known yet.
const (
	charsPerToken  = 4
	tokensPerAsset = 1000
)

type Gateway struct {
	Cfg     *config.Config
	Store   *store.Store
	Client  *openrouter.Client
	Pricing *openrouter.Pricing
	Limiter *ratelimit.Limiter
	Metrics *metrics.Metrics
	// Per-key metrics, served only on the internal listener.
	Users *metrics.Users
	Now   func() time.Time
}

func New(cfg *config.Config, st *store.Store, client *openrouter.Client) *Gateway {
	return &Gateway{
		Cfg: cfg, Store: st, Client: client, Pricing: openrouter.NewPricing(cfg, client),
		Limiter: ratelimit.New(nil), Metrics: metrics.New(), Users: metrics.NewUsers(), Now: time.Now,
	}
}

func (g *Gateway) service() config.Service { return g.Cfg.Service }

// --- auth ---

func bearer(h http.Header) string {
	auth := strings.TrimSpace(h.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// authenticate looks up the caller's key. It must belong to this deployment's service.
func (g *Gateway) authenticate(ctx context.Context, h http.Header) (*store.APIKey, *APIError) {
	plaintext := bearer(h)
	if plaintext == "" {
		return nil, apiError(http.StatusUnauthorized, "missing_api_key",
			"Send your key as 'Authorization: Bearer <key>'.").header("WWW-Authenticate", "Bearer")
	}
	invalid := apiError(http.StatusUnauthorized, "invalid_api_key", "API key is malformed or unknown.")
	parsed, ok := keys.Parse(plaintext)
	if !ok {
		return nil, invalid
	}
	key, err := g.Store.KeyByKeyID(ctx, parsed.KeyID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalid
	}
	if err != nil {
		slog.ErrorContext(ctx, "key lookup failed", "error", err)
		return nil, storeUnavailable()
	}
	if key.Service != parsed.Service || !keys.Verify(plaintext, key.KeyHash) {
		return nil, invalid
	}
	if key.RevokedAt != nil {
		return nil, apiError(http.StatusUnauthorized, "revoked_api_key", "This API key has been revoked.")
	}
	if key.Service != g.service() {
		return nil, apiError(http.StatusForbidden, "wrong_service", "This is a "+string(key.Service)+
			" key; it only works on "+string(key.Service)+", not "+string(g.service())+".")
	}
	if info := requestInfoFrom(ctx); info != nil {
		info.keyID, info.keyName = key.KeyID, key.Name
	}
	return key, nil
}

func storeUnavailable() *APIError {
	return apiError(http.StatusServiceUnavailable, "store_unavailable", "The usage database is unavailable; try again shortly.")
}

// --- request bodies ---

// call is the service-specific part of a request.
type call struct {
	payload         map[string]any
	inputTokens     int
	outputAllowance int
	send            func(context.Context, map[string]any) (*openrouter.Response, error)
	// (input_tokens, output_tokens, reported_cost) from the upstream body.
	readUsage func(map[string]any) (int, int, any)
}

func decodeObject(body []byte) (map[string]any, *APIError) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil || payload == nil {
		return nil, invalidRequest("Request body must be a JSON object.")
	}
	return payload, nil
}

func invalidRequest(msg string) *APIError {
	return apiError(http.StatusUnprocessableEntity, "invalid_request", msg)
}

func estimateTokens(v any) int {
	var n int
	if s, ok := v.(string); ok {
		n = len(s)
	} else {
		b, _ := json.Marshal(v)
		n = len(b)
	}
	return max(1, (n+charsPerToken-1)/charsPerToken)
}

func (g *Gateway) resolveModel(payload map[string]any) (string, *APIError) {
	allowed := g.Cfg.Models(g.service())
	raw, present := payload["model"]
	if !present || raw == nil {
		return allowed[0], nil
	}
	model, ok := raw.(string)
	if !ok {
		return "", invalidRequest("model must be a string.")
	}
	for _, m := range allowed {
		if m == model {
			return model, nil
		}
	}
	return "", apiError(http.StatusBadRequest, "model_not_allowed", "Model '"+model+"' is not allowed.").
		with("allowed_models", allowed)
}

// buildDecide validates an OpenRouter Decisions request: {state, questions, model?, assets?, session_id?, ...}.
func (g *Gateway) buildDecide(payload map[string]any) (*call, *APIError) {
	if s, ok := payload["state"]; !ok || s == nil {
		return nil, invalidRequest("'state' is required.")
	}
	questions, ok := payload["questions"].(map[string]any)
	if !ok || len(questions) == 0 {
		return nil, invalidRequest("'questions' must be an object with at least one question.")
	}
	rest := map[string]any{}
	assets := 0
	for k, v := range payload {
		switch k {
		case "model":
		case "assets":
			// {"<ref>": {type, format, data_base64}}. Images are priced per image, not per base64 character.
			switch a := v.(type) {
			case map[string]any:
				assets = len(a)
			case []any:
				assets = len(a)
			case nil:
			default:
				assets = 1
			}
		default:
			rest[k] = v
		}
	}
	return &call{
		payload:         payload,
		inputTokens:     estimateTokens(rest) + assets*tokensPerAsset,
		outputAllowance: g.Cfg.DecideOutputAllowance,
		send:            g.Client.Decide,
		readUsage: func(b map[string]any) (int, int, any) {
			u, _ := b["usage"].(map[string]any)
			return intField(u, "input_tokens"), intField(u, "output_tokens"), u["cost"]
		},
	}, nil
}

// buildEmbed validates an OpenAI-style embeddings request: {input: str | [str], model?, dimensions?, ...}.
func (g *Gateway) buildEmbed(payload map[string]any) (*call, *APIError) {
	switch input := payload["input"].(type) {
	case string:
		if input == "" {
			return nil, invalidRequest("'input' must not be empty.")
		}
	case []any:
		if len(input) == 0 {
			return nil, invalidRequest("'input' must not be empty.")
		}
		if len(input) > g.Cfg.MaxEmbedInputs {
			return nil, apiError(http.StatusBadRequest, "too_many_inputs",
				"At most "+strconv.Itoa(g.Cfg.MaxEmbedInputs)+" inputs per request.")
		}
		for _, item := range input {
			if _, ok := item.(string); !ok {
				return nil, invalidRequest("'input' must be a string or a list of strings.")
			}
		}
	default:
		return nil, invalidRequest("'input' must be a string or a list of strings.")
	}
	return &call{
		payload:     payload,
		inputTokens: estimateTokens(payload["input"]),
		send:        g.Client.Embed,
		readUsage: func(b map[string]any) (int, int, any) {
			u, _ := b["usage"].(map[string]any)
			return intField(u, "prompt_tokens"), 0, u["cost"]
		},
	}, nil
}

func intField(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case float64:
		return int(v)
	}
	return 0
}

// applyProviderPrefs forces data_collection (a caller can't opt the account into it) and sets a stable per-key
// end-user id for OpenRouter's abuse tooling unless the caller sent one.
func (g *Gateway) applyProviderPrefs(payload map[string]any, key *store.APIKey) {
	provider := map[string]any{}
	if p, ok := payload["provider"].(map[string]any); ok {
		for k, v := range p {
			provider[k] = v
		}
	}
	if g.Cfg.OpenRouterDataCollection != "" {
		provider["data_collection"] = g.Cfg.OpenRouterDataCollection
	}
	if len(provider) > 0 {
		payload["provider"] = provider
	}
	if _, ok := payload["user"]; !ok {
		payload["user"] = string(key.Service) + ":" + key.KeyID
	}
}

// --- pipeline ---

type result struct {
	body            json.RawMessage
	cost            decimal.Decimal
	budgetRemaining decimal.Decimal
}

// handleModelCall runs everything after auth for /v1/decide or /v1/embed.
func (g *Gateway) handleModelCall(ctx context.Context, key *store.APIKey, h http.Header, body []byte) (*result, *APIError) {
	started := time.Now()
	row := store.UsageRow{APIKeyID: key.ID, Service: key.Service, Endpoint: g.service().Endpoint()}
	if g.Cfg.DebugStoreBodies {
		row.RequestBody = strings.ToValidUTF8(string(body), "\uFFFD")
	}
	// Usage rows and settlement must happen even if the caller hangs up.
	bg := context.WithoutCancel(ctx)
	finish := func(status int, e *APIError) {
		row.StatusCode = status
		if e != nil {
			row.ErrorCode = e.Code
		}
		row.LatencyMS = int(time.Since(started).Milliseconds())
		if info := requestInfoFrom(ctx); info != nil {
			info.model, info.inputTokens, info.outputTokens, info.costUSD = row.Model, row.InputTokens, row.OutputTokens, row.CostUSD
		}
	}
	reject := func(e *APIError) (*result, *APIError) {
		finish(e.Status, e)
		if err := g.Store.RecordUsage(bg, row); err != nil {
			slog.ErrorContext(ctx, "failed to record usage", "error", err, "key_id", key.KeyID)
		}
		return nil, e
	}

	attr, e := readAttribution(h)
	if e != nil {
		return reject(e)
	}
	row.Surface, row.AgentID = attr.Surface, attr.AgentID
	if info := requestInfoFrom(ctx); info != nil {
		info.surface, info.agent = attr.Surface, attr.AgentID
	}
	payload, e := decodeObject(body)
	if e != nil {
		return reject(e)
	}
	model, e := g.resolveModel(payload)
	if e != nil {
		return reject(e)
	}
	payload["model"] = model
	row.Model = model
	var c *call
	if g.service() == config.Barista {
		c, e = g.buildDecide(payload)
	} else {
		c, e = g.buildEmbed(payload)
	}
	if e != nil {
		return reject(e)
	}
	if !key.HasScope(g.service().Scope()) {
		return reject(apiError(http.StatusForbidden, "insufficient_scope", "This key lacks the '"+g.service().Scope()+"' scope."))
	}
	if wait := g.Limiter.Allow(key.ID, key.RPM); wait > 0 {
		return reject(apiError(http.StatusTooManyRequests, "rate_limited",
			"Rate limit of "+strconv.Itoa(key.RPM)+" requests/minute exceeded.").
			header("Retry-After", ratelimit.RetryAfter(wait)).
			with("retry_after_s", float64(wait.Milliseconds())/1000))
	}
	if c.inputTokens > key.MaxTokensPerRequest {
		return reject(apiError(http.StatusRequestEntityTooLarge, "too_many_tokens",
			"Request is ~"+strconv.Itoa(c.inputTokens)+" tokens; this key allows "+
				strconv.Itoa(key.MaxTokensPerRequest)+" per request.").
			with("estimated_tokens", c.inputTokens).with("max_tokens_per_request", key.MaxTokensPerRequest))
	}
	if !g.Client.Configured() {
		return reject(apiError(http.StatusServiceUnavailable, "upstream_not_configured", "OPEN_ROUTER_KEY is not configured."))
	}

	estimate := g.Pricing.Estimate(ctx, model, c.inputTokens, c.outputAllowance)
	cap := g.Cfg.MonthlyCap(g.service())
	res, err := g.Store.Reserve(bg, key, cap, estimate, g.Cfg.ReservationTTL, g.Now())
	var over *store.OverLimitError
	switch {
	case errors.As(err, &over) && over.Scope == "key":
		return reject(apiError(http.StatusPaymentRequired, "budget_exceeded",
			"This key's monthly budget of $"+key.MonthlyBudgetUSD.StringFixed(2)+" is used up ($"+
				over.Snapshot.Spent.StringFixed(4)+" spent this month). It resets on the 1st (UTC).").
			with("budget_usd", key.MonthlyBudgetUSD.String()).with("spent_usd", over.Snapshot.Spent.StringFixed(8)))
	case errors.As(err, &over):
		slog.WarnContext(ctx, "service monthly cap reached; refusing request",
			"event", "cafe_gateway_cap_refused", "service", string(g.service()),
			"spent_usd", over.Snapshot.Spent.String(), "reserved_usd", over.Snapshot.Reserved.String(),
			"cap_usd", cap.String(), "estimate_usd", estimate.String())
		return reject(apiError(http.StatusPaymentRequired, "service_budget_exceeded",
			string(g.service())+" has hit its global monthly cap. Try again next month or ask the owner to raise it."))
	case err != nil:
		slog.ErrorContext(ctx, "budget reservation failed", "error", err)
		return reject(storeUnavailable())
	}

	g.applyProviderPrefs(payload, key)
	// Detached from the caller: once OpenRouter has the request we want its cost, even if the caller hangs up.
	upstream, err := c.send(bg, payload)
	var out *result
	switch {
	case err != nil:
		slog.WarnContext(ctx, "openrouter unreachable", "error", err)
		e = apiError(http.StatusBadGateway, "upstream_unavailable", "Could not reach OpenRouter.")
		g.Metrics.Upstream.WithLabelValues(string(g.service()), "unreachable").Inc()
	default:
		g.Metrics.Upstream.WithLabelValues(string(g.service()), strconv.Itoa(upstream.Status)).Inc()
		if upstream.Status >= 400 {
			e = upstreamError(ctx, upstream)
			break
		}
		var parsed map[string]any
		dec := json.NewDecoder(bytes.NewReader(upstream.Body))
		dec.UseNumber()
		_ = dec.Decode(&parsed)
		in, outTok, reported := c.readUsage(parsed)
		cost := g.Pricing.Cost(ctx, model, in, outTok, reported)
		row.InputTokens, row.OutputTokens, row.CostUSD, row.CostSource = in, outTok, cost.USD, cost.Source
		if id, ok := parsed["id"].(string); ok {
			row.UpstreamID = id
		}
		out = &result{body: upstream.Body, cost: cost.USD}
	}
	if e != nil {
		finish(e.Status, e)
	} else {
		finish(http.StatusOK, nil)
	}
	settled, err := g.Store.Settle(bg, res.ID, row, g.Now())
	if err != nil {
		// The reservation stays until RESERVATION_TTL_S, so the cap still counts this request meanwhile.
		slog.ErrorContext(ctx, "failed to settle usage", "error", err, "key_id", key.KeyID, "cost_usd", row.CostUSD.String())
	} else {
		g.afterSettle(ctx, key, cap, settled, row)
	}
	if e != nil {
		return nil, e
	}
	out.budgetRemaining = decimal.Max(decimal.Zero, key.MonthlyBudgetUSD.Sub(settled.KeyAfter))
	if err != nil {
		out.budgetRemaining = decimal.Max(decimal.Zero, res.Key.Remaining().Sub(row.CostUSD))
	}
	return out, nil
}

// upstreamError maps an OpenRouter error to the caller's error. For the caller's own mistakes (400-class) it
// passes OpenRouter's reason through, sanitized, so they can fix the request.
func upstreamError(ctx context.Context, up *openrouter.Response) *APIError {
	message, detail := upstreamReason(up.Body)
	switch up.Status {
	case 400, 404, 413, 422:
		full := "OpenRouter rejected the request: " + message
		if detail != "" {
			full += " (" + detail + ")"
		}
		slog.InfoContext(ctx, "OpenRouter rejected a request", "status", up.Status, "message", message, "detail", detail)
		e := apiError(up.Status, "upstream_rejected", full).with("upstream_status", up.Status).with("upstream_message", message)
		if detail != "" {
			e = e.with("upstream_detail", detail)
		}
		return e
	case 401, 402, 403:
		// Our OpenRouter key or credits, not the caller's fault. Don't leak details.
		slog.ErrorContext(ctx, "OpenRouter refused the gateway's key", "status", up.Status, "message", message, "detail", detail)
		return apiError(http.StatusBadGateway, "upstream_auth", "The gateway's upstream account was refused.")
	case 429:
		return apiError(http.StatusServiceUnavailable, "upstream_rate_limited", "OpenRouter is rate limiting; retry shortly.").
			header("Retry-After", "5")
	}
	slog.WarnContext(ctx, "OpenRouter error", "status", up.Status, "message", message, "detail", detail)
	return apiError(http.StatusBadGateway, "upstream_error", message).with("upstream_status", up.Status)
}

const maxUpstreamText = 600

// upstreamReason pulls (message, detail) out of an OpenRouter error body {"error": {"code", "message", "metadata"}},
// both sanitized. metadata usually holds the specifics (e.g. metadata.raw from the provider).
func upstreamReason(raw []byte) (string, string) {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	message, detail := "OpenRouter returned an error.", ""
	if json.Unmarshal(raw, &body) != nil || len(body.Error) == 0 {
		return message, ""
	}
	var asString string
	if json.Unmarshal(body.Error, &asString) == nil {
		if m := sanitizeUpstream(asString); m != "" {
			message = m
		}
		return message, ""
	}
	var e struct {
		Message  any `json:"message"`
		Metadata any `json:"metadata"`
	}
	if json.Unmarshal(body.Error, &e) != nil {
		return message, ""
	}
	if m := sanitizeUpstream(textOf(e.Message)); m != "" {
		message = m
	}
	if md, ok := e.Metadata.(map[string]any); ok {
		// The provider's own words, if present, are the most useful part; otherwise show the whole metadata.
		if r := textOf(md["raw"]); r != "" {
			detail = r
		} else if len(md) > 0 {
			detail = textOf(md)
		}
	} else if e.Metadata != nil {
		detail = textOf(e.Metadata)
	}
	detail = sanitizeUpstream(detail)
	if detail == message {
		detail = ""
	}
	return message, detail
}

func textOf(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	b, _ := json.Marshal(v)
	return string(b)
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-or-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`\b(bar|grd)_[a-z0-9]{12}_[A-Za-z0-9_-]+`),
}

// sanitizeUpstream makes upstream text safe to show a caller: no key-like strings, no control characters, one
// line, bounded length.
func sanitizeUpstream(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxUpstreamText {
		s = string(r[:maxUpstreamText]) + "…"
	}
	return s
}
