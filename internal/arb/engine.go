package arb

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// spreadSample records a single cross-pair spread observation.
type spreadSample struct {
	spread float64 // log(primaryPrice / hedgePrice)
	tsMs   int64
}

// rollingStats holds rolling mean and standard deviation over a window.
type rollingStats struct {
	mean   float64
	stddev float64
	count  int
}

// basisPosition tracks an open cross-pair spread trade.
type basisPosition struct {
	PairID        string
	PrimarySymbol string
	HedgeSymbol   string
	NotionalUSD   float64 // primary leg notional
	HedgeBeta     float64
	EntrySpread   float64
	EntryTime     int64 // unix ms
	EntryMean     float64
	EntryStdDev   float64
}

// pairState holds per-pair state for the cross-pair spread engine.
type pairState struct {
	primaryPrice float64
	hedgePrice   float64
	lastTsMs     int64
	samples      []spreadSample
	position     *basisPosition
	cooldownMs   int64   // unix ms when cooldown expires
	beta         float64 // current beta estimate
}

// Engine is the cross-pair spread trading engine.
// Strategy: trade the log price ratio between correlated perps.
// Entry when spread > 2 stddev from 24h mean.
// Exit when spread returns within 0.5 stddev, or time-based (> 7 days).
type Engine struct {
	mu    sync.Mutex
	cfg   BasisConfig
	state map[string]*pairState // pairID -> state
	seqID int
}

// NewEngine creates a new cross-pair spread trading engine.
func NewEngine(cfg BasisConfig) *Engine {
	state := make(map[string]*pairState, len(cfg.Pairs))
	for _, pair := range cfg.Pairs {
		pid := pair.Primary + ":" + pair.Hedge
		state[pid] = &pairState{
			samples: make([]spreadSample, 0, 10800),
			beta:    1.0,
		}
	}
	return &Engine{
		cfg:   cfg,
		state: state,
	}
}

// UpdatePrices ingests new perp prices for both assets in a pair and records
// the log price ratio for rolling statistics.
func (e *Engine) UpdatePrices(primary, hedge string, primaryPrice, hedgePrice float64, tsMs int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pid := primary + ":" + hedge
	ps, ok := e.state[pid]
	if !ok {
		ps = &pairState{
			samples: make([]spreadSample, 0, 10800),
			beta:    1.0,
		}
		e.state[pid] = ps
	}

	ps.primaryPrice = primaryPrice
	ps.hedgePrice = hedgePrice
	ps.lastTsMs = tsMs

	if primaryPrice > 0 && hedgePrice > 0 {
		spread := math.Log(primaryPrice / hedgePrice)
		ps.samples = append(ps.samples, spreadSample{
			spread: spread,
			tsMs:   tsMs,
		})
		// Prune samples older than 25h.
		cutoff := tsMs - 25*60*60*1000
		pruneIdx := 0
		for pruneIdx < len(ps.samples) && ps.samples[pruneIdx].tsMs < cutoff {
			pruneIdx++
		}
		if pruneIdx > 0 {
			ps.samples = ps.samples[pruneIdx:]
		}
	}
}

// UpdateBeta updates the current beta estimate for a pair.
func (e *Engine) UpdateBeta(primary, hedge string, beta float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pid := primary + ":" + hedge
	if ps, ok := e.state[pid]; ok {
		ps.beta = beta
	}
}

