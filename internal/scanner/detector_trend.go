package scanner

import (
	"fmt"
)

// TrendPullbackConfig configures the trend pullback to support stack detector.
type TrendPullbackConfig struct {
	// EMA periods for the moving average stack.
	FastEMA int `yaml:"fast_ema"`
	SlowEMA int `yaml:"slow_ema"`

	// PullbackToEMAThresholdPct: price must be within this % of EMA to be a pullback.
	PullbackToEMAThresholdPct float64 `yaml:"pullback_to_ema_threshold_pct"`

	// FibConfluenceTolerancePct: how close price must be to a fib level for confirmation.
	FibConfluenceTolerancePct float64 `yaml:"fib_confluence_tolerance_pct"`

	// RSIPeriod for momentum confirmation.
	RSIPeriod int `yaml:"rsi_period"`

	// SwingLookback for structure detection.
	SwingLookback int `yaml:"swing_lookback"`

	// CandleTF for primary analysis.
	CandleTF Timeframe `yaml:"candle_tf"`
}

// DefaultTrendPullbackConfig returns sensible defaults.
func DefaultTrendPullbackConfig() TrendPullbackConfig {
	return TrendPullbackConfig{
		FastEMA:                   21,
		SlowEMA:                   50,
		PullbackToEMAThresholdPct: 0.3,
		FibConfluenceTolerancePct: 0.5,
		RSIPeriod:                 14,
		SwingLookback:             3,
		CandleTF:                  TF15m,
	}
}

// TrendPullbackDetector detects pullbacks to support/resistance stacks
// within an established trend.
//
// Long setup (uptrend pullback):
//   1. Higher highs & higher lows confirmed (structure = uptrend)
//   2. Price pulls back to EMA zone (21/50 EMA area)
//   3. Pullback lands near a Fibonacci retracement level (0.5 or 0.618)
//   4. RSI not oversold (above 40 = trend intact, just pulling back)
//   5. Optional: VWAP nearby adds confluence
//
// Short setup (downtrend pullback):
//   1. Lower highs & lower lows confirmed
//   2. Price rallies back to EMA zone
//   3. Rally reaches Fibonacci level
//   4. RSI not overbought excessively
type TrendPullbackDetector struct {
	cfg TrendPullbackConfig
}

func NewTrendPullbackDetector(cfg TrendPullbackConfig) *TrendPullbackDetector {
	return &TrendPullbackDetector{cfg: cfg}
}

func (d *TrendPullbackDetector) Name() ConfluenceFactor {
	return FactorTrendPullback
}

