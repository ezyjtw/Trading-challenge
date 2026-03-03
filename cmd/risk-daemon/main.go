// Risk daemon: monitors portfolio risk, manages operating mode transitions.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/exchange/bybit"
	"github.com/ezyjtw/Trading-challenge/internal/risk"
)

func main() {
	slog.Info("risk-daemon: starting")

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	apiKey := os.Getenv("BYBIT_API_KEY")
	apiSecret := os.Getenv("BYBIT_API_SECRET")
	testnet := os.Getenv("BYBIT_TESTNET") == "true"

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := risk.Config{
		ChallengeMode:           true,
		MaxDrawdownPct:          10.0,
		SafeModeDrawdownPct:     7.0,
		DailySoftHaltPct:        3.0,
		DailyHardHaltPct:        4.0,
		MaxErrorRate5mPct:       20.0,
		MaxHedgeDriftMs:         2000,
		StaleDataPauseMs:        3000,
		PositionReconIntervalMs: 60000,
		LegMismatchAlertMs:      60000,
		LegMismatchCloseMs:      120000,
		KellyFraction:           0.15,
	}

	daemon := risk.NewDaemon(cfg)

	var client *bybit.Client
	if apiKey != "" && apiSecret != "" {
		client = bybit.New(apiKey, apiSecret, testnet)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	tickInterval := time.NewTicker(5 * time.Second)
	defer tickInterval.Stop()
	stateTicker := time.NewTicker(30 * time.Second)
	defer stateTicker.Stop()

	for {
		select {
		case <-tickInterval.C:
			// Poll Bybit account equity
			if client != nil {
				eq, err := client.GetAccountEquity(ctx)
				if err != nil {
					slog.Warn("equity poll failed", "err", err)
				} else {
					alerts := daemon.UpdateEquity(eq.TotalEquity)
					for _, a := range alerts {
						bus.Publish(ctx, eventbus.StreamRisk, a)
						slog.Warn("risk alert", "source", a.Source, "msg", a.Message)
					}
					// Publish current equity for other services
					bus.Set(ctx, "equity:current", eq.TotalEquity, 30*time.Second)
				}
			}

			// Run periodic risk evaluation
			alerts := daemon.Tick()
			for _, a := range alerts {
				bus.Publish(ctx, eventbus.StreamRisk, a)
			}

		case <-stateTicker.C:
			state := daemon.CurrentState()
			bus.Publish(ctx, eventbus.StreamRisk, state)
			bus.SetString(ctx, "risk:mode", state.Mode.String(), 0)

			slog.Info("risk state",
				"mode", state.Mode,
				"drawdown", state.DrawdownPct,
				"daily_drawdown", state.DailyDrawdownPct,
				"equity", fmt.Sprintf("%.2f", state.CurrentEquityUSD))

			// Position reconciliation for leg mismatch detection
			if client != nil {
				positions, err := client.GetPositions(ctx)
				if err == nil {
					posSlice := make([]risk.PositionSnapshot, len(positions))
					for i, p := range positions {
						posSlice[i] = risk.PositionSnapshot{
							Symbol: p.Symbol,
							Side:   p.Side,
							Size:   p.Quantity,
							TsMs:   time.Now().UnixMilli(),
						}
					}
					alerts := daemon.RecordPositionSnapshot(posSlice)
					for _, a := range alerts {
						bus.Publish(ctx, eventbus.StreamRisk, a)
						slog.Warn("leg mismatch", "msg", a.Message)
					}
				}
			}

		case <-sig:
			slog.Info("risk-daemon: shutting down")
			cancel()
			return
		}
	}
}
