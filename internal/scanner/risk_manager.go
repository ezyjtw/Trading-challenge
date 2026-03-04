package scanner

import (
	"fmt"
	"math"
)

// RiskConfig controls position sizing and SL/TP calculation.
type RiskConfig struct {
	// MaxRiskPerTradePct is the max % of account balance to risk on a single trade.
	// HyroTrader rule: stop-loss must not result in > 3% loss.
	// We default to 1.5% for safety margin.
	MaxRiskPerTradePct float64 `yaml:"max_risk_per_trade_pct"`

	// DefaultLeverage for position sizing. Applied after risk calculation.
	DefaultLeverage float64 `yaml:"default_leverage"`

	// MaxLeverage caps leverage regardless of setup quality.
	MaxLeverage float64 `yaml:"max_leverage"`

	// DefaultRR is the default risk:reward ratio for TP1.
	DefaultRR float64 `yaml:"default_rr"`

	// TP2Multiplier: TP2 = entry + (entry-SL) * TP2Multiplier.
	TP2Multiplier float64 `yaml:"tp2_multiplier"`

	// TP3Multiplier: TP3 = entry + (entry-SL) * TP3Multiplier (runner).
	TP3Multiplier float64 `yaml:"tp3_multiplier"`

	// ATRMultiplierSL: SL distance = ATR * this multiplier.
	// This ensures SL adapts to current volatility.
	ATRMultiplierSL float64 `yaml:"atr_multiplier_sl"`

	// MinSLDistancePct: minimum SL distance as % of entry price.
	// Prevents SL too close in low-vol environments (whipsaw risk).
	MinSLDistancePct float64 `yaml:"min_sl_distance_pct"`

	// MaxSLDistancePct: maximum SL distance as % of entry price.
	// Prevents SL too wide (excessive risk per trade).
	MaxSLDistancePct float64 `yaml:"max_sl_distance_pct"`

	// MinPositionSizeUSD is the minimum position size worth taking.
	MinPositionSizeUSD float64 `yaml:"min_position_size_usd"`

	// MinTradingDayTradeValuePct: minimum trade value as % of initial balance
	// to count toward trading day requirement (HyroTrader: 5%).
	MinTradingDayTradeValuePct float64 `yaml:"min_trading_day_trade_value_pct"`

	// MinTradingDayPnLPct: minimum PnL as % of trade value to count
	// toward trading day requirement (HyroTrader: 1%).
	MinTradingDayPnLPct float64 `yaml:"min_trading_day_pnl_pct"`

	// DailyMaxDrawdownPct: daily drawdown limit. We use a buffer below
	// HyroTrader's 6% to give ourselves room.
	DailyMaxDrawdownPct float64 `yaml:"daily_max_drawdown_pct"`

	// MaxConcurrentTrades limits how many directional trades can be open.
	MaxConcurrentTrades int `yaml:"max_concurrent_trades"`

	// CorrelationPenalty: reduce size if already exposed in correlated pair.
	CorrelationPenalty float64 `yaml:"correlation_penalty"`
}

// DefaultRiskConfig returns conservative defaults for a HyroTrader challenge.
// SOP-aligned: 0.5% risk, 2R TP (Model A), max 3 concurrent.
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		MaxRiskPerTradePct:         0.5,  // SOP: fixed 0.5% per trade
		DefaultLeverage:            10.0,
		MaxLeverage:                20.0,
		DefaultRR:                  2.0,  // SOP Model A: full TP at 2R, no partials
		TP2Multiplier:              3.0,  // informational only — Model A uses TP1
		TP3Multiplier:              4.0,  // informational only — Model A uses TP1
		ATRMultiplierSL:            1.5,
		MinSLDistancePct:           0.3,
		MaxSLDistancePct:           2.0,
		MinPositionSizeUSD:         50.0,
		MinTradingDayTradeValuePct: 5.0,
		MinTradingDayPnLPct:        1.0,
		DailyMaxDrawdownPct:        4.0, // buffer below HyroTrader's 6%
		MaxConcurrentTrades:        3,   // SOP: max 3 trades per day
		CorrelationPenalty:         0.5,
	}
}

