// Command cafe-gateway serves barista (/v1/decide) or grinder (/v1/embed), selected by BARISTA_SERVICE, and
// administers keys and usage.
//
//	cafe-gateway serve
//	cafe-gateway migrate
//	cafe-gateway keys create --service barista --name alice --rpm 30 --budget 5
//	cafe-gateway keys list [--service grinder]
//	cafe-gateway keys revoke <key_id>
//	cafe-gateway usage [--month [YYYY-MM]] [--service barista] [--by surface|agent|key|model|day|service|status]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
	"github.com/daniel-grinevich/cafe-gateway/internal/gateway"
	"github.com/daniel-grinevich/cafe-gateway/internal/openrouter"
	"github.com/daniel-grinevich/cafe-gateway/internal/store"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "cafe-gateway",
		Short:         "OpenRouter gateway: barista (decisions) and grinder (embeddings)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd(), migrateCmd(), keysCmd(), usageCmd(), healthcheckCmd())
	return root
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return cfg, nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}

func withStore(ctx context.Context, fn func(*config.Config, *store.Store) error) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(cfg, st)
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve this deployment's service (BARISTA_SERVICE) on BARISTA_ADDR (default :8000)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			setupLogging(cfg.LogLevel)
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			st, err := store.Open(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()
			gw := gateway.New(cfg, st, openrouter.New(cfg, nil))
			go gw.RunBackground(ctx, 30*time.Second)
			srv := &http.Server{
				Addr:              cfg.Addr,
				Handler:           gw.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      cfg.UpstreamConnectTimeout + cfg.UpstreamReadTimeout*time.Duration(cfg.UpstreamMaxRetries+1) + 30*time.Second,
				IdleTimeout:       120 * time.Second,
			}
			errc := make(chan error, 1)
			go func() { errc <- srv.ListenAndServe() }()
			slog.Info("serving", "service", string(cfg.Service), "addr", cfg.Addr,
				"models", cfg.Models(cfg.Service), "monthly_cap_usd", cfg.MonthlyCap(cfg.Service).String(),
				"data_collection", cfg.OpenRouterDataCollection, "upstream_configured", cfg.OpenRouterKey != "",
				"admin_enabled", cfg.AdminToken != "", "debug_store_bodies", cfg.DebugStoreBodies)
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
			}
			slog.Info("shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
	}
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations (safe to run concurrently)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd.Context(), func(_ *config.Config, st *store.Store) error {
				applied, err := st.Migrate(cmd.Context())
				if len(applied) > 0 {
					fmt.Println("applied", strings.Join(applied, ", "))
				} else if err == nil {
					fmt.Println("database is up to date")
				}
				return err
			})
		},
	}
}

func serviceFlag(cmd *cobra.Command, required bool) (config.Service, error) {
	raw, _ := cmd.Flags().GetString("service")
	if raw == "" && !required {
		return "", nil
	}
	return config.ParseService(raw)
}

