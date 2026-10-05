package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/routing"
)

var (
	routesCombo  string
	routesVision bool
	routesTools  bool
	routesAudio  bool
)

// routesCmd is a routing-engine diagnostic: it shows the exact plan a request
// would follow right now — ordered groups, eligible connections per group,
// and why anything was skipped.
var routesCmd = &cobra.Command{
	Use:   "routes [model]",
	Short: "Show the routing plan for a combo (or a plain model name)",
	Long: `Show the routing plan for a combo (or a plain model name).

Examples:
  lollm routes                    # plan for the "Auto" combo
  lollm routes --combo Code       # plan for another combo
  lollm routes llama-3.3-70b-versatile   # ad-hoc plan for a plain model
  lollm routes --vision --tools   # simulate a vision + tool-calling request`,
	Args: cobra.MaximumNArgs(1),
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

		conns, err := store.ListConnections(ctx)
		if err != nil {
			return err
		}

		var combo *db.Combo
		if len(args) == 1 {
			combo = routing.BuildAdHocCombo(args[0], conns)
			fmt.Printf("ad-hoc combo for model %q (strategy: %s)\n", args[0], combo.Strategy)
		} else {
			combo, err = store.GetComboByName(ctx, routesCombo)
			if err != nil {
				combos, _ := store.ListCombos(ctx)
				if len(combos) == 0 {
					return fmt.Errorf("no combos defined — run `lollm setup` first")
				}
				return fmt.Errorf("combo %q not found", routesCombo)
			}
			fmt.Printf("combo %q (strategy: %s)\n", combo.Name, combo.Strategy)
		}

		req := &routing.Request{Capabilities: routing.Capabilities{
			Vision:      routesVision,
			Audio:       routesAudio,
			ToolCalling: routesTools,
		}}
		engine := routing.NewEngine()
		groups, report, err := engine.Resolve(combo, conns, req)
		if err != nil {
			fmt.Printf("  request capabilities: %s\n", capString(req.Capabilities))
		}

		fmt.Printf("  %d combo entries, %d eligible\n", report.Total, report.Eligible)

		if len(report.Skipped) > 0 {
			fmt.Println("\n  Skipped:")
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
			fmt.Fprintln(w, "    CONNECTION\tMODEL\tREASON")
			for _, s := range report.Skipped {
				fmt.Fprintf(w, "    %s\t%s\t%s\n", s.ConnectionID, s.Model, s.Reason)
			}
			w.Flush()
		}

		if err != nil {
			fmt.Printf("\n✖ no routable candidates: %v\n", err)
			return nil // diagnostic output, not a command failure
		}

		fmt.Println("\n  Plan (fallback order):")
		for i, g := range groups {
			fmt.Printf("  %d. %s → %s  (priority %d)\n", i+1, g.Provider, g.Model, g.Priority)
			for _, c := range g.Connections {
				fmt.Printf("     • %s  (weight %d, errors %d, ema %dms)\n",
					c.Name, c.Weight, c.ConsecutiveErrors, int(c.LatencyEMAMs))
			}
		}
		return nil
	},
}

func capString(c routing.Capabilities) string {
	s := ""
	add := func(b bool, name string) {
		if b {
			if s != "" {
				s += "+"
			}
			s += name
		}
	}
	add(c.Vision, "vision")
	add(c.Audio, "audio")
	add(c.ToolCalling, "tools")
	add(c.StructuredOutput, "structured")
	if s == "" {
		s = "none"
	}
	return s
}

func init() {
	routesCmd.Flags().StringVar(&routesCombo, "combo", "Auto", "combo name to plan")
	routesCmd.Flags().BoolVar(&routesVision, "vision", false, "simulate a vision request")
	routesCmd.Flags().BoolVar(&routesTools, "tools", false, "simulate a tool-calling request")
	routesCmd.Flags().BoolVar(&routesAudio, "audio", false, "simulate an audio request")
	rootCmd.AddCommand(routesCmd)
}
