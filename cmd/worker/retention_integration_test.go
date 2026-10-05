//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/metrics"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/processing"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func TestRetentionCycleFoldsExpiresReceiptsAndPreservesParity(t *testing.T) {
	ctx := context.Background()
	p := testutil.Database(t)
	if err := postgres.Migrate(ctx, p, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin');
 INSERT INTO projects(id,name) VALUES('project','Retention');INSERT INTO environments(id,project_id,name) VALUES('dev','project','development');
 INSERT INTO project_memberships(project_id,user_id) VALUES('project','admin');
 INSERT INTO flags(id,project_id,key,type) VALUES('flag','project','listing','boolean');
 INSERT INTO flag_revisions(project_id,environment_id,flag_id,revision,definition,created_by) VALUES('project','dev','flag',1,'{}','admin');
 INSERT INTO experiment_runs(id,project_id,environment_id,flag_id,name,state,control_variant_id,definition,created_by) VALUES('run','project','dev','flag','Retention','completed','control','{}','admin');
 INSERT INTO application_keys(id,token_hash,project_id,environment_id,name,permissions,created_by) VALUES('key','fixture','project','dev','Fixture',ARRAY['events:write'],'admin')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp := now.Add(-9 * 24 * time.Hour)
	if _, err := p.Exec(ctx, `INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,occurred_at,received_at,status,quarantine_reason,payload,application_key_id)
 VALUES('project','dev','anchor','run','user','exposure','control',1,$1,$1,'accepted','','{}','key')`, stamp); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := p.QueryRow(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision,created_at,published_at) VALUES('event','project','dev','anchor',1,$1,$1) RETURNING id`, stamp).Scan(&id); err != nil {
		t.Fatal(err)
	}
	envelope := messaging.Envelope{Version: 1, MessageID: fmt.Sprintf("switchyard-outbox-v1-%d", id), Reference: outbox.Reference{Kind: "event", ProjectID: "project", EnvironmentID: "dev", ObjectID: "anchor", Revision: 1}}
	store := processing.New(p)
	if _, err := store.Apply(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE processed_work SET processed_at=$1`, stamp); err != nil {
		t.Fatal(err)
	}
	// Notification receipt precedes the reconcile call's clock.
	now = time.Now().UTC().Truncate(time.Microsecond)
	if worked, err := metrics.ReconcileOne(ctx, p, now); err != nil || !worked {
		t.Fatal("initial reconcile", worked, err)
	}
	stats, err := retentionStep(ctx, p, now, 7, 90)
	if err != nil || stats.FoldedRaw != 1 || stats.Identities != 1 || stats.Publications != 1 || stats.Receipts != 1 {
		t.Fatal("bounded cycle", stats, err)
	}
	if worked, err := metrics.ReconcileOne(ctx, p, now); err != nil || !worked {
		t.Fatal("fold reconcile", worked, err)
	}
	service := metrics.New(p, func() time.Time { return now })
	if equal, err := service.Compare(ctx, auth.Actor{ID: "admin"}, "project", "run"); err != nil || !equal {
		t.Fatal("retained cycle parity", equal, err)
	}
	if _, err := store.Apply(ctx, envelope); !errors.Is(err, processing.ErrSourceMissing) {
		t.Fatal("expired receipt replay regenerated work", err)
	}
	// Retained replay repairs only replaceable contributions. The older exposure
	// summary survives and is not rebuilt from the already-deleted raw source.
	if _, err := p.Exec(ctx, `INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,exposure_id,occurred_at,received_at,status,quarantine_reason,is_error,latency_ms,payload,application_key_id)
 VALUES('project','dev','fresh','run','user','exposure','control',1,NULL,$1::timestamptz-interval '1 minute',$1,'accepted','',NULL,NULL,'{}','key'),
 ('project','dev','fresh_request','run','user','request_outcome','control',1,'fresh',$1::timestamptz-interval '30 seconds',$1,'accepted','',false,500,'{}','key')`, now); err != nil {
		t.Fatal(err)
	}
	for _, eventID := range []string{"fresh", "fresh_request"} {
		var nextID int64
		if err := p.QueryRow(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision,published_at) VALUES('event','project','dev',$1,1,clock_timestamp()) RETURNING id`, eventID).Scan(&nextID); err != nil {
			t.Fatal(err)
		}
		message := messaging.Envelope{Version: 1, MessageID: fmt.Sprintf("switchyard-outbox-v1-%d", nextID), Reference: outbox.Reference{Kind: "event", ProjectID: "project", EnvironmentID: "dev", ObjectID: eventID, Revision: 1}}
		if _, err := store.Apply(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	now = time.Now().UTC().Truncate(time.Microsecond)
	if worked, err := metrics.ReconcileOne(ctx, p, now); err != nil || !worked {
		t.Fatal("retained reconcile", worked, err)
	}
	var historyBefore []byte
	if err := p.QueryRow(ctx, `SELECT historical_contribution FROM metric_user_state`).Scan(&historyBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE metric_user_state SET contribution=jsonb_set(contribution,'{requests,0,count}','0'),due_at=NULL;
 UPDATE metric_counts SET value=0 WHERE category='request' AND metric='count'`); err != nil {
		t.Fatal(err)
	}
	if equal, err := service.Compare(ctx, auth.Actor{ID: "admin"}, "project", "run"); err != nil || equal {
		t.Fatal("corrupted retained contribution accepted", equal, err)
	}
	operator := processing.Operator{Actor: auth.Actor{ID: "admin"}, Reason: "Verify retained-only replay after folding"}
	page, err := store.Replay(ctx, operator, "project", "dev", "run", now.Add(-6*24*time.Hour), now, now, "")
	if err != nil || page.Events != 2 {
		t.Fatal("retained replay", page, err)
	}
	// Replay's database clock schedules after the previous injected now.
	now = time.Now().UTC().Truncate(time.Microsecond)
	if worked, err := metrics.ReconcileOne(ctx, p, now); err != nil || !worked {
		t.Fatal("replay reconciliation", worked, err)
	}
	var unchanged bool
	if err := p.QueryRow(ctx, `SELECT historical_contribution=$1::jsonb FROM metric_user_state`, historyBefore).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("replay changed historical summary", err)
	}
	if equal, err := service.Compare(ctx, auth.Actor{ID: "admin"}, "project", "run"); err != nil || !equal {
		t.Fatal("retained replay parity", equal, err)
	}
	if _, err := store.Replay(ctx, operator, "project", "dev", "run", now.Add(-9*24*time.Hour), now, now, ""); !errors.Is(err, processing.ErrRecovery) {
		t.Fatal("expired interval replay accepted", err)
	}
	now = now.Add(90 * 24 * time.Hour)
	stats, err = retentionStep(ctx, p, now, 7, 90)
	if err != nil || stats.ExpiredUsers != 1 || stats.ExpiredSegments != 1 {
		t.Fatal("summary cycle", stats, err)
	}
	if worked, err := metrics.ReconcileOne(ctx, p, now); err != nil || !worked {
		t.Fatal("expiry reconcile", worked, err)
	}
	if equal, err := service.Compare(ctx, auth.Actor{ID: "admin"}, "project", "run"); err != nil || !equal {
		t.Fatal("expired cycle parity", equal, err)
	}
	stats, err = retentionStep(ctx, p, now, 7, 90)
	if err != nil || stats.FoldedRaw != 2 || stats.Publications != 2 || stats.Receipts != 2 {
		t.Fatal("expired raw tail cycle", stats, err)
	}
	if _, err := metrics.ReconcileOne(ctx, p, now); err != nil {
		t.Fatal(err)
	}
	if stats, err := retentionStep(ctx, p, now, 7, 90); err != nil || stats.worked() {
		t.Fatal("cycle did not drain", stats, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := retentionStep(canceled, p, now, 7, 90); err == nil {
		t.Fatal("canceled maintenance cycle proceeded")
	}
}
