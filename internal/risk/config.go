package risk

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds risk-engine configuration loaded from a YAML file.
// Single-exchange challenge mode — no multi-tenant fields.
type Config struct {
	// ChallengeMode enables stricter drawdown enforcement for prop-firm challenges.
	ChallengeMode bool `yaml:"challenge_mode"`

	// Overall drawdown thresholds (from peak equity ever).
	MaxDrawdownPct      float64 `yaml:"max_drawdown_pct"`
	SafeModeDrawdownPct float64 `yaml:"safe_mode_drawdown_pct"`

	// Daily drawdown thresholds (from daily high water mark, reset at UTC midnight).
	DailySoftHaltPct float64 `yaml:"daily_soft_halt_pct"`
	DailyHardHaltPct float64 `yaml:"daily_hard_halt_pct"`

	// Error-rate threshold: if more than this percentage of operations in a 5-minute
	// window are errors, we escalate.
	MaxErrorRate5mPct float64 `yaml:"max_error_rate_5m_pct"`

	// MaxHedgeDriftMs is the maximum tolerable delay (ms) between legs of a hedge
	// before the risk engine raises an alert.
	MaxHedgeDriftMs int64 `yaml:"max_hedge_drift_ms"`

	// StaleDataPauseMs: if market data is older than this many ms we pause trading.
	StaleDataPauseMs int64 `yaml:"stale_data_pause_ms"`

	// PositionReconIntervalMs: how often (ms) to reconcile local vs exchange positions.
	PositionReconIntervalMs int64 `yaml:"position_recon_interval_ms"`

	// Leg mismatch thresholds.
	LegMismatchAlertMs int64 `yaml:"leg_mismatch_alert_ms"`
	LegMismatchCloseMs int64 `yaml:"leg_mismatch_close_ms"`

	// KellyFraction controls position sizing aggressiveness (0.0–1.0).
	KellyFraction float64 `yaml:"kelly_fraction"`

	// RedisAddr is populated from the REDIS_ADDR environment variable.
	RedisAddr string `yaml:"-"`
}

// DefaultConfig returns sane defaults for challenge mode.
func DefaultConfig() Config {
	return Config{
		ChallengeMode:           true,
		MaxDrawdownPct:          8.0,
		SafeModeDrawdownPct:     5.0,
		DailySoftHaltPct:        3.0,
		DailyHardHaltPct:        5.0,
		MaxErrorRate5mPct:       10.0,
		MaxHedgeDriftMs:         2000,
		StaleDataPauseMs:        5000,
		PositionReconIntervalMs: 10000,
		LegMismatchAlertMs:      3000,
		LegMismatchCloseMs:      10000,
		KellyFraction:           0.25,
	}
}

// LoadConfig reads a YAML file and returns a Config with environment variable overrides.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("risk: read config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("risk: parse config %s: %w", path, err)
	}

	// Environment variable overrides.
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.RedisAddr = v
	}

	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("risk: invalid config: %w", err)
	}

	return cfg, nil
}

// validate checks that configuration values are within acceptable ranges.
func (c *Config) validate() error {
	if c.MaxDrawdownPct <= 0 || c.MaxDrawdownPct > 100 {
		return fmt.Errorf("max_drawdown_pct must be in (0, 100], got %f", c.MaxDrawdownPct)
	}
	if c.SafeModeDrawdownPct <= 0 || c.SafeModeDrawdownPct >= c.MaxDrawdownPct {
		return fmt.Errorf("safe_mode_drawdown_pct must be in (0, max_drawdown_pct), got %f", c.SafeModeDrawdownPct)
	}
	if c.DailySoftHaltPct <= 0 {
		return fmt.Errorf("daily_soft_halt_pct must be > 0, got %f", c.DailySoftHaltPct)
	}
	if c.DailyHardHaltPct <= c.DailySoftHaltPct {
		return fmt.Errorf("daily_hard_halt_pct (%f) must be > daily_soft_halt_pct (%f)", c.DailyHardHaltPct, c.DailySoftHaltPct)
	}
	if c.KellyFraction < 0 || c.KellyFraction > 1 {
		return fmt.Errorf("kelly_fraction must be in [0, 1], got %f", c.KellyFraction)
	}
	if c.MaxErrorRate5mPct <= 0 || c.MaxErrorRate5mPct > 100 {
		return fmt.Errorf("max_error_rate_5m_pct must be in (0, 100], got %f", c.MaxErrorRate5mPct)
	}
	return nil
}
