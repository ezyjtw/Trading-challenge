package scanner

import (
	"fmt"
	"math"
)

// RangeVWAPConfig configures the range + VWAP mean reversion detector.
type RangeVWAPConfig struct {
	// RangeLookbackCandles defines how many candles to use for range detection.
	RangeLookbackCandles int `yaml:"range_lookback_candles"`

	// VWAPBandSD is the standard deviation multiplier for VWAP bands (typically 2.0).
	VWAPBandSD float64 `yaml:"vwap_band_sd"`

	// RangePositionThreshold: price must be within this % of range high/low to trigger.
	// 0.2 means bottom 20% for long, top 20% for short.
	RangePositionThreshold float64 `yaml:"range_position_threshold"`

	// VWAPDeviationThreshold: price must be this many SDs from VWAP to trigger.
	VWAPDeviationThreshold float64 `yaml:"vwap_deviation_threshold"`

	// RSIThresholdLow: RSI below this at range low = oversold confirmation.
	RSIThresholdLow float64 `yaml:"rsi_threshold_low"`

	// RSIThresholdHigh: RSI above this at range high = overbought confirmation.
	RSIThresholdHigh float64 `yaml:"rsi_threshold_high"`

	// RSIPeriod for the oscillator check.
	RSIPeriod int `yaml:"rsi_period"`

	// CandleTF is the primary timeframe for analysis.
	CandleTF Timeframe `yaml:"candle_tf"`

	// POCBins for volume profile calculation.
	POCBins int `yaml:"poc_bins"`

	// MinRangeATRMultiple: range height must be at least this many ATR to be valid.
	// Filters out too-tight ranges where mean reversion has no edge.
	MinRangeATRMultiple float64 `yaml:"min_range_atr_multiple"`
}

// DefaultRangeVWAPConfig returns sensible defaults.
func DefaultRangeVWAPConfig() RangeVWAPConfig {
	return RangeVWAPConfig{
		RangeLookbackCandles:   96, // 24h of 15m candles
		VWAPBandSD:             2.0,
		RangePositionThreshold: 0.20,
		VWAPDeviationThreshold: 1.5,
		RSIThresholdLow:        35,
		RSIThresholdHigh:       65,
		RSIPeriod:              14,
		CandleTF:               TF15m,
		POCBins:                50,
		MinRangeATRMultiple:    1.5,
	}
}

// RangeVWAPDetector detects mean reversion setups at range extremes with
// VWAP band and volume profile confirmation.
//
// Logic for LONG:
//   1. Price is near the bottom of a defined range (below 20% of range)
//   2. Price is below VWAP lower band (oversold relative to volume-weighted mean)
//   3. RSI is oversold (< 35) confirming momentum exhaustion
//   4. Optional: price near POC or above it (value area support)
//
// Logic for SHORT:
//   1. Price is near the top of a defined range (above 80% of range)
//   2. Price is above VWAP upper band (overbought relative to volume-weighted mean)
//   3. RSI is overbought (> 65) confirming momentum exhaustion
//   4. Optional: price near POC or below it (value area resistance)
type RangeVWAPDetector struct {
	cfg RangeVWAPConfig
}

func NewRangeVWAPDetector(cfg RangeVWAPConfig) *RangeVWAPDetector {
	return &RangeVWAPDetector{cfg: cfg}
}

func (d *RangeVWAPDetector) Name() ConfluenceFactor {
	return FactorRangeVWAPRevert
}

