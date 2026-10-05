// Package applicationeval owns application evaluation independently of transport.
package applicationeval

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/flags"
	"switchyard/internal/platform/identity"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

const MaxBatch = 100

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Input struct {
	ProjectID     string                     `json:"project_id"`
	EnvironmentID string                     `json:"environment_id"`
	Key           string                     `json:"key"`
	UserID        string                     `json:"user_id"`
	Attributes    map[string]json.RawMessage `json:"attributes"`
	Fallback      evaluation.Value           `json:"fallback"`
}
type Response struct {
	evaluation.Result
	DecisionID  string `json:"decision_id"`
	Unavailable bool   `json:"-"`
}

// Observer receives the reason of every served application decision (bounded vocabulary).
type Observer interface{ ObserveEvaluation(reason string) }
type Service struct {
	pool      *pgxpool.Pool
	auth      *auth.Service
	snapshots *cache.Coordinator
	observer  Observer
}

// SetObserver must be called before the service handles traffic.
func (s *Service) SetObserver(o Observer) { s.observer = o }

func New(pool *pgxpool.Pool, a *auth.Service, snapshots *cache.Coordinator) *Service {
	return &Service{pool: pool, auth: a, snapshots: snapshots}
}
func validScope(project, env, key string) bool {
	return project != "" && len(project) <= 128 && env != "" && len(env) <= 128 && keyPattern.MatchString(key)
}
func validate(in Input) error {
	if !validScope(in.ProjectID, in.EnvironmentID, in.Key) || evaluation.ValidateContext(in.UserID, in.Attributes) != nil || in.Fallback.Validate(in.Fallback.Type) != nil {
		return auth.ErrInvalid
	}
	return nil
}
func (s *Service) Evaluate(ctx context.Context, token string, in Input) (Response, error) {
	if _, err := s.auth.AuthenticateApplication(ctx, token, in.ProjectID, in.EnvironmentID, "evaluate"); err != nil {
		return Response{}, err
	}
	if err := validate(in); err != nil {
		return Response{}, err
	}
	return s.evaluate(ctx, in)
}

// Batch returns ordered decisions for one scope. All inputs are validated before
// evaluation, and authorization is checked once before exposing any decision.
func (s *Service) Batch(ctx context.Context, token string, inputs []Input) ([]Response, error) {
	if len(inputs) < 1 || len(inputs) > MaxBatch {
		return nil, auth.ErrInvalid
	}
	first := inputs[0]
	if _, err := s.auth.AuthenticateApplication(ctx, token, first.ProjectID, first.EnvironmentID, "evaluate"); err != nil {
		return nil, err
	}
	for _, in := range inputs {
		if in.ProjectID != first.ProjectID || in.EnvironmentID != first.EnvironmentID || validate(in) != nil {
			return nil, auth.ErrInvalid
		}
	}
	responses := make([]Response, 0, len(inputs))
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := s.evaluate(ctx, in)
		// Unavailability carries a valid safe fallback; transport adapters preserve it.
		if err != nil && !errors.Is(err, cache.ErrUnavailable) {
			return nil, err
		}
		responses = append(responses, result)
	}
	return responses, nil
}
func (s *Service) evaluate(ctx context.Context, in Input) (Response, error) {
	response, err := s.decide(ctx, in)
	if s.observer != nil && response.Reason != "" {
		s.observer.ObserveEvaluation(response.Reason)
	}
	return response, err
}
func (s *Service) decide(ctx context.Context, in Input) (Response, error) {
	if s.snapshots != nil {
		result, err := s.snapshots.Evaluate(ctx, snapshot.Key{ProjectID: in.ProjectID, EnvironmentID: in.EnvironmentID, FlagKey: in.Key}, in.UserID, in.Attributes, in.Fallback)
		if errors.Is(err, cache.ErrInvalidFallback) || errors.Is(err, snapshot.ErrInvalid) {
			return Response{}, auth.ErrInvalid
		}
		response := Response{Result: result, DecisionID: identity.New("dec_")}
		if err != nil {
			response.Unavailable = true
			return response, cache.ErrUnavailable
		}
		return response, nil
	}
	return s.Preview(ctx, in)
}

// Preview requires the caller to authorize a human first. It always reads current
// PostgreSQL state, so a management preview never turns into a cached proof.
func (s *Service) Preview(ctx context.Context, in Input) (Response, error) {
	if err := validate(in); err != nil {
		return Response{}, err
	}
	d, err := flags.New(s.pool).Get(ctx, in.ProjectID, in.EnvironmentID, in.Key)
	if errors.Is(err, flags.ErrNotFound) {
		return Response{Result: evaluation.Result{Value: in.Fallback, Reason: "flag_not_found"}, DecisionID: identity.New("dec_")}, nil
	}
	if err != nil {
		return Response{}, err
	}
	if !evaluation.Equal(in.Fallback, d.Safe) {
		return Response{}, auth.ErrInvalid
	}
	compiled, err := evaluation.Compile(d)
	if err != nil {
		return Response{}, err
	}
	result, err := compiled.Evaluate(in.UserID, in.Attributes)
	if err != nil {
		return Response{}, auth.ErrInvalid
	}
	return Response{Result: result, DecisionID: identity.New("dec_")}, nil
}
func (s *Service) Snapshot(ctx context.Context, token string, key snapshot.Key) (*snapshot.Snapshot, error) {
	if _, err := s.auth.AuthenticateApplication(ctx, token, key.ProjectID, key.EnvironmentID, "config:read"); err != nil {
		return nil, err
	}
	if !validScope(key.ProjectID, key.EnvironmentID, key.FlagKey) {
		return nil, auth.ErrInvalid
	}
	if s.snapshots != nil {
		return s.snapshots.Get(ctx, key)
	}
	proof := time.Now()
	d, err := flags.New(s.pool).Get(ctx, key.ProjectID, key.EnvironmentID, key.FlagKey)
	if err != nil {
		return nil, err
	}
	return snapshot.New(d, proof)
}
