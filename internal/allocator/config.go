// Package allocator provides capital allocation and position sizing for the
// trading challenge. It validates intents against strategy-level and total
// allocation limits before they reach execution.
package allocator

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds capital allocator tunables.
type Config struct {
	// InitialCapitalUSD is the starting account balance for the challenge.
	InitialCapitalUSD float64 `yaml:"initial_capital_usd"`

	// FundingMaxPct is the maximum percentage of equity allocated to
	// the funding rate strategy.
	FundingMaxPct float64 `yaml:"funding_max_pct"`

	// BasisMaxPct is the maximum percentage of equity allocated to
	// the basis trading strategy.
	BasisMaxPct float64 `yaml:"basis_max_pct"`

	// CashReservePct is the percentage of equity always kept in cash
	// (never allocated to any strategy).
	CashReservePct float64 `yaml:"cash_reserve_pct"`

	// DrawdownScaling enables dynamic allocation reduction as drawdown
	// increases. When true, allocations are linearly scaled down as
	// drawdown approaches limits.
	DrawdownScaling bool `yaml:"drawdown_scaling"`

	// RebalanceIntervalMin is how often (minutes) to rebalance allocations.
	RebalanceIntervalMin int `yaml:"rebalance_interval_min"`

	// KellyFraction is the Kelly criterion fraction used for position sizing.
	// Lower values are more conservative (0.15 for challenge mode).
	KellyFraction float64 `yaml:"kelly_fraction"`
}

// DefaultConfig returns sensible defaults for the HyroTrader challenge.
func DefaultConfig() Config {
	return Config{
		InitialCapitalUSD:    100000.0,
		FundingMaxPct:        60.0,
		BasisMaxPct:          25.0,
		CashReservePct:       12.0,
		DrawdownScaling:      true,
		RebalanceIntervalMin: 30,
		KellyFraction:        0.15,
	}
}

// LoadConfig reads a YAML file and returns a Config, applying environment
// variable overrides where set.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading allocator config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing allocator config: %w", err)
	}

	// Environment variable overrides.
	if v := os.Getenv("ALLOC_INITIAL_CAPITAL_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.InitialCapitalUSD = f
		}
	}
	if v := os.Getenv("ALLOC_FUNDING_MAX_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.FundingMaxPct = f
		}
	}
	if v := os.Getenv("ALLOC_BASIS_MAX_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.BasisMaxPct = f
		}
	}
	if v := os.Getenv("ALLOC_CASH_RESERVE_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.CashReservePct = f
		}
	}
	if v := os.Getenv("ALLOC_KELLY_FRACTION"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.KellyFraction = f
		}
	}
	if v := os.Getenv("ALLOC_DRAWDOWN_SCALING"); v != "" {
		cfg.DrawdownScaling = v == "true" || v == "1"
	}
	if v := os.Getenv("ALLOC_REBALANCE_INTERVAL_MIN"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.RebalanceIntervalMin = i
		}
	}

	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("allocator: invalid config: %w", err)
	}

	return cfg, nil
}

// validate checks that configuration values are within acceptable ranges.
func (c *Config) validate() error {
	if c.InitialCapitalUSD <= 0 {
		return fmt.Errorf("initial_capital_usd must be positive, got %f", c.InitialCapitalUSD)
	}
	totalMax := c.FundingMaxPct + c.BasisMaxPct + c.CashReservePct
	if totalMax > 100.0 {
		return fmt.Errorf("funding_max_pct (%.1f) + basis_max_pct (%.1f) + cash_reserve_pct (%.1f) = %.1f%% exceeds 100%%",
			c.FundingMaxPct, c.BasisMaxPct, c.CashReservePct, totalMax)
	}
	if c.KellyFraction < 0 || c.KellyFraction > 1 {
		return fmt.Errorf("kelly_fraction must be in [0, 1], got %f", c.KellyFraction)
	}
	return nil
}
