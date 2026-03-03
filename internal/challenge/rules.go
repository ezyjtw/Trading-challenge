package challenge

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Daily drawdown
// ---------------------------------------------------------------------------

// DailyDrawdownResult contains the outcome of a daily drawdown check.
type DailyDrawdownResult struct {
	Level         DrawdownLevel
	DrawdownPct   decimal.Decimal
	DrawdownAbs   decimal.Decimal
	HighWater     decimal.Decimal
	CurrentEquity decimal.Decimal
}

// CheckDailyDrawdown evaluates how far currentEquity has fallen from the
// daily high-water mark. It returns an error when the drawdown reaches the
// soft halt threshold or worse, along with the detailed result.
func CheckDailyDrawdown(currentEquity, dailyHighWater decimal.Decimal, cfg *Config) (DailyDrawdownResult, error) {
	res := DailyDrawdownResult{
		HighWater:     dailyHighWater,
		CurrentEquity: currentEquity,
	}

	if dailyHighWater.IsZero() {
		// No high-water mark established yet.
		return res, nil
	}

	res.DrawdownAbs = dailyHighWater.Sub(currentEquity)
	if res.DrawdownAbs.LessThanOrEqual(decimal.Zero) {
		// Equity is at or above the high-water mark -- no drawdown.
		res.DrawdownAbs = decimal.Zero
		res.DrawdownPct = decimal.Zero
		res.Level = DrawdownNormal
		return res, nil
	}

	res.DrawdownPct = res.DrawdownAbs.Div(dailyHighWater).Mul(hundred)

	switch {
	case res.DrawdownPct.GreaterThanOrEqual(cfg.DailyDrawdownBreachPct):
		res.Level = DrawdownBreach
		return res, fmt.Errorf(
			"DAILY DRAWDOWN BREACH: %.2f%% (limit %.2f%%), equity %s from high-water %s",
			res.DrawdownPct.InexactFloat64(), cfg.DailyDrawdownBreachPct.InexactFloat64(),
			currentEquity.StringFixed(2), dailyHighWater.StringFixed(2),
		)
	case res.DrawdownPct.GreaterThanOrEqual(cfg.DailyDrawdownHardPct):
		res.Level = DrawdownHard
		return res, fmt.Errorf(
			"DAILY DRAWDOWN HARD HALT: %.2f%% (limit %.2f%%), equity %s from high-water %s",
			res.DrawdownPct.InexactFloat64(), cfg.DailyDrawdownHardPct.InexactFloat64(),
			currentEquity.StringFixed(2), dailyHighWater.StringFixed(2),
		)
	case res.DrawdownPct.GreaterThanOrEqual(cfg.DailyDrawdownSoftPct):
		res.Level = DrawdownSoft
		return res, fmt.Errorf(
			"DAILY DRAWDOWN SOFT HALT: %.2f%% (limit %.2f%%), equity %s from high-water %s",
			res.DrawdownPct.InexactFloat64(), cfg.DailyDrawdownSoftPct.InexactFloat64(),
			currentEquity.StringFixed(2), dailyHighWater.StringFixed(2),
		)
	default:
		res.Level = DrawdownNormal
		return res, nil
	}
}

// ---------------------------------------------------------------------------
// Overall drawdown
// ---------------------------------------------------------------------------

// OverallDrawdownResult contains the outcome of an overall drawdown check.
type OverallDrawdownResult struct {
	Level          DrawdownLevel
	DrawdownPct    decimal.Decimal
	DrawdownAbs    decimal.Decimal
	InitialBalance decimal.Decimal
	CurrentEquity  decimal.Decimal
}

