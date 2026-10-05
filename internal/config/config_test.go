package config

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv neutralizes config-relevant environment variables for a test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"HOST", "API_PORT", "DASHBOARD_PORT", "DB_PATH", "LOG_LEVEL",
		"LOLLM_HOST", "LOLLM_API_PORT", "LOLLM_DASHBOARD_PORT", "LOLLM_DB_PATH", "LOLLM_LOG_LEVEL",
	} {
		t.Setenv(name, "")
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(LoadOptions{}) // no file, no env, no flags
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != DefaultHost || cfg.APIPort != DefaultAPIPort ||
		cfg.DashboardPort != DefaultDashboardPort || cfg.DBPath != DefaultDBPath ||
		cfg.LogLevel != DefaultLogLevel {
		t.Fatalf("expected defaults, got %+v", cfg)
	}
}

func TestLoadYAMLOverridesDefaults(t *testing.T) {
	clearEnv(t)
	path := writeFile(t, t.TempDir(), "config.yaml", `
host: "127.0.0.1"
api_port: 30001
dashboard_port: 30002
db_path: "./x/lollm.db"
log_level: "debug"
`)
	cfg, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.APIPort != 30001 || cfg.DashboardPort != 30002 ||
		cfg.DBPath != "./x/lollm.db" || cfg.LogLevel != "debug" {
		t.Fatalf("yaml not applied: %+v", cfg)
	}
}

func TestLoadEnvOverridesYAML(t *testing.T) {
	clearEnv(t)
	path := writeFile(t, t.TempDir(), "config.yaml", "host: \"127.0.0.1\"\napi_port: 30001\n")
	t.Setenv("HOST", "10.0.0.5")
	t.Setenv("API_PORT", "30099")

	cfg, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "10.0.0.5" || cfg.APIPort != 30099 {
		t.Fatalf("env not applied over yaml: %+v", cfg)
	}
}

func TestLoadPrefixedEnvWins(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOST", "10.0.0.1")
	t.Setenv("LOLLM_HOST", "10.0.0.2")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "10.0.0.2" {
		t.Fatalf("LOLLM_HOST should win over HOST: %+v", cfg)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("API_PORT", "30099")
	flagPort := 40001
	cfg, err := Load(LoadOptions{Flags: FlagOverrides{APIPort: &flagPort}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIPort != 40001 {
		t.Fatalf("flags should win over env: %+v", cfg)
	}
}

func TestLoadUnsetFlagsDoNotClobberEnv(t *testing.T) {
	// The key regression guard: commands only pass flags the user actually
	// typed, so unset flags must never hide environment variables.
	clearEnv(t)
	t.Setenv("API_PORT", "30099")
	cfg, err := Load(LoadOptions{Flags: FlagOverrides{}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIPort != 30099 {
		t.Fatalf("empty overrides should keep env value: %+v", cfg)
	}
}

func TestLoadInvalidEnvInt(t *testing.T) {
	clearEnv(t)
	t.Setenv("API_PORT", "not-a-number")
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatal("expected error for non-integer API_PORT")
	}
}

func TestLoadValidation(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name string
		fix  func(*Config)
		want string
	}{
		{"empty host", func(c *Config) { c.Host = "" }, "host"},
		{"port zero", func(c *Config) { c.APIPort = 0 }, "api_port"},
		{"port too high", func(c *Config) { c.DashboardPort = 70000 }, "dashboard_port"},
		{"same ports", func(c *Config) { c.APIPort = 20999; c.DashboardPort = 20999 }, "must differ"},
		{"bad log level", func(c *Config) { c.LogLevel = "loud" }, "log_level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.fix(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestLoadMissingRequiredFile(t *testing.T) {
	clearEnv(t)
	_, err := Load(LoadOptions{Path: "/nonexistent/config.yaml", Required: true})
	if err == nil {
		t.Fatal("expected error for missing required config file")
	}
}

func TestLoadMissingOptionalFileIsFine(t *testing.T) {
	clearEnv(t)
	if _, err := Load(LoadOptions{Path: "/nonexistent/config.yaml"}); err != nil {
		t.Fatalf("optional missing file must not error: %v", err)
	}
}
