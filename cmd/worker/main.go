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
	"switchyard/internal/platform/telemetry"
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
	rolloutsEnabled := true
	if value := os.Getenv("WORKER_ROLLOUTS_ENABLED"); value != "" {
		rolloutsEnabled, err = strconv.ParseBool(value)
		if err != nil {
			return errors.New("WORKER_ROLLOUTS_ENABLED must be a boolean")
		}
	}
	if cfg.WorkerRetentionEnabled && !processingEnabled {
		return errors.New("retention requires normal worker processing")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.InitTracing(ctx, "switchyard-worker")
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flush)
	}()
	metrics := telemetry.NewMetrics("worker")
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	metrics.Registry.MustRegister(telemetry.PoolCollector(pool, "worker"), telemetry.StateCollector(pool, "worker"))
	stopMetrics, err := metrics.Serve(os.Getenv("METRICS_ADDR"), os.Getenv("ENABLE_PPROF") == "true")
	if err != nil {
		return err
	}
	defer stopMetrics()
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
		metrics.Registry.MustRegister(telemetry.ConsumerCollector(func(ctx context.Context) (uint64, uint64, uint64, error) {
			info, err := consumer.Info(ctx)
			if err != nil {
				return 0, 0, 0, err
			}
			return info.NumPending, uint64(info.NumAckPending), uint64(info.NumRedelivered), nil
		}, "worker"))
		tasks.Add(2)
		go func() {
			defer tasks.Done()
			consume(ctx, consumer, processing.New(pool), refreshConfiguration(pool, cacheStore), logger, metrics)
		}()
		go func() { defer tasks.Done(); reconcile(ctx, pool, logger) }()
	}
	if cfg.WorkerRetentionEnabled {
		tasks.Add(1)
		go func() { defer tasks.Done(); retain(ctx, pool, cfg.RawRetentionDays, cfg.SummaryRetentionDays, logger) }()
	}
	if rolloutsEnabled {
		tasks.Add(1)
		go func() { defer tasks.Done(); progressRollouts(ctx, pool, logger, metrics) }()
	}
	store := outbox.New(pool)
	logger.Info("worker started", "rollouts_enabled", rolloutsEnabled, "publication_batch_limit", 8, "lease_seconds", 30, "processing_enabled", processingEnabled, "retention_enabled", cfg.WorkerRetentionEnabled, "raw_retention_days", cfg.RawRetentionDays, "summary_retention_days", cfg.SummaryRetentionDays)
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
			metrics.ObservePublication(stats.Published, stats.Failed, stats.Lost)
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
