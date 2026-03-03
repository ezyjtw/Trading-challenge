// Ledger service: persists trading data to PostgreSQL.
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
	"github.com/ezyjtw/Trading-challenge/internal/execution"
	"github.com/ezyjtw/Trading-challenge/internal/ledger"
	"github.com/ezyjtw/Trading-challenge/internal/risk"
)

func main() {
	slog.Info("ledger: starting")

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	pgURL := os.Getenv("POSTGRES_URL")
	if pgURL == "" {
		pgURL = "postgres://trader:trader@localhost:5432/trading?sslmode=disable"
	}

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	writer, err := ledger.NewWriter(pgURL)
	if err != nil {
		slog.Error("postgres connect failed", "err", err)
		os.Exit(1)
	}
	defer writer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ensure consumer groups
	for _, stream := range []string{
		eventbus.StreamExecution,
		eventbus.StreamRisk,
		eventbus.StreamChallenge,
	} {
		bus.EnsureGroup(ctx, stream, "ledger")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	persisted := 0

	for {
		select {
		case <-pollTicker.C:
			// Drain execution events
			msgs, _ := bus.Read(ctx, eventbus.StreamExecution, "ledger", "ledger-1", 50, 100*time.Millisecond)
			for _, raw := range msgs {
				var ev execution.ExecutionEvent
				if err := json.Unmarshal(raw, &ev); err == nil {
					writer.WriteExecutionEvent(ctx, ev)
					persisted++
				}
			}

			// Drain risk state
			msgs, _ = bus.Read(ctx, eventbus.StreamRisk, "ledger", "ledger-1", 20, 100*time.Millisecond)
			for _, raw := range msgs {
				var state risk.State
				if err := json.Unmarshal(raw, &state); err == nil {
					writer.WriteRiskState(ctx, state)
				}
			}

			// Drain challenge events
			msgs, _ = bus.Read(ctx, eventbus.StreamChallenge, "ledger", "ledger-1", 20, 100*time.Millisecond)
			for _, raw := range msgs {
				writer.WriteRaw(ctx, "challenge_events", string(raw))
			}

		case <-sig:
			slog.Info("ledger: shutting down", "persisted", persisted)
			cancel()
			return
		}
	}
}
