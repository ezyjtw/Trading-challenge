package scanner

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// GuardrailConfig defines the hard emotional guardrails.
// These are NOT configurable at runtime — they require a code change.
type GuardrailConfig struct {
	// MaxTradesPerDay is the absolute max directional trades per calendar day.
	MaxTradesPerDay int `yaml:"max_trades_per_day"`

	// MaxConsecutiveLosses: after this many consecutive losses, stop for the day.
	MaxConsecutiveLosses int `yaml:"max_consecutive_losses"`

	// DailyStopLossR: if daily PnL drops below this many R, stop for the day.
	// Expressed as negative R (e.g. -1.5 means stop after losing 1.5R).
	DailyStopLossR float64 `yaml:"daily_stop_loss_r"`

	// CooldownAfterAlertMs: mandatory wait time between alert and execution.
	// Kills impulse trades.
	CooldownAfterAlertMs int64 `yaml:"cooldown_after_alert_ms"`

	// TimeStopMinutes: if trade hasn't hit SL or TP within this time, close it.
	TimeStopMinutes int `yaml:"time_stop_minutes"`

	// WinRateReviewThreshold: if win rate drops below this over MinReviewTrades,
	// the system pauses and flags for review.
	WinRateReviewThreshold float64 `yaml:"win_rate_review_threshold"`
	MinReviewTrades        int     `yaml:"min_review_trades"`

	// RiskPerTradeR: fixed risk per trade in R (always 1R = risk amount).
	// The actual dollar amount is account_balance * RiskPerTradePct / 100.
	RiskPerTradePct float64 `yaml:"risk_per_trade_pct"`

	// TPMultipleR: take profit expressed as R-multiple.
	// Model A = 2.0 (full TP at 2R).
	TPMultipleR float64 `yaml:"tp_multiple_r"`
}

// DefaultGuardrailConfig returns the SOP-specified defaults.
func DefaultGuardrailConfig() GuardrailConfig {
	return GuardrailConfig{
		MaxTradesPerDay:        3,
		MaxConsecutiveLosses:   2,
		DailyStopLossR:        -1.5,
		CooldownAfterAlertMs:  60000, // 60 seconds
		TimeStopMinutes:       240,   // 4 hours
		WinRateReviewThreshold: 30.0,
		MinReviewTrades:        20,
		RiskPerTradePct:        0.5,
		TPMultipleR:            2.0,
	}
}

// Guardrails enforces all emotional circuit breakers.
// It is the final gate before any trade signal reaches the trader.
type Guardrails struct {
	mu  sync.Mutex
	cfg GuardrailConfig

	// Daily state (resets at UTC midnight).
	tradesToday      int
	consecutiveLosses int
	dailyPnLR        float64 // cumulative R for the day
	lastResetDate    string

	// Session state.
	halted     bool
	haltReason string

	// Alert cooldowns (alert ID → earliest execution time).
	alertCooldowns map[string]int64

	// Open trade timestamps for time-stops.
	openTradeTimestamps map[string]time.Time
}

// NewGuardrails creates a new guardrail enforcer.
func NewGuardrails(cfg GuardrailConfig) *Guardrails {
	return &Guardrails{
		cfg:                 cfg,
		lastResetDate:       utcDateStr(),
		alertCooldowns:      make(map[string]int64),
		openTradeTimestamps: make(map[string]time.Time),
	}
}

