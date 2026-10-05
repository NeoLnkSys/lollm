// Command recon validates every provider key in a backup-format txt file
// against the live provider APIs (no mock), captures each connection's real
// model catalog, and writes a JSON report for the import step.
//
//	go run ./cmd/recon -in ../../uploads/backup_clean-1.txt -out ../live/recon.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/secret"
)

type keyEntry struct {
	Provider  string `json:"provider"`
	Tag       string `json:"tag"`
	Status    string `json:"status"` // from the backup file
	APIKey    string `json:"api_key"`
	AccountID string `json:"account_id,omitempty"`
}

type reconResult struct {
	keyEntry
	Valid      bool     `json:"valid"`
	HTTPStatus int      `json:"http_status"`
	Err        string   `json:"error,omitempty"`
	Models     []string `json:"models"`
	LatencyMs  int64    `json:"latency_ms"`
}

var (
	reProvider = regexp.MustCompile(`^#### PROVIDER:\s+(\S+)`)
	reKeyEntry = regexp.MustCompile(`^\s{2}\[(\w+)\]\s+Status:\s+(\w+)`)
	reAPIKey   = regexp.MustCompile(`^\s+API Key\s*:\s*(\S+)`)
	reAccount  = regexp.MustCompile(`^\s+Account ID\s*:\s*(\S+)`)
)

