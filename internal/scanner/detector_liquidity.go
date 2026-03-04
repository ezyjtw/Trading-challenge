package scanner

import (
	"fmt"
	"math"
)

// LiqSweepReclaimConfig configures the liquidity sweep + reclaim detector.
type LiqSweepReclaimConfig struct {
	// SwingLookback for detecting equal highs/lows (liquidity pools).
	SwingLookback int `yaml:"swing_lookback"`

	// EqualLevelTolerancePct: two swing points are "equal" if within this %
	// of each other (forming a liquidity pool).
	EqualLevelTolerancePct float64 `yaml:"equal_level_tolerance_pct"`

	// SweepBeyondPct: price must exceed the liquidity level by at least this %
	// to count as a sweep (not just a touch).
	SweepBeyondPct float64 `yaml:"sweep_beyond_pct"`

	// ReclaimCandles: price must close back inside within this many candles.
	ReclaimCandles int `yaml:"reclaim_candles"`

	// CandleTF for primary analysis.
	CandleTF Timeframe `yaml:"candle_tf"`

	// MinFVGSizePct: minimum fair value gap size as % of price to be significant.
	MinFVGSizePct float64 `yaml:"min_fvg_size_pct"`
}

// DefaultLiqSweepReclaimConfig returns sensible defaults.
func DefaultLiqSweepReclaimConfig() LiqSweepReclaimConfig {
	return LiqSweepReclaimConfig{
		SwingLookback:          3,
		EqualLevelTolerancePct: 0.15,
		SweepBeyondPct:         0.05,
		ReclaimCandles:         5,
		CandleTF:               TF15m,
		MinFVGSizePct:          0.1,
	}
}

// LiqSweepReclaimDetector detects liquidity sweep + reclaim patterns.
//
// Pattern (Bullish):
//   1. Find equal lows (liquidity pool below) — stop orders clustered here
//   2. Price sweeps below these lows (triggers stops, grabs liquidity)
//   3. Price immediately reclaims back above the level (failed breakdown)
//   4. Bonus: displacement candle (FVG) on the reclaim = institutional footprint
//
// Pattern (Bearish):
//   1. Find equal highs (liquidity pool above)
//   2. Price sweeps above (triggers stops)
//   3. Price reclaims back below
//   4. Bonus: displacement candle downward
type LiqSweepReclaimDetector struct {
	cfg LiqSweepReclaimConfig
}

func NewLiqSweepReclaimDetector(cfg LiqSweepReclaimConfig) *LiqSweepReclaimDetector {
	return &LiqSweepReclaimDetector{cfg: cfg}
}

func (d *LiqSweepReclaimDetector) Name() ConfluenceFactor {
	return FactorLiqSweepReclaim
}

func (d *LiqSweepReclaimDetector) Detect(state *MarketState) []Signal {
	candles, ok := state.Candles[d.cfg.CandleTF]
	if !ok || len(candles) < 30 {
		return nil
	}

	swings := FindSwingPoints(candles, d.cfg.SwingLookback)
	if len(swings) < 4 {
		return nil
	}

	var signals []Signal

	// Check for bullish sweep + reclaim (sweep of lows).
	if sig := d.detectBullishSweep(candles, swings, state); sig != nil {
		signals = append(signals, *sig)
	}

	// Check for bearish sweep + reclaim (sweep of highs).
	if sig := d.detectBearishSweep(candles, swings, state); sig != nil {
		signals = append(signals, *sig)
	}

	return signals
}

