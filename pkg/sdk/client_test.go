package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flagforge/flagforge/pkg/types"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSnapshot(version int64, on bool) types.Snapshot {
	return types.Snapshot{Project: "shop", Environment: "production", Version: version, Flags: map[string]types.FlagConfig{"checkout": {Key: "checkout", Type: types.Boolean, Enabled: on, Variations: map[string]any{"on": true, "off": false}, DefaultVariation: "on", OffVariation: "off", Version: version}}, Segments: map[string]types.Segment{}}
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestStoreOwnershipAndVersions(t *testing.T) {
	var store Store
	s := testSnapshot(2, true)
	if ok, err := store.Replace(s); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Flags["checkout"].Variations["on"] = false
	owned := store.Snapshot()
	owned.Flags["checkout"].Variations["on"] = false
	if v := store.current.Load().Flags["checkout"].Variations["on"]; v != true {
		t.Fatal("mutable snapshot escaped")
	}
	if ok, err := store.Replace(testSnapshot(1, false)); ok || err != nil {
		t.Fatal(ok, err)
	}
	if store.Version() != 2 {
		t.Fatal(store.Version())
	}
}

func TestConcurrentReplaceAndEvaluate(t *testing.T) {
	client, err := NewClient(Config{Offline: true, Bootstrap: ptr(testSnapshot(1, true))})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10000; n++ {
				client.BoolVariation(context.Background(), "checkout", Subject{Key: "u"}, false)
			}
		}()
	}
	for n := int64(2); n < 100; n++ {
		if _, err = client.store.Replace(testSnapshot(n, n%2 == 0)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
func ptr(s types.Snapshot) *types.Snapshot { return &s }

func TestStreamingRecoveryAndOffline(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	var available atomic.Bool
	available.Store(true)
	var requests atomic.Int64
	var connected atomic.Int64
	updates := make(chan struct{}, 10)
	disconnect := make(chan struct{}, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer sdk_test" {
			w.WriteHeader(401)
			return
		}
		if !available.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/sdk/v1/config":
			_ = json.NewEncoder(w).Encode(testSnapshot(version.Load(), version.Load()%2 == 0))
		case "/sdk/v1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, ": connected\n\n")
			w.(http.Flusher).Flush()
			connected.Add(1)
			for {
				select {
				case <-r.Context().Done():
					return
				case <-disconnect:
					return
				case <-updates:
					_, _ = fmt.Fprintf(w, "data: {\"version\":%d}\n\n", version.Load())
					w.(http.Flusher).Flush()
				}
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, SDKKey: "sdk_test", WaitForInitial: true, PollInterval: time.Hour, StreamIdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	eventually(t, func() bool { return connected.Load() > 0 })
	version.Store(2)
	updates <- struct{}{}
	eventually(t, func() bool { return client.Status().Version == 2 })
	if !client.BoolVariation(context.Background(), "checkout", Subject{Key: "u"}, false) {
		t.Fatal("stream update not evaluated")
	}
	available.Store(false)
	disconnect <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	before := requests.Load()
	for i := 0; i < 10000; i++ {
		if !client.BoolVariation(context.Background(), "checkout", Subject{Key: "u"}, false) {
			t.Fatal("offline cache lost")
		}
	}
	if requests.Load()-before > 10 {
		t.Fatal("evaluation made network requests")
	}
	version.Store(3)
	available.Store(true)
	eventually(t, func() bool { return client.Status().Version == 3 })
	if client.BoolVariation(context.Background(), "checkout", Subject{Key: "u"}, true) {
		t.Fatal("recovery failed")
	}
	eventually(t, func() bool { return connected.Load() >= 2 })
}

func TestBootstrapTypesAndFallback(t *testing.T) {
	client, err := NewClient(Config{Offline: true, BootstrapFlags: map[string]any{"bool": true, "string": "hello", "int": 42, "float": 1.5, "json": map[string]any{"value": true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	if !client.Ready() || !client.BoolVariation(ctx, "bool", Subject{}, false) || client.StringVariation(ctx, "string", Subject{}, "") != "hello" || client.IntVariation(ctx, "int", Subject{}, 0) != 42 || client.FloatVariation(ctx, "float", Subject{}, 0) != 1.5 {
		t.Fatal("typed bootstrap failed")
	}
	if client.BoolVariation(ctx, "string", Subject{}, false) {
		t.Fatal("type mismatch")
	}
	owned := client.JSONVariation(ctx, "json", Subject{}, nil).(map[string]any)
	owned["value"] = false
	if client.JSONVariation(ctx, "json", Subject{}, nil).(map[string]any)["value"] != true {
		t.Fatal("JSON ownership lost")
	}
	if !client.BoolVariation(ctx, "missing", Subject{}, true) {
		t.Fatal("fallback lost")
	}
	maxInt := int(^uint(0) >> 1)
	if client.IntVariation(ctx, "missing", Subject{}, maxInt) != maxInt {
		t.Fatal("integer fallback rounded")
	}
}

func TestInitializationTimeoutAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, SDKKey: "key", WaitForInitial: true, InitialTimeout: 20 * time.Millisecond})
	if err == nil || client != nil {
		t.Fatal("expected timeout")
	}
	client, err = NewClient(Config{Endpoint: server.URL, SDKKey: "key", BootstrapFlags: map[string]any{"bool": true}})
	if err != nil {
		t.Fatal(err)
	}
	if client.Ready() {
		t.Fatal("bootstrap must not claim remote readiness")
	}
	_ = client.Close()
	_ = client.Close()
}

func TestPollingETag(t *testing.T) {
	var got atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"1"` {
			got.Store(true)
			w.WriteHeader(304)
			return
		}
		_ = json.NewEncoder(w).Encode(testSnapshot(1, true))
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, SDKKey: "key", Mode: Polling, PollInterval: 10 * time.Millisecond, WaitForInitial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	eventually(t, got.Load)
}

func TestAnalyticsBounded(t *testing.T) {
	client, err := NewClient(Config{Offline: true, Bootstrap: ptr(testSnapshot(1, true))})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.cfg.Offline = false
	client.cfg.Analytics = true
	for i := 0; i < 10000; i++ {
		client.BoolVariation(context.Background(), "checkout", Subject{}, false)
	}
	if client.dropped.Load() == 0 || len(client.events) > cap(client.events) {
		t.Fatal("unbounded analytics")
	}
}

func BenchmarkLocalBool(b *testing.B) {
	client, _ := NewClient(Config{Offline: true, Bootstrap: ptr(testSnapshot(1, true))})
	defer client.Close()
	ctx := context.Background()
	subject := Subject{Key: "u"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		client.BoolVariation(ctx, "checkout", subject, false)
	}
}
