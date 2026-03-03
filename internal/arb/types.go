// Package arb defines the universal trade intent types used across all services.
// Stripped: no TenantID, no consensus type aliases.
package arb

// TradeLeg describes one side of a two-leg delta-neutral trade.
type TradeLeg struct {
	Action         string  `json:"action"`           // BUY | SELL
	Type           string  `json:"type"`             // MARKET | LIMIT | IOC
	Market         string  `json:"market"`           // PERP | SPOT
	Symbol         string  `json:"symbol"`           // e.g. BTCUSDT
	NotionalUSD    float64 `json:"notional_usd"`
	MaxSlippageBps float64 `json:"max_slippage_bps"`
	PriceLimit     float64 `json:"price_limit,omitempty"`
}

type ExpectedMetrics struct {
	EdgeBpsGross      float64 `json:"edge_bps_gross"`
	EdgeBpsNet        float64 `json:"edge_bps_net"`
	ProfitUSDNet      float64 `json:"profit_usd_net"`
	FeesUSDEst        float64 `json:"fees_usd_est"`
	SlippageUSDEst    float64 `json:"slippage_usd_est"`
	FundingRate8hBps  float64 `json:"funding_rate_8h_bps,omitempty"`
	AnnualYieldPctNet float64 `json:"annual_yield_pct_net,omitempty"`
}

type IntentConstraints struct {
	MaxAgeMs        int64  `json:"max_age_ms"`
	HedgePreference string `json:"hedge_preference"` // ATOMIC | SEQUENTIAL
	CooldownKey     string `json:"cooldown_key"`
}

// TradeIntent is the primary output of strategy engines.
type TradeIntent struct {
	IntentID    string            `json:"intent_id"`
	Strategy    string            `json:"strategy"`
	Symbol      string            `json:"symbol"` // canonical e.g. BTCUSDT
	TsMs        int64             `json:"ts_ms"`
	ExpiresMs   int64             `json:"expires_ms"`
	Legs        []TradeLeg        `json:"legs"`
	Expected    ExpectedMetrics   `json:"expected"`
	Constraints IntentConstraints `json:"constraints"`
}

type RejectionReason string

const (
	RejectInsufficientEdge RejectionReason = "insufficient_edge"
	RejectCooldown         RejectionReason = "cooldown"
	RejectDrawdownHalt     RejectionReason = "drawdown_halt"
	RejectChallengeRule    RejectionReason = "challenge_rule"
	RejectMaxAllocation    RejectionReason = "max_allocation"
)
