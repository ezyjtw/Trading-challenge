package strategy

import (
	"testing"
)

func testSelectorConfig() SelectorConfig {
	cfg := DefaultSelectorConfig()
	cfg.CooldownS = 0 // no cooldown for tests
	return cfg
}

func makePairCondition(fundingDiffBps, spreadZScore float64, regime string) PairCondition {
	return PairCondition{
		PrimarySymbol:  "BTCUSDT",
		PrimaryPrice:   50000,
		PrimaryFunding: 0.0003, // 0.03% per 8h
		HedgeSymbol:    "ETHUSDT",
		HedgePrice:     3000,
		HedgeFunding:   0.0001, // 0.01% per 8h
		FundingDiffBps: fundingDiffBps,
		Beta:           1.0,
		Correlation:    0.85,
		SpreadZScore:   spreadZScore,
		SpreadMean:     2.8,
		SpreadStdDev:   0.01,
		Regime:         regime,
		TsMs:           1000,
	}
}

func TestSelector_FundingCarry_PositiveDiff(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Positive funding differential with bullish regime.
	cond := makePairCondition(10.0, 0.5, "BULLISH") // 10 bps diff
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Entries) == 0 {
		t.Fatal("expected at least one entry intent for positive funding diff")
	}

	entry := eval.Entries[0]
	if entry.Strategy != "funding" {
		t.Fatalf("expected funding strategy, got %s", entry.Strategy)
	}
	if len(entry.Legs) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(entry.Legs))
	}
	// Cross-pair: SELL primary PERP + BUY hedge PERP.
	if entry.Legs[0].Action != "SELL" || entry.Legs[0].Market != "PERP" {
		t.Fatalf("leg 0 should be SELL PERP, got %s %s", entry.Legs[0].Action, entry.Legs[0].Market)
	}
	if entry.Legs[0].Symbol != "BTCUSDT" {
		t.Fatalf("leg 0 symbol should be BTCUSDT, got %s", entry.Legs[0].Symbol)
	}
	if entry.Legs[1].Action != "BUY" || entry.Legs[1].Market != "PERP" {
		t.Fatalf("leg 1 should be BUY PERP, got %s %s", entry.Legs[1].Action, entry.Legs[1].Market)
	}
	if entry.Legs[1].Symbol != "ETHUSDT" {
		t.Fatalf("leg 1 symbol should be ETHUSDT, got %s", entry.Legs[1].Symbol)
	}
}

func TestSelector_Hold_NoDiff(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// No significant funding diff, no spread deviation.
	cond := makePairCondition(0.0, 0.3, "BEARISH")
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Entries) != 0 {
		t.Fatal("expected no entries for zero funding diff with no spread opportunity")
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

func TestSelector_BasisReversion_ExtendedSpread(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Extended spread z-score, low funding diff.
	cond := makePairCondition(0.5, 2.5, "NEUTRAL")
	cond.SpreadStdDev = 0.01 // enough edge after fees
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Entries) == 0 {
		t.Fatal("expected basis reversion entry for extended spread")
	}

	entry := eval.Entries[0]
	if entry.Strategy != "basis" {
		t.Fatalf("expected basis strategy, got %s", entry.Strategy)
	}
}

func TestSelector_ExitFunding_DiffReversed(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Manually add a position.
	entryCond := makePairCondition(10.0, 0.5, "BULLISH")
	sel.ConfirmEntry("BTCUSDT", "ETHUSDT", StrategyFundingCarry, 30000, entryCond)

	// Now funding diff reverses.
	cond := makePairCondition(-2.0, 0.3, "BEARISH")
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Exits) == 0 {
		t.Fatal("expected exit intent when funding diff reverses")
	}

	exit := eval.Exits[0]
	if exit.Strategy != "funding_exit" {
		t.Fatalf("expected funding_exit, got %s", exit.Strategy)
	}
	// Exit reverses the entry: BUY primary + SELL hedge.
	if exit.Legs[0].Action != "BUY" || exit.Legs[0].Market != "PERP" {
		t.Fatalf("exit leg 0 should be BUY PERP, got %s %s", exit.Legs[0].Action, exit.Legs[0].Market)
	}
	if exit.Legs[1].Action != "SELL" || exit.Legs[1].Market != "PERP" {
		t.Fatalf("exit leg 1 should be SELL PERP, got %s %s", exit.Legs[1].Action, exit.Legs[1].Market)
	}
}

