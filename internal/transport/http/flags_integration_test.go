//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

func TestEvaluationHTTPScopesSafeFallbackAndNoWrite(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "admin"}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, actor, "Evaluation", "create")
	if err != nil {
		t.Fatal(err)
	}
	var dev, staging string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='staging'`, p.ID).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	k, err := ps.CreateKey(ctx, actor, p.ID, dev, "app", []string{"evaluate"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	fv := func(s string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(s)} }
	fs := flags.New(pool)
	_, err = fs.Create(ctx, actor, p.ID, flags.CreateInput{Key: "listing", Type: "boolean", EnvironmentID: dev, Configuration: flags.Configuration{Default: fv("false"), Safe: fv("false"), Rules: []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: fv("true")}}}, Reason: "demo"}, "flag")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	baseTime := time.Now()
	var elapsed atomic.Int64
	var unavailable atomic.Bool
	options := cache.Defaults()
	options.Now = func() time.Time { return baseTime.Add(time.Duration(elapsed.Load())) }
	options.PollInterval = 0
	pgSource := cache.PostgresSource(pool)
	coordinator, err := cache.NewCoordinator(ctx, func(ctx context.Context, key snapshot.Key) (evaluation.Definition, error) {
		if unavailable.Load() {
			return evaluation.Definition{}, cache.ErrUnavailable
		}
		return pgSource(ctx, key)
	}, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	m, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(logger, func(context.Context) error { return nil }, m.Register)
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	evaluate := func(env, key, fallback, token string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"project_id": p.ID, "environment_id": env, "key": key, "user_id": "user-123", "attributes": map[string]string{"country": "JP"}, "fallback": map[string]any{"type": "boolean", "data": json.RawMessage(fallback)}})
		r := httptest.NewRequest("POST", "/v1/evaluate", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for range 20 {
		w := evaluate(dev, "listing", "false", k.Token)
		if w.Code != 200 {
			t.Fatalf("evaluate=%d %s", w.Code, w.Body.String())
		}
		var response struct {
			evaluation.Result
			DecisionID string `json:"decision_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Reason != "targeting" || string(response.Value.Data) != "true" || response.Revision != 1 || response.DecisionID == "" {
			t.Fatalf("bad result %+v", response)
		}
	}
	if coordinator.Stats().SourceReads != 1 || coordinator.Stats().MemoryHits < 19 {
		t.Fatalf("configuration read amplification: %+v", coordinator.Stats())
	}
	if w := evaluate(dev, "listing", "true", k.Token); w.Code != 400 {
		t.Fatal("unsafe caller fallback accepted")
	}
	if w := evaluate(staging, "listing", "false", k.Token); w.Code != 403 {
		t.Fatal("cross-environment key accepted")
	}
	if w := evaluate(dev, "listing", "false", "bad"); w.Code != 401 {
		t.Fatal("invalid key accepted")
	}
	if w := evaluate(dev, "missing", "false", k.Token); w.Code != 200 || !strings.Contains(w.Body.String(), "flag_not_found") {
		t.Fatal("missing flag did not safely fallback")
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries`).Scan(&after); err != nil || before != after {
		t.Fatal("evaluation wrote audit/state")
	}
	// Configuration outage is injected independently of real PostgreSQL-backed
	// authorization, so expiry cannot be hidden by an earlier auth failure.
	unavailable.Store(true)
	elapsed.Store(int64(30 * time.Second))
	w := evaluate(dev, "listing", "false", k.Token)
	if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Retry-After") != "1" ||
		!strings.Contains(w.Body.String(), `"reason":"cache_expired"`) || !strings.Contains(w.Body.String(), `"data":false`) {
		t.Fatalf("expired HTTP fallback=%d %s", w.Code, w.Body.String())
	}
	if w := evaluate(dev, "listing", "true", k.Token); w.Code != 400 {
		t.Fatal("known unsafe fallback accepted during outage")
	}
	ps.SetRevocationObserver(m.ForgetApplication) // the API instance that processes a revocation denies the key at once
	if err := ps.RevokeKey(ctx, actor, p.ID, k.ID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if w := evaluate(dev, "listing", "false", k.Token); w.Code != 401 {
		t.Fatal("configuration cache bypassed key revocation")
	}
}
