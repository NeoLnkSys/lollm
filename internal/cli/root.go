package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/db"
)

// Build information, wired via -ldflags at build time (see Makefile).
var (
	Version   = "0.1.0"
	Commit    = "none"
	BuildDate = "unknown"
)

// Codename is this release's name: LoLLM Synapse.
const Codename = "Synapse"

var (
	flagConfigPath string
	flagDBPath     string
)

var rootCmd = &cobra.Command{
	Use:   "lollm",
	Short: "LoLLM — self-hosted AI gateway / LLM router in a single binary",
	Long: `LoLLM proxies OpenAI-compatible requests from coding tools (Cursor, Claude
Code, Cline, ...) to multiple LLM providers with fallback, health-aware
routing, multi-account key rotation, token compression, an Agent Mode
pipeline, and an embedded dashboard.`,
	SilenceUsage: true,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagConfigPath, "config", "",
		"config file path (default: $LOLLM_CONFIG, $CONFIG, or ./config.yaml)")
	rootCmd.PersistentFlags().StringVar(&flagDBPath, "db", "",
		"SQLite database path (overrides config file and environment)")
}

// configPath resolves the config file path: --config > LOLLM_CONFIG > CONFIG > ./config.yaml.
func configPath() string {
	if flagConfigPath != "" {
		return flagConfigPath
	}
	for _, env := range []string{"LOLLM_CONFIG", "CONFIG"} {
		if p := os.Getenv(env); p != "" {
			return p
		}
	}
	return config.DefaultConfigPath
}

// newFlagOverrides returns the shared flag overrides for commands that have no
// dedicated config flags (the persistent --db flag is applied inside loadConfig).
func newFlagOverrides() config.FlagOverrides {
	return config.FlagOverrides{}
}

// loadConfig builds the effective config (defaults < YAML < env < CLI flags).
// overrides carries only the flags the calling command received explicitly.
func loadConfig(overrides config.FlagOverrides) (*config.Config, error) {
	if flagDBPath != "" {
		overrides.DBPath = &flagDBPath
	}
	return config.Load(config.LoadOptions{
		Path:     configPath(),
		Required: flagConfigPath != "", // explicit --config must exist
		Flags:    overrides,
	})
}

// openStore opens (and migrates) the database described by cfg.
func openStore(cfg *config.Config) (*db.Store, error) {
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(d); err != nil {
		d.Close()
		return nil, err
	}
	return db.NewStore(d), nil
}
