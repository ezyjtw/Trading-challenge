package scanner

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"
)

// TradeRecord tracks a single trade from signal to outcome.
type TradeRecord struct {
	Alert     SignalAlert `json:"alert"`
	EntryTime time.Time   `json:"entry_time"`
	ExitTime  *time.Time  `json:"exit_time,omitempty"`

	// Actual execution details (filled in after trade is placed).
	ActualEntry    float64 `json:"actual_entry,omitempty"`
	ActualSL       float64 `json:"actual_sl,omitempty"`
	ActualTP       float64 `json:"actual_tp,omitempty"`
	ActualExitPrice float64 `json:"actual_exit_price,omitempty"`
	ActualPnLUSD   float64 `json:"actual_pnl_usd,omitempty"`
	ActualPnLPct   float64 `json:"actual_pnl_pct,omitempty"`
	Slippage       float64 `json:"slippage_bps,omitempty"`
	Fees           float64 `json:"fees_usd,omitempty"`

	// Whether this trade counted toward minimum trading days.
	CountedForTradingDay bool `json:"counted_for_trading_day"`
}

// OutcomeTracker monitors open trades and records their outcomes.
// It feeds results back for strategy performance analysis.
type OutcomeTracker struct {
	mu sync.Mutex

	// Open trades being monitored.
	openTrades map[string]*TradeRecord

	// Completed trades (rolling window for stats).
	history []TradeRecord

	// Stats are recomputed on each outcome.
	stats PerformanceStats

	// File path for persistence.
	historyFile string

	// Max history entries to keep in memory.
	maxHistory int
}

// PerformanceStats tracks aggregate performance metrics.
type PerformanceStats struct {
	TotalTrades     int     `json:"total_trades"`
	WinCount        int     `json:"win_count"`
	LossCount       int     `json:"loss_count"`
	WinRate         float64 `json:"win_rate"`
	AvgWinUSD       float64 `json:"avg_win_usd"`
	AvgLossUSD      float64 `json:"avg_loss_usd"`
	ProfitFactor    float64 `json:"profit_factor"` // gross profit / gross loss
	ExpectancyUSD   float64 `json:"expectancy_usd"` // avg $ per trade
	AvgRR           float64 `json:"avg_rr"`          // average R-multiple
	MaxConsecLosses int     `json:"max_consec_losses"`
	MaxDrawdownUSD  float64 `json:"max_drawdown_usd"`
	TotalPnLUSD     float64 `json:"total_pnl_usd"`
	SharpeEstimate  float64 `json:"sharpe_estimate"`

	// Per-factor stats.
	FactorStats map[ConfluenceFactor]*FactorPerformance `json:"factor_stats"`

	// Per-symbol stats.
	SymbolStats map[string]*SymbolPerformance `json:"symbol_stats"`

	// Trading day tracking.
	TradingDaysCompleted int `json:"trading_days_completed"`
	TradingDaysNeeded    int `json:"trading_days_needed"`
}

// FactorPerformance tracks how well each confluence factor performs.
type FactorPerformance struct {
	Factor    ConfluenceFactor `json:"factor"`
	Trades    int              `json:"trades"`
	Wins      int              `json:"wins"`
	WinRate   float64          `json:"win_rate"`
	AvgPnL    float64          `json:"avg_pnl"`
	TotalPnL  float64          `json:"total_pnl"`
}

// SymbolPerformance tracks per-symbol results.
type SymbolPerformance struct {
	Symbol   string  `json:"symbol"`
	Trades   int     `json:"trades"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"win_rate"`
	TotalPnL float64 `json:"total_pnl"`
}

// NewOutcomeTracker creates a tracker with optional file persistence.
func NewOutcomeTracker(historyFile string, maxHistory int) *OutcomeTracker {
	if maxHistory <= 0 {
		maxHistory = 500
	}

	t := &OutcomeTracker{
		openTrades:  make(map[string]*TradeRecord),
		historyFile: historyFile,
		maxHistory:  maxHistory,
		stats: PerformanceStats{
			FactorStats: make(map[ConfluenceFactor]*FactorPerformance),
			SymbolStats: make(map[string]*SymbolPerformance),
		},
	}

	// Load existing history if file exists.
	if historyFile != "" {
		t.loadHistory()
	}

	return t
}

// RegisterTrade records a new trade being taken based on a signal alert.
func (t *OutcomeTracker) RegisterTrade(alert SignalAlert, actualEntry, actualSL, actualTP float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	record := &TradeRecord{
		Alert:       alert,
		EntryTime:   time.Now(),
		ActualEntry: actualEntry,
		ActualSL:    actualSL,
		ActualTP:    actualTP,
	}

	t.openTrades[alert.ID] = record
	log.Printf("tracker: registered trade %s %s %s entry=%.2f sl=%.2f tp=%.2f",
		alert.ID, alert.Symbol, alert.Direction, actualEntry, actualSL, actualTP)
}

