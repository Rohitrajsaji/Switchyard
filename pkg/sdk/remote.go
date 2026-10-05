// Package sdk provides deadline-bounded remote and immutable local evaluation.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	pb "switchyard/pkg/api/switchyard/v1"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

var ErrInvalid = errors.New("invalid SDK input or response")
var ErrUnavailable = errors.New("SDK configuration unavailable")
var ErrClosed = errors.New("SDK closed")

type Input struct {
	Key        string
	UserID     string
	Attributes map[string]json.RawMessage
	Fallback   evaluation.Value
}
type Decision struct {
	evaluation.Result
	DecisionID string
	Available  bool
}
type Evaluator interface {
	Evaluate(context.Context, Input) (Decision, error)
}

type RemoteConfig struct {
	Target, Token, ProjectID, EnvironmentID string
	Timeout                                 time.Duration
	// Explicit TLS or local insecure credentials; the SDK never guesses transport security.
	Credentials credentials.TransportCredentials
}
type Remote struct {
	cfg    RemoteConfig
	conn   *grpc.ClientConn
	client pb.EvaluationServiceClient
}

func NewRemote(cfg RemoteConfig) (*Remote, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	if cfg.Target == "" || cfg.Token == "" || cfg.ProjectID == "" || cfg.EnvironmentID == "" || cfg.Timeout <= 0 || cfg.Timeout > 5*time.Second || cfg.Credentials == nil {
		return nil, ErrInvalid
	}
	conn, err := grpc.NewClient(cfg.Target, grpc.WithTransportCredentials(cfg.Credentials), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(2<<20), grpc.MaxCallSendMsgSize(65536)))
	if err != nil {
		return nil, err
	}
	return &Remote{cfg: cfg, conn: conn, client: pb.NewEvaluationServiceClient(conn)}, nil
}
func (r *Remote) Close() error { return r.conn.Close() }
func (r *Remote) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	// Replace metadata instead of accidentally forwarding another app's credentials.
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("authorization", "Bearer "+r.cfg.Token)
	return metadata.NewOutgoingContext(ctx, md), cancel
}
func validate(in Input) error {
	if in.Key == "" || evaluation.ValidateContext(in.UserID, in.Attributes) != nil || in.Fallback.Validate(in.Fallback.Type) != nil {
		return ErrInvalid
	}
	return nil
}
func fallback(in Input, reason string) Decision {
	return Decision{Result: evaluation.Result{Value: evaluation.Value{Type: in.Fallback.Type, Data: bytes.Clone(in.Fallback.Data)}, Reason: reason}}
}
func protoInput(in Input) *pb.Evaluation {
	attrs := make(map[string][]byte, len(in.Attributes))
	for k, v := range in.Attributes {
		attrs[k] = bytes.Clone(v)
	}
	return &pb.Evaluation{Key: in.Key, UserId: in.UserID, AttributesJson: attrs, Fallback: &pb.Value{Type: in.Fallback.Type, Json: bytes.Clone(in.Fallback.Data)}}
}
func decode(in Input, out *pb.EvaluateResponse) (Decision, error) {
	if out == nil || out.Value == nil || out.Reason == "" || out.DecisionId == "" {
		return fallback(in, "rpc_invalid"), ErrInvalid
	}
	v := evaluation.Value{Type: out.Value.Type, Data: bytes.Clone(out.Value.Json)}
	if v.Validate(in.Fallback.Type) != nil {
		return fallback(in, "rpc_invalid"), ErrInvalid
	}
	available := out.Availability == pb.Availability_AVAILABILITY_AVAILABLE
	if !available && (out.Availability != pb.Availability_AVAILABILITY_CONFIGURATION_UNAVAILABLE || !evaluation.Equal(v, in.Fallback)) {
		return fallback(in, "rpc_invalid"), ErrInvalid
	}
	decision := Decision{Result: evaluation.Result{Value: v, Reason: out.Reason, Revision: out.Revision, RunID: out.RunId, VariantID: out.VariantId}, DecisionID: out.DecisionId, Available: available}
	if !available {
		return decision, ErrUnavailable
	}
	return decision, nil
}
func (r *Remote) Evaluate(ctx context.Context, in Input) (Decision, error) {
	if err := validate(in); err != nil {
		return fallback(in, "invalid_input"), err
	}
	ctx, cancel := r.callContext(ctx)
	defer cancel()
	out, err := r.client.Evaluate(ctx, &pb.EvaluateRequest{ProjectId: r.cfg.ProjectID, EnvironmentId: r.cfg.EnvironmentID, Input: protoInput(in)})
	if err != nil {
		return fallback(in, "rpc_unavailable"), err
	}
	return decode(in, out)
}

