// Package ledger persists trading data to PostgreSQL.
// Stripped: no TenantID.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/execution"
	"github.com/ezyjtw/Trading-challenge/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChallengeDay represents a single day's progress in the challenge.
type ChallengeDay struct {
	Day              string
	Equity           float64
	DrawdownPct      float64
	Trades           int
	FundingCollected float64
}

// Writer handles all ledger persistence to PostgreSQL.
type Writer struct {
	pool *pgxpool.Pool
}

// NewWriter creates a ledger writer with a connection pool and applies the schema.
func NewWriter(connString string) (*Writer, error) {
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		return nil, fmt.Errorf("ledger: connect to postgres: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ledger: ping postgres: %w", err)
	}

	// Run migrations.
	if _, err := pool.Exec(context.Background(), Schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ledger: apply schema: %w", err)
	}

	slog.Info("ledger: connected to postgres, schema applied")
	return &Writer{pool: pool}, nil
}

// Close shuts down the connection pool.
func (w *Writer) Close() {
	w.pool.Close()
}

// ── Intent persistence ───────────────────────────────────────────────────────

// WriteIntent persists a trade intent with JSON-encoded legs and expected metrics.
func (w *Writer) WriteIntent(ctx context.Context, intentID, strategy, symbol string,
	tsMs, expiresMs int64, legsJSON, expectedJSON []byte, status string) error {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO trade_intents (intent_id, strategy, symbol, ts_ms, expires_ms, legs, expected, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (intent_id) DO UPDATE SET status = $8`,
		intentID, strategy, symbol, tsMs, expiresMs, legsJSON, expectedJSON, status,
	)
	if err != nil {
		return fmt.Errorf("ledger: write intent %s: %w", intentID, err)
	}
	return nil
}

// ── Execution event persistence ──────────────────────────────────────────────

// WriteExecutionEvent persists an execution event.
func (w *Writer) WriteExecutionEvent(ctx context.Context, ev execution.ExecutionEvent) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO execution_events
			(event_type, intent_id, leg_index, symbol, action, strategy, market,
			 requested_notional_usd, filled_notional_usd, filled_price,
			 slippage_bps_actual, fees_usd_actual, ts_ms, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		string(ev.EventType), ev.IntentID, ev.LegIndex, ev.Symbol, ev.Action,
		ev.Strategy, ev.Market, ev.RequestedNotionalUSD, ev.FilledNotionalUSD,
		ev.FilledPrice, ev.SlippageBpsActual, ev.FeesUSDActual, ev.TsMs,
		ev.LatencyMs,
	)
	if err != nil {
		slog.Warn("ledger: write execution event failed", "err", err)
	}
}

// ── Fill persistence ─────────────────────────────────────────────────────────

// WriteFill persists a fill summary.
func (w *Writer) WriteFill(ctx context.Context, fill execution.FillSummary) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO fills
			(intent_id, strategy, symbol, buy_price, sell_price, total_fees, net_pnl, ts_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		fill.IntentID, fill.Strategy, fill.Symbol, fill.BuyPrice, fill.SellPrice,
		fill.TotalFees, fill.NetPnL, fill.TsMs,
	)
	if err != nil {
		slog.Warn("ledger: write fill failed", "err", err)
	}
}

// ── Risk state persistence ───────────────────────────────────────────────────

// WriteRiskState persists a risk state snapshot.
func (w *Writer) WriteRiskState(ctx context.Context, state risk.State) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO risk_snapshots
			(mode, ts_ms, drawdown_pct, daily_drawdown_pct, peak_equity_usd,
			 current_equity_usd, error_rate_5m_pct, reason, challenge_phase)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		state.Mode.String(), state.TsMs, state.DrawdownPct, state.DailyDrawdownPct,
		state.PeakEquityUSD, state.CurrentEquityUSD, state.ErrorRate5mPct,
		state.Reason, state.ChallengePhase,
	)
	if err != nil {
		slog.Warn("ledger: write risk state failed", "err", err)
	}
}

// ── Challenge progress persistence ───────────────────────────────────────────

// WriteChallengeDayProgress persists a challenge day progress record.
// Day should be in YYYY-MM-DD format.
func (w *Writer) WriteChallengeDayProgress(ctx context.Context, day string, equity, drawdownPct float64, trades int, fundingCollected float64) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO challenge_progress (day, equity, drawdown_pct, trades, funding_collected, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (day) DO UPDATE SET
			equity = $2,
			drawdown_pct = $3,
			trades = $4,
			funding_collected = $5,
			updated_at = NOW()`,
		day, equity, drawdownPct, trades, fundingCollected,
	)
	if err != nil {
		slog.Warn("ledger: write challenge progress failed", "err", err)
	}
}

