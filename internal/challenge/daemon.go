package challenge

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// ---------------------------------------------------------------------------
// Position tracking
// ---------------------------------------------------------------------------

// TrackedPosition holds metadata for an open position that is subject to
// stop-loss deadline enforcement.
type TrackedPosition struct {
	Symbol       string
	OpenTime     time.Time
	Deadline     time.Time
	HasStopLoss  bool
	StopLossSet  time.Time
	NotionalUSD  decimal.Decimal
	MarginUSD    decimal.Decimal
}

// ---------------------------------------------------------------------------
// Daemon
// ---------------------------------------------------------------------------

// Daemon is the central challenge compliance engine. It intercepts trade
// intents, tracks equity in real time, enforces all HyroTrader rules, and
// manages the safety-trade clock.
//
// All public methods are safe for concurrent use.
type Daemon struct {
	mu  sync.RWMutex
	cfg *Config

	// Equity tracking.
	currentEquity  decimal.Decimal
	dailyHighWater decimal.Decimal
	lastResetDate  string // "2006-01-02" in UTC

	// Profit tracking (closed trades).
	totalProfit    decimal.Decimal // sum of all positive closed-trade PnLs
	totalLoss      decimal.Decimal // sum of all negative closed-trade PnLs (stored as positive)
	closedTradeCount int

	// Exposure tracking (open positions).
	positions      map[string]*TrackedPosition // keyed by symbol
	totalMargin    decimal.Decimal
	totalNotional  decimal.Decimal

	// Trading-day tracking.
	tradingDays    map[string]bool // date string -> traded?
	tradedToday    bool
	todayDateStr   string

	// Halted indicates the daemon has tripped a breach and will reject
	// all further intents until manually reset.
	halted       bool
	haltedReason string
}

// NewDaemon creates a new challenge Daemon with the given configuration.
// The daemon starts with equity equal to InitialBalance and the daily
// high-water mark set to the same value.
func NewDaemon(cfg *Config) *Daemon {
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")

	return &Daemon{
		cfg:            cfg,
		currentEquity:  cfg.InitialBalance,
		dailyHighWater: cfg.InitialBalance,
		lastResetDate:  dateStr,
		totalProfit:    decimal.Zero,
		totalLoss:      decimal.Zero,
		positions:      make(map[string]*TrackedPosition),
		totalMargin:    decimal.Zero,
		totalNotional:  decimal.Zero,
		tradingDays:    make(map[string]bool),
		tradedToday:    false,
		todayDateStr:   dateStr,
	}
}

// ---------------------------------------------------------------------------
// Daily reset
// ---------------------------------------------------------------------------

// resetDailyIfNeeded checks whether the UTC date has changed since the last
// reset and, if so, resets the daily high-water mark to the current equity
// and clears the traded-today flag.
//
// Must be called with d.mu held (write lock).
func (d *Daemon) resetDailyIfNeeded() {
	dateStr := time.Now().UTC().Format("2006-01-02")
	if dateStr == d.lastResetDate {
		return
	}

	// Roll the day forward.
	d.dailyHighWater = d.currentEquity
	d.lastResetDate = dateStr
	d.tradedToday = false
	d.todayDateStr = dateStr

	log.Printf("[challenge] daily reset: date=%s high_water=%s equity=%s",
		dateStr, d.dailyHighWater.StringFixed(2), d.currentEquity.StringFixed(2))
}

// ---------------------------------------------------------------------------
// Public: equity updates
// ---------------------------------------------------------------------------

