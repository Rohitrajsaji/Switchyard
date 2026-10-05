package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"switchyard/internal/auth"
	"switchyard/internal/experiments"
)

// Compare checks all aggregate dimensions against raw SQL in one repeatable-read
// snapshot. It is a local operations gate, not a public results endpoint.
func (s *Service) Compare(ctx context.Context, actor auth.Actor, project, run string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	if err = auth.Authorize(ctx, tx, actor, project, "", false); err != nil {
		return false, err
	}
	var env string
	err = tx.QueryRow(ctx, `SELECT environment_id FROM experiment_runs WHERE project_id=$1 AND id=$2`, project, run).Scan(&env)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, experiments.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	var rawBody, aggregateBody []byte
	if err = tx.QueryRow(ctx, attributionSQL+resultsSQL, project, env, run, s.now()).Scan(&rawBody); err != nil {
		return false, err
	}
	if err = tx.QueryRow(ctx, materializedSQL, project, env, run).Scan(&aggregateBody); err != nil {
		return false, err
	}
	var raw, aggregate derived
	if json.Unmarshal(rawBody, &raw) != nil || json.Unmarshal(aggregateBody, &aggregate) != nil {
		return false, errors.New("invalid metric parity representation")
	}
	a, b := contributionCounts(raw), contributionCounts(aggregate)
	// An old dimension can remain as a zero counter after corrective subtraction.
	for k, n := range a {
		if n == 0 {
			delete(a, k)
		}
	}
	for k, n := range b {
		if n == 0 {
			delete(b, k)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return reflect.DeepEqual(a, b), nil
}
