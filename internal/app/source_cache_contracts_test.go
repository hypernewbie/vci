package app

import (
	"context"
	"testing"
	"time"
)

// TestRemoteSSHFailureHasDeadlineDiagnostic states the transport
// path surfaces a diagnostic when its context is exhausted. The
// fixture-level deadline is honored rather than producing a silent
// hang.
func TestRemoteSSHFailureHasDeadlineDiagnostic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runSSH(ctx, "127.0.0.1:1", "true"); err == nil {
		t.Fatalf("unreachable destination must surface an error before deadline")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("deadline must be honored; elapsed %v", elapsed)
	}
}
