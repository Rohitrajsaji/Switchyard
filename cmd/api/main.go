package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"switchyard/internal/cache"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/platform/telemetry"
	grpcapi "switchyard/internal/transport/grpc"
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
	shutdownTracing, err := telemetry.InitTracing(ctx, "switchyard-api")
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flush)
	}()
	tracing := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	metrics := telemetry.NewMetrics("api")
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	metrics.Registry.MustRegister(telemetry.PoolCollector(pool, "api"))
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
	if snapshots != nil {
		metrics.Registry.MustRegister(telemetry.CacheCollector(func() telemetry.CacheStats {
			s := snapshots.Stats()
			return telemetry.CacheStats{MemoryHits: s.MemoryHits, RedisHits: s.RedisHits, SourceReads: s.SourceReads, SourceFailures: s.SourceFailures, StoreFailures: s.StoreFailures,
				MemoryMisses: s.MemoryMisses, StaleHits: s.StaleHits, Coalesced: s.Coalesced, Backpressure: s.Backpressure, Evictions: s.Evictions,
				Regressions: s.Regressions, Expired: s.Expired, Entries: s.Entries, OldestVerificationAge: s.OldestVerificationAge}
		}, "api"))
	}
	stopMetrics, err := metrics.Serve(os.Getenv("METRICS_ADDR"), os.Getenv("ENABLE_PPROF") == "true")
	if err != nil {
		return err
	}
	defer stopMetrics()
	management, err := httpapi.NewManagement(pool, logger, origin, os.Getenv("COOKIE_SECURE") == "true", snapshots)
	if err != nil {
		return err
	}
	management.SetMetrics(metrics)
	var h http.Handler = httpapi.NewObserved(logger, func(ctx context.Context) error { return postgres.Ready(ctx, pool) }, metrics, management.Register)
	var rpcOptions []grpc.ServerOption
	if tracing {
		// Health probes would drown real traces; trace everything else.
		h = otelhttp.NewHandler(h, "http.server", otelhttp.WithFilter(func(r *http.Request) bool { return !strings.HasPrefix(r.URL.Path, "/health/") }))
		rpcOptions = append(rpcOptions, grpc.StatsHandler(otelgrpc.NewServerHandler()))
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	rpcListener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	defer rpcListener.Close()
	rpcServer := grpcapi.NewObservedServer(management.EvaluationService(), metrics, rpcOptions...)
	defer rpcServer.Stop()
	done := make(chan error, 1)
	rpcDone := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	go func() { rpcDone <- rpcServer.Serve(rpcListener) }()
	logger.Info("api listening", "address", cfg.HTTPAddr, "grpc_address", cfg.GRPCAddr)
	var serveErr error
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case serveErr = <-rpcDone:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rpcStopped := make(chan struct{})
	go func() { rpcServer.GracefulStop(); close(rpcStopped) }()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	select {
	case <-rpcStopped:
	case <-shutdownCtx.Done():
		rpcServer.Stop()
		<-rpcStopped
		shutdownErr = errors.Join(shutdownErr, shutdownCtx.Err())
	}
	logger.Info("api shutdown complete")
	return errors.Join(serveErr, shutdownErr)
}
