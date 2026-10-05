package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"switchyard/internal/cache"
	"switchyard/internal/metrics"
	"switchyard/internal/outbox"
	"switchyard/internal/processing"
	"switchyard/pkg/snapshot"
)

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func refreshConfiguration(pool *pgxpool.Pool, store cache.Store) func(context.Context, outbox.Reference) error {
	if store == nil {
		return nil
	}
	source := cache.PostgresSource(pool)
	return func(ctx context.Context, r outbox.Reference) error {
		proof := time.Now()
		var key string
		if err := pool.QueryRow(ctx, `SELECT key FROM flags WHERE project_id=$1 AND id=$2`, r.ProjectID, r.ObjectID).Scan(&key); err != nil {
			return err
		}
		d, err := source(ctx, snapshot.Key{ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, FlagKey: key})
		if err != nil {
			return err
		}
		value, err := snapshot.New(d, proof)
		if err != nil {
			return err
		}
		_, err = store.Put(ctx, value)
		return err
	}
}
func consume(ctx context.Context, consumer jetstream.Consumer, store *processing.Store, refresh func(context.Context, outbox.Reference) error, logger *slog.Logger) {
	var received, duplicates, dead, failed, cacheFailures uint64
	nextReport := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		// Four sequential messages fit the thirty-second ack deadline even
		// when each database/cache/ack operation reaches its bounded timeout.
		batch, err := consumer.Fetch(4, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("work fetch interrupted", "failure", "transport_unavailable")
			}
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		for m := range batch.Messages() {
			if ctx.Err() != nil {
				return
			}
			received++
			outcome, err := processing.HandleWithRefresh(ctx, store, m, refresh)
			if outcome.Duplicate {
				duplicates++
			}
			if outcome.Dead {
				dead++
			}
			if outcome.CacheFailure {
				cacheFailures++
			}
			if err != nil {
				failed++
				if ctx.Err() != nil {
					return
				}
				// A failed DB commit or lost confirmed ack remains delivery
				// work. Nak changes no PostgreSQL processing/metric state.
				if err := m.NakWithDelay(time.Second); err != nil {
					logger.Warn("work retry signal interrupted", "failure", "transport_unavailable")
				}
			}
		}
		if err := batch.Error(); err != nil && !errors.Is(err, nats.ErrTimeout) && ctx.Err() == nil {
			logger.Warn("work batch interrupted", "failure", "transport_unavailable")
		}
		if time.Now().After(nextReport) {
			logger.Info("work consumption statistics", "received", received, "duplicates", duplicates, "dead", dead, "failed", failed, "cache_failures", cacheFailures)
			nextReport = time.Now().Add(30 * time.Second)
		}
	}
}
func reconcile(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	var applied, failed uint64
	nextReport := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		worked, err := metrics.ReconcileOne(call, pool, time.Now())
		cancel()
		if err != nil {
			failed++
		} else if worked {
			applied++
		}
		if time.Now().After(nextReport) {
			logger.Info("metric reconciliation statistics", "applied", applied, "failed", failed)
			nextReport = time.Now().Add(30 * time.Second)
		}
		if err != nil || !worked {
			if !pause(ctx, 250*time.Millisecond) {
				return
			}
		}
	}
}
