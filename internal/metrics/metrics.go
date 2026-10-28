// Package metrics holds the Prometheus metrics served on /metrics. Labels are service-level only (never key names),
// because /metrics is reachable from the tailnet.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Metrics struct {
	Registry *prometheus.Registry

	SpendUSD     *prometheus.GaugeVec
	ReservedUSD  *prometheus.GaugeVec
	CapUSD       *prometheus.GaugeVec
	CapRatio     *prometheus.GaugeVec
	CostUSD      *prometheus.CounterVec
	Requests     *prometheus.CounterVec
	Duration     *prometheus.HistogramVec
	Upstream     *prometheus.CounterVec
	CapAlerts    *prometheus.CounterVec
	BudgetAlerts *prometheus.CounterVec

	// The shared OpenRouter key's real credit (GET /api/v1/key). Unlabelled; a series is absent until it is known
	// (and limit/remaining/ratio stay absent for a key with no limit). Every pod exports the same values: use max().
	KeyLimitUSD        *prometheus.GaugeVec
	KeyUsageUSD        *prometheus.GaugeVec
	KeyUsageDailyUSD   *prometheus.GaugeVec
	KeyUsageWeeklyUSD  *prometheus.GaugeVec
	KeyUsageMonthlyUSD *prometheus.GaugeVec
	KeyRemainingUSD    *prometheus.GaugeVec
	KeyRemainingRatio  *prometheus.GaugeVec
	KeyMinRemainingUSD *prometheus.GaugeVec
	KeyBlocked         *prometheus.GaugeVec
	KeyPollSuccess     *prometheus.GaugeVec
	KeyLastPoll        *prometheus.GaugeVec
	KeyAlerts          *prometheus.CounterVec
}

func keyGauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "cafe_gateway_openrouter_key_" + name, Help: help}, nil)
}

func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		SpendUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_month_spend_usd", Help: "Recorded OpenRouter spend this calendar month (UTC).",
		}, []string{"service"}),
		ReservedUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_reserved_usd", Help: "Estimated cost of in-flight requests not yet settled.",
		}, []string{"service"}),
		CapUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_monthly_cap_usd", Help: "Configured global monthly cap.",
		}, []string{"service"}),
		CapRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cafe_gateway_cap_used_ratio", Help: "Month spend / monthly cap (1 = cap reached).",
		}, []string{"service"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_cost_usd_total", Help: "Settled OpenRouter cost since process start.",
		}, []string{"service", "model", "source"}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_requests_total", Help: "Model endpoint requests by HTTP status and error code.",
		}, []string{"service", "status", "code"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cafe_gateway_request_duration_seconds", Help: "Model endpoint latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 4, 8, 15, 30, 60},
		}, []string{"service"}),
		Upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_upstream_responses_total", Help: "OpenRouter responses by status (after retries).",
		}, []string{"service", "status"}),
		CapAlerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_cap_threshold_alerts_total", Help: "Times a service's month spend crossed 80%/100% of its cap.",
		}, []string{"service", "threshold_pct"}),
		BudgetAlerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_key_budget_alerts_total", Help: "Times a key's month spend crossed 80%/100% of its budget.",
		}, []string{"service", "threshold_pct"}),
		KeyLimitUSD:        keyGauge("limit_usd", "Credit limit on the shared OpenRouter key (all apps using it, not just this gateway)."),
		KeyUsageUSD:        keyGauge("usage_usd", "Total spend on the shared OpenRouter key, as OpenRouter reports it."),
		KeyUsageDailyUSD:   keyGauge("usage_daily_usd", "Spend on the shared OpenRouter key today (UTC), as OpenRouter reports it."),
		KeyUsageWeeklyUSD:  keyGauge("usage_weekly_usd", "Spend on the shared OpenRouter key this week (UTC), as OpenRouter reports it."),
		KeyUsageMonthlyUSD: keyGauge("usage_monthly_usd", "Spend on the shared OpenRouter key this month (UTC), as OpenRouter reports it."),
		KeyRemainingUSD:    keyGauge("remaining_usd", "Credit left on the shared OpenRouter key (limit_remaining)."),
		KeyRemainingRatio:  keyGauge("remaining_ratio", "Credit left / credit limit on the shared OpenRouter key (0 = exhausted)."),
		KeyMinRemainingUSD: keyGauge("min_remaining_usd", "Safety floor (OPENROUTER_MIN_REMAINING_USD): below it upstream calls are refused."),
		KeyBlocked:         keyGauge("blocked", "1 while upstream calls are refused because the key's credit is below the floor."),
		KeyPollSuccess:     keyGauge("poll_success", "1 if the last poll of OpenRouter's key endpoint succeeded, else 0."),
		KeyLastPoll:        keyGauge("last_poll_timestamp_seconds", "Unix time of the last successful poll of OpenRouter's key endpoint."),
		KeyAlerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cafe_gateway_openrouter_key_alerts_total", Help: "Times the shared OpenRouter key's usage crossed 80%/95% of its limit.",
		}, []string{"threshold_pct"}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.SpendUSD, m.ReservedUSD, m.CapUSD, m.CapRatio, m.CostUSD, m.Requests, m.Duration, m.Upstream,
		m.CapAlerts, m.BudgetAlerts,
		m.KeyLimitUSD, m.KeyUsageUSD, m.KeyUsageDailyUSD, m.KeyUsageWeeklyUSD, m.KeyUsageMonthlyUSD, m.KeyRemainingUSD,
		m.KeyRemainingRatio, m.KeyMinRemainingUSD, m.KeyBlocked, m.KeyPollSuccess, m.KeyLastPoll, m.KeyAlerts,
	)
	return m
}
