// Execution router: consumes approved intents, places real orders on Bybit.
// Gates execution on circuit breaker state and risk mode before placing orders.
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
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/exchange/bybit"
	"github.com/ezyjtw/Trading-challenge/internal/execution"
)

func main() {
	slog.Info("execution-router: starting")

	redisAddr := env("REDIS_ADDR", "localhost:6379")
	apiKey := os.Getenv("BYBIT_API_KEY")
	apiSecret := os.Getenv("BYBIT_API_SECRET")
	testnet := os.Getenv("BYBIT_TESTNET") == "true"
	webhookURL := os.Getenv("ALERT_WEBHOOK_URL")

	if apiKey == "" || apiSecret == "" {
		slog.Error("BYBIT_API_KEY and BYBIT_API_SECRET must be set")
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

	if err := bus.EnsureGroup(ctx, eventbus.StreamApproved, "execution-router"); err != nil {
		slog.Error("ensure group failed", "err", err)
		os.Exit(1)
	}

	client := bybit.New(apiKey, apiSecret, testnet)
	executor := execution.NewExecutor(client, execution.Config{
		MaxRetriesPerLeg:  3,
		HedgeDriftMaxMs:   2000,
		MinPartialFillPct: 0.10,
		ReconDelayMs:      2000,
		ReconIntervalMs:   60000,
		FeeBpsTaker:       5.5,
		FeeBpsMaker:       2.0,
	})

	alerter := alerting.NewAlerter(alerting.WebhookConfig{
		URL:             webhookURL,
		Enabled:         webhookURL != "",
		RateLimitPerMin: 10,
	})

	// Start periodic position reconciliation
	go executor.StartPeriodicReconciliation(ctx, 60*time.Second)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	executed, skipped := 0, 0

	for {
		select {
		case <-pollTicker.C:
			// --- Gate 1: Check risk mode ---
			riskMode, _ := bus.GetString(ctx, "risk:mode")
			if riskMode == "HALTED" || riskMode == "FLATTEN" {
				// Don't consume intents when risk is escalated
				continue
			}

			// --- Gate 2: Check circuit breaker ---
			cbState, _ := bus.GetString(ctx, "circuit_breaker:open")
			if cbState == "true" {
				slog.Warn("execution paused: circuit breaker open")
				continue
			}

			msgs, err := bus.Read(ctx, eventbus.StreamApproved, "execution-router", "exec-1", 5, 200*time.Millisecond)
			if err != nil {
				continue
			}
			for _, raw := range msgs {
				var intent arb.TradeIntent
				if err := json.Unmarshal(raw, &intent); err != nil {
					slog.Warn("unmarshal intent failed", "err", err)
					continue
				}

				// Re-check gates per-intent (state could change during batch)
				riskMode, _ = bus.GetString(ctx, "risk:mode")
				if riskMode == "HALTED" || riskMode == "FLATTEN" {
					slog.Warn("skipping intent due to risk mode", "id", intent.IntentID, "mode", riskMode)
					skipped++
					continue
				}

				cbState, _ = bus.GetString(ctx, "circuit_breaker:open")
				if cbState == "true" {
					slog.Warn("skipping intent due to circuit breaker", "id", intent.IntentID)
					skipped++
					continue
				}

				slog.Info("executing intent",
					"id", intent.IntentID,
					"strategy", intent.Strategy,
					"symbol", intent.Symbol)

				events, fill := executor.Execute(ctx, intent)

				// Publish events and check for failures
				hedgeFailed := false
				for _, ev := range events {
					bus.Publish(ctx, eventbus.StreamExecution, ev)
					if ev.EventType == execution.EventHedgeFailed {
						hedgeFailed = true
					}
				}
				if fill != nil {
					bus.Publish(ctx, eventbus.StreamExecution, fill)
				}

				if hedgeFailed {
					alerter.SendCritical(ctx, "execution-router",
						"HEDGE FAILED for intent "+intent.IntentID+" — manual review needed")
				}

				executed++
			}

		case <-sig:
			slog.Info("execution-router: shutting down", "executed", executed, "skipped", skipped)
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