// CheckPreTrade runs ALL pre-trade guardrail checks.
// Returns (allowed, reason). If not allowed, the reason explains why.
// This is the SINGLE gate — every trade must pass through here.
func (g *Guardrails) CheckPreTrade(alertID string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.maybeResetDaily()

	// 1. System halted?
	if g.halted {
		return false, fmt.Sprintf("HALTED: %s", g.haltReason)
	}

	// 2. Max trades per day.
	if g.tradesToday >= g.cfg.MaxTradesPerDay {
		return false, fmt.Sprintf("daily trade limit reached (%d/%d)",
			g.tradesToday, g.cfg.MaxTradesPerDay)
	}

	// 3. Consecutive losses.
	if g.consecutiveLosses >= g.cfg.MaxConsecutiveLosses {
		return false, fmt.Sprintf("consecutive loss limit reached (%d/%d) — done for today",
			g.consecutiveLosses, g.cfg.MaxConsecutiveLosses)
	}

	// 4. Daily P&L stop.
	if g.dailyPnLR <= g.cfg.DailyStopLossR {
		return false, fmt.Sprintf("daily stop hit (%.1fR, limit %.1fR) — done for today",
			g.dailyPnLR, g.cfg.DailyStopLossR)
	}

	// 5. Cooldown period (impulse prevention).
	if cooldownUntil, ok := g.alertCooldowns[alertID]; ok {
		now := time.Now().UnixMilli()
		if now < cooldownUntil {
			remaining := (cooldownUntil - now) / 1000
			return false, fmt.Sprintf("cooldown active — wait %ds before executing", remaining)
		}
	}

	return true, "all checks passed"
}

// RegisterAlertReceived should be called when an alert arrives.
// It starts the mandatory cooldown before execution is allowed.
func (g *Guardrails) RegisterAlertReceived(alertID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cooldownUntil := time.Now().UnixMilli() + g.cfg.CooldownAfterAlertMs
	g.alertCooldowns[alertID] = cooldownUntil

	log.Printf("guardrails: alert %s received — cooldown until %s",
		alertID, time.UnixMilli(cooldownUntil).Format("15:04:05"))
}

// RecordTradeOpened increments the daily trade count and registers the
// open time for time-stop monitoring.
func (g *Guardrails) RecordTradeOpened(alertID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.maybeResetDaily()
	g.tradesToday++
	g.openTradeTimestamps[alertID] = time.Now()

	log.Printf("guardrails: trade opened (%d/%d today)", g.tradesToday, g.cfg.MaxTradesPerDay)
}

// RecordTradeResult records the R-result of a completed trade.
// Positive values = win, negative = loss.
func (g *Guardrails) RecordTradeResult(alertID string, resultR float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.maybeResetDaily()
	g.dailyPnLR += resultR

	if resultR < 0 {
		g.consecutiveLosses++
	} else {
		g.consecutiveLosses = 0
	}

	delete(g.openTradeTimestamps, alertID)
	delete(g.alertCooldowns, alertID)

	log.Printf("guardrails: trade result %.2fR | daily: %.2fR | consec losses: %d | trades: %d/%d",
		resultR, g.dailyPnLR, g.consecutiveLosses, g.tradesToday, g.cfg.MaxTradesPerDay)

	// Check if we should halt for the day.
	if g.consecutiveLosses >= g.cfg.MaxConsecutiveLosses {
		log.Printf("guardrails: STOPPED — %d consecutive losses", g.consecutiveLosses)
	}
	if g.dailyPnLR <= g.cfg.DailyStopLossR {
		log.Printf("guardrails: STOPPED — daily PnL %.1fR hit stop %.1fR", g.dailyPnLR, g.cfg.DailyStopLossR)
	}
}

// CheckTimeStops returns alert IDs of trades that have exceeded the time-stop.
func (g *Guardrails) CheckTimeStops() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	maxAge := time.Duration(g.cfg.TimeStopMinutes) * time.Minute
	now := time.Now()
	var expired []string

	for alertID, openTime := range g.openTradeTimestamps {
		if now.Sub(openTime) > maxAge {
			expired = append(expired, alertID)
			log.Printf("guardrails: TIME STOP for %s (open %v, max %v)",
				alertID, now.Sub(openTime).Round(time.Minute), maxAge)
		}
	}

	return expired
}

// Halt manually halts all trading.
func (g *Guardrails) Halt(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.halted = true
	g.haltReason = reason
	log.Printf("guardrails: HALTED — %s", reason)
}

// Resume clears the halt state.
func (g *Guardrails) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.halted = false
	g.haltReason = ""
	log.Printf("guardrails: resumed trading")
}