func (d *TrendPullbackDetector) Detect(state *MarketState) []Signal {
	candles, ok := state.Candles[d.cfg.CandleTF]
	if !ok || len(candles) < d.cfg.SlowEMA+10 {
		return nil
	}

	price := state.Price
	if price <= 0 {
		return nil
	}

	// Compute EMAs.
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	fastEMA := EMA(closes, d.cfg.FastEMA)
	slowEMA := EMA(closes, d.cfg.SlowEMA)
	if len(fastEMA) == 0 || len(slowEMA) == 0 {
		return nil
	}

	currentFastEMA := fastEMA[len(fastEMA)-1]
	currentSlowEMA := slowEMA[len(slowEMA)-1]

	// Detect trend via structure.
	swings := FindSwingPoints(candles, d.cfg.SwingLookback)
	trend := DetectTrend(swings)

	// Also confirm with EMA ordering.
	emaUptrend := currentFastEMA > currentSlowEMA
	emaDowntrend := currentFastEMA < currentSlowEMA

	// Compute RSI.
	rsiValues := RSI(closes, d.cfg.RSIPeriod)
	if len(rsiValues) == 0 {
		return nil
	}
	currentRSI := rsiValues[len(rsiValues)-1]

	// Find recent swing high and low for Fibonacci levels.
	var recentSwingHigh, recentSwingLow float64
	for i := len(swings) - 1; i >= 0; i-- {
		if swings[i].IsHigh && recentSwingHigh == 0 {
			recentSwingHigh = swings[i].Price
		}
		if !swings[i].IsHigh && recentSwingLow == 0 {
			recentSwingLow = swings[i].Price
		}
		if recentSwingHigh > 0 && recentSwingLow > 0 {
			break
		}
	}

	// Compute VWAP for additional confluence.
	vwap := ComputeVWAP(candles[len(candles)-96:], 2.0) // last 24h of 15m
	if len(candles) < 96 {
		vwap = ComputeVWAP(candles, 2.0)
	}

	var signals []Signal

	// LONG: uptrend pullback to support stack.
	if (trend == 1 || emaUptrend) && trend != -1 {
		// Check if price is pulling back to EMA zone.
		distToFast := (price - currentFastEMA) / currentFastEMA * 100
		distToSlow := (price - currentSlowEMA) / currentSlowEMA * 100

		// Price should be near or between the EMAs (pulling back into them).
		nearEMA := distToFast <= d.cfg.PullbackToEMAThresholdPct &&
			distToFast >= -d.cfg.PullbackToEMAThresholdPct*2

		if !nearEMA && distToSlow <= d.cfg.PullbackToEMAThresholdPct*2 &&
			distToSlow >= -d.cfg.PullbackToEMAThresholdPct {
			nearEMA = true
		}

		if nearEMA {
			strength := 0.5
			confluences := 1 // EMA pullback

			// Check Fibonacci confluence.
			if recentSwingHigh > 0 && recentSwingLow > 0 && recentSwingHigh > recentSwingLow {
				fibs := FibRetracements(recentSwingHigh, recentSwingLow)
				for _, fib := range fibs {
					if fib.Ratio >= 0.382 && fib.Ratio <= 0.786 {
						dist := (price - fib.Price) / fib.Price * 100
						if dist >= -d.cfg.FibConfluenceTolerancePct &&
							dist <= d.cfg.FibConfluenceTolerancePct {
							strength += 0.15
							confluences++
							break
						}
					}
				}
			}

			// RSI in healthy pullback zone (40–55 for uptrend pullback).
			if currentRSI >= 40 && currentRSI <= 55 {
				strength += 0.1
				confluences++
			}

			// VWAP confluence.
			if vwap.VWAP > 0 {
				distToVWAP := (price - vwap.VWAP) / vwap.VWAP * 100
				if distToVWAP >= -d.cfg.PullbackToEMAThresholdPct &&
					distToVWAP <= d.cfg.PullbackToEMAThresholdPct {
					strength += 0.1
					confluences++
				}
			}

			// Structure confirmation: both EMA order and swing structure agree.
			if trend == 1 && emaUptrend {
				strength += 0.1
			}

			if confluences >= 2 {
				detail := fmt.Sprintf("Uptrend pullback: EMA21=%.0f EMA50=%.0f, RSI %.1f, %d confluences",
					currentFastEMA, currentSlowEMA, currentRSI, confluences)

				signals = append(signals, Signal{
					Factor:    FactorTrendPullback,
					Direction: Long,
					Strength:  clamp(strength, 0, 1),
					Detail:    detail,
					TsMs:      state.TsMs,
				})
			}
		}
	}

	// SHORT: downtrend rally to resistance stack.
	if (trend == -1 || emaDowntrend) && trend != 1 {
		distToFast := (price - currentFastEMA) / currentFastEMA * 100
		distToSlow := (price - currentSlowEMA) / currentSlowEMA * 100

		nearEMA := distToFast >= -d.cfg.PullbackToEMAThresholdPct &&
			distToFast <= d.cfg.PullbackToEMAThresholdPct*2

		if !nearEMA && distToSlow >= -d.cfg.PullbackToEMAThresholdPct*2 &&
			distToSlow <= d.cfg.PullbackToEMAThresholdPct {
			nearEMA = true
		}

		if nearEMA {
			strength := 0.5
			confluences := 1

			if recentSwingHigh > 0 && recentSwingLow > 0 && recentSwingHigh > recentSwingLow {
				fibs := FibRetracements(recentSwingHigh, recentSwingLow)
				for _, fib := range fibs {
					if fib.Ratio >= 0.382 && fib.Ratio <= 0.786 {
						dist := (price - fib.Price) / fib.Price * 100
						if dist >= -d.cfg.FibConfluenceTolerancePct &&
							dist <= d.cfg.FibConfluenceTolerancePct {
							strength += 0.15
							confluences++
							break
						}
					}
				}
			}

			if currentRSI >= 45 && currentRSI <= 60 {
				strength += 0.1
				confluences++
			}

			if vwap.VWAP > 0 {
				distToVWAP := (price - vwap.VWAP) / vwap.VWAP * 100
				if distToVWAP >= -d.cfg.PullbackToEMAThresholdPct &&
					distToVWAP <= d.cfg.PullbackToEMAThresholdPct {
					strength += 0.1
					confluences++
				}
			}

			if trend == -1 && emaDowntrend {
				strength += 0.1
			}

			if confluences >= 2 {
				detail := fmt.Sprintf("Downtrend rally: EMA21=%.0f EMA50=%.0f, RSI %.1f, %d confluences",
					currentFastEMA, currentSlowEMA, currentRSI, confluences)

				signals = append(signals, Signal{
					Factor:    FactorTrendPullback,
					Direction: Short,
					Strength:  clamp(strength, 0, 1),
					Detail:    detail,
					TsMs:      state.TsMs,
				})
			}
		}
	}

	return signals
}