func TestSelector_ExitBasis_Converged(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Add a basis position.
	entryCond := makePairCondition(0.5, 2.5, "NEUTRAL")
	sel.ConfirmEntry("BTCUSDT", "ETHUSDT", StrategyBasisReversion, 20000, entryCond)

	// Spread has converged.
	cond := makePairCondition(0.5, 0.3, "NEUTRAL") // z-score 0.3 < exit threshold 0.5
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Exits) == 0 {
		t.Fatal("expected exit when spread converges")
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
	entryCond := makePairCondition(10.0, 0.5, "BULLISH")
	sel.ConfirmEntry("BTCUSDT", "ETHUSDT", StrategyFundingCarry, 30000, entryCond)

	// Another pair has opportunity.
	cond := PairCondition{
		PrimarySymbol:  "SOLUSDT",
		PrimaryPrice:   100,
		PrimaryFunding: 0.0005,
		HedgeSymbol:    "AVAXUSDT",
		HedgePrice:     30,
		HedgeFunding:   0.0001,
		FundingDiffBps: 15.0,
		Beta:           1.0,
		Correlation:    0.80,
		SpreadZScore:   0.5,
		Regime:         "BULLISH",
		TsMs:           1000,
	}
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

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

	cond := makePairCondition(10.0, 0.5, "BULLISH")
	sel.ConfirmEntry("BTCUSDT", "ETHUSDT", StrategyFundingCarry, 30000, cond)
	if sel.PositionCount() != 1 {
		t.Fatal("should have 1 position after entry")
	}

	positions := sel.OpenPositions()
	pid := PairID("BTCUSDT", "ETHUSDT")
	if pos, ok := positions[pid]; !ok {
		t.Fatalf("%s should be in positions", pid)
	} else if pos.Strategy != StrategyFundingCarry {
		t.Fatalf("expected funding carry, got %s", pos.Strategy)
	}

	sel.ConfirmExit("BTCUSDT", "ETHUSDT")
	if sel.PositionCount() != 0 {
		t.Fatal("should have 0 positions after exit")
	}
}

func TestSelector_RegimeScale_Zero(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Great opportunity but regime scale is 0.
	cond := makePairCondition(15.0, 0.5, "BULLISH")
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 0.0)

	// With 0 equity (due to 0 scale), no entries should be generated.
	if len(eval.Entries) != 0 {
		t.Fatal("should not enter positions with regime scale 0")
	}
}

func TestSelector_FundingCarry_InsufficientYield(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	// Funding diff too low for minimum yield threshold.
	cond := makePairCondition(0.5, 0.2, "NEUTRAL") // 0.5 bps diff, very low
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Entries) != 0 {
		t.Fatal("should not enter with insufficient yield")
	}
}

func TestSelector_LowCorrelation_Rejected(t *testing.T) {
	sel := NewSelector(testSelectorConfig())

	cond := makePairCondition(15.0, 0.5, "BULLISH")
	cond.Correlation = 0.50 // below MinCorrelation of 0.70
	pairs := []PairCondition{cond}

	eval := sel.Evaluate(pairs, 100000, 1.0)

	if len(eval.Entries) != 0 {
		t.Fatal("should not enter with low correlation")
	}

	// Should have a HOLD decision mentioning correlation.
	foundHold := false
	for _, d := range eval.Decisions {
		if d.Strategy == StrategyHold {
			foundHold = true
		}
	}
	if !foundHold {
		t.Fatal("expected HOLD decision for low correlation")
	}
}
