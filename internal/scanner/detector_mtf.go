package scanner

import (
	"fmt"
)

// MultiTFConfig configures the multi-timeframe alignment detector.
type MultiTFConfig struct {
	// HigherTF is the bias timeframe (e.g. 4h).
	HigherTF Timeframe `yaml:"higher_tf"`
	// LowerTF is the entry timeframe (e.g. 15m).
	LowerTF Timeframe `yaml:"lower_tf"`

	// SwingLookback for structure detection on each TF.
	SwingLookback int `yaml:"swing_lookback"`

	// EMA periods for trend confirmation.
	FastEMA int `yaml:"fast_ema"`
	SlowEMA int `yaml:"slow_ema"`

	// RSIPeriod for momentum check.
	RSIPeriod int `yaml:"rsi_period"`

	// MinHTFCandles is the minimum candles needed on the higher TF.
	MinHTFCandles int `yaml:"min_htf_candles"`

	// MinLTFCandles is the minimum candles needed on the lower TF.
	MinLTFCandles int `yaml:"min_ltf_candles"`
}

// DefaultMultiTFConfig returns sensible defaults.
func DefaultMultiTFConfig() MultiTFConfig {
	return MultiTFConfig{
		HigherTF:      TF4h,
		LowerTF:       TF15m,
		SwingLookback: 3,
		FastEMA:       21,
		SlowEMA:       50,
		RSIPeriod:     14,
		MinHTFCandles: 30,
		MinLTFCandles: 50,
	}
}

// MultiTFDetector detects multi-timeframe alignment.
//
// The setup:
//   1. Determine HTF bias (4H trend direction via structure + EMA)
//   2. Wait for LTF structure shift in the direction of HTF bias
//   3. Confirm with LTF momentum (RSI)
//
// This is the "sniper entry" approach: trade in the direction of the
// higher timeframe, using the lower timeframe for precision entry.
type MultiTFDetector struct {
	cfg MultiTFConfig
}

func NewMultiTFDetector(cfg MultiTFConfig) *MultiTFDetector {
	return &MultiTFDetector{cfg: cfg}
}

func (d *MultiTFDetector) Name() ConfluenceFactor {
	return FactorMultiTFAlignment
}

func (d *MultiTFDetector) Detect(state *MarketState) []Signal {
	htfCandles, htfOK := state.Candles[d.cfg.HigherTF]
	ltfCandles, ltfOK := state.Candles[d.cfg.LowerTF]

	if !htfOK || len(htfCandles) < d.cfg.MinHTFCandles {
		return nil
	}
	if !ltfOK || len(ltfCandles) < d.cfg.MinLTFCandles {
		return nil
	}

	// --- Higher TF Bias ---
	htfCloses := make([]float64, len(htfCandles))
	for i, c := range htfCandles {
		htfCloses[i] = c.Close
	}

	htfFastEMA := EMA(htfCloses, d.cfg.FastEMA)
	htfSlowEMA := EMA(htfCloses, d.cfg.SlowEMA)
	htfSwings := FindSwingPoints(htfCandles, d.cfg.SwingLookback)
	htfTrend := DetectTrend(htfSwings)

	htfEMABias := 0
	if len(htfFastEMA) > 0 && len(htfSlowEMA) > 0 {
		if htfFastEMA[len(htfFastEMA)-1] > htfSlowEMA[len(htfSlowEMA)-1] {
			htfEMABias = 1
		} else {
			htfEMABias = -1
		}
	}

	// HTF bias requires either structure or EMA to agree.
	htfBias := 0
	if htfTrend == 1 || htfEMABias == 1 {
		htfBias = 1
	}
	if htfTrend == -1 || htfEMABias == -1 {
		if htfBias == 1 {
			htfBias = 0 // conflicting — no clear bias
		} else {
			htfBias = -1
		}
	}

	if htfBias == 0 {
		return nil // no clear HTF bias
	}

	// --- Lower TF Structure Shift ---
	ltfCloses := make([]float64, len(ltfCandles))
	for i, c := range ltfCandles {
		ltfCloses[i] = c.Close
	}

	ltfSwings := FindSwingPoints(ltfCandles, d.cfg.SwingLookback)
	ltfTrend := DetectTrend(ltfSwings)

	ltfRSI := RSI(ltfCloses, d.cfg.RSIPeriod)
	currentRSI := 50.0
	if len(ltfRSI) > 0 {
		currentRSI = ltfRSI[len(ltfRSI)-1]
	}

	ltfFastEMA := EMA(ltfCloses, d.cfg.FastEMA)
	ltfSlowEMA := EMA(ltfCloses, d.cfg.SlowEMA)

	var signals []Signal

	// LONG: HTF bullish bias + LTF structure shift to bullish.
	if htfBias == 1 && ltfTrend >= 0 {
		strength := 0.4

		// HTF trend + EMA both agree.
		if htfTrend == 1 && htfEMABias == 1 {
			strength += 0.15
		}

		// LTF has made a definitive shift (not just ranging).
		if ltfTrend == 1 {
			strength += 0.15
		}

		// LTF EMA cross bullish.
		if len(ltfFastEMA) > 0 && len(ltfSlowEMA) > 0 {
			if ltfFastEMA[len(ltfFastEMA)-1] > ltfSlowEMA[len(ltfSlowEMA)-1] {
				strength += 0.1
			}
		}

		// RSI in healthy range for a new long (not already overbought).
		if currentRSI >= 40 && currentRSI <= 65 {
			strength += 0.1
		}

		// Recent LTF candle should be bullish.
		if len(ltfCandles) > 0 && ltfCandles[len(ltfCandles)-1].IsBullish() {
			strength += 0.05
		}

		detail := fmt.Sprintf("HTF(%s) bullish, LTF(%s) aligned (trend=%d), RSI %.1f",
			d.cfg.HigherTF, d.cfg.LowerTF, ltfTrend, currentRSI)

		signals = append(signals, Signal{
			Factor:    FactorMultiTFAlignment,
			Direction: Long,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	// SHORT: HTF bearish bias + LTF structure shift to bearish.
	if htfBias == -1 && ltfTrend <= 0 {
		strength := 0.4

		if htfTrend == -1 && htfEMABias == -1 {
			strength += 0.15
		}

		if ltfTrend == -1 {
			strength += 0.15
		}

		if len(ltfFastEMA) > 0 && len(ltfSlowEMA) > 0 {
			if ltfFastEMA[len(ltfFastEMA)-1] < ltfSlowEMA[len(ltfSlowEMA)-1] {
				strength += 0.1
			}
		}

		if currentRSI >= 35 && currentRSI <= 60 {
			strength += 0.1
		}

		if len(ltfCandles) > 0 && !ltfCandles[len(ltfCandles)-1].IsBullish() {
			strength += 0.05
		}

		detail := fmt.Sprintf("HTF(%s) bearish, LTF(%s) aligned (trend=%d), RSI %.1f",
			d.cfg.HigherTF, d.cfg.LowerTF, ltfTrend, currentRSI)

		signals = append(signals, Signal{
			Factor:    FactorMultiTFAlignment,
			Direction: Short,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	return signals
}
