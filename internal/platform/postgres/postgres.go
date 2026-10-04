package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/migrations"
)

func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	c, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	c.MaxConns = maxConns
	c.MinConns = 0
	c.MaxConnLifetime = 30 * time.Minute
	c.MaxConnIdleTime = 5 * time.Minute
	c.ConnConfig.ConnectTimeout = 3 * time.Second
	return pgxpool.NewWithConfig(ctx, c)
}

func Ready(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM service_metadata WHERE key='service' AND value='switchyard') AND (SELECT max(version) FROM schema_migrations)=$1`, migrations.Latest()).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("foundation migration missing")
	}
	return nil
}
