// Package funding implements a cross-pair funding rate arbitrage engine
// using USDT perpetual contracts on Bybit.
// Strategy: short the high-funding perp + long the correlated hedge perp.
package funding

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// FundingPairConfig defines a cross-pair trading relationship for funding arb.
type FundingPairConfig struct {
	Primary        string  `yaml:"primary"`          // e.g. "BTCUSDT"
	Hedge          string  `yaml:"hedge"`            // e.g. "ETHUSDT"
	MinCorrelation float64 `yaml:"min_correlation"`  // e.g. 0.70
}

// AllocationTier maps a funding differential threshold (in bps)
// to the maximum allocation percentage for that tier.
type AllocationTier struct {
	MinDiffBps    float64 `yaml:"min_diff_bps"`
	MaxDiffBps    float64 `yaml:"max_diff_bps"`
	AllocationPct float64 `yaml:"allocation_pct"`
}

// Config holds all tunables for the cross-pair funding rate arbitrage strategy.
type Config struct {
	// Pairs to monitor for funding rate differential.
	Pairs []FundingPairConfig `yaml:"pairs"`

	// MinAnnualYieldPct is the minimum annualised yield (%) from funding
	// rate differential to enter a position.
	MinAnnualYieldPct float64 `yaml:"min_annual_yield_pct"`

	// ExitThresholdDiffBps is the funding rate differential (bps) below
	// which we consider exiting.
	ExitThresholdDiffBps float64 `yaml:"exit_threshold_diff_bps"`

	// NegativeExitCycles: exit after N consecutive cycles with diff < threshold.
	NegativeExitCycles int `yaml:"negative_exit_cycles"`

	// MaxAllocationPct is the maximum percentage of account equity allocated
	// to the funding strategy overall.
	MaxAllocationPct float64 `yaml:"max_allocation_pct"`

	// AllocationByDiff determines position sizing based on funding differential.
	AllocationByDiff []AllocationTier `yaml:"allocation_by_diff"`

	// FeeBpsTaker is the taker fee on Bybit in basis points (default 5.5).
	FeeBpsTaker float64 `yaml:"fee_bps_taker"`

	// EvalIntervalS is how often (seconds) the engine re-evaluates positions.
	EvalIntervalS int `yaml:"eval_interval_s"`

	// IntentTTLMs is the time-to-live for generated TradeIntents in milliseconds.
	IntentTTLMs int64 `yaml:"intent_ttl_ms"`

	// MaxSlippageBps is the maximum allowed slippage in basis points.
	MaxSlippageBps float64 `yaml:"max_slippage_bps"`

	// CooldownS is the minimum seconds between trades on the same pair.
	CooldownS int `yaml:"cooldown_s"`
}

// DefaultConfig returns a Config with sensible defaults for HyroTrader challenge.
func DefaultConfig() Config {
	return Config{
		Pairs: []FundingPairConfig{
			{Primary: "BTCUSDT", Hedge: "ETHUSDT", MinCorrelation: 0.70},
		},
		MinAnnualYieldPct:    10.0,
		ExitThresholdDiffBps: 0.5,
		NegativeExitCycles:   3,
		MaxAllocationPct:     60.0,
		AllocationByDiff: []AllocationTier{
			{MinDiffBps: 30.0, MaxDiffBps: 10000.0, AllocationPct: 60.0},
			{MinDiffBps: 10.0, MaxDiffBps: 30.0, AllocationPct: 40.0},
			{MinDiffBps: 0.0, MaxDiffBps: 10.0, AllocationPct: 20.0},
		},
		FeeBpsTaker:    5.5,
		EvalIntervalS:  30,
		IntentTTLMs:    10000,
		MaxSlippageBps: 5.0,
		CooldownS:      300,
	}
}

// LoadConfig reads a YAML file and returns a Config, applying environment
// variable overrides where set.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading funding config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing funding config: %w", err)
	}

	// Environment variable overrides.
	if v := os.Getenv("FUNDING_MIN_ANNUAL_YIELD_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MinAnnualYieldPct = f
		}
	}
	if v := os.Getenv("FUNDING_EXIT_THRESHOLD_DIFF_BPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.ExitThresholdDiffBps = f
		}
	}
	if v := os.Getenv("FUNDING_MAX_ALLOCATION_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxAllocationPct = f
		}
	}
	if v := os.Getenv("FUNDING_FEE_BPS_TAKER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.FeeBpsTaker = f
		}
	}
	if v := os.Getenv("FUNDING_EVAL_INTERVAL_S"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.EvalIntervalS = i
		}
	}
	if v := os.Getenv("FUNDING_INTENT_TTL_MS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.IntentTTLMs = i
		}
	}
	if v := os.Getenv("FUNDING_MAX_SLIPPAGE_BPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxSlippageBps = f
		}
	}
	if v := os.Getenv("FUNDING_COOLDOWN_S"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.CooldownS = i
		}
	}
	if v := os.Getenv("FUNDING_NEGATIVE_EXIT_CYCLES"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.NegativeExitCycles = i
		}
	}

	return cfg, nil
}

// AllocationPctForDiff returns the allocation percentage for a given
// funding rate differential (in bps).
func (c *Config) AllocationPctForDiff(diffBps float64) float64 {
	for _, tier := range c.AllocationByDiff {
		if diffBps >= tier.MinDiffBps && diffBps < tier.MaxDiffBps {
			return tier.AllocationPct
		}
	}
	if len(c.AllocationByDiff) > 0 && diffBps >= c.AllocationByDiff[0].MinDiffBps {
		return c.AllocationByDiff[0].AllocationPct
	}
	return 20.0
}