// RiskManager computes position sizing, stop-loss, and take-profit levels.
type RiskManager struct {
	cfg RiskConfig
}

// NewRiskManager creates a new risk manager.
func NewRiskManager(cfg RiskConfig) *RiskManager {
	return &RiskManager{cfg: cfg}
}

// ComputeRiskParams calculates the full risk parameter set for a trade.
//
// Inputs:
//   - direction: Long or Short
//   - entryPrice: the expected entry price
//   - atr: current ATR value for the asset
//   - accountBalance: current account equity
//   - initialBalance: starting account balance (for HyroTrader rules)
//   - confluenceScore: how many factors aligned (used for confidence scaling)
//   - structureLevel: nearest structure level for SL placement (0 = use ATR)
func (rm *RiskManager) ComputeRiskParams(
	direction Direction,
	entryPrice float64,
	atr float64,
	accountBalance float64,
	initialBalance float64,
	confluenceScore int,
	structureLevel float64,
) (RiskParams, error) {

	if entryPrice <= 0 {
		return RiskParams{}, fmt.Errorf("entry price must be positive")
	}
	if accountBalance <= 0 {
		return RiskParams{}, fmt.Errorf("account balance must be positive")
	}

	// --- Step 1: Determine SL distance ---
	slDistance := rm.computeSLDistance(direction, entryPrice, atr, structureLevel)

	// --- Step 2: Compute position size from risk budget ---
	riskBudget := accountBalance * (rm.cfg.MaxRiskPerTradePct / 100.0)

	// Scale risk budget by confluence score (more confluences = more confidence).
	confidenceScale := rm.confidenceScale(confluenceScore)
	adjustedRisk := riskBudget * confidenceScale

	// Position size in base asset: risk_budget / sl_distance.
	positionQty := adjustedRisk / slDistance
	positionUSD := positionQty * entryPrice

	// Apply leverage constraint.
	leverage := rm.cfg.DefaultLeverage
	maxPositionFromLeverage := accountBalance * leverage
	if positionUSD > maxPositionFromLeverage {
		positionUSD = maxPositionFromLeverage
		positionQty = positionUSD / entryPrice
	}

	// Verify position doesn't exceed max risk even with adjusted size.
	actualRisk := positionQty * slDistance
	maxAllowedRisk := initialBalance * (3.0 / 100.0) // HyroTrader hard cap: 3%
	if actualRisk > maxAllowedRisk {
		positionQty = maxAllowedRisk / slDistance
		positionUSD = positionQty * entryPrice
		actualRisk = maxAllowedRisk
	}

	if positionUSD < rm.cfg.MinPositionSizeUSD {
		return RiskParams{}, fmt.Errorf("position size $%.2f below minimum $%.2f",
			positionUSD, rm.cfg.MinPositionSizeUSD)
	}

	// --- Step 3: Compute SL and TP prices ---
	var sl, tp1, tp2, tp3 float64
	slDistancePct := slDistance / entryPrice * 100

	if direction == Long {
		sl = entryPrice - slDistance
		tp1 = entryPrice + slDistance*rm.cfg.DefaultRR
		tp2 = entryPrice + slDistance*rm.cfg.TP2Multiplier
		tp3 = entryPrice + slDistance*rm.cfg.TP3Multiplier
	} else {
		sl = entryPrice + slDistance
		tp1 = entryPrice - slDistance*rm.cfg.DefaultRR
		tp2 = entryPrice - slDistance*rm.cfg.TP2Multiplier
		tp3 = entryPrice - slDistance*rm.cfg.TP3Multiplier
	}

	riskPct := actualRisk / accountBalance * 100

	return RiskParams{
		Entry:           entryPrice,
		StopLoss:        sl,
		TakeProfit1:     tp1,
		TakeProfit2:     tp2,
		TakeProfit3:     tp3,
		PositionSizeUSD: positionUSD,
		PositionSizeQty: positionQty,
		RiskUSD:         actualRisk,
		RiskPct:         riskPct,
		RiskRewardRatio: rm.cfg.DefaultRR,
		Leverage:        leverage,
		SLDistancePct:   slDistancePct,
	}, nil
}

