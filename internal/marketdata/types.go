// Package marketdata provides market data types and feed adapters for the
// trading challenge. All price fields are float64 because market data is
// observational, not monetary accounting.
package marketdata

// Quote represents a single market data snapshot for a symbol.
type Quote struct {
	Symbol      string  `json:"symbol"`
	BestBid     float64 `json:"best_bid"`
	BestAsk     float64 `json:"best_ask"`
	MarkPrice   float64 `json:"mark_price"`
	IndexPrice  float64 `json:"index_price"`
	FundingRate float64 `json:"funding_rate"`
	LastPrice   float64 `json:"last_price"`
	Volume24h   float64 `json:"volume_24h"`
	TsMs        int64   `json:"ts_ms"`
	FeedHealthy bool    `json:"feed_healthy"`
}
