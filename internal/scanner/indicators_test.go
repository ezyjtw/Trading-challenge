package scanner

import (
	"math"
	"testing"
)

func TestEMA(t *testing.T) {
	values := []float64{10, 11, 12, 11, 10, 11, 12, 13, 14, 15}
	result := EMA(values, 3)

	if len(result) != len(values) {
		t.Fatalf("expected %d values, got %d", len(values), len(result))
	}

	// EMA should respond to trend.
	if result[len(result)-1] <= result[len(result)-3] {
		t.Error("EMA should be trending up with rising prices")
	}
}

func TestSMA(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	result := SMA(values, 3)

	// SMA of last 3 values (3,4,5) should be 4.
	expected := 4.0
	if math.Abs(result[4]-expected) > 0.001 {
		t.Errorf("SMA expected %.1f, got %.1f", expected, result[4])
	}
}

func TestRSI(t *testing.T) {
	// Generate a sequence that goes up then down.
	var closes []float64
	for i := 0; i < 20; i++ {
		closes = append(closes, float64(100+i))
	}
	// Then decline.
	for i := 0; i < 20; i++ {
		closes = append(closes, float64(120-i))
	}

	rsi := RSI(closes, 14)
	if rsi == nil {
		t.Fatal("RSI returned nil")
	}

	// RSI should be high during the uptrend.
	if rsi[19] < 60 {
		t.Errorf("RSI during uptrend should be > 60, got %.1f", rsi[19])
	}

	// RSI should be lower during the downtrend.
	if rsi[len(rsi)-1] > 50 {
		t.Errorf("RSI during downtrend should be < 50, got %.1f", rsi[len(rsi)-1])
	}
}

func TestATR(t *testing.T) {
	candles := make([]Candle, 20)
	for i := range candles {
		candles[i] = Candle{
			Open:  100,
			High:  102,
			Low:   98,
			Close: 101,
		}
	}

	atr := ATR(candles, 14)
	if atr == nil {
		t.Fatal("ATR returned nil")
	}

	// With consistent $4 range candles, ATR should be around 4.
	lastATR := atr[len(atr)-1]
	if lastATR < 3 || lastATR > 5 {
		t.Errorf("ATR expected ~4, got %.2f", lastATR)
	}
}

func TestVWAP(t *testing.T) {
	candles := []Candle{
		{High: 102, Low: 98, Close: 100, Volume: 1000},
		{High: 103, Low: 99, Close: 101, Volume: 1500},
		{High: 104, Low: 100, Close: 102, Volume: 2000},
	}

	result := ComputeVWAP(candles, 2.0)

	if result.VWAP == 0 {
		t.Fatal("VWAP should not be zero")
	}

	// VWAP should be between the low and high of the range.
	if result.VWAP < 98 || result.VWAP > 104 {
		t.Errorf("VWAP %.2f out of expected range", result.VWAP)
	}

	// Upper band should be above VWAP.
	if result.UpperBand <= result.VWAP {
		t.Error("upper band should be above VWAP")
	}

	// Lower band should be below VWAP.
	if result.LowerBand >= result.VWAP {
		t.Error("lower band should be below VWAP")
	}
}

func TestFibRetracements(t *testing.T) {
	levels := FibRetracements(100, 50)

	// Should have 7 standard levels.
	if len(levels) != 7 {
		t.Fatalf("expected 7 levels, got %d", len(levels))
	}

	// 0.0 level should be the high.
	if math.Abs(levels[0].Price-100) > 0.001 {
		t.Errorf("0.0 level should be 100, got %.2f", levels[0].Price)
	}

	// 1.0 level should be the low.
	if math.Abs(levels[6].Price-50) > 0.001 {
		t.Errorf("1.0 level should be 50, got %.2f", levels[6].Price)
	}

	// 0.618 level.
	expected618 := 100 - 50*0.618
	for _, l := range levels {
		if math.Abs(l.Ratio-0.618) < 0.001 {
			if math.Abs(l.Price-expected618) > 0.1 {
				t.Errorf("0.618 level expected %.2f, got %.2f", expected618, l.Price)
			}
		}
	}
}

func TestPOC(t *testing.T) {
	// Create candles with most volume at $100.
	candles := []Candle{
		{High: 102, Low: 98, Close: 100, Volume: 5000},
		{High: 101, Low: 99, Close: 100, Volume: 8000},
		{High: 110, Low: 105, Close: 108, Volume: 1000},
		{High: 101, Low: 99, Close: 100, Volume: 6000},
	}

	poc := POC(candles, 20)

	// POC should be near $100 where most volume is.
	if poc < 98 || poc > 102 {
		t.Errorf("POC expected near 100, got %.2f", poc)
	}
}

func TestDetectTrend(t *testing.T) {
	// Uptrend: higher highs, higher lows.
	swings := []SwingPoint{
		{Price: 100, IsHigh: false},
		{Price: 110, IsHigh: true},
		{Price: 105, IsHigh: false},
		{Price: 115, IsHigh: true},
	}

	trend := DetectTrend(swings)
	if trend != 1 {
		t.Errorf("expected uptrend (1), got %d", trend)
	}

	// Downtrend: lower highs, lower lows.
	swings = []SwingPoint{
		{Price: 100, IsHigh: false},
		{Price: 115, IsHigh: true},
		{Price: 95, IsHigh: false},
		{Price: 110, IsHigh: true},
	}

	trend = DetectTrend(swings)
	if trend != -1 {
		t.Errorf("expected downtrend (-1), got %d", trend)
	}
}
