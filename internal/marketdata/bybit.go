package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// BybitConfig configures the Bybit market data adapter.
type BybitConfig struct {
	// WsURL is the WebSocket endpoint for Bybit V5 public linear data.
	// Default: wss://stream.bybit.com/v5/public/linear
	WsURL string `yaml:"ws_url"`

	// RestURL is the REST API base URL for Bybit V5.
	// Default: https://api.bybit.com
	RestURL string `yaml:"rest_url"`

	// Symbols to subscribe to (e.g. ["BTCUSDT", "ETHUSDT"]).
	Symbols []string `yaml:"symbols"`

	// ReconnectBackoffMs specifies successive backoff delays in milliseconds.
	// After exhausting the list, the last value is reused.
	// Default: [1000, 2000, 4000, 8000, 16000, 30000]
	ReconnectBackoffMs []int `yaml:"reconnect_backoff_ms"`

	// HeartbeatIntervalMs is the interval between ping frames (ms).
	// Default: 20000
	HeartbeatIntervalMs int `yaml:"heartbeat_interval_ms"`

	// FundingPollIntervalMs is how often to poll REST for funding rate history.
	// Default: 60000
	FundingPollIntervalMs int `yaml:"funding_poll_interval_ms"`

	// OpenInterestPollIntervalMs is how often to poll REST for open interest.
	// Default: 30000
	OpenInterestPollIntervalMs int `yaml:"open_interest_poll_interval_ms"`
}

// DefaultBybitConfig returns a BybitConfig with sane defaults.
func DefaultBybitConfig() BybitConfig {
	return BybitConfig{
		WsURL:                      "wss://stream.bybit.com/v5/public/linear",
		RestURL:                    "https://api.bybit.com",
		Symbols:                    []string{"BTCUSDT", "ETHUSDT"},
		ReconnectBackoffMs:         []int{1000, 2000, 4000, 8000, 16000, 30000},
		HeartbeatIntervalMs:        20000,
		FundingPollIntervalMs:      60000,
		OpenInterestPollIntervalMs: 30000,
	}
}

// BybitFeed connects to Bybit V5 public WebSocket and publishes parsed quotes
// to a channel. It also runs REST pollers for funding rate history and open
// interest at configurable intervals.
type BybitFeed struct {
	cfg    BybitConfig
	quotes chan Quote

	mu   sync.Mutex
	conn *websocket.Conn

	// latest tracks the most recent quote per symbol for REST enrichment.
	latest   map[string]*Quote
	latestMu sync.RWMutex

	cancel context.CancelFunc
}

// NewBybitFeed creates a new Bybit market data feed.
// Quotes are published to the returned channel.
func NewBybitFeed(cfg BybitConfig) (*BybitFeed, chan Quote) {
	if cfg.WsURL == "" {
		cfg.WsURL = DefaultBybitConfig().WsURL
	}
	if cfg.RestURL == "" {
		cfg.RestURL = DefaultBybitConfig().RestURL
	}
	if len(cfg.Symbols) == 0 {
		cfg.Symbols = DefaultBybitConfig().Symbols
	}
	if len(cfg.ReconnectBackoffMs) == 0 {
		cfg.ReconnectBackoffMs = DefaultBybitConfig().ReconnectBackoffMs
	}
	if cfg.HeartbeatIntervalMs == 0 {
		cfg.HeartbeatIntervalMs = DefaultBybitConfig().HeartbeatIntervalMs
	}
	if cfg.FundingPollIntervalMs == 0 {
		cfg.FundingPollIntervalMs = DefaultBybitConfig().FundingPollIntervalMs
	}
	if cfg.OpenInterestPollIntervalMs == 0 {
		cfg.OpenInterestPollIntervalMs = DefaultBybitConfig().OpenInterestPollIntervalMs
	}

	ch := make(chan Quote, 256)
	f := &BybitFeed{
		cfg:    cfg,
		quotes: ch,
		latest: make(map[string]*Quote),
	}
	return f, ch
}

// Start begins the WebSocket connection loop and REST pollers. It blocks until
// ctx is cancelled.
func (f *BybitFeed) Start(ctx context.Context) {
	ctx, f.cancel = context.WithCancel(ctx)

	var wg sync.WaitGroup

	// WebSocket connection loop with reconnect.
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.wsLoop(ctx)
	}()

	// REST poller: funding rate history.
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.fundingPoller(ctx)
	}()

	// REST poller: open interest.
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.openInterestPoller(ctx)
	}()

	wg.Wait()
}

// Stop cancels the feed context, disconnecting WebSocket and stopping pollers.
func (f *BybitFeed) Stop() {
	if f.cancel != nil {
		f.cancel()
	}
}

// ---------- WebSocket connection loop ----------

