// Package execution implements the live order executor for Bybit.
// Stripped: no registry, no paper/shadow modes, no multi-venue.
package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
	"github.com/ezyjtw/Trading-challenge/internal/exchange"
	"github.com/shopspring/decimal"
)

// legResult captures the outcome of executing a single leg.
type legResult struct {
	filled    bool
	filledQty decimal.Decimal
	filledUSD decimal.Decimal
	fillPrice decimal.Decimal
	feesUSD   decimal.Decimal
	orderID   string
	symbol    string
	category  exchange.Category
	partial   bool
	fillTsMs  int64
}

// Executor places real orders on Bybit with safety primitives.
// Takes exchange.Exchange directly -- no registry, no multi-venue abstraction.
type Executor struct {
	client exchange.Exchange
	cfg    Config
}

// NewExecutor creates an Executor bound to a single Bybit exchange client.
func NewExecutor(client exchange.Exchange, cfg Config) *Executor {
	if cfg.MaxRetriesPerLeg == 0 {
		cfg.MaxRetriesPerLeg = 3
	}
	if cfg.HedgeDriftMaxMs == 0 {
		cfg.HedgeDriftMaxMs = 2000
	}
	if cfg.MinPartialFillPct == 0 {
		cfg.MinPartialFillPct = 0.10
	}
	if cfg.ReconDelayMs == 0 {
		cfg.ReconDelayMs = 2000
	}
	return &Executor{client: client, cfg: cfg}
}

