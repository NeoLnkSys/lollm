package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lollm/lollm/internal/api"
	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/dashboard"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/health"
	"github.com/lollm/lollm/internal/logging"
	"github.com/lollm/lollm/internal/secret"
)

var (
	serveHost     string
	serveAPIPort  int
	serveDashPort int
	serveLogLevel string
)

// serveCmd runs the gateway: OpenAI-compatible API on --api-port and the
// dashboard on --dashboard-port, with graceful shutdown on SIGINT/SIGTERM.
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the gateway (OpenAI-compatible API + dashboard)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Only apply flags the user actually passed, so that environment
		// variables and config.yaml keep their place in the precedence chain.
		ov := config.FlagOverrides{}
		if cmd.Flags().Changed("host") {
			ov.Host = &serveHost
		}
		if cmd.Flags().Changed("api-port") {
			ov.APIPort = &serveAPIPort
		}
		if cmd.Flags().Changed("dashboard-port") {
			ov.DashboardPort = &serveDashPort
		}
		if cmd.Flags().Changed("log-level") {
			ov.LogLevel = &serveLogLevel
		}

		cfg, err := loadConfig(ov)
		if err != nil {
			return err
		}
		log := logging.New(cfg.LogLevel)

		// Dependencies: database, master key, seed (idempotent).
		store, err := openStore(cfg)
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()
		masterKey, err := secret.LoadOrCreateMasterKey(secret.MasterKeyPath(filepath.Dir(cfg.DBPath)))
		if err != nil {
			return fmt.Errorf("master key: %w", err)
		}
		if err := db.SeedIfEmpty(ctx, store, masterKey); err != nil {
			return fmt.Errorf("seed: %w", err)
		}

		apiSrv := api.New(cfg, store, masterKey, log, Version)
		bus := health.NewBus()
		apiSrv.UseEventBus(bus)

		// Background health watcher: auto-recovery + proxy checks + events.
		checker := health.NewChecker(store, apiSrv, log, bus)
		go checker.Run(ctx)

		apiServer := &http.Server{
			Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.APIPort),
			Handler:           apiSrv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		dashServer := &http.Server{
			Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.DashboardPort),
			Handler:           dashboard.New(Version, apiSrv),
			ReadHeaderTimeout: 10 * time.Second,
		}

		errCh := make(chan error, 2)
		go func() { errCh <- apiServer.ListenAndServe() }()
		go func() { errCh <- dashServer.ListenAndServe() }()

		fmt.Printf("LoLLM %s v%s — self-hosted AI gateway\n", Codename, Version)
		fmt.Printf("  API:       http://%s:%d/v1\n", displayHost(cfg.Host), cfg.APIPort)
		fmt.Printf("  Dashboard: http://%s:%d  (chat playground + management UI)\n", displayHost(cfg.Host), cfg.DashboardPort)
		fmt.Printf("  Database:  %s\n", cfg.DBPath)
		fmt.Println("  Press Ctrl+C to stop.")
		log.Info("gateway started",
			"host", cfg.Host, "api_port", cfg.APIPort, "dashboard_port", cfg.DashboardPort)

		select {
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("server: %w", err)
			}
		case <-ctx.Done():
		}

		log.Info("shutting down…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = apiServer.Shutdown(shutdownCtx)
		_ = dashServer.Shutdown(shutdownCtx)
		return nil
	},
}

func displayHost(host string) string {
	if host == "0.0.0.0" || host == "" {
		return "localhost"
	}
	return host
}

func init() {
	serveCmd.Flags().StringVar(&serveHost, "host", config.DefaultHost, "bind address")
	serveCmd.Flags().IntVar(&serveAPIPort, "api-port", config.DefaultAPIPort, "OpenAI-compatible API port")
	serveCmd.Flags().IntVar(&serveDashPort, "dashboard-port", config.DefaultDashboardPort, "dashboard port")
	serveCmd.Flags().StringVar(&serveLogLevel, "log-level", config.DefaultLogLevel, "debug | info | warn | error")
	rootCmd.AddCommand(serveCmd)
}