func (f *BybitFeed) wsLoop(ctx context.Context) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := f.connectAndRead(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("marketdata/bybit: ws error: %v", err)
		}

		// Exponential backoff for reconnect.
		backoffMs := f.backoffMs(attempt)
		jitter := time.Duration(rand.Int63n(int64(backoffMs)/4+1)) * time.Millisecond
		delay := time.Duration(backoffMs)*time.Millisecond + jitter
		log.Printf("marketdata/bybit: reconnecting in %v (attempt %d)", delay, attempt+1)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		attempt++
	}
}

func (f *BybitFeed) backoffMs(attempt int) int {
	if attempt < len(f.cfg.ReconnectBackoffMs) {
		return f.cfg.ReconnectBackoffMs[attempt]
	}
	return f.cfg.ReconnectBackoffMs[len(f.cfg.ReconnectBackoffMs)-1]
}

func (f *BybitFeed) connectAndRead(ctx context.Context) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, f.cfg.WsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", f.cfg.WsURL, err)
	}

	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.conn = nil
		f.mu.Unlock()
		conn.Close()
	}()

	log.Printf("marketdata/bybit: connected to %s", f.cfg.WsURL)

	// Subscribe to ticker topics.
	if err := f.subscribe(conn); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Start heartbeat goroutine.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go f.heartbeat(heartbeatCtx, conn)

	// Read messages.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		f.handleMessage(message)
	}
}

// subscribe sends the subscription request for ticker topics.
func (f *BybitFeed) subscribe(conn *websocket.Conn) error {
	args := make([]string, len(f.cfg.Symbols))
	for i, sym := range f.cfg.Symbols {
		args[i] = "tickers." + sym
	}

	sub := struct {
		Op   string   `json:"op"`
		Args []string `json:"args"`
	}{
		Op:   "subscribe",
		Args: args,
	}

	data, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// heartbeat sends ping frames at the configured interval.
func (f *BybitFeed) heartbeat(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(time.Duration(f.cfg.HeartbeatIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	ping := struct {
		Op string `json:"op"`
	}{Op: "ping"}
	pingData, _ := json.Marshal(ping)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.mu.Lock()
			if f.conn == conn {
				if err := conn.WriteMessage(websocket.TextMessage, pingData); err != nil {
					log.Printf("marketdata/bybit: heartbeat ping failed: %v", err)
				}
			}
			f.mu.Unlock()
		}
	}
}

// ---------- Message parsing ----------

// bybitWsMsg represents the envelope of a Bybit V5 WebSocket message.
type bybitWsMsg struct {
	Topic string          `json:"topic"`
	Type  string          `json:"type"` // "snapshot" or "delta"
	Ts    int64           `json:"ts"`
	Data  json.RawMessage `json:"data"`

	// Pong response fields.
	Op     string `json:"op,omitempty"`
	RetMsg string `json:"ret_msg,omitempty"`
}

// bybitTickerData matches Bybit V5 linear ticker data fields.
type bybitTickerData struct {
	Symbol        string `json:"symbol"`
	LastPrice     string `json:"lastPrice"`
	Bid1Price     string `json:"bid1Price"`
	Ask1Price     string `json:"ask1Price"`
	MarkPrice     string `json:"markPrice"`
	IndexPrice    string `json:"indexPrice"`
	FundingRate   string `json:"fundingRate"`
	NextFundingTime string `json:"nextFundingTime"`
	Volume24h     string `json:"volume24h"`
}

func (f *BybitFeed) handleMessage(raw []byte) {
	var msg bybitWsMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("marketdata/bybit: unmarshal ws msg: %v", err)
		return
	}

	// Handle pong / subscription responses.
	if msg.Op == "pong" || msg.Op == "subscribe" {
		return
	}

	// We only care about ticker topics.
	if msg.Topic == "" || msg.Data == nil {
		return
	}

	var ticker bybitTickerData
	if err := json.Unmarshal(msg.Data, &ticker); err != nil {
		log.Printf("marketdata/bybit: unmarshal ticker data: %v", err)
		return
	}

	quote := f.tickerToQuote(ticker, msg.Ts)

	// Update latest quote cache.
	f.latestMu.Lock()
	cached := f.latest[quote.Symbol]
	if cached != nil {
		// For delta updates, merge non-zero fields into existing quote.
		if quote.BestBid == 0 {
			quote.BestBid = cached.BestBid
		}
		if quote.BestAsk == 0 {
			quote.BestAsk = cached.BestAsk
		}
		if quote.MarkPrice == 0 {
			quote.MarkPrice = cached.MarkPrice
		}
		if quote.IndexPrice == 0 {
			quote.IndexPrice = cached.IndexPrice
		}
		if quote.LastPrice == 0 {
			quote.LastPrice = cached.LastPrice
		}
		if quote.Volume24h == 0 {
			quote.Volume24h = cached.Volume24h
		}
		// FundingRate can legitimately be 0, so we only merge if the string was empty.
		if ticker.FundingRate == "" {
			quote.FundingRate = cached.FundingRate
		}
	}
	copyQ := quote
	f.latest[quote.Symbol] = &copyQ
	f.latestMu.Unlock()

	// Non-blocking publish.
	select {
	case f.quotes <- quote:
	default:
		log.Printf("marketdata/bybit: quote channel full, dropping %s", quote.Symbol)
	}
}

