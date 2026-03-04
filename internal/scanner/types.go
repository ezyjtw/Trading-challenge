// Package scanner implements confluence-based trade setup detection.
// It monitors market data for high-probability directional setups and emits
// alerts with pre-calculated risk parameters (entry, SL, TP, position size).
//
// The scanner does NOT execute trades — it produces SignalAlert messages that
// are published to the event bus for human review or downstream automation.
package scanner

import (
	"fmt"
	"time"
)

// Direction represents the trade bias.
type Direction int

const (
	Long Direction = iota
	Short
)

func (d Direction) String() string {
	if d == Long {
		return "LONG"
	}
	return "SHORT"
}

// Timeframe represents a candle interval.
type Timeframe string

const (
	TF1m  Timeframe = "1"
	TF3m  Timeframe = "3"
	TF5m  Timeframe = "5"
	TF15m Timeframe = "15"
	TF30m Timeframe = "30"
	TF1h  Timeframe = "60"
	TF4h  Timeframe = "240"
	TF1d  Timeframe = "D"
)

// Candle represents an OHLCV bar.
type Candle struct {
	OpenTime  int64   `json:"open_time"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
	Turnover  float64 `json:"turnover"` // quote volume (USDT)
	CloseTime int64   `json:"close_time"`
}

// MidPrice returns the midpoint of the candle.
func (c Candle) MidPrice() float64 {
	return (c.High + c.Low) / 2
}

// Range returns the high-low range.
func (c Candle) Range() float64 {
	return c.High - c.Low
}

// IsBullish returns true if close > open.
func (c Candle) IsBullish() bool {
	return c.Close > c.Open
}

// ConfluenceFactor identifies which detector produced a signal.
type ConfluenceFactor string

const (
	FactorFundingExtreme   ConfluenceFactor = "FUNDING_EXTREME"
	FactorOIAbsorption     ConfluenceFactor = "OI_ABSORPTION"
	FactorRangeVWAPRevert  ConfluenceFactor = "RANGE_VWAP_REVERT"
	FactorLiqSweepReclaim  ConfluenceFactor = "LIQ_SWEEP_RECLAIM"
	FactorTrendPullback    ConfluenceFactor = "TREND_PULLBACK"
	FactorMultiTFAlignment ConfluenceFactor = "MULTI_TF_ALIGNMENT"
)

// Signal represents a single confluence factor firing.
type Signal struct {
	Factor    ConfluenceFactor `json:"factor"`
	Direction Direction        `json:"direction"`
	Strength  float64          `json:"strength"` // 0.0–1.0 confidence
	Detail    string           `json:"detail"`   // human-readable explanation
	TsMs      int64            `json:"ts_ms"`
}

// TradeOutcome records what happened after a signal was acted on.
type TradeOutcome string

const (
	OutcomePending   TradeOutcome = "PENDING"
	OutcomeTPHit     TradeOutcome = "TP_HIT"
	OutcomeSLHit     TradeOutcome = "SL_HIT"
	OutcomeManual    TradeOutcome = "MANUAL_CLOSE"
	OutcomeExpired   TradeOutcome = "EXPIRED"
	OutcomeCancelled TradeOutcome = "CANCELLED"
)

// RiskParams contains the pre-calculated risk management parameters for a trade.
type RiskParams struct {
	Entry           float64 `json:"entry"`
	StopLoss        float64 `json:"stop_loss"`
	TakeProfit1     float64 `json:"take_profit_1"`     // 1R target
	TakeProfit2     float64 `json:"take_profit_2"`     // 2R target
	TakeProfit3     float64 `json:"take_profit_3"`     // 3R target (runner)
	PositionSizeUSD float64 `json:"position_size_usd"` // notional size
	PositionSizeQty float64 `json:"position_size_qty"` // base asset quantity
	RiskUSD         float64 `json:"risk_usd"`          // $ at risk if SL hit
	RiskPct         float64 `json:"risk_pct"`           // % of account at risk
	RiskRewardRatio float64 `json:"risk_reward_ratio"`  // using TP1
	Leverage        float64 `json:"leverage"`
	SLDistancePct   float64 `json:"sl_distance_pct"`   // distance to SL as % of entry
}

// SignalAlert is the top-level alert emitted when confluences align.
// This is what gets sent to the trader (via Telegram/Discord/etc).
type SignalAlert struct {
	ID              string           `json:"id"`
	Symbol          string           `json:"symbol"`
	Direction       Direction        `json:"direction"`
	Signals         []Signal         `json:"signals"`          // individual factors
	ConfluenceScore int              `json:"confluence_score"` // count of aligned factors
	Risk            RiskParams       `json:"risk"`
	Timeframe       Timeframe        `json:"timeframe"`   // primary analysis TF
	ExpiresAt       time.Time        `json:"expires_at"`  // signal validity window
	CreatedAt       time.Time        `json:"created_at"`
	Outcome         TradeOutcome     `json:"outcome"`
	OutcomeDetail   string           `json:"outcome_detail,omitempty"`
	OutcomePnL      float64          `json:"outcome_pnl,omitempty"`
	OutcomeAt       *time.Time       `json:"outcome_at,omitempty"`
}

// MeetsMinConfluence returns true if the alert has enough aligned factors.
func (a *SignalAlert) MeetsMinConfluence(min int) bool {
	return a.ConfluenceScore >= min
}

// QualifiesForTradingDay checks whether this trade, if taken, would satisfy
// HyroTrader's minimum trading day requirements:
// 1. Position size >= 5% of initial balance
// 2. P&L potential >= ±1% of trade value
func (a *SignalAlert) QualifiesForTradingDay(initialBalance float64) (bool, string) {
	minSize := initialBalance * 0.05
	if a.Risk.PositionSizeUSD < minSize {
		return false, fmt.Sprintf("size $%.0f < min $%.0f (5%% of $%.0f)",
			a.Risk.PositionSizeUSD, minSize, initialBalance)
	}

	// TP1 must be >= 1% of trade value for the day to count.
	tp1PnL := a.Risk.PositionSizeUSD * a.Risk.RiskRewardRatio * a.Risk.SLDistancePct / 100
	minPnL := a.Risk.PositionSizeUSD * 0.01
	if tp1PnL < minPnL {
		return false, fmt.Sprintf("potential PnL $%.2f < 1%% of trade value $%.2f",
			tp1PnL, minPnL)
	}

	return true, "qualifies"
}

// OpenInterestSnapshot holds a time series point of OI data.
type OpenInterestSnapshot struct {
	Symbol       string  `json:"symbol"`
	OpenInterest float64 `json:"open_interest"`
	TsMs         int64   `json:"ts_ms"`
}

// FundingSnapshot holds a time series point of funding rate data.
type FundingSnapshot struct {
	Symbol      string  `json:"symbol"`
	FundingRate float64 `json:"funding_rate"`
	TsMs        int64   `json:"ts_ms"`
}

// MarketState aggregates all data a detector needs to evaluate.
type MarketState struct {
	Symbol string

	// Current price data.
	Price     float64
	BestBid   float64
	BestAsk   float64
	MarkPrice float64
	Volume24h float64
	TsMs      int64

	// Candle history by timeframe.
	Candles map[Timeframe][]Candle

	// Funding rate history (most recent first).
	FundingHistory []FundingSnapshot

	// Open interest history (most recent first).
	OIHistory []OpenInterestSnapshot

	// Current funding rate.
	FundingRate float64
}

// Detector is the interface all confluence detectors implement.
type Detector interface {
	// Name returns the detector's identifier.
	Name() ConfluenceFactor

	// Detect evaluates market state and returns signals (0 or more).
	// A detector may return signals for both long and short if conditions
	// are ambiguous, but typically returns 0 or 1.
	Detect(state *MarketState) []Signal
}
