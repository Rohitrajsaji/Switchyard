// Package grpcapi adapts the shared application service to bounded unary RPCs.
package grpcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"switchyard/internal/applicationeval"
	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/flags"
	pb "switchyard/pkg/api/switchyard/v1"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

const MaxRequestBytes = 65536

type adapter struct {
	pb.UnimplementedEvaluationServiceServer
	service *applicationeval.Service
}

// Observer receives one record per unary RPC (bounded method and code vocabularies).
type Observer interface {
	ObserveGRPC(fullMethod, code string, d time.Duration)
}

func NewServer(service *applicationeval.Service) *grpc.Server {
	return NewObservedServer(service, nil)
}

// NewObservedServer is NewServer with request metrics and extra options (for example tracing).
func NewObservedServer(service *applicationeval.Service, observer Observer, extra ...grpc.ServerOption) *grpc.Server {
	slots := make(chan struct{}, 64)
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		started := time.Now()
		result, err := func() (any, error) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				return nil, status.Error(codes.ResourceExhausted, "evaluation capacity exhausted")
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return handler(ctx, req)
		}()
		if observer != nil {
			observer.ObserveGRPC(info.FullMethod, status.Code(err).String(), time.Since(started))
		}
		return result, err
	}
	options := append([]grpc.ServerOption{grpc.MaxRecvMsgSize(MaxRequestBytes), grpc.MaxSendMsgSize(2 << 20), grpc.MaxHeaderListSize(65536), grpc.MaxConcurrentStreams(64), grpc.UnaryInterceptor(interceptor)}, extra...)
	server := grpc.NewServer(options...)
	pb.RegisterEvaluationServiceServer(server, &adapter{service: service})
	return server
}
func token(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 {
		return "", status.Error(codes.Unauthenticated, "application key required")
	}
	value, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok {
		return "", status.Error(codes.Unauthenticated, "application key required")
	}
	return value, nil
}
func rpcError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, auth.ErrUnauthorized):
		return status.Error(codes.Unauthenticated, "application key required")
	case errors.Is(err, auth.ErrForbidden):
		return status.Error(codes.PermissionDenied, "scope or permission denied")
	case errors.Is(err, auth.ErrInvalid), errors.Is(err, snapshot.ErrInvalid):
		return status.Error(codes.InvalidArgument, "invalid evaluation input")
	case errors.Is(err, flags.ErrNotFound), errors.Is(err, cache.ErrNotFound):
		return status.Error(codes.NotFound, "flag not found")
	case errors.Is(err, cache.ErrUnavailable), errors.Is(err, cache.ErrClosed), errors.Is(err, cache.ErrBusy):
		return status.Error(codes.Unavailable, "configuration unavailable")
	default:
		return status.Error(codes.Internal, "evaluation failed")
	}
}
func input(project, env string, in *pb.Evaluation) (applicationeval.Input, error) {
	if in == nil || in.Fallback == nil {
		// Preserve the scope for authorization before semantic validation, as HTTP does.
		return applicationeval.Input{ProjectID: project, EnvironmentID: env}, nil
	}
	attrs := make(map[string]json.RawMessage, len(in.AttributesJson))
	for key, value := range in.AttributesJson {
		attrs[key] = bytes.Clone(value)
	}
	return applicationeval.Input{ProjectID: project, EnvironmentID: env, Key: in.Key, UserID: in.UserId, Attributes: attrs, Fallback: evaluation.Value{Type: in.Fallback.Type, Data: bytes.Clone(in.Fallback.Json)}}, nil
}
func response(in applicationeval.Response) *pb.EvaluateResponse {
	availability := pb.Availability_AVAILABILITY_AVAILABLE
	if in.Unavailable {
		availability = pb.Availability_AVAILABILITY_CONFIGURATION_UNAVAILABLE
	}
	return &pb.EvaluateResponse{Value: &pb.Value{Type: in.Value.Type, Json: bytes.Clone(in.Value.Data)}, Reason: in.Reason, Revision: in.Revision, RunId: in.RunID, VariantId: in.VariantID, DecisionId: in.DecisionID, Availability: availability}
}
func (a *adapter) Evaluate(ctx context.Context, req *pb.EvaluateRequest) (*pb.EvaluateResponse, error) {
	credential, err := token(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || proto.Size(req) > MaxRequestBytes {
		return nil, rpcError(auth.ErrInvalid)
	}
	in, err := input(req.ProjectId, req.EnvironmentId, req.Input)
	if err != nil {
		return nil, rpcError(err)
	}
	result, err := a.service.Evaluate(ctx, credential, in)
	if err != nil && !errors.Is(err, cache.ErrUnavailable) {
		return nil, rpcError(err)
	}
	return response(result), nil
}
func (a *adapter) EvaluateBatch(ctx context.Context, req *pb.EvaluateBatchRequest) (*pb.EvaluateBatchResponse, error) {
	credential, err := token(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || proto.Size(req) > MaxRequestBytes || len(req.Inputs) < 1 || len(req.Inputs) > applicationeval.MaxBatch {
		return nil, rpcError(auth.ErrInvalid)
	}
	inputs := make([]applicationeval.Input, 0, len(req.Inputs))
	for _, item := range req.Inputs {
		in, err := input(req.ProjectId, req.EnvironmentId, item)
		if err != nil {
			return nil, rpcError(err)
		}
		inputs = append(inputs, in)
	}
	results, err := a.service.Batch(ctx, credential, inputs)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &pb.EvaluateBatchResponse{Decisions: make([]*pb.EvaluateResponse, 0, len(results))}
	for _, result := range results {
		out.Decisions = append(out.Decisions, response(result))
	}
	return out, nil
}
func (a *adapter) GetSnapshot(ctx context.Context, req *pb.GetSnapshotRequest) (*pb.GetSnapshotResponse, error) {
	credential, err := token(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || proto.Size(req) > MaxRequestBytes {
		return nil, rpcError(auth.ErrInvalid)
	}
	value, err := a.service.Snapshot(ctx, credential, snapshot.Key{ProjectID: req.ProjectId, EnvironmentID: req.EnvironmentId, FlagKey: req.Key})
	if err != nil {
		return nil, rpcError(err)
	}
	body, err := value.MarshalJSON()
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.GetSnapshotResponse{SnapshotJson: body}, nil
}
