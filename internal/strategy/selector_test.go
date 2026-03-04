package strategy

import (
	"testing"
)

func testSelectorConfig() SelectorConfig {
	cfg := DefaultSelectorConfig()
	cfg.CooldownS = 0 // no cooldown for tests
	return cfg
}

func TestSelector_FundingCarry_PositiveRate(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Positive funding rate with bullish regime.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   50050,
		FundingRate: 0.0003, // 0.03% per 8h = ~32.85% annual
		Basis:       0.001,
		BasisZScore: 0.5,
		Regime:      "BULLISH",
		TsMs:        1000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Entries) == 0 {
		t.Fatal("expected at least one entry intent for positive funding")
	}

	entry := eval.Entries[0]
	if entry.Strategy != "funding" {
		t.Fatalf("expected funding strategy, got %s", entry.Strategy)
	}
	if len(entry.Legs) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(entry.Legs))
	}
	if entry.Legs[0].Action != "BUY" || entry.Legs[0].Market != "SPOT" {
		t.Fatal("leg 0 should be BUY SPOT")
	}
	if entry.Legs[1].Action != "SELL" || entry.Legs[1].Market != "PERP" {
		t.Fatal("leg 1 should be SELL PERP")
	}
}

func TestSelector_Hold_NegativeFunding(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Negative funding, no extended basis.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   49990,
		FundingRate: -0.0001,
		Basis:       -0.0002,
		BasisZScore: -0.5,
		Regime:      "BEARISH",
		TsMs:        1000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Entries) != 0 {
		t.Fatal("expected no entries for negative funding with no basis opportunity")
	}

	// Should have a HOLD decision.
	foundHold := false
	for _, d := range eval.Decisions {
		if d.Strategy == StrategyHold {
			foundHold = true
		}
	}
	if !foundHold {
		t.Fatal("expected HOLD decision")
	}
}

func TestSelector_BasisReversion_ExtendedBasis(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Extended positive basis (perp premium high), neutral/negative funding.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   50200,
		FundingRate: 0.00001, // too low for funding carry
		Basis:       0.004,   // 0.4%
		BasisZScore: 2.5,     // above entry threshold (2.0)
		BasisMean:   0.001,
		BasisStdDev: 0.0012,
		Regime:      "NEUTRAL",
		TsMs:        1000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Entries) == 0 {
		t.Fatal("expected basis reversion entry for extended positive basis")
	}

	entry := eval.Entries[0]
	if entry.Strategy != "basis" {
		t.Fatalf("expected basis strategy, got %s", entry.Strategy)
	}
}

func TestSelector_ExitFunding_NegativeRate(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Manually add a position.
	sel.ConfirmEntry("BTCUSDT", StrategyFundingCarry, 30000, MarketCondition{
		FundingRate: 0.0003,
		Basis:       0.001,
	})

	// Now funding turns negative.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   49990,
		FundingRate: -0.0001,
		Basis:       -0.0002,
		BasisZScore: -0.3,
		Regime:      "BEARISH",
		TsMs:        2000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Exits) == 0 {
		t.Fatal("expected exit intent when funding turns negative")
	}

	exit := eval.Exits[0]
	if exit.Strategy != "funding_exit" {
		t.Fatalf("expected funding_exit, got %s", exit.Strategy)
	}
	if exit.Legs[0].Action != "SELL" || exit.Legs[0].Market != "SPOT" {
		t.Fatal("exit leg 0 should be SELL SPOT")
	}
}

func TestSelector_ExitBasis_Converged(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Add a basis position.
	sel.ConfirmEntry("BTCUSDT", StrategyBasisReversion, 20000, MarketCondition{
		Basis:       0.004,
		BasisZScore: 2.5,
	})

	// Basis has converged.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   50010,
		FundingRate: 0.00005,
		Basis:       0.0002,
		BasisZScore: 0.3, // below exit threshold (0.5)
		Regime:      "NEUTRAL",
		TsMs:        2000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Exits) == 0 {
		t.Fatal("expected exit when basis converges")
	}

	if eval.Exits[0].Strategy != "basis_exit" {
		t.Fatalf("expected basis_exit, got %s", eval.Exits[0].Strategy)
	}
}

func TestSelector_MaxConcurrentPositions(t *testing.T) {
	cfg := testSelectorConfig()
	cfg.MaxConcurrentPositions = 1
	sel := NewSelector(cfg)

	// Add one position.
	sel.ConfirmEntry("BTCUSDT", StrategyFundingCarry, 30000, MarketCondition{})

	// Both symbols have opportunities.
	conditions := []MarketCondition{
		{
			Symbol: "BTCUSDT", SpotPrice: 50000, PerpPrice: 50050,
			FundingRate: 0.0003, BasisZScore: 0.5, Regime: "BULLISH", TsMs: 1000,
		},
		{
			Symbol: "ETHUSDT", SpotPrice: 3000, PerpPrice: 3003,
			FundingRate: 0.0003, BasisZScore: 0.5, Regime: "BULLISH", TsMs: 1000,
		},
	}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	// Should not open a second position.
	if len(eval.Entries) != 0 {
		t.Fatal("should not open new position when at max concurrent")
	}
}

func TestSelector_ConfirmEntryAndExit(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	if sel.PositionCount() != 0 {
		t.Fatal("should start with 0 positions")
	}

	sel.ConfirmEntry("BTCUSDT", StrategyFundingCarry, 30000, MarketCondition{})
	if sel.PositionCount() != 1 {
		t.Fatal("should have 1 position after entry")
	}

	positions := sel.OpenPositions()
	if pos, ok := positions["BTCUSDT"]; !ok {
		t.Fatal("BTCUSDT should be in positions")
	} else if pos.Strategy != StrategyFundingCarry {
		t.Fatalf("expected funding carry, got %s", pos.Strategy)
	}

	sel.ConfirmExit("BTCUSDT")
	if sel.PositionCount() != 0 {
		t.Fatal("should have 0 positions after exit")
	}
}

func TestSelector_RegimeScale_Zero(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Great opportunity but regime scale is 0 (persistent negative).
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   50050,
		FundingRate: 0.0003,
		BasisZScore: 0.5,
		Regime:      "BULLISH",
		TsMs:        1000,
	}}

	eval := sel.Evaluate(conditions, 100000, 0.0)

	// With 0 equity (due to 0 scale), no entries should be generated.
	if len(eval.Entries) != 0 {
		t.Fatal("should not enter positions with regime scale 0")
	}
}

func TestSelector_FundingCarry_InsufficientYield(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Funding rate is positive but too low for minimum yield threshold.
	conditions := []MarketCondition{{
		Symbol:      "BTCUSDT",
		SpotPrice:   50000,
		PerpPrice:   50002,
		FundingRate: 0.000005, // 0.0005% per 8h = ~0.55% annual (below 5% threshold)
		Basis:       0.00004,
		BasisZScore: 0.2,
		Regime:      "NEUTRAL",
		TsMs:        1000,
	}}

	eval := sel.Evaluate(conditions, 100000, 1.0)

	if len(eval.Entries) != 0 {
		t.Fatal("should not enter with insufficient yield")
	}
}
