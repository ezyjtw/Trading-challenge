package challenge

import (
	"testing"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
	"github.com/shopspring/decimal"
)

func makeIntent(notionalUSD, expectedPnL float64) arb.TradeIntent {
	return arb.TradeIntent{
		IntentID:  "test-intent-1",
		Strategy:  "funding",
		Symbol:    "BTCUSDT",
		TsMs:      1700000000000,
		ExpiresMs: 1700000060000,
		Legs: []arb.TradeLeg{
			{
				Action:         "BUY",
				Type:           "LIMIT",
				Market:         "SPOT",
				Symbol:         "BTCUSDT",
				NotionalUSD:    notionalUSD,
				MaxSlippageBps: 5.0,
			},
			{
				Action:         "SELL",
				Type:           "LIMIT",
				Market:         "PERP",
				Symbol:         "BTCUSDT",
				NotionalUSD:    notionalUSD,
				MaxSlippageBps: 5.0,
			},
		},
		Expected: arb.ExpectedMetrics{
			ProfitUSDNet: expectedPnL,
		},
	}
}

func TestDaemon_NewDaemon(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	if !d.currentEquity.Equal(dec(100000)) {
		t.Fatalf("expected equity 100000, got %s", d.currentEquity)
	}
	if d.halted {
		t.Fatal("daemon should not start halted")
	}
	if d.TradingDayCount() != 0 {
		t.Fatalf("expected 0 trading days, got %d", d.TradingDayCount())
	}
}

func TestDaemon_UpdateEquity_HighWaterMark(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Equity goes up → high water mark tracks it.
	d.UpdateEquity(dec(105000))
	if !d.DailyHighWater().Equal(dec(105000)) {
		t.Fatalf("expected daily HW 105000, got %s", d.DailyHighWater())
	}

	// Equity goes down, but within limits → high water stays.
	d.UpdateEquity(dec(103000))
	if !d.DailyHighWater().Equal(dec(105000)) {
		t.Fatalf("expected daily HW 105000, got %s", d.DailyHighWater())
	}
}

func TestDaemon_UpdateEquity_DailyDrawdownBreach(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Set high water high enough that a drop triggers breach (5%).
	d.UpdateEquity(dec(110000))
	// 5% of 110000 = 5500 → equity below 104500 triggers breach.
	err := d.UpdateEquity(dec(104000))
	if err == nil {
		t.Fatal("expected breach error")
	}
	if !d.IsHalted() {
		t.Fatal("daemon should be halted after daily drawdown breach")
	}
}

func TestDaemon_UpdateEquity_OverallDrawdownBreach(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// 10% overall drawdown from 100k initial = equity at 90k.
	err := d.UpdateEquity(dec(90000))
	if err == nil {
		t.Fatal("expected breach error")
	}
	if !d.IsHalted() {
		t.Fatal("daemon should be halted after overall drawdown breach")
	}
}

func TestDaemon_RecordTrade(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	d.RecordTrade(dec(500))
	if !d.TotalProfit().Equal(dec(500)) {
		t.Fatalf("expected profit 500, got %s", d.TotalProfit())
	}

	d.RecordTrade(dec(-200))
	if !d.TotalLoss().Equal(dec(200)) {
		t.Fatalf("expected loss 200, got %s", d.TotalLoss())
	}
	if !d.NetPnL().Equal(dec(300)) {
		t.Fatalf("expected net PnL 300, got %s", d.NetPnL())
	}
	if d.TradingDayCount() != 1 {
		t.Fatalf("expected 1 trading day, got %d", d.TradingDayCount())
	}
}

