// Command mock-openrouter serves internal/mockopenrouter for local development. It never calls the real API.
//
//	OPENROUTER_BASE_URL=http://localhost:8090/api/v1
//	OPENROUTER_DECISIONS_URL=http://localhost:8090/api/alpha/decisions
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/daniel-grinevich/cafe-gateway/internal/mockopenrouter"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	srv := &http.Server{Addr: addr, Handler: mockopenrouter.Handler(), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("mock openrouter listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}
