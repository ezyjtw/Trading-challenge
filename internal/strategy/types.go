// Package strategy provides autonomous delta-neutral strategy selection.
// It evaluates cross-pair market conditions each cycle and picks the best
// strategy: funding carry when rate differential is attractive, spread
// mean-reversion when the price ratio deviates, or hold when nothing
// is attractive. All trades use USDT perpetual futures only (no spot).
package strategy

import (
	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// StrategyType classifies the active trading approach.
type StrategyType string

const (
	// StrategyFundingCarry collects funding payments via cross-pair delta-neutral:
	// short the high-funding perp + long a correlated hedge perp (beta-weighted).
	StrategyFundingCarry StrategyType = "FUNDING_CARRY"
	// StrategyBasisReversion trades cross-pair spread convergence when the
	// price ratio between correlated perps deviates from its rolling mean.
	StrategyBasisReversion StrategyType = "BASIS_REVERSION"
	// StrategyHold does nothing (safety trades only to count trading days).
	StrategyHold StrategyType = "HOLD"
)

// PairCondition is a snapshot of a cross-pair's market state for strategy evaluation.
// Both primary and hedge are USDT perpetual futures on correlated assets.
type PairCondition struct {
	// Primary asset (the one with the funding opportunity).
	PrimarySymbol  string  `json:"primary_symbol"`
	PrimaryPrice   float64 `json:"primary_price"`    // perp mark price
	PrimaryFunding float64 `json:"primary_funding"`   // per-8h funding rate

	// Hedge asset (correlated, for delta-neutral positioning).
	HedgeSymbol  string  `json:"hedge_symbol"`
	HedgePrice   float64 `json:"hedge_price"`     // perp mark price
	HedgeFunding float64 `json:"hedge_funding"`    // per-8h funding rate

	// Cross-pair metrics.
	FundingDiffBps float64 `json:"funding_diff_bps"` // (primary - hedge) funding in bps
	Beta           float64 `json:"beta"`              // OLS beta (hedge return per unit primary return)
	Correlation    float64 `json:"correlation"`        // Pearson correlation of returns

	// Spread statistics (log price ratio deviation from rolling mean).
	SpreadZScore float64 `json:"spread_z_score"`
	SpreadMean   float64 `json:"spread_mean"`
	SpreadStdDev float64 `json:"spread_std_dev"`

	// Regime from RegimeDetector (based on primary symbol funding).
	Regime string `json:"regime"`
	TsMs   int64  `json:"ts_ms"`
}

// MarketCondition is kept for backward compatibility and internal use.
// It wraps single-symbol data that feeds into PairCondition construction.
type MarketCondition struct {
	Symbol      string  `json:"symbol"`
	PerpPrice   float64 `json:"perp_price"`
	IndexPrice  float64 `json:"index_price"`   // exchange index price (≈ spot reference)
	FundingRate float64 `json:"funding_rate"`   // per-8h rate
	PerpPremium float64 `json:"perp_premium"`   // (perpPrice - indexPrice) / indexPrice
	BasisZScore float64 `json:"basis_z_score"`
	BasisMean   float64 `json:"basis_mean"`
	BasisStdDev float64 `json:"basis_std_dev"`
	Regime      string  `json:"regime"` // from RegimeDetector
	TsMs        int64   `json:"ts_ms"`
}

// Decision is the selector's recommendation for a trading pair.
type Decision struct {
	PrimarySymbol       string       `json:"primary_symbol"`
	HedgeSymbol         string       `json:"hedge_symbol"`
	Strategy            StrategyType `json:"strategy"`
	Confidence          float64      `json:"confidence"` // 0.0 - 1.0
	Reason              string       `json:"reason"`
	AllocationPct       float64      `json:"allocation_pct"`
	ExpectedEdgeBps     float64      `json:"expected_edge_bps"`
	ExpectedHoldingTime string       `json:"expected_holding_time"` // "8h", "1h", etc.
}

// Evaluation is the output of a strategy cycle: intents to open and close.
type Evaluation struct {
	Entries   []arb.TradeIntent `json:"entries"`
	Exits     []arb.TradeIntent `json:"exits"`
	Decisions []Decision        `json:"decisions"`
}

// PositionState tracks what the strategy engine knows about its open positions.
type PositionState struct {
	PrimarySymbol string       `json:"primary_symbol"`
	HedgeSymbol   string       `json:"hedge_symbol"`
	Strategy      StrategyType `json:"strategy"`
	NotionalUSD   float64      `json:"notional_usd"`    // primary leg notional
	HedgeBeta     float64      `json:"hedge_beta"`       // beta at entry
	EntrySpread   float64      `json:"entry_spread,omitempty"` // for basis trades
	EntryRate     float64      `json:"entry_rate,omitempty"`   // funding diff at entry
	EntryTimeMs   int64        `json:"entry_time_ms"`
}
