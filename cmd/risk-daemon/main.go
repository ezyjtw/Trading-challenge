// Risk daemon: monitors portfolio risk, manages operating mode transitions.
// Wires in: MarginMonitor, CircuitBreaker, cross-margin reconciliation, webhook alerts.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/alerting"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/exchange/bybit"
	"github.com/ezyjtw/Trading-challenge/internal/risk"
	"github.com/shopspring/decimal"
)

func main() {
	slog.Info("risk-daemon: starting")

	redisAddr := env("REDIS_ADDR", "localhost:6379")
	apiKey := os.Getenv("BYBIT_API_KEY")
	apiSecret := os.Getenv("BYBIT_API_SECRET")
	testnet := os.Getenv("BYBIT_TESTNET") == "true"
	webhookURL := os.Getenv("ALERT_WEBHOOK_URL")

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Core risk daemon with tighter mismatch thresholds ---
	cfg := risk.Config{
		ChallengeMode:           true,
		MaxDrawdownPct:          10.0,
		SafeModeDrawdownPct:     7.0,
		DailySoftHaltPct:        3.0,
		DailyHardHaltPct:        4.0,
		MaxErrorRate5mPct:       20.0,
		MaxHedgeDriftMs:         2000,
		StaleDataPauseMs:        3000,
		PositionReconIntervalMs: 10000,  // was 60000 — now 10s
		LegMismatchAlertMs:      5000,   // was 60000 — now 5s
		LegMismatchCloseMs:      10000,  // was 120000 — now 10s
		KellyFraction:           0.15,
	}
	daemon := risk.NewDaemon(cfg)

	// --- Margin monitor ---
	marginMon := risk.NewMarginMonitor(risk.MarginConfig{
		WarnRatioPct:      60.0,
		CriticalRatioPct:  75.0,
		HaltRatioPct:      85.0,
		LiqPriceBufferPct: 5.0,
	})

	// --- Circuit breaker (fed by market data stream) ---
	cb := risk.NewCircuitBreaker(risk.CircuitBreakerConfig{
		PriceMovePctThreshold:     5.0,
		WindowMs:                  60000,
		SpreadMultiplierThreshold: 3.0,
		NormalSpreadBps:           2.0,
		CooldownMs:               30000,
		StaleQuoteMs:              5000,
	})

	// --- Webhook alerter ---
	alerter := alerting.NewAlerter(alerting.WebhookConfig{
		URL:             webhookURL,
		Enabled:         webhookURL != "",
		RateLimitPerMin: 10,
	})

	// Publish circuit breaker state for execution router
	go publishCBState(ctx, bus, cb)

	var client *bybit.Client
	if apiKey != "" && apiSecret != "" {
		client = bybit.New(apiKey, apiSecret, testnet)
	}

	// Consume market data for circuit breaker
	bus.EnsureGroup(ctx, eventbus.StreamMarketData, "risk-daemon")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	equityTicker := time.NewTicker(5 * time.Second)
	defer equityTicker.Stop()
	positionTicker := time.NewTicker(10 * time.Second)
	defer positionTicker.Stop()
	stateTicker := time.NewTicker(15 * time.Second)
	defer stateTicker.Stop()
	mdTicker := time.NewTicker(1 * time.Second) // drain market data for circuit breaker
	defer mdTicker.Stop()

	for {
		select {
		case <-equityTicker.C:
			if client == nil {
				continue
			}

			// --- Poll equity + margin (cross-margin reconciliation) ---
			eq, err := client.GetAccountEquity(ctx)
			if err != nil {
				slog.Warn("equity poll failed", "err", err)
				alerter.Send(ctx, alerting.Alert{
					Level: "WARN", Source: "risk-daemon",
					Message: "equity poll failed: " + err.Error(),
				})
				continue
			}

			// Update risk daemon with total equity (covers both spot + perp)
			alerts := daemon.UpdateEquity(eq.TotalEquity)
			for _, a := range alerts {
				bus.Publish(ctx, eventbus.StreamRisk, a)
				slog.Warn("risk alert", "source", a.Source, "msg", a.Message)
				alerter.Send(ctx, alerting.Alert{
					Level: a.Severity.String(), Source: a.Source, Message: a.Message,
					Metric: a.Metric, Value: fmt.Sprintf("%.4f", a.Value),
				})
			}
			bus.Set(ctx, "equity:current", eq.TotalEquity, 30*time.Second)

			// --- Margin monitor ---
			marginAlerts := marginMon.UpdateMargin(risk.MarginSnapshot{
				TotalEquity:     eq.TotalEquity,
				TotalMarginUsed: eq.TotalMargin,
				AvailableMargin: eq.AvailableBalance,
				TsMs:            time.Now().UnixMilli(),
			})
			for _, ma := range marginAlerts {
				bus.Publish(ctx, eventbus.StreamRisk, ma)
				slog.Warn("margin alert", "level", ma.Level, "msg", ma.Message,
					"ratio", fmt.Sprintf("%.1f%%", ma.Value))
				alerter.Send(ctx, alerting.Alert{
					Level: ma.Level, Source: "margin-monitor", Message: ma.Message,
					Metric: ma.Metric, Value: fmt.Sprintf("%.2f", ma.Value),
				})

				// Escalate risk mode on critical/halt margin alerts
				if ma.Level == "HALT" {
					daemon.SetMode(risk.ModeHalted, "margin ratio exceeded halt threshold")
					alerter.SendHalt(ctx, "margin-monitor", ma.Message)
				} else if ma.Level == "CRITICAL" {
					daemon.SetMode(risk.ModeFlatten, "margin ratio critical — flatten")
					alerter.SendCritical(ctx, "margin-monitor", ma.Message)
				}
			}

			// Run periodic risk tick
			tickAlerts := daemon.Tick()
			for _, a := range tickAlerts {
				bus.Publish(ctx, eventbus.StreamRisk, a)
			}

		case <-positionTicker.C:
			if client == nil {
				continue
			}

			positions, err := client.GetPositions(ctx)
			if err != nil {
				slog.Warn("position poll failed", "err", err)
				continue
			}

			// Leg mismatch detection (now at 10s intervals, 5s alert, 10s close)
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
				alerter.SendCritical(ctx, "risk-daemon", "LEG MISMATCH: "+a.Message)
			}

			// Liquidation proximity check
			marginPositions := make([]risk.PositionMarginInfo, len(positions))
			for i, p := range positions {
				marginPositions[i] = risk.PositionMarginInfo{
					Symbol:    p.Symbol,
					MarkPrice: p.MarkPrice,
					LiqPrice:  p.LiqPrice,
					Side:      p.Side,
					Notional:  p.NotionalUSD,
				}
			}
			liqAlerts := marginMon.UpdatePositions(marginPositions)
			for _, la := range liqAlerts {
				bus.Publish(ctx, eventbus.StreamRisk, la)
				alerter.SendCritical(ctx, "margin-monitor",
					fmt.Sprintf("LIQUIDATION WARNING: %s buffer %.1f%%", la.Symbol, la.Value))
			}

		case <-mdTicker.C:
			// Drain market data to feed the circuit breaker
			msgs, err := bus.Read(ctx, eventbus.StreamMarketData, "risk-daemon", "risk-md-1", 50, 100*time.Millisecond)
			if err != nil {
				continue
			}
			for _, raw := range msgs {
				var q struct {
					Symbol    string  `json:"symbol"`
					LastPrice float64 `json:"last_price"`
					BestBid   float64 `json:"best_bid"`
					BestAsk   float64 `json:"best_ask"`
					TsMs      int64   `json:"ts_ms"`
				}
				if err := json.Unmarshal(raw, &q); err != nil {
					continue
				}
				reason := cb.RecordQuote(q.Symbol, q.LastPrice, q.BestBid, q.BestAsk, q.TsMs)
				if reason != "" {
					msg := fmt.Sprintf("CIRCUIT BREAKER TRIPPED: %s — %s", q.Symbol, reason)
					slog.Warn(msg)
					bus.Publish(ctx, eventbus.StreamRisk, map[string]interface{}{
						"type": "CIRCUIT_BREAKER", "symbol": q.Symbol, "reason": reason,
						"ts_ms": time.Now().UnixMilli(),
					})
					alerter.SendCritical(ctx, "circuit-breaker", msg)
				}
			}

		case <-stateTicker.C:
			state := daemon.CurrentState()
			bus.Publish(ctx, eventbus.StreamRisk, state)
			bus.SetString(ctx, "risk:mode", state.Mode.String(), 0)

			// Publish circuit breaker state
			cbOpen := cb.IsOpenAny()
			bus.SetString(ctx, "circuit_breaker:open", fmt.Sprintf("%t", cbOpen), 30*time.Second)

			// Publish margin ratio
			marginRatio := marginMon.CurrentRatioPct()
			bus.SetString(ctx, "margin:ratio_pct", fmt.Sprintf("%.2f", marginRatio), 30*time.Second)

			slog.Info("risk state",
				"mode", state.Mode,
				"drawdown", fmt.Sprintf("%.2f%%", state.DrawdownPct),
				"daily_dd", fmt.Sprintf("%.2f%%", state.DailyDrawdownPct),
				"equity", fmt.Sprintf("%.2f", state.CurrentEquityUSD),
				"margin_ratio", fmt.Sprintf("%.1f%%", marginRatio),
				"cb_open", cbOpen)

		case <-sig:
			slog.Info("risk-daemon: shutting down")
			alerter.Send(ctx, alerting.Alert{
				Level: "INFO", Source: "risk-daemon", Message: "shutting down",
			})
			cancel()
			return
		}
	}
}

// publishCBState periodically publishes circuit breaker state to Redis.
func publishCBState(ctx context.Context, bus *eventbus.Bus, cb *risk.CircuitBreaker) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			open := cb.IsOpenAny()
			bus.SetString(ctx, "circuit_breaker:open", fmt.Sprintf("%t", open), 10*time.Second)
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Ensure decimal import is used (equity is decimal.Decimal).
var _ = decimal.Zero
