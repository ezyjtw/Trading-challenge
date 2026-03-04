package risk

import (
	"testing"
	"time"
)

func TestCircuitBreaker_Normal(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now().UnixMilli()

	// Normal price updates — no trip.
	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, now)
	cb.RecordQuote("BTCUSDT", 50010, 50009, 50011, now+1000)
	cb.RecordQuote("BTCUSDT", 50020, 50019, 50021, now+2000)

	if cb.IsOpen("BTCUSDT") {
		t.Fatal("circuit should not be open for normal price action")
	}
}

func TestCircuitBreaker_FlashCrash(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  60000,
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           1.0,
		CooldownMs:               30000,
		StaleQuoteMs:              5000,
	})
	now := time.Now().UnixMilli()

	// Record a normal price.
	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, now)

	// 6% crash within the window.
	reason := cb.RecordQuote("BTCUSDT", 47000, 46999, 47001, now+5000)
	if reason == "" {
		t.Fatal("expected circuit breaker to trip on 6% crash")
	}
	if reason != "flash_crash_detected" {
		t.Fatalf("expected flash_crash_detected, got %s", reason)
	}
	if !cb.IsOpen("BTCUSDT") {
		t.Fatal("circuit should be open after flash crash")
	}
}

func TestCircuitBreaker_AbnormalSpread(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  60000,
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           1.0,
		CooldownMs:               30000,
		StaleQuoteMs:              5000,
	})
	now := time.Now().UnixMilli()

	// Bid-ask spread of 50 bps (bid=50000, ask=50250) — way above 3 bps threshold.
	reason := cb.RecordQuote("BTCUSDT", 50125, 50000, 50250, now)
	if reason != "abnormal_spread" {
		t.Fatalf("expected abnormal_spread, got %q", reason)
	}
}

func TestCircuitBreaker_CooldownExpiry(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  10, // 10ms window so old samples expire fast
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           10.0,
		CooldownMs:               1,     // 1ms cooldown for testing
		StaleQuoteMs:              60000, // long stale threshold so that doesn't trip
	})
	now := time.Now().UnixMilli()

	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, now)
	cb.RecordQuote("BTCUSDT", 47000, 46999, 47001, now+5) // trip

	// Wait for both cooldown AND window expiry so old samples are pruned.
	time.Sleep(50 * time.Millisecond)
	freshTs := time.Now().UnixMilli()
	// Fresh quote at the new price — old samples outside 10ms window get pruned.
	cb.RecordQuote("BTCUSDT", 47100, 47099, 47101, freshTs)

	if cb.IsOpen("BTCUSDT") {
		t.Fatal("circuit should be closed after cooldown expiry with fresh quote")
	}
}

func TestCircuitBreaker_StaleData(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  60000,
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           1.0,
		CooldownMs:               30000,
		StaleQuoteMs:              100, // 100ms stale threshold for testing
	})

	// Record a quote in the past.
	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, time.Now().UnixMilli()-200)

	// Data is now stale.
	time.Sleep(5 * time.Millisecond)
	if !cb.IsOpen("BTCUSDT") {
		t.Fatal("circuit should be open when data is stale")
	}
}

func TestCircuitBreaker_Reset(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	now := time.Now().UnixMilli()

	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, now)
	cb.RecordQuote("BTCUSDT", 47000, 46999, 47001, now+100) // trip

	cb.Reset("BTCUSDT")
	// After reset + fresh quote, circuit should be closed.
	cb.RecordQuote("BTCUSDT", 47100, 47099, 47101, time.Now().UnixMilli())
	if cb.IsOpen("BTCUSDT") {
		t.Fatal("circuit should be closed after manual reset")
	}
}

func TestCircuitBreaker_IsOpenAny(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cfg.NormalSpreadBps = 10.0 // wider normal spread to avoid false triggers
	cb := NewCircuitBreaker(cfg)
	now := time.Now().UnixMilli()

	// Not tracking any symbol.
	if cb.IsOpenAny() {
		t.Fatal("should not be open when nothing tracked")
	}

	// Use tight spreads to avoid triggering the spread check.
	cb.RecordQuote("BTCUSDT", 50000, 49999, 50001, now)
	cb.RecordQuote("ETHUSDT", 3000, 2999.5, 3000.5, now)

	// Normal — not open.
	if cb.IsOpenAny() {
		t.Fatal("should not be open with normal quotes")
	}

	// Trip one with >5% price move.
	cb.RecordQuote("ETHUSDT", 2800, 2799.5, 2800.5, now+100)

	if !cb.IsOpenAny() {
		t.Fatal("should be open when one symbol is tripped")
	}
}
