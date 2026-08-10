package app

import (
	"context"
	"fmt"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
	"sync/atomic"
	"time"
)

// Abort persists a cancellation request and transitions run state; the owning
// Abort cancels a run or build request. A build request fans the cancellation
// out to every non-terminal target and then records the request as aborted.
func Abort(l model.Layout, id model.RunID) (store.RunRecord, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return store.RunRecord{}, err
	}
	if record.ParentRunID != "" {
		if parent, perr := runStore.Load(record.ParentRunID); perr == nil {
			record = parent
			id = parent.ID
		}
	}
	if len(record.Children) > 0 {
		children, _ := runStore.LoadChildren(id)
		for _, child := range children {
			if model.IsTerminal(child.State) {
				continue
			}
			if _, cerr := abortSingle(l, runStore, child); cerr == nil {
				_ = scheduler.Release(l, child.Machine, child.ID)
			}
		}
		return runStore.Transition(id, model.RunAborted, time.Now().UTC())
	}
	return abortSingle(l, runStore, record)
}

// abortSingle cancels one target run: an already-started run requests
// cancellation for its worker; a queued or lost run terminates immediately
// and frees its scheduler slot.
func abortSingle(l model.Layout, runStore store.Store, record store.RunRecord) (store.RunRecord, error) {
	switch record.State {
	case model.RunQueued:
		aborted, abortErr := runStore.Transition(record.ID, model.RunAborted, time.Now().UTC())
		if abortErr != nil {
			return store.RunRecord{}, abortErr
		}
		_ = scheduler.Release(l, aborted.Machine, record.ID)
		return aborted, nil
	case model.RunStaging, model.RunRunning, model.RunCommitting:
		return runStore.RequestCancellation(record.ID, time.Now().UTC())
	case model.RunLost:
		_ = scheduler.Release(l, record.Machine, record.ID)
		return record, nil
	default:
		return store.RunRecord{}, fmt.Errorf("run %s cannot be aborted from state %s", record.ID, record.State)
	}
}

// Abandon aborts queued/staging runs when detached worker startup fails,
// releasing the scheduler slot and preventing staging deadlock.
func Abandon(l model.Layout, id model.RunID) error {
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

func watchCancellation(ctx context.Context, cancel context.CancelFunc, runStore store.Store, id model.RunID, executionPath string, stop <-chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			record, err := runStore.Load(id)
			if err != nil {
				continue
			}
			if record.CancellationRequestedAt != nil {
				if execution, readErr := process.ReadExecution(executionPath, id); readErr == nil {
					execution.CancellationPhase = process.CancellationTerminating
					_ = process.WriteExecution(executionPath, execution)
				}
				cancel()
				return
			}
		}
	}
}
func renewLease(ctx context.Context, cancel context.CancelFunc, l model.Layout, id model.RunID, owner string, ttl time.Duration, lost *atomic.Bool, stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := store.Renew(l, id, owner, now.UTC(), ttl); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}
func cancellationRequested(s store.Store, id model.RunID) bool {
	r, err := s.Load(id)
	return err == nil && r.CancellationRequestedAt != nil
}

// cancelledByWorker reports an aborted result when worker context ended
// or cancellation is durably persisted despite load timing.
func cancelledByWorker(workerCtx context.Context, latest store.RunRecord, loadErr error) bool {
	return workerCtx.Err() != nil || (loadErr == nil && latest.CancellationRequestedAt != nil)
}

type MaintenanceReport struct {
	Reaped    reaper.Report          `json:"reaped"`
	Retention reaper.RetentionReport `json:"retention"`
}

// Maintain executes the reaper sweep and retention enforcement, returning
// artifact-reap and retention metrics in a combined report.
func Maintain(l model.Layout) (MaintenanceReport, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return MaintenanceReport{}, err
	}
	now := time.Now().UTC()
	reaped, err := reaper.Run(l, now)
	if err != nil {
		return MaintenanceReport{}, err
	}
	reaper.ReapRemoteBundleCaches(&reaped, cfg, now)
	DispatchPending(l)
	retained, err := reaper.Enforce(l, cfg.Retention)
	if err != nil {
		return MaintenanceReport{}, err
	}
	return MaintenanceReport{Reaped: reaped, Retention: retained}, nil
}
