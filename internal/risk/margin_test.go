package risk

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dec(v float64) decimal.Decimal { return decimal.NewFromFloat(v) }

func TestMarginMonitor_Normal(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	alerts := m.UpdateMargin(MarginSnapshot{
		TotalEquity:     dec(100000),
		TotalMarginUsed: dec(30000), // 30% < 60% warn threshold
		AvailableMargin: dec(70000),
		TsMs:            time.Now().UnixMilli(),
	})
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts, got %d", len(alerts))
	}
}

func TestMarginMonitor_Warn(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	alerts := m.UpdateMargin(MarginSnapshot{
		TotalEquity:     dec(100000),
		TotalMarginUsed: dec(65000), // 65% > 60% warn
		AvailableMargin: dec(35000),
		TsMs:            time.Now().UnixMilli(),
	})
	if len(alerts) != 1 || alerts[0].Level != "WARN" {
		t.Fatalf("expected WARN alert, got %v", alerts)
	}
}

func TestMarginMonitor_Critical(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	alerts := m.UpdateMargin(MarginSnapshot{
		TotalEquity:     dec(100000),
		TotalMarginUsed: dec(80000), // 80% > 75% critical
		AvailableMargin: dec(20000),
		TsMs:            time.Now().UnixMilli(),
	})
	if len(alerts) != 1 || alerts[0].Level != "CRITICAL" {
		t.Fatalf("expected CRITICAL alert, got %v", alerts)
	}
	if !m.NeedsReducePosition() {
		t.Fatal("expected NeedsReducePosition to be true")
	}
}

func TestMarginMonitor_Halt(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	alerts := m.UpdateMargin(MarginSnapshot{
		TotalEquity:     dec(100000),
		TotalMarginUsed: dec(90000), // 90% > 85% halt
		AvailableMargin: dec(10000),
		TsMs:            time.Now().UnixMilli(),
	})
	if len(alerts) != 1 || alerts[0].Level != "HALT" {
		t.Fatalf("expected HALT alert, got %v", alerts)
	}
	if !m.NeedsHalt() {
		t.Fatal("expected NeedsHalt to be true")
	}
}

func TestMarginMonitor_LiqProximity(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	// Mark price 50000, liq price 48000 → buffer = 4% < 5% threshold.
	alerts := m.UpdatePositions([]PositionMarginInfo{
		{Symbol: "BTCUSDT", MarkPrice: dec(50000), LiqPrice: dec(48000), Side: "Sell"},
	})
	if len(alerts) != 1 || alerts[0].Level != "CRITICAL" {
		t.Fatalf("expected CRITICAL liq proximity alert, got %v", alerts)
	}
}

func TestMarginMonitor_LiqProximity_Safe(t *testing.T) {
	m := NewMarginMonitor(DefaultMarginConfig())
	// Mark price 50000, liq price 45000 → buffer = 10% > 5% threshold.
	alerts := m.UpdatePositions([]PositionMarginInfo{
		{Symbol: "BTCUSDT", MarkPrice: dec(50000), LiqPrice: dec(45000), Side: "Sell"},
	})
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts, got %v", alerts)
	}
}
