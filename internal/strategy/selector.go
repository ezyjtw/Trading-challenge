package strategy

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// SelectorConfig tunes the autonomous strategy selector.
type SelectorConfig struct {
	// BasisEntryZScore is the z-score threshold to enter a spread trade.
	BasisEntryZScore float64 `yaml:"basis_entry_z_score"` // e.g. 2.0
	// BasisExitZScore is the z-score threshold to exit (mean reversion target).
	BasisExitZScore float64 `yaml:"basis_exit_z_score"` // e.g. 0.5
	// MinSpreadEdgeBps is the minimum edge in bps after fees to enter a spread trade.
	MinSpreadEdgeBps float64 `yaml:"min_spread_edge_bps"` // e.g. 5.0
	// MinFundingDiffBps is the minimum funding rate differential in bps for carry.
	MinFundingDiffBps float64 `yaml:"min_funding_diff_bps"` // e.g. 5.0
	// MinFundingAnnualPct is the minimum annualized yield (%) for funding carry.
	MinFundingAnnualPct float64 `yaml:"min_funding_annual_pct"` // e.g. 5.0
	// MinCorrelation is the minimum correlation required to enter a cross-pair trade.
	MinCorrelation float64 `yaml:"min_correlation"` // e.g. 0.70
	// MaxConcurrentPositions limits how many pairs can be in position simultaneously.
	MaxConcurrentPositions int `yaml:"max_concurrent_positions"` // e.g. 2
	// MaxAllocationPct is the max % of equity per position (primary leg).
	MaxAllocationPct float64 `yaml:"max_allocation_pct"` // e.g. 30.0
	// TotalMaxAllocationPct caps total allocation across all positions.
	TotalMaxAllocationPct float64 `yaml:"total_max_allocation_pct"` // e.g. 60.0
	// FeeBpsTaker is the estimated round-trip taker fee in bps (per leg).
	FeeBpsTaker float64 `yaml:"fee_bps_taker"` // e.g. 5.5
	// MaxSlippageBps is the max slippage tolerance per leg.
	MaxSlippageBps float64 `yaml:"max_slippage_bps"` // e.g. 5.0
	// IntentTTLMs is the time-to-live for generated intents.
	IntentTTLMs int64 `yaml:"intent_ttl_ms"` // e.g. 10000
	// CooldownS is the minimum seconds between trades on the same pair.
	CooldownS int `yaml:"cooldown_s"` // e.g. 300
	// FundingExitThresholdBps is the funding diff (bps) below which we exit carry.
	FundingExitThresholdBps float64 `yaml:"funding_exit_threshold_bps"` // e.g. 0.5
	// FundingNegativeExitCycles exits after N consecutive cycles below threshold.
	FundingNegativeExitCycles int `yaml:"funding_negative_exit_cycles"` // e.g. 2
	// BasisHoldingMaxMs is the maximum time to hold a spread trade before force-exiting.
	BasisHoldingMaxMs int64 `yaml:"basis_holding_max_ms"` // e.g. 4 hours
}

// DefaultSelectorConfig returns conservative defaults for the challenge.
func DefaultSelectorConfig() SelectorConfig {
	return SelectorConfig{
		BasisEntryZScore:          2.0,
		BasisExitZScore:           0.5,
		MinSpreadEdgeBps:          5.0,
		MinFundingDiffBps:         5.0,
		MinFundingAnnualPct:       5.0,
		MinCorrelation:            0.70,
		MaxConcurrentPositions:    2,
		MaxAllocationPct:          30.0,
		TotalMaxAllocationPct:     60.0,
		FeeBpsTaker:               5.5,
		MaxSlippageBps:            5.0,
		IntentTTLMs:               10000,
		CooldownS:                 300,
		FundingExitThresholdBps:   0.5,
		FundingNegativeExitCycles: 2,
		BasisHoldingMaxMs:         4 * 60 * 60 * 1000, // 4 hours
	}
}

