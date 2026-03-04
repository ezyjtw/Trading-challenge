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
	// BasisEntryZScore is the z-score threshold to enter a basis trade.
	BasisEntryZScore float64 `yaml:"basis_entry_z_score"` // e.g. 2.0
	// BasisExitZScore is the z-score threshold to exit a basis trade (mean reversion target).
	BasisExitZScore float64 `yaml:"basis_exit_z_score"` // e.g. 0.5
	// MinBasisAbsPct is the minimum absolute basis (%) to bother trading.
	MinBasisAbsPct float64 `yaml:"min_basis_abs_pct"` // e.g. 0.05
	// MinFundingAnnualPct is the minimum annualized yield (%) for funding carry.
	MinFundingAnnualPct float64 `yaml:"min_funding_annual_pct"` // e.g. 5.0
	// MaxConcurrentPositions limits how many symbols can be in position simultaneously.
	MaxConcurrentPositions int `yaml:"max_concurrent_positions"` // e.g. 2
	// MaxAllocationPct is the max % of equity per position.
	MaxAllocationPct float64 `yaml:"max_allocation_pct"` // e.g. 30.0
	// TotalMaxAllocationPct caps total allocation across all positions.
	TotalMaxAllocationPct float64 `yaml:"total_max_allocation_pct"` // e.g. 60.0
	// FeeBpsTaker is the estimated round-trip taker fee in bps.
	FeeBpsTaker float64 `yaml:"fee_bps_taker"` // e.g. 5.5
	// MaxSlippageBps is the max slippage tolerance per leg.
	MaxSlippageBps float64 `yaml:"max_slippage_bps"` // e.g. 5.0
	// IntentTTLMs is the time-to-live for generated intents.
	IntentTTLMs int64 `yaml:"intent_ttl_ms"` // e.g. 10000
	// CooldownS is the minimum seconds between trades on the same symbol.
	CooldownS int `yaml:"cooldown_s"` // e.g. 300
	// FundingExitThreshold is the 8h rate below which we consider exiting funding carry.
	FundingExitThreshold float64 `yaml:"funding_exit_threshold"` // e.g. 0.00005
	// FundingNegativeExitCycles exits after N consecutive cycles below threshold.
	FundingNegativeExitCycles int `yaml:"funding_negative_exit_cycles"` // e.g. 2
	// BasisHoldingMaxMs is the maximum time to hold a basis trade before force-exiting.
	BasisHoldingMaxMs int64 `yaml:"basis_holding_max_ms"` // e.g. 4 hours
}

// DefaultSelectorConfig returns conservative defaults for the challenge.
func DefaultSelectorConfig() SelectorConfig {
	return SelectorConfig{
		BasisEntryZScore:          2.0,
		BasisExitZScore:           0.5,
		MinBasisAbsPct:            0.05,
		MinFundingAnnualPct:       5.0,
		MaxConcurrentPositions:    2,
		MaxAllocationPct:          30.0,
		TotalMaxAllocationPct:     60.0,
		FeeBpsTaker:               5.5,
		MaxSlippageBps:            5.0,
		IntentTTLMs:               10000,
		CooldownS:                 300,
		FundingExitThreshold:      0.00005,
		FundingNegativeExitCycles: 2,
		BasisHoldingMaxMs:         4 * 60 * 60 * 1000, // 4 hours
	}
}

// openPosition tracks a live position managed by the selector.
type openPosition struct {
	Symbol         string
	Strategy       StrategyType
	NotionalUSD    float64
	EntryBasis     float64 // for basis trades
	EntryRate      float64 // for funding carry
	EntryTimeMs    int64
	NegativeCycles int // consecutive cycles with funding below threshold
}

