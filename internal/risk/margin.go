package risk

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// MarginConfig holds thresholds for margin ratio monitoring.
type MarginConfig struct {
	// WarnRatioPct triggers a warning when used margin / total equity exceeds this.
	WarnRatioPct float64 `yaml:"warn_ratio_pct"` // e.g. 60
	// CriticalRatioPct triggers position reduction when margin ratio exceeds this.
	CriticalRatioPct float64 `yaml:"critical_ratio_pct"` // e.g. 75
	// HaltRatioPct triggers full halt and flatten when margin ratio exceeds this.
	HaltRatioPct float64 `yaml:"halt_ratio_pct"` // e.g. 85
	// LiqPriceBufferPct is the minimum buffer between mark price and liquidation
	// price as a percentage of mark price. If buffer drops below this, escalate.
	LiqPriceBufferPct float64 `yaml:"liq_price_buffer_pct"` // e.g. 5.0
}

// DefaultMarginConfig returns conservative margin thresholds for challenge mode.
func DefaultMarginConfig() MarginConfig {
	return MarginConfig{
		WarnRatioPct:      60.0,
		CriticalRatioPct:  75.0,
		HaltRatioPct:      85.0,
		LiqPriceBufferPct: 5.0,
	}
}

// MarginSnapshot represents a point-in-time margin state from the exchange.
type MarginSnapshot struct {
	TotalEquity     decimal.Decimal
	TotalMarginUsed decimal.Decimal
	AvailableMargin decimal.Decimal
	TsMs            int64
}

// PositionMarginInfo tracks a single position's proximity to liquidation.
type PositionMarginInfo struct {
	Symbol    string
	MarkPrice decimal.Decimal
	LiqPrice  decimal.Decimal
	Side      string // "Buy" or "Sell"
	Notional  decimal.Decimal
}

// MarginMonitor tracks margin utilization and liquidation proximity.
// All public methods are safe for concurrent use.
type MarginMonitor struct {
	mu  sync.Mutex
	cfg MarginConfig

	lastSnapshot *MarginSnapshot
	positions    []PositionMarginInfo
	lastCheckMs  int64
}

// NewMarginMonitor creates a new margin monitor with the given config.
func NewMarginMonitor(cfg MarginConfig) *MarginMonitor {
	return &MarginMonitor{cfg: cfg}
}

// MarginAlert represents a margin-related alert.
type MarginAlert struct {
	TsMs      int64   `json:"ts_ms"`
	Level     string  `json:"level"` // "WARN", "CRITICAL", "HALT"
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Symbol    string  `json:"symbol,omitempty"`
	Message   string  `json:"message"`
}

// UpdateMargin ingests a new margin snapshot and checks all thresholds.
func (m *MarginMonitor) UpdateMargin(snap MarginSnapshot) []MarginAlert {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastSnapshot = &snap
	m.lastCheckMs = snap.TsMs
	var alerts []MarginAlert

	if snap.TotalEquity.IsZero() || snap.TotalEquity.IsNegative() {
		return alerts
	}

	// Margin utilization ratio = used / equity * 100.
	ratioPct, _ := snap.TotalMarginUsed.Div(snap.TotalEquity).
		Mul(decimal.NewFromInt(100)).Float64()

	if ratioPct >= m.cfg.HaltRatioPct {
		alerts = append(alerts, MarginAlert{
			TsMs:      snap.TsMs,
			Level:     "HALT",
			Metric:    "margin_ratio_pct",
			Value:     ratioPct,
			Threshold: m.cfg.HaltRatioPct,
			Message:   "margin ratio exceeded halt threshold — flatten all positions",
		})
	} else if ratioPct >= m.cfg.CriticalRatioPct {
		alerts = append(alerts, MarginAlert{
			TsMs:      snap.TsMs,
			Level:     "CRITICAL",
			Metric:    "margin_ratio_pct",
			Value:     ratioPct,
			Threshold: m.cfg.CriticalRatioPct,
			Message:   "margin ratio critical — reduce position size immediately",
		})
	} else if ratioPct >= m.cfg.WarnRatioPct {
		alerts = append(alerts, MarginAlert{
			TsMs:      snap.TsMs,
			Level:     "WARN",
			Metric:    "margin_ratio_pct",
			Value:     ratioPct,
			Threshold: m.cfg.WarnRatioPct,
			Message:   "margin ratio elevated",
		})
	}

	return alerts
}

// UpdatePositions ingests exchange positions and checks liquidation proximity.
func (m *MarginMonitor) UpdatePositions(positions []PositionMarginInfo) []MarginAlert {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.positions = positions
	now := time.Now().UnixMilli()
	var alerts []MarginAlert

	for _, pos := range positions {
		if pos.MarkPrice.IsZero() || pos.LiqPrice.IsZero() {
			continue
		}

		markF, _ := pos.MarkPrice.Float64()
		liqF, _ := pos.LiqPrice.Float64()
		if markF <= 0 {
			continue
		}

		// Buffer = |mark - liq| / mark * 100.
		bufferPct := math.Abs(markF-liqF) / markF * 100.0

		if bufferPct < m.cfg.LiqPriceBufferPct {
			alerts = append(alerts, MarginAlert{
				TsMs:      now,
				Level:     "CRITICAL",
				Metric:    "liq_price_buffer_pct",
				Value:     bufferPct,
				Threshold: m.cfg.LiqPriceBufferPct,
				Symbol:    pos.Symbol,
				Message:   "position approaching liquidation price — add margin or reduce",
			})
		}
	}

	return alerts
}

// CurrentRatioPct returns the last known margin utilization ratio.
func (m *MarginMonitor) CurrentRatioPct() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lastSnapshot == nil || m.lastSnapshot.TotalEquity.IsZero() {
		return 0
	}
	r, _ := m.lastSnapshot.TotalMarginUsed.Div(m.lastSnapshot.TotalEquity).
		Mul(decimal.NewFromInt(100)).Float64()
	return r
}

// NeedsReducePosition returns true if the margin ratio is above the critical threshold.
func (m *MarginMonitor) NeedsReducePosition() bool {
	return m.CurrentRatioPct() >= m.cfg.CriticalRatioPct
}

// NeedsHalt returns true if the margin ratio is above the halt threshold.
func (m *MarginMonitor) NeedsHalt() bool {
	return m.CurrentRatioPct() >= m.cfg.HaltRatioPct
}

func init() {
	// Verify MarginConfig defaults compile.
	_ = DefaultMarginConfig()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}
