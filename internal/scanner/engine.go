package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// EngineConfig configures the scanner engine.
type EngineConfig struct {
	// MinConfluence is the minimum number of aligned factors to emit an alert.
	MinConfluence int `yaml:"min_confluence"`

	// MinStrength is the minimum average strength across signals.
	MinStrength float64 `yaml:"min_strength"`

	// AlertValidityMinutes: how long an alert remains actionable.
	AlertValidityMinutes int `yaml:"alert_validity_minutes"`

	// CooldownPerSymbolMs: minimum time between alerts for the same symbol.
	CooldownPerSymbolMs int64 `yaml:"cooldown_per_symbol_ms"`

	// Symbols to scan.
	Symbols []string `yaml:"symbols"`

	// ScanIntervalMs: how often to run the scan loop.
	ScanIntervalMs int64 `yaml:"scan_interval_ms"`

	// AccountBalance for risk calculations (updated dynamically).
	InitialBalance float64 `yaml:"initial_balance"`

	// IsChallenge determines SL type (always market during challenge).
	IsChallenge bool `yaml:"is_challenge"`

	// Detector configs.
	FundingExtreme FundingExtremeConfig `yaml:"funding_extreme"`
	OIAbsorption   OIAbsorptionConfig   `yaml:"oi_absorption"`
	RangeVWAP      RangeVWAPConfig      `yaml:"range_vwap"`
	LiqSweep       LiqSweepReclaimConfig `yaml:"liq_sweep"`
	TrendPullback  TrendPullbackConfig  `yaml:"trend_pullback"`
	MultiTF        MultiTFConfig        `yaml:"multi_tf"`
	Risk           RiskConfig           `yaml:"risk"`
	Guardrail      GuardrailConfig      `yaml:"guardrail"`
}

// DefaultEngineConfig returns a full default configuration.
// SOP-aligned: 15m TF, BTC+ETH+SOL, 0.5% risk, 2R TP, 3 trades/day max.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		MinConfluence:        3,
		MinStrength:          0.5,
		AlertValidityMinutes: 30,
		CooldownPerSymbolMs:  300000, // 5 minutes
		Symbols:              []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"},
		ScanIntervalMs:       15000,
		InitialBalance:       100000,
		IsChallenge:          true,
		FundingExtreme:       DefaultFundingExtremeConfig(),
		OIAbsorption:         DefaultOIAbsorptionConfig(),
		RangeVWAP:            DefaultRangeVWAPConfig(),
		LiqSweep:             DefaultLiqSweepReclaimConfig(),
		TrendPullback:        DefaultTrendPullbackConfig(),
		MultiTF:              DefaultMultiTFConfig(),
		Risk:                 DefaultRiskConfig(),
		Guardrail:            DefaultGuardrailConfig(),
	}
}

// AlertCallback is called when a new signal alert is generated.
type AlertCallback func(alert SignalAlert)

// Engine is the main scanner that orchestrates detectors, scores confluences,
// and emits alerts with pre-calculated risk parameters.
type Engine struct {
	mu sync.Mutex

	cfg       EngineConfig
	detectors []Detector
	riskMgr   *RiskManager
	tracker   *OutcomeTracker
	guard     *Guardrails

	// Market state per symbol.
	states map[string]*MarketState

	// Cooldown tracking.
	lastAlert map[string]int64

	// Alert callbacks.
	callbacks []AlertCallback

	// Account balance (updated externally).
	accountBalance float64

	// Alert counter for ID generation.
	alertSeq int

	cancel context.CancelFunc
}