// EvaluateBatch preserves order. Transport/validation failure returns no partial
// decisions. Configuration-unavailable decisions remain explicit in the result.
func (r *Remote) EvaluateBatch(ctx context.Context, inputs []Input) ([]Decision, error) {
	if len(inputs) < 1 || len(inputs) > 100 {
		return nil, ErrInvalid
	}
	request := &pb.EvaluateBatchRequest{ProjectId: r.cfg.ProjectID, EnvironmentId: r.cfg.EnvironmentID}
	for _, in := range inputs {
		if err := validate(in); err != nil {
			return nil, err
		}
		request.Inputs = append(request.Inputs, protoInput(in))
	}
	ctx, cancel := r.callContext(ctx)
	defer cancel()
	out, err := r.client.EvaluateBatch(ctx, request)
	if err != nil {
		return nil, err
	}
	if out == nil || len(out.Decisions) != len(inputs) {
		return nil, ErrInvalid
	}
	decisions := make([]Decision, len(inputs))
	var resultErr error
	for i, in := range inputs {
		decision, err := decode(in, out.Decisions[i])
		if err != nil && !errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		decisions[i] = decision
		resultErr = errors.Join(resultErr, err)
	}
	return decisions, resultErr
}

// Snapshot preserves the server's original authoritative proof. The local client
// validates scope and age independently; an RPC receipt never extends freshness.
func (r *Remote) Snapshot(ctx context.Context, key snapshot.Key) ([]byte, error) {
	if key.ProjectID != r.cfg.ProjectID || key.EnvironmentID != r.cfg.EnvironmentID {
		return nil, ErrInvalid
	}
	ctx, cancel := r.callContext(ctx)
	defer cancel()
	out, err := r.client.GetSnapshot(ctx, &pb.GetSnapshotRequest{ProjectId: key.ProjectID, EnvironmentId: key.EnvironmentID, Key: key.FlagKey})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ErrInvalid
	}
	return bytes.Clone(out.SnapshotJson), nil
}

// Boolean always returns the caller's typed default on validation/transport
// failure, plus the error and decision metadata so callers can observe failure.
func Boolean(ctx context.Context, e Evaluator, key, user string, attrs map[string]json.RawMessage, safe bool) (bool, Decision, error) {
	raw := json.RawMessage("false")
	if safe {
		raw = json.RawMessage("true")
	}
	decision, err := e.Evaluate(ctx, Input{Key: key, UserID: user, Attributes: attrs, Fallback: evaluation.Value{Type: "boolean", Data: raw}})
	var result bool
	if json.Unmarshal(decision.Value.Data, &result) != nil || decision.Value.Type != "boolean" {
		return safe, decision, ErrInvalid
	}
	return result, decision, err
}
func JSON(ctx context.Context, e Evaluator, key, user string, attrs map[string]json.RawMessage, safe json.RawMessage) (json.RawMessage, Decision, error) {
	decision, err := e.Evaluate(ctx, Input{Key: key, UserID: user, Attributes: attrs, Fallback: evaluation.Value{Type: "json", Data: safe}})
	if decision.Value.Validate("json") != nil {
		return bytes.Clone(safe), decision, ErrInvalid
	}
	return bytes.Clone(decision.Value.Data), decision, err
}