// UpdateEquity is called by the equity feed to supply the latest account
// equity value. It updates the daily high-water mark and checks both daily
// and overall drawdown rules. If a breach-level drawdown is detected the
// daemon enters a halted state and all future intents will be rejected.
func (d *Daemon) UpdateEquity(equity decimal.Decimal) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetDailyIfNeeded()
	d.currentEquity = equity

	// Update daily high-water mark.
	if equity.GreaterThan(d.dailyHighWater) {
		d.dailyHighWater = equity
	}

	// Check daily drawdown.
	dailyRes, dailyErr := CheckDailyDrawdown(equity, d.dailyHighWater, d.cfg)
	if dailyErr != nil {
		log.Printf("[challenge] daily drawdown alert: level=%s pct=%s err=%v",
			dailyRes.Level, dailyRes.DrawdownPct.StringFixed(2), dailyErr)
		if dailyRes.Level == DrawdownBreach {
			d.halt(dailyErr.Error())
			return dailyErr
		}
	}

	// Check overall drawdown.
	overallRes, overallErr := CheckOverallDrawdown(equity, d.cfg.InitialBalance, d.cfg)
	if overallErr != nil {
		log.Printf("[challenge] overall drawdown alert: level=%s pct=%s err=%v",
			overallRes.Level, overallRes.DrawdownPct.StringFixed(2), overallErr)
		if overallRes.Level == DrawdownBreach {
			d.halt(overallErr.Error())
			return overallErr
		}
	}

	// Return the most severe non-breach error for callers that want to log warnings.
	if dailyErr != nil {
		return dailyErr
	}
	return overallErr
}

// ---------------------------------------------------------------------------
// Public: trade recording
// ---------------------------------------------------------------------------

// RecordTrade records a closed trade's PnL for profit distribution tracking
// and marks today as a trading day.
func (d *Daemon) RecordTrade(tradePnL decimal.Decimal) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetDailyIfNeeded()
	d.closedTradeCount++

	// Mark today as a trading day.
	d.tradedToday = true
	d.tradingDays[d.todayDateStr] = true

	// Accumulate profit and loss separately.
	if tradePnL.GreaterThan(decimal.Zero) {
		// Check profit distribution before accepting.
		if err := CheckProfitDistribution(tradePnL, d.totalProfit, d.cfg); err != nil {
			log.Printf("[challenge] profit distribution warning: %v", err)
			// This is a warning -- the trade already happened, but we log it.
			// The pre-trade check in ValidateIntent should have caught this.
		}
		d.totalProfit = d.totalProfit.Add(tradePnL)
	} else if tradePnL.LessThan(decimal.Zero) {
		d.totalLoss = d.totalLoss.Add(tradePnL.Abs())
	}

	log.Printf("[challenge] trade recorded: pnl=%s total_profit=%s total_loss=%s closed=%d trading_days=%d",
		tradePnL.StringFixed(2), d.totalProfit.StringFixed(2), d.totalLoss.StringFixed(2),
		d.closedTradeCount, len(d.tradingDays))

	return nil
}

// ---------------------------------------------------------------------------
// Public: position tracking
// ---------------------------------------------------------------------------

// TrackPosition registers an open position for stop-loss deadline enforcement
// and exposure tracking.
func (d *Daemon) TrackPosition(symbol string, notionalUSD, marginUSD decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	info := CheckStopLossRequired(now, d.cfg)

	d.positions[symbol] = &TrackedPosition{
		Symbol:      symbol,
		OpenTime:    now,
		Deadline:    info.Deadline,
		HasStopLoss: false,
		NotionalUSD: notionalUSD,
		MarginUSD:   marginUSD,
	}

	d.recalcExposure()

	log.Printf("[challenge] position tracked: symbol=%s notional=%s margin=%s deadline=%s",
		symbol, notionalUSD.StringFixed(2), marginUSD.StringFixed(2),
		info.Deadline.Format(time.RFC3339))
}

// ConfirmStopLoss marks a tracked position as having a stop-loss attached.
func (d *Daemon) ConfirmStopLoss(symbol string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if pos, ok := d.positions[symbol]; ok {
		pos.HasStopLoss = true
		pos.StopLossSet = time.Now().UTC()
	}
}

// ClosePosition removes a position from active tracking.
func (d *Daemon) ClosePosition(symbol string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.positions, symbol)
	d.recalcExposure()
}

// recalcExposure recomputes totalMargin and totalNotional from tracked positions.
// Must be called with d.mu held.
func (d *Daemon) recalcExposure() {
	d.totalMargin = decimal.Zero
	d.totalNotional = decimal.Zero
	for _, pos := range d.positions {
		d.totalMargin = d.totalMargin.Add(pos.MarginUSD)
		d.totalNotional = d.totalNotional.Add(pos.NotionalUSD)
	}
}

// ---------------------------------------------------------------------------
// Public: intent validation
// ---------------------------------------------------------------------------

