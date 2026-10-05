package sdk_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	pb "switchyard/pkg/api/switchyard/v1"
	"switchyard/pkg/sdk"
)

type testRPC struct {
	pb.UnimplementedEvaluationServiceServer
}

func (s *testRPC) Evaluate(ctx context.Context, in *pb.EvaluateRequest) (*pb.EvaluateResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer test-key" {
		return nil, status.Error(codes.Unauthenticated, "bad credential")
	}
	response := &pb.EvaluateResponse{Value: &pb.Value{Type: "boolean", Json: []byte("true")}, Reason: "default", Revision: 1, DecisionId: "dec_test", Availability: pb.Availability_AVAILABILITY_AVAILABLE}
	switch in.Input.Key {
	case "slow":
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	case "unauthorized":
		return nil, status.Error(codes.PermissionDenied, "denied")
	case "malformed":
		response.Value.Json = []byte(`"not boolean"`)
	case "unknown":
		response.Availability = pb.Availability_AVAILABILITY_UNSPECIFIED
	case "unavailable":
		response.Value.Json = []byte("false")
		response.Reason = "cache_expired"
		response.Availability = pb.Availability_AVAILABILITY_CONFIGURATION_UNAVAILABLE
	}
	return response, nil
}
func TestRemoteDeadlineTypedDefaultsAndResponseValidation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterEvaluationServiceServer(server, &testRPC{})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()
	remote, err := sdk.NewRemote(sdk.RemoteConfig{Target: listener.Addr().String(), Token: "test-key", ProjectID: "project", EnvironmentID: "dev", Timeout: 100 * time.Millisecond, Credentials: insecure.NewCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	// An incoming caller's metadata cannot add another credential to the SDK request.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer wrong-key")
	enabled, decision, err := sdk.Boolean(ctx, remote, "listing", "user", nil, false)
	if err != nil || !enabled || !decision.Available {
		t.Fatal("credential isolation", decision, err)
	}
	for _, test := range []struct {
		key    string
		code   codes.Code
		sdkErr error
		reason string
	}{
		{"slow", codes.DeadlineExceeded, nil, "rpc_unavailable"},
		{"unauthorized", codes.PermissionDenied, nil, "rpc_unavailable"},
		{"malformed", codes.Unknown, sdk.ErrInvalid, "rpc_invalid"},
		{"unknown", codes.Unknown, sdk.ErrInvalid, "rpc_invalid"},
		{"unavailable", codes.Unknown, sdk.ErrUnavailable, "cache_expired"},
	} {
		started := time.Now()
		enabled, decision, err := sdk.Boolean(ctx, remote, test.key, "user", nil, false)
		if enabled || err == nil || decision.Available || decision.Reason != test.reason {
			t.Fatal("unsafe default", test.key, decision, err)
		}
		if test.sdkErr != nil {
			if !errors.Is(err, test.sdkErr) {
				t.Fatal(test.key, err)
			}
		} else if status.Code(err) != test.code {
			t.Fatal(test.key, err)
		}
		if test.key == "slow" && time.Since(started) > time.Second {
			t.Fatal("unbounded deadline")
		}
		if test.key != "unavailable" && decision.DecisionID != "" {
			t.Fatal("failed RPC invented decision identity")
		}
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	enabled, decision, err = sdk.Boolean(ctx, remote, "listing", "user", nil, false)
	if enabled || err == nil || decision.DecisionID != "" {
		t.Fatal("closed remote invented decision", decision, err)
	}
}