func (d *RangeVWAPDetector) Detect(state *MarketState) []Signal {
	candles, ok := state.Candles[d.cfg.CandleTF]
	if !ok || len(candles) < d.cfg.RangeLookbackCandles {
		return nil
	}

	lookbackCandles := candles[len(candles)-d.cfg.RangeLookbackCandles:]
	price := state.Price
	if price <= 0 {
		return nil
	}

	// 1. Detect range.
	rangeHigh := lookbackCandles[0].High
	rangeLow := lookbackCandles[0].Low
	for _, c := range lookbackCandles {
		if c.High > rangeHigh {
			rangeHigh = c.High
		}
		if c.Low < rangeLow {
			rangeLow = c.Low
		}
	}
	rangeSize := rangeHigh - rangeLow
	if rangeSize <= 0 {
		return nil
	}

	// Filter: range must be meaningful (at least minRangeATRMultiple * ATR).
	atrValues := ATR(lookbackCandles, 14)
	if len(atrValues) > 0 {
		currentATR := atrValues[len(atrValues)-1]
		if currentATR > 0 && rangeSize < currentATR*d.cfg.MinRangeATRMultiple {
			return nil // range too tight
		}
	}

	pricePosition := (price - rangeLow) / rangeSize // 0.0 = bottom, 1.0 = top

	// 2. Compute VWAP with bands.
	vwap := ComputeVWAP(lookbackCandles, d.cfg.VWAPBandSD)
	if vwap.VWAP == 0 || vwap.StdDev == 0 {
		return nil
	}

	vwapDeviation := (price - vwap.VWAP) / vwap.StdDev // negative = below VWAP

	// 3. Compute RSI.
	closes := make([]float64, len(lookbackCandles))
	for i, c := range lookbackCandles {
		closes[i] = c.Close
	}
	rsiValues := RSI(closes, d.cfg.RSIPeriod)
	if len(rsiValues) == 0 {
		return nil
	}
	currentRSI := rsiValues[len(rsiValues)-1]

	// 4. Compute POC for value area reference.
	poc := POC(lookbackCandles, d.cfg.POCBins)

	var signals []Signal

	// LONG setup: price at range bottom + below VWAP lower band + RSI oversold.
	if pricePosition <= d.cfg.RangePositionThreshold &&
		vwapDeviation <= -d.cfg.VWAPDeviationThreshold &&
		currentRSI <= d.cfg.RSIThresholdLow {

		strength := 0.5

		// Stronger if further from VWAP.
		if math.Abs(vwapDeviation) > d.cfg.VWAPDeviationThreshold*1.5 {
			strength += 0.15
		}

		// Stronger if RSI deeply oversold.
		if currentRSI < 25 {
			strength += 0.15
		}

		// POC above current price = value area support from above.
		if poc > price {
			strength += 0.1
		}

		// Check for bullish divergence.
		div := DetectRSIDivergence(lookbackCandles, rsiValues, 20)
		if div == BullishDivergence {
			strength += 0.1
		}

		detail := fmt.Sprintf("Range bottom (%.0f%%), VWAP dev %.1fσ, RSI %.1f, POC $%.0f",
			pricePosition*100, vwapDeviation, currentRSI, poc)

		signals = append(signals, Signal{
			Factor:    FactorRangeVWAPRevert,
			Direction: Long,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	// SHORT setup: price at range top + above VWAP upper band + RSI overbought.
	if pricePosition >= (1-d.cfg.RangePositionThreshold) &&
		vwapDeviation >= d.cfg.VWAPDeviationThreshold &&
		currentRSI >= d.cfg.RSIThresholdHigh {

		strength := 0.5

		if math.Abs(vwapDeviation) > d.cfg.VWAPDeviationThreshold*1.5 {
			strength += 0.15
		}

		if currentRSI > 75 {
			strength += 0.15
		}

		if poc < price {
			strength += 0.1
		}

		div := DetectRSIDivergence(lookbackCandles, rsiValues, 20)
		if div == BearishDivergence {
			strength += 0.1
		}

		detail := fmt.Sprintf("Range top (%.0f%%), VWAP dev +%.1fσ, RSI %.1f, POC $%.0f",
			pricePosition*100, vwapDeviation, currentRSI, poc)

		signals = append(signals, Signal{
			Factor:    FactorRangeVWAPRevert,
			Direction: Short,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	return signals
}
