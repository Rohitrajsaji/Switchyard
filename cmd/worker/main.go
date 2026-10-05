package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/messaging"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/processing"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	processingEnabled := true
	if value := os.Getenv("WORKER_PROCESSING_ENABLED"); value != "" {
		processingEnabled, err = strconv.ParseBool(value)
		if err != nil {
			return errors.New("WORKER_PROCESSING_ENABLED must be a boolean")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://127.0.0.1:42229"
	}
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	transport, err := messaging.Open(setup, url, messaging.StreamName, messaging.SubjectPrefix, messaging.DefaultLimits())
	cancel()
	if err != nil {
		return err
	}
	defer transport.Close()
	var cacheStore cache.Store
	if cfg.RedisURL != "" {
		r, err := cache.NewRedis(cfg.RedisURL, "switchyard:snapshot:v1:", time.Now)
		if err != nil {
			return err
		}
		defer r.Close()
		cacheStore = r
	}
	var tasks sync.WaitGroup
	defer func() { stop(); tasks.Wait() }()
	if processingEnabled {
		setup, cancel := context.WithTimeout(ctx, 5*time.Second)
		consumer, err := transport.Consumer(setup, "PROCESSOR_V1")
		cancel()
		if err != nil {
			return err
		}
		tasks.Add(2)
		go func() {
			defer tasks.Done()
			consume(ctx, consumer, processing.New(pool), refreshConfiguration(pool, cacheStore), logger)
		}()
		go func() { defer tasks.Done(); reconcile(ctx, pool, logger) }()
	}
	store := outbox.New(pool)
	logger.Info("worker started", "publication_batch_limit", 8, "lease_seconds", 30, "processing_enabled", processingEnabled)
	wake := time.NewTimer(0)
	defer wake.Stop()
	var total outbox.PublicationStats
	report := time.NewTicker(30 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker shutdown complete")
			return nil
		case <-report.C:
			logger.Info("outbox publication statistics", "statistics", total)
		case <-wake.C:
			batchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			stats, err := outbox.PublishBatch(batchCtx, store, transport)
			cancel()
			total.Claimed += stats.Claimed
			total.Published += stats.Published
			total.Failed += stats.Failed
			total.Lost += stats.Lost
			if err != nil && ctx.Err() == nil {
				logger.Warn("outbox publication interrupted", "failure", "database_or_transport_unavailable")
			}
			delay := 250 * time.Millisecond
			if err == nil && stats.Claimed == 8 {
				delay = 0
			}
			wake.Reset(delay)
		}
	}
}
