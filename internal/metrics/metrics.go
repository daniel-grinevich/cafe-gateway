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
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.SpendUSD, m.ReservedUSD, m.CapUSD, m.CapRatio, m.CostUSD, m.Requests, m.Duration, m.Upstream,
		m.CapAlerts, m.BudgetAlerts,
	)
	return m
}