// Evaluate checks all pairs for spread entry and exit opportunities.
func (e *Engine) Evaluate(accountEquity float64) []TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for _, pair := range e.cfg.Pairs {
		pid := pair.Primary + ":" + pair.Hedge
		ps, ok := e.state[pid]
		if !ok || ps.primaryPrice <= 0 || ps.hedgePrice <= 0 {
			continue
		}

		currentSpread := math.Log(ps.primaryPrice / ps.hedgePrice)

		stats1h := computeRollingStats(ps.samples, now, 1*60*60*1000)
		stats4h := computeRollingStats(ps.samples, now, 4*60*60*1000)
		stats24h := computeRollingStats(ps.samples, now, 24*60*60*1000)

		// Check exit conditions first.
		if ps.position != nil {
			exitIntents := e.evaluateExit(ps, currentSpread, stats24h, now)
			intents = append(intents, exitIntents...)
			continue
		}

		if stats24h.count < 100 || stats24h.stddev <= 0 {
			continue
		}

		if now < ps.cooldownMs {
			continue
		}

		// Entry: spread > 2 stddev from 24h mean (either direction).
		deviation := (currentSpread - stats24h.mean) / stats24h.stddev
		absDeviation := math.Abs(deviation)
		if absDeviation < e.cfg.EntryStdDev {
			continue
		}

		annualizedBasisPct := math.Abs(currentSpread-stats24h.mean) * (365.0 / float64(e.cfg.MaxHoldDays)) * 100.0
		if annualizedBasisPct < e.cfg.MinAnnualizedBasisPct {
			continue
		}

		primaryNotional := accountEquity * (e.cfg.MaxAllocationPct / 100.0)
		if primaryNotional < 10.0 {
			continue
		}

		beta := ps.beta
		if beta <= 0 {
			beta = 1.0
		}
		hedgeNotional := primaryNotional / beta

		totalNotional := primaryNotional + hedgeNotional
		feesEst := totalNotional * (e.cfg.FeeBpsTaker / 10000.0)
		slippageEst := totalNotional * (e.cfg.MaxSlippageBps / 10000.0)

		edgeBpsGross := absDeviation * stats24h.stddev * 10000.0
		totalCostBps := (feesEst + slippageEst) / primaryNotional * 10000.0
		edgeBpsNet := edgeBpsGross - totalCostBps

		// Direction: if spread is above mean (positive z), sell primary + buy hedge.
		// If spread is below mean (negative z), buy primary + sell hedge.
		primaryAction := "SELL"
		hedgeAction := "BUY"
		if deviation < 0 {
			primaryAction = "BUY"
			hedgeAction = "SELL"
		}

		e.seqID++
		intentID := fmt.Sprintf("basis-%s-%d-%d", pid, now, e.seqID)

		intent := TradeIntent{
			IntentID:    intentID,
			Strategy:    "basis",
			Symbol:      pair.Primary,
			HedgeSymbol: pair.Hedge,
			HedgeBeta:   beta,
			TsMs:        now,
			ExpiresMs:   now + e.cfg.IntentTTLMs,
			Legs: []TradeLeg{
				{
					Action:         primaryAction,
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pair.Primary,
					NotionalUSD:    primaryNotional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
				{
					Action:         hedgeAction,
					Type:           "LIMIT",
					Market:         "PERP",
					Symbol:         pair.Hedge,
					NotionalUSD:    hedgeNotional,
					MaxSlippageBps: e.cfg.MaxSlippageBps,
				},
			},
			Expected: ExpectedMetrics{
				EdgeBpsGross:      edgeBpsGross,
				EdgeBpsNet:        edgeBpsNet,
				ProfitUSDNet:      primaryNotional * (edgeBpsNet / 10000.0),
				FeesUSDEst:        feesEst,
				SlippageUSDEst:    slippageEst,
				AnnualYieldPctNet: annualizedBasisPct,
			},
			Constraints: IntentConstraints{
				MaxAgeMs:        e.cfg.IntentTTLMs,
				HedgePreference: "SEQUENTIAL",
				CooldownKey:     fmt.Sprintf("basis:%s", pid),
			},
		}
		intents = append(intents, intent)

		_ = stats1h
		_ = stats4h
	}

	return intents
}