// ── Funding payment persistence ──────────────────────────────────────────────

// WriteFundingPayment persists a funding payment record.
func (w *Writer) WriteFundingPayment(ctx context.Context, symbol string, rate, amountUSD float64, tsMs int64, direction string) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO funding_payments (symbol, rate, amount_usd, ts_ms, direction)
		VALUES ($1, $2, $3, $4, $5)`,
		symbol, rate, amountUSD, tsMs, direction,
	)
	if err != nil {
		slog.Warn("ledger: write funding payment failed", "err", err)
	}
}

// ── Raw event persistence ────────────────────────────────────────────────────

// WriteRaw logs a raw event. Used for events that don't have a dedicated
// persistence method (e.g. challenge events).
func (w *Writer) WriteRaw(ctx context.Context, table, data string) {
	// For raw events, just log them. In production, these could be written
	// to a generic events table.
	slog.Info("ledger: raw event", "table", table, "data_len", len(data))
}

// ── Queries ──────────────────────────────────────────────────────────────────

// PnLSummary returns total PnL grouped by strategy.
func (w *Writer) PnLSummary(ctx context.Context) (map[string]float64, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT strategy, COALESCE(SUM(net_pnl), 0) FROM fills GROUP BY strategy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]float64)
	for rows.Next() {
		var strategy string
		var pnl float64
		if err := rows.Scan(&strategy, &pnl); err != nil {
			continue
		}
		result[strategy] = pnl
	}
	return result, nil
}

// TotalPnL returns the total net P&L across all fills.
func (w *Writer) TotalPnL(ctx context.Context) (float64, error) {
	var total float64
	err := w.pool.QueryRow(ctx, `SELECT COALESCE(SUM(net_pnl), 0) FROM fills`).Scan(&total)
	if err != nil && err != pgx.ErrNoRows {
		return 0, fmt.Errorf("ledger: total pnl: %w", err)
	}
	return total, nil
}

// TotalFundingCollected returns the total funding payments received.
func (w *Writer) TotalFundingCollected(ctx context.Context) (float64, error) {
	var total float64
	err := w.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd), 0) FROM funding_payments WHERE direction = 'received'`).Scan(&total)
	if err != nil && err != pgx.ErrNoRows {
		return 0, fmt.Errorf("ledger: total funding: %w", err)
	}
	return total, nil
}

// TradingDaysCount returns the number of distinct days with at least one fill.
func (w *Writer) TradingDaysCount(ctx context.Context) (int, error) {
	var count int
	err := w.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT DATE(TO_TIMESTAMP(ts_ms / 1000.0))) FROM fills`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("ledger: trading days count: %w", err)
	}
	return count, nil
}

// RecentFills returns the last N fills as JSON.
func (w *Writer) RecentFills(ctx context.Context, limit int) ([]json.RawMessage, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT row_to_json(f) FROM fills f ORDER BY ts_ms DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fills []json.RawMessage
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		fills = append(fills, raw)
	}
	return fills, nil
}

// GetChallengeProgress returns all challenge progress records ordered by day.
func (w *Writer) GetChallengeProgress(ctx context.Context) ([]ChallengeDay, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT day, equity, drawdown_pct, trades, funding_collected
		FROM challenge_progress
		ORDER BY day ASC`)
	if err != nil {
		return nil, fmt.Errorf("ledger: query challenge progress: %w", err)
	}
	defer rows.Close()

	var records []ChallengeDay
	for rows.Next() {
		var rec ChallengeDay
		if err := rows.Scan(&rec.Day, &rec.Equity, &rec.DrawdownPct, &rec.Trades, &rec.FundingCollected); err != nil {
			return nil, fmt.Errorf("ledger: scan challenge progress: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// LatestEquity returns the most recent equity from challenge_progress.
func (w *Writer) LatestEquity(ctx context.Context) (float64, time.Time, error) {
	var equity float64
	var day string
	err := w.pool.QueryRow(ctx, `
		SELECT equity, day FROM challenge_progress
		ORDER BY day DESC LIMIT 1`).Scan(&equity, &day)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, fmt.Errorf("ledger: latest equity: %w", err)
	}
	t, _ := time.Parse("2006-01-02", day)
	return equity, t, nil
}
