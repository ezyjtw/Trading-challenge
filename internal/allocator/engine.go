package allocator

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/ezyjtw/Trading-challenge/internal/arb"
)

// Engine is the capital allocator for the HyroTrader challenge.
// It validates trade intents against allocation limits and tracks notional
// usage per strategy.
type Engine struct {
	mu  sync.Mutex
	cfg Config

	// Current account equity (updated via UpdateEquity).
	currentEquity float64

	// Notional allocated per strategy.
	fundingNotional float64
	basisNotional   float64

	// Peak equity for drawdown calculation.
	peakEquity  float64
	drawdownPct float64
}

// NewEngine creates a new capital allocator with the given config.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:           cfg,
		currentEquity: cfg.InitialCapitalUSD,
		peakEquity:    cfg.InitialCapitalUSD,
	}
}

// Approve validates a trade intent against allocation limits.
// Returns (true, "") if approved, or (false, reason) if rejected.
func (e *Engine) Approve(intent arb.TradeIntent) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Calculate per-leg notional. Each leg of a hedged pair has the same
	// notional; use the first leg as the allocation unit.
	totalIntentNotional := 0.0
	for _, leg := range intent.Legs {
		totalIntentNotional += leg.NotionalUSD
	}
	perLegNotional := totalIntentNotional
	if len(intent.Legs) == 2 {
		perLegNotional = totalIntentNotional / 2
	}

	maxCapital := e.currentEquity
	if maxCapital <= 0 {
		maxCapital = e.cfg.InitialCapitalUSD
	}

	strategy := strings.ToUpper(intent.Strategy)

	// Exit intents are always approved (they reduce exposure).
	if strings.Contains(strategy, "EXIT") {
		return true, ""
	}
	// Safety trades are always approved.
	if strings.Contains(strategy, "SAFETY") {
		return true, ""
	}

	// Apply drawdown scaling: reduce allocations as drawdown increases.
	scaleFactor := 1.0
	if e.cfg.DrawdownScaling && e.drawdownPct > 0 {
		// Linearly reduce: at 5% drawdown, scale to 50%; floor at 25%.
		scaleFactor = math.Max(0.25, 1.0-(e.drawdownPct/10.0))
	}

	// ── Strategy-level cap ───────────────────────────────────────────────
	switch {
	case strings.Contains(strategy, "FUNDING"):
		maxAlloc := maxCapital * e.cfg.FundingMaxPct / 100 * scaleFactor
		if e.fundingNotional+perLegNotional > maxAlloc {
			return false, fmt.Sprintf("funding allocation exceeded: current=%.0f + intent=%.0f > max=%.0f",
				e.fundingNotional, perLegNotional, maxAlloc)
		}

	case strings.Contains(strategy, "BASIS"):
		maxAlloc := maxCapital * e.cfg.BasisMaxPct / 100 * scaleFactor
		if e.basisNotional+perLegNotional > maxAlloc {
			return false, fmt.Sprintf("basis allocation exceeded: current=%.0f + intent=%.0f > max=%.0f",
				e.basisNotional, perLegNotional, maxAlloc)
		}

	default:
		return false, fmt.Sprintf("unknown strategy: %s", intent.Strategy)
	}

	// ── Combined cap: funding + basis <= (100 - cash_reserve)% ───────────
	combinedMax := maxCapital * (100 - e.cfg.CashReservePct) / 100 * scaleFactor
	totalDeployed := e.fundingNotional + e.basisNotional + perLegNotional
	if totalDeployed > combinedMax {
		return false, fmt.Sprintf("combined allocation exceeded: %.0f > %.0f", totalDeployed, combinedMax)
	}

	// ── Cash reserve ─────────────────────────────────────────────────────
	cashReserve := maxCapital * e.cfg.CashReservePct / 100
	available := maxCapital - e.fundingNotional - e.basisNotional - cashReserve
	if perLegNotional > available {
		return false, fmt.Sprintf("insufficient available capital: requested=%.0f > available=%.0f",
			perLegNotional, available)
	}

	// ── Approved: record allocation ──────────────────────────────────────
	switch {
	case strings.Contains(strategy, "FUNDING"):
		e.fundingNotional += perLegNotional
	case strings.Contains(strategy, "BASIS"):
		e.basisNotional += perLegNotional
	}

	slog.Info("intent approved",
		"id", intent.IntentID,
		"strategy", intent.Strategy,
		"notional", perLegNotional,
		"funding_total", e.fundingNotional,
		"basis_total", e.basisNotional,
		"scale_factor", scaleFactor)

	return true, ""
}

// ReleaseNotional releases allocated notional when a position is closed.
func (e *Engine) ReleaseNotional(strategy string, notionalUSD float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s := strings.ToUpper(strategy)
	switch {
	case strings.Contains(s, "FUNDING"):
		e.fundingNotional -= notionalUSD
		if e.fundingNotional < 0 {
			e.fundingNotional = 0
		}
	case strings.Contains(s, "BASIS"):
		e.basisNotional -= notionalUSD
		if e.basisNotional < 0 {
			e.basisNotional = 0
		}
	}
}

// UpdateEquity updates the capital pool tracking with current equity and
// drawdown percentage.
func (e *Engine) UpdateEquity(equity float64, drawdownPct float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentEquity = equity
	e.drawdownPct = drawdownPct
	if equity > e.peakEquity {
		e.peakEquity = equity
	}
}

// CurrentAllocations returns a snapshot of current allocation state.
func (e *Engine) CurrentAllocations() (fundingNotional, basisNotional, equity, peakEquity float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fundingNotional, e.basisNotional, e.currentEquity, e.peakEquity
}

// AvailableCapital returns the remaining capital available for new allocations.
func (e *Engine) AvailableCapital() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	cashReserve := e.currentEquity * e.cfg.CashReservePct / 100
	available := e.currentEquity - e.fundingNotional - e.basisNotional - cashReserve
	if available < 0 {
		return 0
	}
	return available
}

// DrawdownPct returns the current drawdown percentage.
func (e *Engine) DrawdownPct() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drawdownPct
}
