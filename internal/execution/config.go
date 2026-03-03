package execution

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds execution engine tunables.
type Config struct {
	// MaxRetriesPerLeg is the maximum number of retry attempts for a single leg.
	MaxRetriesPerLeg int `yaml:"max_retries_per_leg"`

	// HedgeDriftMaxMs is the maximum allowed delay (ms) between leg A fill
	// and leg B fill. Exceeding this triggers emergency unwind. Tighter for
	// challenge mode: 2 seconds.
	HedgeDriftMaxMs int64 `yaml:"hedge_drift_max_ms"`

	// MinPartialFillPct is the minimum partial fill percentage (0.0-1.0)
	// to consider a leg viable for hedging.
	MinPartialFillPct float64 `yaml:"min_partial_fill_pct"`

	// ReconDelayMs is the delay before reconciliation after a fill.
	ReconDelayMs int64 `yaml:"recon_delay_ms"`

	// ReconIntervalMs is how often (ms) to run periodic position reconciliation.
	ReconIntervalMs int64 `yaml:"recon_interval_ms"`

	// FeeBpsTaker is the taker fee in basis points (Bybit default 5.5).
	FeeBpsTaker float64 `yaml:"fee_bps_taker"`

	// FeeBpsMaker is the maker fee in basis points (Bybit default 2.0).
	FeeBpsMaker float64 `yaml:"fee_bps_maker"`
}

// DefaultConfig returns sensible defaults for challenge mode execution.
func DefaultConfig() Config {
	return Config{
		MaxRetriesPerLeg:  3,
		HedgeDriftMaxMs:   2000,
		MinPartialFillPct: 0.10,
		ReconDelayMs:      2000,
		ReconIntervalMs:   60000,
		FeeBpsTaker:       5.5,
		FeeBpsMaker:       2.0,
	}
}

// LoadConfig reads a YAML file and returns a Config, applying environment
// variable overrides where set.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading execution config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing execution config: %w", err)
	}

	// Environment variable overrides.
	if v := os.Getenv("EXEC_MAX_RETRIES"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MaxRetriesPerLeg = i
		}
	}
	if v := os.Getenv("EXEC_HEDGE_DRIFT_MAX_MS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.HedgeDriftMaxMs = i
		}
	}
	if v := os.Getenv("EXEC_MIN_PARTIAL_FILL_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MinPartialFillPct = f
		}
	}
	if v := os.Getenv("EXEC_RECON_DELAY_MS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ReconDelayMs = i
		}
	}
	if v := os.Getenv("EXEC_RECON_INTERVAL_MS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ReconIntervalMs = i
		}
	}
	if v := os.Getenv("EXEC_FEE_BPS_TAKER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.FeeBpsTaker = f
		}
	}
	if v := os.Getenv("EXEC_FEE_BPS_MAKER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.FeeBpsMaker = f
		}
	}

	return cfg, nil
}