// ValidateIntent checks all challenge rules against the proposed trade intent.
// It returns nil if the intent is allowed, or an error describing the first
// rule violation encountered.
//
// The caller should only proceed with order execution if ValidateIntent
// returns nil.
func (d *Daemon) ValidateIntent(intent arb.TradeIntent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetDailyIfNeeded()

	// 1. Halted -- reject everything.
	if d.halted {
		return fmt.Errorf("challenge halted: %s", d.haltedReason)
	}

	// 2. Challenge must be enabled.
	if !d.cfg.Enabled {
		return nil // passthrough when challenge compliance is disabled
	}

	// 3. Daily drawdown check.
	if _, err := CheckDailyDrawdown(d.currentEquity, d.dailyHighWater, d.cfg); err != nil {
		return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
	}

	// 4. Overall drawdown check.
	if _, err := CheckOverallDrawdown(d.currentEquity, d.cfg.InitialBalance, d.cfg); err != nil {
		return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
	}

	// 5. Per-trade risk check.
	// Compute the total notional of this intent across all legs.
	intentNotional := decimal.Zero
	for _, leg := range intent.Legs {
		intentNotional = intentNotional.Add(decimal.NewFromFloat(leg.NotionalUSD))
	}

	// Estimate risk using the default stop-loss percentage as a proxy for
	// stop-loss distance when no explicit stop is provided. This is a
	// conservative pre-trade estimate.
	estimatedStopDistance := intentNotional.Mul(d.cfg.StopLossDefaultPct).Div(hundred)
	positionSize := decimal.NewFromInt(1) // notional-based: risk = stopDist * 1

	if err := CheckPerTradeRisk(estimatedStopDistance, positionSize, d.cfg.InitialBalance, d.cfg); err != nil {
		return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
	}

	// 6. Profit distribution check (phase1/phase2 only).
	// Use the expected net profit as a proxy for trade PnL.
	expectedPnL := decimal.NewFromFloat(intent.Expected.ProfitUSDNet)
	if expectedPnL.GreaterThan(decimal.Zero) {
		if err := CheckProfitDistribution(expectedPnL, d.totalProfit, d.cfg); err != nil {
			return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
		}
	}

	// 7. Funded-mode exposure checks.
	if d.cfg.Mode == ModeFunded {
		// Margin exposure: estimate margin as notional (conservative upper bound).
		projectedMargin := d.totalMargin.Add(intentNotional)
		if err := CheckMaxExposure(projectedMargin, d.cfg.InitialBalance, d.cfg); err != nil {
			return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
		}

		// Notional cap.
		projectedNotional := d.totalNotional.Add(intentNotional)
		if err := CheckMaxNotional(projectedNotional, d.currentEquity, d.cfg); err != nil {
			return fmt.Errorf("intent %s rejected: %w", intent.IntentID, err)
		}
	}

	// 8. Stop-loss deadline enforcement for existing positions.
	now := time.Now().UTC()
	for symbol, pos := range d.positions {
		if !pos.HasStopLoss && now.After(pos.Deadline) {
			return fmt.Errorf(
				"intent %s rejected: position %s has no stop-loss and deadline %s has passed",
				intent.IntentID, symbol, pos.Deadline.Format(time.RFC3339),
			)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Public: safety trade check
// ---------------------------------------------------------------------------

// CheckSafetyTrade returns true if it is at or past the configured safety
// trade time (e.g. 22:00 UTC) and no trade has been recorded today. The
// caller should place a minimum-size trade to satisfy the minimum trading
// days requirement.
func (d *Daemon) CheckSafetyTrade() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.tradedToday {
		return false
	}

	safetyTime, err := d.cfg.ParseSafetyTradeTime()
	if err != nil {
		log.Printf("[challenge] safety trade time parse error: %v", err)
		return false
	}

	now := time.Now().UTC()
	todaySafety := time.Date(
		now.Year(), now.Month(), now.Day(),
		safetyTime.Hour(), safetyTime.Minute(), 0, 0,
		time.UTC,
	)

	return !now.Before(todaySafety)
}

// SafetyTradeSizeUSD returns the configured minimum safety trade notional.
func (d *Daemon) SafetyTradeSizeUSD() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.SafetyTradeSizeUSD
}

// ---------------------------------------------------------------------------
// Public: queries
// ---------------------------------------------------------------------------

// TradingDayCount returns the number of distinct UTC calendar days on which
// at least one trade was recorded.
func (d *Daemon) TradingDayCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.tradingDays)
}

