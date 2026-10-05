//go:build integration

package processing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/platform/messaging"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func setup(t *testing.T) (*pgxpool.Pool, *Store) {
	t.Helper()
	p := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, p, migrations.Files); err != nil {
		t.Fatal(err)
	}
	_, err := p.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin');
INSERT INTO projects(id,name) VALUES('project','Processing');INSERT INTO environments(id,project_id,name) VALUES('dev','project','development');
INSERT INTO flags(id,project_id,key,type) VALUES('flag','project','listing','boolean');
INSERT INTO flag_revisions(project_id,environment_id,flag_id,revision,definition,created_by) VALUES('project','dev','flag',1,'{}','admin');
INSERT INTO experiment_runs(id,project_id,environment_id,flag_id,name,state,control_variant_id,definition,created_by) VALUES('run','project','dev','flag','Processing','completed','control','{}','admin');
INSERT INTO application_keys(id,token_hash,project_id,environment_id,name,permissions,created_by) VALUES('key','fixture','project','dev','Fixture',ARRAY['events:write'],'admin')`)
	if err != nil {
		t.Fatal(err)
	}
	return p, New(p)
}
func fact(t *testing.T, p *pgxpool.Pool, id, user string) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	_, err := p.Exec(ctx, `INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,occurred_at,received_at,status,quarantine_reason,payload,application_key_id)
        VALUES('project','dev',$1,'run',$2,'exposure','control',1,now(),now(),'accepted','','{}','key')`, id, user)
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	err = p.QueryRow(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision) VALUES('event','project','dev',$1,1)
        RETURNING jsonb_build_object('version',1,'message_id','switchyard-outbox-v1-'||id,'reference',jsonb_build_object('kind',kind,'project_id',project_id,'environment_id',environment_id,'object_id',object_id,'revision',revision))`, id).Scan(&body)
	if err != nil {
		t.Fatal(err)
	}
	e, err := messaging.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestConcurrentReceiptsAndExpiredReplayCannotDoubleCount(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	e := fact(t, p, "display", "user")
	var group sync.WaitGroup
	var applied, duplicates atomic.Int32
	for range 20 {
		group.Go(func() {
			duplicate, err := s.Apply(ctx, e)
			if err != nil {
				t.Error(err)
				return
			}
			if duplicate {
				duplicates.Add(1)
			} else {
				applied.Add(1)
			}
		})
	}
	group.Wait()
	if applied.Load() != 1 || duplicates.Load() != 19 {
		t.Fatal("concurrent receipts not idempotent")
	}
	var states, receipts int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM metric_user_state),(SELECT count(*) FROM processed_work)`).Scan(&states, &receipts); err != nil || states != 1 || receipts != 1 {
		t.Fatalf("state/receipt=%d/%d %v", states, receipts, err)
	}
	wrong := e
	wrong.Reference.ProjectID = "other"
	if _, err := s.Apply(ctx, wrong); !errors.Is(err, ErrIdentity) {
		t.Fatal("message identity reused for another scope")
	}
	if _, err := p.Exec(ctx, `DELETE FROM raw_events WHERE event_id='display'`); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.Apply(ctx, e); err != nil || !duplicate {
		t.Fatal("retention removed durable deduplication")
	}
	expired := fact(t, p, "expired", "other")
	if _, err := p.Exec(ctx, `DELETE FROM raw_events WHERE event_id='expired'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, expired); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("unprocessed expired source was applied")
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM processed_work`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("missing source committed processing receipt")
	}
}
func TestSchedulingFailureRollsBackReceiptAndDeadLetterIsDurable(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	e := fact(t, p, "display", "user")
	if _, err := p.Exec(ctx, `CREATE FUNCTION reject_work_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'work fixture failure'; END; $$; CREATE TRIGGER work_fixture BEFORE INSERT ON metric_user_state FOR EACH ROW EXECUTE FUNCTION reject_work_fixture()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, e); err == nil {
		t.Fatal("receipt committed without durable reconciliation")
	}
	var count int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM processed_work`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed transaction retained receipt")
	}
	for range 2 {
		if err := s.DeadLetter(ctx, "STREAM", 1, e.MessageID, []byte(`{"version":99}`), "invalid_envelope"); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM work_dead_letters`).Scan(&count); err != nil || count != 1 {
		t.Fatal(fmt.Sprintf("dead letters=%d %v", count, err))
	}
}
