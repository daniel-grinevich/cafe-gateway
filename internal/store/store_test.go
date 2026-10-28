package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
	"github.com/daniel-grinevich/cafe-gateway/internal/testdb"
)

func TestMonthBounds(t *testing.T) {
	m, start, end, err := store.MonthBounds("2026-12", time.Now())
	if err != nil || m != "2026-12" || !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%s %v %v %v", m, start, end, err)
	}
	m, _, _, _ = store.MonthBounds("", time.Date(2026, 10, 31, 23, 0, 0, 0, time.FixedZone("PDT", -7*3600)))
	if m != "2026-11" {
		t.Fatalf("month is computed in UTC, got %s", m)
	}
	for _, bad := range []string{"2026-13", "26-01", "2026-1", "x"} {
		if _, _, _, err := store.MonthBounds(bad, time.Now()); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMigrationsAreEmbedded(t *testing.T) {
	m, err := store.Migrations()
	if err != nil || len(m) == 0 || m[0][0] != "0001_init" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestMigrateIsIdempotentAndConcurrencySafe(t *testing.T) {
	st, url := testdb.NewWithURL(t) // already migrated once
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := store.Open(ctx, url)
			if err != nil {
				errs[i] = err
				return
			}
			defer other.Close()
			applied, err := other.Migrate(ctx)
			if err == nil && len(applied) != 0 {
				t.Errorf("re-applied %v", applied)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema_migrations = %d %v", n, err)
	}
}

func TestExpiredReservationsStopCounting(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	key, _, err := st.CreateKey(ctx, store.NewKey{Service: config.Barista, Name: "a", RPM: 1, MaxTokensPerRequest: 1, MonthlyBudgetUSD: decimal.NewFromInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cap := decimal.RequireFromString("0.5")
	if _, err := st.Reserve(ctx, key, cap, decimal.RequireFromString("0.4"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reserve(ctx, key, cap, decimal.RequireFromString("0.4"), time.Minute, now); err == nil {
		t.Fatal("second reservation should exceed the cap")
	}
	// A crashed pod's reservation expires and stops counting.
	later := now.Add(2 * time.Minute)
	if _, err := st.Reserve(ctx, key, cap, decimal.RequireFromString("0.4"), time.Minute, later); err != nil {
		t.Fatalf("expired reservation still counted: %v", err)
	}
	if n, err := st.PruneReservations(ctx, later); err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	key, _, _ := st.CreateKey(ctx, store.NewKey{Service: config.Grinder, Name: "a", RPM: 1, MaxTokensPerRequest: 1})
	first, err := st.RevokeKey(ctx, key.KeyID)
	if err != nil || first.RevokedAt == nil {
		t.Fatal(err)
	}
	second, _ := st.RevokeKey(ctx, "grd_"+key.KeyID+"_…")
	if !second.RevokedAt.Equal(*first.RevokedAt) {
		t.Fatal("revoked_at changed")
	}
}
