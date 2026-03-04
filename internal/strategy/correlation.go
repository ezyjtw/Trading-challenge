package strategy

import (
	"math"
	"sync"
)

// PairConfig defines a trading pair for cross-pair delta-neutral strategies.
type PairConfig struct {
	Primary        string  `yaml:"primary"`         // e.g. "BTCUSDT" (the funding target)
	Hedge          string  `yaml:"hedge"`            // e.g. "ETHUSDT" (the correlated hedge)
	MinCorrelation float64 `yaml:"min_correlation"`  // exit if correlation drops below this (e.g. 0.70)
	MaxBetaDrift   float64 `yaml:"max_beta_drift"`   // exit if beta drifts more than this from entry (e.g. 0.5)
}

// DefaultPairs returns the default cross-pair configurations.
func DefaultPairs() []PairConfig {
	return []PairConfig{
		{Primary: "BTCUSDT", Hedge: "ETHUSDT", MinCorrelation: 0.70, MaxBetaDrift: 0.5},
	}
}

// returnSample records a single price return observation.
type returnSample struct {
	primaryReturn float64
	hedgeReturn   float64
	tsMs          int64
}

// CorrelationTracker maintains rolling correlation and beta between paired assets.
// Beta is computed via OLS regression: hedge_return = alpha + beta * primary_return.
// Correlation is Pearson's r over the same window.
type CorrelationTracker struct {
	mu      sync.Mutex
	window  int // number of return observations to keep
	samples map[string][]returnSample // pairID -> samples

	// Previous prices for computing returns.
	lastPrices map[string][2]float64 // pairID -> [primaryPrice, hedgePrice]
}

// NewCorrelationTracker creates a tracker with the given window size (number of returns).
func NewCorrelationTracker(windowSize int) *CorrelationTracker {
	if windowSize < 10 {
		windowSize = 10
	}
	return &CorrelationTracker{
		window:     windowSize,
		samples:    make(map[string][]returnSample),
		lastPrices: make(map[string][2]float64),
	}
}

// PairID returns a canonical identifier for a trading pair.
func PairID(primary, hedge string) string {
	return primary + ":" + hedge
}

// RecordPrices records new prices for both assets in a pair and computes returns.
// Call this at regular intervals (e.g. every 30s).
func (ct *CorrelationTracker) RecordPrices(primary, hedge string, primaryPrice, hedgePrice float64, tsMs int64) {
	if primaryPrice <= 0 || hedgePrice <= 0 {
		return
	}

	ct.mu.Lock()
	defer ct.mu.Unlock()

	pid := PairID(primary, hedge)

	last, hasLast := ct.lastPrices[pid]
	ct.lastPrices[pid] = [2]float64{primaryPrice, hedgePrice}

	if !hasLast || last[0] <= 0 || last[1] <= 0 {
		return // need at least two observations to compute returns
	}

	primaryRet := (primaryPrice - last[0]) / last[0]
	hedgeRet := (hedgePrice - last[1]) / last[1]

	ct.samples[pid] = append(ct.samples[pid], returnSample{
		primaryReturn: primaryRet,
		hedgeReturn:   hedgeRet,
		tsMs:          tsMs,
	})

	// Prune to window.
	if len(ct.samples[pid]) > ct.window {
		ct.samples[pid] = ct.samples[pid][len(ct.samples[pid])-ct.window:]
	}
}

// Correlation returns the Pearson correlation coefficient between primary and
// hedge returns. Returns 0 if insufficient data (< 10 samples).
func (ct *CorrelationTracker) Correlation(primary, hedge string) float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	samples := ct.samples[PairID(primary, hedge)]
	if len(samples) < 10 {
		return 0
	}

	n := float64(len(samples))

	var sumX, sumY, sumXY, sumX2, sumY2 float64
	for _, s := range samples {
		sumX += s.primaryReturn
		sumY += s.hedgeReturn
		sumXY += s.primaryReturn * s.hedgeReturn
		sumX2 += s.primaryReturn * s.primaryReturn
		sumY2 += s.hedgeReturn * s.hedgeReturn
	}

	num := n*sumXY - sumX*sumY
	den := math.Sqrt((n*sumX2 - sumX*sumX) * (n*sumY2 - sumY*sumY))
	if den < 1e-12 {
		return 0
	}

	return num / den
}

// Beta returns the OLS regression slope: hedge_return = alpha + beta * primary_return.
// This tells us how much hedge moves per unit of primary movement.
// Returns 1.0 if insufficient data.
func (ct *CorrelationTracker) Beta(primary, hedge string) float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	samples := ct.samples[PairID(primary, hedge)]
	if len(samples) < 10 {
		return 1.0 // default to 1:1 until we have data
	}

	n := float64(len(samples))

	var sumX, sumY, sumXY, sumX2 float64
	for _, s := range samples {
		sumX += s.primaryReturn
		sumY += s.hedgeReturn
		sumXY += s.primaryReturn * s.hedgeReturn
		sumX2 += s.primaryReturn * s.primaryReturn
	}

	den := n*sumX2 - sumX*sumX
	if math.Abs(den) < 1e-12 {
		return 1.0
	}

	return (n*sumXY - sumX*sumY) / den
}

// SampleCount returns the number of return samples available for a pair.
func (ct *CorrelationTracker) SampleCount(primary, hedge string) int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return len(ct.samples[PairID(primary, hedge)])
}

// IsReliable returns true if correlation and beta estimates have enough
// data to be trustworthy (>= minSamples observations).
func (ct *CorrelationTracker) IsReliable(primary, hedge string, minSamples int) bool {
	return ct.SampleCount(primary, hedge) >= minSamples
}
