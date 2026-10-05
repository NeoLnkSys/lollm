package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// doctorCmd runs diagnostics. Phase P1 checks config + database wiring;
// provider connection tests and port availability checks arrive with the
// provider adapters (P3/P5) and the full CLI pass (P9).
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostics (config, database, environment)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		failed := false

		// 1) Config.
		cfg, err := loadConfig(newFlagOverrides())
		if err != nil {
			fmt.Printf("✖ config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✔ config: host=%s api_port=%d dashboard_port=%d db=%s log_level=%s\n",
			cfg.Host, cfg.APIPort, cfg.DashboardPort, cfg.DBPath, cfg.LogLevel)

		// 2) Database.
		store, err := openStore(cfg)
		if err != nil {
			fmt.Printf("✖ database: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()
		version, err := store.SQLiteVersion(ctx)
		if err != nil {
			fmt.Printf("✖ database: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✔ database: sqlite %s at %s\n", version, cfg.DBPath)

		// 3) Content summary.
		conns, _ := store.ListConnections(ctx)
		activeConns := 0
		for _, c := range conns {
			if c.IsActive {
				activeConns++
			}
		}
		combos, _ := store.ListCombos(ctx)
		keys, _ := store.ListAPIKeys(ctx)
		activeKeys := 0
		for _, k := range keys {
			if k.IsActive {
				activeKeys++
			}
		}
		fmt.Printf("• connections: %d total (%d active) | combos: %d | API keys: %d active\n",
			len(conns), activeConns, len(combos), activeKeys)

		fmt.Println("• provider connection tests + port availability checks arrive in later phases (P3/P9)")
		if activeConns == 0 {
			fmt.Println("  hint: demo connections are seeded inactive — add a real provider key via the dashboard")
		}
		if failed {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
