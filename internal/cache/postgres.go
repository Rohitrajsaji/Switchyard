package cache

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/flags"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

// PostgresSource loads configuration, not authorization. Transport callers
// authenticate and authorize independently before asking the coordinator.
func PostgresSource(pool *pgxpool.Pool) Source {
	service := flags.New(pool)
	return func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		d, err := service.Get(ctx, k.ProjectID, k.EnvironmentID, k.FlagKey)
		if errors.Is(err, flags.ErrNotFound) {
			err = ErrNotFound
		}
		return d, err
	}
}
