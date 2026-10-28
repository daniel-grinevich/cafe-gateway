package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// GroupBys are the dimensions a usage report can be grouped by.
var GroupBys = []string{"surface", "agent", "model", "key", "day", "service", "status"}

// Whitelisted SQL for each group-by; never built from user input.
var groupColumns = map[string]string{
	"surface": "u.surface",
	"agent":   "u.agent_id",
	"model":   "u.model",
	"key":     "k.name || ' (' || k.key_id || ')'",
	"day":     "to_char(u.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')",
	"service": "u.service",
	"status":  "u.status_code::text",
}

type UsageFilter struct {
	Month    string // YYYY-MM, "" = this month (UTC)
	Service  string
	APIKeyID string
	Surface  string
	AgentID  string
}

type UsageTotals struct {
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostUSD      string `json:"cost_usd"`
}

type UsageGroup struct {
	Group *string `json:"group"`
	UsageTotals
}

type UsageReport struct {
	Month   string            `json:"month"`
	Filters map[string]string `json:"filters"`
	Totals  UsageTotals       `json:"totals"`
	GroupBy string            `json:"group_by,omitempty"`
	Groups  []UsageGroup      `json:"groups,omitempty"`
}

// Summarize totals a month of usage, optionally grouped. It returns an error for a bad month or group-by.
func (s *Store) Summarize(ctx context.Context, f UsageFilter, groupBy string, now time.Time) (*UsageReport, error) {
	month, start, end, err := MonthBounds(f.Month, now)
	if err != nil {
		return nil, err
	}
	where := []string{"u.created_at >= $1", "u.created_at < $2"}
	args := []any{start, end}
	filters := map[string]string{}
	for _, c := range []struct{ col, val, name string }{
		{"u.service", f.Service, "service"},
		{"u.api_key_id::text", f.APIKeyID, ""},
		{"u.surface", f.Surface, "surface"},
		{"u.agent_id", f.AgentID, "agent_id"},
	} {
		if c.val == "" {
			continue
		}
		args = append(args, c.val)
		where = append(where, fmt.Sprintf("%s = $%d", c.col, len(args)))
		if c.name != "" {
			filters[c.name] = c.val
		}
	}
	const aggregates = `count(*), coalesce(sum(u.input_tokens), 0), coalesce(sum(u.output_tokens), 0),
		round(coalesce(sum(u.cost_usd), 0), 8)::text`
	from := " FROM usage u JOIN api_keys k ON k.id = u.api_key_id WHERE " + strings.Join(where, " AND ")

	report := &UsageReport{Month: month, Filters: filters}
	t := &report.Totals
	if err := s.Pool.QueryRow(ctx, "SELECT "+aggregates+from, args...).
		Scan(&t.Requests, &t.InputTokens, &t.OutputTokens, &t.CostUSD); err != nil {
		return nil, err
	}
	t.CostUSD = money8(t.CostUSD)
	if groupBy == "" {
		return report, nil
	}
	col, ok := groupColumns[groupBy]
	if !ok {
		return nil, fmt.Errorf("by must be one of %s", strings.Join(GroupBys, ", "))
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+col+", "+aggregates+from+
		" GROUP BY 1 ORDER BY sum(u.cost_usd) DESC, count(*) DESC, 1", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	report.GroupBy = groupBy
	report.Groups = []UsageGroup{}
	for rows.Next() {
		var g UsageGroup
		if err := rows.Scan(&g.Group, &g.Requests, &g.InputTokens, &g.OutputTokens, &g.CostUSD); err != nil {
			return nil, err
		}
		g.CostUSD = money8(g.CostUSD)
		report.Groups = append(report.Groups, g)
	}
	return report, rows.Err()
}

// money8 formats a numeric string with exactly 8 decimals ("0.00040000").
func money8(s string) string {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return s
	}
	return d.StringFixed(8)
}
