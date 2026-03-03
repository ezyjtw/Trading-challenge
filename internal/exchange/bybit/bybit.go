// Package bybit implements the Exchange interface for Bybit V5 API.
// Stripped to single-exchange: supports both linear (perp) and spot categories.
package bybit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/exchange"
	"github.com/shopspring/decimal"
)

type Client struct {
	http      *exchange.HTTPClient
	apiKey    string
	apiSecret string
	testnet   bool
}

func New(apiKey, apiSecret string, testnet bool) *Client {
	baseURL := "https://api.bybit.com"
	if testnet {
		baseURL = "https://api-testnet.bybit.com"
	}
	return &Client{
		http:      exchange.NewHTTPClient(baseURL, 10*time.Second),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		testnet:   testnet,
	}
}

func (c *Client) Name() string { return "bybit" }

func (c *Client) sign(params string) map[string]string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	recvWindow := "5000"
	preSign := ts + c.apiKey + recvWindow + params
	sig := exchange.HMACSha256(c.apiSecret, preSign)
	return map[string]string{
		"X-BAPI-API-KEY":     c.apiKey,
		"X-BAPI-SIGN":        sig,
		"X-BAPI-TIMESTAMP":   ts,
		"X-BAPI-RECV-WINDOW": recvWindow,
	}
}

type bybitResp[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  T      `json:"result"`
}

