package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/openrouter"
)

// The OpenRouter key is shared (barista, grinder, bean, ledger, ...), so the gateway's own caps only see part of
// what's spent on it. The real limit lives at OpenRouter: we poll GET /api/v1/key and refuse upstream calls once
// what's left drops below OPENROUTER_MIN_REMAINING_USD. A failed poll never blocks (fail open); OpenRouter still
// enforces the hard limit itself.

// KeyAlertThresholds are the fractions of the key's credit limit whose crossing logs a WARNING.
var KeyAlertThresholds = []decimal.Decimal{decimal.RequireFromString("0.8"), decimal.RequireFromString("0.95")}

const (
	creditPollTimeout = 10 * time.Second
	maxCreditBackoff  = 15 * time.Minute
)

type creditState struct {
	mu       sync.Mutex
	info     *openrouter.KeyInfo // last successful poll
	okAt     time.Time           // when that was
	polledAt time.Time           // last attempt
	pollOK   bool                // the last attempt succeeded
	lastErr  string
	// low: the last successful poll showed less than the floor left. Calls are refused only while pollOK && low.
	low bool
}

// PollCredit fetches the key's credit once, updates the gauges, logs threshold and floor crossings, and decides
// whether upstream calls are refused.
func (g *Gateway) PollCredit(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, creditPollTimeout)
	defer cancel()
	info, err := g.Client.KeyInfo(pctx)
	now := g.Now()
	floor := g.Cfg.OpenRouterMinRemaining

	c := &g.credit
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polledAt = now
	g.Metrics.KeyMinRemainingUSD.WithLabelValues().Set(floor.InexactFloat64())
	if err != nil {
		if c.pollOK && c.low {
			slog.WarnContext(ctx, "OpenRouter key credit is unknown (poll failed); no longer refusing upstream calls",
				"event", "cafe_gateway_openrouter_credit_unknown", "error", err.Error())
		}
		c.pollOK, c.lastErr = false, err.Error()
		g.Metrics.KeyPollSuccess.WithLabelValues().Set(0)
		g.Metrics.KeyBlocked.WithLabelValues().Set(0)
		return err
	}
	prev := c.info
	c.info, c.okAt, c.pollOK, c.lastErr = info, now, true, ""

	g.Metrics.KeyPollSuccess.WithLabelValues().Set(1)
	g.Metrics.KeyLastPoll.WithLabelValues().Set(float64(now.UnixMilli()) / 1000)
	g.Metrics.KeyUsageUSD.WithLabelValues().Set(info.Usage.InexactFloat64())
	setOptional(g.Metrics.KeyUsageDailyUSD, info.UsageDaily)
	setOptional(g.Metrics.KeyUsageWeeklyUSD, info.UsageWeekly)
	setOptional(g.Metrics.KeyUsageMonthlyUSD, info.UsageMonthly)
	remaining, limited := info.Remaining()
	var remainingPtr, ratio *decimal.Decimal
	if limited {
		remainingPtr = &remaining
		if info.Limit != nil && info.Limit.IsPositive() {
			r := remaining.Div(*info.Limit)
			ratio = &r
		}
	}
	setOptional(g.Metrics.KeyLimitUSD, info.Limit)
	setOptional(g.Metrics.KeyRemainingUSD, remainingPtr)
	setOptional(g.Metrics.KeyRemainingRatio, ratio)

	before := decimal.Zero
	if prev != nil {
		before = usedRatio(prev)
	}
	for _, t := range KeyAlertThresholds {
		if after := usedRatio(info); before.LessThan(t) && after.GreaterThanOrEqual(t) {
			pct := int(t.Mul(decimal.NewFromInt(100)).IntPart())
			g.Metrics.KeyAlerts.WithLabelValues(strconv.Itoa(pct)).Inc()
			slog.WarnContext(ctx, "shared OpenRouter key usage crossed "+strconv.Itoa(pct)+"% of its credit limit",
				"event", "cafe_gateway_openrouter_key_threshold", "threshold_pct", pct, "usage_usd", info.Usage.String(),
				"limit_usd", info.Limit.String(), "remaining_usd", remaining.String())
		}
	}

	low := limited && remaining.LessThan(floor)
	switch {
	case low && !c.low:
		slog.WarnContext(ctx, "shared OpenRouter key credit is below the safety floor; refusing upstream calls",
			"event", "cafe_gateway_openrouter_credit_low", "service", string(g.service()),
			"remaining_usd", remaining.String(), "min_remaining_usd", floor.String())
	case !low && c.low:
		slog.InfoContext(ctx, "shared OpenRouter key credit is back above the safety floor; resuming upstream calls",
			"event", "cafe_gateway_openrouter_credit_ok", "service", string(g.service()),
			"remaining_usd", remaining.String(), "min_remaining_usd", floor.String())
	}
	c.low = low
	g.Metrics.KeyBlocked.WithLabelValues().Set(b2f(low))
	return nil
}

// setOptional sets an unlabelled gauge, or removes its series when the value is unknown.
func setOptional(g *prometheus.GaugeVec, v *decimal.Decimal) {
	if v == nil {
		g.Reset()
		return
	}
	g.WithLabelValues().Set(v.InexactFloat64())
}

