package arb

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// SpreadPairConfig defines a cross-pair relationship for spread trading.
type SpreadPairConfig struct {
	Primary        string  `yaml:"primary"`          // e.g. "BTCUSDT"
	Hedge          string  `yaml:"hedge"`            // e.g. "ETHUSDT"
	MinCorrelation float64 `yaml:"min_correlation"`  // e.g. 0.70
}

// BasisConfig holds all tunables for the cross-pair spread trading strategy.
type BasisConfig struct {
	// Pairs to monitor for spread trades.
	Pairs []SpreadPairConfig `yaml:"pairs"`

	// EntryStdDev is the number of standard deviations from the 24h mean
	// required to enter a basis trade (default 2.0).
	EntryStdDev float64 `yaml:"entry_std_dev"`

	// ExitStdDev is the number of standard deviations within which
	// the spread must return to trigger an exit (default 0.5).
	ExitStdDev float64 `yaml:"exit_std_dev"`

	// MinAnnualizedBasisPct is the minimum annualised basis (%) to enter
	// a position (default 15.0).
	MinAnnualizedBasisPct float64 `yaml:"min_annualized_basis_pct"`

	// MaxHoldDays is the maximum number of days to hold a basis trade
	// before forcing a time-based exit (default 7).
	MaxHoldDays int `yaml:"max_hold_days"`

	// MaxAllocationPct is the maximum percentage of account equity
	// allocated to basis trades (default 25.0).
	MaxAllocationPct float64 `yaml:"max_allocation_pct"`

	// MaxSlippageBps is the maximum allowed slippage per leg in basis points.
	MaxSlippageBps float64 `yaml:"max_slippage_bps"`

	// CooldownS is the minimum seconds between trades on the same symbol.
	CooldownS int `yaml:"cooldown_s"`

	// IntentTTLMs is the time-to-live for generated TradeIntents in milliseconds.
	IntentTTLMs int64 `yaml:"intent_ttl_ms"`

	// FeeBpsTaker is the taker fee in basis points (Bybit default 5.5).
	FeeBpsTaker float64 `yaml:"fee_bps_taker"`
}

// DefaultBasisConfig returns sensible defaults for HyroTrader challenge.
func DefaultBasisConfig() BasisConfig {
	return BasisConfig{
		Pairs: []SpreadPairConfig{
			{Primary: "BTCUSDT", Hedge: "ETHUSDT", MinCorrelation: 0.70},
		},
		EntryStdDev:           2.0,
		ExitStdDev:            0.5,
		MinAnnualizedBasisPct: 15.0,
		MaxHoldDays:           7,
		MaxAllocationPct:      25.0,
		MaxSlippageBps:        5.0,
		CooldownS:             300,
		IntentTTLMs:           10000,
		FeeBpsTaker:           5.5,
	}
}

// LoadBasisConfig reads a YAML file and returns a BasisConfig, applying
// environment variable overrides where set.
func LoadBasisConfig(path string) (BasisConfig, error) {
	cfg := DefaultBasisConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading basis config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing basis config: %w", err)
	}

	// Environment variable overrides.
	if v := os.Getenv("BASIS_ENTRY_STDDEV"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.EntryStdDev = f
		}
	}
	if v := os.Getenv("BASIS_EXIT_STDDEV"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.ExitStdDev = f
		}
	}
	if v := os.Getenv("BASIS_MIN_ANNUALIZED_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MinAnnualizedBasisPct = f
		}
	}
	if v := os.Getenv("BASIS_MAX_HOLD_DAYS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MaxHoldDays = i
		}
	}
	if v := os.Getenv("BASIS_MAX_ALLOCATION_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxAllocationPct = f
		}
	}
	if v := os.Getenv("BASIS_MAX_SLIPPAGE_BPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxSlippageBps = f
		}
	}
	if v := os.Getenv("BASIS_COOLDOWN_S"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.CooldownS = i
		}
	}

	return cfg, nil
}