// NewEngine creates a fully configured scanner engine.
func NewEngine(cfg EngineConfig) *Engine {
	e := &Engine{
		cfg:            cfg,
		riskMgr:        NewRiskManager(cfg.Risk),
		tracker:        NewOutcomeTracker("trade_history.json", 500),
		guard:          NewGuardrails(cfg.Guardrail),
		states:         make(map[string]*MarketState),
		lastAlert:      make(map[string]int64),
		accountBalance: cfg.InitialBalance,
	}

	// Initialize detectors.
	e.detectors = []Detector{
		NewFundingExtremeDetector(cfg.FundingExtreme),
		NewOIAbsorptionDetector(cfg.OIAbsorption),
		NewRangeVWAPDetector(cfg.RangeVWAP),
		NewLiqSweepReclaimDetector(cfg.LiqSweep),
		NewTrendPullbackDetector(cfg.TrendPullback),
		NewMultiTFDetector(cfg.MultiTF),
	}

	// Initialize market state for each symbol.
	for _, sym := range cfg.Symbols {
		e.states[sym] = &MarketState{
			Symbol:  sym,
			Candles: make(map[Timeframe][]Candle),
		}
	}

	return e
}

// OnAlert registers a callback for new signal alerts.
func (e *Engine) OnAlert(cb AlertCallback) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.callbacks = append(e.callbacks, cb)
}

// UpdateMarketState updates the market state for a symbol.
func (e *Engine) UpdateMarketState(symbol string, price, bid, ask, markPrice, volume24h, fundingRate float64, tsMs int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.states[symbol]
	if !ok {
		return
	}

	state.Price = price
	state.BestBid = bid
	state.BestAsk = ask
	state.MarkPrice = markPrice
	state.Volume24h = volume24h
	state.FundingRate = fundingRate
	state.TsMs = tsMs
}

// UpdateCandles updates candle history for a symbol and timeframe.
func (e *Engine) UpdateCandles(symbol string, tf Timeframe, candles []Candle) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.states[symbol]
	if !ok {
		return
	}
	state.Candles[tf] = candles
}

// UpdateFundingHistory updates the funding rate history for a symbol.
func (e *Engine) UpdateFundingHistory(symbol string, history []FundingSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.states[symbol]
	if !ok {
		return
	}
	state.FundingHistory = history
}

// UpdateOIHistory updates the open interest history for a symbol.
func (e *Engine) UpdateOIHistory(symbol string, history []OpenInterestSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, ok := e.states[symbol]
	if !ok {
		return
	}
	state.OIHistory = history
}

// UpdateAccountBalance updates the current account equity.
func (e *Engine) UpdateAccountBalance(balance float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.accountBalance = balance
}

// Scan runs a single scan cycle across all symbols.
func (e *Engine) Scan() []SignalAlert {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var alerts []SignalAlert

	// Check guardrails first — if daily limits are hit, skip entire scan.
	dailyState := e.guard.DailyStatus()
	if !dailyState.CanTrade {
		log.Printf("scanner: guardrails blocked scan — %s", dailyState.Reason)
		return nil
	}

	// Check time-stops on open trades.
	expired := e.guard.CheckTimeStops()
	for _, alertID := range expired {
		log.Printf("scanner: time-stop triggered for %s", alertID)
	}

	for _, symbol := range e.cfg.Symbols {
		state, ok := e.states[symbol]
		if !ok || state.Price == 0 {
			continue
		}

		// Check cooldown.
		if last, ok := e.lastAlert[symbol]; ok {
			if now-last < e.cfg.CooldownPerSymbolMs {
				continue
			}
		}

		// Check if too many trades open.
		if e.tracker.OpenTradeCount() >= e.cfg.Risk.MaxConcurrentTrades {
			continue
		}

		// Run all detectors.
		var allSignals []Signal
		for _, det := range e.detectors {
			signals := det.Detect(state)
			allSignals = append(allSignals, signals...)
		}

		if len(allSignals) == 0 {
			continue
		}

		// Group signals by direction.
		longSignals := filterByDirection(allSignals, Long)
		shortSignals := filterByDirection(allSignals, Short)

		// Score each direction.
		if alert := e.scoreAndBuild(symbol, state, longSignals, Long, now); alert != nil {
			alerts = append(alerts, *alert)
			e.lastAlert[symbol] = now
		}
		if alert := e.scoreAndBuild(symbol, state, shortSignals, Short, now); alert != nil {
			alerts = append(alerts, *alert)
			e.lastAlert[symbol] = now
		}
	}

	// Register alerts with guardrails (starts cooldown timer) and notify callbacks.
	for _, alert := range alerts {
		e.guard.RegisterAlertReceived(alert.ID)
		for _, cb := range e.callbacks {
			cb(alert)
		}
	}

	return alerts
}