// RecordOutcome records the result of a trade (TP hit, SL hit, or manual close).
func (t *OutcomeTracker) RecordOutcome(alertID string, outcome TradeOutcome, exitPrice, fees float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, ok := t.openTrades[alertID]
	if !ok {
		return fmt.Errorf("trade %s not found in open trades", alertID)
	}

	now := time.Now()
	record.ExitTime = &now
	record.ActualExitPrice = exitPrice
	record.Fees = fees
	record.Alert.Outcome = outcome
	record.Alert.OutcomeAt = &now

	// Calculate P&L.
	if record.ActualEntry > 0 {
		var rawPnL float64
		if record.Alert.Direction == Long {
			rawPnL = (exitPrice - record.ActualEntry) / record.ActualEntry * record.Alert.Risk.PositionSizeUSD
		} else {
			rawPnL = (record.ActualEntry - exitPrice) / record.ActualEntry * record.Alert.Risk.PositionSizeUSD
		}
		record.ActualPnLUSD = rawPnL - fees
		record.ActualPnLPct = record.ActualPnLUSD / record.Alert.Risk.PositionSizeUSD * 100

		// Calculate slippage from expected entry.
		record.Slippage = math.Abs(record.ActualEntry-record.Alert.Risk.Entry) /
			record.Alert.Risk.Entry * 10000
	}

	record.Alert.OutcomePnL = record.ActualPnLUSD
	record.Alert.OutcomeDetail = fmt.Sprintf("%s at $%.2f, PnL $%.2f (%.2f%%)",
		outcome, exitPrice, record.ActualPnLUSD, record.ActualPnLPct)

	// Check if this trade counts toward trading day.
	// PnL must be ≥ 1% of trade value.
	pnlPctOfTrade := math.Abs(record.ActualPnLPct)
	record.CountedForTradingDay = pnlPctOfTrade >= 1.0

	// Move to history.
	t.history = append(t.history, *record)
	if len(t.history) > t.maxHistory {
		t.history = t.history[len(t.history)-t.maxHistory:]
	}
	delete(t.openTrades, alertID)

	// Recompute stats.
	t.recomputeStats()

	// Persist.
	if t.historyFile != "" {
		t.saveHistory()
	}

	log.Printf("tracker: trade %s outcome=%s pnl=$%.2f (%.2f%%) counted_for_day=%v",
		alertID, outcome, record.ActualPnLUSD, record.ActualPnLPct, record.CountedForTradingDay)

	return nil
}

// CheckPriceForOutcome evaluates current price against open trades' SL/TP levels.
// Returns a list of alerts that have triggered.
func (t *OutcomeTracker) CheckPriceForOutcome(symbol string, currentPrice float64) []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var triggered []string

	for id, record := range t.openTrades {
		if record.Alert.Symbol != symbol {
			continue
		}

		if record.Alert.Direction == Long {
			if currentPrice <= record.ActualSL {
				triggered = append(triggered, id)
				log.Printf("tracker: SL HIT for %s at $%.2f (SL=$%.2f)",
					id, currentPrice, record.ActualSL)
			} else if currentPrice >= record.ActualTP {
				triggered = append(triggered, id)
				log.Printf("tracker: TP HIT for %s at $%.2f (TP=$%.2f)",
					id, currentPrice, record.ActualTP)
			}
		} else {
			if currentPrice >= record.ActualSL {
				triggered = append(triggered, id)
				log.Printf("tracker: SL HIT for %s at $%.2f (SL=$%.2f)",
					id, currentPrice, record.ActualSL)
			} else if currentPrice <= record.ActualTP {
				triggered = append(triggered, id)
				log.Printf("tracker: TP HIT for %s at $%.2f (TP=$%.2f)",
					id, currentPrice, record.ActualTP)
			}
		}
	}

	return triggered
}

// DetermineOutcome figures out if SL or TP was hit based on exit price.
func (t *OutcomeTracker) DetermineOutcome(alertID string, exitPrice float64) TradeOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, ok := t.openTrades[alertID]
	if !ok {
		return OutcomeManual
	}

	if record.Alert.Direction == Long {
		if exitPrice <= record.ActualSL {
			return OutcomeSLHit
		}
		if exitPrice >= record.ActualTP {
			return OutcomeTPHit
		}
	} else {
		if exitPrice >= record.ActualSL {
			return OutcomeSLHit
		}
		if exitPrice <= record.ActualTP {
			return OutcomeTPHit
		}
	}

	return OutcomeManual
}

// Stats returns the current performance statistics.
func (t *OutcomeTracker) Stats() PerformanceStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// OpenTradeCount returns how many trades are currently open.
func (t *OutcomeTracker) OpenTradeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.openTrades)
}

