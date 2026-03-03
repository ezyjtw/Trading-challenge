// Package challenge implements the HyroTrader compliance layer.
// It enforces all challenge rules (drawdown limits, stop-loss requirements,
// profit distribution, exposure caps) before any trade intent reaches execution.
package challenge

import (
	"fmt"
	"os"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// Mode represents the current challenge phase.
type Mode string

const (
	ModePhase1 Mode = "phase1"
	ModePhase2 Mode = "phase2"
	ModeFunded Mode = "funded"
)

// DrawdownLevel classifies the severity of a drawdown reading.
type DrawdownLevel int

const (
	DrawdownNormal DrawdownLevel = iota
	DrawdownSoft
	DrawdownHard
	DrawdownBreach
)

// String returns a human-readable label for a DrawdownLevel.
func (d DrawdownLevel) String() string {
	switch d {
	case DrawdownSoft:
		return "SOFT_HALT"
	case DrawdownHard:
		return "HARD_HALT"
	case DrawdownBreach:
		return "BREACH"
	default:
		return "NORMAL"
	}
}

// Config holds all HyroTrader challenge parameters.
// Values are loaded from YAML and converted to decimal.Decimal for
// precision-safe monetary calculations.
type Config struct {
	Enabled bool `yaml:"enabled"`
	Mode    Mode `yaml:"mode"`

	// InitialBalance is the starting account balance (e.g. 100000 USD).
	InitialBalance decimal.Decimal `yaml:"initial_balance"`

	// Daily drawdown thresholds as percentages of daily high water mark.
	DailyDrawdownSoftPct   decimal.Decimal `yaml:"daily_drawdown_soft_pct"`
	DailyDrawdownHardPct   decimal.Decimal `yaml:"daily_drawdown_hard_pct"`
	DailyDrawdownBreachPct decimal.Decimal `yaml:"daily_drawdown_breach_pct"`

	// Overall drawdown thresholds as percentages of initial balance.
	OverallDrawdownSoftPct   decimal.Decimal `yaml:"overall_drawdown_soft_pct"`
	OverallDrawdownHardPct   decimal.Decimal `yaml:"overall_drawdown_hard_pct"`
	OverallDrawdownBreachPct decimal.Decimal `yaml:"overall_drawdown_breach_pct"`

	// MaxRiskPerTradePct is the maximum allowed risk per trade as a percentage
	// of initial balance (stop-loss distance * position size).
	MaxRiskPerTradePct decimal.Decimal `yaml:"max_risk_per_trade_pct"`

	// ProfitDistributionMaxPct caps how much of total profit a single trade
	// can represent. Only enforced in phase1 and phase2.
	ProfitDistributionMaxPct decimal.Decimal `yaml:"profit_distribution_max_pct"`

	// MinTradingDays is the minimum number of calendar days with at least one
	// qualifying trade before the challenge phase can be considered passed.
	MinTradingDays int `yaml:"min_trading_days"`

	// StopLossDeadlineSeconds is the maximum time in seconds after opening a
	// position before a stop-loss must be attached.
	StopLossDeadlineSeconds int `yaml:"stop_loss_deadline_seconds"`

	// StopLossDefaultPct is the default emergency stop-loss distance as a
	// percentage of entry price (used for delta-neutral legs).
	StopLossDefaultPct decimal.Decimal `yaml:"stop_loss_default_pct"`

	// LowCapThresholdUSD defines the market-cap boundary below which a token
	// is considered low-cap and subject to allocation restrictions.
	LowCapThresholdUSD decimal.Decimal `yaml:"low_cap_threshold_usd"`

	// LowCapMaxAllocationPct is the maximum percentage of balance that can
	// be allocated to low-cap tokens.
	LowCapMaxAllocationPct decimal.Decimal `yaml:"low_cap_max_allocation_pct"`

	// FundedMaxMarginExposurePct caps total margin across all open positions
	// as a percentage of initial balance. Only enforced in funded mode.
	FundedMaxMarginExposurePct decimal.Decimal `yaml:"funded_max_margin_exposure_pct"`

	// FundedMaxNotionalMultiplier caps cumulative open position notional value
	// as a multiple of account balance. Only enforced in funded mode.
	FundedMaxNotionalMultiplier decimal.Decimal `yaml:"funded_max_notional_multiplier"`

	// SafetyTradeTimeUTC is the UTC wall-clock time (HH:MM) at which a
	// minimum safety trade is placed if no trade has occurred that day.
	SafetyTradeTimeUTC string `yaml:"safety_trade_time_utc"`

	// SafetyTradeSizeUSD is the notional value of the safety trade in USD.
	SafetyTradeSizeUSD decimal.Decimal `yaml:"safety_trade_size_usd"`
}

// fileWrapper mirrors the top-level YAML structure: challenge key wraps Config.
type fileWrapper struct {
	Challenge Config `yaml:"challenge"`
}

// LoadConfig reads a YAML file from disk and returns the parsed Config.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("challenge: reading config %s: %w", path, err)
	}
	return ParseConfig(data)
}

