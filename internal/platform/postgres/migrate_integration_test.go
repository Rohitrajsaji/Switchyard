//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"testing/fstest"

	"switchyard/internal/platform/postgres"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func TestMigrationFreshSchemaIdempotenceAndConcurrentStartup(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Ready(ctx, pool); err == nil {
		t.Fatal("unmigrated database reported ready")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- postgres.Migrate(ctx, pool, migrations.Files) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := postgres.Ready(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration count=%d", count)
	}
}

func TestMigrationRefusesChangedHistoryAndRollsBackBadSQL(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	changed := fstest.MapFS{"0001_foundation.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}
	if err := postgres.Migrate(ctx, pool, changed); err == nil {
		t.Fatal("edited migration accepted")
	}
	body, err := migrations.Files.ReadFile("0001_foundation.sql")
	if err != nil {
		t.Fatal(err)
	}
	broken := fstest.MapFS{
		"0001_foundation.sql": &fstest.MapFile{Data: body},
		"9998_probe.sql":      &fstest.MapFile{Data: []byte("CREATE TABLE migration_probe(id int);")},
		"9999_bad.sql":        &fstest.MapFile{Data: []byte("INVALID SQL;")},
	}
	if err := postgres.Migrate(ctx, pool, broken); err == nil {
		t.Fatal("bad SQL accepted")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('migration_probe') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("failed migration partially committed")
	}
}
