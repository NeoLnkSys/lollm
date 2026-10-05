// Package config resolves the effective LoLLM configuration.
//
// Precedence, from highest to lowest (spec §1):
//  1. CLI flags
//  2. Environment variables
//  3. config.yaml
//  4. Built-in defaults
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultHost          = "0.0.0.0"
	DefaultAPIPort       = 20999
	DefaultDashboardPort = 21000
	DefaultDBPath        = "./data/lollm.db"
	DefaultLogLevel      = "info"
	DefaultConfigPath    = "config.yaml"
)

// Config holds the gateway-level configuration (everything that lives in
// config.yaml). Connections, combos, keys, etc. live in SQLite.
type Config struct {
	Host          string `yaml:"host"`
	APIPort       int    `yaml:"api_port"`
	DashboardPort int    `yaml:"dashboard_port"`
	DBPath        string `yaml:"db_path"`
	LogLevel      string `yaml:"log_level"`
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		Host:          DefaultHost,
		APIPort:       DefaultAPIPort,
		DashboardPort: DefaultDashboardPort,
		DBPath:        DefaultDBPath,
		LogLevel:      DefaultLogLevel,
	}
}

// YAML renders the config as YAML (used by `lollm setup`).
func (c *Config) YAML() (string, error) {
	b, err := yaml.Marshal(c)
	return string(b), err
}

// FlagOverrides carry explicit CLI flag values (highest priority). Commands
// should only populate the fields whose flags were actually changed, so env
// vars keep their position in the precedence chain.
type FlagOverrides struct {
	Host          *string
	APIPort       *int
	DashboardPort *int
	DBPath        *string
	LogLevel      *string
}

// LoadOptions control how Load resolves the configuration.
type LoadOptions struct {
	Path     string        // config file path ("" = skip file)
	Required bool          // error if the file is missing
	Flags    FlagOverrides // explicit CLI flag values
}

// Load builds the effective config: defaults < YAML file < env < CLI flags,
// then validates the result.
func Load(opts LoadOptions) (*Config, error) {
	cfg := Default()

	if opts.Path != "" {
		data, err := os.ReadFile(opts.Path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", opts.Path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			if opts.Required {
				return nil, fmt.Errorf("config file not found: %s", opts.Path)
			}
		default:
			return nil, fmt.Errorf("read config %s: %w", opts.Path, err)
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	applyFlags(cfg, opts.Flags)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv overlays environment variables. LOLLM_-prefixed variables win over
// the plain ones (spec example: HOST=0.0.0.0 API_PORT=20999 ./lollm serve).
func applyEnv(cfg *Config) error {
	str := func(names ...string) (string, bool) {
		for _, n := range names {
			if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), true
			}
		}
		return "", false
	}
	num := func(dst *int, names ...string) error {
		if v, ok := str(names...); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("invalid value for %s: %q is not an integer", names[0], v)
			}
			*dst = n
		}
		return nil
	}

	if v, ok := str("LOLLM_HOST", "HOST"); ok {
		cfg.Host = v
	}
	if err := num(&cfg.APIPort, "LOLLM_API_PORT", "API_PORT"); err != nil {
		return err
	}
	if err := num(&cfg.DashboardPort, "LOLLM_DASHBOARD_PORT", "DASHBOARD_PORT"); err != nil {
		return err
	}
	if v, ok := str("LOLLM_DB_PATH", "DB_PATH"); ok {
		cfg.DBPath = v
	}
	if v, ok := str("LOLLM_LOG_LEVEL", "LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}
	return nil
}

func applyFlags(cfg *Config, f FlagOverrides) {
	if f.Host != nil {
		cfg.Host = *f.Host
	}
	if f.APIPort != nil {
		cfg.APIPort = *f.APIPort
	}
	if f.DashboardPort != nil {
		cfg.DashboardPort = *f.DashboardPort
	}
	if f.DBPath != nil {
		cfg.DBPath = *f.DBPath
	}
	if f.LogLevel != nil {
		cfg.LogLevel = *f.LogLevel
	}
}

// Validate checks the merged configuration.
func (c *Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Host) == "" {
		errs = append(errs, errors.New("host must not be empty"))
	}
	if c.APIPort < 1 || c.APIPort > 65535 {
		errs = append(errs, fmt.Errorf("api_port must be 1-65535, got %d", c.APIPort))
	}
	if c.DashboardPort < 1 || c.DashboardPort > 65535 {
		errs = append(errs, fmt.Errorf("dashboard_port must be 1-65535, got %d", c.DashboardPort))
	}
	if c.APIPort == c.DashboardPort {
		errs = append(errs, fmt.Errorf("api_port and dashboard_port must differ (both are %d)", c.APIPort))
	}
	if strings.TrimSpace(c.DBPath) == "" {
		errs = append(errs, errors.New("db_path must not be empty"))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level must be debug|info|warn|error, got %q", c.LogLevel))
	}
	return errors.Join(errs...)
}
