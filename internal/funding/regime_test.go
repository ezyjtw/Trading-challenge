package funding

import (
	"testing"
	"time"
)

func TestRegimeDetector_Neutral_Insufficient(t *testing.T) {
	rd := NewRegimeDetector(DefaultRegimeConfig())
	regime := rd.DetectRegime("BTCUSDT")
	if regime != RegimeNeutral {
		t.Fatalf("expected NEUTRAL with no data, got %s", regime)
	}
}

func TestRegimeDetector_Bullish(t *testing.T) {
	rd := NewRegimeDetector(DefaultRegimeConfig())
	now := time.Now().UnixMilli()

	// 10 positive observations.
	for i := 0; i < 10; i++ {
		rd.RecordRate("BTCUSDT", 0.0003, now+int64(i)*1000)
	}

	regime := rd.DetectRegime("BTCUSDT")
	if regime != RegimeBullish {
		t.Fatalf("expected BULLISH, got %s", regime)
	}

	factor := rd.PositionScaleFactor("BTCUSDT")
	if factor != 1.0 {
		t.Fatalf("expected scale factor 1.0, got %f", factor)
	}
}

func TestRegimeDetector_Bearish(t *testing.T) {
	cfg := DefaultRegimeConfig()
	cfg.NegativeThresholdPct = 60.0
	rd := NewRegimeDetector(cfg)
	now := time.Now().UnixMilli()

	// 7 negative, 3 positive = 70% negative > 60% threshold.
	for i := 0; i < 7; i++ {
		rd.RecordRate("BTCUSDT", -0.0002, now+int64(i)*1000)
	}
	for i := 7; i < 10; i++ {
		rd.RecordRate("BTCUSDT", 0.0001, now+int64(i)*1000)
	}

	regime := rd.DetectRegime("BTCUSDT")
	if regime != RegimeBearish {
		t.Fatalf("expected BEARISH, got %s", regime)
	}

	factor := rd.PositionScaleFactor("BTCUSDT")
	if factor != 0.25 { // default ReducePositionPct = 25%
		t.Fatalf("expected scale factor 0.25, got %f", factor)
	}
}

func TestRegimeDetector_Persistent(t *testing.T) {
	cfg := DefaultRegimeConfig()
	cfg.PersistentNegativePeriods = 5
	rd := NewRegimeDetector(cfg)
	now := time.Now().UnixMilli()

	// 5 consecutive negatives at the end.
	for i := 0; i < 5; i++ {
		rd.RecordRate("BTCUSDT", -0.0003, now+int64(i)*1000)
	}

	regime := rd.DetectRegime("BTCUSDT")
	if regime != RegimePersistent {
		t.Fatalf("expected PERSISTENT, got %s", regime)
	}

	if !rd.ShouldExitAll("BTCUSDT") {
		t.Fatal("expected ShouldExitAll to be true")
	}

	factor := rd.PositionScaleFactor("BTCUSDT")
	if factor != 0.0 {
		t.Fatalf("expected scale factor 0.0, got %f", factor)
	}
}

func TestRegimeDetector_Mixed(t *testing.T) {
	cfg := DefaultRegimeConfig()
	cfg.NegativeThresholdPct = 60.0
	rd := NewRegimeDetector(cfg)
	now := time.Now().UnixMilli()

	// 5 positive, 5 negative = 50% each → neither threshold crossed.
	for i := 0; i < 5; i++ {
		rd.RecordRate("BTCUSDT", 0.0003, now+int64(i)*1000)
	}
	for i := 5; i < 10; i++ {
		rd.RecordRate("BTCUSDT", -0.0002, now+int64(i)*1000)
	}

	regime := rd.DetectRegime("BTCUSDT")
	if regime != RegimeNeutral {
		t.Fatalf("expected NEUTRAL, got %s", regime)
	}
}

func TestRegimeDetector_LookbackPrune(t *testing.T) {
	cfg := DefaultRegimeConfig()
	cfg.LookbackPeriods = 5
	rd := NewRegimeDetector(cfg)
	now := time.Now().UnixMilli()

	// Add 10 samples — should keep only last 5.
	for i := 0; i < 10; i++ {
		rd.RecordRate("BTCUSDT", 0.0003, now+int64(i)*1000)
	}

	rd.mu.Lock()
	count := len(rd.samples["BTCUSDT"])
	rd.mu.Unlock()

	if count != 5 {
		t.Fatalf("expected 5 samples after pruning, got %d", count)
	}
}
