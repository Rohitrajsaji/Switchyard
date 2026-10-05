// Command listing shows the Go SDK on the marketplace listing experiment: evaluate
// remotely, show the chosen flow, then report exposure, completion and a request sample
// with explicit delivery status. Local development only: it uses plaintext gRPC.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/credentials/insecure"
	"switchyard/pkg/sdk"
)

func need(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "set %s\n", name)
		os.Exit(2)
	}
	return v
}

func main() {
	project, env, key, user := need("SWITCHYARD_PROJECT_ID"), need("SWITCHYARD_ENVIRONMENT_ID"), need("SWITCHYARD_API_KEY"), need("SWITCHYARD_USER_ID")
	remote, err := sdk.NewRemote(sdk.RemoteConfig{Target: need("SWITCHYARD_GRPC_ADDR"), Token: key, ProjectID: project, EnvironmentID: env, Credentials: insecure.NewCredentials()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdk:", err)
		os.Exit(1)
	}
	defer remote.Close()
	events, err := sdk.NewEvents(sdk.EventsConfig{BaseURL: need("SWITCHYARD_HTTP_URL"), Token: key, ProjectID: project, EnvironmentID: env})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdk events:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The declared safe value routes to the control flow whenever evaluation is unavailable.
	treatment, decision, err := sdk.Boolean(ctx, remote, "listing_flow", user, nil, false)
	if err != nil {
		fmt.Println("evaluation degraded, using safe control flow:", err, "reason:", decision.Reason)
	}
	fmt.Printf("flow=%s reason=%s revision=%d run=%q variant=%q\n", map[bool]string{true: "simplified", false: "control"}[treatment], decision.Reason, decision.Revision, decision.RunID, decision.VariantID)

	started := time.Now()
	// ... the application renders the chosen listing flow here ...
	exposure, err := sdk.ExposureEvent(decision, user, time.Now())
	if err != nil {
		fmt.Println("no exposure recorded:", err) // e.g. unavailable or non-randomized decision
		return
	}
	completion, _ := sdk.CompletionEvent(exposure, time.Now())
	request, _ := sdk.RequestOutcomeEvent(exposure, "create-listing", time.Now(), false, float64(time.Since(started).Milliseconds()))
	for _, e := range []sdk.Event{exposure, completion, request} {
		if err := events.Enqueue(e); err != nil {
			fmt.Fprintln(os.Stderr, "enqueue:", err)
			os.Exit(1)
		}
	}
	delivery, err := events.Flush(ctx)
	fmt.Printf("accepted=%d quarantined=%d duplicates=%d rejected=%d retained=%d\n", delivery.Accepted, delivery.Quarantined, delivery.Duplicates, len(delivery.Rejected), delivery.Retained)
	if err != nil {
		// Events remain only in this process's memory; persist the stable Event values to survive exit.
		fmt.Fprintln(os.Stderr, "delivery incomplete:", err)
		os.Exit(1)
	}
}
