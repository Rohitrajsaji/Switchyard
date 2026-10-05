// workctl is a local operator command, using existing database credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/platform/config"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/processing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	f := flag.NewFlagSet("workctl", flag.ContinueOnError)
	action := f.String("action", "inspect", "inspect, replay, retry-publication, retry-dead-letter")
	actor := f.String("actor", "", "existing active administrator ID")
	project := f.String("project", "", "project ID")
	env := f.String("environment", "", "environment ID")
	reason := f.String("reason", "", "audit reason required for writes")
	runID := f.String("run", "", "experiment run ID")
	from := f.String("from", "", "inclusive received-at RFC3339 timestamp")
	until := f.String("until", "", "exclusive received-at RFC3339 timestamp")
	cursor := f.String("cursor", "", "previous replay page cursor")
	id := f.Int64("id", 0, "dead publication ID")
	stream := f.String("stream", "", "dead-letter stream")
	sequence := f.Int64("sequence", 0, "dead-letter sequence")
	afterPublication := f.Int64("after-publication", 0, "inspection publication cursor")
	afterStream := f.String("after-stream", "", "inspection stream cursor")
	afterSequence := f.Int64("after-sequence", 0, "inspection sequence cursor")
	if err := f.Parse(os.Args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *actor == "" || *project == "" || *env == "" {
		return errors.New("actor, project and environment are required; positional arguments are unsupported")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := processing.NewWithRawRetention(pool, cfg.RawRetentionDays)
	if err != nil {
		return err
	}
	operator := processing.Operator{Actor: auth.Actor{ID: *actor}, Reason: *reason}
	now := time.Now().UTC()
	var result any
	switch *action {
	case "inspect":
		result, err = store.InspectPage(ctx, operator.Actor, *project, *env, *afterPublication, *afterStream, *afterSequence)
	case "replay":
		start, parseErr := time.Parse(time.RFC3339Nano, *from)
		if parseErr != nil {
			return errors.New("from must be RFC3339")
		}
		end, parseErr := time.Parse(time.RFC3339Nano, *until)
		if parseErr != nil {
			return errors.New("until must be RFC3339")
		}
		result, err = store.Replay(ctx, operator, *project, *env, *runID, start, end, now, *cursor)
	case "retry-publication":
		err = store.RetryPublication(ctx, operator, *project, *env, *id, now)
		result = map[string]bool{"scheduled": err == nil}
	case "retry-dead-letter":
		err = store.RetryDeadLetter(ctx, operator, *project, *env, *stream, *sequence, now)
		result = map[string]bool{"resolved": err == nil}
	default:
		return errors.New("unknown recovery action")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