// CheckOverallDrawdown evaluates how far currentEquity has fallen from the
// initial account balance. It returns an error when the drawdown reaches the
// soft halt threshold or worse.
func CheckOverallDrawdown(currentEquity, initialBalance decimal.Decimal, cfg *Config) (OverallDrawdownResult, error) {
	res := OverallDrawdownResult{
		InitialBalance: initialBalance,
		CurrentEquity:  currentEquity,
	}

	res.DrawdownAbs = initialBalance.Sub(currentEquity)
	if res.DrawdownAbs.LessThanOrEqual(decimal.Zero) {
		res.DrawdownAbs = decimal.Zero
		res.DrawdownPct = decimal.Zero
		res.Level = DrawdownNormal
		return res, nil
	}

	res.DrawdownPct = res.DrawdownAbs.Div(initialBalance).Mul(hundred)

	switch {
	case res.DrawdownPct.GreaterThanOrEqual(cfg.OverallDrawdownBreachPct):
		res.Level = DrawdownBreach
		return res, fmt.Errorf(
			"OVERALL DRAWDOWN BREACH: %.2f%% (limit %.2f%%), equity %s from initial %s",
			res.DrawdownPct.InexactFloat64(), cfg.OverallDrawdownBreachPct.InexactFloat64(),
			currentEquity.StringFixed(2), initialBalance.StringFixed(2),
		)
	case res.DrawdownPct.GreaterThanOrEqual(cfg.OverallDrawdownHardPct):
		res.Level = DrawdownHard
		return res, fmt.Errorf(
			"OVERALL DRAWDOWN HARD HALT: %.2f%% (limit %.2f%%), equity %s from initial %s",
			res.DrawdownPct.InexactFloat64(), cfg.OverallDrawdownHardPct.InexactFloat64(),
			currentEquity.StringFixed(2), initialBalance.StringFixed(2),
		)
	case res.DrawdownPct.GreaterThanOrEqual(cfg.OverallDrawdownSoftPct):
		res.Level = DrawdownSoft
		return res, fmt.Errorf(
			"OVERALL DRAWDOWN SOFT HALT: %.2f%% (limit %.2f%%), equity %s from initial %s",
			res.DrawdownPct.InexactFloat64(), cfg.OverallDrawdownSoftPct.InexactFloat64(),
			currentEquity.StringFixed(2), initialBalance.StringFixed(2),
		)
	default:
		res.Level = DrawdownNormal
		return res, nil
	}
}

// ---------------------------------------------------------------------------
// Per-trade risk
// ---------------------------------------------------------------------------

