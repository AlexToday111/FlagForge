// Package sdk synchronizes configuration in the background and evaluates locally.
package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flagforge/flagforge/pkg/evaluation"
	"github.com/flagforge/flagforge/pkg/types"
)

type UpdateMode string

const (
	Streaming UpdateMode = "streaming"
	Polling   UpdateMode = "polling"
)

type Subject = types.Subject

type Config struct {
	SDKKey            string
	Endpoint          string
	Mode              UpdateMode
	Offline           bool
	WaitForInitial    bool
	InitialTimeout    time.Duration
	PollInterval      time.Duration
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	Bootstrap         *types.Snapshot
	// BootstrapFlags is an unversioned set of typed defaults until first sync.
	BootstrapFlags    map[string]any
	HTTPClient        *http.Client
	Analytics         bool
	AnalyticsBuffer   int
	AnalyticsInterval time.Duration
}

type Client struct {
	cfg         Config
	http        *http.Client
	store       Store
	bootstrap   atomic.Pointer[types.Snapshot]
	ready       atomic.Bool
	readyCh     chan struct{}
	readyOnce   sync.Once
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	events      chan types.EvaluationEvent
	dropped     atomic.Uint64
	lastSync    atomic.Int64
	lastError   atomic.Pointer[string]
	propagation atomic.Int64
}

type Status struct {
	Ready              bool          `json:"ready"`
	Version            int64         `json:"version"`
	LastSync           time.Time     `json:"last_sync"`
	LastError          string        `json:"last_error"`
	DroppedEvents      uint64        `json:"dropped_events"`
	PropagationLatency time.Duration `json:"propagation_latency"`
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Mode == "" {
		cfg.Mode = Streaming
	}
	if cfg.Mode != Streaming && cfg.Mode != Polling {
		return nil, fmt.Errorf("unknown update mode")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.InitialTimeout <= 0 {
		cfg.InitialTimeout = 10 * time.Second
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 45 * time.Second
	}
	if cfg.AnalyticsBuffer <= 0 {
		cfg.AnalyticsBuffer = 1000
	}
	if cfg.AnalyticsInterval <= 0 {
		cfg.AnalyticsInterval = 5 * time.Second
	}
	if !cfg.Offline {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || cfg.SDKKey == "" {
			return nil, fmt.Errorf("valid endpoint and SDK key required")
		}
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{cfg: cfg, http: cfg.HTTPClient, cancel: cancel, readyCh: make(chan struct{}), events: make(chan types.EvaluationEvent, cfg.AnalyticsBuffer)}
	if c.http == nil {
		c.http = &http.Client{Transport: http.DefaultTransport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		// Do not allow credentials to follow redirects, including custom clients.
		copy := *c.http
		copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
		c.http = &copy
	}
	if cfg.Bootstrap != nil {
		if _, err := c.store.Replace(*cfg.Bootstrap); err != nil {
			cancel()
			return nil, err
		}
	}
	if len(cfg.BootstrapFlags) > 0 {
		s := types.Snapshot{Flags: map[string]types.FlagConfig{}, Segments: map[string]types.Segment{}}
		for k, v := range cfg.BootstrapFlags {
			kind := types.JSON
			switch v.(type) {
			case bool:
				kind = types.Boolean
			case string:
				kind = types.String
			case int, int64:
				kind = types.Integer
			case float64:
				kind = types.Float
			}
			s.Flags[k] = types.FlagConfig{Key: k, Type: kind, Enabled: true, Variations: map[string]any{"default": v}, DefaultVariation: "default", OffVariation: "default"}
		}
		var bootstrap Store
		if _, err := bootstrap.Replace(s); err != nil {
			cancel()
			return nil, err
		}
		c.bootstrap.Store(bootstrap.current.Load())
	}
	if cfg.Offline {
		if cfg.Bootstrap != nil || len(cfg.BootstrapFlags) > 0 {
			c.markReady()
		}
		return c, nil
	}
	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.poll(ctx) }()
	if cfg.Mode == Streaming {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.streamLoop(ctx) }()
	}
	if cfg.Analytics {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.analytics(ctx) }()
	}
	if cfg.WaitForInitial {
		timer := time.NewTimer(cfg.InitialTimeout)
		defer timer.Stop()
		select {
		case <-c.readyCh:
		case <-timer.C:
			_ = c.Close()
			return nil, fmt.Errorf("initial configuration timeout: %s", c.Status().LastError)
		}
	}
	return c, nil
}

func (c *Client) markReady()   { c.ready.Store(true); c.readyOnce.Do(func() { close(c.readyCh) }) }
func (c *Client) Ready() bool  { return c.ready.Load() }
func (c *Client) Close() error { c.cancel(); c.wg.Wait(); return nil }
func (c *Client) Status() Status {
	s := Status{Ready: c.Ready(), Version: c.store.Version(), DroppedEvents: c.dropped.Load(), PropagationLatency: time.Duration(c.propagation.Load())}
	if n := c.lastSync.Load(); n > 0 {
		s.LastSync = time.Unix(0, n)
	}
	if p := c.lastError.Load(); p != nil {
		s.LastError = *p
	}
	return s
}
func (c *Client) setError(err error) {
	if err == nil {
		c.lastError.Store(nil)
	} else {
		message := err.Error()
		c.lastError.Store(&message)
	}
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.cfg.Endpoint+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.cfg.SDKKey)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r, nil
}

