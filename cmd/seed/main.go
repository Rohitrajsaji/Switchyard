// Seed is an explicitly enabled local-only bootstrap; normal API startup never creates accounts.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(); err != nil {
		logger.Error("seed failed", "error", err)
		os.Exit(1)
	}
	logger.Info("demo users available; password supplied through SWITCHYARD_DEMO_PASSWORD")
}
func run() error {
	if os.Getenv("SWITCHYARD_SEED_DEMO") != "true" {
		return errors.New("set SWITCHYARD_SEED_DEMO=true to explicitly enable demo accounts")
	}
	password := os.Getenv("SWITCHYARD_DEMO_PASSWORD")
	hash, err := auth.PasswordHash(password)
	if err != nil {
		return errors.New("SWITCHYARD_DEMO_PASSWORD must contain 12–72 bytes")
	}
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, user := range []struct{ id, email, role string }{{"demo_admin", "admin@example.test", "admin"}, {"demo_reviewer", "reviewer@example.test", "admin"}, {"demo_developer", "developer@example.test", "developer"}, {"demo_viewer", "viewer@example.test", "viewer"}} {
		result, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, user.id, user.email, hash, user.role)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 1 {
			if err := audit.Record(ctx, tx, audit.Entry{ActorID: "system:demo-seed", Source: "system", Action: "demo_user.created", RequestID: "demo-seed", Reason: "explicit local demo bootstrap"}); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
