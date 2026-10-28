package config

import (
	"strings"
	"testing"
)

func load(t *testing.T, vars map[string]string) (*Config, error) {
	t.Helper()
	return Load(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
}

func TestDefaults(t *testing.T) {
	c, err := load(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.InternalMetricsAddr != ":9090" {
		t.Fatalf("internal metrics addr = %q", c.InternalMetricsAddr)
	}
	if off, _ := load(t, map[string]string{"BARISTA_INTERNAL_METRICS_ADDR": "off"}); off.InternalMetricsAddr != "" {
		t.Fatal("off should disable the internal listener")
	}
	if c.Service != Barista || c.Addr != ":8000" || c.OpenRouterDataCollection != "deny" || c.DebugStoreBodies {
		t.Fatalf("defaults = %+v", c)
	}
	if c.BaristaModels[0] != "perplexity/pplx-decider-flash-v1.2" || c.GrinderModels[0] != "qwen/qwen3-embedding-8b" {
		t.Fatal("default models")
	}
	if c.BaristaMonthlyCap.String() != "10" || c.GrinderMonthlyCap.String() != "10" {
		t.Fatal("caps should default low ($10)")
	}
}

func TestClusterEnvNames(t *testing.T) {
	c, err := load(t, map[string]string{
		"BARISTA_SERVICE": "Grinder", "BARISTA_MODELS": "a/b, c/d", "GRINDER_MODELS": "e/f",
		"BARISTA_MONTHLY_CAP_USD": "2.5", "GRINDER_MONTHLY_CAP_USD": "3", "OPENROUTER_DATA_COLLECTION": "allow",
		"BARISTA_DEBUG_STORE_BODIES": "true", "BARISTA_ADMIN_TOKEN": "t", "OPEN_ROUTER_KEY": "k",
		"DATABASE_URL": "postgresql+asyncpg://u:p@h:5432/d", "BARISTA_ALLOWED_HOSTS": "Grinder.Example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Service != Grinder || len(c.BaristaModels) != 2 || c.BaristaModels[1] != "c/d" || c.Models(Grinder)[0] != "e/f" {
		t.Fatalf("config = %+v", c)
	}
	if c.MonthlyCap(Barista).String() != "2.5" || c.MonthlyCap(Grinder).String() != "3" {
		t.Fatal("caps")
	}
	if !c.DebugStoreBodies || c.OpenRouterDataCollection != "allow" || c.AdminToken != "t" || c.OpenRouterKey != "k" {
		t.Fatal("flags")
	}
	if c.DatabaseURL != "postgresql://u:p@h:5432/d" || c.AllowedHosts[0] != "grinder.example" {
		t.Fatalf("db=%s hosts=%v", c.DatabaseURL, c.AllowedHosts)
	}
}

func TestInvalidConfigIsAnError(t *testing.T) {
	_, err := load(t, map[string]string{"BARISTA_SERVICE": "espresso", "BARISTA_MONTHLY_CAP_USD": "-1", "DEFAULT_RPM": "x"})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"BARISTA_SERVICE", "BARISTA_MONTHLY_CAP_USD", "DEFAULT_RPM"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %s: %v", want, err)
		}
	}
}

func TestServiceProperties(t *testing.T) {
	if Barista.KeyPrefix() != "bar" || Grinder.KeyPrefix() != "grd" || Barista.Endpoint() != "/v1/decide" || Grinder.Endpoint() != "/v1/embed" {
		t.Fatal("service properties")
	}
}
