package scanner

import "math"

// --- EMA ---

// EMA computes the exponential moving average for a slice of values.
// Returns a slice the same length as input; the first value seeds the EMA.
func EMA(values []float64, period int) []float64 {
	if len(values) == 0 || period <= 0 {
		return nil
	}
	result := make([]float64, len(values))
	k := 2.0 / float64(period+1)
	result[0] = values[0]
	for i := 1; i < len(values); i++ {
		result[i] = values[i]*k + result[i-1]*(1-k)
	}
	return result
}

// SMA computes the simple moving average. Returns a slice the same length
// as input; values before the period is complete use the available window.
func SMA(values []float64, period int) []float64 {
	if len(values) == 0 || period <= 0 {
		return nil
	}
	result := make([]float64, len(values))
	sum := 0.0
	for i := 0; i < len(values); i++ {
		sum += values[i]
		if i >= period {
			sum -= values[i-period]
			result[i] = sum / float64(period)
		} else {
			result[i] = sum / float64(i+1)
		}
	}
	return result
}

// --- RSI ---

// RSI computes the Relative Strength Index using Wilder's smoothing.
// Returns a slice the same length as input; the first `period` values
// are computed with simple averaging.
func RSI(closes []float64, period int) []float64 {
	if len(closes) < 2 || period <= 0 {
		return nil
	}

	result := make([]float64, len(closes))
	gains := make([]float64, len(closes))
	losses := make([]float64, len(closes))

	for i := 1; i < len(closes); i++ {
		change := closes[i] - closes[i-1]
		if change > 0 {
			gains[i] = change
		} else {
			losses[i] = -change
		}
	}

	// Initial average gain/loss (simple average over first period).
	var avgGain, avgLoss float64
	for i := 1; i <= period && i < len(closes); i++ {
		avgGain += gains[i]
		avgLoss += losses[i]
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	for i := 0; i < len(closes); i++ {
		if i < period {
			result[i] = 50 // insufficient data
			continue
		}
		if i == period {
			if avgLoss == 0 {
				result[i] = 100
			} else {
				rs := avgGain / avgLoss
				result[i] = 100 - 100/(1+rs)
			}
			continue
		}
		// Wilder's smoothing.
		avgGain = (avgGain*float64(period-1) + gains[i]) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + losses[i]) / float64(period)
		if avgLoss == 0 {
			result[i] = 100
		} else {
			rs := avgGain / avgLoss
			result[i] = 100 - 100/(1+rs)
		}
	}
	return result
}

// --- VWAP ---

// VWAP computes Volume-Weighted Average Price from candles.
// Returns VWAP value and upper/lower bands at the given standard deviation multiplier.
type VWAPResult struct {
	VWAP      float64
	UpperBand float64 // VWAP + sdMult * stddev
	LowerBand float64 // VWAP - sdMult * stddev
	StdDev    float64
}

// ComputeVWAP calculates VWAP and bands from a slice of candles (intraday).
// sdMult is typically 2.0 for standard bands.
func ComputeVWAP(candles []Candle, sdMult float64) VWAPResult {
	if len(candles) == 0 {
		return VWAPResult{}
	}

	var cumTPV, cumVol float64
	tpValues := make([]float64, 0, len(candles))

	for _, c := range candles {
		tp := (c.High + c.Low + c.Close) / 3
		tpValues = append(tpValues, tp)
		cumTPV += tp * c.Volume
		cumVol += c.Volume
	}

	if cumVol == 0 {
		return VWAPResult{}
	}

	vwap := cumTPV / cumVol

	// Standard deviation of typical prices from VWAP weighted by volume.
	var sumSqDev float64
	for i, tp := range tpValues {
		dev := tp - vwap
		sumSqDev += dev * dev * candles[i].Volume
	}
	variance := sumSqDev / cumVol
	stddev := math.Sqrt(variance)

	return VWAPResult{
		VWAP:      vwap,
		UpperBand: vwap + sdMult*stddev,
		LowerBand: vwap - sdMult*stddev,
		StdDev:    stddev,
	}
}

