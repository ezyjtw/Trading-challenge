package exchange

import (
	"math"
	"time"

	"github.com/shopspring/decimal"
)

type Side string

const (
	SideBuy  Side = "Buy"
	SideSell Side = "Sell"
)

type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeIOC    OrderType = "IOC"
)

type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
)

type Category string

const (
	CategoryLinear Category = "linear" // USDT perpetual
	CategorySpot   Category = "spot"
)

type OrderRequest struct {
	Category       Category  `json:"category"`
	Symbol         string    `json:"symbol"`
	Side           Side      `json:"side"`
	Type           OrderType `json:"type"`
	Quantity       decimal.Decimal `json:"quantity"`
	Price          decimal.Decimal `json:"price,omitempty"`
	NotionalUSD    decimal.Decimal `json:"notional_usd"`
	MaxSlippageBps float64         `json:"max_slippage_bps"`
	ClientOrderID  string          `json:"client_order_id"`
	ReduceOnly     bool            `json:"reduce_only,omitempty"`
	// StopLoss attaches a stop-loss trigger price to the order.
	StopLoss       decimal.Decimal `json:"stop_loss,omitempty"`
	// TakeProfit attaches a take-profit trigger price to the order.
	TakeProfit     decimal.Decimal `json:"take_profit,omitempty"`
}

type OrderResponse struct {
	OrderID       string          `json:"order_id"`
	ClientOrderID string          `json:"client_order_id"`
	Symbol        string          `json:"symbol"`
	Side          Side            `json:"side"`
	Type          OrderType       `json:"type"`
	Status        OrderStatus     `json:"status"`
	Price         decimal.Decimal `json:"price"`
	AvgFillPrice  decimal.Decimal `json:"avg_fill_price"`
	Quantity      decimal.Decimal `json:"quantity"`
	FilledQty     decimal.Decimal `json:"filled_qty"`
	FeesUSD       decimal.Decimal `json:"fees_usd"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type Balance struct {
	Asset    string          `json:"asset"`
	Free     decimal.Decimal `json:"free"`
	Locked   decimal.Decimal `json:"locked"`
	Total    decimal.Decimal `json:"total"`
	USDValue decimal.Decimal `json:"usd_value"`
}

type Position struct {
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"` // LONG | SHORT
	Quantity      decimal.Decimal `json:"quantity"`
	EntryPrice    decimal.Decimal `json:"entry_price"`
	MarkPrice     decimal.Decimal `json:"mark_price"`
	UnrealizedPnL decimal.Decimal `json:"unrealized_pnl"`
	Leverage      decimal.Decimal `json:"leverage"`
	NotionalUSD   decimal.Decimal `json:"notional_usd"`
	LiqPrice      decimal.Decimal `json:"liquidation_price"`
}

type AccountEquity struct {
	TotalEquity    decimal.Decimal `json:"total_equity"`
	AvailableBalance decimal.Decimal `json:"available_balance"`
	UnrealizedPnL  decimal.Decimal `json:"unrealized_pnl"`
	TotalMargin    decimal.Decimal `json:"total_margin"`
}

type FundingInfo struct {
	Symbol       string          `json:"symbol"`
	FundingRate  decimal.Decimal `json:"funding_rate"`
	NextFundingTime time.Time    `json:"next_funding_time"`
}

type VenueConstraints struct {
	Symbol      string  `json:"symbol"`
	TickSize    float64 `json:"tick_size"`
	LotSize     float64 `json:"lot_size"`
	MinQty      float64 `json:"min_qty"`
	MinNotional float64 `json:"min_notional"`
}

func (vc VenueConstraints) RoundPrice(price float64) float64 {
	if vc.TickSize <= 0 {
		return price
	}
	return math.Round(price/vc.TickSize) * vc.TickSize
}

func (vc VenueConstraints) RoundQty(qty float64) float64 {
	if vc.LotSize <= 0 {
		return qty
	}
	return math.Floor(qty/vc.LotSize) * vc.LotSize
}
