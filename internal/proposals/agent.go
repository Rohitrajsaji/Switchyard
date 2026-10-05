package proposals

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"switchyard/internal/auth"
	"switchyard/pkg/evaluation"
)

// MaxAgentIncreaseBP is the largest standalone-rollout increase one agent proposal may carry.
// A human still has to approve the exact diff. Larger jumps stay a human-authored proposal.
const MaxAgentIncreaseBP = 1000

func trafficIncrease(old *evaluation.Definition, next evaluation.Definition) int {
	before := 0
	if old != nil && old.Rollout != nil {
		before = old.Rollout.TrafficBP
	}
	after := 0
	if next.Rollout != nil {
		after = next.Rollout.TrafficBP
	}
	if after <= before {
		return 0
	}
	return after - before
}

// FlagContext is the configuration an agent may copy into a proposal. It omits salts.
type FlagContext struct {
	Key          string            `json:"key"`
	Type         string            `json:"type"`
	Revision     int64             `json:"revision"`
	Killed       bool              `json:"killed"`
	TrafficBP    *int              `json:"traffic_bp"`
	Default      evaluation.Value  `json:"default"`
	Safe         evaluation.Value  `json:"safe"`
	Rules        []evaluation.Rule `json:"rules"`
	RolloutValue *evaluation.Value `json:"rollout_value,omitempty"`
}

// EnvironmentContext is a scoped, secret-free summary. Application keys with context:read
// are the only callers.
type EnvironmentContext struct {
	ProjectID     string        `json:"project_id"`
	EnvironmentID string        `json:"environment_id"`
	Environment   string        `json:"environment"`
	Flags         []FlagContext `json:"flags"`
}

// EnvironmentContext loads the current flag configurations for one environment. Authorization
// reads the database, so a revoked key is refused on the next call.
func (s *Service) EnvironmentContext(ctx context.Context, token, projectID, environmentID string) (EnvironmentContext, error) {
	if environmentID == "" {
		return EnvironmentContext{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnvironmentContext{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := auth.AuthorizeApplication(ctx, tx, token, projectID, environmentID, "context:read"); err != nil {
		return EnvironmentContext{}, err
	}
	var name string
	err = tx.QueryRow(ctx, `SELECT name FROM environments WHERE id=$1 AND project_id=$2`, environmentID, projectID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentContext{}, auth.ErrForbidden
	}
	if err != nil {
		return EnvironmentContext{}, err
	}
	rows, err := tx.Query(ctx, `SELECT r.definition FROM flags f
		JOIN environment_flag_state s ON s.flag_id=f.id AND s.project_id=f.project_id
		JOIN flag_revisions r ON r.flag_id=s.flag_id AND r.environment_id=s.environment_id AND r.revision=s.current_revision
		WHERE f.project_id=$1 AND s.environment_id=$2 ORDER BY f.key LIMIT 100`, projectID, environmentID)
	if err != nil {
		return EnvironmentContext{}, err
	}
	defer rows.Close()
	out := EnvironmentContext{ProjectID: projectID, EnvironmentID: environmentID, Environment: name, Flags: []FlagContext{}}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return EnvironmentContext{}, err
		}
		var d evaluation.Definition
		if err := json.Unmarshal(body, &d); err != nil {
			return EnvironmentContext{}, err
		}
		item := FlagContext{Key: d.Key, Type: d.Type, Revision: d.Revision, Killed: d.Killed, Default: d.Default, Safe: d.Safe, Rules: d.Rules}
		if item.Rules == nil {
			item.Rules = []evaluation.Rule{}
		}
		if d.Rollout != nil {
			bp := d.Rollout.TrafficBP
			item.TrafficBP = &bp
			value := d.Rollout.Value
			item.RolloutValue = &value
		}
		out.Flags = append(out.Flags, item)
	}
	if err := rows.Err(); err != nil {
		return EnvironmentContext{}, err
	}
	return out, tx.Commit(ctx)
}
