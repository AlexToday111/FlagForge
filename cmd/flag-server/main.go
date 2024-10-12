package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/flagforge/flagforge/internal/api"
	"github.com/flagforge/flagforge/internal/config"
	"github.com/flagforge/flagforge/internal/eventbus"
	"github.com/flagforge/flagforge/internal/observability"
	"github.com/flagforge/flagforge/internal/repository"
	workers "github.com/flagforge/flagforge/internal/runtime"
	"github.com/flagforge/flagforge/internal/streaming"
	"github.com/flagforge/flagforge/migrations"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "YAML configuration file")
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	startup, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err = migrations.Up(startup, cfg.Database.DSN); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if *migrateOnly {
		return nil
	}
	r, err := repository.New(startup, cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer r.Pool.Close()
	if err = r.BootstrapAdmin(startup, cfg.Server.AdminToken); err != nil {
		return err
	}
	traceShutdown, err := observability.Tracing(startup)
	if err != nil {
		return err
	}
	defer func() {
		flush, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = traceShutdown(flush)
	}()
	bus := eventbus.New(cfg.Redis.Address, cfg.Redis.Password)
	defer bus.Client.Close()
	hub := streaming.New()
	metrics := observability.New()
	join := workers.Start(ctx, r, bus, hub, metrics, cfg.Outbox.PollInterval, cfg.Outbox.BatchSize)
	app := &api.Server{Repo: r, Bus: bus, Hub: hub, Metrics: metrics, Context: ctx, Heartbeat: cfg.Streaming.HeartbeatInterval, CacheTTL: cfg.SDK.SnapshotCacheTTL}
	server := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Server.Port), Handler: app.Handler(cfg.Server.Pprof), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: nil}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.ListenAndServe() }()
	slog.Info("flagforge listening", "port", cfg.Server.Port)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			cancel()
			join()
			return err
		}
	}
	cancel()
	shutdown, stopShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopShutdown()
	err = server.Shutdown(shutdown)
	if err != nil {
		_ = server.Close()
	}
	join()
	// Best effort final publication; unprocessed durable rows remain for next startup.
	_, _ = r.PublishOutbox(shutdown, bus, cfg.Outbox.BatchSize)
	return err
}