func checkCode(code int, msg string) error {
	if code != 0 {
		return fmt.Errorf("bybit API error code=%d: %s", code, msg)
	}
	return nil
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	cat := string(req.Category)
	if cat == "" {
		cat = "linear"
	}

	body := fmt.Sprintf(`{"category":"%s","symbol":"%s","side":"%s","orderType":"%s","qty":"%s","orderLinkId":"%s"`,
		cat, req.Symbol, string(req.Side), mapOrderType(req.Type),
		req.Quantity.StringFixed(8), req.ClientOrderID)

	if req.Type == exchange.OrderTypeLimit || req.Type == exchange.OrderTypeIOC {
		body += fmt.Sprintf(`,"price":"%s"`, req.Price.StringFixed(2))
		if req.Type == exchange.OrderTypeIOC {
			body += `,"timeInForce":"IOC"`
		} else {
			body += `,"timeInForce":"GTC"`
		}
	}
	if req.ReduceOnly {
		body += `,"reduceOnly":true`
	}
	if req.StopLoss.IsPositive() {
		body += fmt.Sprintf(`,"stopLoss":"%s"`, req.StopLoss.StringFixed(2))
	}
	if req.TakeProfit.IsPositive() {
		body += fmt.Sprintf(`,"takeProfit":"%s"`, req.TakeProfit.StringFixed(2))
	}
	body += "}"

	headers := c.sign(body)

	var resp bybitResp[struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodPost, "/v5/order/create", strings.NewReader(body), headers, &resp); err != nil {
		return nil, fmt.Errorf("bybit PlaceOrder: %w", err)
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	return &exchange.OrderResponse{
		OrderID:       resp.Result.OrderID,
		ClientOrderID: resp.Result.OrderLinkID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Type:          req.Type,
		Status:        exchange.OrderStatusNew,
		CreatedAt:     time.Now(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, category exchange.Category, symbol, orderID string) error {
	body := fmt.Sprintf(`{"category":"%s","symbol":"%s","orderId":"%s"}`, string(category), symbol, orderID)
	headers := c.sign(body)
	var resp bybitResp[struct{}]
	if err := c.http.DoJSON(ctx, http.MethodPost, "/v5/order/cancel", strings.NewReader(body), headers, &resp); err != nil {
		return err
	}
	return checkCode(resp.RetCode, resp.RetMsg)
}

func (c *Client) GetOrder(ctx context.Context, category exchange.Category, symbol, orderID string) (*exchange.OrderResponse, error) {
	query := fmt.Sprintf("category=%s&symbol=%s&orderId=%s", string(category), symbol, orderID)
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
			Symbol      string `json:"symbol"`
			Side        string `json:"side"`
			Status      string `json:"orderStatus"`
			AvgPrice    string `json:"avgPrice"`
			Qty         string `json:"qty"`
			CumExecQty  string `json:"cumExecQty"`
			CumExecFee  string `json:"cumExecFee"`
			UpdatedTime string `json:"updatedTime"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, "/v5/order/realtime?"+query, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("order %s not found", orderID)
	}

	o := resp.Result.List[0]
	avgPx := parseDecimal(o.AvgPrice)
	qty := parseDecimal(o.Qty)
	filledQty := parseDecimal(o.CumExecQty)
	fees := parseDecimal(o.CumExecFee)
	ut, _ := strconv.ParseInt(o.UpdatedTime, 10, 64)

	return &exchange.OrderResponse{
		OrderID:       o.OrderID,
		ClientOrderID: o.OrderLinkID,
		Symbol:        o.Symbol,
		Side:          exchange.Side(o.Side),
		Status:        mapStatus(o.Status),
		AvgFillPrice:  avgPx,
		Quantity:      qty,
		FilledQty:     filledQty,
		FeesUSD:       fees,
		UpdatedAt:     time.UnixMilli(ut),
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	query := "accountType=UNIFIED"
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			Coin []struct {
				Coin            string `json:"coin"`
				WalletBalance   string `json:"walletBalance"`
				AvailableToWithdraw string `json:"availableToWithdraw"`
				Locked          string `json:"locked"`
			} `json:"coin"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, "/v5/account/wallet-balance?"+query, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	if len(resp.Result.List) > 0 {
		for _, coin := range resp.Result.List[0].Coin {
			total := parseDecimal(coin.WalletBalance)
			if total.IsZero() {
				continue
			}
			balances = append(balances, exchange.Balance{
				Asset:  coin.Coin,
				Free:   parseDecimal(coin.AvailableToWithdraw),
				Locked: parseDecimal(coin.Locked),
				Total:  total,
			})
		}
	}
	return balances, nil
}

func (c *Client) GetPositions(ctx context.Context) ([]exchange.Position, error) {
	query := "category=linear&settleCoin=USDT"
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Size          string `json:"size"`
			AvgPrice      string `json:"avgPrice"`
			MarkPrice     string `json:"markPrice"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			Leverage      string `json:"leverage"`
			PositionValue string `json:"positionValue"`
			LiqPrice      string `json:"liqPrice"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, "/v5/position/list?"+query, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	var positions []exchange.Position
	for _, p := range resp.Result.List {
		qty := parseDecimal(p.Size)
		if qty.IsZero() {
			continue
		}
		side := "LONG"
		if p.Side == "Sell" {
			side = "SHORT"
		}
		positions = append(positions, exchange.Position{
			Symbol:        p.Symbol,
			Side:          side,
			Quantity:      qty,
			EntryPrice:    parseDecimal(p.AvgPrice),
			MarkPrice:     parseDecimal(p.MarkPrice),
			UnrealizedPnL: parseDecimal(p.UnrealisedPnl),
			Leverage:      parseDecimal(p.Leverage),
			NotionalUSD:   parseDecimal(p.PositionValue),
			LiqPrice:      parseDecimal(p.LiqPrice),
		})
	}
	return positions, nil
}

func (c *Client) GetAccountEquity(ctx context.Context) (*exchange.AccountEquity, error) {
	query := "accountType=UNIFIED"
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			TotalEquity        string `json:"totalEquity"`
			TotalAvailableBalance string `json:"totalAvailableBalance"`
			TotalPerpUPL       string `json:"totalPerpUPL"`
			TotalInitialMargin string `json:"totalInitialMargin"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, "/v5/account/wallet-balance?"+query, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("no account data returned")
	}
	a := resp.Result.List[0]
	return &exchange.AccountEquity{
		TotalEquity:      parseDecimal(a.TotalEquity),
		AvailableBalance: parseDecimal(a.TotalAvailableBalance),
		UnrealizedPnL:    parseDecimal(a.TotalPerpUPL),
		TotalMargin:      parseDecimal(a.TotalInitialMargin),
	}, nil
}

// ── Market Data (REST) ──────────────────────────────────────────────────────

func (c *Client) GetFundingRate(ctx context.Context, symbol string) (*exchange.FundingInfo, error) {
	path := "/v5/market/tickers?category=linear&symbol=" + symbol
	var resp bybitResp[struct {
		List []struct {
			FundingRate     string `json:"fundingRate"`
			NextFundingTime string `json:"nextFundingTime"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("no ticker for %s", symbol)
	}
	t := resp.Result.List[0]
	nft, _ := strconv.ParseInt(t.NextFundingTime, 10, 64)
	return &exchange.FundingInfo{
		Symbol:          symbol,
		FundingRate:     parseDecimal(t.FundingRate),
		NextFundingTime: time.UnixMilli(nft),
	}, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, category exchange.Category, symbol string) (decimal.Decimal, error) {
	path := fmt.Sprintf("/v5/market/tickers?category=%s&symbol=%s", string(category), symbol)
	var resp bybitResp[struct {
		List []struct {
			LastPrice string `json:"lastPrice"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return decimal.Zero, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return decimal.Zero, err
	}
	if len(resp.Result.List) == 0 {
		return decimal.Zero, fmt.Errorf("no ticker for %s", symbol)
	}
	return parseDecimal(resp.Result.List[0].LastPrice), nil
}

func (c *Client) GetConstraints(_ context.Context, category exchange.Category, symbol string) (*exchange.VenueConstraints, error) {
	// Sensible defaults for Bybit. In production, fetch from /v5/market/instruments-info.
	switch category {
	case exchange.CategorySpot:
		return &exchange.VenueConstraints{
			Symbol:      symbol,
			TickSize:    0.01,
			LotSize:     0.00001,
			MinQty:      0.00001,
			MinNotional: 1.0,
		}, nil
	default: // linear
		return &exchange.VenueConstraints{
			Symbol:      symbol,
			TickSize:    0.10,
			LotSize:     0.001,
			MinQty:      0.001,
			MinNotional: 5.0,
		}, nil
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func parseDecimal(s string) decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return d
}

func mapOrderType(t exchange.OrderType) string {
	switch t {
	case exchange.OrderTypeMarket:
		return "Market"
	case exchange.OrderTypeLimit, exchange.OrderTypeIOC:
		return "Limit"
	default:
		return "Market"
	}
}

func mapStatus(s string) exchange.OrderStatus {
	switch s {
	case "New", "Created":
		return exchange.OrderStatusNew
	case "PartiallyFilled":
		return exchange.OrderStatusPartiallyFilled
	case "Filled":
		return exchange.OrderStatusFilled
	case "Cancelled":
		return exchange.OrderStatusCancelled
	case "Rejected":
		return exchange.OrderStatusRejected
	case "Deactivated":
		return exchange.OrderStatusExpired
	default:
		return exchange.OrderStatus(s)
	}
}
