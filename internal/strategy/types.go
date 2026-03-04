// Package strategy provides autonomous delta-neutral strategy selection.
// It evaluates market conditions each cycle and picks the best strategy:
// funding carry when rates are positive, basis mean-reversion when spreads
// are extended, or conservative hold when nothing is attractive.
package strategy

import (
	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// StrategyType classifies the active trading approach.
type StrategyType string

const (
	// StrategyFundingCarry collects funding payments: long spot + short perp.
	StrategyFundingCarry StrategyType = "FUNDING_CARRY"
	// StrategyBasisReversion trades spot-perp basis convergence.
	StrategyBasisReversion StrategyType = "BASIS_REVERSION"
	// StrategyHold does nothing (safety trades only to count trading days).
	StrategyHold StrategyType = "HOLD"
)

// MarketCondition is a snapshot of one symbol's market state for strategy evaluation.
type MarketCondition struct {
	Symbol      string  `json:"symbol"`
	SpotPrice   float64 `json:"spot_price"`
	PerpPrice   float64 `json:"perp_price"`
	FundingRate float64 `json:"funding_rate"` // per-8h rate
	Basis       float64 `json:"basis"`        // (perp - spot) / spot
	BasisZScore float64 `json:"basis_z_score"`
	BasisMean   float64 `json:"basis_mean"`
	BasisStdDev float64 `json:"basis_std_dev"`
	Regime      string  `json:"regime"` // from RegimeDetector
	TsMs        int64   `json:"ts_ms"`
}

// Decision is the selector's recommendation for a single symbol.
type Decision struct {
	Symbol              string       `json:"symbol"`
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
	Symbol      string       `json:"symbol"`
	Strategy    StrategyType `json:"strategy"`
	NotionalUSD float64      `json:"notional_usd"`
	EntryBasis  float64      `json:"entry_basis,omitempty"`  // for basis trades
	EntryRate   float64      `json:"entry_rate,omitempty"`   // for funding carry
	EntryTimeMs int64        `json:"entry_time_ms"`
}
