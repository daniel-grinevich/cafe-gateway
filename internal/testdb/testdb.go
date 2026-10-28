// Package testdb gives each test its own migrated Postgres database.
//
// Set BARISTA_TEST_ADMIN_DSN to a Postgres URL whose user may CREATE DATABASE (CI and compose do). Without it,
// database tests are skipped, unless BARISTA_TEST_REQUIRE_DB is set (CI), which makes that a failure.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniel-grinevich/cafe-gateway/internal/store"
)

const EnvVar = "BARISTA_TEST_ADMIN_DSN"

// New creates a database, migrates it, and drops it when the test ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	st, _ := NewWithURL(t)
	return st
}

// NewWithURL is New plus the database's URL.
func NewWithURL(t testing.TB) (*store.Store, string) {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		if os.Getenv("BARISTA_TEST_REQUIRE_DB") != "" {
			t.Fatalf("%s is not set and BARISTA_TEST_REQUIRE_DB is", EnvVar)
		}
		t.Skipf("%s is not set; skipping Postgres test", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "cafe_test_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect %s: %v", EnvVar, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, admin); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, u.String()
}
