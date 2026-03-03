// Challenge daemon: HyroTrader compliance layer.
// Intercepts trade intents, validates against challenge rules, and forwards approved ones.
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
	"github.com/ezyjtw/Trading-challenge/internal/challenge"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/shopspring/decimal"
)

func main() {
	slog.Info("challenge-daemon: starting")

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	cfgPath := os.Getenv("CHALLENGE_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/challenge_config.yaml"
	}

	cfg, err := challenge.LoadConfig(cfgPath)
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.EnsureGroup(ctx, eventbus.StreamTradeIntents, "challenge-daemon"); err != nil {
		slog.Error("ensure group failed", "err", err)
		os.Exit(1)
	}

	daemon := challenge.NewDaemon(cfg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()
	equityTicker := time.NewTicker(5 * time.Second)
	defer equityTicker.Stop()
	safetyTicker := time.NewTicker(1 * time.Minute)
	defer safetyTicker.Stop()

	approved, rejected := 0, 0

	for {
		select {
		case <-pollTicker.C:
			// Read pending trade intents
			msgs, err := bus.Read(ctx, eventbus.StreamTradeIntents, "challenge-daemon", "challenge-1", 10, 200*time.Millisecond)
			if err != nil {
				continue
			}
			for _, raw := range msgs {
				var intent arb.TradeIntent
				if err := json.Unmarshal(raw, &intent); err != nil {
					slog.Warn("unmarshal intent failed", "err", err)
					continue
				}

				if err := daemon.ValidateIntent(intent); err != nil {
					rejected++
					slog.Info("intent rejected by challenge rules",
						"id", intent.IntentID,
						"reason", err.Error())

					bus.Publish(ctx, eventbus.StreamChallenge, map[string]interface{}{
						"type":    "INTENT_REJECTED",
						"intent":  intent.IntentID,
						"reason":  err.Error(),
						"ts_ms":   time.Now().UnixMilli(),
					})
					continue
				}

				approved++
				// Forward to approved stream for execution
				if err := bus.Publish(ctx, eventbus.StreamApproved, intent); err != nil {
					slog.Warn("publish approved intent failed", "err", err)
				}
			}

		case <-equityTicker.C:
			// Read equity from Redis (set by risk-daemon from Bybit API)
			var equity decimal.Decimal
			if err := bus.Get(ctx, "equity:current", &equity); err == nil {
				daemon.UpdateEquity(equity)
			}

		case <-safetyTicker.C:
			if daemon.CheckSafetyTrade() {
				slog.Info("challenge-daemon: safety trade needed — no trades today")
				// Emit a minimal safety trade intent
				safetyIntent := arb.TradeIntent{
					IntentID: "safety-" + time.Now().Format("20060102-150405"),
					Strategy: "SAFETY_TRADE",
					Symbol:   "BTCUSDT",
					TsMs:     time.Now().UnixMilli(),
					ExpiresMs: time.Now().Add(5 * time.Minute).UnixMilli(),
					Legs: []arb.TradeLeg{{
						Action:         "BUY",
						Type:           "MARKET",
						Market:         "SPOT",
						Symbol:         "BTCUSDT",
						NotionalUSD:    100,
						MaxSlippageBps: 10,
					}},
				}
				bus.Publish(ctx, eventbus.StreamApproved, safetyIntent)
			}

		case <-sig:
			slog.Info("challenge-daemon: shutting down",
				"approved", approved, "rejected", rejected)
			cancel()
			return
		}
	}
}