// openPosition tracks a live cross-pair position managed by the selector.
type openPosition struct {
	PairID         string // "PRIMARY:HEDGE"
	PrimarySymbol  string
	HedgeSymbol    string
	Strategy       StrategyType
	NotionalUSD    float64 // primary leg notional
	HedgeBeta      float64 // beta at entry
	EntrySpread    float64 // for basis trades (log price ratio)
	EntryFundDiff  float64 // for funding carry (funding diff in bps)
	EntryTimeMs    int64
	NegativeCycles int // consecutive cycles with funding diff below threshold
}

// Selector autonomously evaluates cross-pair market conditions and generates
// trade intents for delta-neutral strategies using only USDT perpetual futures.
type Selector struct {
	mu        sync.Mutex
	cfg       SelectorConfig
	positions map[string]*openPosition // pairID -> position
	cooldowns map[string]int64         // pairID -> cooldown expiry (unix ms)
	seqID     int
}

// NewSelector creates a new autonomous strategy selector.
func NewSelector(cfg SelectorConfig) *Selector {
	return &Selector{
		cfg:       cfg,
		positions: make(map[string]*openPosition),
		cooldowns: make(map[string]int64),
	}
}

// Evaluate runs a full strategy evaluation cycle over cross-pair conditions.
// Returns trade intents for new entries and exits.
func (s *Selector) Evaluate(pairs []PairCondition, accountEquity float64, regimeScale float64) Evaluation {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	scaledEquity := accountEquity * regimeScale

	var eval Evaluation

	// --- Phase 1: Evaluate exits for existing positions ---
	for pid, pos := range s.positions {
		cond := findPairCondition(pairs, pos.PrimarySymbol, pos.HedgeSymbol)
		if cond == nil {
			continue
		}

		shouldExit, reason := s.shouldExit(pos, cond, now)
		if !shouldExit {
			continue
		}

		intent := s.buildExitIntent(pos, now, reason)
		eval.Exits = append(eval.Exits, intent)
		eval.Decisions = append(eval.Decisions, Decision{
			PrimarySymbol: pos.PrimarySymbol,
			HedgeSymbol:   pos.HedgeSymbol,
			Strategy:      pos.Strategy,
			Confidence:    1.0,
			Reason:        "exit: " + reason,
		})
		_ = pid
	}

	// --- Phase 2: Evaluate entries for pairs without positions ---
	totalAllocated := s.totalAllocatedPct(scaledEquity)

	for _, cond := range pairs {
		pid := PairID(cond.PrimarySymbol, cond.HedgeSymbol)

		// Skip if already in position.
		if _, open := s.positions[pid]; open {
			continue
		}

		// Skip if on cooldown.
		if cd, ok := s.cooldowns[pid]; ok && now < cd {
			continue
		}

		// Skip if at max concurrent positions.
		if len(s.positions) >= s.cfg.MaxConcurrentPositions {
			break
		}

		// Skip if total allocation would exceed cap.
		if totalAllocated >= s.cfg.TotalMaxAllocationPct {
			break
		}

		// Require minimum correlation for any cross-pair trade.
		if cond.Correlation < s.cfg.MinCorrelation {
			eval.Decisions = append(eval.Decisions, Decision{
				PrimarySymbol: cond.PrimarySymbol,
				HedgeSymbol:   cond.HedgeSymbol,
				Strategy:      StrategyHold,
				Confidence:    1.0,
				Reason:        fmt.Sprintf("hold: correlation=%.2f < min=%.2f", cond.Correlation, s.cfg.MinCorrelation),
			})
			continue
		}

		decision := s.selectStrategy(cond, scaledEquity, totalAllocated)
		eval.Decisions = append(eval.Decisions, decision)

		if decision.Strategy == StrategyHold {
			continue
		}

		allocPct := math.Min(decision.AllocationPct, s.cfg.TotalMaxAllocationPct-totalAllocated)
		notional := scaledEquity * (allocPct / 100.0)
		if notional < 20.0 {
			continue
		}

		intent := s.buildEntryIntent(cond, decision, notional, now)
		eval.Entries = append(eval.Entries, intent)
		totalAllocated += allocPct
	}

	return eval
}

