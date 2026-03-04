package scanner

import (
	"fmt"
	"math"
)

// OIAbsorptionConfig configures the open interest absorption detector.
type OIAbsorptionConfig struct {
	// OISpikeThresholdPct: OI increase of this % signals fresh positioning.
	OISpikeThresholdPct float64 `yaml:"oi_spike_threshold_pct"`

	// PriceAbsorptionPct: if price moves less than this % while OI spikes,
	// it indicates absorption (large orders being filled without moving price).
	PriceAbsorptionPct float64 `yaml:"price_absorption_pct"`

	// OIDropWithPricePct: OI drops this % while price moves = liquidation cascade.
	OIDropWithPricePct float64 `yaml:"oi_drop_with_price_pct"`

	// MinOIDataPoints is the minimum OI snapshots needed.
	MinOIDataPoints int `yaml:"min_oi_data_points"`

	// LookbackCandles for price absorption check.
	LookbackCandles int `yaml:"lookback_candles"`

	// CandleTF is the timeframe for price analysis.
	CandleTF Timeframe `yaml:"candle_tf"`
}

// DefaultOIAbsorptionConfig returns sensible defaults.
func DefaultOIAbsorptionConfig() OIAbsorptionConfig {
	return OIAbsorptionConfig{
		OISpikeThresholdPct: 5.0,
		PriceAbsorptionPct:  0.5,
		OIDropWithPricePct:  5.0,
		MinOIDataPoints:     4,
		LookbackCandles:     20,
		CandleTF:            TF15m,
	}
}

// OIAbsorptionDetector detects divergence between open interest changes and
// price movement, which indicates institutional absorption or liquidation.
//
// Setup 1: OI Spike + Price Flat = Absorption
//   Large positions being opened (OI rising) but price barely moves.
//   This means a large player is absorbing sell pressure (bullish) or
//   buy pressure (bearish). Direction determined by recent price action.
//
// Setup 2: OI Drop + Price Drop = Liquidation Cascade (Long Squeeze)
//   OI dropping rapidly while price falls = longs getting liquidated.
//   The flush often overshoots, creating a reversal opportunity.
//
// Setup 3: OI Drop + Price Rise = Short Squeeze
//   OI dropping while price rises = shorts getting liquidated.
//   May continue but often signals exhaustion near the top.
type OIAbsorptionDetector struct {
	cfg OIAbsorptionConfig
}

func NewOIAbsorptionDetector(cfg OIAbsorptionConfig) *OIAbsorptionDetector {
	return &OIAbsorptionDetector{cfg: cfg}
}

func (d *OIAbsorptionDetector) Name() ConfluenceFactor {
	return FactorOIAbsorption
}

func (d *OIAbsorptionDetector) Detect(state *MarketState) []Signal {
	if len(state.OIHistory) < d.cfg.MinOIDataPoints {
		return nil
	}

	candles, ok := state.Candles[d.cfg.CandleTF]
	if !ok || len(candles) < d.cfg.LookbackCandles {
		return nil
	}

	// Compute OI change over the available history.
	recentOI := state.OIHistory[0].OpenInterest
	oldestOI := state.OIHistory[len(state.OIHistory)-1].OpenInterest
	if oldestOI == 0 {
		return nil
	}
	oiChangePct := (recentOI - oldestOI) / oldestOI * 100

	// Compute price change over the same period.
	recentCandles := candles[len(candles)-d.cfg.LookbackCandles:]
	priceStart := recentCandles[0].Close
	priceEnd := recentCandles[len(recentCandles)-1].Close
	if priceStart == 0 {
		return nil
	}
	priceChangePct := (priceEnd - priceStart) / priceStart * 100

	var signals []Signal

	// Setup 1: OI Spike + Price Flat = Absorption.
	if oiChangePct >= d.cfg.OISpikeThresholdPct &&
		math.Abs(priceChangePct) <= d.cfg.PriceAbsorptionPct {

		// Determine direction: if price is near the low of the range
		// during absorption, buying is occurring (bullish).
		rangeHigh := recentCandles[0].High
		rangeLow := recentCandles[0].Low
		for _, c := range recentCandles {
			if c.High > rangeHigh {
				rangeHigh = c.High
			}
			if c.Low < rangeLow {
				rangeLow = c.Low
			}
		}

		priceRange := rangeHigh - rangeLow
		pricePosition := 0.5
		if priceRange > 0 {
			pricePosition = (priceEnd - rangeLow) / priceRange
		}

		dir := Long
		if pricePosition > 0.6 {
			dir = Short // absorption at the top = selling pressure being absorbed? Less clear
		}
		// Near the bottom of range with OI spiking = bullish absorption.
		if pricePosition < 0.4 {
			dir = Long
		}

		strength := 0.6
		if oiChangePct > d.cfg.OISpikeThresholdPct*2 {
			strength = 0.8
		}

		signals = append(signals, Signal{
			Factor:    FactorOIAbsorption,
			Direction: dir,
			Strength:  strength,
			Detail: fmt.Sprintf("OI +%.1f%% with price flat (%.2f%%), absorption at %.0f%% of range",
				oiChangePct, priceChangePct, pricePosition*100),
			TsMs: state.TsMs,
		})
	}

	// Setup 2: OI Drop + Price Drop = Long Liquidation Cascade → Reversal Long.
	if oiChangePct <= -d.cfg.OIDropWithPricePct && priceChangePct < -1.0 {
		strength := 0.5
		if oiChangePct <= -d.cfg.OIDropWithPricePct*2 {
			strength = 0.7 // massive deleveraging = stronger reversal signal
		}

		// Check if RSI is oversold to confirm exhaustion.
		closes := make([]float64, len(recentCandles))
		for i, c := range recentCandles {
			closes[i] = c.Close
		}
		rsi := RSI(closes, 14)
		if len(rsi) > 0 && rsi[len(rsi)-1] < 30 {
			strength += 0.2
		}

		signals = append(signals, Signal{
			Factor:    FactorOIAbsorption,
			Direction: Long,
			Strength:  clamp(strength, 0, 1),
			Detail: fmt.Sprintf("Long liquidation: OI %.1f%%, price %.1f%% — reversal setup",
				oiChangePct, priceChangePct),
			TsMs: state.TsMs,
		})
	}

	// Setup 3: OI Drop + Price Rise = Short Squeeze → Caution on longs.
	if oiChangePct <= -d.cfg.OIDropWithPricePct && priceChangePct > 1.0 {
		strength := 0.5
		if oiChangePct <= -d.cfg.OIDropWithPricePct*2 {
			strength = 0.7
		}

		// Check if RSI is overbought.
		closes := make([]float64, len(recentCandles))
		for i, c := range recentCandles {
			closes[i] = c.Close
		}
		rsi := RSI(closes, 14)
		if len(rsi) > 0 && rsi[len(rsi)-1] > 70 {
			strength += 0.2
		}

		signals = append(signals, Signal{
			Factor:    FactorOIAbsorption,
			Direction: Short,
			Strength:  clamp(strength, 0, 1),
			Detail: fmt.Sprintf("Short squeeze exhaustion: OI %.1f%%, price +%.1f%% — reversal setup",
				oiChangePct, priceChangePct),
			TsMs: state.TsMs,
		})
	}

	return signals
}