func (d *LiqSweepReclaimDetector) detectBullishSweep(candles []Candle, swings []SwingPoint, state *MarketState) *Signal {
	// Find equal lows (liquidity pool).
	var swingLows []SwingPoint
	for _, s := range swings {
		if !s.IsHigh {
			swingLows = append(swingLows, s)
		}
	}

	if len(swingLows) < 2 {
		return nil
	}

	// Look for clusters of equal lows.
	liquidityLevel := 0.0
	for i := 0; i < len(swingLows)-1; i++ {
		for j := i + 1; j < len(swingLows); j++ {
			diff := math.Abs(swingLows[i].Price-swingLows[j].Price) / swingLows[i].Price * 100
			if diff <= d.cfg.EqualLevelTolerancePct {
				// Found equal lows — the lower one is the liquidity level.
				liquidityLevel = math.Min(swingLows[i].Price, swingLows[j].Price)
			}
		}
	}

	if liquidityLevel == 0 {
		return nil
	}

	// Check if recent candles swept below and reclaimed.
	n := len(candles)
	sweepWindow := d.cfg.ReclaimCandles * 2
	if sweepWindow > n {
		sweepWindow = n
	}

	recent := candles[n-sweepWindow:]
	swept := false
	reclaimed := false
	sweepDepth := 0.0

	for i, c := range recent {
		// Did any candle wick below the liquidity level?
		sweepThreshold := liquidityLevel * (1 - d.cfg.SweepBeyondPct/100)
		if c.Low < sweepThreshold {
			swept = true
			depth := (liquidityLevel - c.Low) / liquidityLevel * 100
			if depth > sweepDepth {
				sweepDepth = depth
			}

			// Check if subsequent candles reclaimed above.
			for j := i + 1; j < len(recent) && j <= i+d.cfg.ReclaimCandles; j++ {
				if recent[j].Close > liquidityLevel {
					reclaimed = true
					break
				}
			}
		}
	}

	if !swept || !reclaimed {
		return nil
	}

	// Check for FVG (fair value gap) on the reclaim — displacement confirmation.
	hasFVG := false
	for i := n - d.cfg.ReclaimCandles; i < n-2 && i >= 0; i++ {
		// Bullish FVG: candle[i+2].low > candle[i].high (gap up).
		if candles[i+2].Low > candles[i].High {
			gapSize := (candles[i+2].Low - candles[i].High) / candles[i].Close * 100
			if gapSize >= d.cfg.MinFVGSizePct {
				hasFVG = true
				break
			}
		}
	}

	strength := 0.55
	if sweepDepth > 0.2 {
		strength += 0.15
	}
	if hasFVG {
		strength += 0.2
	}

	// Current price should be back above the liquidity level.
	if state.Price < liquidityLevel {
		return nil // hasn't reclaimed yet
	}

	detail := fmt.Sprintf("Sweep of equal lows $%.2f (depth %.2f%%), reclaimed", liquidityLevel, sweepDepth)
	if hasFVG {
		detail += ", FVG confirmed"
	}

	return &Signal{
		Factor:    FactorLiqSweepReclaim,
		Direction: Long,
		Strength:  clamp(strength, 0, 1),
		Detail:    detail,
		TsMs:      state.TsMs,
	}
}

func (d *LiqSweepReclaimDetector) detectBearishSweep(candles []Candle, swings []SwingPoint, state *MarketState) *Signal {
	// Find equal highs (liquidity pool).
	var swingHighs []SwingPoint
	for _, s := range swings {
		if s.IsHigh {
			swingHighs = append(swingHighs, s)
		}
	}

	if len(swingHighs) < 2 {
		return nil
	}

	liquidityLevel := 0.0
	for i := 0; i < len(swingHighs)-1; i++ {
		for j := i + 1; j < len(swingHighs); j++ {
			diff := math.Abs(swingHighs[i].Price-swingHighs[j].Price) / swingHighs[i].Price * 100
			if diff <= d.cfg.EqualLevelTolerancePct {
				liquidityLevel = math.Max(swingHighs[i].Price, swingHighs[j].Price)
			}
		}
	}

	if liquidityLevel == 0 {
		return nil
	}

	n := len(candles)
	sweepWindow := d.cfg.ReclaimCandles * 2
	if sweepWindow > n {
		sweepWindow = n
	}

	recent := candles[n-sweepWindow:]
	swept := false
	reclaimed := false
	sweepDepth := 0.0

	for i, c := range recent {
		sweepThreshold := liquidityLevel * (1 + d.cfg.SweepBeyondPct/100)
		if c.High > sweepThreshold {
			swept = true
			depth := (c.High - liquidityLevel) / liquidityLevel * 100
			if depth > sweepDepth {
				sweepDepth = depth
			}

			for j := i + 1; j < len(recent) && j <= i+d.cfg.ReclaimCandles; j++ {
				if recent[j].Close < liquidityLevel {
					reclaimed = true
					break
				}
			}
		}
	}

	if !swept || !reclaimed {
		return nil
	}

	// Check for bearish FVG.
	hasFVG := false
	for i := n - d.cfg.ReclaimCandles; i < n-2 && i >= 0; i++ {
		// Bearish FVG: candle[i+2].high < candle[i].low (gap down).
		if candles[i+2].High < candles[i].Low {
			gapSize := (candles[i].Low - candles[i+2].High) / candles[i].Close * 100
			if gapSize >= d.cfg.MinFVGSizePct {
				hasFVG = true
				break
			}
		}
	}

	strength := 0.55
	if sweepDepth > 0.2 {
		strength += 0.15
	}
	if hasFVG {
		strength += 0.2
	}

	if state.Price > liquidityLevel {
		return nil
	}

	detail := fmt.Sprintf("Sweep of equal highs $%.2f (depth %.2f%%), reclaimed", liquidityLevel, sweepDepth)
	if hasFVG {
		detail += ", FVG confirmed"
	}

	return &Signal{
		Factor:    FactorLiqSweepReclaim,
		Direction: Short,
		Strength:  clamp(strength, 0, 1),
		Detail:    detail,
		TsMs:      state.TsMs,
	}
}
