// Command import-recon builds the live gateway database from a recon report
// (see cmd/recon): one connection per real key, a health-aware "Auto" combo
// made of verified models, the Vercel relay proxy pool, and a fresh gateway
// API key.
//
//	go run ./cmd/import-recon -recon ../live/recon.json -backup ../../uploads/backup_clean-1.txt -db ../live/data/lollm.db
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

type reconKey struct {
	Provider  string   `json:"provider"`
	Tag       string   `json:"tag"`
	APIKey    string   `json:"api_key"`
	AccountID string   `json:"account_id"`
	Valid     bool     `json:"valid"`
	Models    []string `json:"models"`
}

// providerPriority orders connections inside a provider group.
var providerPriority = map[string]int{
	"openrouter":    10,
	"gemini":        20,
	"groq":          30,
	"poolside":      35,
	"ollama":        40,
	"mistral":       50,
	"cloudflare-ai": 60,
}

// autoModels lists the verified models for the Auto combo, in priority order.
var autoModels = []struct {
	provider, model string
	priority        int
}{
	{"gemini", "gemini-3.8-flash", 1},
	{"groq", "openai/gpt-oss-120b", 2},
	{"poolside", "poolside/laguna-s-2.1", 3},
	{"ollama", "gpt-oss:120b", 4},
	{"mistral", "mistral-small-latest", 5},
	{"openrouter", "qwen/qwen3.8-27b:free", 6},
	{"cloudflare-ai", "@cf/meta/llama-3.3-70b-instruct-fp8-fast", 7},
}

func main() {
	reconPath := flag.String("recon", "live/recon.json", "recon report")
	backupPath := flag.String("backup", "", "original backup txt (for proxy pool URLs)")
	dbPath := flag.String("db", "live/data/lollm.db", "target SQLite path")
	flag.Parse()

	raw, err := os.ReadFile(*reconPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read recon:", err)
		os.Exit(1)
	}
	var keys []reconKey
	if err := json.Unmarshal(raw, &keys); err != nil {
		fmt.Fprintln(os.Stderr, "parse recon:", err)
		os.Exit(1)
	}

	// Database + master key.
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	d, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	store := db.NewStore(d)
	ctx := context.Background()

	masterKey, err := secret.LoadOrCreateMasterKey(filepath.Join(filepath.Dir(*dbPath), ".master.key"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "master key:", err)
		os.Exit(1)
	}

	// --- connections: one per key -------------------------------------------------
	validPerProvider := map[string]*db.Connection{} // first valid connection per provider
	connProvider := map[string]string{}             // connID → provider (for reporting)
	nActive, nInactive := 0, 0
	for _, k := range keys {
		models := cleanModels(k.Provider, k.Models)
		modelsJSON, _ := json.Marshal(models)

		prio := providerPriority[k.Provider]
		if prio == 0 {
			prio = 100
		}
		conn := &db.Connection{
			Name:       k.Provider + "-" + strings.ToLower(k.Tag),
			Provider:   k.Provider,
			Priority:   prio,
			Weight:     1,
			IsActive:   k.Valid,
			Status:     db.StatusActive,
			ModelsJSON: string(modelsJSON),
		}
		if !conn.IsActive {
			conn.Status = db.StatusUnavailable
		}
		if k.Provider == "cloudflare-ai" && k.AccountID != "" {
			conn.BaseURL = "https://api.cloudflare.com/client/v4/accounts/" + k.AccountID + "/ai/v1"
		}
		enc, err := secret.EncryptString(masterKey, k.APIKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "encrypt:", err)
			os.Exit(1)
		}
		conn.APIKeyEncrypted = enc

		if err := store.CreateConnection(ctx, conn); err != nil {
			fmt.Fprintln(os.Stderr, "create connection:", err)
			os.Exit(1)
		}
		if k.Valid {
			nActive++
			if _, ok := validPerProvider[k.Provider]; !ok {
				validPerProvider[k.Provider] = conn
			}
		} else {
			nInactive++
		}
		connProvider[conn.ID] = k.Provider
	}

	// --- Auto combo from verified models --------------------------------------------
	var comboModels []db.ComboModel
	for _, am := range autoModels {
		conn, ok := validPerProvider[am.provider]
		if !ok {
			fmt.Printf("  skipping %s in Auto (no valid connection)\n", am.provider)
			continue
		}
		// The model must appear in the connection's real catalog (except the
		// two verified-by-chat exceptions below).
		if !contains(conn.Models(), am.model) && !verifiedException(am.provider, am.model) {
			fmt.Printf("  skipping %s %s (not in live catalog)\n", am.provider, am.model)
			continue
		}
		comboModels = append(comboModels, db.ComboModel{
			ConnectionID: conn.ID, Model: am.model, Priority: am.priority,
		})
	}
	auto, err := store.GetComboByName(ctx, "Auto")
	if err == nil {
		auto.Strategy = db.StrategyHealthAware
		auto.Models = comboModels
		if err := store.UpdateCombo(ctx, auto); err != nil {
			fmt.Fprintln(os.Stderr, "update combo:", err)
			os.Exit(1)
		}
	} else {
		auto = &db.Combo{Name: "Auto", Strategy: db.StrategyHealthAware, Models: comboModels}
		if err := store.CreateCombo(ctx, auto); err != nil {
			fmt.Fprintln(os.Stderr, "create combo:", err)
			os.Exit(1)
		}
	}

	// --- proxy pool from the backup file ---------------------------------------------
	if *backupPath != "" {
		if urls := parseProxyURLs(*backupPath); len(urls) > 0 {
			pool := &db.ProxyPool{Name: "vercel-relay", Proxies: urls}
			if err := store.CreateProxyPool(ctx, pool); err != nil {
				fmt.Fprintln(os.Stderr, "create pool:", err)
				os.Exit(1)
			}
			fmt.Printf("✔ proxy pool \"vercel-relay\": %d relays\n", len(urls))
		}
	}

	// --- gateway API key ---------------------------------------------------------------
	plain, err := auth.GenerateAPIKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := store.CreateAPIKey(ctx, &db.APIKey{
		Name: "live", KeyHash: auth.HashKey(plain), KeyPrefix: auth.DisplayPrefix(plain), IsActive: true,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "create api key:", err)
		os.Exit(1)
	}

	fmt.Printf("✔ connections: %d active, %d inactive (invalid/dead keys kept for the record)\n", nActive, nInactive)
	fmt.Printf("✔ combo \"Auto\" (%s): %d entries —", auto.Strategy, len(auto.Models))
	for _, m := range auto.Models {
		fmt.Printf(" %s@%s(p%d)", m.Model, connProvider[m.ConnectionID], m.Priority)
	}
	fmt.Println()
	fmt.Println("✔ gateway API key (shown ONCE):")
	fmt.Println("    " + plain)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// verifiedException lists model choices proven by a real chat completion even
// though they are absent from the provider's /models listing.
func verifiedException(provider, model string) bool {
	switch {
	case provider == "gemini" && model == "gemini-3.8-flash":
		return true // verified live; listing only shows deprecated 2.5 names
	case provider == "mistral" && model == "mistral-small-latest":
		return false
	}
	return false
}

// cleanModels normalizes catalog entries (strips Gemini's "models/" prefix).
func cleanModels(provider string, models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if provider == "gemini" {
			m = strings.TrimPrefix(m, "models/")
		}
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

var reProxyURL = regexp.MustCompile(`URL\s*:\s*(https?://\S+vercel\.app)`)

func parseProxyURLs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range reProxyURL.FindAllStringSubmatch(string(data), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}
