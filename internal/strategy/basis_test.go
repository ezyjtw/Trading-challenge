package strategy

import (
	"math"
	"testing"
)

func TestBasisTracker_RecordAndStats(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 100, MinSamples: 3})

	// Record some samples: perp at slight premium to spot.
	bt.Record("BTCUSDT", 50000, 50050, 1000) // basis = 0.001 (0.1%)
	bt.Record("BTCUSDT", 50000, 50025, 2000) // basis = 0.0005 (0.05%)
	bt.Record("BTCUSDT", 50000, 50075, 3000) // basis = 0.0015 (0.15%)

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
		bt.Record("ETHUSDT", 3000, 3000+float64(i), int64(i*1000))
	}

	_, _, count := bt.Stats("ETHUSDT")
	if count != 5 {
		t.Fatalf("expected 5 samples after pruning, got %d", count)
	}
}

func TestBasisTracker_ZScore(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 200, MinSamples: 5})

	// Add many samples at a stable basis of 0.1%.
	for i := 0; i < 100; i++ {
		bt.Record("BTCUSDT", 50000, 50050, int64(i*1000))
	}

	// Z-score should be near 0 since current = mean.
	z := bt.ZScore("BTCUSDT")
	if math.Abs(z) > 0.01 {
		t.Fatalf("expected z-score near 0 for stable basis, got %.4f", z)
	}
}

func TestBasisTracker_ZScore_Extended(t *testing.T) {
	bt := NewBasisTracker(BasisConfig{WindowSize: 200, MinSamples: 5})

	// Add 50 samples at basis of 0.1%.
	for i := 0; i < 50; i++ {
		bt.Record("BTCUSDT", 50000, 50050, int64(i*1000))
	}

	// Now add one sample at much higher basis (0.5%).
	bt.Record("BTCUSDT", 50000, 50250, 50000)

	z := bt.ZScore("BTCUSDT")
	if z <= 1.0 {
		t.Fatalf("expected z-score > 1.0 for extended basis, got %.4f", z)
	}
}

func TestBasisTracker_CurrentBasis(t *testing.T) {
	bt := NewBasisTracker(DefaultBasisConfig())

	// No data.
	_, ok := bt.CurrentBasis("BTCUSDT")
	if ok {
		t.Fatal("expected no current basis for untracked symbol")
	}

	bt.Record("BTCUSDT", 50000, 50100, 1000)
	basis, ok := bt.CurrentBasis("BTCUSDT")
	if !ok {
		t.Fatal("expected current basis after recording")
	}

	expected := (50100 - 50000) / 50000.0
	if math.Abs(basis-expected) > 1e-10 {
		t.Fatalf("expected basis=%.6f, got %.6f", expected, basis)
	}
}

func TestBasisTracker_ZeroSpotPrice(t *testing.T) {
	bt := NewBasisTracker(DefaultBasisConfig())

	// Should not panic or record.
	bt.Record("BTCUSDT", 0, 50000, 1000)
	_, _, count := bt.Stats("BTCUSDT")
	if count != 0 {
		t.Fatal("should not record sample with zero spot price")
	}
}