// --- ATR ---

// ATR computes Average True Range over the given period.
func ATR(candles []Candle, period int) []float64 {
	if len(candles) < 2 || period <= 0 {
		return nil
	}

	tr := make([]float64, len(candles))
	tr[0] = candles[0].High - candles[0].Low

	for i := 1; i < len(candles); i++ {
		hl := candles[i].High - candles[i].Low
		hc := math.Abs(candles[i].High - candles[i-1].Close)
		lc := math.Abs(candles[i].Low - candles[i-1].Close)
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}

	// Wilder's smoothing for ATR.
	atr := make([]float64, len(candles))
	sum := 0.0
	for i := 0; i < period && i < len(tr); i++ {
		sum += tr[i]
	}
	atr[period-1] = sum / float64(period)

	for i := period; i < len(tr); i++ {
		atr[i] = (atr[i-1]*float64(period-1) + tr[i]) / float64(period)
	}

	return atr
}

// --- Fibonacci Retracement ---

// FibLevel represents a Fibonacci retracement level.
type FibLevel struct {
	Ratio float64
	Price float64
}

// FibRetracements computes Fibonacci retracement levels between a swing high and low.
func FibRetracements(swingHigh, swingLow float64) []FibLevel {
	diff := swingHigh - swingLow
	ratios := []float64{0.0, 0.236, 0.382, 0.5, 0.618, 0.786, 1.0}
	levels := make([]FibLevel, len(ratios))
	for i, r := range ratios {
		levels[i] = FibLevel{
			Ratio: r,
			Price: swingHigh - diff*r,
		}
	}
	return levels
}

// --- Structure Detection ---

// SwingPoint represents a detected swing high or low.
type SwingPoint struct {
	Index int
	Price float64
	IsHigh bool
	TsMs  int64
}

// FindSwingPoints detects swing highs and lows using a lookback window.
// A swing high has `lookback` lower highs on each side.
// A swing low has `lookback` higher lows on each side.
func FindSwingPoints(candles []Candle, lookback int) []SwingPoint {
	if len(candles) < lookback*2+1 {
		return nil
	}

	var points []SwingPoint

	for i := lookback; i < len(candles)-lookback; i++ {
		isHigh := true
		isLow := true

		for j := 1; j <= lookback; j++ {
			if candles[i].High <= candles[i-j].High || candles[i].High <= candles[i+j].High {
				isHigh = false
			}
			if candles[i].Low >= candles[i-j].Low || candles[i].Low >= candles[i+j].Low {
				isLow = false
			}
		}

		if isHigh {
			points = append(points, SwingPoint{
				Index:  i,
				Price:  candles[i].High,
				IsHigh: true,
				TsMs:   candles[i].OpenTime,
			})
		}
		if isLow {
			points = append(points, SwingPoint{
				Index:  i,
				Price:  candles[i].Low,
				IsHigh: false,
				TsMs:   candles[i].OpenTime,
			})
		}
	}

	return points
}

// DetectTrend determines if price is in an uptrend, downtrend, or ranging.
// Uses higher-highs/higher-lows for uptrend, lower-highs/lower-lows for downtrend.
// Returns +1 for uptrend, -1 for downtrend, 0 for ranging/unclear.
func DetectTrend(swings []SwingPoint) int {
	if len(swings) < 4 {
		return 0
	}

	// Look at the last 4 swing points.
	recent := swings[len(swings)-4:]

	var highs, lows []SwingPoint
	for _, s := range recent {
		if s.IsHigh {
			highs = append(highs, s)
		} else {
			lows = append(lows, s)
		}
	}

	if len(highs) >= 2 && len(lows) >= 2 {
		hh := highs[len(highs)-1].Price > highs[len(highs)-2].Price
		hl := lows[len(lows)-1].Price > lows[len(lows)-2].Price
		lh := highs[len(highs)-1].Price < highs[len(highs)-2].Price
		ll := lows[len(lows)-1].Price < lows[len(lows)-2].Price

		if hh && hl {
			return 1 // uptrend
		}
		if lh && ll {
			return -1 // downtrend
		}
	}

	return 0
}

