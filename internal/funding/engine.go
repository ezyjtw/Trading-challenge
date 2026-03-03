package funding

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// Quote is a market data snapshot ingested by the engine.
type Quote struct {
	Symbol      string  `json:"symbol"`
	SpotPrice   float64 `json:"spot_price"`
	PerpPrice   float64 `json:"perp_price"`
	FundingRate float64 `json:"funding_rate"` // per-period rate (e.g. 8h)
	TsMs        int64   `json:"ts_ms"`
}

// openPosition tracks an active funding carry position.
type openPosition struct {
	Symbol          string
	NotionalUSD     float64
	EntryRate       float64
	EntryTime       int64
	NegativeCycles  int     // consecutive cycles where rate < exit threshold
	LastFundingRate float64 // most recent observed funding rate
}

// Engine is the single-exchange funding rate arbitrage engine.
// Strategy: Long spot + Short perp on Bybit, collect funding every 8h.
type Engine struct {
	mu        sync.Mutex
	cfg       Config
	quotes    map[string]*Quote    // latest quote per symbol
	positions map[string]*openPosition
	cooldowns map[string]int64     // symbol -> unix ms when cooldown expires
	seqID     int
}

// NewEngine creates a new funding arbitrage engine with the given config.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:       cfg,
		quotes:    make(map[string]*Quote),
		positions: make(map[string]*openPosition),
		cooldowns: make(map[string]int64),
	}
}

// UpdateQuote ingests a market data quote into the engine.
func (e *Engine) UpdateQuote(q Quote) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := q
	e.quotes[q.Symbol] = &cp
}

