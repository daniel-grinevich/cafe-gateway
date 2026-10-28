package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daniel-grinevich/cafe-gateway/internal/testdb"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

func TestKeysAndUsageCommands(t *testing.T) {
	_, url := testdb.NewWithURL(t)
	t.Setenv("DATABASE_URL", url)

	if out := run(t, "migrate"); !strings.Contains(out, "") {
		t.Fatal(out)
	}
	out := run(t, "keys", "create", "--service", "barista", "--name", "alice", "--rpm", "30", "--budget", "5")
	var created map[string]any
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&created); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if created["rpm"] != 30.0 || created["monthly_budget_usd"] != "5" || created["service"] != "barista" {
		t.Fatalf("created = %v", created)
	}
	keyID := created["key_id"].(string)
	if !strings.Contains(out, "bar_"+keyID+"_") || !strings.Contains(out, "only time the key is shown") {
		t.Fatal("plaintext not printed once")
	}
	run(t, "keys", "create", "--service", "grinder", "--name", "bob")

	list := run(t, "keys", "list", "--service", "barista")
	if !strings.Contains(list, keyID) || strings.Contains(list, "bob") || !strings.Contains(list, "active") {
		t.Fatalf("list = %s", list)
	}
	run(t, "keys", "revoke", "bar_"+keyID+"_…")
	if list := run(t, "keys", "list"); !strings.Contains(list, "revoked") || !strings.Contains(list, "bob") {
		t.Fatalf("list = %s", list)
	}

	for _, args := range [][]string{{"usage"}, {"usage", "--month"}, {"usage", "--month", "2026-10", "--by", "surface"}, {"usage", "--by", "agent", "--service", "grinder"}} {
		var report map[string]any
		if err := json.Unmarshal([]byte(run(t, args...)), &report); err != nil || report["totals"] == nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	root := newRoot()
	root.SetArgs([]string{"usage", "--by", "nope"})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("bad --by accepted")
	}
	root = newRoot()
	root.SetArgs([]string{"keys", "create", "--service", "espresso", "--name", "x"})
	if err := root.Execute(); err == nil {
		t.Fatal("bad --service accepted")
	}
}