func (c *Client) syncConfig(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	r, err := c.request(ctx, http.MethodGet, "/sdk/v1/config", nil)
	if err != nil {
		return err
	}
	if version := c.store.Version(); version >= 0 {
		r.Header.Set("If-None-Match", fmt.Sprintf(`"%d"`, version))
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("snapshot request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		c.lastSync.Store(time.Now().UnixNano())
		c.markReady()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("snapshot status %d", resp.StatusCode)
	}
	var s types.Snapshot
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&s); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if _, err = c.store.Replace(s); err != nil {
		return err
	}
	c.lastSync.Store(time.Now().UnixNano())
	c.markReady()
	return nil
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (c *Client) poll(ctx context.Context) {
	for {
		c.setError(c.syncConfig(ctx))
		if !wait(ctx, c.cfg.PollInterval) {
			return
		}
	}
}

func (c *Client) streamLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.stream(ctx)
		c.setError(err)
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		if !wait(ctx, backoff/2+time.Duration(rand.Int64N(int64(backoff/2)))) {
			return
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (c *Client) stream(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r, err := c.request(ctx, http.MethodGet, "/sdk/v1/stream", nil)
	if err != nil {
		return err
	}
	// An idle timer bounds stalled handshakes and half-open streams.
	timer := time.AfterFunc(c.cfg.StreamIdleTimeout, cancel)
	defer timer.Stop()
	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("stream disconnected")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream status %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("invalid stream content type")
	}
	// Subscription is established before resync, closing the snapshot/subscribe gap.
	if err = c.syncConfig(ctx); err != nil {
		return err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		timer.Reset(c.cfg.StreamIdleTimeout)
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e types.Event
		if err = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e); err != nil {
			continue
		}
		if e.Version > c.store.Version() {
			if err = c.syncConfig(ctx); err != nil {
				return err
			}
			if !e.CreatedAt.IsZero() && c.store.Version() >= e.Version {
				d := time.Since(e.CreatedAt)
				if d >= 0 {
					c.propagation.Store(int64(d))
				}
			}
		}
	}
	if err = scanner.Err(); err != nil {
		return fmt.Errorf("stream read interrupted")
	}
	return io.EOF
}

func cloneValue(v any) any {
	switch v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		var owned any
		_ = json.Unmarshal(b, &owned)
		return owned
	}
	return v
}

func (c *Client) evaluate(ctx context.Context, key string, s Subject, fallback any, kind types.FlagType) types.EvaluationResult {
	snapshot := c.store.current.Load()
	if snapshot == nil {
		snapshot = c.bootstrap.Load()
	}
	result := types.EvaluationResult{Value: fallback, Reason: "ERROR"}
	if snapshot != nil {
		if flag, ok := snapshot.Flags[key]; ok && (kind == "" || flag.Type == kind) {
			result = evaluation.Evaluate(ctx, snapshot, key, s, fallback)
		}
	}
	if c.cfg.Analytics && !c.cfg.Offline {
		select {
		case c.events <- types.EvaluationEvent{Flag: key, Variation: result.Variation, Reason: result.Reason, Timestamp: time.Now().UTC()}:
		default:
			c.dropped.Add(1)
		}
	}
	return result
}

func (c *Client) Evaluate(ctx context.Context, key string, s Subject, fallback any) types.EvaluationResult {
	r := c.evaluate(ctx, key, s, fallback, "")
	r.Value = cloneValue(r.Value)
	return r
}
func (c *Client) BoolVariation(ctx context.Context, key string, s Subject, fallback bool) bool {
	v, ok := c.evaluate(ctx, key, s, fallback, types.Boolean).Value.(bool)
	if !ok {
		return fallback
	}
	return v
}
func (c *Client) StringVariation(ctx context.Context, key string, s Subject, fallback string) string {
	v, ok := c.evaluate(ctx, key, s, fallback, types.String).Value.(string)
	if !ok {
		return fallback
	}
	return v
}
func (c *Client) IntVariation(ctx context.Context, key string, s Subject, fallback int) int {
	result := c.evaluate(ctx, key, s, fallback, types.Integer)
	if result.Reason == "ERROR" {
		return fallback
	}
	v, ok := evaluation.Number(result.Value)
	limit := float64(uint(^uint(0)>>1)) + 1
	if !ok || v >= limit || v < -limit {
		return fallback
	}
	return int(v)
}
func (c *Client) FloatVariation(ctx context.Context, key string, s Subject, fallback float64) float64 {
	v, ok := evaluation.Number(c.evaluate(ctx, key, s, fallback, types.Float).Value)
	if !ok {
		return fallback
	}
	return v
}
func (c *Client) JSONVariation(ctx context.Context, key string, s Subject, fallback any) any {
	return cloneValue(c.evaluate(ctx, key, s, fallback, types.JSON).Value)
}

func (c *Client) analytics(ctx context.Context) {
	flush := func(batch []types.EvaluationEvent) {
		if len(batch) == 0 {
			return
		}
		b, _ := json.Marshal(map[string]any{"events": batch})
		requestCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer cancel()
		r, err := c.request(requestCtx, http.MethodPost, "/sdk/v1/events/batch", bytes.NewReader(b))
		if err != nil {
			c.dropped.Add(uint64(len(batch)))
			return
		}
		resp, err := c.http.Do(r)
		if err != nil {
			c.dropped.Add(uint64(len(batch)))
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			c.dropped.Add(uint64(len(batch)))
		}
	}
	ticker := time.NewTicker(c.cfg.AnalyticsInterval)
	defer ticker.Stop()
	batch := make([]types.EvaluationEvent, 0, 100)
	for {
		select {
		case <-ctx.Done():
			c.dropped.Add(uint64(len(batch) + len(c.events)))
			return
		case e := <-c.events:
			batch = append(batch, e)
			if len(batch) == 100 {
				flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			flush(batch)
			batch = batch[:0]
		}
	}
}
