package app

// Regression coverage for ExecutePrepared's cancellation
// classification: a worker whose context was cancelled must publish
// RunAborted even when the final run-store load fails. The
// classification is exercised through the same predicate
// ExecutePrepared calls (cancelledByWorker); the store's file-backed
// Load and Transition both read the same run.json, so a deterministic
// end-to-end test cannot make only the final Load fail without a
// production test hook. TestBuildAbortTerminatesOwnedCommand already
// pins the end-to-end publish of RunAborted.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/store"
)

// TestCancelledWorkerClassifiesAbortedOnFinalLoadFailure pins the
// misclassification regression: before the fix,
// `cancelled := loadErr == nil && latest.CancellationRequestedAt != nil`
// forced cancelled=false whenever the final run-store load failed,
// so a cancelled worker was published as failed/infrastructure
// (execErr = context.Canceled) instead of aborted.
func TestCancelledWorkerClassifiesAbortedOnFinalLoadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The regression: worker cancelled + final run-store load fails
	// ⇒ the run must be classified aborted.
	if !cancelledByWorker(ctx, store.RunRecord{}, errors.New("final load failed")) {
		t.Fatal("cancelled worker with failing final load must classify as aborted")
	}

	// Control cases pin the preserved behavior.
	now := time.Now().UTC()
	if !cancelledByWorker(context.Background(), store.RunRecord{CancellationRequestedAt: &now}, nil) {
		t.Fatal("durable cancellation request must classify as aborted")
	}
	if cancelledByWorker(context.Background(), store.RunRecord{}, nil) {
		t.Fatal("healthy worker must not classify as aborted")
	}
	if !cancelledByWorker(ctx, store.RunRecord{}, nil) {
		t.Fatal("cancelled worker must classify as aborted even without a durable request")
	}
}
