// Package eventbus provides simplified Redis Stream communication between services.
// Stripped from multi-tenant: no TenantID, no consensus streams.
package eventbus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Stream names used across the system.
const (
	StreamMarketData   = "market:data"
	StreamTradeIntents = "trade:intents"
	StreamApproved     = "trade:approved"
	StreamExecution    = "execution:events"
	StreamRisk         = "risk:state"
	StreamChallenge    = "challenge:events"
	StreamLedger       = "ledger:entries"
)

type Config struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	UseTLS   bool   `yaml:"use_tls"`
}

type Bus struct {
	rdb *redis.Client
}

func New(cfg Config) (*Bus, error) {
	var opts *redis.Options
	var err error

	if strings.HasPrefix(cfg.Addr, "redis://") || strings.HasPrefix(cfg.Addr, "rediss://") {
		opts, err = redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
	} else {
		opts = &redis.Options{Addr: cfg.Addr}
	}
	if cfg.Password != "" {
		opts.Password = cfg.Password
	}
	if cfg.UseTLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Bus{rdb: rdb}, nil
}

// EnsureGroup creates a consumer group on a stream if it doesn't exist.
func (b *Bus) EnsureGroup(ctx context.Context, stream, group string) error {
	err := b.rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("creating consumer group %s on %s: %w", group, stream, err)
	}
	return nil
}

// Publish adds a JSON-encoded message to a Redis stream.
func (b *Bus) Publish(ctx context.Context, stream string, msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// Read consumes messages from a stream using a consumer group.
func (b *Bus) Read(ctx context.Context, stream, group, consumer string, count int64, block time.Duration) ([]json.RawMessage, error) {
	streams, err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("XReadGroup: %w", err)
	}

	var msgs []json.RawMessage
	for _, s := range streams {
		for _, msg := range s.Messages {
			raw, ok := msg.Values["data"].(string)
			if !ok {
				slog.Warn("eventbus: msg missing data field", "id", msg.ID)
				continue
			}
			msgs = append(msgs, json.RawMessage(raw))
			if err := b.rdb.XAck(ctx, stream, group, msg.ID).Err(); err != nil {
				slog.Warn("eventbus: XAck failed", "id", msg.ID, "err", err)
			}
		}
	}
	return msgs, nil
}

// Set stores a key-value pair in Redis with optional TTL.
func (b *Bus) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, key, string(data), ttl).Err()
}

// Get retrieves a value from Redis and unmarshals it.
func (b *Bus) Get(ctx context.Context, key string, dst interface{}) error {
	raw, err := b.rdb.Get(ctx, key).Result()
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), dst)
}

// GetString retrieves a raw string value.
func (b *Bus) GetString(ctx context.Context, key string) (string, error) {
	return b.rdb.Get(ctx, key).Result()
}

// SetString stores a raw string value.
func (b *Bus) SetString(ctx context.Context, key, value string, ttl time.Duration) error {
	return b.rdb.Set(ctx, key, value, ttl).Err()
}

// Client returns the underlying Redis client for direct access.
func (b *Bus) Client() *redis.Client { return b.rdb }

func (b *Bus) Close() error { return b.rdb.Close() }
