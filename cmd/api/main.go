package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/postgres"
	httpapi "switchyard/internal/transport/http"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 2 * time.Second}
		_, port, err := net.SplitHostPort(os.Getenv("HTTP_ADDR"))
		if err != nil {
			os.Exit(1)
		}
		response, err := client.Get("http://127.0.0.1:" + port + "/health/ready")
		if err != nil {
			os.Exit(1)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	origin := os.Getenv("SWITCHYARD_ORIGIN")
	if origin == "" {
		origin = "http://localhost:3000"
	}
	var snapshots *cache.Coordinator
	if cfg.CacheEnabled {
		var store cache.Store
		if cfg.RedisURL != "" {
			r, err := cache.NewRedis(cfg.RedisURL, "switchyard:snapshot:v1:", time.Now)
			if err != nil {
				return err
			}
			defer r.Close()
			store = r
		}
		snapshots, err = cache.NewCoordinator(context.Background(), cache.PostgresSource(pool), store, cache.Defaults())
		if err != nil {
			return err
		}
		defer snapshots.Close()
		reportCtx, cancelReport := context.WithCancel(context.Background())
		reportDone := make(chan struct{})
		go func() {
			defer close(reportDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-reportCtx.Done():
					return
				case <-ticker.C:
					logger.Info("cache statistics", "statistics", snapshots.Stats())
				}
			}
		}()
		defer func() { cancelReport(); <-reportDone }()
	}
	management, err := httpapi.NewManagement(pool, logger, origin, os.Getenv("COOKIE_SECURE") == "true", snapshots)
	if err != nil {
		return err
	}
	h := httpapi.New(logger, func(ctx context.Context) error { return postgres.Ready(ctx, pool) }, management.Register)
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("api listening", "address", cfg.HTTPAddr)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("api shutdown complete")
		return nil
	}
}
