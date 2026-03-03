// Capital allocator: validates intents against allocation limits.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/allocator"
	"github.com/ezyjtw/Trading-challenge/internal/arb"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
)

func main() {
	slog.Info("capital-allocator: starting")

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

	if err := bus.EnsureGroup(ctx, eventbus.StreamTradeIntents, "capital-allocator"); err != nil {
		slog.Error("ensure group failed", "err", err)
		os.Exit(1)
	}

	cfg := allocator.Config{
		InitialCapitalUSD:    100000,
		FundingMaxPct:        60,
		BasisMaxPct:          25,
		CashReservePct:       12,
		DrawdownScaling:      true,
		RebalanceIntervalMin: 30,
		KellyFraction:        0.15,
	}
	engine := allocator.NewEngine(cfg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	approved, rejected := 0, 0

	for {
		select {
		case <-pollTicker.C:
			msgs, err := bus.Read(ctx, eventbus.StreamTradeIntents, "capital-allocator", "alloc-1", 10, 200*time.Millisecond)
			if err != nil {
				continue
			}
			for _, raw := range msgs {
				var intent arb.TradeIntent
				if err := json.Unmarshal(raw, &intent); err != nil {
					continue
				}

				ok, reason := engine.Approve(intent)
				if !ok {
					rejected++
					slog.Info("intent rejected by allocator",
						"id", intent.IntentID,
						"reason", reason)
					continue
				}

				approved++
				if err := bus.Publish(ctx, eventbus.StreamApproved, intent); err != nil {
					slog.Warn("publish approved failed", "err", err)
				}
			}

		case <-sig:
			slog.Info("capital-allocator: shutting down",
				"approved", approved, "rejected", rejected)
			cancel()
			return
		}
	}
}