func parseBackup(path string) ([]keyEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []keyEntry
	provider := ""
	for _, line := range strings.Split(string(data), "\n") {
		if m := reProvider.FindStringSubmatch(line); m != nil {
			provider = strings.ToLower(strings.TrimSuffix(m[1], ":"))
			continue
		}
		if m := reKeyEntry.FindStringSubmatch(line); m != nil {
			entries = append(entries, keyEntry{Provider: provider, Tag: m[1], Status: m[2]})
			continue
		}
		if m := reAPIKey.FindStringSubmatch(line); m != nil && len(entries) > 0 {
			entries[len(entries)-1].APIKey = m[1]
			continue
		}
		if m := reAccount.FindStringSubmatch(line); m != nil && len(entries) > 0 {
			entries[len(entries)-1].AccountID = m[1]
		}
	}
	// Drop entries without keys (e.g. section headers parsed by accident).
	out := entries[:0]
	for _, e := range entries {
		if e.APIKey != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// connFor builds a throwaway connection for the adapter probe.
func connFor(e keyEntry) *db.Connection {
	c := &db.Connection{
		ID: "recon", Name: e.Tag, Provider: e.Provider,
		IsActive: true, Status: db.StatusActive,
	}
	// Cloudflare needs the account-scoped base URL; keys are stored in the
	// clear only inside this throwaway struct (never persisted).
	c.APIKeyEncrypted = "plaintext:" + e.APIKey
	if e.Provider == "cloudflare-ai" && e.AccountID != "" {
		c.BaseURL = "https://api.cloudflare.com/client/v4/accounts/" + e.AccountID + "/ai/v1"
	}
	return c
}

// reconMasterKey encrypts keys for the in-memory probe only (the throwaway
// connections are never persisted; the report file is written 0600).
var reconMasterKey = make([]byte, 32)

func encryptForRecon(plain string) string {
	enc, err := secret.EncryptString(reconMasterKey, plain)
	if err != nil {
		panic(err)
	}
	return enc
}

func main() {
	in := flag.String("in", "", "backup txt file")
	out := flag.String("out", "recon.json", "output JSON")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "usage: recon -in <backup.txt> -out <recon.json>")
		os.Exit(2)
	}

	entries, err := parseBackup(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	fmt.Printf("parsed %d keys from %s — probing live (this hits real provider APIs)…\n\n", len(entries), *in)

	results := make([]reconResult, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12) // parallelism cap
	for i, e := range entries {
		wg.Add(1)
		go func(i int, e keyEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probe(e)
		}(i, e)
	}
	wg.Wait()

	// Report
	byProvider := map[string][]reconResult{}
	for _, r := range results {
		byProvider[r.Provider] = append(byProvider[r.Provider], r)
	}
	names := make([]string, 0, len(byProvider))
	for n := range byProvider {
		names = append(names, n)
	}
	sort.Strings(names)
	totalValid := 0
	for _, n := range names {
		rs := byProvider[n]
		valid := 0
		var models = map[string]bool{}
		for _, r := range rs {
			if r.Valid {
				valid++
				for _, m := range r.Models {
					models[m] = true
				}
			}
		}
		totalValid += valid
		fmt.Printf("=== %s: %d/%d keys valid, %d unique models\n", n, valid, len(rs), len(models))
		for _, r := range rs {
			mark := "✖"
			if r.Valid {
				mark = "✔"
			}
			extra := ""
			if r.Err != "" {
				extra = " — " + r.Err
			}
			fmt.Printf("  %s %-6s %4dms %3d models%s\n", mark, r.Tag, r.LatencyMs, len(r.Models), extra)
		}
		modelList := make([]string, 0, len(models))
		for m := range models {
			modelList = append(modelList, m)
		}
		sort.Strings(modelList)
		if len(modelList) > 0 {
			show := modelList
			if len(show) > 8 {
				show = show[:8]
			}
			fmt.Printf("  models: %s", strings.Join(show, ", "))
			if len(modelList) > 8 {
				fmt.Printf(" … (+%d more)", len(modelList)-8)
			}
			fmt.Println()
		}
		fmt.Println()
	}
	fmt.Printf("TOTAL: %d/%d keys valid\n", totalValid, len(results))

	blob, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*out, blob, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("full report → %s (contains raw keys — keep local)\n", *out)
}

func probe(e keyEntry) reconResult {
	r := reconResult{keyEntry: e, Models: []string{}}
	meta, ok := providers.Lookup(e.Provider)
	if !ok {
		r.Err = "unknown provider"
		return r
	}
	adapter := providers.NewOpenAICompat(meta, reconMasterKey, nil)
	conn := connFor(e)
	conn.APIKeyEncrypted = encryptForRecon(e.APIKey)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()

	switch e.Provider {
	case "cloudflare-ai":
		// No OpenAI-style GET /models on Cloudflare: pull the catalog from the
		// account models-search endpoint, then prove the key with a real chat.
		r.Models = cfCatalog(ctx, e)
		r = chatProbe(adapter, conn, e, r, "@cf/meta/llama-3.3-70b-instruct-fp8-fast")
		r.LatencyMs = time.Since(start).Milliseconds()
		return r

	case "ollama":
		// Ollama 32-hex keys pass GET /models but are rejected by chat —
		// only a real completion proves the key is usable.
		models, err := adapter.ListModels(ctx, conn)
		if err == nil {
			r.Models = models
		}
		r = chatProbe(adapter, conn, e, r, "gpt-oss:20b")
		r.LatencyMs = time.Since(start).Milliseconds()
		return r
	}

	models, err := adapter.ListModels(ctx, conn)
	r.LatencyMs = time.Since(start).Milliseconds()
	if err == nil {
		// A 200 on an authenticated /models proves the key.
		r.Valid = true
		r.HTTPStatus = 200
		r.Models = models
		return r
	}
	if pe, ok := err.(*providers.ProviderError); ok {
		r.HTTPStatus = pe.Status
		r.Err = fmt.Sprintf("%s: %s", pe.Kind, pe.Message)
		if pe.Status == 404 || pe.Status == 405 {
			r = chatProbe(adapter, conn, e, r, probeModel(e.Provider, models))
		}
	} else {
		r.Err = err.Error()
	}
	return r
}

// probeModel picks a small/cheap model for a chat probe.
func probeModel(provider string, listed []string) string {
	if len(listed) > 0 {
		return listed[0]
	}
	switch provider {
	case "gemini":
		return "gemini-2.5-flash"
	case "groq":
		return "llama-3.1-8b-instant"
	case "openrouter":
		return "meta-llama/llama-3.3-70b-instruct:free"
	}
	return "gpt-oss:20b"
}

func chatProbe(adapter providers.Adapter, conn *db.Connection, e keyEntry, r reconResult, model string) reconResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if model == "" {
		model = "gpt-oss:20b"
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": 5,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly: OK"}},
	}
	_, err := adapter.Chat(ctx, conn, &providers.ChatRequest{Body: body, Model: model})
	if err == nil {
		r.Valid = true
		r.HTTPStatus = 200
		if r.Err == "" {
			r.Err = "verified via real chat completion"
		}
		return r
	}
	if pe, ok := err.(*providers.ProviderError); ok {
		r.HTTPStatus = pe.Status
		r.Err = fmt.Sprintf("%s: %s (chat probe with %s)", pe.Kind, pe.Message, model)
	} else {
		r.Err = err.Error() + " (chat probe)"
	}
	return r
}

// cfCatalog fetches the Cloudflare Workers AI model catalog for an account.
func cfCatalog(ctx context.Context, e keyEntry) []string {
	if e.AccountID == "" {
		return nil
	}
	url := "https://api.cloudflare.com/client/v4/accounts/" + e.AccountID + "/ai/models/search?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+e.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var out struct {
		Result []struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out) != nil {
		return nil
	}
	var names []string
	for _, m := range out.Result {
		if strings.HasPrefix(m.Name, "@cf/") {
			names = append(names, m.Name)
		}
	}
	return names
}
