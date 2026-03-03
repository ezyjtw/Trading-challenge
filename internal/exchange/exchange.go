package exchange

import (
	"context"

	"github.com/shopspring/decimal"
)

// Exchange is the unified interface for Bybit V5 API operations.
// Stripped to single-exchange: no registry, no multi-venue abstraction.
type Exchange interface {
	Name() string

	// Trading
	PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error)
	CancelOrder(ctx context.Context, category Category, symbol, orderID string) error
	GetOrder(ctx context.Context, category Category, symbol, orderID string) (*OrderResponse, error)

	// Account
	GetBalances(ctx context.Context) ([]Balance, error)
	GetPositions(ctx context.Context) ([]Position, error)
	GetAccountEquity(ctx context.Context) (*AccountEquity, error)

	// Market data (REST)
	GetFundingRate(ctx context.Context, symbol string) (*FundingInfo, error)
	GetTickerPrice(ctx context.Context, category Category, symbol string) (decimal.Decimal, error)

	// Constraints
	GetConstraints(ctx context.Context, category Category, symbol string) (*VenueConstraints, error)
}