// computeSLDistance determines the stop-loss distance based on ATR and structure.
func (rm *RiskManager) computeSLDistance(direction Direction, entryPrice, atr, structureLevel float64) float64 {
	// Primary: ATR-based SL.
	atrSL := atr * rm.cfg.ATRMultiplierSL

	// If a structure level is provided, use the distance to it (with buffer).
	structureSL := 0.0
	if structureLevel > 0 {
		if direction == Long {
			structureSL = entryPrice - structureLevel
		} else {
			structureSL = structureLevel - entryPrice
		}
		// Add a small buffer beyond structure (0.1% of price).
		structureSL += entryPrice * 0.001
	}

	// Use the tighter of ATR-based and structure-based, but prefer structure
	// if it's available and reasonable.
	sl := atrSL
	if structureSL > 0 && structureSL < atrSL*2 {
		sl = structureSL
	}

	// Apply min/max constraints.
	minSL := entryPrice * (rm.cfg.MinSLDistancePct / 100.0)
	maxSL := entryPrice * (rm.cfg.MaxSLDistancePct / 100.0)

	sl = math.Max(sl, minSL)
	sl = math.Min(sl, maxSL)

	return sl
}

// confidenceScale returns a multiplier (0.5–1.0) based on confluence count.
func (rm *RiskManager) confidenceScale(confluenceScore int) float64 {
	switch {
	case confluenceScore >= 5:
		return 1.0
	case confluenceScore >= 4:
		return 0.9
	case confluenceScore >= 3:
		return 0.8
	case confluenceScore >= 2:
		return 0.65
	default:
		return 0.5
	}
}

// ValidateForTradingDay checks whether a planned trade meets HyroTrader's
// minimum trading day qualification rules.
func (rm *RiskManager) ValidateForTradingDay(positionSizeUSD, initialBalance float64) (bool, string) {
	minTradeValue := initialBalance * (rm.cfg.MinTradingDayTradeValuePct / 100.0)
	if positionSizeUSD < minTradeValue {
		return false, fmt.Sprintf("trade value $%.0f < required $%.0f (%.0f%% of initial)",
			positionSizeUSD, minTradeValue, rm.cfg.MinTradingDayTradeValuePct)
	}

	// The trade must have potential to move ±1% of its own value.
	// With our TP1 at 1.5R and min SL at 0.3%, the TP1 should be at least 0.45%.
	// But the PnL needs to be ≥1% of trade value:
	// PnL = positionSize * slDist * RR / entryPrice
	// We check this at the alert level, not here, since we need the SL distance.

	return true, "qualifies"
}

// CheckDailyRiskBudget verifies we haven't used too much of the daily drawdown budget.
func (rm *RiskManager) CheckDailyRiskBudget(
	dailyPnL float64,
	accountBalance float64,
	pendingRisk float64,
) (bool, string) {
	maxDailyLoss := accountBalance * (rm.cfg.DailyMaxDrawdownPct / 100.0)
	totalExposure := math.Abs(dailyPnL) + pendingRisk

	if dailyPnL < 0 && totalExposure > maxDailyLoss {
		return false, fmt.Sprintf("daily risk exceeded: PnL $%.2f + pending $%.2f > limit $%.2f",
			dailyPnL, pendingRisk, maxDailyLoss)
	}

	remaining := maxDailyLoss - math.Abs(math.Min(dailyPnL, 0))
	if pendingRisk > remaining {
		return false, fmt.Sprintf("insufficient daily budget: $%.2f remaining, $%.2f needed",
			remaining, pendingRisk)
	}

	return true, fmt.Sprintf("daily budget: $%.2f remaining", remaining)
}

// SuggestSLType recommends market or limit SL based on conditions.
// ALWAYS returns market for prop firm challenges.
func (rm *RiskManager) SuggestSLType(isChallenge bool, volume24h float64) string {
	// For prop firm challenges: ALWAYS market SL.
	// A limit SL that doesn't fill = potential challenge breach.
	if isChallenge {
		return "MARKET"
	}

	// For funded accounts with high-volume pairs, limit SL saves on slippage.
	// But only for very liquid pairs (>$500M 24h volume).
	if volume24h > 500_000_000 {
		return "LIMIT"
	}

	return "MARKET"
}