// recomputeStats recalculates all performance metrics from history.
func (t *OutcomeTracker) recomputeStats() {
	s := PerformanceStats{
		FactorStats: make(map[ConfluenceFactor]*FactorPerformance),
		SymbolStats: make(map[string]*SymbolPerformance),
	}

	if len(t.history) == 0 {
		t.stats = s
		return
	}

	var grossProfit, grossLoss float64
	var winPnLs, lossPnLs []float64
	var allPnLs []float64
	var consecLosses, maxConsecLosses int
	var equity, peakEquity, maxDD float64

	tradingDays := make(map[string]bool)

	for _, rec := range t.history {
		s.TotalTrades++
		pnl := rec.ActualPnLUSD
		allPnLs = append(allPnLs, pnl)

		// Equity curve for drawdown.
		equity += pnl
		if equity > peakEquity {
			peakEquity = equity
		}
		dd := peakEquity - equity
		if dd > maxDD {
			maxDD = dd
		}

		if pnl > 0 {
			s.WinCount++
			grossProfit += pnl
			winPnLs = append(winPnLs, pnl)
			consecLosses = 0
		} else {
			s.LossCount++
			grossLoss += math.Abs(pnl)
			lossPnLs = append(lossPnLs, pnl)
			consecLosses++
			if consecLosses > maxConsecLosses {
				maxConsecLosses = consecLosses
			}
		}

		// Per-factor stats.
		for _, sig := range rec.Alert.Signals {
			fp, ok := s.FactorStats[sig.Factor]
			if !ok {
				fp = &FactorPerformance{Factor: sig.Factor}
				s.FactorStats[sig.Factor] = fp
			}
			fp.Trades++
			fp.TotalPnL += pnl
			if pnl > 0 {
				fp.Wins++
			}
		}

		// Per-symbol stats.
		sp, ok := s.SymbolStats[rec.Alert.Symbol]
		if !ok {
			sp = &SymbolPerformance{Symbol: rec.Alert.Symbol}
			s.SymbolStats[rec.Alert.Symbol] = sp
		}
		sp.Trades++
		sp.TotalPnL += pnl
		if pnl > 0 {
			sp.Wins++
		}

		// Trading day tracking.
		if rec.CountedForTradingDay && rec.ExitTime != nil {
			day := rec.ExitTime.UTC().Format("2006-01-02")
			tradingDays[day] = true
		}
	}

	s.TotalPnLUSD = equity
	s.MaxDrawdownUSD = maxDD
	s.MaxConsecLosses = maxConsecLosses
	s.TradingDaysCompleted = len(tradingDays)
	s.TradingDaysNeeded = 10 // HyroTrader requirement

	if s.TotalTrades > 0 {
		s.WinRate = float64(s.WinCount) / float64(s.TotalTrades) * 100
		s.ExpectancyUSD = equity / float64(s.TotalTrades)
	}

	if s.WinCount > 0 {
		s.AvgWinUSD = grossProfit / float64(s.WinCount)
	}
	if s.LossCount > 0 {
		s.AvgLossUSD = grossLoss / float64(s.LossCount)
	}

	if grossLoss > 0 {
		s.ProfitFactor = grossProfit / grossLoss
	}

	// Average R-multiple.
	if len(allPnLs) > 0 && s.AvgLossUSD > 0 {
		totalR := 0.0
		for _, pnl := range allPnLs {
			totalR += pnl / s.AvgLossUSD
		}
		s.AvgRR = totalR / float64(len(allPnLs))
	}

	// Simplified Sharpe estimate.
	if len(allPnLs) > 1 {
		mean := equity / float64(len(allPnLs))
		var sumSqDev float64
		for _, pnl := range allPnLs {
			dev := pnl - mean
			sumSqDev += dev * dev
		}
		stddev := math.Sqrt(sumSqDev / float64(len(allPnLs)-1))
		if stddev > 0 {
			s.SharpeEstimate = mean / stddev * math.Sqrt(252) // annualized
		}
	}

	// Finalize per-factor stats.
	for _, fp := range s.FactorStats {
		if fp.Trades > 0 {
			fp.WinRate = float64(fp.Wins) / float64(fp.Trades) * 100
			fp.AvgPnL = fp.TotalPnL / float64(fp.Trades)
		}
	}
	for _, sp := range s.SymbolStats {
		if sp.Trades > 0 {
			sp.WinRate = float64(sp.Wins) / float64(sp.Trades) * 100
		}
	}

	t.stats = s
}

// loadHistory reads persisted history from disk.
func (t *OutcomeTracker) loadHistory() {
	data, err := os.ReadFile(t.historyFile)
	if err != nil {
		return // file doesn't exist yet
	}
	if err := json.Unmarshal(data, &t.history); err != nil {
		log.Printf("tracker: failed to parse history file: %v", err)
		return
	}
	t.recomputeStats()
	log.Printf("tracker: loaded %d historical trades", len(t.history))
}

// saveHistory writes current history to disk.
func (t *OutcomeTracker) saveHistory() {
	data, err := json.MarshalIndent(t.history, "", "  ")
	if err != nil {
		log.Printf("tracker: failed to marshal history: %v", err)
		return
	}
	if err := os.WriteFile(t.historyFile, data, 0644); err != nil {
		log.Printf("tracker: failed to write history: %v", err)
	}
}
