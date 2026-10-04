package experiments

import (
	"errors"
	"switchyard/internal/auth"
	"testing"
)

func TestLifecycle(t *testing.T) {
	cases := []struct {
		state, action, want string
		err                 error
	}{
		{"draft", "start", "running", nil}, {"running", "pause", "paused", nil}, {"paused", "start", "running", nil},
		{"draft", "complete", "completed", nil}, {"running", "complete", "completed", nil}, {"paused", "complete", "completed", nil},
		{"completed", "start", "", auth.ErrConflict}, {"draft", "pause", "", auth.ErrConflict}, {"running", "start", "", auth.ErrConflict}, {"paused", "pause", "", auth.ErrConflict}, {"completed", "complete", "", auth.ErrConflict}, {"running", "edit", "", auth.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.state+"/"+c.action, func(t *testing.T) {
			got, err := nextState(c.state, c.action)
			if got != c.want || !errors.Is(err, c.err) {
				t.Fatalf("got %s %v", got, err)
			}
		})
	}
}