// selectStrategy picks the best strategy for a cross-pair based on conditions.
func (s *Selector) selectStrategy(cond PairCondition, equity, currentAllocPct float64) Decision {
	// Round-trip cost for two perp legs (both entry and exit = 4 legs total).
	roundTripCostBps := s.cfg.FeeBpsTaker * 4.0

	// --- Priority 1: Cross-pair funding carry ---
	// Short the primary (high funding) + Long the hedge (lower funding).
	// Net funding income per 8h cycle = primaryFunding + |hedgeFundingCost|/beta.
	// When primary funding is positive, shorting it earns the funding.
	// When hedge funding is negative, longing it also earns (shorts pay longs).
	fundingDiffBps := cond.FundingDiffBps
	annualDiffPct := math.Abs(fundingDiffBps) / 10000.0 * 100.0 * 1095.0

	if fundingDiffBps > 0 && fundingDiffBps >= s.cfg.MinFundingDiffBps &&
		annualDiffPct >= s.cfg.MinFundingAnnualPct &&
		(cond.Regime == "BULLISH" || cond.Regime == "NEUTRAL") {

		allocPct := s.fundingAllocationPct(fundingDiffBps)
		// Net edge per cycle after amortizing entry+exit fees over expected 6-cycle hold.
		netEdgeBps := fundingDiffBps - (roundTripCostBps / 6.0)
		return Decision{
			PrimarySymbol:       cond.PrimarySymbol,
			HedgeSymbol:         cond.HedgeSymbol,
			Strategy:            StrategyFundingCarry,
			Confidence:          math.Min(annualDiffPct/20.0, 1.0),
			Reason:              fmt.Sprintf("funding_carry: diff=%.1fbps annual=%.1f%% corr=%.2f beta=%.2f regime=%s", fundingDiffBps, annualDiffPct, cond.Correlation, cond.Beta, cond.Regime),
			AllocationPct:       allocPct,
			ExpectedEdgeBps:     netEdgeBps,
			ExpectedHoldingTime: "8h+",
		}
	}

	// --- Priority 2: Cross-pair spread mean reversion ---
	// Enter when the log price ratio deviates > 2 stddev from rolling mean.
	absZScore := math.Abs(cond.SpreadZScore)

	if absZScore >= s.cfg.BasisEntryZScore {
		spreadEdgeBps := absZScore * cond.SpreadStdDev * 10000.0
		netEdge := spreadEdgeBps - roundTripCostBps
		if netEdge >= s.cfg.MinSpreadEdgeBps {
			allocPct := math.Min(s.cfg.MaxAllocationPct, 20.0)
			return Decision{
				PrimarySymbol:       cond.PrimarySymbol,
				HedgeSymbol:         cond.HedgeSymbol,
				Strategy:            StrategyBasisReversion,
				Confidence:          math.Min(absZScore/4.0, 1.0),
				Reason:              fmt.Sprintf("spread_reversion: z=%.2f edge=%.1fbps corr=%.2f", cond.SpreadZScore, netEdge, cond.Correlation),
				AllocationPct:       allocPct,
				ExpectedEdgeBps:     netEdge,
				ExpectedHoldingTime: "1-4h",
			}
		}
	}

	// --- Priority 3: Hold ---
	return Decision{
		PrimarySymbol: cond.PrimarySymbol,
		HedgeSymbol:   cond.HedgeSymbol,
		Strategy:      StrategyHold,
		Confidence:    1.0,
		Reason:        fmt.Sprintf("hold: diff=%.1fbps z=%.2f corr=%.2f regime=%s", fundingDiffBps, cond.SpreadZScore, cond.Correlation, cond.Regime),
	}
}

