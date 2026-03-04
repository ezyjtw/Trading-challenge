package funding

import (
	"log"
	"sync"
)

// RegimeConfig configures the funding rate regime detector.
type RegimeConfig struct {
	// LookbackPeriods is how many 8h funding observations to keep.
	LookbackPeriods int `yaml:"lookback_periods"` // e.g. 90 (30 days)
	// NegativeThresholdPct: if this % of recent observations are negative, regime is "bearish".
	NegativeThresholdPct float64 `yaml:"negative_threshold_pct"` // e.g. 60
	// ReducePositionPct: when in bearish regime, scale notional by this factor.
	ReducePositionPct float64 `yaml:"reduce_position_pct"` // e.g. 25 (reduce to 25%)
	// ExitAllOnPersistent: if true, exit all positions when regime is persistently bearish.
	ExitAllOnPersistent bool `yaml:"exit_all_on_persistent"`
	// PersistentNegativePeriods: how many consecutive negative observations before "persistent".
	PersistentNegativePeriods int `yaml:"persistent_negative_periods"` // e.g. 12 (4 days)
}

// DefaultRegimeConfig returns conservative defaults.
func DefaultRegimeConfig() RegimeConfig {
	return RegimeConfig{
		LookbackPeriods:           90,
		NegativeThresholdPct:      60.0,
		ReducePositionPct:         25.0,
		ExitAllOnPersistent:       true,
		PersistentNegativePeriods: 12,
	}
}

// Regime classifies the current funding rate environment.
type Regime string

const (
	RegimeBullish    Regime = "BULLISH"    // mostly positive funding, collect carry
	RegimeNeutral    Regime = "NEUTRAL"    // mixed signals, normal sizing
	RegimeBearish    Regime = "BEARISH"    // mostly negative funding, reduce exposure
	RegimePersistent Regime = "PERSISTENT" // persistently negative, exit all
)

// rateSample records a single funding rate observation.
type rateSample struct {
	symbol string
	rate   float64
	tsMs   int64
}

// RegimeDetector monitors funding rate trends across symbols and classifies
// the current market regime. This allows the funding engine to proactively
// reduce exposure before cumulative negative funding bleeds the account.
type RegimeDetector struct {
	mu      sync.Mutex
	cfg     RegimeConfig
	samples map[string][]rateSample // symbol -> recent observations
}

// NewRegimeDetector creates a new regime detector.
func NewRegimeDetector(cfg RegimeConfig) *RegimeDetector {
	return &RegimeDetector{
		cfg:     cfg,
		samples: make(map[string][]rateSample),
	}
}

// RecordRate adds a funding rate observation for a symbol.
func (rd *RegimeDetector) RecordRate(symbol string, rate float64, tsMs int64) {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	rd.samples[symbol] = append(rd.samples[symbol], rateSample{
		symbol: symbol,
		rate:   rate,
		tsMs:   tsMs,
	})

	// Prune to lookback window.
	if len(rd.samples[symbol]) > rd.cfg.LookbackPeriods {
		rd.samples[symbol] = rd.samples[symbol][len(rd.samples[symbol])-rd.cfg.LookbackPeriods:]
	}
}

// DetectRegime returns the current funding rate regime for a symbol.
func (rd *RegimeDetector) DetectRegime(symbol string) Regime {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	samples, ok := rd.samples[symbol]
	if !ok || len(samples) < 3 {
		return RegimeNeutral // not enough data
	}

	// Check persistent negative streak (most recent N observations all negative).
	if len(samples) >= rd.cfg.PersistentNegativePeriods {
		allNeg := true
		tail := samples[len(samples)-rd.cfg.PersistentNegativePeriods:]
		for _, s := range tail {
			if s.rate >= 0 {
				allNeg = false
				break
			}
		}
		if allNeg {
			return RegimePersistent
		}
	}

	// Count negatives in lookback.
	negCount := 0
	for _, s := range samples {
		if s.rate < 0 {
			negCount++
		}
	}
	negPct := float64(negCount) / float64(len(samples)) * 100.0

	if negPct >= rd.cfg.NegativeThresholdPct {
		return RegimeBearish
	}

	// Positive-dominant.
	posPct := 100.0 - negPct
	if posPct >= rd.cfg.NegativeThresholdPct {
		return RegimeBullish
	}

	return RegimeNeutral
}

// PositionScaleFactor returns a multiplier (0.0–1.0) for position sizing
// based on the current regime. In bullish regime it's 1.0; in bearish it's
// reduced; in persistent it's 0.0 (exit all).
func (rd *RegimeDetector) PositionScaleFactor(symbol string) float64 {
	regime := rd.DetectRegime(symbol)
	switch regime {
	case RegimeBullish:
		return 1.0
	case RegimeNeutral:
		return 0.75
	case RegimeBearish:
		return rd.cfg.ReducePositionPct / 100.0
	case RegimePersistent:
		if rd.cfg.ExitAllOnPersistent {
			return 0.0
		}
		return rd.cfg.ReducePositionPct / 100.0
	default:
		return 1.0
	}
}

// ShouldExitAll returns true if the regime is persistently negative and
// the config says to exit all positions.
func (rd *RegimeDetector) ShouldExitAll(symbol string) bool {
	regime := rd.DetectRegime(symbol)
	if regime == RegimePersistent && rd.cfg.ExitAllOnPersistent {
		log.Printf("funding/regime: PERSISTENT negative regime for %s — exit all", symbol)
		return true
	}
	return false
}

// Summary returns a map of symbol -> regime for all tracked symbols.
func (rd *RegimeDetector) Summary() map[string]Regime {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	out := make(map[string]Regime, len(rd.samples))
	for sym := range rd.samples {
		// Release and reacquire to call public method.
		rd.mu.Unlock()
		out[sym] = rd.DetectRegime(sym)
		rd.mu.Lock()
	}
	return out
}
