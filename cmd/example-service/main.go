package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/flagforge/flagforge/pkg/sdk"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("example stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	endpoint := os.Getenv("FLAGFORGE_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:18080"
	}
	client, err := sdk.NewClient(sdk.Config{Endpoint: endpoint, SDKKey: os.Getenv("FLAGFORGE_SDK_KEY"), BootstrapFlags: map[string]any{"new-checkout": false}, Analytics: true})
	if err != nil {
		return err
	}
	defer client.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		user := sdk.Subject{Key: r.URL.Query().Get("user"), Attributes: map[string]any{"country": r.URL.Query().Get("country"), "plan": r.URL.Query().Get("plan")}}
		value := client.BoolVariation(r.Context(), "new-checkout", user, false)
		checkout := "old"
		if value {
			checkout = "new"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"checkout": checkout, "sdk": client.Status()})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	server := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ch := make(chan error, 1)
	go func() { ch <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-ch:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return server.Shutdown(shutdown)
}
