// Funding engine service: monitors funding rates, generates delta-neutral trade intents.
// Wires in: RegimeDetector for proactive position scaling in negative-rate environments.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/funding"
	"github.com/ezyjtw/Trading-challenge/internal/marketdata"
)

func main() {
	slog.Info("funding-engine: starting")

	redisAddr := env("REDIS_ADDR", "localhost:6379")

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.EnsureGroup(ctx, eventbus.StreamMarketData, "funding-engine"); err != nil {
		slog.Error("ensure group failed", "err", err)
		os.Exit(1)
	}

	cfg := funding.Config{
		Symbols:            []string{"BTCUSDT", "ETHUSDT"},
		MinAnnualYieldPct:  5.0,
		ExitThresholdRate:  0.00005,
		NegativeExitCycles: 2,
		MaxAllocationPct:   60.0,
		FeeBpsTaker:        5.5,
		EvalIntervalS:      30,
		IntentTTLMs:        10000,
		MaxSlippageBps:     5.0,
		CooldownS:          300,
	}

	engine := funding.NewEngine(cfg)

	// --- Regime detector: proactive funding rate environment classification ---
	regime := funding.NewRegimeDetector(funding.RegimeConfig{
		LookbackPeriods:           90,  // 30 days of 8h observations
		NegativeThresholdPct:      60.0,
		ReducePositionPct:         25.0,
		ExitAllOnPersistent:       true,
		PersistentNegativePeriods: 12, // 4 days consecutive negative
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	evalTicker := time.NewTicker(time.Duration(cfg.EvalIntervalS) * time.Second)
	defer evalTicker.Stop()

	for {
		select {
		case <-evalTicker.C:
			// Check risk mode — don't generate intents if risk is escalated
			riskMode, _ := bus.GetString(ctx, "risk:mode")
			if riskMode == "HALTED" || riskMode == "FLATTEN" {
				slog.Info("funding-engine: paused due to risk mode", "mode", riskMode)
				continue
			}

			// Drain market data
			msgs, err := bus.Read(ctx, eventbus.StreamMarketData, "funding-engine", "funding-1", 50, 100*time.Millisecond)
			if err != nil {
				slog.Warn("read market data failed", "err", err)
				continue
			}
			for _, raw := range msgs {
				var q marketdata.Quote
				if err := json.Unmarshal(raw, &q); err != nil {
					continue
				}
				fq := funding.Quote{
					Symbol:      q.Symbol,
					SpotPrice:   q.IndexPrice,
					PerpPrice:   q.MarkPrice,
					FundingRate: q.FundingRate,
					TsMs:        q.TsMs,
				}
				engine.UpdateQuote(fq)

				// Feed funding rate to regime detector
				if q.FundingRate != 0 {
					regime.RecordRate(q.Symbol, q.FundingRate, q.TsMs)
				}
			}

			// Get equity from Redis (set by risk-daemon)
			accountEquity := 100000.0
			if eqVal, err := bus.GetString(ctx, "equity:current"); err == nil && eqVal != "" {
				var eq float64
				if err := json.Unmarshal([]byte(eqVal), &eq); err == nil && eq > 0 {
					accountEquity = eq
				}
			}

			// --- Regime-adjusted evaluation ---
			for _, sym := range cfg.Symbols {
				// Check if regime says to exit all for this symbol
				if regime.ShouldExitAll(sym) {
					slog.Warn("regime: persistent negative — forcing exits", "symbol", sym)
					exits := engine.EvaluateExits()
					for _, intent := range exits {
						if intent.Symbol == sym {
							bus.Publish(ctx, eventbus.StreamTradeIntents, intent)
							slog.Info("regime-forced exit", "id", intent.IntentID, "symbol", sym)
						}
					}
					continue
				}
			}

			// Scale account equity by worst regime factor across symbols.
			// This reduces position sizing in bearish environments.
			minScale := 1.0
			for _, sym := range cfg.Symbols {
				scale := regime.PositionScaleFactor(sym)
				if scale < minScale {
					minScale = scale
				}
			}
			scaledEquity := accountEquity * minScale

			if minScale < 1.0 {
				slog.Info("funding: regime scaling active",
					"scale", minScale,
					"raw_equity", accountEquity,
					"scaled_equity", scaledEquity)
			}

			// Evaluate entries with regime-scaled equity
			intents := engine.Evaluate(scaledEquity)
			for _, intent := range intents {
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish intent failed", "err", err)
				}
				slog.Info("funding intent emitted",
					"id", intent.IntentID,
					"symbol", intent.Symbol,
					"strategy", intent.Strategy,
					"regime_scale", minScale)
			}

			// Evaluate exits
			exits := engine.EvaluateExits()
			for _, intent := range exits {
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish exit intent failed", "err", err)
				}
				slog.Info("funding exit emitted",
					"id", intent.IntentID,
					"symbol", intent.Symbol)
			}

		case <-sig:
			slog.Info("funding-engine: shutting down")
			cancel()
			return
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