// shouldExit determines if an open cross-pair position should be closed.
func (s *Selector) shouldExit(pos *openPosition, cond *PairCondition, now int64) (bool, string) {
	// Always exit if correlation has degraded below threshold.
	if cond.Correlation < s.cfg.MinCorrelation {
		return true, fmt.Sprintf("correlation_degraded_%.2f", cond.Correlation)
	}

	switch pos.Strategy {
	case StrategyFundingCarry:
		// Exit if funding differential reversed (primary no longer high).
		if cond.FundingDiffBps < 0 {
			return true, "funding_diff_reversed"
		}
		// Exit on persistently low differential.
		if cond.FundingDiffBps < s.cfg.FundingExitThresholdBps {
			pos.NegativeCycles++
			if pos.NegativeCycles >= s.cfg.FundingNegativeExitCycles {
				return true, fmt.Sprintf("diff_below_threshold_%d_cycles", pos.NegativeCycles)
			}
		} else {
			pos.NegativeCycles = 0
		}
		// Exit if regime turned bearish/persistent.
		if cond.Regime == "BEARISH" || cond.Regime == "PERSISTENT" {
			return true, "regime_bearish"
		}

	case StrategyBasisReversion:
		// Exit when spread reverts to near mean.
		if math.Abs(cond.SpreadZScore) <= s.cfg.BasisExitZScore {
			return true, "spread_converged"
		}
		// Exit if spread moved further against us.
		if cond.SpreadZScore < -s.cfg.BasisEntryZScore && pos.EntrySpread > 0 {
			return true, "spread_adverse"
		}
		if cond.SpreadZScore > s.cfg.BasisEntryZScore && pos.EntrySpread < 0 {
			return true, "spread_adverse"
		}
		// Time-based exit.
		if now-pos.EntryTimeMs > s.cfg.BasisHoldingMaxMs {
			return true, "spread_timeout"
		}
	}

	return false, ""
}

// fundingAllocationPct returns position size as % of equity based on funding diff.
func (s *Selector) fundingAllocationPct(diffBps float64) float64 {
	switch {
	case diffBps >= 30.0: // 30+ bps differential
		return math.Min(s.cfg.MaxAllocationPct, 30.0)
	case diffBps >= 10.0:
		return math.Min(s.cfg.MaxAllocationPct, 20.0)
	default:
		return math.Min(s.cfg.MaxAllocationPct, 15.0)
	}
}

// totalAllocatedPct returns the total position notional as % of equity.
func (s *Selector) totalAllocatedPct(equity float64) float64 {
	if equity <= 0 {
		return 100.0
	}
	var total float64
	for _, pos := range s.positions {
		// Total notional includes both legs: primary + hedge.
		hedgeNotional := pos.NotionalUSD / math.Max(pos.HedgeBeta, 0.1)
		total += pos.NotionalUSD + hedgeNotional
	}
	return total / equity * 100.0
}

// buildEntryIntent creates a two-leg cross-pair delta-neutral entry intent.
// Leg A: SELL primary (short the high-funding asset).
// Leg B: BUY hedge (long the correlated asset, beta-weighted).
func (s *Selector) buildEntryIntent(cond PairCondition, decision Decision, notional float64, now int64) arb.TradeIntent {
	s.seqID++

	strategyName := "funding"
	if decision.Strategy == StrategyBasisReversion {
		strategyName = "basis"
	}

	pid := PairID(cond.PrimarySymbol, cond.HedgeSymbol)
	intentID := fmt.Sprintf("%s-%s-%d-%d", strategyName, pid, now, s.seqID)

	beta := cond.Beta
	if beta <= 0 {
		beta = 1.0
	}

	primaryNotional := notional
	hedgeNotional := notional / beta

	// For spread reversion: direction depends on z-score sign.
	primaryAction := "SELL"
	hedgeAction := "BUY"
	if decision.Strategy == StrategyBasisReversion && cond.SpreadZScore < 0 {
		// Spread is below mean: primary is cheap relative to hedge.
		// Buy primary, sell hedge (expect ratio to increase).
		primaryAction = "BUY"
		hedgeAction = "SELL"
	}

	return arb.TradeIntent{
		IntentID:    intentID,
		Strategy:    strategyName,
		Symbol:      cond.PrimarySymbol,
		HedgeSymbol: cond.HedgeSymbol,
		HedgeBeta:   beta,
		TsMs:        now,
		ExpiresMs:   now + s.cfg.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Action:         primaryAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         cond.PrimarySymbol,
				NotionalUSD:    primaryNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
			{
				Action:         hedgeAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         cond.HedgeSymbol,
				NotionalUSD:    hedgeNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsGross: decision.ExpectedEdgeBps + s.cfg.FeeBpsTaker*4.0,
			EdgeBpsNet:   decision.ExpectedEdgeBps,
		},
		Constraints: arb.IntentConstraints{
			MaxAgeMs:        s.cfg.IntentTTLMs,
			HedgePreference: "SEQUENTIAL",
			CooldownKey:     fmt.Sprintf("%s:%s", strategyName, pid),
		},
	}
}

