package scanner

import (
	"fmt"
	"math"
)

// FundingExtremeConfig configures the funding rate extreme detector.
type FundingExtremeConfig struct {
	// ExtremeThreshold is the absolute funding rate above which we consider
	// the market extremely positioned. Typical range: 0.01–0.05 (1–5 bps).
	// Bybit funding is every 8h; 0.01 = 0.01% per 8h = ~13.7% annualized.
	ExtremeThreshold float64 `yaml:"extreme_threshold"`

	// MomentumStallRSILow is the RSI level below which we confirm momentum
	// exhaustion for shorts (funding very positive = market overleveraged long,
	// RSI dropping = momentum dying).
	MomentumStallRSILow float64 `yaml:"momentum_stall_rsi_low"`

	// MomentumStallRSIHigh is the RSI level above which we confirm momentum
	// exhaustion for longs (funding very negative = market overleveraged short,
	// RSI rising = shorts getting squeezed).
	MomentumStallRSIHigh float64 `yaml:"momentum_stall_rsi_high"`

	// MinHistoryPoints is the minimum number of funding snapshots needed.
	MinHistoryPoints int `yaml:"min_history_points"`

	// OIDropThresholdPct: if OI drops by this % while funding is extreme,
	// it confirms liquidation/deleveraging is underway.
	OIDropThresholdPct float64 `yaml:"oi_drop_threshold_pct"`

	// RSIPeriod for the momentum check.
	RSIPeriod int `yaml:"rsi_period"`

	// CandleTimeframe for RSI calculation.
	CandleTF Timeframe `yaml:"candle_tf"`
}

// DefaultFundingExtremeConfig returns sensible defaults.
func DefaultFundingExtremeConfig() FundingExtremeConfig {
	return FundingExtremeConfig{
		ExtremeThreshold:     0.0005, // 0.05% per 8h = ~22% annualized
		MomentumStallRSILow:  40,
		MomentumStallRSIHigh: 60,
		MinHistoryPoints:     3,
		OIDropThresholdPct:   3.0,
		RSIPeriod:            14,
		CandleTF:             TF15m,
	}
}

// FundingExtremeDetector detects extreme funding rate conditions combined with
// momentum stall and OI deleveraging.
//
// Logic:
// - Funding rate extremely positive → market overleveraged long → look for SHORT
//   - Confirmed by: RSI dropping from high (momentum stall), OI declining
// - Funding rate extremely negative → market overleveraged short → look for LONG
//   - Confirmed by: RSI rising from low (shorts getting squeezed), OI declining
type FundingExtremeDetector struct {
	cfg FundingExtremeConfig
}

func NewFundingExtremeDetector(cfg FundingExtremeConfig) *FundingExtremeDetector {
	return &FundingExtremeDetector{cfg: cfg}
}

func (d *FundingExtremeDetector) Name() ConfluenceFactor {
	return FactorFundingExtreme
}

func (d *FundingExtremeDetector) Detect(state *MarketState) []Signal {
	if len(state.FundingHistory) < d.cfg.MinHistoryPoints {
		return nil
	}

	candles, ok := state.Candles[d.cfg.CandleTF]
	if !ok || len(candles) < d.cfg.RSIPeriod+5 {
		return nil
	}

	currentFunding := state.FundingRate
	absFunding := math.Abs(currentFunding)

	if absFunding < d.cfg.ExtremeThreshold {
		return nil // funding not extreme
	}

	// Check if funding has been consistently extreme (at least half of recent history).
	extremeCount := 0
	for _, f := range state.FundingHistory {
		if math.Abs(f.FundingRate) >= d.cfg.ExtremeThreshold {
			extremeCount++
		}
	}
	if extremeCount < d.cfg.MinHistoryPoints/2+1 {
		return nil // not sustained
	}

	// Compute RSI for momentum check.
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	rsiValues := RSI(closes, d.cfg.RSIPeriod)
	if len(rsiValues) == 0 {
		return nil
	}
	currentRSI := rsiValues[len(rsiValues)-1]

	// Check OI deleveraging.
	oiDeclining := false
	if len(state.OIHistory) >= 2 {
		recent := state.OIHistory[0].OpenInterest
		older := state.OIHistory[len(state.OIHistory)-1].OpenInterest
		if older > 0 {
			oiChange := (recent - older) / older * 100
			oiDeclining = oiChange <= -d.cfg.OIDropThresholdPct
		}
	}

	var signals []Signal

	if currentFunding > d.cfg.ExtremeThreshold {
		// Market overleveraged LONG → look for SHORT.
		strength := 0.5

		// Momentum stall: RSI dropping / not making new highs.
		if currentRSI < d.cfg.MomentumStallRSIHigh {
			strength += 0.2
		}
		if currentRSI < 50 {
			strength += 0.1
		}

		// OI dropping = longs getting liquidated.
		if oiDeclining {
			strength += 0.2
		}

		detail := fmt.Sprintf("Funding +%.4f%% (extreme), RSI %.1f", currentFunding*100, currentRSI)
		if oiDeclining {
			detail += ", OI declining"
		}

		signals = append(signals, Signal{
			Factor:    FactorFundingExtreme,
			Direction: Short,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	if currentFunding < -d.cfg.ExtremeThreshold {
		// Market overleveraged SHORT → look for LONG.
		strength := 0.5

		if currentRSI > d.cfg.MomentumStallRSILow {
			strength += 0.2
		}
		if currentRSI > 50 {
			strength += 0.1
		}

		if oiDeclining {
			strength += 0.2
		}

		detail := fmt.Sprintf("Funding %.4f%% (extreme negative), RSI %.1f", currentFunding*100, currentRSI)
		if oiDeclining {
			detail += ", OI declining"
		}

		signals = append(signals, Signal{
			Factor:    FactorFundingExtreme,
			Direction: Long,
			Strength:  clamp(strength, 0, 1),
			Detail:    detail,
			TsMs:      state.TsMs,
		})
	}

	return signals
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
