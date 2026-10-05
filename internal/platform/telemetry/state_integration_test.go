//go:build integration

package telemetry_test

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/platform/telemetry"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

// The collector's SQL must run against the real migrated schema: a wrong table or column makes
// every durable-state gauge disappear, which a dashboard would show as "no data" rather than an error.
func TestStateCollectorQueriesTheMigratedSchema(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(telemetry.StateCollector(pool, "worker"), telemetry.PoolCollector(pool, "worker"))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Gauge != nil {
				values[family.GetName()] = metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				values[family.GetName()] = metric.Counter.GetValue()
			}
		}
	}
	if values["switchyard_state_scrape_ok"] != 1 {
		t.Fatal("durable-state query failed against the migrated schema", values)
	}
	for _, name := range []string{"switchyard_outbox_unpublished", "switchyard_outbox_oldest_unpublished_age_seconds", "switchyard_outbox_dead",
		"switchyard_dead_letters_unresolved", "switchyard_safety_rollbacks_total", "switchyard_rollout_plans_running", "switchyard_db_pool_max_connections"} {
		if _, ok := values[name]; !ok {
			t.Fatal("missing gauge", name)
		}
	}
	// A pending intent becomes visible with a positive age.
	if _, err := pool.Exec(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision,created_at) VALUES('event','p','e','o',1,clock_timestamp()-interval '90 seconds')`); err != nil {
		t.Fatal(err)
	}
	other := prometheus.NewRegistry()
	other.MustRegister(telemetry.StateCollector(pool, "worker"))
	families, _ = other.Gather()
	seen := 0
	for _, family := range families {
		switch family.GetName() {
		case "switchyard_outbox_unpublished":
			seen++
			if family.Metric[0].Gauge.GetValue() != 1 {
				t.Fatal("pending intent not counted")
			}
		case "switchyard_outbox_oldest_unpublished_age_seconds":
			seen++
			if age := family.Metric[0].Gauge.GetValue(); age < 90 || age > 120 {
				t.Fatal("unexpected oldest age", age)
			}
		}
	}
	if seen != 2 {
		t.Fatal("outbox gauges missing from second scrape")
	}
}
