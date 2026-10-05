// check-metrics verifies current retained raw facts against materialized counts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/metrics"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT r.project_id,r.id,
        (SELECT u.id FROM users u JOIN project_memberships m ON m.user_id=u.id WHERE m.project_id=r.project_id AND u.active
         ORDER BY (u.role='admin') DESC,u.id LIMIT 1) FROM experiment_runs r ORDER BY r.project_id,r.id`)
	if err != nil {
		return err
	}
	type scope struct {
		project, run string
		actor        *string
	}
	scopes := make([]scope, 0)
	for rows.Next() {
		var r scope
		if err = rows.Scan(&r.project, &r.run, &r.actor); err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Reads only: source mutations and worker scheduling remain separate.
	if len(scopes) == 0 {
		return errors.New("aggregate parity requires at least one experiment fixture")
	}
	service := metrics.New(pool, time.Now)
	for _, r := range scopes {
		if r.actor == nil {
			return errors.New("parity scope has no active human member")
		}
		equal, err := service.Compare(ctx, auth.Actor{ID: *r.actor}, r.project, r.run)
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("aggregate parity mismatch for run %s", r.run)
		}
	}
	fmt.Printf("Aggregate parity passed for %d runs: cohorts, conversions, requests, histograms and quality\n", len(scopes))
	return nil
}
