package strategy

import (
	"math"
	"sync"
)

// BasisConfig tunes the basis tracking and trading parameters.
type BasisConfig struct {
	// WindowSize is the number of observations to keep for statistics.
	WindowSize int `yaml:"window_size"` // e.g. 720 (6h of 30s samples)
	// MinSamples is the minimum observations needed before computing z-scores.
	MinSamples int `yaml:"min_samples"` // e.g. 60
}

// DefaultBasisConfig returns conservative defaults.
func DefaultBasisConfig() BasisConfig {
	return BasisConfig{
		WindowSize: 720, // 6 hours at 30s intervals
		MinSamples: 60,  // 30 minutes of data
	}
}

type basisSample struct {
	basis float64
	tsMs  int64
}

// BasisTracker maintains rolling statistics on the perp premium per symbol.
// The perp premium is defined as (perp_price - index_price) / index_price.
// Also tracks cross-pair spread (log price ratio) for pair trading.
type BasisTracker struct {
	mu      sync.Mutex
	cfg     BasisConfig
	samples map[string][]basisSample // keyed by symbol or pairID
}

// NewBasisTracker creates a new BasisTracker.
func NewBasisTracker(cfg BasisConfig) *BasisTracker {
	return &BasisTracker{
		cfg:     cfg,
		samples: make(map[string][]basisSample),
	}
}

// RecordPremium adds a perp premium observation for a single symbol.
// Premium is (perpPrice - indexPrice) / indexPrice.
func (bt *BasisTracker) RecordPremium(symbol string, indexPrice, perpPrice float64, tsMs int64) {
	if indexPrice <= 0 {
		return
	}
	premium := (perpPrice - indexPrice) / indexPrice

	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.samples[symbol] = append(bt.samples[symbol], basisSample{
		basis: premium,
		tsMs:  tsMs,
	})

	if len(bt.samples[symbol]) > bt.cfg.WindowSize {
		bt.samples[symbol] = bt.samples[symbol][len(bt.samples[symbol])-bt.cfg.WindowSize:]
	}
}

// RecordPairSpread records the cross-pair log price ratio for a trading pair.
// spread = ln(primaryPrice / hedgePrice) — tracks relative value drift.
func (bt *BasisTracker) RecordPairSpread(primary, hedge string, primaryPrice, hedgePrice float64, tsMs int64) {
	if primaryPrice <= 0 || hedgePrice <= 0 {
		return
	}
	spread := math.Log(primaryPrice / hedgePrice)

	bt.mu.Lock()
	defer bt.mu.Unlock()

	key := PairID(primary, hedge)
	bt.samples[key] = append(bt.samples[key], basisSample{
		basis: spread,
		tsMs:  tsMs,
	})

	if len(bt.samples[key]) > bt.cfg.WindowSize {
		bt.samples[key] = bt.samples[key][len(bt.samples[key])-bt.cfg.WindowSize:]
	}
}

// Stats returns the rolling mean, standard deviation, and sample count for a key.
func (bt *BasisTracker) Stats(key string) (mean, stddev float64, count int) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	samples := bt.samples[key]
	count = len(samples)
	if count == 0 {
		return 0, 0, 0
	}

	var sum float64
	for _, s := range samples {
		sum += s.basis
	}
	mean = sum / float64(count)

	if count < 2 {
		return mean, 0, count
	}
	var sqDiff float64
	for _, s := range samples {
		d := s.basis - mean
		sqDiff += d * d
	}
	stddev = math.Sqrt(sqDiff / float64(count-1))

	return mean, stddev, count
}

// CurrentValue returns the most recent observation for a key.
func (bt *BasisTracker) CurrentValue(key string) (float64, bool) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	samples := bt.samples[key]
	if len(samples) == 0 {
		return 0, false
	}
	return samples[len(samples)-1].basis, true
}

// ZScore returns the current z-score for a key.
// Returns 0 if insufficient data.
func (bt *BasisTracker) ZScore(key string) float64 {
	mean, stddev, count := bt.Stats(key)
	if count < bt.cfg.MinSamples || stddev < 1e-10 {
		return 0
	}

	current, ok := bt.CurrentValue(key)
	if !ok {
		return 0
	}

	return (current - mean) / stddev
}

// PairSpreadZScore returns the z-score of the cross-pair log price ratio.
func (bt *BasisTracker) PairSpreadZScore(primary, hedge string) float64 {
	return bt.ZScore(PairID(primary, hedge))
}

// PairSpreadStats returns spread statistics for a trading pair.
func (bt *BasisTracker) PairSpreadStats(primary, hedge string) (mean, stddev float64, count int) {
	return bt.Stats(PairID(primary, hedge))
}

// Condition returns a MarketCondition populated with perp premium statistics.
func (bt *BasisTracker) Condition(symbol string, indexPrice, perpPrice, fundingRate float64, regime string, tsMs int64) MarketCondition {
	mean, stddev, _ := bt.Stats(symbol)
	premium := 0.0
	if indexPrice > 0 {
		premium = (perpPrice - indexPrice) / indexPrice
	}
	zScore := bt.ZScore(symbol)

	return MarketCondition{
		Symbol:      symbol,
		PerpPrice:   perpPrice,
		IndexPrice:  indexPrice,
		FundingRate: fundingRate,
		PerpPremium: premium,
		BasisZScore: zScore,
		BasisMean:   mean,
		BasisStdDev: stddev,
		Regime:      regime,
		TsMs:        tsMs,
	}
}
