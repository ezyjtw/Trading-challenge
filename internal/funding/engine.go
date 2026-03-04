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
	PerpPrice   float64 `json:"perp_price"`
	FundingRate float64 `json:"funding_rate"` // per-period rate (e.g. 8h)
	TsMs        int64   `json:"ts_ms"`
}

// openPosition tracks an active cross-pair funding carry position.
type openPosition struct {
	PairID         string  // "PRIMARY:HEDGE"
	PrimarySymbol  string
	HedgeSymbol    string
	NotionalUSD    float64 // primary leg notional
	HedgeBeta      float64 // beta at entry
	EntryDiffBps   float64 // funding rate differential at entry
	EntryTime      int64
	NegativeCycles int     // consecutive cycles where diff < exit threshold
	LastDiffBps    float64 // most recent observed funding differential
}

// Engine is the cross-pair funding rate arbitrage engine.
// Strategy: Short the high-funding perp + Long the correlated hedge perp.
// Collect net funding differential every 8h.
type Engine struct {
	mu        sync.Mutex
	cfg       Config
	quotes    map[string]*Quote          // latest quote per symbol
	positions map[string]*openPosition   // pairID -> position
	cooldowns map[string]int64           // pairID -> unix ms when cooldown expires
	betas     map[string]float64         // pairID -> current beta estimate
	seqID     int
}

// NewEngine creates a new cross-pair funding arbitrage engine.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:       cfg,
		quotes:    make(map[string]*Quote),
		positions: make(map[string]*openPosition),
		cooldowns: make(map[string]int64),
		betas:     make(map[string]float64),
	}
}

// UpdateQuote ingests a market data quote into the engine.
func (e *Engine) UpdateQuote(q Quote) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := q
	e.quotes[q.Symbol] = &cp
}

// UpdateBeta updates the current beta estimate for a pair.
func (e *Engine) UpdateBeta(primary, hedge string, beta float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pid := pairID(primary, hedge)
	e.betas[pid] = beta
}

