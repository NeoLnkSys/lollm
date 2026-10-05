package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lollm/lollm/internal/backup"
	"github.com/lollm/lollm/internal/secret"
)

// exportConfigCmd dumps the whole configuration to a portable backup.json
// (spec section 3.8: Export / Import).
var exportConfigCmd = &cobra.Command{
	Use:   "export-config",
	Short: "Export connections, combos, keys, agent configs & settings to backup.json",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		cfg, err := loadConfig(newFlagOverrides())
		if err != nil {
			return err
		}
		store, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer store.Close()
		masterKey, err := secret.LoadOrCreateMasterKey(secret.MasterKeyPath(filepath.Dir(cfg.DBPath)))
		if err != nil {
			return err
		}

		opts := backup.Options{IncludeSecrets: exportSecrets, Password: exportPassword}
		if exportPassword != "" {
			opts.IncludeSecrets = true // --password implies --secrets
		}
		b, err := backup.Build(ctx, store, masterKey, opts)
		if err != nil {
			return fmt.Errorf("export: %w", err)
		}

		out, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(exportOut, out, 0o600); err != nil {
			return err
		}

		mode := "tanpa secret"
		switch b.Secrets {
		case backup.SecretsPlain:
			mode = "PLAINTEXT — simpan file ini aman (chmod 600 sudah diterapkan)"
		case backup.SecretsPBKDF2:
			mode = "terenkripsi password (PBKDF2-AES-256-GCM)"
		}
		fmt.Printf("✓ backup tertulis ke %s (%d bytes)\n", exportOut, len(out))
		fmt.Printf("  %d connection · %d combo · %d agent config · %d api key · %d pool · %d setting\n",
			len(b.Connections), len(b.Combos), len(b.AgentConfigs), len(b.APIKeys), len(b.ProxyPools), len(b.Settings))
		fmt.Printf("  secret: %s\n", mode)
		return nil
	},
}

// importConfigCmd restores a backup.json (merge by default, --replace wipes
// the affected tables first).
var importConfigCmd = &cobra.Command{
	Use:   "import-config <backup.json>",
	Short: "Import configuration from a backup.json export",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var b backup.Backup
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("bukan backup.json yang valid: %w", err)
		}
		if b.Format != backup.Format {
			return fmt.Errorf("file bukan %s (format=%q)", backup.Format, b.Format)
		}

		if b.Secrets == backup.SecretsPBKDF2 {
			pass := importPassword
			if pass == "" {
				fmt.Print("Password backup: ")
				fmt.Scanln(&pass)
				if strings.TrimSpace(pass) == "" {
					return fmt.Errorf("password diperlukan")
				}
			}
			if err := b.DecryptSecrets(pass); err != nil {
				return err
			}
			fmt.Println("✓ secret terdekripsi")
		}

		if importReplace && !importYes {
			fmt.Print("PERINGATAN: --replace menghapus semua connection/combo/agent-config/api-key/pool yang ada lalu mengimpor backup. Lanjut? [y/N] ")
			var answer string
			fmt.Scanln(&answer)
			if !strings.EqualFold(strings.TrimSpace(answer), "y") {
				return fmt.Errorf("dibatalkan")
			}
		}

		cfg, err := loadConfig(newFlagOverrides())
		if err != nil {
			return err
		}
		store, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer store.Close()
		masterKey, err := secret.LoadOrCreateMasterKey(secret.MasterKeyPath(filepath.Dir(cfg.DBPath)))
		if err != nil {
			return err
		}

		stats, err := backup.Restore(ctx, store, masterKey, &b, backup.RestoreOptions{Replace: importReplace})
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		fmt.Printf("✓ import selesai (%s): %d connection · %d combo · %d agent config · %d api key · %d pool · %d setting\n",
			map[bool]string{true: "replace", false: "merge"}[importReplace],
			stats.Connections, stats.Combos, stats.AgentConfigs, stats.APIKeys, stats.ProxyPools, stats.Settings)
		return nil
	},
}

// usageCmd prints recent usage logs (table or CSV).
var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show recent request usage logs",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		cfg, err := loadConfig(newFlagOverrides())
		if err != nil {
			return err
		}
		store, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer store.Close()

		logs, err := store.ListUsageLogs(ctx, usageLimit)
		if err != nil {
			return err
		}

		if usageCSV {
			w := csv.NewWriter(os.Stdout)
			_ = w.Write([]string{"time", "request_id", "combo", "model", "agent_role", "status",
				"prompt_tokens", "completion_tokens", "tokens_saved", "latency_ms", "error"})
			for _, l := range logs {
				_ = w.Write([]string{
					l.CreatedAt.Format(time.RFC3339), l.RequestID, l.ComboName, l.Model, l.AgentRole,
					fmt.Sprint(l.StatusCode), fmt.Sprint(l.PromptTokens), fmt.Sprint(l.CompletionTokens),
					fmt.Sprint(l.TokensSaved), fmt.Sprint(l.LatencyMs), l.Error,
				})
			}
			w.Flush()
			return w.Error()
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tCOMBO\tMODEL\tROLE\tSTATUS\tPROMPT\tCOMPL\tSAVED\tMS")
		for _, l := range logs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\n",
				l.CreatedAt.Format("01-02 15:04"), orDash(l.ComboName), truncate(orDash(l.Model), 34),
				orDash(l.AgentRole), l.StatusCode, l.PromptTokens, l.CompletionTokens, l.TokensSaved, l.LatencyMs)
		}
		return tw.Flush()
	},
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var (
	exportOut      string
	exportSecrets  bool
	exportPassword string
	importPassword string
	importReplace  bool
	importYes      bool
	usageLimit     int
	usageCSV       bool
)

func init() {
	exportConfigCmd.Flags().StringVar(&exportOut, "out", "backup.json", "output file")
	exportConfigCmd.Flags().BoolVar(&exportSecrets, "secrets", false, "include provider API keys")
	exportConfigCmd.Flags().StringVar(&exportPassword, "password", "", "encrypt secrets with a password (implies --secrets)")

	importConfigCmd.Flags().StringVar(&importPassword, "password", "", "password to decrypt a sealed backup")
	importConfigCmd.Flags().BoolVar(&importReplace, "replace", false, "wipe existing objects before importing")
	importConfigCmd.Flags().BoolVar(&importYes, "yes", false, "assume yes (no confirmation for --replace)")

	usageCmd.Flags().IntVar(&usageLimit, "limit", 50, "how many rows (max 500)")
	usageCmd.Flags().BoolVar(&usageCSV, "csv", false, "output CSV instead of a table")

	rootCmd.AddCommand(exportConfigCmd)
	rootCmd.AddCommand(importConfigCmd)
	rootCmd.AddCommand(usageCmd)
}