// MinTradingDaysMet returns true if the trading day count meets or exceeds
// the configured minimum.
func (d *Daemon) MinTradingDaysMet() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.tradingDays) >= d.cfg.MinTradingDays
}

// IsHalted returns true if the daemon has tripped a breach and is no longer
// accepting intents.
func (d *Daemon) IsHalted() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.halted
}

// HaltReason returns the reason the daemon was halted, or an empty string
// if it is running normally.
func (d *Daemon) HaltReason() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.haltedReason
}

// CurrentEquity returns the last known equity value.
func (d *Daemon) CurrentEquity() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.currentEquity
}

// DailyHighWater returns the daily high-water mark.
func (d *Daemon) DailyHighWater() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.dailyHighWater
}

// TotalProfit returns the accumulated realised profit from closed trades.
func (d *Daemon) TotalProfit() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.totalProfit
}

// TotalLoss returns the accumulated realised loss from closed trades
// (always non-negative).
func (d *Daemon) TotalLoss() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.totalLoss
}

// NetPnL returns the net realised PnL (profit minus loss).
func (d *Daemon) NetPnL() decimal.Decimal {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.totalProfit.Sub(d.totalLoss)
}

// Snapshot returns a point-in-time copy of the daemon's key metrics.
type Snapshot struct {
	Equity         decimal.Decimal `json:"equity"`
	DailyHighWater decimal.Decimal `json:"daily_high_water"`
	InitialBalance decimal.Decimal `json:"initial_balance"`
	TotalProfit    decimal.Decimal `json:"total_profit"`
	TotalLoss      decimal.Decimal `json:"total_loss"`
	NetPnL         decimal.Decimal `json:"net_pnl"`
	TotalMargin    decimal.Decimal `json:"total_margin"`
	TotalNotional  decimal.Decimal `json:"total_notional"`
	TradingDays    int             `json:"trading_days"`
	OpenPositions  int             `json:"open_positions"`
	ClosedTrades   int             `json:"closed_trades"`
	Halted         bool            `json:"halted"`
	HaltReason     string          `json:"halt_reason,omitempty"`
	Mode           Mode            `json:"mode"`
}

// GetSnapshot returns a Snapshot of the daemon's current state.
func (d *Daemon) GetSnapshot() Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return Snapshot{
		Equity:         d.currentEquity,
		DailyHighWater: d.dailyHighWater,
		InitialBalance: d.cfg.InitialBalance,
		TotalProfit:    d.totalProfit,
		TotalLoss:      d.totalLoss,
		NetPnL:         d.totalProfit.Sub(d.totalLoss),
		TotalMargin:    d.totalMargin,
		TotalNotional:  d.totalNotional,
		TradingDays:    len(d.tradingDays),
		OpenPositions:  len(d.positions),
		ClosedTrades:   d.closedTradeCount,
		Halted:         d.halted,
		HaltReason:     d.haltedReason,
		Mode:           d.cfg.Mode,
	}
}

// PositionsWithExpiredStopLoss returns symbols of positions whose stop-loss
// deadline has passed without a stop-loss being set. These positions need
// immediate attention (either set a stop-loss or force-close).
func (d *Daemon) PositionsWithExpiredStopLoss() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	now := time.Now().UTC()
	var expired []string
	for symbol, pos := range d.positions {
		if !pos.HasStopLoss && now.After(pos.Deadline) {
			expired = append(expired, symbol)
		}
	}
	return expired
}

// ---------------------------------------------------------------------------
// Public: manual controls
// ---------------------------------------------------------------------------

// Reset clears the halted state, allowing the daemon to accept intents again.
// This is an operator-level action and should be called only after the cause
// of the halt has been addressed.
func (d *Daemon) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	log.Printf("[challenge] manual reset: previous halt reason=%q", d.haltedReason)
	d.halted = false
	d.haltedReason = ""
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// halt transitions the daemon into a halted state. Must be called with d.mu held.
func (d *Daemon) halt(reason string) {
	d.halted = true
	d.haltedReason = reason
	log.Printf("[challenge] HALTED: %s", reason)
}
