package risk

import (
	"log"
	"math"
	"sync"
	"time"
)

// CircuitBreakerConfig configures flash crash and anomaly detection.
type CircuitBreakerConfig struct {
	// PriceMovePctThreshold: if price moves more than this % in WindowMs, trip.
	PriceMovePctThreshold float64 `yaml:"price_move_pct_threshold"` // e.g. 5.0
	// WindowMs is the lookback window for price move detection.
	WindowMs int64 `yaml:"window_ms"` // e.g. 60000 (1 min)
	// SpreadMultiplierThreshold: if bid-ask spread exceeds normal * this, trip.
	SpreadMultiplierThreshold float64 `yaml:"spread_multiplier_threshold"` // e.g. 3.0
	// NormalSpreadBps is the expected typical spread for liquid pairs.
	NormalSpreadBps float64 `yaml:"normal_spread_bps"` // e.g. 1.0
	// CooldownMs: after tripping, reject intents for this duration.
	CooldownMs int64 `yaml:"cooldown_ms"` // e.g. 30000
	// StaleQuoteMs: if latest quote is older than this, circuit is open.
	StaleQuoteMs int64 `yaml:"stale_quote_ms"` // e.g. 5000
}

// DefaultCircuitBreakerConfig returns conservative defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  60000,
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           1.0,
		CooldownMs:               30000,
		StaleQuoteMs:              5000,
	}
}

// priceSample records a price observation.
type priceSample struct {
	price float64
	tsMs  int64
}

// symbolCBState tracks per-symbol circuit breaker state.
type symbolCBState struct {
	samples       []priceSample
	lastBid       float64
	lastAsk       float64
	lastQuoteTsMs int64
	trippedUntil  int64 // unix ms; 0 = not tripped
	tripReason    string
}

// CircuitBreaker monitors for flash crashes, abnormal spreads, and stale data.
// When tripped, it rejects all new order intents until the cooldown expires.
type CircuitBreaker struct {
	mu    sync.Mutex
	cfg   CircuitBreakerConfig
	state map[string]*symbolCBState
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		cfg:   cfg,
		state: make(map[string]*symbolCBState),
	}
}

// RecordQuote ingests a market data quote and checks for anomalies.
// Returns a non-empty reason string if the circuit breaker was tripped.
func (cb *CircuitBreaker) RecordQuote(symbol string, price, bid, ask float64, tsMs int64) string {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st, ok := cb.state[symbol]
	if !ok {
		st = &symbolCBState{
			samples: make([]priceSample, 0, 1024),
		}
		cb.state[symbol] = st
	}

	st.lastBid = bid
	st.lastAsk = ask
	st.lastQuoteTsMs = tsMs

	if price <= 0 {
		return ""
	}

	st.samples = append(st.samples, priceSample{price: price, tsMs: tsMs})

	// Prune samples outside the window.
	cutoff := tsMs - cb.cfg.WindowMs*2 // keep 2x window for safety
	pruneIdx := 0
	for pruneIdx < len(st.samples) && st.samples[pruneIdx].tsMs < cutoff {
		pruneIdx++
	}
	if pruneIdx > 0 {
		st.samples = st.samples[pruneIdx:]
	}

	// Check 1: rapid price move.
	windowCutoff := tsMs - cb.cfg.WindowMs
	for _, s := range st.samples {
		if s.tsMs >= windowCutoff && s.price > 0 {
			movePct := math.Abs(price-s.price) / s.price * 100.0
			if movePct >= cb.cfg.PriceMovePctThreshold {
				reason := "flash_crash_detected"
				st.trippedUntil = tsMs + cb.cfg.CooldownMs
				st.tripReason = reason
				log.Printf("risk/circuit_breaker: TRIPPED %s — %.2f%% move in %dms (threshold %.2f%%)",
					symbol, movePct, tsMs-s.tsMs, cb.cfg.PriceMovePctThreshold)
				return reason
			}
		}
	}

	// Check 2: abnormal bid-ask spread.
	if bid > 0 && ask > 0 {
		spreadBps := (ask - bid) / ((ask + bid) / 2) * 10000
		threshold := cb.cfg.NormalSpreadBps * cb.cfg.SpreadMultiplierThreshold
		if spreadBps > threshold {
			reason := "abnormal_spread"
			st.trippedUntil = tsMs + cb.cfg.CooldownMs
			st.tripReason = reason
			log.Printf("risk/circuit_breaker: TRIPPED %s — spread %.1f bps (threshold %.1f bps)",
				symbol, spreadBps, threshold)
			return reason
		}
	}

	return ""
}

// IsOpen returns true if the circuit is tripped for the given symbol (reject orders).
func (cb *CircuitBreaker) IsOpen(symbol string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now().UnixMilli()

	st, ok := cb.state[symbol]
	if !ok {
		return false
	}

	// Circuit is open if tripped and cooldown hasn't expired.
	if st.trippedUntil > 0 && now < st.trippedUntil {
		return true
	}

	// Circuit is open if data is stale.
	if st.lastQuoteTsMs > 0 && now-st.lastQuoteTsMs > cb.cfg.StaleQuoteMs {
		return true
	}

	return false
}

// IsOpenAny returns true if any tracked symbol has a tripped circuit.
func (cb *CircuitBreaker) IsOpenAny() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now().UnixMilli()
	for _, st := range cb.state {
		if st.trippedUntil > 0 && now < st.trippedUntil {
			return true
		}
		if st.lastQuoteTsMs > 0 && now-st.lastQuoteTsMs > cb.cfg.StaleQuoteMs {
			return true
		}
	}
	return false
}

// TripReason returns the reason the circuit was last tripped for the symbol.
func (cb *CircuitBreaker) TripReason(symbol string) string {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st, ok := cb.state[symbol]
	if !ok {
		return ""
	}
	return st.tripReason
}

// Reset manually clears the tripped state and price history for a symbol.
func (cb *CircuitBreaker) Reset(symbol string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if st, ok := cb.state[symbol]; ok {
		st.trippedUntil = 0
		st.tripReason = ""
		st.samples = st.samples[:0]
	}
}
