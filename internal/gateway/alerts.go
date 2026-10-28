package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
)

// AlertThresholds are the fractions of a cap or budget that trigger a WARNING when crossed.
var AlertThresholds = []decimal.Decimal{decimal.RequireFromString("0.8"), decimal.RequireFromString("1")}

// crossed returns the thresholds (as percentages) that spend passed going from before to after.
func crossed(limit, before, after decimal.Decimal) []int {
	if !limit.IsPositive() {
		return nil
	}
	var out []int
	for _, t := range AlertThresholds {
		mark := limit.Mul(t)
		if before.LessThan(mark) && after.GreaterThanOrEqual(mark) {
			out = append(out, int(t.Mul(decimal.NewFromInt(100)).IntPart()))
		}
	}
	return out
}

// afterSettle updates metrics and logs threshold alerts. Settle runs under the service lock, so each crossing is
// reported by exactly one request, across replicas.
func (g *Gateway) afterSettle(ctx context.Context, key *store.APIKey, cap decimal.Decimal, s store.Settlement, row store.UsageRow) {
	svc := string(key.Service)
	if row.CostUSD.IsPositive() {
		g.Metrics.CostUSD.WithLabelValues(svc, row.Model, row.CostSource).Add(row.CostUSD.InexactFloat64())
	}
	g.setSpendGauges(key.Service, cap, s.ServiceAfter, decimal.Zero, false)
	g.setKeyBudget(key, s.KeyAfter)
	for _, pct := range crossed(cap, s.ServiceBefore, s.ServiceAfter) {
		g.Metrics.CapAlerts.WithLabelValues(svc, strconv.Itoa(pct)).Inc()
		slog.WarnContext(ctx, "service monthly spend crossed "+strconv.Itoa(pct)+"% of its cap",
			"event", "cafe_gateway_cap_threshold", "service", svc, "threshold_pct", pct,
			"spent_usd", s.ServiceAfter.String(), "cap_usd", cap.String())
	}
	for _, pct := range crossed(key.MonthlyBudgetUSD, s.KeyBefore, s.KeyAfter) {
		g.Metrics.BudgetAlerts.WithLabelValues(svc, strconv.Itoa(pct)).Inc()
		slog.WarnContext(ctx, "key monthly spend crossed "+strconv.Itoa(pct)+"% of its budget",
			"event", "cafe_gateway_key_budget_threshold", "service", svc, "key_id", key.KeyID, "key_name", key.Name,
			"threshold_pct", pct, "spent_usd", s.KeyAfter.String(), "budget_usd", key.MonthlyBudgetUSD.String())
	}
}

func (g *Gateway) setSpendGauges(service config.Service, cap, spent, reserved decimal.Decimal, withReserved bool) {
	svc := string(service)
	g.Metrics.SpendUSD.WithLabelValues(svc).Set(spent.InexactFloat64())
	g.Metrics.CapUSD.WithLabelValues(svc).Set(cap.InexactFloat64())
	if withReserved {
		g.Metrics.ReservedUSD.WithLabelValues(svc).Set(reserved.InexactFloat64())
	}
	g.Users.CapRemaining.WithLabelValues(svc).Set(decimal.Max(decimal.Zero, cap.Sub(spent).Sub(reserved)).InexactFloat64())
	ratio := 1.0
	if cap.IsPositive() {
		ratio = spent.Div(cap).InexactFloat64()
	}
	g.Metrics.CapRatio.WithLabelValues(svc).Set(ratio)
}

// RefreshSpend reloads this service's spend gauges from Postgres (other replicas' spend, month rollover) and
// prunes expired reservations and idle rate-limit buckets.
func (g *Gateway) RefreshSpend(ctx context.Context) error {
	now := g.Now()
	if _, err := g.Store.PruneReservations(ctx, now); err != nil {
		return err
	}
	g.Limiter.Prune(10 * time.Minute)
	spent, held, err := g.Store.ServiceSpend(ctx, g.service(), now)
	if err != nil {
		return err
	}
	g.setSpendGauges(g.service(), g.Cfg.MonthlyCap(g.service()), spent, held, true)
	keys, err := g.Store.ActiveKeySpends(ctx, g.service(), now)
	if err != nil {
		return err
	}
	// Rebuild from scratch so revoked keys' series disappear.
	g.Users.BudgetUSD.Reset()
	g.Users.BudgetRemaining.Reset()
	for _, k := range keys {
		g.setKeyBudget(k.Key, k.Spent)
	}
	return nil
}

func (g *Gateway) setKeyBudget(key *store.APIKey, spent decimal.Decimal) {
	svc := string(key.Service)
	g.Users.BudgetUSD.WithLabelValues(svc, key.Name, key.KeyID).Set(key.MonthlyBudgetUSD.InexactFloat64())
	g.Users.BudgetRemaining.WithLabelValues(svc, key.Name, key.KeyID).
		Set(decimal.Max(decimal.Zero, key.MonthlyBudgetUSD.Sub(spent)).InexactFloat64())
}

// RunBackground refreshes gauges every interval until ctx is done.
func (g *Gateway) RunBackground(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := g.RefreshSpend(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "spend refresh failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// capStatus is ok, warning (>= 80%) or exceeded.
func capStatus(spent, cap decimal.Decimal) string {
	switch {
	case !cap.IsPositive() || spent.GreaterThanOrEqual(cap):
		return "exceeded"
	case spent.GreaterThanOrEqual(cap.Mul(AlertThresholds[0])):
		return "warning"
	}
	return "ok"
}
