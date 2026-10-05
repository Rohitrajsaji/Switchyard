package httpapi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/outbox"
)

func TestClassifyFailureTreatsDeadlineAsRetryableOverload(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("query: %w", context.DeadlineExceeded)} {
		status, code, retry := classifyFailure(err)
		if status != 503 || code != "overloaded" || !retry {
			t.Fatalf("deadline mapped to %d %s retry=%v", status, code, retry)
		}
	}
	status, code, retry := classifyFailure(outbox.ErrCapacity)
	if status != 503 || code != "durable_work_capacity" || !retry {
		t.Fatalf("capacity mapped to %d %s retry=%v", status, code, retry)
	}
	status, code, retry = classifyFailure(auth.ErrInvalid)
	if status != 400 || code != "invalid_input" || retry {
		t.Fatalf("invalid input mapped to %d %s retry=%v", status, code, retry)
	}
	status, code, _ = classifyFailure(errors.New("unclassified"))
	if status != 500 || code != "internal_error" {
		t.Fatalf("unknown error mapped to %d %s", status, code)
	}
}
