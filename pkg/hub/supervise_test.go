package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A worker that returns nil because the shutdown signal cancelled the context
// is a normal stop; the supervisor must not report it as "stopped
// unexpectedly" (which exited with code 1 whenever the worker's exit and the
// cancellation were both ready and the select happened to pick the worker).
func TestWorkerExitDuringShutdownIsNotAnError(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(live)
	cancel()

	boom := errors.New("listener died")
	if got := unexpectedStop(cancelled, nil); got != nil {
		t.Errorf("a nil exit after cancellation is a normal shutdown, got %v", got)
	}
	if got := unexpectedStop(live, nil); got == nil || !strings.Contains(got.Error(), "stopped unexpectedly") {
		t.Errorf("a nil exit with a live context is unexpected, got %v", got)
	}
	if got := unexpectedStop(live, boom); !errors.Is(got, boom) {
		t.Errorf("a worker's error is kept, got %v", got)
	}
	if got := unexpectedStop(cancelled, boom); !errors.Is(got, boom) {
		t.Errorf("a real error during shutdown is still reported, got %v", got)
	}
}