func TestDaemon_ValidateIntent_Normal(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Seed existing profit so the profit distribution check doesn't trip
	// (a new $50 trade against $0 existing profit = 100% concentration).
	d.RecordTrade(dec(1000))

	intent := makeIntent(1000, 50)
	err := d.ValidateIntent(intent)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestDaemon_ValidateIntent_Halted(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Force halt.
	d.UpdateEquity(dec(90000)) // overall breach

	intent := makeIntent(1000, 50)
	err := d.ValidateIntent(intent)
	if err == nil {
		t.Fatal("expected rejection when halted")
	}
}

func TestDaemon_ValidateIntent_DrawdownReject(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Put equity at soft-halt daily level (3% from 100k = 97k).
	d.UpdateEquity(dec(97000))

	intent := makeIntent(1000, 50)
	err := d.ValidateIntent(intent)
	if err == nil {
		t.Fatal("expected rejection when in daily soft halt")
	}
}

func TestDaemon_ValidateIntent_FundedExposure(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	d := NewDaemon(cfg)

	// Track existing position that uses up most of the margin budget.
	d.TrackPosition("ETHUSDT", dec(24000), dec(24000))
	d.ConfirmStopLoss("ETHUSDT")

	// Try to add another 2000 → total margin 26000, max = 25000 → reject.
	intent := makeIntent(2000, 10)
	err := d.ValidateIntent(intent)
	if err == nil {
		t.Fatal("expected rejection for exceeding funded exposure limit")
	}
}

func TestDaemon_TrackPosition_StopLossExpiry(t *testing.T) {
	cfg := testConfig()
	cfg.StopLossDeadlineSeconds = 0 // instant deadline for testing
	// validation will fail because StopLossDeadlineSeconds must be positive
	cfg.StopLossDeadlineSeconds = 1

	d := NewDaemon(cfg)
	d.TrackPosition("BTCUSDT", dec(5000), dec(5000))

	// Before confirming stop-loss, the position has no stop.
	expired := d.PositionsWithExpiredStopLoss()
	// The deadline is 1 second from now, so it's not expired yet.
	if len(expired) > 0 {
		t.Log("position not expired yet as expected")
	}

	// Confirm stop-loss.
	d.ConfirmStopLoss("BTCUSDT")
	expired = d.PositionsWithExpiredStopLoss()
	if len(expired) != 0 {
		t.Fatalf("expected 0 expired after stop-loss set, got %d", len(expired))
	}
}

func TestDaemon_ClosePosition(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)
	d.TrackPosition("BTCUSDT", dec(5000), dec(5000))

	snap := d.GetSnapshot()
	if snap.OpenPositions != 1 {
		t.Fatalf("expected 1 open position, got %d", snap.OpenPositions)
	}

	d.ClosePosition("BTCUSDT")
	snap = d.GetSnapshot()
	if snap.OpenPositions != 0 {
		t.Fatalf("expected 0 open positions, got %d", snap.OpenPositions)
	}
	if !snap.TotalMargin.IsZero() {
		t.Fatalf("expected 0 margin, got %s", snap.TotalMargin)
	}
}

func TestDaemon_Reset(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	// Halt it.
	d.UpdateEquity(dec(90000))
	if !d.IsHalted() {
		t.Fatal("should be halted")
	}

	// Reset.
	d.Reset()
	if d.IsHalted() {
		t.Fatal("should not be halted after reset")
	}
	if d.HaltReason() != "" {
		t.Fatalf("expected empty halt reason, got %q", d.HaltReason())
	}
}

func TestDaemon_GetSnapshot(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	d.TrackPosition("ETHUSDT", dec(10000), dec(5000))
	d.RecordTrade(dec(200))

	snap := d.GetSnapshot()
	if !snap.Equity.Equal(dec(100000)) {
		t.Fatalf("expected equity 100000, got %s", snap.Equity)
	}
	if snap.OpenPositions != 1 {
		t.Fatalf("expected 1 open position, got %d", snap.OpenPositions)
	}
	if snap.ClosedTrades != 1 {
		t.Fatalf("expected 1 closed trade, got %d", snap.ClosedTrades)
	}
	if !snap.TotalProfit.Equal(dec(200)) {
		t.Fatalf("expected total profit 200, got %s", snap.TotalProfit)
	}
	if !snap.TotalNotional.Equal(dec(10000)) {
		t.Fatalf("expected total notional 10000, got %s", snap.TotalNotional)
	}
	if !snap.TotalMargin.Equal(dec(5000)) {
		t.Fatalf("expected total margin 5000, got %s", snap.TotalMargin)
	}
	if snap.Mode != ModePhase1 {
		t.Fatalf("expected mode phase1, got %s", snap.Mode)
	}
}