// Evaluate checks all pairs for funding rate differential entry opportunities
// and returns TradeIntents for new positions.
func (e *Engine) Evaluate(accountEquity float64) []arb.TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	for _, pair := range e.cfg.Pairs {
		pid := pairID(pair.Primary, pair.Hedge)

		// Skip if already in position.
		if _, open := e.positions[pid]; open {
			continue
		}

		// Skip if on cooldown.
		if cd, ok := e.cooldowns[pid]; ok && now < cd {
			continue
		}

		pq, pOk := e.quotes[pair.Primary]
		hq, hOk := e.quotes[pair.Hedge]
		if !pOk || !hOk {
			continue
		}

		// Funding rate differential in bps.
		// Positive = primary has higher funding (shorts get paid more on primary).
		diffBps := (pq.FundingRate - hq.FundingRate) * 10000.0
		if diffBps <= 0 {
			continue
		}

		// Annualised yield from differential.
		annualDiffPct := diffBps / 10000.0 * 100.0 * 1095.0
		if annualDiffPct < e.cfg.MinAnnualYieldPct {
			continue
		}

		allocPct := e.cfg.AllocationPctForDiff(diffBps)

		// Cap at max allocation.
		maxAlloc := accountEquity * (e.cfg.MaxAllocationPct / 100.0)
		primaryNotional := accountEquity * (allocPct / 100.0)
		if primaryNotional > maxAlloc {
			primaryNotional = maxAlloc
		}
		if primaryNotional < 10.0 {
			continue
		}

		beta := e.betas[pid]
		if beta <= 0 {
			beta = 1.0
		}
		hedgeNotional := primaryNotional / beta

		// Estimate fees for both legs (2 perp legs for entry).
		totalNotional := primaryNotional + hedgeNotional
		feesEst := totalNotional * (e.cfg.FeeBpsTaker / 10000.0)
		slippageEst := totalNotional * (e.cfg.MaxSlippageBps / 10000.0)

		totalCostBps := (feesEst + slippageEst) / primaryNotional * 10000.0
		edgeBpsNet := diffBps - totalCostBps
		profitPerCycle := primaryNotional * (edgeBpsNet / 10000.0)

		e.seqID++
		intentID := fmt.Sprintf("fund-%s-%d-%d", pid, now, e.seqID)

		intent := arb.TradeIntent{
			IntentID:    intentID,
			Strategy:    "funding",
			Symbol:      pair.Primary,
			HedgeSymbol: pair.Hedge,
			HedgeBeta:   beta,
			TsMs:        now,
			ExpiresMs:   now + e.cfg.IntentTTLMs,
			Legs: []arb.TradeLeg{
				{
					Action:         "SELL",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pair.Primary,
					NotionalUSD:    primaryNotional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
				{
					Action:         "BUY",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pair.Hedge,
					NotionalUSD:    hedgeNotional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
			},
			Expected: arb.ExpectedMetrics{
				EdgeBpsGross:      diffBps,
				EdgeBpsNet:        edgeBpsNet,
				ProfitUSDNet:      profitPerCycle,
				FeesUSDEst:        feesEst,
				SlippageUSDEst:    slippageEst,
				FundingRate8hBps:  diffBps,
				AnnualYieldPctNet: annualDiffPct - (totalCostBps/10000.0)*1095.0*100.0,
			},
			Constraints: arb.IntentConstraints{
				MaxAgeMs:        e.cfg.IntentTTLMs,
				HedgePreference: "SEQUENTIAL",
				CooldownKey:     fmt.Sprintf("funding:%s", pid),
			},
		}

		intents = append(intents, intent)
	}

	return intents
}

// ConfirmEntry records that a cross-pair position has been opened.
func (e *Engine) ConfirmEntry(primary, hedge string, notionalUSD, beta, entryDiffBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	pid := pairID(primary, hedge)
	e.positions[pid] = &openPosition{
		PairID:         pid,
		PrimarySymbol:  primary,
		HedgeSymbol:    hedge,
		NotionalUSD:    notionalUSD,
		HedgeBeta:      beta,
		EntryDiffBps:   entryDiffBps,
		EntryTime:      now,
		NegativeCycles: 0,
		LastDiffBps:    entryDiffBps,
	}
	e.cooldowns[pid] = now + int64(e.cfg.CooldownS)*1000
}

// EvaluateExits checks open positions for exit conditions and returns
// TradeIntents to close positions.
// Exit when:
//   - Funding differential < threshold for N consecutive cycles
//   - Funding differential reverses (primary no longer higher)
func (e *Engine) EvaluateExits() []arb.TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	for pid, pos := range e.positions {
		pq, pOk := e.quotes[pos.PrimarySymbol]
		hq, hOk := e.quotes[pos.HedgeSymbol]
		if !pOk || !hOk {
			continue
		}

		diffBps := (pq.FundingRate - hq.FundingRate) * 10000.0
		pos.LastDiffBps = diffBps

		shouldExit := false
		reason := ""

		if diffBps < 0 {
			shouldExit = true
			reason = "funding_diff_reversed"
		} else if diffBps < e.cfg.ExitThresholdDiffBps {
			pos.NegativeCycles++
			if pos.NegativeCycles >= e.cfg.NegativeExitCycles {
				shouldExit = true
				reason = fmt.Sprintf("below_threshold_%d_cycles", pos.NegativeCycles)
			}
		} else {
			pos.NegativeCycles = 0
		}

		if !shouldExit {
			continue
		}

		beta := pos.HedgeBeta
		if beta <= 0 {
			beta = 1.0
		}
		hedgeNotional := pos.NotionalUSD / beta

		e.seqID++
		intentID := fmt.Sprintf("fund-exit-%s-%d-%d", pid, now, e.seqID)

		// Reverse of entry: BUY primary (close short) + SELL hedge (close long).
		intent := arb.TradeIntent{
			IntentID:    intentID,
			Strategy:    "funding_exit",
			Symbol:      pos.PrimarySymbol,
			HedgeSymbol: pos.HedgeSymbol,
			HedgeBeta:   beta,
			TsMs:        now,
			ExpiresMs:   now + e.cfg.IntentTTLMs,
			Legs: []arb.TradeLeg{
				{
					Action:         "BUY",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pos.PrimarySymbol,
					NotionalUSD:    pos.NotionalUSD,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
				{
					Action:         "SELL",
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pos.HedgeSymbol,
					NotionalUSD:    hedgeNotional,
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
				CooldownKey:     fmt.Sprintf("funding-exit:%s", pid),
			},
		}

		intents = append(intents, intent)
		e.cooldowns[pid] = now + int64(e.cfg.CooldownS)*1000
		_ = reason
	}

	return intents
}

// ConfirmExit removes a position after it has been closed.
func (e *Engine) ConfirmExit(primary, hedge string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.positions, pairID(primary, hedge))
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

// TotalNotional returns the total notional USD across all open positions
// (both primary and hedge legs combined).
func (e *Engine) TotalNotional() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total float64
	for _, pos := range e.positions {
		beta := pos.HedgeBeta
		if beta <= 0 {
			beta = 1.0
		}
		total += pos.NotionalUSD + pos.NotionalUSD/beta
	}
	return total
}

// AnnualYieldFromDiffBps converts a per-8h funding rate differential (bps)
// to annualised yield percentage.
func AnnualYieldFromDiffBps(diffBps float64) float64 {
	return math.Abs(diffBps) / 10000.0 * 100.0 * 1095.0
}

func pairID(primary, hedge string) string {
	return primary + ":" + hedge
}