func keysCmd() *cobra.Command {
	keys := &cobra.Command{Use: "keys", Short: "Manage barista/grinder API keys"}

	create := &cobra.Command{
		Use:   "create",
		Short: "Create a key (the full key is printed once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := serviceFlag(cmd, true)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")
			return withStore(cmd.Context(), func(cfg *config.Config, st *store.Store) error {
				nk := store.NewKey{Service: service, Name: name, RPM: cfg.DefaultRPM,
					MaxTokensPerRequest: cfg.DefaultMaxTokensPerRequest, MonthlyBudgetUSD: cfg.DefaultMonthlyBudget}
				if cmd.Flags().Changed("rpm") {
					nk.RPM, _ = cmd.Flags().GetInt("rpm")
				}
				if cmd.Flags().Changed("max-tokens") {
					nk.MaxTokensPerRequest, _ = cmd.Flags().GetInt("max-tokens")
				}
				if cmd.Flags().Changed("budget") {
					raw, _ := cmd.Flags().GetString("budget")
					if nk.MonthlyBudgetUSD, err = decimal.NewFromString(raw); err != nil {
						return fmt.Errorf("--budget: not a number: %q", raw)
					}
				}
				key, plaintext, err := st.CreateKey(cmd.Context(), nk)
				if err != nil {
					return err
				}
				printJSON(cmd.OutOrStdout(), key.JSON())
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n\nThis is the only time the key is shown. Send it to %s privately.\n", plaintext, key.Name)
				return nil
			})
		},
	}
	create.Flags().String("service", "", "barista or grinder (required)")
	create.Flags().String("name", "", "who the key is for, e.g. alice (required)")
	create.Flags().Int("rpm", 0, "requests per minute (default DEFAULT_RPM, 30)")
	create.Flags().String("budget", "", "monthly budget in USD (default DEFAULT_MONTHLY_BUDGET_USD, 1)")
	create.Flags().Int("max-tokens", 0, "max input tokens per request (default DEFAULT_MAX_TOKENS_PER_REQUEST, 8000)")
	_ = create.MarkFlagRequired("service")
	_ = create.MarkFlagRequired("name")

	list := &cobra.Command{
		Use:   "list",
		Short: "List keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := serviceFlag(cmd, false)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			return withStore(cmd.Context(), func(_ *config.Config, st *store.Store) error {
				all, err := st.ListKeys(cmd.Context(), service)
				if err != nil {
					return err
				}
				if asJSON {
					out := make([]store.KeyJSON, 0, len(all))
					for _, k := range all {
						out = append(out, k.JSON())
					}
					printJSON(cmd.OutOrStdout(), out)
					return nil
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "KEY_ID\tSERVICE\tNAME\tRPM\tMAX_TOKENS\tBUDGET_USD\tLAST_USED\tSTATUS")
				for _, k := range all {
					status, last := "active", "-"
					if k.RevokedAt != nil {
						status = "revoked"
					}
					if k.LastUsedAt != nil {
						last = k.LastUsedAt.UTC().Format(time.RFC3339)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n", k.KeyID, k.Service, k.Name, k.RPM,
						k.MaxTokensPerRequest, k.MonthlyBudgetUSD.String(), last, status)
				}
				return tw.Flush()
			})
		},
	}
	list.Flags().String("service", "", "only barista or grinder keys")
	list.Flags().Bool("json", false, "print JSON")

	revoke := &cobra.Command{
		Use:   "revoke <key_id>",
		Short: "Revoke a key (by key_id or its bar_…/grd_… prefix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(cmd.Context(), func(_ *config.Config, st *store.Store) error {
				key, err := st.RevokeKey(cmd.Context(), args[0])
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no key %q", args[0])
				}
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (%s, %s)\n", key.KeyID, key.Name, key.Service)
				return nil
			})
		},
	}
	keys.AddCommand(create, list, revoke)
	return keys
}

func usageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Usage and spend for a month (default: this month, UTC)",
		// A bare --month takes no value, so "--month 2026-10" leaves 2026-10 as an argument.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := serviceFlag(cmd, false)
			if err != nil {
				return err
			}
			month, _ := cmd.Flags().GetString("month")
			month = strings.TrimSpace(month)
			if len(args) == 1 {
				if month != "" {
					return fmt.Errorf("unexpected argument %q", args[0])
				}
				month = args[0]
			}
			by, _ := cmd.Flags().GetString("by")
			keyID, _ := cmd.Flags().GetString("key")
			if by != "" && !slices.Contains(store.GroupBys, by) {
				return fmt.Errorf("--by must be one of %s", strings.Join(store.GroupBys, ", "))
			}
			return withStore(cmd.Context(), func(_ *config.Config, st *store.Store) error {
				f := store.UsageFilter{Month: month, Service: string(service)}
				if keyID != "" {
					key, err := st.KeyByKeyID(cmd.Context(), keyID)
					if err != nil {
						return fmt.Errorf("key %q: %w", keyID, err)
					}
					f.APIKeyID = key.ID
				}
				report, err := st.Summarize(cmd.Context(), f, by, time.Now())
				if err != nil {
					return err
				}
				printJSON(cmd.OutOrStdout(), report)
				return nil
			})
		},
	}
	cmd.Flags().String("month", "", "YYYY-MM; a bare --month means this month")
	cmd.Flags().Lookup("month").NoOptDefVal = " "
	cmd.Flags().String("service", "", "barista or grinder")
	cmd.Flags().String("key", "", "only this key (key_id or prefix)")
	cmd.Flags().String("by", "", "group by: "+strings.Join(store.GroupBys, "|"))
	return cmd
}

// healthcheckCmd lets the distroless image (no curl) run a Docker HEALTHCHECK.
func healthcheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "healthcheck",
		Short:  "Exit 0 if this container's /health answers",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr := os.Getenv("BARISTA_ADDR")
			if addr == "" {
				addr = ":8000"
			}
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			client := &http.Client{Timeout: 3 * time.Second}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, "http://"+addr+"/health", nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("health: %s", resp.Status)
			}
			return nil
		},
	}
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
