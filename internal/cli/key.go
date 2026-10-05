package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/db"
)

// keyCmd manages the internal API keys that clients (Cursor, Claude Code, ...)
// use to authenticate against the gateway. Keys are stored hashed; the plain
// text is shown exactly once at creation time.
var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage internal API keys",
}

var keyGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate a new internal API key",
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

		plain, err := auth.GenerateAPIKey()
		if err != nil {
			return err
		}
		k := &db.APIKey{
			Name:      keyGenName,
			KeyHash:   auth.HashKey(plain),
			KeyPrefix: auth.DisplayPrefix(plain),
			IsActive:  true,
		}
		if err := store.CreateAPIKey(ctx, k); err != nil {
			return err
		}
		fmt.Println("API key created (shown ONCE — store it now):")
		fmt.Printf("  key:  %s\n", plain)
		fmt.Printf("  id:   %s  (use with `lollm key revoke`)\n", k.ID)
		return nil
	},
}

var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List internal API keys",
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

		keys, err := store.ListAPIKeys(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tPREFIX\tACTIVE\tCREATED\tLAST USED")
		for _, k := range keys {
			active := "yes"
			if !k.IsActive {
				active = "no"
			}
			lastUsed := "never"
			if k.LastUsedAt != nil {
				lastUsed = k.LastUsedAt.UTC().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				k.ID, k.Name, k.KeyPrefix, active,
				k.CreatedAt.UTC().Format("2006-01-02 15:04"), lastUsed)
		}
		return w.Flush()
	},
}

var keyRevokeCmd = &cobra.Command{
	Use:   "revoke <id>",
	Short: "Revoke (deactivate) an internal API key",
	Args:  cobra.ExactArgs(1),
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

		id := args[0]
		if _, err := store.GetAPIKey(ctx, id); err != nil {
			return err
		}
		if err := store.SetAPIKeyActive(ctx, id, false); err != nil {
			return err
		}
		fmt.Printf("✔ key %s revoked\n", id)
		return nil
	},
}

var keyGenName string

func init() {
	keyGenerateCmd.Flags().StringVar(&keyGenName, "name", "unnamed", "label for the new key")
	keyCmd.AddCommand(keyGenerateCmd, keyListCmd, keyRevokeCmd)
	rootCmd.AddCommand(keyCmd)
}
