// Package funding implements a single-exchange funding rate arbitrage engine
// targeting Bybit perpetual contracts.
package funding

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// AllocationTier maps a funding rate threshold (in percent, e.g. 0.03)
// to the maximum allocation percentage for that tier.
type AllocationTier struct {
	MinRatePct    float64 `yaml:"min_rate_pct"`
	MaxRatePct    float64 `yaml:"max_rate_pct"`
	AllocationPct float64 `yaml:"allocation_pct"`
}

// Config holds all tunables for the funding rate arbitrage strategy.
type Config struct {
	// Symbols to monitor, e.g. ["BTCUSDT", "ETHUSDT"].
	Symbols []string `yaml:"symbols"`

	// MinAnnualYieldPct is the minimum annualised yield (%) to enter a position.
	MinAnnualYieldPct float64 `yaml:"min_annual_yield_pct"`

	// ExitThresholdRate is the funding rate below which we consider exiting.
	ExitThresholdRate float64 `yaml:"exit_threshold_rate"`

	// NegativeExitCycles: exit after N consecutive cycles with rate < ExitThresholdRate.
	NegativeExitCycles int `yaml:"negative_exit_cycles"`

	// MaxAllocationPct is the maximum percentage of account equity allocated
	// to the funding strategy overall.
	MaxAllocationPct float64 `yaml:"max_allocation_pct"`

	// AllocationByRate determines position sizing based on the predicted
	// funding rate magnitude.
	//   rate > 0.03%  -> 60%
	//   0.01%-0.03%   -> 40%
	//   rate < 0.01%  -> 20%
	AllocationByRate []AllocationTier `yaml:"allocation_by_rate"`

	// FeeBpsTaker is the taker fee on Bybit in basis points (default 5.5).
	FeeBpsTaker float64 `yaml:"fee_bps_taker"`

	// EvalIntervalS is how often (seconds) the engine re-evaluates positions.
	EvalIntervalS int `yaml:"eval_interval_s"`

	// IntentTTLMs is the time-to-live for generated TradeIntents in milliseconds.
	IntentTTLMs int64 `yaml:"intent_ttl_ms"`

	// MaxSlippageBps is the maximum allowed slippage in basis points.
	MaxSlippageBps float64 `yaml:"max_slippage_bps"`

	// CooldownS is the minimum seconds between trades on the same symbol.
	CooldownS int `yaml:"cooldown_s"`

	// SymbolWeights maps symbol -> weight for allocation splitting.
	// e.g. {"BTCUSDT": 0.60, "ETHUSDT": 0.40}
	SymbolWeights map[string]float64 `yaml:"symbol_weights"`
}

// DefaultConfig returns a Config with sensible defaults for HyroTrader challenge.
func DefaultConfig() Config {
	return Config{
		Symbols:           []string{"BTCUSDT", "ETHUSDT"},
		MinAnnualYieldPct: 10.0,
		ExitThresholdRate: 0.0001,
		NegativeExitCycles: 3,
		MaxAllocationPct:  60.0,
		AllocationByRate: []AllocationTier{
			{MinRatePct: 0.03, MaxRatePct: 100.0, AllocationPct: 60.0},
			{MinRatePct: 0.01, MaxRatePct: 0.03, AllocationPct: 40.0},
			{MinRatePct: 0.0, MaxRatePct: 0.01, AllocationPct: 20.0},
		},
		FeeBpsTaker:    5.5,
		EvalIntervalS:  30,
		IntentTTLMs:    10000,
		MaxSlippageBps: 5.0,
		CooldownS:      300,
		SymbolWeights: map[string]float64{
			"BTCUSDT": 0.60,
			"ETHUSDT": 0.40,
		},
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
	if v := os.Getenv("FUNDING_EXIT_THRESHOLD_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.ExitThresholdRate = f
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

// AllocationPctForRate returns the allocation percentage for a given
// funding rate (expressed as a percentage, e.g. 0.03 for 0.03%).
func (c *Config) AllocationPctForRate(ratePct float64) float64 {
	for _, tier := range c.AllocationByRate {
		if ratePct >= tier.MinRatePct && ratePct < tier.MaxRatePct {
			return tier.AllocationPct
		}
	}
	// Fallback: if rate > highest tier, use the highest allocation.
	if len(c.AllocationByRate) > 0 && ratePct >= c.AllocationByRate[0].MinRatePct {
		return c.AllocationByRate[0].AllocationPct
	}
	return 20.0
}
