// Funding engine service: monitors funding rates, generates delta-neutral trade intents.
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

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	evalTicker := time.NewTicker(time.Duration(cfg.EvalIntervalS) * time.Second)
	defer evalTicker.Stop()

	for {
		select {
		case <-evalTicker.C:
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
				// Convert marketdata.Quote to funding.Quote
				fq := funding.Quote{
					Symbol:      q.Symbol,
					SpotPrice:   q.IndexPrice, // index price approximates spot
					PerpPrice:   q.MarkPrice,
					FundingRate: q.FundingRate,
					TsMs:        q.TsMs,
				}
				engine.UpdateQuote(fq)
			}

			// Get equity from Redis (set by risk-daemon)
			accountEquity := 100000.0 // default
			if eqVal, err := bus.GetString(ctx, "equity:current"); err == nil && eqVal != "" {
				var eq float64
				if err := json.Unmarshal([]byte(eqVal), &eq); err == nil && eq > 0 {
					accountEquity = eq
				}
			}

			// Evaluate entries
			intents := engine.Evaluate(accountEquity)
			for _, intent := range intents {
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish intent failed", "err", err)
				}
				slog.Info("funding intent emitted",
					"id", intent.IntentID,
					"symbol", intent.Symbol,
					"strategy", intent.Strategy)
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
