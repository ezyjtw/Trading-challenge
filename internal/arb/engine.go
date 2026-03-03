package arb

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// spreadSample records a single spread observation for rolling statistics.
type spreadSample struct {
	spread float64
	tsMs   int64
}

// rollingStats holds rolling mean and standard deviation over a window.
type rollingStats struct {
	mean   float64
	stddev float64
	count  int
}

// basisPosition tracks an open basis trade.
type basisPosition struct {
	Symbol      string
	NotionalUSD float64
	EntrySpread float64
	EntryTime   int64 // unix ms
	EntryMean   float64
	EntryStdDev float64
}

// symbolState holds per-symbol state for the basis engine.
type symbolState struct {
	spotPrice  float64
	perpPrice  float64
	lastTsMs   int64
	samples    []spreadSample
	position   *basisPosition
	cooldownMs int64 // unix ms when cooldown expires
}

// Engine is the intra-Bybit basis trading engine.
// Strategy: spot-perp spread capture on the same exchange.
// Entry when spread > 2 stddev from 24h mean AND annualized basis > 15%.
// Exit when spread returns within 0.5 stddev, or time-based (> 7 days).
type Engine struct {
	mu    sync.Mutex
	cfg   BasisConfig
	state map[string]*symbolState
	seqID int
}

// NewEngine creates a new basis trading engine with the given config.
func NewEngine(cfg BasisConfig) *Engine {
	state := make(map[string]*symbolState, len(cfg.Symbols))
	for _, sym := range cfg.Symbols {
		state[sym] = &symbolState{
			samples: make([]spreadSample, 0, 10800), // ~24h at 8s intervals
		}
	}
	return &Engine{
		cfg:   cfg,
		state: state,
	}
}

// UpdatePrices ingests new spot and perp prices for a symbol and records
// the spread sample for rolling statistics.
func (e *Engine) UpdatePrices(symbol string, spotPrice, perpPrice float64, tsMs int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ss, ok := e.state[symbol]
	if !ok {
		ss = &symbolState{
			samples: make([]spreadSample, 0, 10800),
		}
		e.state[symbol] = ss
	}

	ss.spotPrice = spotPrice
	ss.perpPrice = perpPrice
	ss.lastTsMs = tsMs

	// Calculate spread: (perp - spot) / spot.
	if spotPrice > 0 {
		spread := (perpPrice - spotPrice) / spotPrice
		ss.samples = append(ss.samples, spreadSample{
			spread: spread,
			tsMs:   tsMs,
		})
		// Prune samples older than 25h to keep memory bounded.
		cutoff := tsMs - 25*60*60*1000
		pruneIdx := 0
		for pruneIdx < len(ss.samples) && ss.samples[pruneIdx].tsMs < cutoff {
			pruneIdx++
		}
		if pruneIdx > 0 {
			ss.samples = ss.samples[pruneIdx:]
		}
	}
}

// Evaluate checks all symbols for basis entry and exit opportunities
// and returns TradeIntents.
func (e *Engine) Evaluate(accountEquity float64) []TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for _, sym := range e.cfg.Symbols {
		ss, ok := e.state[sym]
		if !ok || ss.spotPrice <= 0 || ss.perpPrice <= 0 {
			continue
		}

		currentSpread := (ss.perpPrice - ss.spotPrice) / ss.spotPrice

		// Compute rolling stats over 1h, 4h, 24h windows.
		stats1h := computeRollingStats(ss.samples, now, 1*60*60*1000)
		stats4h := computeRollingStats(ss.samples, now, 4*60*60*1000)
		stats24h := computeRollingStats(ss.samples, now, 24*60*60*1000)

		// Check for exit conditions first (if we have a position).
		if ss.position != nil {
			exitIntents := e.evaluateExit(sym, ss, currentSpread, stats24h, now)
			intents = append(intents, exitIntents...)
			continue // don't evaluate entry while in position
		}

		// Entry evaluation requires sufficient data.
		if stats24h.count < 100 || stats24h.stddev <= 0 {
			continue
		}

		// Skip if on cooldown.
		if now < ss.cooldownMs {
			continue
		}

		// Entry condition 1: spread > 2 stddev from 24h mean.
		deviation := (currentSpread - stats24h.mean) / stats24h.stddev
		if deviation < e.cfg.EntryStdDev {
			continue
		}

		// Entry condition 2: annualized basis > 15%.
		// Annualized basis = spread * (365 / holdDays) * 100.
		// For a conservative estimate, use the current spread annualized over 7 days.
		annualizedBasisPct := math.Abs(currentSpread) * (365.0 / float64(e.cfg.MaxHoldDays)) * 100.0
		if annualizedBasisPct < e.cfg.MinAnnualizedBasisPct {
			continue
		}

		// Position sizing: max allocation percentage of account.
		notional := accountEquity * (e.cfg.MaxAllocationPct / 100.0)
		if notional < 10.0 {
			continue
		}

		// Estimate fees for both legs (entry + exit = 4 legs total, but intent is just entry).
		feesEst := notional * 2.0 * (e.cfg.FeeBpsTaker / 10000.0)
		slippageEst := notional * 2.0 * (e.cfg.MaxSlippageBps / 10000.0)

		edgeBpsGross := currentSpread * 10000.0
		totalCostBps := (feesEst + slippageEst) / notional * 10000.0
		edgeBpsNet := edgeBpsGross - totalCostBps

		e.seqID++
		intentID := fmt.Sprintf("basis-%s-%d-%d", sym, now, e.seqID)

		// Perp at premium: BUY SPOT + SELL PERP to capture convergence.
		intent := TradeIntent{
			IntentID:  intentID,
			Strategy:  "basis",
			Symbol:    sym,
			TsMs:      now,
			ExpiresMs: now + e.cfg.IntentTTLMs,
			Legs: []TradeLeg{
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
			Expected: ExpectedMetrics{
				EdgeBpsGross:      edgeBpsGross,
				EdgeBpsNet:        edgeBpsNet,
				ProfitUSDNet:      notional * (edgeBpsNet / 10000.0),
				FeesUSDEst:        feesEst,
				SlippageUSDEst:    slippageEst,
				AnnualYieldPctNet: annualizedBasisPct,
			},
			Constraints: IntentConstraints{
				MaxAgeMs:        e.cfg.IntentTTLMs,
				HedgePreference: "SEQUENTIAL",
				CooldownKey:     fmt.Sprintf("basis:%s", sym),
			},
		}
		intents = append(intents, intent)

		// Log stats for debugging (consumed by caller).
		_ = stats1h
		_ = stats4h
	}

	return intents
}