// scoreAndBuild evaluates signals for a direction and builds an alert if qualified.
func (e *Engine) scoreAndBuild(
	symbol string,
	state *MarketState,
	signals []Signal,
	direction Direction,
	now int64,
) *SignalAlert {

	if len(signals) == 0 {
		return nil
	}

	// Count unique factors.
	factorSet := make(map[ConfluenceFactor]bool)
	var totalStrength float64
	for _, s := range signals {
		factorSet[s.Factor] = true
		totalStrength += s.Strength
	}

	confluenceScore := len(factorSet)
	avgStrength := totalStrength / float64(len(signals))

	// Must meet minimum confluence and strength.
	if confluenceScore < e.cfg.MinConfluence {
		return nil
	}
	if avgStrength < e.cfg.MinStrength {
		return nil
	}

	// Compute ATR for risk management.
	var currentATR float64
	if candles, ok := state.Candles[TF15m]; ok && len(candles) > 14 {
		atrValues := ATR(candles, 14)
		if len(atrValues) > 0 {
			currentATR = atrValues[len(atrValues)-1]
		}
	}
	if currentATR == 0 {
		currentATR = state.Price * 0.005 // fallback: 0.5% of price
	}

	// Find nearest structure level for SL placement.
	structureLevel := 0.0
	if candles, ok := state.Candles[TF15m]; ok {
		swings := FindSwingPoints(candles, 3)
		if direction == Long {
			// Find nearest swing low below current price.
			for i := len(swings) - 1; i >= 0; i-- {
				if !swings[i].IsHigh && swings[i].Price < state.Price {
					structureLevel = swings[i].Price
					break
				}
			}
		} else {
			// Find nearest swing high above current price.
			for i := len(swings) - 1; i >= 0; i-- {
				if swings[i].IsHigh && swings[i].Price > state.Price {
					structureLevel = swings[i].Price
					break
				}
			}
		}
	}

	// Compute risk parameters.
	riskParams, err := e.riskMgr.ComputeRiskParams(
		direction,
		state.Price,
		currentATR,
		e.accountBalance,
		e.cfg.InitialBalance,
		confluenceScore,
		structureLevel,
	)
	if err != nil {
		log.Printf("scanner: risk computation failed for %s %s: %v", symbol, direction, err)
		return nil
	}

	// Verify trade qualifies for trading day.
	qualifies, reason := e.riskMgr.ValidateForTradingDay(riskParams.PositionSizeUSD, e.cfg.InitialBalance)
	if !qualifies {
		log.Printf("scanner: trade doesn't qualify for trading day: %s", reason)
		// Still emit the alert but note it won't count.
	}

	// SL type recommendation.
	slType := e.riskMgr.SuggestSLType(e.cfg.IsChallenge, state.Volume24h)

	e.alertSeq++
	alertID := fmt.Sprintf("scan-%s-%s-%d-%d", symbol, direction, now, e.alertSeq)

	alert := &SignalAlert{
		ID:              alertID,
		Symbol:          symbol,
		Direction:       direction,
		Signals:         signals,
		ConfluenceScore: confluenceScore,
		Risk:            riskParams,
		Timeframe:       TF15m,
		ExpiresAt:       time.Now().Add(time.Duration(e.cfg.AlertValidityMinutes) * time.Minute),
		CreatedAt:       time.Now(),
		Outcome:         OutcomePending,
	}

	log.Printf("ALERT: %s %s %s | Score: %d | Strength: %.2f | Entry: $%.2f | SL: $%.2f (%s) | TP1: $%.2f | Size: $%.0f | Risk: $%.2f (%.1f%%)",
		symbol, direction, alertID,
		confluenceScore, avgStrength,
		riskParams.Entry, riskParams.StopLoss, slType,
		riskParams.TakeProfit1,
		riskParams.PositionSizeUSD,
		riskParams.RiskUSD, riskParams.RiskPct)

	for _, s := range signals {
		log.Printf("  [%s] %s (%.2f): %s", s.Factor, direction, s.Strength, s.Detail)
	}

	if !qualifies {
		log.Printf("  WARNING: won't count toward trading day (%s)", reason)
	}

	return alert
}

