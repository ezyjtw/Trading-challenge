// Challenge daemon: HyroTrader compliance layer.
// Intercepts trade intents, validates against challenge rules, forwards approved ones.
// Wires in: paired stop-loss coordination, circuit breaker gate, webhook alerts.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/alerting"
	"github.com/ezyjtw/Trading-challenge/internal/arb"
	"github.com/ezyjtw/Trading-challenge/internal/challenge"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/shopspring/decimal"
)

func main() {
	slog.Info("challenge-daemon: starting")

	redisAddr := env("REDIS_ADDR", "localhost:6379")
	webhookURL := os.Getenv("ALERT_WEBHOOK_URL")

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

	alerter := alerting.NewAlerter(alerting.WebhookConfig{
		URL:             webhookURL,
		Enabled:         webhookURL != "",
		RateLimitPerMin: 10,
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()
	equityTicker := time.NewTicker(5 * time.Second)
	defer equityTicker.Stop()
	safetyTicker := time.NewTicker(1 * time.Minute)
	defer safetyTicker.Stop()
	slCheckTicker := time.NewTicker(30 * time.Second)
	defer slCheckTicker.Stop()

	approved, rejected := 0, 0

	for {
		select {
		case <-pollTicker.C:
			// Gate: circuit breaker
			cbState, _ := bus.GetString(ctx, "circuit_breaker:open")
			if cbState == "true" {
				continue
			}

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
						"id", intent.IntentID, "reason", err.Error())

					bus.Publish(ctx, eventbus.StreamChallenge, map[string]interface{}{
						"type":   "INTENT_REJECTED",
						"intent": intent.IntentID,
						"reason": err.Error(),
						"ts_ms":  time.Now().UnixMilli(),
					})

					if daemon.IsHalted() {
						alerter.SendHalt(ctx, "challenge-daemon",
							"Challenge HALTED — all intents rejected: "+err.Error())
					}
					continue
				}

				approved++

				// Track position and compute paired stop-loss
				intentNotional := decimal.Zero
				for _, leg := range intent.Legs {
					intentNotional = intentNotional.Add(decimal.NewFromFloat(leg.NotionalUSD))
				}
				perLegNotional := intentNotional.Div(decimal.NewFromInt(int64(len(intent.Legs))))
				daemon.TrackPosition(intent.Symbol, intentNotional, perLegNotional)

				// Publish paired stop-loss info for execution router
				if len(intent.Legs) >= 2 {
					spotEntry := decimal.NewFromFloat(intent.Legs[0].NotionalUSD)
					perpEntry := decimal.NewFromFloat(intent.Legs[1].NotionalUSD)
					slInfo := daemon.ComputePairedStopLoss(intent.Symbol, spotEntry, perpEntry)
					bus.Publish(ctx, eventbus.StreamChallenge, map[string]interface{}{
						"type":          "PAIRED_STOP_LOSS",
						"intent":        intent.IntentID,
						"symbol":        intent.Symbol,
						"min_width_pct": slInfo.MinWidthPct.StringFixed(2),
						"spot_stop":     slInfo.SpotStopPrice.StringFixed(2),
						"perp_stop":     slInfo.PerpStopPrice.StringFixed(2),
						"ts_ms":         time.Now().UnixMilli(),
					})
				}

				if err := bus.Publish(ctx, eventbus.StreamApproved, intent); err != nil {
					slog.Warn("publish approved intent failed", "err", err)
				}
			}

		case <-equityTicker.C:
			var equity decimal.Decimal
			if err := bus.Get(ctx, "equity:current", &equity); err == nil {
				if err := daemon.UpdateEquity(equity); err != nil {
					slog.Warn("challenge equity update warning", "err", err)
					if daemon.IsHalted() {
						alerter.SendHalt(ctx, "challenge-daemon",
							"Challenge HALTED after equity update: "+err.Error())
					}
				}
			}
			snap := daemon.GetSnapshot()
			bus.Publish(ctx, eventbus.StreamChallenge, snap)

		case <-slCheckTicker.C:
			expired := daemon.PositionsWithExpiredStopLoss()
			for _, sym := range expired {
				msg := "STOP-LOSS DEADLINE EXPIRED for " + sym + " — set stop-loss or close position!"
				slog.Error(msg)
				alerter.SendCritical(ctx, "challenge-daemon", msg)
				bus.Publish(ctx, eventbus.StreamChallenge, map[string]interface{}{
					"type":   "STOP_LOSS_EXPIRED",
					"symbol": sym,
					"ts_ms":  time.Now().UnixMilli(),
				})
			}

		case <-safetyTicker.C:
			if daemon.CheckSafetyTrade() {
				slog.Info("challenge-daemon: safety trade needed — no trades today")
				safetyIntent := arb.TradeIntent{
					IntentID:  "safety-" + time.Now().Format("20060102-150405"),
					Strategy:  "SAFETY_TRADE",
					Symbol:    "BTCUSDT",
					TsMs:      time.Now().UnixMilli(),
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
				alerter.Send(ctx, alerting.Alert{
					Level: "INFO", Source: "challenge-daemon",
					Message: "Safety trade placed to count as trading day",
				})
			}

		case <-sig:
			slog.Info("challenge-daemon: shutting down",
				"approved", approved, "rejected", rejected)
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
