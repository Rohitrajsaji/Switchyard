//go:build integration

package grpcapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	grpcapi "switchyard/internal/transport/grpc"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
	pb "switchyard/pkg/api/switchyard/v1"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/sdk"
	"switchyard/pkg/snapshot"
)

func TestHTTPGRPCParityAuthorizationBatchAndFreshness(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('rpc-admin','rpc@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "rpc-admin"}
	ps := projects.New(pool)
	project, err := ps.Create(ctx, actor, "RPC parity", "create")
	if err != nil {
		t.Fatal(err)
	}
	var env string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, project.ID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	key, err := ps.CreateKey(ctx, actor, project.ID, env, "RPC", []string{"evaluate", "config:read"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	evalOnly, err := ps.CreateKey(ctx, actor, project.ID, env, "Evaluation only", []string{"evaluate"}, "eval-key")
	if err != nil {
		t.Fatal(err)
	}
	value := func(kind, body string) evaluation.Value {
		return evaluation.Value{Type: kind, Data: json.RawMessage(body)}
	}
	safe := value("boolean", "false")
	cases := []struct {
		key      string
		config   flags.Configuration
		fallback evaluation.Value
		attrs    map[string]json.RawMessage
		reason   string
	}{
		{"listing", flags.Configuration{Default: safe, Safe: safe, Rules: []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: value("boolean", "true")}}}, safe, map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)}, "targeting"},
		{"killed", flags.Configuration{Default: value("boolean", "true"), Safe: safe, Killed: true}, safe, nil, "kill_switch"},
		{"rollout", flags.Configuration{Default: safe, Safe: safe, Rollout: &evaluation.Rollout{TrafficBP: 10000, Value: value("boolean", "true")}}, safe, nil, "rollout"},
		{"decimal", flags.Configuration{Default: value("json", `{"n":0.1234567890123456789}`), Safe: value("json", `null`), Rules: []evaluation.Rule{{Attribute: "score", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`0.1234567890123456789`)}, Value: value("json", `{"n":0.12345678901234567890123456789}`)}}}, value("json", `null`), map[string]json.RawMessage{"score": json.RawMessage(`0.1234567890123456789`)}, "targeting"},
		{"missing", flags.Configuration{}, safe, nil, "flag_not_found"},
	}
	for _, c := range cases[:len(cases)-1] {
		if _, err := flags.New(pool).Create(ctx, actor, project.ID, flags.CreateInput{Key: c.key, Type: c.fallback.Type, EnvironmentID: env, Configuration: c.config, Reason: "parity fixture"}, "flag-"+c.key); err != nil {
			t.Fatalf("fixture %s: %v", c.key, err)
		}
	}
	if _, err := flags.New(pool).Create(ctx, actor, project.ID, flags.CreateInput{Key: "experiment", Type: "boolean", EnvironmentID: env, Configuration: flags.Configuration{Default: safe, Safe: safe}, Reason: "experiment baseline"}, "experiment-flag"); err != nil {
		t.Fatal(err)
	}
	experimentService := experiments.New(pool)
	run, err := experimentService.Create(ctx, actor, project.ID, experiments.CreateInput{EnvironmentID: env, FlagKey: "experiment", ExpectedRevision: 1, Name: "Unequal listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 3000, Value: safe}, {ID: "treatment", Ordinal: 1, WeightBP: 7000, Value: value("boolean", "true")}}, Reason: "parity"}, "experiment-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := experimentService.Transition(ctx, actor, project.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "parity"}, "experiment-start"); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct {
		key      string
		config   flags.Configuration
		fallback evaluation.Value
		attrs    map[string]json.RawMessage
		reason   string
	}{key: "experiment", fallback: safe, reason: "experiment"})
	base := time.Now()
	var elapsed atomic.Int64
	var outage atomic.Bool
	opts := cache.Defaults()
	opts.PollInterval = 0
	opts.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	source := cache.PostgresSource(pool)
	coordinator, err := cache.NewCoordinator(ctx, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		if outage.Load() {
			return evaluation.Definition{}, cache.ErrUnavailable
		}
		return source(ctx, k)
	}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	management, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(logger, func(context.Context) error { return nil }, management.Register)
	server := grpcapi.NewServer(management.EvaluationService())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); <-done })
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewEvaluationServiceClient(conn)
	remote, err := sdk.NewRemote(sdk.RemoteConfig{Target: listener.Addr().String(), Token: key.Token, ProjectID: project.ID, EnvironmentID: env, Credentials: insecure.NewCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	subscriptions := make(map[string]evaluation.Value)
	for _, c := range cases {
		if c.key != "missing" {
			subscriptions[c.key] = c.fallback
		}
	}
	local, err := sdk.NewLocal(remote, sdk.LocalConfig{ProjectID: project.ID, EnvironmentID: env, Flags: subscriptions, Now: opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	if err := local.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	rpcCtx := func(credential string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
	}
	inputs := make([]*pb.Evaluation, 0, len(cases))
	for _, c := range cases {
		attrs := map[string][]byte{}
		for k, v := range c.attrs {
			attrs[k] = v
		}
		in := &pb.Evaluation{Key: c.key, UserId: "stable-user", AttributesJson: attrs, Fallback: &pb.Value{Type: c.fallback.Type, Json: c.fallback.Data}}
		inputs = append(inputs, in)
		body, err := json.Marshal(map[string]any{"project_id": project.ID, "environment_id": env, "key": c.key, "user_id": "stable-user", "attributes": c.attrs, "fallback": c.fallback})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v1/evaluate", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != 200 {
			t.Fatalf("HTTP %s: %d %s", c.key, recorder.Code, recorder.Body.String())
		}
		var httpResult evaluation.Result
		if err := json.Unmarshal(recorder.Body.Bytes(), &httpResult); err != nil {
			t.Fatal(err)
		}
		result, err := client.Evaluate(rpcCtx(key.Token), &pb.EvaluateRequest{ProjectId: project.ID, EnvironmentId: env, Input: in})
		if err != nil {
			t.Fatal(err)
		}
		if !evaluation.Equal(httpResult.Value, value(result.Value.Type, string(result.Value.Json))) || result.Reason != httpResult.Reason || result.Reason != c.reason || result.Revision != httpResult.Revision || result.RunId != httpResult.RunID || result.VariantId != httpResult.VariantID || !strings.HasPrefix(result.DecisionId, "dec_") || result.Availability != pb.Availability_AVAILABILITY_AVAILABLE {
			t.Fatalf("parity %s: HTTP=%+v RPC=%+v", c.key, httpResult, result)
		}
		if c.key == "decimal" && !bytes.Contains(result.Value.Json, []byte("0.12345678901234567890123456789")) {
			t.Fatal("decimal precision lost")
		}
		sdkInput := sdk.Input{Key: c.key, UserID: "stable-user", Attributes: c.attrs, Fallback: c.fallback}
		remoteResult, err := remote.Evaluate(ctx, sdkInput)
		if err != nil || !evaluation.Equal(remoteResult.Value, httpResult.Value) || remoteResult.Reason != httpResult.Reason || remoteResult.RunID != httpResult.RunID || remoteResult.VariantID != httpResult.VariantID || remoteResult.Revision != httpResult.Revision {
			t.Fatal("remote SDK parity", remoteResult, err)
		}
		if c.key != "missing" {
			localResult, err := local.Evaluate(ctx, sdkInput)
			if err != nil || !evaluation.Equal(localResult.Value, httpResult.Value) || localResult.Reason != httpResult.Reason || localResult.RunID != httpResult.RunID || localResult.VariantID != httpResult.VariantID || localResult.Revision != httpResult.Revision {
				t.Fatal("local SDK parity", localResult, err)
			}
		}
	}
	batch, err := client.EvaluateBatch(rpcCtx(key.Token), &pb.EvaluateBatchRequest{ProjectId: project.ID, EnvironmentId: env, Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Decisions) != len(cases) {
		t.Fatal("batch length")
	}
	for i, c := range cases {
		if batch.Decisions[i].Reason != c.reason {
			t.Fatal("batch order", i, batch)
		}
	}
	invalid := &pb.Evaluation{Key: "listing", UserId: "user", Fallback: &pb.Value{Type: "boolean", Json: []byte("true")}}
	if response, err := client.EvaluateBatch(rpcCtx(key.Token), &pb.EvaluateBatchRequest{ProjectId: project.ID, EnvironmentId: env, Inputs: []*pb.Evaluation{inputs[0], invalid}}); status.Code(err) != codes.InvalidArgument || response != nil {
		t.Fatal("partial invalid batch", response, err)
	}
	if _, err := client.EvaluateBatch(rpcCtx(key.Token), &pb.EvaluateBatchRequest{ProjectId: project.ID, EnvironmentId: env, Inputs: make([]*pb.Evaluation, 101)}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("unbounded batch", err)
	}
	for _, test := range []struct {
		credential, environment string
		input                   *pb.Evaluation
		code                    codes.Code
	}{{"invalid", env, nil, codes.Unauthenticated}, {key.Token, "another-environment", nil, codes.PermissionDenied}, {key.Token, env, nil, codes.InvalidArgument}} {
		if response, err := client.Evaluate(rpcCtx(test.credential), &pb.EvaluateRequest{ProjectId: project.ID, EnvironmentId: test.environment, Input: test.input}); status.Code(err) != test.code || response != nil {
			t.Fatal("auth/input precedence", test.code, response, err)
		}
	}
	snapshotReq := &pb.GetSnapshotRequest{ProjectId: project.ID, EnvironmentId: env, Key: "listing"}
	if _, err := client.GetSnapshot(rpcCtx(evalOnly.Token), snapshotReq); status.Code(err) != codes.PermissionDenied {
		t.Fatal("snapshot permission bypass", err)
	}
	first, err := client.GetSnapshot(rpcCtx(key.Token), snapshotReq)
	if err != nil {
		t.Fatal(err)
	}
	outage.Store(true)
	elapsed.Store(int64(10 * time.Second))
	second, err := client.GetSnapshot(rpcCtx(key.Token), snapshotReq)
	if err != nil || !bytes.Equal(first.SnapshotJson, second.GetSnapshotJson()) {
		t.Fatal("intermediary renewed proof", err)
	}
	elapsed.Store(int64(30 * time.Second))
	localValue, localDecision, localErr := sdk.Boolean(ctx, local, "listing", "stable-user", nil, false)
	if localValue || !errors.Is(localErr, sdk.ErrUnavailable) || localDecision.Reason != "cache_expired" {
		t.Fatal("local SDK renewed proof", localDecision, localErr)
	}
	expired, err := client.Evaluate(rpcCtx(key.Token), &pb.EvaluateRequest{ProjectId: project.ID, EnvironmentId: env, Input: inputs[0]})
	if err != nil || expired.Availability != pb.Availability_AVAILABILITY_CONFIGURATION_UNAVAILABLE || expired.Reason != "cache_expired" || string(expired.Value.Json) != "false" {
		t.Fatal("expired safe decision", expired, err)
	}
	if _, err := client.GetSnapshot(rpcCtx(key.Token), snapshotReq); status.Code(err) != codes.Unavailable {
		t.Fatal("expired snapshot served", err)
	}
	canceled, cancel := context.WithCancel(rpcCtx(key.Token))
	cancel()
	if _, err := client.Evaluate(canceled, &pb.EvaluateRequest{ProjectId: project.ID, EnvironmentId: env, Input: inputs[0]}); status.Code(err) != codes.Canceled {
		t.Fatal("cancellation ignored", err)
	}
	ps.SetRevocationObserver(management.ForgetApplication) // this instance drops the key immediately; a remote revocation waits for the cache TTL
	if err := ps.RevokeKey(ctx, actor, project.ID, key.ID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Evaluate(rpcCtx(key.Token), &pb.EvaluateRequest{ProjectId: project.ID, EnvironmentId: env, Input: inputs[0]}); status.Code(err) != codes.Unauthenticated || response != nil {
		t.Fatal("revocation bypass", response, err)
	}
}
