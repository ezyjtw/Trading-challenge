package scanner

import (
	"testing"
)

func TestComputeRiskParams_Basic(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	params, err := rm.ComputeRiskParams(Long, 50000, 250, 100000, 100000, 3, 49500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SL should be below entry for long.
	if params.StopLoss >= params.Entry {
		t.Errorf("SL %.2f should be below entry %.2f for long", params.StopLoss, params.Entry)
	}

	// TP1 should be above entry for long.
	if params.TakeProfit1 <= params.Entry {
		t.Errorf("TP1 %.2f should be above entry %.2f for long", params.TakeProfit1, params.Entry)
	}

	// TP2 > TP1.
	if params.TakeProfit2 <= params.TakeProfit1 {
		t.Errorf("TP2 %.2f should be above TP1 %.2f", params.TakeProfit2, params.TakeProfit1)
	}

	// Risk should not exceed 3% of initial balance.
	maxRisk := 100000 * 0.03
	if params.RiskUSD > maxRisk {
		t.Errorf("risk $%.2f exceeds max $%.2f", params.RiskUSD, maxRisk)
	}

	// Risk% should be reasonable.
	if params.RiskPct > 3.0 {
		t.Errorf("risk pct %.2f%% exceeds 3%%", params.RiskPct)
	}

	t.Logf("Entry: $%.2f, SL: $%.2f, TP1: $%.2f, Size: $%.0f, Risk: $%.2f (%.1f%%)",
		params.Entry, params.StopLoss, params.TakeProfit1, params.PositionSizeUSD,
		params.RiskUSD, params.RiskPct)
}

func TestComputeRiskParams_Short(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	params, err := rm.ComputeRiskParams(Short, 3000, 30, 100000, 100000, 4, 3100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SL should be above entry for short.
	if params.StopLoss <= params.Entry {
		t.Errorf("SL %.2f should be above entry %.2f for short", params.StopLoss, params.Entry)
	}

	// TP1 should be below entry for short.
	if params.TakeProfit1 >= params.Entry {
		t.Errorf("TP1 %.2f should be below entry %.2f for short", params.TakeProfit1, params.Entry)
	}
}

func TestComputeRiskParams_ConfluenceScaling(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	// 2 confluences should give smaller position than 5.
	p2, _ := rm.ComputeRiskParams(Long, 50000, 250, 100000, 100000, 2, 0)
	p5, _ := rm.ComputeRiskParams(Long, 50000, 250, 100000, 100000, 5, 0)

	if p2.PositionSizeUSD >= p5.PositionSizeUSD {
		t.Errorf("2 confluences ($%.0f) should have smaller position than 5 ($%.0f)",
			p2.PositionSizeUSD, p5.PositionSizeUSD)
	}
}

func TestComputeRiskParams_HyroTraderCap(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	// Even with high confluence, risk should never exceed 3% of initial.
	params, _ := rm.ComputeRiskParams(Long, 100, 5, 100000, 100000, 6, 0)

	maxRisk := 100000 * 0.03
	if params.RiskUSD > maxRisk*1.01 { // 1% tolerance for rounding
		t.Errorf("risk $%.2f exceeds HyroTrader cap $%.2f", params.RiskUSD, maxRisk)
	}
}

func TestValidateForTradingDay(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	// $6000 trade on $100K account = 6% > 5% threshold.
	ok, reason := rm.ValidateForTradingDay(6000, 100000)
	if !ok {
		t.Errorf("expected to qualify: %s", reason)
	}

	// $4000 trade on $100K account = 4% < 5% threshold.
	ok, reason = rm.ValidateForTradingDay(4000, 100000)
	if ok {
		t.Errorf("expected not to qualify, got: %s", reason)
	}
}

func TestCheckDailyRiskBudget(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	// No losses yet, $1000 pending risk on $100K = well within 4% budget.
	ok, _ := rm.CheckDailyRiskBudget(0, 100000, 1000)
	if !ok {
		t.Error("should have budget available")
	}

	// Already lost $3500 on $100K, trying to risk another $1000.
	ok, reason := rm.CheckDailyRiskBudget(-3500, 100000, 1000)
	if ok {
		t.Errorf("should NOT have budget: %s", reason)
	}
}

func TestSuggestSLType(t *testing.T) {
	rm := NewRiskManager(DefaultRiskConfig())

	// Challenge mode: always market.
	if rm.SuggestSLType(true, 1e12) != "MARKET" {
		t.Error("challenge mode should always use MARKET SL")
	}

	// Funded mode, high volume: limit ok.
	if rm.SuggestSLType(false, 1e9) != "LIMIT" {
		t.Error("funded + high volume should use LIMIT SL")
	}

	// Funded mode, low volume: market.
	if rm.SuggestSLType(false, 1e6) != "MARKET" {
		t.Error("funded + low volume should use MARKET SL")
	}
}