// buildExitIntent creates a two-leg exit intent (reverse of entry).
func (s *Selector) buildExitIntent(pos *openPosition, now int64, reason string) arb.TradeIntent {
	s.seqID++

	strategyName := "funding_exit"
	if pos.Strategy == StrategyBasisReversion {
		strategyName = "basis_exit"
	}

	intentID := fmt.Sprintf("%s-%s-%d-%d", strategyName, pos.PairID, now, s.seqID)

	beta := pos.HedgeBeta
	if beta <= 0 {
		beta = 1.0
	}
	hedgeNotional := pos.NotionalUSD / beta

	// Reverse of entry. If we entered SELL primary + BUY hedge,
	// exit is BUY primary + SELL hedge.
	primaryAction := "BUY"  // close short
	hedgeAction := "SELL"   // close long

	return arb.TradeIntent{
		IntentID:    intentID,
		Strategy:    strategyName,
		Symbol:      pos.PrimarySymbol,
		HedgeSymbol: pos.HedgeSymbol,
		HedgeBeta:   beta,
		TsMs:        now,
		ExpiresMs:   now + s.cfg.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Action:         primaryAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         pos.PrimarySymbol,
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
			{
				Action:         hedgeAction,
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         pos.HedgeSymbol,
				NotionalUSD:    hedgeNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsGross: 0,
			EdgeBpsNet:   0,
		},
		Constraints: arb.IntentConstraints{
			MaxAgeMs:        s.cfg.IntentTTLMs,
			HedgePreference: "SEQUENTIAL",
			CooldownKey:     fmt.Sprintf("%s:%s", strategyName, pos.PairID),
		},
	}
}

// ConfirmEntry records that a cross-pair position has been successfully opened.
func (s *Selector) ConfirmEntry(primary, hedge string, strategy StrategyType, notionalUSD float64, cond PairCondition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	pid := PairID(primary, hedge)
	s.positions[pid] = &openPosition{
		PairID:        pid,
		PrimarySymbol: primary,
		HedgeSymbol:   hedge,
		Strategy:      strategy,
		NotionalUSD:   notionalUSD,
		HedgeBeta:     cond.Beta,
		EntrySpread:   cond.SpreadZScore,
		EntryFundDiff: cond.FundingDiffBps,
		EntryTimeMs:   now,
	}
	s.cooldowns[pid] = now + int64(s.cfg.CooldownS)*1000
}

// ConfirmExit removes a position after it has been closed.
func (s *Selector) ConfirmExit(primary, hedge string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	pid := PairID(primary, hedge)
	delete(s.positions, pid)
	s.cooldowns[pid] = now + int64(s.cfg.CooldownS)*1000
}

// OpenPositions returns a snapshot of all open positions.
func (s *Selector) OpenPositions() map[string]PositionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]PositionState, len(s.positions))
	for pid, pos := range s.positions {
		out[pid] = PositionState{
			PrimarySymbol: pos.PrimarySymbol,
			HedgeSymbol:   pos.HedgeSymbol,
			Strategy:      pos.Strategy,
			NotionalUSD:   pos.NotionalUSD,
			HedgeBeta:     pos.HedgeBeta,
			EntrySpread:   pos.EntrySpread,
			EntryRate:     pos.EntryFundDiff,
			EntryTimeMs:   pos.EntryTimeMs,
		}
	}
	return out
}

// PositionCount returns the number of open positions.
func (s *Selector) PositionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.positions)
}

func findPairCondition(pairs []PairCondition, primary, hedge string) *PairCondition {
	for i := range pairs {
		if pairs[i].PrimarySymbol == primary && pairs[i].HedgeSymbol == hedge {
			return &pairs[i]
		}
	}
	return nil
}
