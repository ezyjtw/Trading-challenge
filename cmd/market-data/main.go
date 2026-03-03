// Market data service: connects to Bybit WebSocket, publishes normalized quotes to Redis.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/Trading-challenge/internal/eventbus"
	"github.com/ezyjtw/Trading-challenge/internal/marketdata"
)

func main() {
	slog.Info("market-data: starting")

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	bus, err := eventbus.New(eventbus.Config{Addr: redisAddr})
	if err != nil {
		slog.Error("redis connect failed", "err", err)
		os.Exit(1)
	}
	defer bus.Close()

	wsURL := "wss://stream.bybit.com/v5/public/linear"
	if os.Getenv("BYBIT_TESTNET") == "true" {
		wsURL = "wss://stream-testnet.bybit.com/v5/public/linear"
	}

	symbols := []string{"BTCUSDT", "ETHUSDT"}

	feed, quoteCh := marketdata.NewBybitFeed(marketdata.BybitConfig{
		WsURL:              wsURL,
		Symbols:            symbols,
		ReconnectBackoffMs: []int{1000, 2000, 5000, 10000},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go feed.Start(ctx)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	published := 0

	for {
		select {
		case q := <-quoteCh:
			data, _ := json.Marshal(q)
			if err := bus.Publish(ctx, eventbus.StreamMarketData, json.RawMessage(data)); err != nil {
				slog.Warn("publish failed", "err", err)
			}
			published++

		case <-ticker.C:
			slog.Info("market-data stats", "published", published)

		case <-sig:
			slog.Info("market-data: shutting down")
			feed.Stop()
			cancel()
			return
		}
	}
}
