package sdk_test

import (
	"context"
	"testing"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/sdk"
	"switchyard/pkg/snapshot"
)

// Measures cached local reads only (including per-decision identity generation); network RPC
// latency is a separate measurement and must not be compared with these numbers.
func benchLocal(b *testing.B) *sdk.Local {
	b.Helper()
	proof := time.Now()
	body := wire(b, 1, proof, true)
	source := sourceFunc(func(context.Context, snapshot.Key) ([]byte, error) { return body, nil })
	local, err := sdk.NewLocal(source, sdk.LocalConfig{ProjectID: "project", EnvironmentID: "dev", Flags: map[string]evaluation.Value{"listing": boolValue(false)}, Now: func() time.Time { return proof }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { local.Close() })
	if err := local.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	return local
}

func BenchmarkLocalBoolean(b *testing.B) {
	local := benchLocal(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := sdk.Boolean(ctx, local, "listing", "user-1", nil, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLocalBooleanParallel(b *testing.B) {
	local := benchLocal(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, _, err := sdk.Boolean(ctx, local, "listing", "user-1", nil, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}
