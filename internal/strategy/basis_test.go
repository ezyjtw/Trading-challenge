package strategy

import (
	"math"
	"testing"
)

func TestBasisTracker_RecordAndStats(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 100, MinSamples: 3})

	// Record some samples: perp at slight premium to index.
	bt.RecordPremium("BTCUSDT", 50000, 50050, 1000) // premium = 0.001 (0.1%)
	bt.RecordPremium("BTCUSDT", 50000, 50025, 2000) // premium = 0.0005 (0.05%)
	bt.RecordPremium("BTCUSDT", 50000, 50075, 3000) // premium = 0.0015 (0.15%)

	mean, stddev, count := bt.Stats("BTCUSDT")
	if count != 3 {
		t.Fatalf("expected 3 samples, got %d", count)
	}

	expectedMean := (0.001 + 0.0005 + 0.0015) / 3.0
	if math.Abs(mean-expectedMean) > 1e-10 {
		t.Fatalf("expected mean=%.6f, got %.6f", expectedMean, mean)
	}

	if stddev <= 0 {
		t.Fatal("stddev should be positive")
	}
}

func TestBasisTracker_WindowPrune(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 5, MinSamples: 2})

	for i := 0; i < 10; i++ {
		bt.RecordPremium("ETHUSDT", 3000, 3000+float64(i), int64(i*1000))
	}

	_, _, count := bt.Stats("ETHUSDT")
	if count != 5 {
		t.Fatalf("expected 5 samples after pruning, got %d", count)
	}
}

func TestBasisTracker_ZScore(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 200, MinSamples: 5})

	// Add many samples at a stable premium of 0.1%.
	for i := 0; i < 100; i++ {
		bt.RecordPremium("BTCUSDT", 50000, 50050, int64(i*1000))
	}

	// Z-score should be near 0 since current = mean.
	z := bt.ZScore("BTCUSDT")
	if math.Abs(z) > 0.01 {
		t.Fatalf("expected z-score near 0 for stable premium, got %.4f", z)
	}
}

func TestBasisTracker_ZScore_Extended(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 200, MinSamples: 5})

	// Add 50 samples at premium of 0.1%.
	for i := 0; i < 50; i++ {
		bt.RecordPremium("BTCUSDT", 50000, 50050, int64(i*1000))
	}

	// Now add one sample at much higher premium (0.5%).
	bt.RecordPremium("BTCUSDT", 50000, 50250, 50000)

	z := bt.ZScore("BTCUSDT")
	if z <= 1.0 {
		t.Fatalf("expected z-score > 1.0 for extended premium, got %.4f", z)
	}
}

func TestBasisTracker_CurrentValue(t *testing.T) {
	bt := NewBasisTracker(DefaultBasisConfig())

	// No data.
	_, ok := bt.CurrentValue("BTCUSDT")
	if ok {
		t.Fatal("expected no current value for untracked symbol")
	}

	bt.RecordPremium("BTCUSDT", 50000, 50100, 1000)
	val, ok := bt.CurrentValue("BTCUSDT")
	if !ok {
		t.Fatal("expected current value after recording")
	}

	expected := (50100 - 50000) / 50000.0
	if math.Abs(val-expected) > 1e-10 {
		t.Fatalf("expected value=%.6f, got %.6f", expected, val)
	}
}

func TestBasisTracker_ZeroIndexPrice(t *testing.T) {
	bt := NewBasisTracker(DefaultBasisConfig())

	// Should not panic or record.
	bt.RecordPremium("BTCUSDT", 0, 50000, 1000)
	_, _, count := bt.Stats("BTCUSDT")
	if count != 0 {
		t.Fatal("should not record sample with zero index price")
	}
}

func TestBasisTracker_PairSpread(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 100, MinSamples: 3})

	// Record cross-pair spread samples.
	bt.RecordPairSpread("BTCUSDT", "ETHUSDT", 50000, 3000, 1000)
	bt.RecordPairSpread("BTCUSDT", "ETHUSDT", 50100, 3010, 2000)
	bt.RecordPairSpread("BTCUSDT", "ETHUSDT", 49900, 2990, 3000)

	mean, stddev, count := bt.PairSpreadStats("BTCUSDT", "ETHUSDT")
	if count != 3 {
		t.Fatalf("expected 3 pair spread samples, got %d", count)
	}
	if mean == 0 {
		t.Fatal("expected non-zero mean for pair spread")
	}
	if stddev < 0 {
		t.Fatal("stddev should be non-negative")
	}
}

func TestBasisTracker_PairSpreadZScore(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 200, MinSamples: 5})

	// Add stable spread samples.
	for i := 0; i < 100; i++ {
		bt.RecordPairSpread("BTCUSDT", "ETHUSDT", 50000, 3000, int64(i*1000))
	}

	z := bt.PairSpreadZScore("BTCUSDT", "ETHUSDT")
	if math.Abs(z) > 0.01 {
		t.Fatalf("expected z-score near 0 for stable spread, got %.4f", z)
	}
}