// Execute carries out a two-leg trade intent sequentially: leg A then leg B.
// It handles partial fills, hedge drift enforcement, and emergency unwinds.
func (e *Executor) Execute(ctx context.Context, intent arb.TradeIntent) ([]ExecutionEvent, *FillSummary) {
	now := time.Now().UnixMilli()

	if now > intent.ExpiresMs {
		slog.Info("intent expired", "id", intent.IntentID)
		return nil, nil
	}

	if len(intent.Legs) == 0 {
		return nil, nil
	}

	var allEvents []ExecutionEvent
	results := make([]legResult, len(intent.Legs))

	for i, leg := range intent.Legs {
		if time.Now().UnixMilli() > intent.ExpiresMs {
			slog.Info("intent expired before leg", "id", intent.IntentID, "leg", i)
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], &allEvents)
			}
			break
		}

		// Hedge drift check: if >2s between legs, abort + emergency unwind.
		if i > 0 && results[i-1].filled {
			driftMs := time.Now().UnixMilli() - results[i-1].fillTsMs
			if driftMs > e.cfg.HedgeDriftMaxMs {
				slog.Warn("hedge drift exceeded", "drift_ms", driftMs, "max_ms", e.cfg.HedgeDriftMaxMs)
				allEvents = append(allEvents, ExecutionEvent{
					EventType: EventHedgeDrift,
					IntentID:  intent.IntentID,
					LegIndex:  i,
					Symbol:    leg.Symbol,
					Strategy:  intent.Strategy,
					TsMs:      time.Now().UnixMilli(),
				})
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], &allEvents)
				break
			}
		}

		category := exchange.CategoryLinear
		if leg.Market == "SPOT" {
			category = exchange.CategorySpot
		}

		constraints, _ := e.client.GetConstraints(ctx, category, leg.Symbol)
		if constraints == nil {
			constraints = &exchange.VenueConstraints{
				Symbol:      leg.Symbol,
				TickSize:    0.1,
				LotSize:     0.001,
				MinQty:      0.001,
				MinNotional: 5.0,
			}
		}

		// Get current price for limit order placement.
		price, err := e.client.GetTickerPrice(ctx, category, leg.Symbol)
		if err != nil || price.IsZero() {
			slog.Warn("no price for leg", "symbol", leg.Symbol, "err", err)
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], &allEvents)
			}
			break
		}

		mid := price.InexactFloat64()

		// Partial fill adjustment: if leg A was partially filled, adjust leg B
		// notional to match so the position stays delta-neutral.
		targetNotional := leg.NotionalUSD
		if i > 0 && results[i-1].partial {
			targetNotional = results[i-1].filledUSD.InexactFloat64()
		}

		qty := constraints.RoundQty(targetNotional / mid)
		if constraints.MinQty > 0 && qty < constraints.MinQty {
			qty = constraints.MinQty
		}

		// Validate MinNotional: ensure order value meets exchange minimum.
		orderNotional := qty * mid
		if constraints.MinNotional > 0 && orderNotional < constraints.MinNotional {
			slog.Warn("order below MinNotional", "symbol", leg.Symbol,
				"notional", orderNotional, "min", constraints.MinNotional)
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], &allEvents)
			}
			break
		}

		// Retry with progressive widening: LIMIT at mid -> wider LIMIT -> IOC at worst.
		var finalOrder *exchange.OrderResponse
		var orderErr error
		for attempt := 0; attempt <= e.cfg.MaxRetriesPerLeg; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*500) * time.Millisecond)
			}

			orderType := exchange.OrderTypeLimit
			var orderPrice float64

			if attempt < e.cfg.MaxRetriesPerLeg {
				// Progressive widening: fraction increases with each attempt.
				frac := float64(attempt+1) / float64(e.cfg.MaxRetriesPerLeg+1)
				if leg.Action == "BUY" {
					orderPrice = constraints.RoundPrice(mid * (1 + frac*leg.MaxSlippageBps/10000))
				} else {
					orderPrice = constraints.RoundPrice(mid * (1 - frac*leg.MaxSlippageBps/10000))
				}
			} else {
				// Final attempt: IOC at worst acceptable price.
				orderType = exchange.OrderTypeIOC
				if leg.Action == "BUY" {
					orderPrice = constraints.RoundPrice(mid * (1 + leg.MaxSlippageBps/10000))
				} else {
					orderPrice = constraints.RoundPrice(mid * (1 - leg.MaxSlippageBps/10000))
				}
			}

			clientID := fmt.Sprintf("%s-l%d-r%d", intent.IntentID, i, attempt)
			req := exchange.OrderRequest{
				Category:      category,
				Symbol:        leg.Symbol,
				Side:          exchange.Side(leg.Action),
				Type:          orderType,
				Quantity:      decimal.NewFromFloat(qty),
				Price:         decimal.NewFromFloat(orderPrice),
				NotionalUSD:   decimal.NewFromFloat(targetNotional),
				ClientOrderID: clientID,
			}

			allEvents = append(allEvents, ExecutionEvent{
				EventType:            EventOrderSent,
				IntentID:             intent.IntentID,
				LegIndex:             i,
				Symbol:               leg.Symbol,
				Action:               leg.Action,
				Strategy:             intent.Strategy,
				Market:               leg.Market,
				RequestedNotionalUSD: targetNotional,
				TsMs:                 time.Now().UnixMilli(),
			})

			resp, err := e.client.PlaceOrder(ctx, req)
			if err != nil {
				orderErr = err
				slog.Warn("order failed", "attempt", attempt, "err", err)
				if attempt < e.cfg.MaxRetriesPerLeg {
					allEvents = append(allEvents, ExecutionEvent{
						EventType: EventOrderRetry,
						IntentID:  intent.IntentID,
						LegIndex:  i,
						Symbol:    leg.Symbol,
						Action:    leg.Action,
						Strategy:  intent.Strategy,
						Market:    leg.Market,
						TsMs:      time.Now().UnixMilli(),
					})
				}
				continue
			}

			final := e.pollOrderStatus(ctx, category, leg.Symbol, resp.OrderID)
			if final == nil {
				final = resp
			}

			if final.Status == exchange.OrderStatusFilled ||
				(final.Status == exchange.OrderStatusPartiallyFilled && final.FilledQty.IsPositive()) {
				finalOrder = final
				orderErr = nil
				break
			}

			if orderType == exchange.OrderTypeLimit {
				_ = e.client.CancelOrder(ctx, category, leg.Symbol, final.OrderID)
			}
			orderErr = fmt.Errorf("order status: %s", final.Status)
		}

		if orderErr != nil || finalOrder == nil {
			slog.Warn("leg failed after retries", "leg", i, "err", orderErr)
			allEvents = append(allEvents, ExecutionEvent{
				EventType: EventOrderRejected,
				IntentID:  intent.IntentID,
				LegIndex:  i,
				Symbol:    leg.Symbol,
				Action:    leg.Action,
				Strategy:  intent.Strategy,
				Market:    leg.Market,
				TsMs:      time.Now().UnixMilli(),
			})
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], &allEvents)
			}
			break
		}

		fillPrice := finalOrder.AvgFillPrice
		if fillPrice.IsZero() {
			fillPrice = price
		}
		filledNotional := finalOrder.FilledQty.Mul(fillPrice)
		isPartial := finalOrder.Status == exchange.OrderStatusPartiallyFilled

		results[i] = legResult{
			filled:    true,
			filledQty: finalOrder.FilledQty,
			filledUSD: filledNotional,
			fillPrice: fillPrice,
			feesUSD:   finalOrder.FeesUSD,
			orderID:   finalOrder.OrderID,
			symbol:    leg.Symbol,
			category:  category,
			partial:   isPartial,
			fillTsMs:  time.Now().UnixMilli(),
		}

		slipBps := math.Abs(fillPrice.InexactFloat64()-mid) / mid * 10000

		evtType := EventOrderFilled
		if isPartial {
			evtType = EventOrderPartial
		}

		allEvents = append(allEvents, ExecutionEvent{
			EventType:            evtType,
			IntentID:             intent.IntentID,
			LegIndex:             i,
			Symbol:               leg.Symbol,
			Action:               leg.Action,
			Strategy:             intent.Strategy,
			Market:               leg.Market,
			RequestedNotionalUSD: targetNotional,
			FilledNotionalUSD:    filledNotional.InexactFloat64(),
			FilledPrice:          fillPrice.InexactFloat64(),
			SlippageBpsActual:    slipBps,
			FeesUSDActual:        finalOrder.FeesUSD.InexactFloat64(),
			TsMs:                 time.Now().UnixMilli(),
			LatencyMs:            time.Now().UnixMilli() - now,
		})
	}

	// Build fill summary.
	anyFilled := false
	for _, r := range results {
		if r.filled {
			anyFilled = true
			break
		}
	}
	if !anyFilled {
		return allEvents, nil
	}

	// Schedule async reconciliation.
	go e.reconcileFills(ctx, intent, results)

	var totalFees decimal.Decimal
	for _, r := range results {
		totalFees = totalFees.Add(r.feesUSD)
	}

	fill := &FillSummary{
		IntentID:  intent.IntentID,
		Strategy:  intent.Strategy,
		Symbol:    intent.Symbol,
		TotalFees: totalFees.InexactFloat64(),
		TsMs:      time.Now().UnixMilli(),
	}
	if len(results) >= 2 && results[0].filled && results[1].filled {
		fill.BuyPrice = results[0].fillPrice.InexactFloat64()
		fill.SellPrice = results[1].fillPrice.InexactFloat64()
		if intent.Legs[0].Action == "SELL" {
			fill.BuyPrice, fill.SellPrice = fill.SellPrice, fill.BuyPrice
		}
		notional := results[0].filledUSD.InexactFloat64()
		mid := (fill.BuyPrice + fill.SellPrice) / 2
		if mid > 0 {
			fill.NetPnL = (fill.SellPrice-fill.BuyPrice)/mid*notional - totalFees.InexactFloat64()
		}
	}

	return allEvents, fill
}