// Start begins the continuous scan loop.
func (e *Engine) Start(ctx context.Context) {
	ctx, e.cancel = context.WithCancel(ctx)

	interval := time.Duration(e.cfg.ScanIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("scanner: started with %d detectors, %d symbols, interval %v, min confluence %d",
		len(e.detectors), len(e.cfg.Symbols), interval, e.cfg.MinConfluence)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			alerts := e.Scan()
			if len(alerts) > 0 {
				log.Printf("scanner: emitted %d alerts", len(alerts))
			}
		}
	}
}

// Stop halts the scan loop.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
}

// Tracker returns the outcome tracker for external monitoring.
func (e *Engine) Tracker() *OutcomeTracker {
	return e.tracker
}

// RiskManager returns the risk manager.
func (e *Engine) RiskManager() *RiskManager {
	return e.riskMgr
}

// Guardrails returns the guardrail enforcer.
func (e *Engine) Guardrails() *Guardrails {
	return e.guard
}

// AlertToJSON serializes an alert for transmission (e.g. Telegram, Discord).
func AlertToJSON(alert SignalAlert) string {
	data, err := json.MarshalIndent(alert, "", "  ")
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return string(data)
}

// FormatAlertMessage creates a human-readable alert message.
func FormatAlertMessage(alert SignalAlert) string {
	dir := "LONG"
	if alert.Direction == Short {
		dir = "SHORT"
	}

	msg := fmt.Sprintf("🎯 %s %s %s\n", dir, alert.Symbol, alert.Timeframe)
	msg += fmt.Sprintf("Score: %d confluences\n\n", alert.ConfluenceScore)

	msg += fmt.Sprintf("Entry:  $%.2f\n", alert.Risk.Entry)
	msg += fmt.Sprintf("SL:     $%.2f (%.2f%%)\n", alert.Risk.StopLoss, alert.Risk.SLDistancePct)
	msg += fmt.Sprintf("TP1:    $%.2f (%.1fR)\n", alert.Risk.TakeProfit1, alert.Risk.RiskRewardRatio)
	msg += fmt.Sprintf("TP2:    $%.2f (2R)\n", alert.Risk.TakeProfit2)
	msg += fmt.Sprintf("TP3:    $%.2f (3R)\n\n", alert.Risk.TakeProfit3)

	msg += fmt.Sprintf("Size:   $%.0f (%.4f)\n", alert.Risk.PositionSizeUSD, alert.Risk.PositionSizeQty)
	msg += fmt.Sprintf("Risk:   $%.2f (%.1f%%)\n", alert.Risk.RiskUSD, alert.Risk.RiskPct)
	msg += fmt.Sprintf("Leverage: %.0fx\n\n", alert.Risk.Leverage)

	msg += "Confluences:\n"
	for _, s := range alert.Signals {
		msg += fmt.Sprintf("  [%.0f%%] %s: %s\n", s.Strength*100, s.Factor, s.Detail)
	}

	msg += fmt.Sprintf("\nExpires: %s", alert.ExpiresAt.Format("15:04 UTC"))

	return msg
}

// filterByDirection returns only signals matching the given direction.
func filterByDirection(signals []Signal, dir Direction) []Signal {
	var filtered []Signal
	for _, s := range signals {
		if s.Direction == dir {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
