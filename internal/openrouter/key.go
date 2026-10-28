package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/shopspring/decimal"
)

// KeyInfo is the credit state of the gateway's OpenRouter key (GET /api/v1/key). The key is shared with other apps,
// so Usage is everything spent on it, not just the gateway's share. Limit and LimitRemaining are nil when the key
// has no credit limit. The label (a masked copy of the key) is deliberately not decoded.
type KeyInfo struct {
	Limit          *decimal.Decimal `json:"limit"`
	LimitRemaining *decimal.Decimal `json:"limit_remaining"`
	LimitReset     *string          `json:"limit_reset"`
	Usage          decimal.Decimal  `json:"usage"`
	UsageDaily     *decimal.Decimal `json:"usage_daily"`
	UsageWeekly    *decimal.Decimal `json:"usage_weekly"`
	UsageMonthly   *decimal.Decimal `json:"usage_monthly"`
	IsFreeTier     bool             `json:"is_free_tier"`
}

// Remaining is limit_remaining, or limit - usage if OpenRouter left it out. ok is false for a key with no limit.
func (k *KeyInfo) Remaining() (decimal.Decimal, bool) {
	switch {
	case k.LimitRemaining != nil:
		return *k.LimitRemaining, true
	case k.Limit != nil:
		return decimal.Max(decimal.Zero, k.Limit.Sub(k.Usage)), true
	}
	return decimal.Zero, false
}

// KeyInfo fetches the key's limit and usage once, without retries (the poller backs off on its own).
func (c *Client) KeyInfo(ctx context.Context) (*KeyInfo, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("%w: OPEN_ROUTER_KEY is not set", ErrUnavailable)
	}
	req, err := c.newRequest(ctx, http.MethodGet, c.cfg.OpenRouterBaseURL+"/key", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, errorKind(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("openrouter key info: status %d", resp.StatusCode)
	}
	var body struct {
		Data *KeyInfo `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.Data == nil {
		return nil, fmt.Errorf("openrouter key info: unreadable response")
	}
	return body.Data, nil
}
