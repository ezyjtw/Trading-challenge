// Arb engine service: cross-pair spread trading (log price ratio mean-reversion).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/marketdata"
)

func main() {
	slog.Info("arb-engine: starting")

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

	if err := bus.EnsureGroup(ctx, eventbus.StreamMarketData, "arb-engine"); err != nil {
		slog.Error("ensure group failed", "err", err)
		os.Exit(1)
	}

	cfg := arb.BasisConfig{
		Pairs: []arb.SpreadPairConfig{
			{Primary: "BTCUSDT", Hedge: "ETHUSDT", MinCorrelation: 0.70},
		},
		EntryStdDev:           2.0,
		ExitStdDev:            0.5,
		MinAnnualizedBasisPct: 15.0,
		MaxHoldDays:           7,
		MaxAllocationPct:      25.0,
		MaxSlippageBps:        5.0,
		CooldownS:             600,
	}

	engine := arb.NewEngine(cfg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	evalTicker := time.NewTicker(30 * time.Second)
	defer evalTicker.Stop()

	// Track latest quotes per symbol for cross-pair updates.
	latestPrices := make(map[string]float64) // symbol -> mark price

	for {
		select {
		case <-evalTicker.C:
			msgs, err := bus.Read(ctx, eventbus.StreamMarketData, "arb-engine", "arb-1", 50, 100*time.Millisecond)
			if err != nil {
				slog.Warn("read failed", "err", err)
				continue
			}
			for _, raw := range msgs {
				var q marketdata.Quote
				if err := json.Unmarshal(raw, &q); err != nil {
					continue
				}
				latestPrices[q.Symbol] = q.MarkPrice
			}

			// Update prices for each configured pair.
			for _, pair := range cfg.Pairs {
				pp, pOk := latestPrices[pair.Primary]
				hp, hOk := latestPrices[pair.Hedge]
				if pOk && hOk {
					engine.UpdatePrices(pair.Primary, pair.Hedge, pp, hp, time.Now().UnixMilli())
				}
			}

			intents := engine.Evaluate(100000) // TODO: get real equity from Redis
			for _, intent := range intents {
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish intent failed", "err", err)
				}
				slog.Info("basis intent emitted",
					"id", intent.IntentID,
					"symbol", intent.Symbol,
					"hedge", intent.HedgeSymbol)
			}

		case <-sig:
			slog.Info("arb-engine: shutting down")
			cancel()
			return
		}
	}
}
