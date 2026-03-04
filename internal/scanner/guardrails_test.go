package scanner

import (
	"testing"
)

func TestCheckPreTrade_AllClear(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	ok, reason := g.CheckPreTrade("test-alert-1")
	if !ok {
		t.Errorf("expected clear, got blocked: %s", reason)
	}
}

func TestCheckPreTrade_MaxTrades(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	// Open 3 trades (the max).
	g.RecordTradeOpened("a1")
	g.RecordTradeOpened("a2")
	g.RecordTradeOpened("a3")

	ok, reason := g.CheckPreTrade("a4")
	if ok {
		t.Errorf("should be blocked after %d trades: %s", g.cfg.MaxTradesPerDay, reason)
	}
}

func TestCheckPreTrade_ConsecutiveLosses(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	g.RecordTradeOpened("a1")
	g.RecordTradeResult("a1", -1.0) // loss
	g.RecordTradeOpened("a2")
	g.RecordTradeResult("a2", -1.0) // second consecutive loss

	ok, reason := g.CheckPreTrade("a3")
	if ok {
		t.Errorf("should be blocked after 2 consecutive losses: %s", reason)
	}
}

func TestCheckPreTrade_ConsecutiveLossesReset(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	g.RecordTradeOpened("a1")
	g.RecordTradeResult("a1", -1.0) // loss
	g.RecordTradeOpened("a2")
	g.RecordTradeResult("a2", 2.0) // win resets consecutive losses

	ok, _ := g.CheckPreTrade("a3")
	if !ok {
		t.Error("consecutive losses should have reset after a win")
	}
}

func TestCheckPreTrade_DailyPnLStop(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	// Lose 1.5R which hits the -1.5R daily stop.
	g.RecordTradeOpened("a1")
	g.RecordTradeResult("a1", -1.5)

	ok, reason := g.CheckPreTrade("a2")
	if ok {
		t.Errorf("should be blocked at daily PnL stop: %s", reason)
	}
}

func TestCheckPreTrade_Halted(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	g.Halt("manual review required")

	ok, reason := g.CheckPreTrade("a1")
	if ok {
		t.Errorf("should be blocked when halted: %s", reason)
	}

	g.Resume()

	ok, _ = g.CheckPreTrade("a1")
	if !ok {
		t.Error("should be clear after resume")
	}
}

func TestCheckPreTrade_Cooldown(t *testing.T) {
	cfg := DefaultGuardrailConfig()
	cfg.CooldownAfterAlertMs = 60000 // 60 seconds
	g := NewGuardrails(cfg)

	g.RegisterAlertReceived("a1")

	ok, reason := g.CheckPreTrade("a1")
	if ok {
		t.Errorf("should be blocked during cooldown: %s", reason)
	}
}

func TestDailyStatus(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	status := g.DailyStatus()
	if !status.CanTrade {
		t.Error("should be able to trade initially")
	}
	if status.TradesToday != 0 {
		t.Errorf("expected 0 trades, got %d", status.TradesToday)
	}
	if status.MaxTrades != 3 {
		t.Errorf("expected max 3 trades, got %d", status.MaxTrades)
	}

	g.RecordTradeOpened("a1")
	status = g.DailyStatus()
	if status.TradesToday != 1 {
		t.Errorf("expected 1 trade, got %d", status.TradesToday)
	}
}

func TestCheckPerformanceHealth(t *testing.T) {
	g := NewGuardrails(DefaultGuardrailConfig())

	// Not enough data.
	ok, _ := g.CheckPerformanceHealth(PerformanceStats{TotalTrades: 5, WinRate: 20})
	if !ok {
		t.Error("should pass with insufficient data")
	}

	// Enough data, low win rate.
	ok, reason := g.CheckPerformanceHealth(PerformanceStats{TotalTrades: 25, WinRate: 20})
	if ok {
		t.Errorf("should fail with 20%% win rate: %s", reason)
	}

	// Enough data, healthy win rate.
	ok, _ = g.CheckPerformanceHealth(PerformanceStats{TotalTrades: 25, WinRate: 45})
	if !ok {
		t.Error("should pass with 45% win rate")
	}
}

func TestFormatDailyStatus(t *testing.T) {
	state := DailyState{
		TradesToday:       1,
		MaxTrades:         3,
		ConsecutiveLosses: 0,
		MaxConsecLosses:   2,
		DailyPnLR:         0.5,
		DailyStopR:        -1.5,
		OpenTrades:        1,
		CanTrade:          true,
		Reason:            "ready",
		Date:              "2026-03-04",
	}

	msg := FormatDailyStatus(state)
	if msg == "" {
		t.Error("status message should not be empty")
	}
}
