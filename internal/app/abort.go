package app

import (
	"fmt"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

// Abort records a durable request. The worker that owns the unreaped child is
// solely responsible for TERM/KILL escalation; this process never signals a PID.
func Abort(l layout.Layout, id model.RunID) (store.RunRecord, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return store.RunRecord{}, err
	}
	switch record.State {
	case model.RunQueued:
		// Queued -> Aborted releases the scheduler reservation that
		// Prepare took before the worker had a chance to start.
		aborted, abortErr := runStore.Transition(id, model.RunAborted, time.Now().UTC())
		if abortErr != nil {
			return store.RunRecord{}, abortErr
		}
		_ = scheduler.Release(l, aborted.Machine, id)
		return aborted, nil
	case model.RunStaging, model.RunRunning, model.RunCommitting:
		return runStore.RequestCancellation(id, time.Now().UTC())
	case model.RunLost:
		_ = scheduler.Release(l, record.Machine, id)
		return record, nil
	default:
		return store.RunRecord{}, fmt.Errorf("run %s cannot be aborted from state %s", id, record.State)
	}
}

// Abandon is the app-level helper used only when `cli.spawnRun` fails
// to start the detached worker. It terminalizes the prepared run as
// aborted so the slot is freed for the next submission and the record
// does not stall in `staging` waiting for a worker that never
// arrived.
func Abandon(l layout.Layout, id model.RunID) error {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	switch record.State {
	case model.RunQueued, model.RunStaging:
		if _, err := runStore.Transition(id, model.RunAborted, now); err != nil {
			return err
		}
		_ = scheduler.Release(l, record.Machine, id)
		return nil
	default:
		_ = scheduler.Release(l, record.Machine, id)
		return nil
	}
}
