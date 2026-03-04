package risk

import (
	"log"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// PositionSnapshot represents a single exchange position for leg-mismatch tracking.
type PositionSnapshot struct {
	Symbol string
	Side   string          // "Buy" or "Sell"
	Size   decimal.Decimal // absolute quantity
	TsMs   int64
}

// Daemon is the risk engine. It evaluates drawdown, error rates, and leg
// mismatches, escalating mode when thresholds are breached.
//
// Escalation is one-way: RUNNING -> PAUSED -> SAFE -> FLATTEN -> HALTED.
// An operator must explicitly call SetMode to de-escalate.
type Daemon struct {
	mu  sync.Mutex
	cfg Config

	mode   Mode
	reason string

	// Equity tracking.
	peakEquityUSD    decimal.Decimal // overall high water mark (never resets)
	currentEquityUSD decimal.Decimal
	dailyPeakEquity  decimal.Decimal // resets at UTC midnight
	lastDailyReset   time.Time       // date of last daily-peak reset

	// Error rate tracking (sliding 5-minute window).
	fillEvents  []fillEvent
	errorEvents []fillEvent

	// Leg mismatch tracking.
	lastPositionSnap []PositionSnapshot
	lastSnapTsMs     int64
	unhedgedSince    int64 // 0 means no active unhedged leg

	// Challenge metadata.
	challengePhase string
}

type fillEvent struct {
	tsMs   int64
	pnlUSD decimal.Decimal
}

// NewDaemon creates a risk daemon with the given configuration.
func NewDaemon(cfg Config) *Daemon {
	phase := ""
	if cfg.ChallengeMode {
		phase = "evaluation"
	}
	return &Daemon{
		cfg:            cfg,
		mode:           ModeRunning,
		challengePhase: phase,
		lastDailyReset: utcToday(),
	}
}

// ---------- Public API (thread-safe) ----------

// RecordFill records a completed fill. pnlUSD is the realised P&L; isError
// indicates a rejected or failed order.
func (d *Daemon) RecordFill(pnlUSD decimal.Decimal, isError bool) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := nowMs()
	evt := fillEvent{tsMs: now, pnlUSD: pnlUSD}
	d.fillEvents = append(d.fillEvents, evt)
	if isError {
		d.errorEvents = append(d.errorEvents, evt)
	}

	return d.evaluate(now)
}

// UpdateEquity is called periodically (e.g. every 5 s from a Bybit account poll)
// with the latest account equity.
func (d *Daemon) UpdateEquity(equity decimal.Decimal) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := nowMs()
	d.currentEquityUSD = equity

	// Reset daily peak at UTC midnight boundary.
	today := utcToday()
	if today.After(d.lastDailyReset) {
		d.dailyPeakEquity = equity
		d.lastDailyReset = today
	}

	// Update high water marks.
	if equity.GreaterThan(d.peakEquityUSD) {
		d.peakEquityUSD = equity
	}
	if equity.GreaterThan(d.dailyPeakEquity) {
		d.dailyPeakEquity = equity
	}

	return d.evaluate(now)
}

// Tick is called periodically to let the daemon re-evaluate without new data.
func (d *Daemon) Tick() []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evaluate(nowMs())
}

// RecordPositionSnapshot records exchange positions for leg-mismatch detection.
// For cross-pair delta-neutral strategies, positions are on different symbols
// but hedged via beta weighting. We detect mismatch by checking if total net
// dollar exposure exceeds a threshold (rather than per-symbol zero-net check).
func (d *Daemon) RecordPositionSnapshot(positions []PositionSnapshot) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := nowMs()
	d.lastPositionSnap = positions
	d.lastSnapTsMs = now

	// For cross-pair strategies, positions are on different symbols but hedged.
	// Sum net dollar exposure across all positions:
	// long = positive, short = negative.
	netDollarExposure := decimal.Zero
	for _, p := range positions {
		notional := p.Size
		if p.Side == "Sell" {
			notional = notional.Neg()
		}
		netDollarExposure = netDollarExposure.Add(notional)
	}

	// Allow small imbalance (up to 5% of total size) for beta weighting imprecision.
	totalSize := decimal.Zero
	for _, p := range positions {
		totalSize = totalSize.Add(p.Size)
	}

	threshold := totalSize.Mul(decimal.NewFromFloat(0.05))
	hasUnhedged := netDollarExposure.Abs().GreaterThan(threshold) && totalSize.IsPositive()

	var alerts []Alert

	if hasUnhedged {
		if d.unhedgedSince == 0 {
			d.unhedgedSince = now
		}
		durationMs := now - d.unhedgedSince

		if durationMs >= d.cfg.LegMismatchCloseMs {
			alerts = append(alerts, d.escalateTo(ModeFlatten, now,
				"unhedged exposure exceeded close threshold"))
		} else if durationMs >= d.cfg.LegMismatchAlertMs {
			alerts = append(alerts, Alert{
				TsMs:      now,
				Source:    "risk.daemon",
				Severity:  SeverityWarn,
				Message:   "unhedged exposure detected",
				Metric:    "leg_mismatch_duration_ms",
				Value:     float64(durationMs),
				Threshold: float64(d.cfg.LegMismatchAlertMs),
			})
		}
	} else {
		d.unhedgedSince = 0
	}

	return alerts
}

// SetMode allows an operator to explicitly set the risk mode (including
// de-escalation). This is the only way to move back to a lower severity mode.
func (d *Daemon) SetMode(m Mode, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mode = m
	d.reason = reason
	log.Printf("risk: mode set to %s by operator: %s", m, reason)
}

// CurrentMode returns the current risk mode.
func (d *Daemon) CurrentMode() Mode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mode
}