// Evaluate checks all symbols for funding rate entry opportunities and
// returns TradeIntents for new positions.
func (e *Engine) Evaluate(accountEquity float64) []arb.TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	for _, sym := range e.cfg.Symbols {
		// Skip if already in position.
		if _, open := e.positions[sym]; open {
			continue
		}

		// Skip if on cooldown.
		if cd, ok := e.cooldowns[sym]; ok && now < cd {
			continue
		}

		q, ok := e.quotes[sym]
		if !ok || q.FundingRate <= 0 {
			continue
		}

		// Calculate annualised yield:
		// funding is collected 3x/day (every 8h), 365 days/year = 1095 periods.
		annualYieldPct := q.FundingRate * 100.0 * 1095.0
		if annualYieldPct < e.cfg.MinAnnualYieldPct {
			continue
		}

		// Position sizing based on rate magnitude.
		ratePct := q.FundingRate * 100.0 // convert to percentage
		allocPct := e.cfg.AllocationPctForRate(ratePct)

		// Apply symbol weight.
		weight := 1.0
		if w, ok := e.cfg.SymbolWeights[sym]; ok {
			weight = w
		}

		// Cap at max allocation.
		maxAlloc := accountEquity * (e.cfg.MaxAllocationPct / 100.0)
		notional := accountEquity * (allocPct / 100.0) * weight
		if notional > maxAlloc {
			notional = maxAlloc
		}
		if notional < 10.0 {
			continue // too small
		}

		// Estimate fees for both legs.
		feesEst := notional * 2.0 * (e.cfg.FeeBpsTaker / 10000.0)
		slippageEst := notional * 2.0 * (e.cfg.MaxSlippageBps / 10000.0)

		// Net edge per 8h cycle in bps.
		edgeBpsGross := q.FundingRate * 10000.0
		totalCostBps := (feesEst + slippageEst) / notional * 10000.0
		edgeBpsNet := edgeBpsGross - totalCostBps

		profitPerCycle := notional * (edgeBpsNet / 10000.0)

		e.seqID++
		intentID := fmt.Sprintf("fund-%s-%d-%d", sym, now, e.seqID)

		intent := arb.TradeIntent{
			IntentID:  intentID,
			Strategy:  "funding",
			Symbol:    sym,
			TsMs:      now,
			ExpiresMs: now + e.cfg.IntentTTLMs,
			Legs: []arb.TradeLeg{
				{
					Action:         "BUY",
					Type:           "LIMIT",
					Market:         "SPOT",
					Symbol:         sym,
					NotionalUSD:    notional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
				{
					Action:         "SELL",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         sym,
					NotionalUSD:    notional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
			},
			Expected: arb.ExpectedMetrics{
				EdgeBpsGross:      edgeBpsGross,
				EdgeBpsNet:        edgeBpsNet,
				ProfitUSDNet:      profitPerCycle,
				FeesUSDEst:        feesEst,
				SlippageUSDEst:    slippageEst,
				FundingRate8hBps:  edgeBpsGross,
				AnnualYieldPctNet: annualYieldPct - (totalCostBps/10000.0)*1095.0*100.0,
			},
			Constraints: arb.IntentConstraints{
				MaxAgeMs:        e.cfg.IntentTTLMs,
				HedgePreference: "SEQUENTIAL",
				CooldownKey:     fmt.Sprintf("funding:%s", sym),
			},
		}

		intents = append(intents, intent)
	}

	return intents
}

// ConfirmEntry records that a position has been opened for the given symbol.
func (e *Engine) ConfirmEntry(sym string, notionalUSD, entryRate float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	e.positions[sym] = &openPosition{
		Symbol:          sym,
		NotionalUSD:     notionalUSD,
		EntryRate:       entryRate,
		EntryTime:       now,
		NegativeCycles:  0,
		LastFundingRate: entryRate,
	}
	e.cooldowns[sym] = now + int64(e.cfg.CooldownS)*1000
}

// EvaluateExits checks open positions for exit conditions and returns
// TradeIntents to close positions.
// Exit when:
//   - Funding rate < ExitThresholdRate for NegativeExitCycles consecutive cycles
//   - Funding rate turns negative
func (e *Engine) EvaluateExits() []arb.TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	for sym, pos := range e.positions {
		q, ok := e.quotes[sym]
		if !ok {
			continue
		}

		shouldExit := false
		reason := ""

		// Update position with latest funding rate.
		pos.LastFundingRate = q.FundingRate

		// Check if rate turned negative -> immediate exit.
		if q.FundingRate < 0 {
			shouldExit = true
			reason = "negative_funding"
		} else if q.FundingRate < e.cfg.ExitThresholdRate {
			// Rate below threshold, increment negative cycle counter.
			pos.NegativeCycles++
			if pos.NegativeCycles >= e.cfg.NegativeExitCycles {
				shouldExit = true
				reason = fmt.Sprintf("below_threshold_%d_cycles", pos.NegativeCycles)
			}
		} else {
			// Rate recovered, reset counter.
			pos.NegativeCycles = 0
		}

		if !shouldExit {
			continue
		}

		// Generate close intent: SELL SPOT + BUY PERP (reverse of entry).
		e.seqID++
		intentID := fmt.Sprintf("fund-exit-%s-%d-%d", sym, now, e.seqID)

		intent := arb.TradeIntent{
			IntentID:  intentID,
			Strategy:  "funding_exit",
			Symbol:    sym,
			TsMs:      now,
			ExpiresMs: now + e.cfg.IntentTTLMs,
			Legs: []arb.TradeLeg{
				{
					Action:         "SELL",
					Type:           "LIMIT",
					Market:         "SPOT",
					Symbol:         sym,
					NotionalUSD:    pos.NotionalUSD,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
				{
					Action:         "BUY",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         sym,
					NotionalUSD:    pos.NotionalUSD,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
			},
			Expected: arb.ExpectedMetrics{
				EdgeBpsGross: 0,
				EdgeBpsNet:   0,
			},
			Constraints: arb.IntentConstraints{
				MaxAgeMs:        e.cfg.IntentTTLMs,
				HedgePreference: "SEQUENTIAL",
				CooldownKey:     fmt.Sprintf("funding-exit:%s", sym),
			},
		}

		intents = append(intents, intent)

		// Mark cooldown (will be cleaned up by ConfirmExit).
		e.cooldowns[sym] = now + int64(e.cfg.CooldownS)*1000

		_ = reason // used for logging in production
	}

	return intents
}

// ConfirmExit removes a position after it has been closed.
func (e *Engine) ConfirmExit(sym string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.positions, sym)
}

// OpenPositions returns a snapshot of all currently open positions.
func (e *Engine) OpenPositions() map[string]openPosition {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]openPosition, len(e.positions))
	for k, v := range e.positions {
		out[k] = *v
	}
	return out
}

// TotalNotional returns the total notional USD across all open positions.
func (e *Engine) TotalNotional() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total float64
	for _, pos := range e.positions {
		total += pos.NotionalUSD
	}
	return total
}

// AnnualYieldFromRate converts a per-8h funding rate to annualised yield percentage.
func AnnualYieldFromRate(rate8h float64) float64 {
	return math.Abs(rate8h) * 100.0 * 1095.0
}