// evaluateExit checks if an open cross-pair spread position should be closed.
func (e *Engine) evaluateExit(ps *pairState, currentSpread float64, stats24h rollingStats, now int64) []TradeIntent {
	pos := ps.position
	if pos == nil {
		return nil
	}

	shouldExit := false
	reason := ""

	if stats24h.stddev > 0 {
		deviation := math.Abs(currentSpread-stats24h.mean) / stats24h.stddev
		if deviation <= e.cfg.ExitStdDev {
			shouldExit = true
			reason = "spread_mean_reversion"
		}
	}

	holdMs := now - pos.EntryTime
	maxHoldMs := int64(e.cfg.MaxHoldDays) * 24 * 60 * 60 * 1000
	if holdMs >= maxHoldMs {
		shouldExit = true
		reason = "max_hold_time_exceeded"
	}

	if !shouldExit {
		return nil
	}

	beta := pos.HedgeBeta
	if beta <= 0 {
		beta = 1.0
	}
	hedgeNotional := pos.NotionalUSD / beta

	// Reverse of entry.
	primaryAction := "BUY"
	hedgeAction := "SELL"
	if pos.EntrySpread < pos.EntryMean {
		primaryAction = "SELL"
		hedgeAction = "BUY"
	}

	e.seqID++
	intentID := fmt.Sprintf("basis-exit-%s-%d-%d", pos.PairID, now, e.seqID)

	intent := TradeIntent{
		IntentID:    intentID,
		Strategy:    "basis_exit",
		Symbol:      pos.PrimarySymbol,
		HedgeSymbol: pos.HedgeSymbol,
		HedgeBeta:   beta,
		TsMs:        now,
		ExpiresMs:   now + e.cfg.IntentTTLMs,
		Legs: []TradeLeg{
			{
				Action:         primaryAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         pos.PrimarySymbol,
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: e.cfg.MaxSlippageBps,
			},
			{
				Action:         hedgeAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         pos.HedgeSymbol,
				NotionalUSD:    hedgeNotional,
				MaxSlippageBps: e.cfg.MaxSlippageBps,
			},
		},
		Expected: ExpectedMetrics{
			EdgeBpsGross: (pos.EntrySpread - currentSpread) * 10000.0,
			EdgeBpsNet:   0,
		},
		Constraints: IntentConstraints{
			MaxAgeMs:        e.cfg.IntentTTLMs,
			HedgePreference: "SEQUENTIAL",
			CooldownKey:     fmt.Sprintf("basis-exit:%s", pos.PairID),
		},
	}

	_ = reason
	return []TradeIntent{intent}
}

// ConfirmEntry records that a cross-pair spread position has been opened.
func (e *Engine) ConfirmEntry(primary, hedge string, notionalUSD, beta, entrySpread float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	pid := primary + ":" + hedge
	ps, ok := e.state[pid]
	if !ok {
		return
	}

	stats24h := computeRollingStats(ps.samples, now, 24*60*60*1000)

	ps.position = &basisPosition{
		PairID:        pid,
		PrimarySymbol: primary,
		HedgeSymbol:   hedge,
		NotionalUSD:   notionalUSD,
		HedgeBeta:     beta,
		EntrySpread:   entrySpread,
		EntryTime:     now,
		EntryMean:     stats24h.mean,
		EntryStdDev:   stats24h.stddev,
	}
	ps.cooldownMs = now + int64(e.cfg.CooldownS)*1000
}

// ConfirmExit removes a cross-pair position after it has been closed.
func (e *Engine) ConfirmExit(primary, hedge string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pid := primary + ":" + hedge
	ps, ok := e.state[pid]
	if !ok {
		return
	}
	ps.position = nil
	ps.cooldownMs = time.Now().UnixMilli() + int64(e.cfg.CooldownS)*1000
}

// OpenPositions returns a snapshot of all currently open spread positions.
func (e *Engine) OpenPositions() map[string]basisPosition {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make(map[string]basisPosition)
	for pid, ps := range e.state {
		if ps.position != nil {
			out[pid] = *ps.position
		}
	}
	return out
}

// TotalNotional returns the total notional USD across all open positions
// (both primary and hedge legs combined).
func (e *Engine) TotalNotional() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	var total float64
	for _, ps := range e.state {
		if ps.position != nil {
			beta := ps.position.HedgeBeta
			if beta <= 0 {
				beta = 1.0
			}
			total += ps.position.NotionalUSD + ps.position.NotionalUSD/beta
		}
	}
	return total
}

// SpreadStats returns rolling spread statistics for a pair at the
// 1h, 4h, and 24h windows.
func (e *Engine) SpreadStats(primary, hedge string) (stats1h, stats4h, stats24h rollingStats) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pid := primary + ":" + hedge
	ps, ok := e.state[pid]
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	stats1h = computeRollingStats(ps.samples, now, 1*60*60*1000)
	stats4h = computeRollingStats(ps.samples, now, 4*60*60*1000)
	stats24h = computeRollingStats(ps.samples, now, 24*60*60*1000)
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
