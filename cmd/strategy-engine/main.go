// Strategy engine: autonomous delta-neutral strategy orchestrator.
// Evaluates market conditions each cycle, selects the best strategy
// (funding carry, basis reversion, or hold), and generates trade intents.
// Listens to execution events to confirm entries/exits and maintain state.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/alerting"
	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/execution"
	"github.com/ezyjtw/Trading-challenge/internal/funding"
	"github.com/ezyjtw/Trading-challenge/internal/marketdata"
	"github.com/ezyjtw/Trading-challenge/internal/strategy"
)

func main() {
	slog.Info("strategy-engine: starting")

	redisAddr := env("REDIS_ADDR", "localhost:6379")
	webhookURL := os.Getenv("ALERT_WEBHOOK_URL")

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ensure consumer groups.
	for _, stream := range []string{eventbus.StreamMarketData, eventbus.StreamExecution} {
		if err := bus.EnsureGroup(ctx, stream, "strategy-engine"); err != nil {
			slog.Error("ensure group failed", "stream", stream, "err", err)
			os.Exit(1)
		}
	}

	symbols := []string{"BTCUSDT", "ETHUSDT"}

	// --- Core components ---
	selector := strategy.NewSelector(strategy.DefaultSelectorConfig())
	basisTracker := strategy.NewBasisTracker(strategy.DefaultBasisConfig())
	regime := funding.NewRegimeDetector(funding.RegimeConfig{
		LookbackPeriods:           90,
		NegativeThresholdPct:      60.0,
		ReducePositionPct:         25.0,
		ExitAllOnPersistent:       true,
		PersistentNegativePeriods: 12,
	})

	alerter := alerting.NewAlerter(alerting.WebhookConfig{
		URL:             webhookURL,
		Enabled:         webhookURL != "",
		RateLimitPerMin: 10,
	})

	// --- Intent dedup: prevent executing the same intent twice ---
	var dedup sync.Map // intentID -> true

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Latest quotes per symbol (updated from market data stream).
	var quotesMu sync.Mutex
	latestQuotes := make(map[string]*marketdata.Quote)

	evalTicker := time.NewTicker(30 * time.Second)
	defer evalTicker.Stop()
	mdTicker := time.NewTicker(1 * time.Second)
	defer mdTicker.Stop()
	execTicker := time.NewTicker(2 * time.Second)
	defer execTicker.Stop()

	entriesPublished, exitsPublished := 0, 0

	for {
		select {
		// === Market data ingestion ===
		case <-mdTicker.C:
			msgs, err := bus.Read(ctx, eventbus.StreamMarketData, "strategy-engine", "strat-md-1", 50, 100*time.Millisecond)
			if err != nil {
				continue
			}
			quotesMu.Lock()
			for _, raw := range msgs {
				var q marketdata.Quote
				if err := json.Unmarshal(raw, &q); err != nil {
					continue
				}
				cp := q
				latestQuotes[q.Symbol] = &cp

				// Feed basis tracker.
				if q.IndexPrice > 0 && q.MarkPrice > 0 {
					basisTracker.Record(q.Symbol, q.IndexPrice, q.MarkPrice, q.TsMs)
				}

				// Feed regime detector.
				if q.FundingRate != 0 {
					regime.RecordRate(q.Symbol, q.FundingRate, q.TsMs)
				}
			}
			quotesMu.Unlock()

		// === Execution event consumption (confirm entries/exits) ===
		case <-execTicker.C:
			msgs, err := bus.Read(ctx, eventbus.StreamExecution, "strategy-engine", "strat-exec-1", 20, 100*time.Millisecond)
			if err != nil {
				continue
			}
			for _, raw := range msgs {
				// Try to parse as FillSummary (successful two-leg fill).
				var fill execution.FillSummary
				if err := json.Unmarshal(raw, &fill); err != nil || fill.IntentID == "" {
					continue
				}

				// Determine if this is an entry or exit fill.
				strat := fill.Strategy
				sym := fill.Symbol
				if strat == "" || sym == "" {
					continue
				}

				quotesMu.Lock()
				q := latestQuotes[sym]
				quotesMu.Unlock()

				if strings.HasSuffix(strat, "_exit") {
					selector.ConfirmExit(sym)
					slog.Info("strategy: position closed",
						"symbol", sym, "strategy", strat, "intent", fill.IntentID)
					alerter.Send(ctx, alerting.Alert{
						Level: "INFO", Source: "strategy-engine",
						Message: fmt.Sprintf("Position closed: %s via %s (PnL: %.2f)", sym, strat, fill.NetPnL),
					})
				} else {
					// Build condition for ConfirmEntry.
					var cond strategy.MarketCondition
					if q != nil {
						cond = basisTracker.Condition(sym, q.IndexPrice, q.MarkPrice, q.FundingRate,
							string(regime.DetectRegime(sym)), q.TsMs)
					}

					stratType := strategy.StrategyFundingCarry
					if strat == "basis" {
						stratType = strategy.StrategyBasisReversion
					}

					notional := fill.BuyPrice // approximate
					if fill.TotalFees > 0 {
						notional = (fill.BuyPrice + fill.SellPrice) / 2
					}

					selector.ConfirmEntry(sym, stratType, notional, cond)
					slog.Info("strategy: position confirmed",
						"symbol", sym, "strategy", strat, "intent", fill.IntentID)
				}
			}

		// === Strategy evaluation cycle ===
		case <-evalTicker.C:
			// Check risk mode — don't generate intents if risk is escalated.
			riskMode, _ := bus.GetString(ctx, "risk:mode")
			if riskMode == "HALTED" || riskMode == "FLATTEN" {
				slog.Info("strategy-engine: paused due to risk mode", "mode", riskMode)
				continue
			}

			// Build market conditions for all symbols.
			quotesMu.Lock()
			var conditions []strategy.MarketCondition
			for _, sym := range symbols {
				q, ok := latestQuotes[sym]
				if !ok {
					continue
				}
				regimeStr := string(regime.DetectRegime(sym))
				cond := basisTracker.Condition(sym, q.IndexPrice, q.MarkPrice, q.FundingRate, regimeStr, q.TsMs)
				conditions = append(conditions, cond)
			}
			quotesMu.Unlock()

			if len(conditions) == 0 {
				continue
			}

			// Get equity from Redis.
			accountEquity := 100000.0
			if eqVal, err := bus.GetString(ctx, "equity:current"); err == nil && eqVal != "" {
				var eq float64
				if err := json.Unmarshal([]byte(eqVal), &eq); err == nil && eq > 0 {
					accountEquity = eq
				}
			}

			// Calculate regime scale factor (min across all symbols).
			minScale := 1.0
			for _, sym := range symbols {
				scale := regime.PositionScaleFactor(sym)
				if scale < minScale {
					minScale = scale
				}
			}

			// Run the strategy selector.
			eval := selector.Evaluate(conditions, accountEquity, minScale)

			// Log decisions.
			for _, d := range eval.Decisions {
				slog.Info("strategy decision",
					"symbol", d.Symbol,
					"strategy", d.Strategy,
					"confidence", fmt.Sprintf("%.2f", d.Confidence),
					"reason", d.Reason,
					"alloc_pct", fmt.Sprintf("%.1f", d.AllocationPct))
			}

			// Publish exit intents.
			for _, intent := range eval.Exits {
				if _, loaded := dedup.LoadOrStore(intent.IntentID, true); loaded {
					continue
				}
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish exit intent failed", "err", err)
					continue
				}
				exitsPublished++
				slog.Info("strategy: exit intent emitted",
					"id", intent.IntentID, "symbol", intent.Symbol,
					"strategy", intent.Strategy, "regime_scale", minScale)
			}

			// Publish entry intents.
			for _, intent := range eval.Entries {
				if _, loaded := dedup.LoadOrStore(intent.IntentID, true); loaded {
					continue
				}
				if err := bus.Publish(ctx, eventbus.StreamTradeIntents, intent); err != nil {
					slog.Warn("publish entry intent failed", "err", err)
					continue
				}
				entriesPublished++
				slog.Info("strategy: entry intent emitted",
					"id", intent.IntentID, "symbol", intent.Symbol,
					"strategy", intent.Strategy,
					"regime_scale", minScale)

				alerter.Send(ctx, alerting.Alert{
					Level: "INFO", Source: "strategy-engine",
					Message: fmt.Sprintf("New %s intent: %s %s", intent.Strategy, intent.Symbol, intent.IntentID),
				})
			}

			// Publish strategy state for monitoring.
			positions := selector.OpenPositions()
			bus.Publish(ctx, eventbus.StreamChallenge, map[string]interface{}{
				"type":              "STRATEGY_STATE",
				"positions":         positions,
				"regime_scale":      minScale,
				"entries_published": entriesPublished,
				"exits_published":   exitsPublished,
				"ts_ms":             time.Now().UnixMilli(),
			})

		case <-sig:
			slog.Info("strategy-engine: shutting down",
				"entries", entriesPublished,
				"exits", exitsPublished,
				"open_positions", selector.PositionCount())
			alerter.Send(ctx, alerting.Alert{
				Level: "INFO", Source: "strategy-engine",
				Message: fmt.Sprintf("Shutting down. Entries: %d, Exits: %d, Open: %d",
					entriesPublished, exitsPublished, selector.PositionCount()),
			})
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