// CheckPerTradeRisk verifies that the risk on a single trade (stop-loss
// distance multiplied by position size) does not exceed the configured
// maximum risk per trade.
//
// stopLossDistance is the absolute price distance from entry to stop-loss.
// positionSize is the quantity of the position (in base units).
// initialBalance is used to compute the dollar risk cap.
//
// Returns nil when the trade is within limits, or an error describing the
// violation.
func CheckPerTradeRisk(stopLossDistance, positionSize, initialBalance decimal.Decimal, cfg *Config) error {
	if stopLossDistance.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("stop-loss distance must be positive, got %s", stopLossDistance)
	}
	if positionSize.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("position size must be positive, got %s", positionSize)
	}

	risk := stopLossDistance.Mul(positionSize)
	maxRisk := initialBalance.Mul(cfg.MaxRiskPerTradePct).Div(hundred)

	if risk.GreaterThan(maxRisk) {
		return fmt.Errorf(
			"per-trade risk %s USD exceeds max %s USD (%.2f%% of %s); "+
				"stop_loss_distance=%s, position_size=%s",
			risk.StringFixed(2), maxRisk.StringFixed(2),
			cfg.MaxRiskPerTradePct.InexactFloat64(), initialBalance.StringFixed(2),
			stopLossDistance.StringFixed(6), positionSize.StringFixed(6),
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Profit distribution (Phase 1 & Phase 2 only)
// ---------------------------------------------------------------------------

// CheckProfitDistribution verifies that a single closed trade's PnL does not
// account for more than the configured maximum percentage of total accumulated
// profit. This rule only applies during phase1 and phase2.
//
// tradePnL is the PnL of the trade being evaluated (must be positive for
// this rule to apply). totalProfit is the sum of all previously realised
// positive PnL.
//
// Returns nil if the rule does not apply (funded mode) or if the trade is
// within limits.
func CheckProfitDistribution(tradePnL, totalProfit decimal.Decimal, cfg *Config) error {
	// Rule only applies during challenge phases.
	if cfg.Mode == ModeFunded {
		return nil
	}

	// Rule only applies to profitable trades.
	if tradePnL.LessThanOrEqual(decimal.Zero) {
		return nil
	}

	// The denominator is total profit AFTER including this trade.
	newTotal := totalProfit.Add(tradePnL)
	if newTotal.LessThanOrEqual(decimal.Zero) {
		return nil
	}

	pct := tradePnL.Div(newTotal).Mul(hundred)
	if pct.GreaterThan(cfg.ProfitDistributionMaxPct) {
		return fmt.Errorf(
			"profit distribution violation: trade PnL %s is %.2f%% of total profit %s "+
				"(max %.2f%%)",
			tradePnL.StringFixed(2), pct.InexactFloat64(),
			newTotal.StringFixed(2), cfg.ProfitDistributionMaxPct.InexactFloat64(),
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Funded-mode exposure limits
// ---------------------------------------------------------------------------

// CheckMaxExposure verifies that total margin across all open positions does
// not exceed the configured percentage of initial balance. This rule only
// applies in funded mode.
//
// totalMargin is the sum of margin used across all open positions.
func CheckMaxExposure(totalMargin, initialBalance decimal.Decimal, cfg *Config) error {
	if cfg.Mode != ModeFunded {
		return nil
	}

	maxMargin := initialBalance.Mul(cfg.FundedMaxMarginExposurePct).Div(hundred)
	if totalMargin.GreaterThan(maxMargin) {
		return fmt.Errorf(
			"funded exposure violation: total margin %s exceeds max %s (%.2f%% of %s)",
			totalMargin.StringFixed(2), maxMargin.StringFixed(2),
			cfg.FundedMaxMarginExposurePct.InexactFloat64(),
			initialBalance.StringFixed(2),
		)
	}
	return nil
}

// CheckMaxNotional verifies that the cumulative open position notional value
// does not exceed the configured multiplier of the current account balance.
// This rule only applies in funded mode.
//
// totalNotional is the sum of notional values across all open positions.
// balance is the current account balance (not initial -- this rule uses
// the live balance).
func CheckMaxNotional(totalNotional, balance decimal.Decimal, cfg *Config) error {
	if cfg.Mode != ModeFunded {
		return nil
	}

	maxNotional := balance.Mul(cfg.FundedMaxNotionalMultiplier)
	if totalNotional.GreaterThan(maxNotional) {
		return fmt.Errorf(
			"funded notional violation: total notional %s exceeds max %s (%.2fx of balance %s)",
			totalNotional.StringFixed(2), maxNotional.StringFixed(2),
			cfg.FundedMaxNotionalMultiplier.InexactFloat64(),
			balance.StringFixed(2),
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stop-loss enforcement
// ---------------------------------------------------------------------------

// StopLossDeadlineInfo carries the deadline by which a stop-loss must be
// attached to a position, along with the default stop-loss percentage if
// no explicit stop is provided.
type StopLossDeadlineInfo struct {
	Deadline       time.Time
	DefaultStopPct decimal.Decimal
}

// CheckStopLossRequired determines the deadline by which a stop-loss must be
// attached to a position. The caller is responsible for tracking whether the
// stop-loss was actually set in time.
//
// positionOpenTime is the timestamp when the position was opened.
func CheckStopLossRequired(positionOpenTime time.Time, cfg *Config) StopLossDeadlineInfo {
	return StopLossDeadlineInfo{
		Deadline:       positionOpenTime.Add(cfg.StopLossDeadline()),
		DefaultStopPct: cfg.StopLossDefaultPct,
	}
}