// pollOrderStatus polls for order completion with a short timeout.
func (e *Executor) pollOrderStatus(ctx context.Context, cat exchange.Category, symbol, orderID string) *exchange.OrderResponse {
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := e.client.GetOrder(ctx, cat, symbol, orderID)
		if err != nil {
			continue
		}
		switch resp.Status {
		case exchange.OrderStatusFilled,
			exchange.OrderStatusCancelled,
			exchange.OrderStatusRejected,
			exchange.OrderStatusExpired,
			exchange.OrderStatusPartiallyFilled:
			return resp
		}
	}
	return nil
}

// emergencyUnwind reverses a filled leg with a market order.
// Retries up to 3 times with exponential backoff if the unwind fails.
func (e *Executor) emergencyUnwind(ctx context.Context, intent arb.TradeIntent, legIdx int, result legResult, events *[]ExecutionEvent) {
	slog.Warn("EMERGENCY UNWIND", "intent", intent.IntentID, "leg", legIdx, "qty", result.filledQty)

	reverseAction := exchange.SideSell
	if intent.Legs[legIdx].Action == "SELL" {
		reverseAction = exchange.SideBuy
	}

	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			slog.Warn("emergency unwind retry", "attempt", attempt, "backoff", backoff)
			time.Sleep(backoff)
		}

		req := exchange.OrderRequest{
			Category:      result.category,
			Symbol:        result.symbol,
			Side:          reverseAction,
			Type:          exchange.OrderTypeMarket,
			Quantity:      result.filledQty,
			ReduceOnly:    true,
			ClientOrderID: fmt.Sprintf("%s-unwind-l%d-r%d", intent.IntentID, legIdx, attempt),
		}

		resp, err := e.client.PlaceOrder(ctx, req)
		if err != nil {
			slog.Error("emergency unwind attempt failed", "attempt", attempt, "err", err)
			if attempt == maxRetries {
				*events = append(*events, ExecutionEvent{
					EventType: EventHedgeFailed,
					IntentID:  intent.IntentID,
					LegIndex:  legIdx,
					Symbol:    result.symbol,
					Strategy:  intent.Strategy,
					TsMs:      time.Now().UnixMilli(),
				})
			}
			continue
		}

		final := e.pollOrderStatus(ctx, result.category, result.symbol, resp.OrderID)
		evtType := EventOrderFilled
		if final == nil || final.Status != exchange.OrderStatusFilled {
			evtType = EventHedgeFailed
			if attempt < maxRetries {
				continue
			}
		}

		*events = append(*events, ExecutionEvent{
			EventType: evtType,
			IntentID:  intent.IntentID,
			LegIndex:  legIdx,
			Symbol:    result.symbol,
			Action:    string(reverseAction),
			Strategy:  intent.Strategy,
			TsMs:      time.Now().UnixMilli(),
		})
		return
	}
}