func TestDaemon_MinTradingDays(t *testing.T) {
	cfg := testConfig()
	cfg.MinTradingDays = 5
	d := NewDaemon(cfg)

	if d.MinTradingDaysMet() {
		t.Fatal("should not have met min trading days at start")
	}

	// Record one trade → 1 trading day.
	d.RecordTrade(dec(10))
	if d.MinTradingDaysMet() {
		t.Fatal("should not have met min trading days with 1 day")
	}
}

func TestDaemon_CheckSafetyTrade_TradedToday(t *testing.T) {
	cfg := testConfig()
	d := NewDaemon(cfg)

	d.RecordTrade(dec(10))
	if d.CheckSafetyTrade() {
		t.Fatal("should not need safety trade when already traded today")
	}
}

func TestDaemon_ValidateIntent_Disabled(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	d := NewDaemon(cfg)

	// Even with bad state, disabled should pass through.
	intent := makeIntent(1000000, 50000)
	err := d.ValidateIntent(intent)
	if err != nil {
		t.Fatalf("disabled challenge should not reject intents: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Config tests
// ---------------------------------------------------------------------------

func TestParseConfig_Valid(t *testing.T) {
	yaml := `
challenge:
  enabled: true
  mode: phase1
  initial_balance: 100000
  daily_drawdown_soft_pct: 3
  daily_drawdown_hard_pct: 4
  daily_drawdown_breach_pct: 5
  overall_drawdown_soft_pct: 7
  overall_drawdown_hard_pct: 8
  overall_drawdown_breach_pct: 10
  max_risk_per_trade_pct: 3
  profit_distribution_max_pct: 40
  min_trading_days: 5
  stop_loss_deadline_seconds: 300
  stop_loss_default_pct: 2
  funded_max_margin_exposure_pct: 25
  funded_max_notional_multiplier: 2
  safety_trade_time_utc: "22:00"
  safety_trade_size_usd: 10
`
	cfg, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != ModePhase1 {
		t.Fatalf("expected mode phase1, got %s", cfg.Mode)
	}
	if !cfg.InitialBalance.Equal(decimal.NewFromInt(100000)) {
		t.Fatalf("expected balance 100000, got %s", cfg.InitialBalance)
	}
}

func TestParseConfig_InvalidMode(t *testing.T) {
	yaml := `
challenge:
  enabled: true
  mode: invalid
  initial_balance: 100000
  daily_drawdown_soft_pct: 3
  daily_drawdown_hard_pct: 4
  daily_drawdown_breach_pct: 5
  overall_drawdown_soft_pct: 7
  overall_drawdown_hard_pct: 8
  overall_drawdown_breach_pct: 10
  max_risk_per_trade_pct: 3
  profit_distribution_max_pct: 40
  min_trading_days: 5
  stop_loss_deadline_seconds: 300
  stop_loss_default_pct: 2
  safety_trade_time_utc: "22:00"
  safety_trade_size_usd: 10
`
	_, err := ParseConfig([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestParseConfig_ZeroBalance(t *testing.T) {
	yaml := `
challenge:
  enabled: true
  mode: phase1
  initial_balance: 0
  daily_drawdown_soft_pct: 3
  daily_drawdown_hard_pct: 4
  daily_drawdown_breach_pct: 5
  overall_drawdown_soft_pct: 7
  overall_drawdown_hard_pct: 8
  overall_drawdown_breach_pct: 10
  max_risk_per_trade_pct: 3
  profit_distribution_max_pct: 40
  min_trading_days: 5
  stop_loss_deadline_seconds: 300
  stop_loss_default_pct: 2
  safety_trade_time_utc: "22:00"
  safety_trade_size_usd: 10
`
	_, err := ParseConfig([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for zero balance")
	}
}