// evaluateExit checks if an open basis position should be closed.
func (e *Engine) evaluateExit(sym string, ss *symbolState, currentSpread float64, stats24h rollingStats, now int64) []TradeIntent {
	pos := ss.position
	if pos == nil {
		return nil
	}

	shouldExit := false
	reason := ""

	// Condition 1: spread has returned within 0.5 stddev of mean.
	if stats24h.stddev > 0 {
		deviation := math.Abs(currentSpread-stats24h.mean) / stats24h.stddev
		if deviation <= e.cfg.ExitStdDev {
			shouldExit = true
			reason = "spread_mean_reversion"
		}
	}

	// Condition 2: time-based exit after MaxHoldDays.
	holdMs := now - pos.EntryTime
	maxHoldMs := int64(e.cfg.MaxHoldDays) * 24 * 60 * 60 * 1000
	if holdMs >= maxHoldMs {
		shouldExit = true
		reason = "max_hold_time_exceeded"
	}

	if !shouldExit {
		return nil
	}

	e.seqID++
	intentID := fmt.Sprintf("basis-exit-%s-%d-%d", sym, now, e.seqID)

	// Reverse of entry: SELL SPOT + BUY PERP.
	intent := TradeIntent{
		IntentID:  intentID,
		Strategy:  "basis_exit",
		Symbol:    sym,
		TsMs:      now,
		ExpiresMs: now + e.cfg.IntentTTLMs,
		Legs: []TradeLeg{
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
		Expected: ExpectedMetrics{
			EdgeBpsGross: (pos.EntrySpread - currentSpread) * 10000.0,
			EdgeBpsNet:   0, // to be computed after fees
		},
		Constraints: IntentConstraints{
			MaxAgeMs:        e.cfg.IntentTTLMs,
			HedgePreference: "SEQUENTIAL",
			CooldownKey:     fmt.Sprintf("basis-exit:%s", sym),
		},
	}

	_ = reason // used for logging in production

	return []TradeIntent{intent}
}

// ConfirmEntry records that a basis position has been opened.
func (e *Engine) ConfirmEntry(sym string, notionalUSD, entrySpread float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	ss, ok := e.state[sym]
	if !ok {
		return
	}

	stats24h := computeRollingStats(ss.samples, now, 24*60*60*1000)

	ss.position = &basisPosition{
		Symbol:      sym,
		NotionalUSD: notionalUSD,
		EntrySpread: entrySpread,
		EntryTime:   now,
		EntryMean:   stats24h.mean,
		EntryStdDev: stats24h.stddev,
	}
	ss.cooldownMs = now + int64(e.cfg.CooldownS)*1000
}

// ConfirmExit removes a basis position after it has been closed.
func (e *Engine) ConfirmExit(sym string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ss, ok := e.state[sym]
	if !ok {
		return
	}
	ss.position = nil
	ss.cooldownMs = time.Now().UnixMilli() + int64(e.cfg.CooldownS)*1000
}

// OpenPositions returns a snapshot of all currently open basis positions.
func (e *Engine) OpenPositions() map[string]basisPosition {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make(map[string]basisPosition)
	for sym, ss := range e.state {
		if ss.position != nil {
			out[sym] = *ss.position
		}
	}
	return out
}

// TotalNotional returns the total notional USD across all open basis positions.
func (e *Engine) TotalNotional() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	var total float64
	for _, ss := range e.state {
		if ss.position != nil {
			total += ss.position.NotionalUSD
		}
	}
	return total
}

// SpreadStats returns rolling spread statistics for a symbol at the
// 1h, 4h, and 24h windows. Returns zeros if insufficient data.
func (e *Engine) SpreadStats(symbol string) (stats1h, stats4h, stats24h rollingStats) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ss, ok := e.state[symbol]
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	stats1h = computeRollingStats(ss.samples, now, 1*60*60*1000)
	stats4h = computeRollingStats(ss.samples, now, 4*60*60*1000)
	stats24h = computeRollingStats(ss.samples, now, 24*60*60*1000)
	return
}

// computeRollingStats computes mean and standard deviation for spread samples
// within the given window (in milliseconds from now).
func computeRollingStats(samples []spreadSample, nowMs int64, windowMs int64) rollingStats {
	cutoff := nowMs - windowMs

	var sum float64
	var count int
	for _, s := range samples {
		if s.tsMs >= cutoff {
			sum += s.spread
			count++
		}
	}
	if count < 2 {
		return rollingStats{count: count}
	}

	mean := sum / float64(count)

	var varianceSum float64
	for _, s := range samples {
		if s.tsMs >= cutoff {
			diff := s.spread - mean
			varianceSum += diff * diff
		}
	}
	stddev := math.Sqrt(varianceSum / float64(count-1))

	return rollingStats{
		mean:   mean,
		stddev: stddev,
		count:  count,
	}
}
