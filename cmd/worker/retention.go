package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/events"
	"switchyard/internal/metrics"
	"switchyard/internal/processing"
)

type retentionStats struct {
	ExpiredUsers, ExpiredSegments, ExpiredPending        int64
	FoldedRaw, FoldedPending, PreservedPending           int64
	Identities, Publications, Receipts, ResolvedFailures int64
}

func (s *retentionStats) add(v retentionStats) {
	s.ExpiredUsers += v.ExpiredUsers
	s.ExpiredSegments += v.ExpiredSegments
	s.ExpiredPending += v.ExpiredPending
	s.FoldedRaw += v.FoldedRaw
	s.FoldedPending += v.FoldedPending
	s.PreservedPending += v.PreservedPending
	s.Identities += v.Identities
	s.Publications += v.Publications
	s.Receipts += v.Receipts
	s.ResolvedFailures += v.ResolvedFailures
}

func (s retentionStats) worked() bool {
	return s.ExpiredUsers+s.ExpiredSegments+s.ExpiredPending+s.FoldedRaw+s.FoldedPending+s.Identities+s.Publications+s.Receipts+s.ResolvedFailures > 0
}

// Each step commits independently; interruption preserves already completed
// work and retries the rest. One fixed loop, bounded transactions, no fan-out.
func retentionStep(ctx context.Context, pool *pgxpool.Pool, now time.Time, rawDays, summaryDays int) (retentionStats, error) {
	var stats retentionStats
	expired, err := metrics.ExpireOne(ctx, pool, now, summaryDays)
	if err != nil {
		return stats, err
	}
	if expired.User {
		stats.ExpiredUsers = 1
	}
	stats.ExpiredSegments = expired.Segments
	stats.ExpiredPending = expired.Pending
	folded, err := metrics.FoldWithRetention(ctx, pool, now, rawDays)
	if err != nil {
		return stats, err
	}
	stats.FoldedRaw = int64(folded.Raw)
	stats.FoldedPending = int64(folded.ResolvedPending)
	stats.PreservedPending = int64(folded.PreservedPending)
	stats.Identities, err = events.PruneIdentities(ctx, pool, now, 100)
	if err != nil {
		return stats, err
	}
	completed, err := processing.PruneCompleted(ctx, pool, now, 100)
	if err != nil {
		return stats, err
	}
	stats.Publications = completed.Publications
	stats.Receipts = completed.Receipts
	stats.ResolvedFailures = completed.ResolvedFailures
	return stats, nil
}

func retain(ctx context.Context, pool *pgxpool.Pool, rawDays, summaryDays int, logger *slog.Logger) {
	nextReport := time.Now()
	var total retentionStats
	var failures int64
	defer func() { logger.Info("retention shutdown statistics", "statistics", total, "failed_cycles", failures) }()
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		stats, err := retentionStep(call, pool, time.Now(), rawDays, summaryDays)
		cancel()
		total.add(stats)
		if err != nil {
			failures++
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("retention interrupted", "failure", "database_or_retention_validation_failed")
		}
		if time.Now().After(nextReport) {
			logger.Info("retention statistics", "statistics", total, "failed_cycles", failures)
			nextReport = time.Now().Add(30 * time.Second)
		}
		delay := time.Second
		if err == nil && stats.worked() {
			delay = 10 * time.Millisecond
		}
		if !pause(ctx, delay) {
			return
		}
	}
}