// usedRatio is usage / limit, or 0 for a key without a (positive) limit.
func usedRatio(k *openrouter.KeyInfo) decimal.Decimal {
	if k.Limit == nil || !k.Limit.IsPositive() {
		return decimal.Zero
	}
	remaining, _ := k.Remaining()
	return decimal.NewFromInt(1).Sub(remaining.Div(*k.Limit))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// creditBlock is the 402 for a request while the shared key is below the floor, or nil.
func (g *Gateway) creditBlock() *APIError {
	c := &g.credit
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pollOK || !c.low {
		return nil
	}
	remaining, _ := c.info.Remaining()
	return apiError(http.StatusPaymentRequired, "upstream_credit_low",
		"The gateway's shared OpenRouter credit is nearly used up ($"+remaining.StringFixed(2)+
			" left). Calls resume automatically once the owner adds credit.").
		with("remaining_usd", remaining.StringFixed(8)).with("min_remaining_usd", g.Cfg.OpenRouterMinRemaining.String())
}

// RunCreditPoller polls every OPENROUTER_POLL_SECONDS until ctx is done, backing off (up to 15 min) while polls
// fail. It does nothing if polling is off or there is no OpenRouter key.
func (g *Gateway) RunCreditPoller(ctx context.Context) {
	interval := g.Cfg.OpenRouterPollInterval
	if interval <= 0 || !g.Client.Configured() {
		return
	}
	failures := 0
	for {
		delay := interval
		if err := g.PollCredit(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			delay = min(interval<<min(failures, 6), max(interval, maxCreditBackoff))
			slog.WarnContext(ctx, "OpenRouter key credit poll failed", "event", "cafe_gateway_openrouter_poll_failed",
				"error", err.Error(), "failures", failures, "next_poll_s", int(delay.Seconds()))
		} else {
			failures = 0
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// creditJSON is the key credit section of GET /v1/admin/spend. Without a successful poll yet (or with polling off)
// it fetches the key info directly, without touching the poller's state.
func (g *Gateway) creditJSON(ctx context.Context) map[string]any {
	c := &g.credit
	c.mu.Lock()
	info, okAt, polledAt, pollOK, lastErr, low := c.info, c.okAt, c.polledAt, c.pollOK, c.lastErr, c.low
	c.mu.Unlock()
	polling := g.Cfg.OpenRouterPollInterval > 0
	if info == nil {
		pctx, cancel := context.WithTimeout(ctx, creditPollTimeout)
		defer cancel()
		live, err := g.Client.KeyInfo(pctx)
		if err != nil {
			return map[string]any{"error": err.Error(), "polling": polling, "blocking": false,
				"min_remaining_usd": g.Cfg.OpenRouterMinRemaining.String()}
		}
		info, okAt = live, g.Now()
	}
	out := KeyCreditJSON(info, g.Cfg.OpenRouterMinRemaining)
	out["polling"] = polling
	out["poll_ok"] = pollOK
	out["blocking"] = pollOK && low
	out["checked_at"] = okAt.UTC().Format(time.RFC3339)
	if !polledAt.IsZero() {
		out["last_poll_at"] = polledAt.UTC().Format(time.RFC3339)
	}
	if lastErr != "" {
		out["poll_error"] = lastErr
	}
	return out
}

// KeyCreditJSON describes the shared key's credit for the admin API and the CLI. Amounts are strings (USD), null
// when OpenRouter doesn't report them (a key with no limit has no limit or remaining).
func KeyCreditJSON(info *openrouter.KeyInfo, floor decimal.Decimal) map[string]any {
	usd := func(d *decimal.Decimal) any {
		if d == nil {
			return nil
		}
		return d.StringFixed(8)
	}
	out := map[string]any{
		"note":              "This OpenRouter key is shared with other apps; its limit, not the gateway's caps, is the real one.",
		"limit_usd":         usd(info.Limit),
		"usage_usd":         info.Usage.StringFixed(8),
		"usage_daily_usd":   usd(info.UsageDaily),
		"usage_weekly_usd":  usd(info.UsageWeekly),
		"usage_monthly_usd": usd(info.UsageMonthly),
		"limit_reset":       info.LimitReset,
		"min_remaining_usd": floor.String(),
		"remaining_usd":     nil,
		"remaining_ratio":   nil,
	}
	remaining, limited := info.Remaining()
	if !limited {
		out["status"] = "unlimited"
		return out
	}
	out["remaining_usd"] = remaining.StringFixed(8)
	if info.Limit != nil && info.Limit.IsPositive() {
		out["remaining_ratio"] = remaining.Div(*info.Limit).Round(4).InexactFloat64()
	}
	used := usedRatio(info)
	switch {
	case remaining.LessThan(floor):
		out["status"] = "below_floor"
	case used.GreaterThanOrEqual(KeyAlertThresholds[1]):
		out["status"] = "critical"
	case used.GreaterThanOrEqual(KeyAlertThresholds[0]):
		out["status"] = "warning"
	default:
		out["status"] = "ok"
	}
	return out
}