// CurrentState returns a snapshot of the full risk state.
func (d *Daemon) CurrentState() State {
	d.mu.Lock()
	defer d.mu.Unlock()

	drawdownPct := 0.0
	if d.peakEquityUSD.IsPositive() {
		dd := d.peakEquityUSD.Sub(d.currentEquityUSD)
		drawdownPct, _ = dd.Div(d.peakEquityUSD).Mul(decimal.NewFromInt(100)).Float64()
	}

	dailyDDPct := 0.0
	if d.dailyPeakEquity.IsPositive() {
		dd := d.dailyPeakEquity.Sub(d.currentEquityUSD)
		dailyDDPct, _ = dd.Div(d.dailyPeakEquity).Mul(decimal.NewFromInt(100)).Float64()
	}

	peakF, _ := d.peakEquityUSD.Float64()
	curF, _ := d.currentEquityUSD.Float64()

	return State{
		Mode:             d.mode,
		TsMs:             nowMs(),
		DrawdownPct:      drawdownPct,
		DailyDrawdownPct: dailyDDPct,
		PeakEquityUSD:    peakF,
		CurrentEquityUSD: curF,
		ErrorRate5mPct:   d.errorRate5m(nowMs()),
		Reason:           d.reason,
		ChallengePhase:   d.challengePhase,
	}
}

// ---------- Internal evaluation ----------

// evaluate checks all risk metrics and escalates mode if any threshold is
// breached. Escalation is one-way within this function; de-escalation requires
// an explicit SetMode call.
func (d *Daemon) evaluate(nowMs int64) []Alert {
	var alerts []Alert

	// --- Error rate ---
	errRate := d.errorRate5m(nowMs)
	if errRate > d.cfg.MaxErrorRate5mPct {
		a := d.escalateTo(ModePaused, nowMs, "error rate exceeded threshold")
		a.Metric = "error_rate_5m_pct"
		a.Value = errRate
		a.Threshold = d.cfg.MaxErrorRate5mPct
		alerts = append(alerts, a)
	}

	// --- Overall drawdown ---
	if d.peakEquityUSD.IsPositive() && d.currentEquityUSD.IsPositive() {
		dd := d.peakEquityUSD.Sub(d.currentEquityUSD)
		ddPct, _ := dd.Div(d.peakEquityUSD).Mul(decimal.NewFromInt(100)).Float64()

		if ddPct >= d.cfg.MaxDrawdownPct {
			a := d.escalateTo(ModeHalted, nowMs, "overall drawdown exceeded max")
			a.Metric = "drawdown_pct"
			a.Value = ddPct
			a.Threshold = d.cfg.MaxDrawdownPct
			alerts = append(alerts, a)
		} else if ddPct >= d.cfg.SafeModeDrawdownPct {
			a := d.escalateTo(ModeSafe, nowMs, "overall drawdown exceeded safe threshold")
			a.Metric = "drawdown_pct"
			a.Value = ddPct
			a.Threshold = d.cfg.SafeModeDrawdownPct
			alerts = append(alerts, a)
		}
	}

	// --- Daily drawdown ---
	if d.dailyPeakEquity.IsPositive() && d.currentEquityUSD.IsPositive() {
		dd := d.dailyPeakEquity.Sub(d.currentEquityUSD)
		ddPct, _ := dd.Div(d.dailyPeakEquity).Mul(decimal.NewFromInt(100)).Float64()

		if ddPct >= d.cfg.DailyHardHaltPct {
			a := d.escalateTo(ModeHalted, nowMs, "daily drawdown exceeded hard halt")
			a.Metric = "daily_drawdown_pct"
			a.Value = ddPct
			a.Threshold = d.cfg.DailyHardHaltPct
			alerts = append(alerts, a)
		} else if ddPct >= d.cfg.DailySoftHaltPct {
			a := d.escalateTo(ModeFlatten, nowMs, "daily drawdown exceeded soft halt")
			a.Metric = "daily_drawdown_pct"
			a.Value = ddPct
			a.Threshold = d.cfg.DailySoftHaltPct
			alerts = append(alerts, a)
		}
	}

	return alerts
}

// escalateTo attempts to escalate the risk mode. If the requested mode is
// lower-or-equal severity compared to the current mode, no change is made
// (escalation only).
func (d *Daemon) escalateTo(target Mode, tsMs int64, reason string) Alert {
	sev := SeverityWarn
	if target >= ModeFlatten {
		sev = SeverityCritical
	}
	if target == ModePaused {
		sev = SeverityInfo
	}

	alert := Alert{
		TsMs:     tsMs,
		Source:   "risk.daemon",
		Severity: sev,
		Message:  reason,
	}

	// Only escalate (higher numeric value = more severe).
	if target > d.mode {
		d.mode = target
		d.reason = reason
		log.Printf("risk: escalated to %s — %s", target, reason)
	}

	return alert
}

// errorRate5m computes the error percentage over the last 5 minutes.
func (d *Daemon) errorRate5m(now int64) float64 {
	cutoff := now - 5*60*1000

	// Prune old events while we're at it.
	d.fillEvents = pruneOlderThan(d.fillEvents, cutoff)
	d.errorEvents = pruneOlderThan(d.errorEvents, cutoff)

	total := len(d.fillEvents)
	if total == 0 {
		return 0
	}
	return float64(len(d.errorEvents)) / float64(total) * 100.0
}

// pruneOlderThan removes events older than the cutoff (ms).
func pruneOlderThan(events []fillEvent, cutoffMs int64) []fillEvent {
	i := 0
	for _, e := range events {
		if e.tsMs >= cutoffMs {
			events[i] = e
			i++
		}
	}
	return events[:i]
}

// ---------- Time helpers ----------

func nowMs() int64 {
	return time.Now().UnixMilli()
}

func utcToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}
