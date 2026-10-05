//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func setup(t *testing.T) (*pgxpool.Pool, *outbox.Store) {
	t.Helper()
	pool := testutil.Database(t)
	if err := postgres.Migrate(context.Background(), pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return pool, outbox.New(pool)
}

func TestUpgradeBackfillsRetainedFactsExactlyOnce(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	legacy := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "0007_") {
			continue
		}
		body, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		legacy[name] = &fstest.MapFile{Data: body}
	}
	if err := postgres.Migrate(ctx, pool, legacy); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin');
INSERT INTO projects(id,name) VALUES('project','Upgrade');
INSERT INTO environments(id,project_id,name) VALUES('dev','project','development');
INSERT INTO flags(id,project_id,key,type) VALUES('flag','project','listing','boolean');
INSERT INTO flag_revisions(project_id,environment_id,flag_id,revision,definition,created_by) VALUES('project','dev','flag',1,'{}','admin');
INSERT INTO experiment_runs(id,project_id,environment_id,flag_id,name,state,control_variant_id,definition,created_by)
VALUES('run','project','dev','flag','Upgrade','completed','control','{}','admin');
INSERT INTO application_keys(id,token_hash,project_id,environment_id,name,permissions,created_by)
VALUES('key','fixture','project','dev','Fixture',ARRAY['events:write'],'admin');
INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,occurred_at,received_at,status,quarantine_reason,payload,application_key_id)
VALUES('project','dev','event','run','synthetic','exposure','control',1,now(),now(),'accepted','','{}','key')`)
	for range 2 {
		if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	items, err := outbox.New(pool).Claim(ctx, 10, 10*time.Second)
	if err != nil || len(items) != 2 {
		t.Fatalf("upgrade intents=%v %v", items, err)
	}
	kinds := map[string]int{}
	for _, i := range items {
		kinds[i.Kind]++
	}
	if kinds["configuration"] != 1 || kinds["event"] != 1 {
		t.Fatal("upgrade skipped retained facts")
	}
}
func record(t *testing.T, pool *pgxpool.Pool, id string, commit bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	r := outbox.Reference{Kind: "event", ProjectID: "project", EnvironmentID: "dev", ObjectID: id, Revision: 1}
	if err = outbox.Record(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	if commit {
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func TestPublicationIntentRollbackIdentityLeaseAndAmbiguousAck(t *testing.T) {
	pool, s := setup(t)
	ctx := context.Background()
	record(t, pool, "rolled_back", false)
	if items, err := s.Claim(ctx, 10, 10*time.Second); err != nil || len(items) != 0 {
		t.Fatalf("rolled back intent=%v %v", items, err)
	}
	record(t, pool, "committed", true)
	record(t, pool, "committed", true)
	items, err := s.Claim(ctx, 10, 10*time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("duplicate intent=%v %v", items, err)
	}
	first := items[0]
	if err = s.Published(ctx, outbox.Item{ID: first.ID, ClaimToken: "forged"}); !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatal("unowned publication accepted")
	}
	if next, err := s.Claim(ctx, 10, 10*time.Second); err != nil || len(next) != 0 {
		t.Fatal("active lease claimed twice")
	}
	// Simulate publish acknowledgement lost, then publisher death before marking.
	exec(t, pool, `UPDATE outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID)
	items, err = s.Claim(ctx, 1, 10*time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("reclaim=%v %v", items, err)
	}
	second := items[0]
	if second.MessageID() != first.MessageID() || second.ClaimToken == first.ClaimToken || second.Attempts != 2 {
		t.Fatal("retry lost identity or lease fence")
	}
	if err = s.Published(ctx, first); !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatal("stale publisher completed new lease")
	}
	if err = s.Published(ctx, second); err != nil {
		t.Fatal(err)
	}
	if items, err = s.Claim(ctx, 100, 10*time.Second); err != nil || len(items) != 0 {
		t.Fatal("completed publication reclaimed")
	}
}
func TestConcurrentClaimsBoundedRetryAndFinalAttemptCrash(t *testing.T) {
	pool, s := setup(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		record(t, pool, id, true)
	}
	var group sync.WaitGroup
	claimed := make(chan outbox.Item, 4)
	for range 2 {
		group.Go(func() {
			items, err := s.Claim(ctx, 2, 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			for _, i := range items {
				claimed <- i
			}
		})
	}
	group.Wait()
	close(claimed)
	seen := map[int64]bool{}
	for i := range claimed {
		if seen[i.ID] {
			t.Fatal("concurrent claim overlap")
		}
		seen[i.ID] = true
		if err := s.Failed(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 4 {
		t.Fatal("claims lost work")
	}
	if items, err := s.Claim(ctx, 4, 10*time.Second); err != nil || len(items) != 0 {
		t.Fatal("retry ignored backoff")
	}
	// Inject elapsed retry/lease time, never wait for domain timing in a test.
	exec(t, pool, `UPDATE outbox SET attempts=9,available_at=clock_timestamp()-interval '1 second'`)
	items, err := s.Claim(ctx, 4, 10*time.Second)
	if err != nil || len(items) != 4 {
		t.Fatal(err)
	}
	if err = s.Failed(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE lease_until IS NOT NULL`)
	if items, err = s.Claim(ctx, 4, 10*time.Second); err != nil || len(items) != 0 {
		t.Fatal("exhausted crash retried forever")
	}
	var dead int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dead_at IS NOT NULL AND failure_code='attempts_exhausted' AND claim_token IS NULL`).Scan(&dead); err != nil || dead != 4 {
		t.Fatalf("dead=%d %v", dead, err)
	}
	if _, err = s.Claim(ctx, 101, time.Second); !errors.Is(err, outbox.ErrInvalid) {
		t.Fatal("unbounded claim accepted")
	}
}