func (f *BybitFeed) tickerToQuote(t bybitTickerData, tsMs int64) Quote {
	return Quote{
		Symbol:      t.Symbol,
		BestBid:     parseFloat(t.Bid1Price),
		BestAsk:     parseFloat(t.Ask1Price),
		MarkPrice:   parseFloat(t.MarkPrice),
		IndexPrice:  parseFloat(t.IndexPrice),
		FundingRate: parseFloat(t.FundingRate),
		LastPrice:   parseFloat(t.LastPrice),
		Volume24h:   parseFloat(t.Volume24h),
		TsMs:        tsMs,
		FeedHealthy: true,
	}
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// ---------- REST pollers ----------

// FundingRateRecord represents a historical funding rate entry from the REST API.
type FundingRateRecord struct {
	Symbol          string  `json:"symbol"`
	FundingRate     float64 `json:"funding_rate"`
	FundingRateTs   int64   `json:"funding_rate_ts"`
	NextFundingTime int64   `json:"next_funding_time"`
}

// OpenInterestRecord represents an open interest data point from the REST API.
type OpenInterestRecord struct {
	Symbol       string  `json:"symbol"`
	OpenInterest float64 `json:"open_interest"`
	TsMs         int64   `json:"ts_ms"`
}

func (f *BybitFeed) fundingPoller(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(f.cfg.FundingPollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Initial poll.
	f.pollFundingRates(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.pollFundingRates(ctx)
		}
	}
}

func (f *BybitFeed) pollFundingRates(ctx context.Context) {
	for _, sym := range f.cfg.Symbols {
		url := fmt.Sprintf("%s/v5/market/funding/history?category=linear&symbol=%s&limit=1", f.cfg.RestURL, sym)
		body, err := f.httpGet(ctx, url)
		if err != nil {
			log.Printf("marketdata/bybit: funding poll %s: %v", sym, err)
			continue
		}

		var resp struct {
			RetCode int `json:"retCode"`
			Result  struct {
				List []struct {
					Symbol          string `json:"symbol"`
					FundingRate     string `json:"fundingRate"`
					FundingRateTimestamp string `json:"fundingRateTimestamp"`
				} `json:"list"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Printf("marketdata/bybit: funding parse %s: %v", sym, err)
			continue
		}

		if resp.RetCode != 0 || len(resp.Result.List) == 0 {
			continue
		}

		entry := resp.Result.List[0]
		rate := parseFloat(entry.FundingRate)

		f.latestMu.Lock()
		if q, ok := f.latest[sym]; ok {
			q.FundingRate = rate
		}
		f.latestMu.Unlock()
	}
}

func (f *BybitFeed) openInterestPoller(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(f.cfg.OpenInterestPollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Initial poll.
	f.pollOpenInterest(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.pollOpenInterest(ctx)
		}
	}
}

func (f *BybitFeed) pollOpenInterest(ctx context.Context) {
	for _, sym := range f.cfg.Symbols {
		url := fmt.Sprintf("%s/v5/market/open-interest?category=linear&symbol=%s&intervalTime=5min&limit=1", f.cfg.RestURL, sym)
		body, err := f.httpGet(ctx, url)
		if err != nil {
			log.Printf("marketdata/bybit: open interest poll %s: %v", sym, err)
			continue
		}

		var resp struct {
			RetCode int `json:"retCode"`
			Result  struct {
				List []struct {
					Symbol       string `json:"symbol"`
					OpenInterest string `json:"openInterest"`
					Timestamp    string `json:"timestamp"`
				} `json:"list"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Printf("marketdata/bybit: open interest parse %s: %v", sym, err)
			continue
		}

		if resp.RetCode != 0 || len(resp.Result.List) == 0 {
			continue
		}

		// Open interest is informational; we log it but don't push it into
		// the quote channel since Quote has no OI field. Extensions can
		// read this from a separate channel if needed.
		entry := resp.Result.List[0]
		oi := parseFloat(entry.OpenInterest)
		log.Printf("marketdata/bybit: %s open_interest=%.2f", sym, oi)
	}
}

// httpGet performs a GET request with context and returns the response body.
func (f *BybitFeed) httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

// LatestQuote returns the most recently cached quote for a symbol, or nil.
func (f *BybitFeed) LatestQuote(symbol string) *Quote {
	f.latestMu.RLock()
	defer f.latestMu.RUnlock()
	q, ok := f.latest[symbol]
	if !ok {
		return nil
	}
	copy := *q
	return &copy
}
