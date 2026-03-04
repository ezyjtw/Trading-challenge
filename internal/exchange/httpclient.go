package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type HTTPClient struct {
	Client  *http.Client
	BaseURL string
}

func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &HTTPClient{
		Client:  &http.Client{Timeout: timeout},
		BaseURL: strings.TrimRight(baseURL, "/"),
	}
}

func (c *HTTPClient) DoJSON(ctx context.Context, method, path string, body io.Reader, headers map[string]string, dst interface{}) error {
	// Buffer the body so we can retry on transient errors.
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("reading request body: %w", err)
		}
	}

	const maxRetries = 3
	backoffs := [3]time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			slog.Warn("HTTP retry", "attempt", attempt, "method", method, "path", path)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffs[attempt-1]):
			}
		}

		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		url := c.BaseURL + path
		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.Client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("executing request %s %s: %w", method, path, err)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("reading response body: %w", err)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &APIError{
				StatusCode: resp.StatusCode,
				Body:       string(respBody),
				Method:     method,
				Path:       path,
			}
			if apiErr.IsTemporary() && attempt < maxRetries {
				lastErr = apiErr
				continue
			}
			return apiErr
		}

		if dst != nil {
			if err := json.Unmarshal(respBody, dst); err != nil {
				return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
			}
		}
		return nil
	}

	return fmt.Errorf("request %s %s failed after %d retries: %w", method, path, maxRetries, lastErr)
}

// IsTemporaryErr checks if an error is a retryable API error.
func IsTemporaryErr(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsTemporary()
}

type APIError struct {
	StatusCode int
	Body       string
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("exchange API error: %s %s returned %d: %s",
		e.Method, e.Path, e.StatusCode, e.Body)
}

func (e *APIError) IsRateLimited() bool { return e.StatusCode == 429 }
func (e *APIError) IsTemporary() bool   { return e.StatusCode == 429 || e.StatusCode >= 500 }