// ParseConfig unmarshals raw YAML bytes into a Config.
func ParseConfig(data []byte) (*Config, error) {
	var wrapper fileWrapper
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("challenge: parsing config: %w", err)
	}
	cfg := wrapper.Challenge
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("challenge: invalid config: %w", err)
	}
	return &cfg, nil
}

// validate performs basic sanity checks on the loaded configuration.
func (c *Config) validate() error {
	if c.InitialBalance.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("initial_balance must be positive, got %s", c.InitialBalance)
	}
	switch c.Mode {
	case ModePhase1, ModePhase2, ModeFunded:
		// valid
	default:
		return fmt.Errorf("unknown mode %q, expected phase1|phase2|funded", c.Mode)
	}
	if c.DailyDrawdownSoftPct.GreaterThanOrEqual(c.DailyDrawdownHardPct) {
		return fmt.Errorf("daily_drawdown_soft_pct (%s) must be less than hard (%s)",
			c.DailyDrawdownSoftPct, c.DailyDrawdownHardPct)
	}
	if c.DailyDrawdownHardPct.GreaterThanOrEqual(c.DailyDrawdownBreachPct) {
		return fmt.Errorf("daily_drawdown_hard_pct (%s) must be less than breach (%s)",
			c.DailyDrawdownHardPct, c.DailyDrawdownBreachPct)
	}
	if c.OverallDrawdownSoftPct.GreaterThanOrEqual(c.OverallDrawdownHardPct) {
		return fmt.Errorf("overall_drawdown_soft_pct (%s) must be less than hard (%s)",
			c.OverallDrawdownSoftPct, c.OverallDrawdownHardPct)
	}
	if c.OverallDrawdownHardPct.GreaterThanOrEqual(c.OverallDrawdownBreachPct) {
		return fmt.Errorf("overall_drawdown_hard_pct (%s) must be less than breach (%s)",
			c.OverallDrawdownHardPct, c.OverallDrawdownBreachPct)
	}
	if c.StopLossDeadlineSeconds <= 0 {
		return fmt.Errorf("stop_loss_deadline_seconds must be positive")
	}
	if c.MinTradingDays <= 0 {
		return fmt.Errorf("min_trading_days must be positive")
	}
	if _, err := c.ParseSafetyTradeTime(); err != nil {
		return fmt.Errorf("safety_trade_time_utc %q: %w", c.SafetyTradeTimeUTC, err)
	}
	return nil
}

// ParseSafetyTradeTime returns the safety trade wall-clock time as hour and minute.
func (c *Config) ParseSafetyTradeTime() (time.Time, error) {
	t, err := time.Parse("15:04", c.SafetyTradeTimeUTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time format %q: expected HH:MM", c.SafetyTradeTimeUTC)
	}
	return t, nil
}

// StopLossDeadline returns the stop-loss attachment deadline as a time.Duration.
func (c *Config) StopLossDeadline() time.Duration {
	return time.Duration(c.StopLossDeadlineSeconds) * time.Second
}

// MaxRiskPerTrade returns the absolute dollar amount of max risk per trade.
func (c *Config) MaxRiskPerTrade() decimal.Decimal {
	return c.InitialBalance.Mul(c.MaxRiskPerTradePct).Div(decimal.NewFromInt(100))
}

// IsChallengePhase returns true when the mode is phase1 or phase2 (not funded).
func (c *Config) IsChallengePhase() bool {
	return c.Mode == ModePhase1 || c.Mode == ModePhase2
}

// hundred is a convenience constant to avoid repeated allocation.
var hundred = decimal.NewFromInt(100)