// DailyStatus returns the current daily state.
func (g *Guardrails) DailyStatus() DailyState {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.maybeResetDaily()

	canTrade := true
	reason := "ready"

	if g.halted {
		canTrade = false
		reason = g.haltReason
	} else if g.tradesToday >= g.cfg.MaxTradesPerDay {
		canTrade = false
		reason = "max trades reached"
	} else if g.consecutiveLosses >= g.cfg.MaxConsecutiveLosses {
		canTrade = false
		reason = "consecutive loss limit"
	} else if g.dailyPnLR <= g.cfg.DailyStopLossR {
		canTrade = false
		reason = "daily stop hit"
	}

	return DailyState{
		TradesToday:       g.tradesToday,
		MaxTrades:         g.cfg.MaxTradesPerDay,
		ConsecutiveLosses: g.consecutiveLosses,
		MaxConsecLosses:   g.cfg.MaxConsecutiveLosses,
		DailyPnLR:         g.dailyPnLR,
		DailyStopR:        g.cfg.DailyStopLossR,
		OpenTrades:        len(g.openTradeTimestamps),
		CanTrade:          canTrade,
		Reason:            reason,
		Date:              g.lastResetDate,
	}
}

// DailyState represents the current day's trading status.
type DailyState struct {
	TradesToday       int     `json:"trades_today"`
	MaxTrades         int     `json:"max_trades"`
	ConsecutiveLosses int     `json:"consecutive_losses"`
	MaxConsecLosses   int     `json:"max_consec_losses"`
	DailyPnLR         float64 `json:"daily_pnl_r"`
	DailyStopR        float64 `json:"daily_stop_r"`
	OpenTrades        int     `json:"open_trades"`
	CanTrade          bool    `json:"can_trade"`
	Reason            string  `json:"reason"`
	Date              string  `json:"date"`
}

// CheckPerformanceHealth evaluates if the system should pause based on
// aggregate performance (win rate too low over N trades).
func (g *Guardrails) CheckPerformanceHealth(stats PerformanceStats) (bool, string) {
	if stats.TotalTrades < g.cfg.MinReviewTrades {
		return true, fmt.Sprintf("insufficient data (%d/%d trades)", stats.TotalTrades, g.cfg.MinReviewTrades)
	}

	if stats.WinRate < g.cfg.WinRateReviewThreshold {
		return false, fmt.Sprintf("win rate %.1f%% below threshold %.1f%% over %d trades — REVIEW REQUIRED",
			stats.WinRate, g.cfg.WinRateReviewThreshold, stats.TotalTrades)
	}

	return true, fmt.Sprintf("healthy: %.1f%% win rate over %d trades", stats.WinRate, stats.TotalTrades)
}

// FormatDailyStatus returns a human-readable status string.
func FormatDailyStatus(state DailyState) string {
	status := "READY"
	if !state.CanTrade {
		status = "STOPPED"
	}

	return fmt.Sprintf("[%s] %s | Trades: %d/%d | Losses: %d/%d | PnL: %.1fR (stop: %.1fR) | Open: %d",
		state.Date, status,
		state.TradesToday, state.MaxTrades,
		state.ConsecutiveLosses, state.MaxConsecLosses,
		state.DailyPnLR, state.DailyStopR,
		state.OpenTrades)
}

// maybeResetDaily resets daily counters if we've crossed UTC midnight.
func (g *Guardrails) maybeResetDaily() {
	today := utcDateStr()
	if today != g.lastResetDate {
		log.Printf("guardrails: daily reset (was %s, now %s) — trades: %d, PnL: %.1fR",
			g.lastResetDate, today, g.tradesToday, g.dailyPnLR)
		g.tradesToday = 0
		g.consecutiveLosses = 0
		g.dailyPnLR = 0
		g.lastResetDate = today
		// Don't clear halted — that requires manual resume.
	}
}

func utcDateStr() string {
	return time.Now().UTC().Format("2006-01-02")
}
