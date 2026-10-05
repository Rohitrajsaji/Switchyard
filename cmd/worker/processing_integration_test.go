//go:build integration

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/cache"
	"switchyard/internal/flags"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

type capturedSnapshots struct{ values []*snapshot.Snapshot }

func (s *capturedSnapshots) Get(context.Context, snapshot.Key) (*snapshot.Snapshot, error) {
	return nil, cache.ErrMiss
}
func (s *capturedSnapshots) Put(ctx context.Context, v *snapshot.Snapshot) (bool, error) {
	s.values = append(s.values, v)
	return true, nil
}
func TestDelayedConfigurationNotificationLoadsCurrentDisable(t *testing.T) {
	ctx := context.Background()
	p := testutil.Database(t)
	if err := postgres.Migrate(ctx, p, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "admin"}
	project, err := projects.New(p).Create(ctx, actor, "Notification", "project")
	if err != nil {
		t.Fatal(err)
	}
	var env string
	if err = p.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, project.ID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	v := func(data string) evaluation.Value {
		return evaluation.Value{Type: "boolean", Data: json.RawMessage(data)}
	}
	cfg := flags.Configuration{Default: v("true"), Safe: v("false")}
	service := flags.New(p)
	d, err := service.Create(ctx, actor, project.ID, flags.CreateInput{EnvironmentID: env, Key: "listing", Type: "boolean", Configuration: cfg, Reason: "baseline"}, "create")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Killed = true
	if _, err = service.Update(ctx, actor, project.ID, "listing", flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 1, Configuration: cfg, Reason: "disable"}, "disable"); err != nil {
		t.Fatal(err)
	}
	store := &capturedSnapshots{}
	refresh := refreshConfiguration(p, store)
	for _, revision := range []int64{2, 1} {
		if err = refresh(ctx, outbox.Reference{Kind: "configuration", ProjectID: project.ID, EnvironmentID: env, ObjectID: d.FlagID, Revision: revision}); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.values) != 2 {
		t.Fatal("notification was not refreshed")
	}
	for _, s := range store.values {
		result, err := s.Evaluate(time.Now(), "synthetic", nil)
		if err != nil || s.Revision() != 2 || result.Reason != "kill_switch" || string(result.Value.Data) != "false" {
			t.Fatal("late historical message restored enabled state")
		}
	}
}
