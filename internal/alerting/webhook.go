// Package alerting provides webhook notifications for critical trading events.
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// WebhookConfig configures the webhook alerter.
type WebhookConfig struct {
	// URL is the webhook endpoint (Telegram bot, Discord webhook, Slack, etc).
	URL string `yaml:"url"`
	// Enabled controls whether alerts are actually sent.
	Enabled bool `yaml:"enabled"`
	// RateLimitPerMin caps how many alerts can be sent per minute.
	RateLimitPerMin int `yaml:"rate_limit_per_min"`
}

// Alert represents a structured alert payload.
type Alert struct {
	Level   string `json:"level"`   // INFO, WARN, CRITICAL, HALT
	Source  string `json:"source"`  // e.g. "risk-daemon", "circuit-breaker"
	Message string `json:"message"`
	Metric  string `json:"metric,omitempty"`
	Value   string `json:"value,omitempty"`
	TsUTC   string `json:"ts_utc"`
}

// Alerter sends webhook notifications for critical events.
type Alerter struct {
	cfg        WebhookConfig
	httpClient *http.Client
	sentCount  int
	windowStart time.Time
}

// NewAlerter creates a new webhook alerter.
func NewAlerter(cfg WebhookConfig) *Alerter {
	if cfg.RateLimitPerMin == 0 {
		cfg.RateLimitPerMin = 10
	}
	return &Alerter{
		cfg: cfg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		windowStart: time.Now(),
	}
}

// Send sends an alert via the configured webhook.
func (a *Alerter) Send(ctx context.Context, alert Alert) {
	if !a.cfg.Enabled || a.cfg.URL == "" {
		return
	}

	// Rate limiting: reset window every minute.
	if time.Since(a.windowStart) > time.Minute {
		a.sentCount = 0
		a.windowStart = time.Now()
	}
	if a.sentCount >= a.cfg.RateLimitPerMin {
		return
	}

	if alert.TsUTC == "" {
		alert.TsUTC = time.Now().UTC().Format(time.RFC3339)
	}

	body, err := json.Marshal(alert)
	if err != nil {
		slog.Warn("alerting: marshal failed", "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.URL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alerting: create request failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		slog.Warn("alerting: send failed", "err", err)
		return
	}
	resp.Body.Close()
	a.sentCount++

	slog.Info("alert sent", "level", alert.Level, "source", alert.Source, "status", resp.StatusCode)
}

// SendCritical is a convenience method for critical alerts.
func (a *Alerter) SendCritical(ctx context.Context, source, message string) {
	a.Send(ctx, Alert{
		Level:   "CRITICAL",
		Source:  source,
		Message: message,
	})
}

// SendHalt is a convenience method for halt-level alerts.
func (a *Alerter) SendHalt(ctx context.Context, source, message string) {
	a.Send(ctx, Alert{
		Level:   "HALT",
		Source:  source,
		Message: message,
	})
}

// FormatMetric creates a formatted alert with a metric value.
func FormatMetric(level, source, metric string, value, threshold float64) Alert {
	return Alert{
		Level:   level,
		Source:  source,
		Message: fmt.Sprintf("%s: %.2f (threshold: %.2f)", metric, value, threshold),
		Metric:  metric,
		Value:   fmt.Sprintf("%.4f", value),
	}
}
