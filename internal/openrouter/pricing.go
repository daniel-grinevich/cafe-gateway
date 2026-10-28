package openrouter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

var mtok = decimal.NewFromInt(1_000_000)

// Price is USD per token (prompt, completion) and per request.
type Price struct {
	Prompt, Completion, Request decimal.Decimal
}

type Cost struct {
	USD decimal.Decimal
	// openrouter (usage.cost), pricing (catalogue) or fallback.
	Source string
}

// Pricing caches the model catalogue's prices. Cost prefers OpenRouter's reported usage.cost, then the catalogue,
// then a pessimistic fallback so budgets still bite when both are missing.
type Pricing struct {
	cfg       *config.Config
	client    *Client
	mu        sync.Mutex
	prices    map[string]Price
	fetchedAt time.Time
}

func NewPricing(cfg *config.Config, client *Client) *Pricing {
	return &Pricing{cfg: cfg, client: client, prices: map[string]Price{}}
}

// SetPrices replaces the catalogue (tests, or a manual refresh).
func (p *Pricing) SetPrices(prices map[string]Price) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prices = prices
	p.fetchedAt = time.Now()
}

func (p *Pricing) price(ctx context.Context, model string) (Price, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.fetchedAt) > p.cfg.PricingTTL {
		p.refresh(ctx)
	}
	price, ok := p.prices[model]
	return price, ok
}

// refresh runs with p.mu held. A failed fetch is retried after a few minutes, not on every call, so a dead
// catalogue doesn't add latency to every request.
func (p *Pricing) refresh(ctx context.Context) {
	p.fetchedAt = time.Now()
	if !p.client.Configured() {
		return
	}
	// Detached from the caller: a client hanging up mustn't leave the cache empty for a whole TTL.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	prices := map[string]Price{}
	for _, m := range p.client.ListModels(ctx) {
		raw, _ := m["pricing"].(map[string]any)
		price := Price{Prompt: dec(raw["prompt"]), Completion: dec(raw["completion"]), Request: dec(raw["request"])}
		if id, ok := m["id"].(string); ok {
			prices[id] = price
		}
		if slug, ok := m["canonical_slug"].(string); ok && slug != "" {
			if _, exists := prices[slug]; !exists {
				prices[slug] = price
			}
		}
	}
	if len(prices) > 0 {
		p.prices = prices
		return
	}
	// Nothing fetched: try again in a few minutes rather than a full TTL.
	p.fetchedAt = time.Now().Add(min(5*time.Minute, p.cfg.PricingTTL) - p.cfg.PricingTTL)
}

func (p *Pricing) fallbackPerToken() decimal.Decimal { return p.cfg.FallbackPricePerMTok.Div(mtok) }

// Estimate is the cost to reserve before a call: input tokens plus an output allowance. Unknown or zero prices
// use the fallback, so a reservation is never free.
func (p *Pricing) Estimate(ctx context.Context, model string, inputTokens, outputTokens int) decimal.Decimal {
	in, out := decimal.NewFromInt(int64(inputTokens)), decimal.NewFromInt(int64(outputTokens))
	price, ok := p.price(ctx, model)
	if !ok || (price.Prompt.IsZero() && price.Completion.IsZero() && price.Request.IsZero()) {
		return p.fallbackPerToken().Mul(in.Add(out))
	}
	return price.Prompt.Mul(in).Add(price.Completion.Mul(out)).Add(price.Request)
}

// Cost is the real cost of a finished call. reported is usage.cost from the response (nil if absent).
func (p *Pricing) Cost(ctx context.Context, model string, inputTokens, outputTokens int, reported any) Cost {
	if reported != nil {
		if d, err := decimal.NewFromString(fmt.Sprint(reported)); err == nil && !d.IsNegative() {
			return Cost{USD: d, Source: "openrouter"}
		}
	}
	in, out := decimal.NewFromInt(int64(inputTokens)), decimal.NewFromInt(int64(outputTokens))
	price, ok := p.price(ctx, model)
	if !ok {
		return Cost{USD: p.fallbackPerToken().Mul(in.Add(out)), Source: "fallback"}
	}
	return Cost{USD: price.Prompt.Mul(in).Add(price.Completion.Mul(out)).Add(price.Request), Source: "pricing"}
}

func dec(v any) decimal.Decimal {
	if v == nil {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(fmt.Sprint(v))
	if err != nil {
		return decimal.Zero
	}
	return d
}
