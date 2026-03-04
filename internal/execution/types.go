// Package execution provides the live order executor for Bybit-only trading.
// Stripped: no TenantID, no Mode field.
package execution

// EventType classifies what happened during execution.
type EventType string

const (
	EventOrderSent     EventType = "ORDER_SENT"
	EventOrderFilled   EventType = "ORDER_FILLED"
	EventOrderPartial  EventType = "ORDER_PARTIAL"
	EventOrderCanceled EventType = "ORDER_CANCELED"
	EventOrderRejected EventType = "ORDER_REJECTED"
	EventOrderRetry    EventType = "ORDER_RETRY"
	EventHedgeDrift    EventType = "HEDGE_DRIFT"
	EventEmergencyUnw  EventType = "EMERGENCY_UNWIND"
	EventHedgeFailed   EventType = "HEDGE_FAILED"
	EventReconcile     EventType = "RECONCILE"
)

// ExecutionEvent records a single step in the execution of a trade leg.
type ExecutionEvent struct {
	EventType            EventType `json:"event_type"`
	IntentID             string    `json:"intent_id"`
	LegIndex             int       `json:"leg_index"`
	Symbol               string    `json:"symbol"`
	Action               string    `json:"action"`   // BUY | SELL
	Strategy             string    `json:"strategy"`
	Market               string    `json:"market"`    // SPOT | PERP
	RequestedNotionalUSD float64   `json:"requested_notional_usd"`
	FilledNotionalUSD    float64   `json:"filled_notional_usd"`
	FilledPrice          float64   `json:"filled_price"`
	SlippageBpsActual    float64   `json:"slippage_bps_actual"`
	FeesUSDActual        float64   `json:"fees_usd_actual"`
	TsMs                 int64     `json:"ts_ms"`
	LatencyMs            int64     `json:"latency_ms"`
}

// FillSummary aggregates a completed two-leg trade.
type FillSummary struct {
	IntentID    string  `json:"intent_id"`
	Strategy    string  `json:"strategy"`
	Symbol      string  `json:"symbol"`       // primary symbol
	HedgeSymbol string  `json:"hedge_symbol"` // hedge symbol (cross-pair)
	BuyPrice    float64 `json:"buy_price"`
	SellPrice   float64 `json:"sell_price"`
	TotalFees   float64 `json:"total_fees"`
	NetPnL      float64 `json:"net_pnl"`
	TsMs        int64   `json:"ts_ms"`
}