// Selector autonomously evaluates market conditions and generates trade intents
// for the best available delta-neutral strategy.
type Selector struct {
	mu        sync.Mutex
	cfg       SelectorConfig
	positions map[string]*openPosition // symbol -> position
	cooldowns map[string]int64         // symbol -> cooldown expiry (unix ms)
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

// Evaluate runs a full strategy evaluation cycle. It takes current market
// conditions for all symbols, account equity, and the regime scale factor.
// Returns trade intents for new entries and exits.
func (s *Selector) Evaluate(conditions []MarketCondition, accountEquity float64, regimeScale float64) Evaluation {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	scaledEquity := accountEquity * regimeScale

	var eval Evaluation

	// --- Phase 1: Evaluate exits for existing positions ---
	for sym, pos := range s.positions {
		cond := findCondition(conditions, sym)
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
			Symbol:     sym,
			Strategy:   pos.Strategy,
			Confidence: 1.0,
			Reason:     "exit: " + reason,
		})
	}

	// --- Phase 2: Evaluate entries for symbols without positions ---
	totalAllocated := s.totalAllocatedPct(scaledEquity)

	for _, cond := range conditions {
		// Skip if already in position.
		if _, open := s.positions[cond.Symbol]; open {
			continue
		}

		// Skip if on cooldown.
		if cd, ok := s.cooldowns[cond.Symbol]; ok && now < cd {
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

// selectStrategy picks the best strategy for a symbol based on conditions.
func (s *Selector) selectStrategy(cond MarketCondition, equity, currentAllocPct float64) Decision {
	// --- Priority 1: Funding carry (safest, most predictable) ---
	// Funding carry collects rate every 8h. Entry/exit fees are paid once.
	annualYieldPct := cond.FundingRate * 100.0 * 1095.0
	roundTripCostBps := s.cfg.FeeBpsTaker * 2.0
	fundingBpsPerCycle := cond.FundingRate * 10000.0

	// How many 8h cycles to break even on fees. Lower = better.
	cyclesToBreakeven := math.MaxFloat64
	if fundingBpsPerCycle > 0 {
		cyclesToBreakeven = roundTripCostBps / fundingBpsPerCycle
	}

	if cond.FundingRate > 0 && annualYieldPct >= s.cfg.MinFundingAnnualPct &&
		cyclesToBreakeven <= 12 && // break even within 4 days
		(cond.Regime == "BULLISH" || cond.Regime == "NEUTRAL") {

		allocPct := s.fundingAllocationPct(cond.FundingRate)
		// Net edge per cycle after amortizing fees over expected 6-cycle (2-day) hold.
		netEdgeBps := fundingBpsPerCycle - (roundTripCostBps / 6.0)
		return Decision{
			Symbol:              cond.Symbol,
			Strategy:            StrategyFundingCarry,
			Confidence:          math.Min(annualYieldPct/20.0, 1.0),
			Reason:              fmt.Sprintf("funding_carry: annual=%.1f%% bps/cycle=%.1f breakeven=%.0f regime=%s", annualYieldPct, fundingBpsPerCycle, cyclesToBreakeven, cond.Regime),
			AllocationPct:       allocPct,
			ExpectedEdgeBps:     netEdgeBps,
			ExpectedHoldingTime: "8h+",
		}
	}

	// --- Priority 2: Basis mean reversion ---
	// Only enter when basis z-score is extended (positive = perp premium high).
	// We trade: buy spot + sell perp (same structure as funding carry),
	// but exit when basis converges rather than on funding rate changes.
	absZScore := math.Abs(cond.BasisZScore)
	absBasisPct := math.Abs(cond.Basis) * 100.0

	if absZScore >= s.cfg.BasisEntryZScore && absBasisPct >= s.cfg.MinBasisAbsPct {
		// Only trade the positive basis side (perp premium) since we can
		// only buy spot + sell perp on the challenge account.
		if cond.BasisZScore > 0 {
			basisEdgeBps := absBasisPct * 100.0 // convert % to bps
			netBasisEdge := basisEdgeBps - roundTripCostBps
			if netBasisEdge > 0 {
				allocPct := math.Min(s.cfg.MaxAllocationPct, 20.0) // conservative for basis trades
				return Decision{
					Symbol:              cond.Symbol,
					Strategy:            StrategyBasisReversion,
					Confidence:          math.Min(absZScore/4.0, 1.0),
					Reason:              fmt.Sprintf("basis_reversion: z=%.2f basis=%.4f%% edge=%.1fbps", cond.BasisZScore, absBasisPct, netBasisEdge),
					AllocationPct:       allocPct,
					ExpectedEdgeBps:     netBasisEdge,
					ExpectedHoldingTime: "1-4h",
				}
			}
		}
	}

	// --- Priority 3: Hold ---
	return Decision{
		Symbol:     cond.Symbol,
		Strategy:   StrategyHold,
		Confidence: 1.0,
		Reason:     fmt.Sprintf("hold: funding=%.4f%% z_score=%.2f regime=%s", cond.FundingRate*100, cond.BasisZScore, cond.Regime),
	}
}

// shouldExit determines if an open position should be closed.
func (s *Selector) shouldExit(pos *openPosition, cond *MarketCondition, now int64) (bool, string) {
	switch pos.Strategy {
	case StrategyFundingCarry:
		// Exit on negative funding (immediate).
		if cond.FundingRate < 0 {
			return true, "negative_funding"
		}
		// Exit on persistently low funding.
		if cond.FundingRate < s.cfg.FundingExitThreshold {
			pos.NegativeCycles++
			if pos.NegativeCycles >= s.cfg.FundingNegativeExitCycles {
				return true, fmt.Sprintf("below_threshold_%d_cycles", pos.NegativeCycles)
			}
		} else {
			pos.NegativeCycles = 0
		}
		// Exit if regime turned bearish/persistent.
		if cond.Regime == "BEARISH" || cond.Regime == "PERSISTENT" {
			return true, "regime_bearish"
		}

	case StrategyBasisReversion:
		// Exit when basis reverts to near mean.
		if math.Abs(cond.BasisZScore) <= s.cfg.BasisExitZScore {
			return true, "basis_converged"
		}
		// Exit if basis moved against us (stop-loss: z-score doubled).
		if cond.BasisZScore < -s.cfg.BasisEntryZScore {
			return true, "basis_adverse"
		}
		// Time-based exit: don't hold basis trades too long.
		if now-pos.EntryTimeMs > s.cfg.BasisHoldingMaxMs {
			return true, "basis_timeout"
		}
	}

	return false, ""
}

// fundingAllocationPct returns position size as % of equity based on rate magnitude.
func (s *Selector) fundingAllocationPct(rate float64) float64 {
	ratePct := rate * 100.0
	switch {
	case ratePct >= 0.03:
		return math.Min(s.cfg.MaxAllocationPct, 30.0)
	case ratePct >= 0.01:
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
		total += pos.NotionalUSD
	}
	return total / equity * 100.0
}

// buildEntryIntent creates a two-leg delta-neutral entry intent.
func (s *Selector) buildEntryIntent(cond MarketCondition, decision Decision, notional float64, now int64) arb.TradeIntent {
	s.seqID++

	strategyName := "funding"
	if decision.Strategy == StrategyBasisReversion {
		strategyName = "basis"
	}

	intentID := fmt.Sprintf("%s-%s-%d-%d", strategyName, cond.Symbol, now, s.seqID)
	halfNotional := notional / 2.0

	return arb.TradeIntent{
		IntentID:  intentID,
		Strategy:  strategyName,
		Symbol:    cond.Symbol,
		TsMs:      now,
		ExpiresMs: now + s.cfg.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Action:         "BUY",
				Type:           "LIMIT",
				Market:         "SPOT",
				Symbol:         cond.Symbol,
				NotionalUSD:    halfNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
			{
				Action:         "SELL",
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         cond.Symbol,
				NotionalUSD:    halfNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsGross: decision.ExpectedEdgeBps + s.cfg.FeeBpsTaker*2.0,
			EdgeBpsNet:   decision.ExpectedEdgeBps,
		},
		Constraints: arb.IntentConstraints{
			MaxAgeMs:        s.cfg.IntentTTLMs,
			HedgePreference: "SEQUENTIAL",
			CooldownKey:     fmt.Sprintf("%s:%s", strategyName, cond.Symbol),
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

	intentID := fmt.Sprintf("%s-%s-%d-%d", strategyName, pos.Symbol, now, s.seqID)
	halfNotional := pos.NotionalUSD / 2.0

	return arb.TradeIntent{
		IntentID:  intentID,
		Strategy:  strategyName,
		Symbol:    pos.Symbol,
		TsMs:      now,
		ExpiresMs: now + s.cfg.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Action:         "SELL",
				Type:           "LIMIT",
				Market:         "SPOT",
				Symbol:         pos.Symbol,
				NotionalUSD:    halfNotional,
				MaxSlippageBps: s.cfg.MaxSlippageBps,
			},
			{
				Action:         "BUY",
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         pos.Symbol,
				NotionalUSD:    halfNotional,
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
			CooldownKey:     fmt.Sprintf("%s:%s", strategyName, pos.Symbol),
		},
	}
}

// ConfirmEntry records that a position has been successfully opened.
func (s *Selector) ConfirmEntry(symbol string, strategy StrategyType, notionalUSD float64, cond MarketCondition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	s.positions[symbol] = &openPosition{
		Symbol:      symbol,
		Strategy:    strategy,
		NotionalUSD: notionalUSD,
		EntryBasis:  cond.Basis,
		EntryRate:   cond.FundingRate,
		EntryTimeMs: now,
	}
	s.cooldowns[symbol] = now + int64(s.cfg.CooldownS)*1000
}

// ConfirmExit removes a position after it has been closed.
func (s *Selector) ConfirmExit(symbol string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	delete(s.positions, symbol)
	s.cooldowns[symbol] = now + int64(s.cfg.CooldownS)*1000
}

// OpenPositions returns a snapshot of all open positions.
func (s *Selector) OpenPositions() map[string]PositionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]PositionState, len(s.positions))
	for sym, pos := range s.positions {
		out[sym] = PositionState{
			Symbol:      sym,
			Strategy:    pos.Strategy,
			NotionalUSD: pos.NotionalUSD,
			EntryBasis:  pos.EntryBasis,
			EntryRate:   pos.EntryRate,
			EntryTimeMs: pos.EntryTimeMs,
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

func findCondition(conditions []MarketCondition, symbol string) *MarketCondition {
	for i := range conditions {
		if conditions[i].Symbol == symbol {
			return &conditions[i]
		}
	}
	return nil
}