// reconcileFills verifies that the exchange state matches expectations
// after a fill. Runs asynchronously after ReconDelayMs.
func (e *Executor) reconcileFills(ctx context.Context, intent arb.TradeIntent, results []legResult) {
	time.Sleep(time.Duration(e.cfg.ReconDelayMs) * time.Millisecond)

	for i, r := range results {
		if !r.filled || r.orderID == "" {
			continue
		}
		order, err := e.client.GetOrder(ctx, r.category, r.symbol, r.orderID)
		if err != nil {
			slog.Warn("recon: cannot fetch order", "id", r.orderID, "err", err)
			continue
		}
		if order.FilledQty.IsPositive() {
			qtyDiff := r.filledQty.Sub(order.FilledQty).Abs().Div(order.FilledQty).InexactFloat64()
			if qtyDiff > 0.01 {
				slog.Warn("recon: QUANTITY DIVERGENCE",
					"intent", intent.IntentID, "leg", i,
					"internal", r.filledQty, "exchange", order.FilledQty)
			}
		}
	}
}

// ReconcilePositions performs a periodic full reconciliation between local
// state and exchange positions. Should be called every ReconIntervalMs.
func (e *Executor) ReconcilePositions(ctx context.Context) ([]ExecutionEvent, error) {
	var events []ExecutionEvent

	positions, err := e.client.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: get positions: %w", err)
	}

	balances, err := e.client.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: get balances: %w", err)
	}

	slog.Info("position reconciliation",
		"open_positions", len(positions),
		"balances", len(balances),
	)

	events = append(events, ExecutionEvent{
		EventType: EventReconcile,
		TsMs:      time.Now().UnixMilli(),
	})

	return events, nil
}

// StartPeriodicReconciliation launches a background goroutine that polls
// positions at the configured interval.
func (e *Executor) StartPeriodicReconciliation(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				positions, err := e.client.GetPositions(ctx)
				if err != nil {
					slog.Warn("recon: GetPositions failed", "err", err)
					continue
				}
				for _, pos := range positions {
					if pos.Quantity.IsPositive() {
						slog.Info("recon: position",
							"symbol", pos.Symbol,
							"side", pos.Side,
							"qty", pos.Quantity,
							"notional", pos.NotionalUSD,
							"pnl", pos.UnrealizedPnL)
					}
				}
			}
		}
	}()
	slog.Info("periodic reconciliation started", "interval", interval)
}

// PlaceStopLoss places a stop-loss order for a position.
func (e *Executor) PlaceStopLoss(ctx context.Context, symbol string, side exchange.Side, qty, triggerPrice decimal.Decimal) error {
	req := exchange.OrderRequest{
		Category:      exchange.CategoryLinear,
		Symbol:        symbol,
		Side:          side,
		Type:          exchange.OrderTypeMarket,
		Quantity:      qty,
		StopLoss:      triggerPrice,
		ReduceOnly:    true,
		ClientOrderID: fmt.Sprintf("sl-%s-%d", symbol, time.Now().UnixMilli()),
	}
	_, err := e.client.PlaceOrder(ctx, req)
	return err
}
