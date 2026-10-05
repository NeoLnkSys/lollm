package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("LoLLM %s v%s (commit %s, built %s)\n", Codename, Version, Commit, BuildDate)
		fmt.Printf("  %s/%s, %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
		fmt.Println("  self-hosted AI gateway / LLM router — single binary")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
