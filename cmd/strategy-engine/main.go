// Strategy engine: autonomous delta-neutral strategy orchestrator.
// Evaluates cross-pair market conditions each cycle, selects the best strategy
// (funding carry, spread mean-reversion, or hold), and generates trade intents.
// All trades use USDT perpetual futures only (no spot).
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

// pairDef defines a cross-pair trading relationship.
type pairDef struct {
	Primary string
	Hedge   string
}

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

	// Configured cross-pair relationships.
	pairs := []pairDef{
		{Primary: "BTCUSDT", Hedge: "ETHUSDT"},
	}

	// --- Core components ---
	selector := strategy.NewSelector(strategy.DefaultSelectorConfig())
	basisTracker := strategy.NewBasisTracker(strategy.DefaultBasisConfig())
	corrTracker := strategy.NewCorrelationTracker(500) // 500-sample rolling window
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

				// Feed basis tracker (single-symbol perp premium).
				if q.IndexPrice > 0 && q.MarkPrice > 0 {
					basisTracker.RecordPremium(q.Symbol, q.IndexPrice, q.MarkPrice, q.TsMs)
				}

				// Feed regime detector.
				if q.FundingRate != 0 {
					regime.RecordRate(q.Symbol, q.FundingRate, q.TsMs)
				}
			}

			// Update cross-pair trackers with latest prices.
			for _, pair := range pairs {
				pq, pOk := latestQuotes[pair.Primary]
				hq, hOk := latestQuotes[pair.Hedge]
				if pOk && hOk {
					ts := pq.TsMs
					if hq.TsMs > ts {
						ts = hq.TsMs
					}
					corrTracker.RecordPrices(pair.Primary, pair.Hedge, pq.MarkPrice, hq.MarkPrice, ts)
					basisTracker.RecordPairSpread(pair.Primary, pair.Hedge, pq.MarkPrice, hq.MarkPrice, ts)
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
				var fill execution.FillSummary
				if err := json.Unmarshal(raw, &fill); err != nil || fill.IntentID == "" {
					continue
				}

				strat := fill.Strategy
				sym := fill.Symbol
				if strat == "" || sym == "" {
					continue
				}

				// Determine hedge symbol from the fill or intent.
				hedgeSym := fill.HedgeSymbol
				if hedgeSym == "" {
					// Fallback: infer from configured pairs.
					for _, pair := range pairs {
						if pair.Primary == sym {
							hedgeSym = pair.Hedge
							break
						}
					}
				}

				if strings.HasSuffix(strat, "_exit") {
					selector.ConfirmExit(sym, hedgeSym)
					slog.Info("strategy: position closed",
						"primary", sym, "hedge", hedgeSym,
						"strategy", strat, "intent", fill.IntentID)
					alerter.Send(ctx, alerting.Alert{
						Level: "INFO", Source: "strategy-engine",
						Message: fmt.Sprintf("Position closed: %s/%s via %s (PnL: %.2f)", sym, hedgeSym, strat, fill.NetPnL),
					})
				} else {
					// Build PairCondition for ConfirmEntry.
					quotesMu.Lock()
					pq := latestQuotes[sym]
					hq := latestQuotes[hedgeSym]
					quotesMu.Unlock()

					var cond strategy.PairCondition
					cond.PrimarySymbol = sym
					cond.HedgeSymbol = hedgeSym
					if pq != nil {
						cond.PrimaryPrice = pq.MarkPrice
						cond.PrimaryFunding = pq.FundingRate
					}
					if hq != nil {
						cond.HedgePrice = hq.MarkPrice
						cond.HedgeFunding = hq.FundingRate
					}
					if pq != nil && hq != nil {
						cond.FundingDiffBps = (pq.FundingRate - hq.FundingRate) * 10000.0
					}
					cond.Beta = corrTracker.Beta(sym, hedgeSym)
					cond.Correlation = corrTracker.Correlation(sym, hedgeSym)
					cond.SpreadZScore = basisTracker.PairSpreadZScore(sym, hedgeSym)
					cond.Regime = string(regime.DetectRegime(sym))

					stratType := strategy.StrategyFundingCarry
					if strat == "basis" {
						stratType = strategy.StrategyBasisReversion
					}

					notional := fill.BuyPrice
					if fill.TotalFees > 0 {
						notional = (fill.BuyPrice + fill.SellPrice) / 2
					}

					selector.ConfirmEntry(sym, hedgeSym, stratType, notional, cond)
					slog.Info("strategy: position confirmed",
						"primary", sym, "hedge", hedgeSym,
						"strategy", strat, "intent", fill.IntentID)
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

			// Build cross-pair conditions.
			quotesMu.Lock()
			var pairConditions []strategy.PairCondition
			for _, pair := range pairs {
				pq, pOk := latestQuotes[pair.Primary]
				hq, hOk := latestQuotes[pair.Hedge]
				if !pOk || !hOk {
					continue
				}

				cond := strategy.PairCondition{
					PrimarySymbol:  pair.Primary,
					PrimaryPrice:   pq.MarkPrice,
					PrimaryFunding: pq.FundingRate,
					HedgeSymbol:    pair.Hedge,
					HedgePrice:     hq.MarkPrice,
					HedgeFunding:   hq.FundingRate,
					FundingDiffBps: (pq.FundingRate - hq.FundingRate) * 10000.0,
					Beta:           corrTracker.Beta(pair.Primary, pair.Hedge),
					Correlation:    corrTracker.Correlation(pair.Primary, pair.Hedge),
					SpreadZScore:   basisTracker.PairSpreadZScore(pair.Primary, pair.Hedge),
					Regime:         string(regime.DetectRegime(pair.Primary)),
					TsMs:           pq.TsMs,
				}
				spreadMean, spreadStdDev, _ := basisTracker.PairSpreadStats(pair.Primary, pair.Hedge)
				cond.SpreadMean = spreadMean
				cond.SpreadStdDev = spreadStdDev

				pairConditions = append(pairConditions, cond)
			}
			quotesMu.Unlock()

			if len(pairConditions) == 0 {
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

			// Calculate regime scale factor.
			minScale := 1.0
			for _, pair := range pairs {
				for _, sym := range []string{pair.Primary, pair.Hedge} {
					scale := regime.PositionScaleFactor(sym)
					if scale < minScale {
						minScale = scale
					}
				}
			}

			// Run the strategy selector.
			eval := selector.Evaluate(pairConditions, accountEquity, minScale)

			// Log decisions.
			for _, d := range eval.Decisions {
				slog.Info("strategy decision",
					"primary", d.PrimarySymbol,
					"hedge", d.HedgeSymbol,
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
					"id", intent.IntentID, "primary", intent.Symbol,
					"hedge", intent.HedgeSymbol,
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
					"id", intent.IntentID, "primary", intent.Symbol,
					"hedge", intent.HedgeSymbol,
					"strategy", intent.Strategy,
					"regime_scale", minScale)

				alerter.Send(ctx, alerting.Alert{
					Level: "INFO", Source: "strategy-engine",
					Message: fmt.Sprintf("New %s intent: %s/%s %s", intent.Strategy, intent.Symbol, intent.HedgeSymbol, intent.IntentID),
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