// --- Volume Profile (simplified POC) ---

// POC computes the Point of Control — the price level with the most volume.
// Uses a histogram approach with the given number of bins.
func POC(candles []Candle, bins int) float64 {
	if len(candles) == 0 || bins <= 0 {
		return 0
	}

	// Find price range.
	minPrice := candles[0].Low
	maxPrice := candles[0].High
	for _, c := range candles {
		if c.Low < minPrice {
			minPrice = c.Low
		}
		if c.High > maxPrice {
			maxPrice = c.High
		}
	}

	if maxPrice == minPrice {
		return maxPrice
	}

	binSize := (maxPrice - minPrice) / float64(bins)
	histogram := make([]float64, bins)

	for _, c := range candles {
		// Distribute volume across bins that the candle spans.
		lowBin := int((c.Low - minPrice) / binSize)
		highBin := int((c.High - minPrice) / binSize)
		if lowBin < 0 {
			lowBin = 0
		}
		if highBin >= bins {
			highBin = bins - 1
		}
		binsSpanned := highBin - lowBin + 1
		volPerBin := c.Volume / float64(binsSpanned)
		for b := lowBin; b <= highBin; b++ {
			histogram[b] += volPerBin
		}
	}

	// Find the bin with maximum volume.
	maxVol := histogram[0]
	maxBin := 0
	for i, v := range histogram {
		if v > maxVol {
			maxVol = v
			maxBin = i
		}
	}

	return minPrice + (float64(maxBin)+0.5)*binSize
}

// --- Divergence Detection ---

// DivergenceType represents the type of RSI-price divergence.
type DivergenceType int

const (
	NoDivergence      DivergenceType = iota
	BullishDivergence                // price makes lower low, RSI makes higher low
	BearishDivergence                // price makes higher high, RSI makes lower high
)

// DetectRSIDivergence checks for bullish or bearish divergence between
// price and RSI over the last `lookback` candles.
func DetectRSIDivergence(candles []Candle, rsiValues []float64, lookback int) DivergenceType {
	if len(candles) < lookback || len(rsiValues) < lookback {
		return NoDivergence
	}

	n := len(candles)

	// Find recent and prior swing lows/highs within lookback window.
	recentLow := candles[n-1].Low
	recentLowRSI := rsiValues[n-1]
	recentHigh := candles[n-1].High
	recentHighRSI := rsiValues[n-1]

	priorLow := math.MaxFloat64
	priorLowRSI := 100.0
	priorHigh := 0.0
	priorHighRSI := 0.0

	// Look at the window before the most recent candles.
	start := n - lookback
	mid := n - lookback/3 // split window into prior and recent

	for i := start; i < mid; i++ {
		if candles[i].Low < priorLow {
			priorLow = candles[i].Low
			priorLowRSI = rsiValues[i]
		}
		if candles[i].High > priorHigh {
			priorHigh = candles[i].High
			priorHighRSI = rsiValues[i]
		}
	}

	for i := mid; i < n; i++ {
		if candles[i].Low < recentLow {
			recentLow = candles[i].Low
			recentLowRSI = rsiValues[i]
		}
		if candles[i].High > recentHigh {
			recentHigh = candles[i].High
			recentHighRSI = rsiValues[i]
		}
	}

	// Bullish divergence: price makes lower low but RSI makes higher low.
	if recentLow < priorLow && recentLowRSI > priorLowRSI && recentLowRSI < 40 {
		return BullishDivergence
	}

	// Bearish divergence: price makes higher high but RSI makes lower high.
	if recentHigh > priorHigh && recentHighRSI < priorHighRSI && recentHighRSI > 60 {
		return BearishDivergence
	}

	return NoDivergence
}
