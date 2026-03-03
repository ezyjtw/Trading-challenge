package challenge

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dec(v float64) decimal.Decimal { return decimal.NewFromFloat(v) }

func testConfig() *Config {
	return &Config{
		Enabled:                     true,
		Mode:                        ModePhase1,
		InitialBalance:              dec(100000),
		DailyDrawdownSoftPct:        dec(3),
		DailyDrawdownHardPct:        dec(4),
		DailyDrawdownBreachPct:      dec(5),
		OverallDrawdownSoftPct:      dec(7),
		OverallDrawdownHardPct:      dec(8),
		OverallDrawdownBreachPct:    dec(10),
		MaxRiskPerTradePct:          dec(3),
		ProfitDistributionMaxPct:    dec(40),
		MinTradingDays:              5,
		StopLossDeadlineSeconds:     300,
		StopLossDefaultPct:          dec(2),
		FundedMaxMarginExposurePct:  dec(25),
		FundedMaxNotionalMultiplier: dec(2),
		SafetyTradeTimeUTC:          "22:00",
		SafetyTradeSizeUSD:          dec(10),
	}
}

// ---------------------------------------------------------------------------
// Daily Drawdown
// ---------------------------------------------------------------------------

func TestCheckDailyDrawdown_NoDrawdown(t *testing.T) {
	cfg := testConfig()
	res, err := CheckDailyDrawdown(dec(100000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Level != DrawdownNormal {
		t.Fatalf("expected NORMAL, got %s", res.Level)
	}
}

func TestCheckDailyDrawdown_EquityAboveHighWater(t *testing.T) {
	cfg := testConfig()
	res, err := CheckDailyDrawdown(dec(105000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Level != DrawdownNormal {
		t.Fatalf("expected NORMAL, got %s", res.Level)
	}
	if !res.DrawdownPct.IsZero() {
		t.Fatalf("expected 0%% drawdown, got %s", res.DrawdownPct)
	}
}

func TestCheckDailyDrawdown_SoftHalt(t *testing.T) {
	cfg := testConfig()
	// 3% drawdown from 100k = 97k
	res, err := CheckDailyDrawdown(dec(97000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for soft halt")
	}
	if res.Level != DrawdownSoft {
		t.Fatalf("expected SOFT_HALT, got %s", res.Level)
	}
}

func TestCheckDailyDrawdown_HardHalt(t *testing.T) {
	cfg := testConfig()
	// 4% drawdown from 100k = 96k
	res, err := CheckDailyDrawdown(dec(96000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for hard halt")
	}
	if res.Level != DrawdownHard {
		t.Fatalf("expected HARD_HALT, got %s", res.Level)
	}
}

func TestCheckDailyDrawdown_Breach(t *testing.T) {
	cfg := testConfig()
	// 5% drawdown from 100k = 95k
	res, err := CheckDailyDrawdown(dec(95000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for breach")
	}
	if res.Level != DrawdownBreach {
		t.Fatalf("expected BREACH, got %s", res.Level)
	}
}

func TestCheckDailyDrawdown_ZeroHighWater(t *testing.T) {
	cfg := testConfig()
	res, err := CheckDailyDrawdown(dec(1000), decimal.Zero, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Level != DrawdownNormal {
		t.Fatalf("expected NORMAL with zero high-water, got %s", res.Level)
	}
}

// ---------------------------------------------------------------------------
// Overall Drawdown
// ---------------------------------------------------------------------------

func TestCheckOverallDrawdown_NoDrawdown(t *testing.T) {
	cfg := testConfig()
	res, err := CheckOverallDrawdown(dec(100000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Level != DrawdownNormal {
		t.Fatalf("expected NORMAL, got %s", res.Level)
	}
}

func TestCheckOverallDrawdown_SoftHalt(t *testing.T) {
	cfg := testConfig()
	// 7% from 100k = 93k
	res, err := CheckOverallDrawdown(dec(93000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for soft halt")
	}
	if res.Level != DrawdownSoft {
		t.Fatalf("expected SOFT_HALT, got %s", res.Level)
	}
}

func TestCheckOverallDrawdown_Breach(t *testing.T) {
	cfg := testConfig()
	// 10% from 100k = 90k
	res, err := CheckOverallDrawdown(dec(90000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for breach")
	}
	if res.Level != DrawdownBreach {
		t.Fatalf("expected BREACH, got %s", res.Level)
	}
}

func TestCheckOverallDrawdown_EquityAboveInitial(t *testing.T) {
	cfg := testConfig()
	res, err := CheckOverallDrawdown(dec(110000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Level != DrawdownNormal {
		t.Fatalf("expected NORMAL, got %s", res.Level)
	}
}

// ---------------------------------------------------------------------------
// Per-Trade Risk
// ---------------------------------------------------------------------------

func TestCheckPerTradeRisk_WithinLimits(t *testing.T) {
	cfg := testConfig()
	// max risk = 100000 * 3% = 3000
	// risk = 100 * 20 = 2000 < 3000 → OK
	err := CheckPerTradeRisk(dec(100), dec(20), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckPerTradeRisk_ExceedsLimit(t *testing.T) {
	cfg := testConfig()
	// risk = 200 * 20 = 4000 > 3000 → VIOLATION
	err := CheckPerTradeRisk(dec(200), dec(20), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for per-trade risk violation")
	}
}

func TestCheckPerTradeRisk_NegativeStopDistance(t *testing.T) {
	cfg := testConfig()
	err := CheckPerTradeRisk(dec(-10), dec(20), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for negative stop distance")
	}
}

func TestCheckPerTradeRisk_ZeroPositionSize(t *testing.T) {
	cfg := testConfig()
	err := CheckPerTradeRisk(dec(100), decimal.Zero, dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for zero position size")
	}
}

// ---------------------------------------------------------------------------
// Profit Distribution
// ---------------------------------------------------------------------------

func TestCheckProfitDistribution_WithinLimits(t *testing.T) {
	cfg := testConfig()
	// tradePnL=100, totalProfit=1000 → new total = 1100, pct = 100/1100 ≈ 9.1% < 40%
	err := CheckProfitDistribution(dec(100), dec(1000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProfitDistribution_ExceedsLimit(t *testing.T) {
	cfg := testConfig()
	// tradePnL=500, totalProfit=100 → new total = 600, pct = 500/600 ≈ 83% > 40%
	err := CheckProfitDistribution(dec(500), dec(100), cfg)
	if err == nil {
		t.Fatal("expected error for profit distribution violation")
	}
}

func TestCheckProfitDistribution_NegativePnL(t *testing.T) {
	cfg := testConfig()
	// Negative PnL should be ignored.
	err := CheckProfitDistribution(dec(-100), dec(1000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProfitDistribution_FundedMode(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	// In funded mode, rule does not apply.
	err := CheckProfitDistribution(dec(500), dec(100), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Exposure Limits (Funded Mode)
// ---------------------------------------------------------------------------

func TestCheckMaxExposure_NotFunded(t *testing.T) {
	cfg := testConfig()
	// In phase1, rule does not apply.
	err := CheckMaxExposure(dec(50000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckMaxExposure_Funded_WithinLimits(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	// max margin = 100000 * 25% = 25000; totalMargin = 20000 → OK
	err := CheckMaxExposure(dec(20000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckMaxExposure_Funded_ExceedsLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	// max margin = 100000 * 25% = 25000; totalMargin = 30000 → VIOLATION
	err := CheckMaxExposure(dec(30000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for exposure violation")
	}
}

func TestCheckMaxNotional_NotFunded(t *testing.T) {
	cfg := testConfig()
	err := CheckMaxNotional(dec(300000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckMaxNotional_Funded_WithinLimits(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	// max = 100000 * 2 = 200000; notional = 150000 → OK
	err := CheckMaxNotional(dec(150000), dec(100000), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckMaxNotional_Funded_ExceedsLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = ModeFunded
	// max = 100000 * 2 = 200000; notional = 250000 → VIOLATION
	err := CheckMaxNotional(dec(250000), dec(100000), cfg)
	if err == nil {
		t.Fatal("expected error for notional violation")
	}
}

// ---------------------------------------------------------------------------
// Stop-Loss Required
// ---------------------------------------------------------------------------

func TestCheckStopLossRequired(t *testing.T) {
	cfg := testConfig()
	openTime := time.Now().UTC()
	info := CheckStopLossRequired(openTime, cfg)
	expectedDeadline := openTime.Add(300 * time.Second)
	if info.Deadline.Sub(expectedDeadline) > time.Second {
		t.Fatalf("deadline mismatch: got %v, expected %v", info.Deadline, expectedDeadline)
	}
	if !info.DefaultStopPct.Equal(dec(2)) {
		t.Fatalf("default stop pct mismatch: got %s, expected 2", info.DefaultStopPct)
	}
}
