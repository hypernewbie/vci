package app

import (
	"fmt"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
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
		return runStore.Transition(id, model.RunAborted, time.Now().UTC())
	case model.RunStaging, model.RunRunning, model.RunCommitting:
		return runStore.RequestCancellation(id, time.Now().UTC())
	case model.RunLost:
		return record, nil
	default:
		return store.RunRecord{}, fmt.Errorf("run %s cannot be aborted from state %s", id, record.State)
	}
}
