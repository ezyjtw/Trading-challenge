package strategy

import (
	"math"
	"sync"
)

// BasisConfig tunes the basis tracking and trading parameters.
type BasisConfig struct {
	// WindowSize is the number of basis observations to keep for statistics.
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

// BasisTracker maintains rolling statistics on the spot-perp basis per symbol.
// The basis is defined as (perp_price - spot_price) / spot_price.
type BasisTracker struct {
	mu      sync.Mutex
	cfg     BasisConfig
	samples map[string][]basisSample
}

// NewBasisTracker creates a new BasisTracker.
func NewBasisTracker(cfg BasisConfig) *BasisTracker {
	return &BasisTracker{
		cfg:     cfg,
		samples: make(map[string][]basisSample),
	}
}

// Record adds a basis observation for a symbol.
func (bt *BasisTracker) Record(symbol string, spotPrice, perpPrice float64, tsMs int64) {
	if spotPrice <= 0 {
		return
	}
	basis := (perpPrice - spotPrice) / spotPrice

	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.samples[symbol] = append(bt.samples[symbol], basisSample{
		basis: basis,
		tsMs:  tsMs,
	})

	// Prune to window size.
	if len(bt.samples[symbol]) > bt.cfg.WindowSize {
		bt.samples[symbol] = bt.samples[symbol][len(bt.samples[symbol])-bt.cfg.WindowSize:]
	}
}

// Stats returns the rolling mean, standard deviation, and sample count for a symbol.
func (bt *BasisTracker) Stats(symbol string) (mean, stddev float64, count int) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	samples := bt.samples[symbol]
	count = len(samples)
	if count == 0 {
		return 0, 0, 0
	}

	// Mean.
	var sum float64
	for _, s := range samples {
		sum += s.basis
	}
	mean = sum / float64(count)

	// Std dev.
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

// CurrentBasis returns the most recent basis observation for a symbol.
func (bt *BasisTracker) CurrentBasis(symbol string) (float64, bool) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	samples := bt.samples[symbol]
	if len(samples) == 0 {
		return 0, false
	}
	return samples[len(samples)-1].basis, true
}

// ZScore returns the current basis z-score for a symbol.
// Returns 0 if insufficient data.
func (bt *BasisTracker) ZScore(symbol string) float64 {
	mean, stddev, count := bt.Stats(symbol)
	if count < bt.cfg.MinSamples || stddev < 1e-10 {
		return 0
	}

	current, ok := bt.CurrentBasis(symbol)
	if !ok {
		return 0
	}

	return (current - mean) / stddev
}

// Condition returns a MarketCondition populated with basis statistics.
func (bt *BasisTracker) Condition(symbol string, spotPrice, perpPrice, fundingRate float64, regime string, tsMs int64) MarketCondition {
	mean, stddev, _ := bt.Stats(symbol)
	basis := 0.0
	if spotPrice > 0 {
		basis = (perpPrice - spotPrice) / spotPrice
	}
	zScore := bt.ZScore(symbol)

	return MarketCondition{
		Symbol:      symbol,
		SpotPrice:   spotPrice,
		PerpPrice:   perpPrice,
		FundingRate: fundingRate,
		Basis:       basis,
		BasisZScore: zScore,
		BasisMean:   mean,
		BasisStdDev: stddev,
		Regime:      regime,
		TsMs:        tsMs,
	}
}
