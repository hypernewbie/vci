package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

// ErrTargetUnavailable marks a target that could not be reached or staged, so
// the worker classifies the target run as unavailable rather than failed.
var ErrTargetUnavailable = errors.New("target unavailable")

// ErrTargetSelect marks a logs/artifacts request against a build request that
// did not name a target, or named one it does not have.
var ErrTargetSelect = errors.New("target selection required")

// unavailableError attaches ErrTargetUnavailable to a transport failure so the
// worker records the target as unavailable instead of failed.
func unavailableError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrTargetUnavailable, err)
}

// TargetView is one machine's public outcome in a build summary.
type TargetView struct {
	Machine      string         `json:"machine"`
	State        model.RunState `json:"state"`
	ExitCode     int            `json:"exit_code,omitempty"`
	Failure      string         `json:"failure,omitempty"`
	ErrorContext string         `json:"error_context,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
}

// BuildSummary is the public aggregate for a build request.
type BuildSummary struct {
	RunID              model.RunID    `json:"run_id"`
	Project            string         `json:"project"`
	State              model.RunState `json:"state"`
	Targets            []TargetView   `json:"targets"`
	Succeeded          int            `json:"succeeded"`
	Failed             int            `json:"failed"`
	Lost               int            `json:"lost"`
	Unavailable        int            `json:"unavailable"`
	Aborted            int            `json:"aborted"`
	NoMachineResponded bool           `json:"no_machine_responded"`
	Warnings           []string       `json:"warnings,omitempty"`
}

// BuildSummaryView builds the public aggregate for a run id. A child id
// resolves to its parent; a legacy single-machine run is returned as-is.
func BuildSummaryView(l model.Layout, id model.RunID) (any, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return nil, err
	}
	if record.ParentRunID != "" {
		if parent, perr := runStore.Load(record.ParentRunID); perr == nil {
			return parentSummary(l, parent)
		}
	}
	if len(record.Children) > 0 {
		return parentSummary(l, record)
	}
	return legacySummary(l, record)
}

func parentSummary(l model.Layout, parent store.RunRecord) (BuildSummary, error) {
	runStore := store.Store{Layout: l}
	children, err := runStore.LoadChildren(parent.ID)
	if err != nil {
		return BuildSummary{}, err
	}
	sum := BuildSummary{RunID: parent.ID, Project: parent.Project, Targets: make([]TargetView, 0, len(children))}
	for _, child := range children {
		tv := TargetView{Machine: child.Machine, State: child.State}
		if model.IsTerminal(child.State) {
			if data, rerr := runStore.ReadResult(child.ID); rerr == nil {
				var r BuildResult
				if json.Unmarshal(data, &r) == nil {
					tv.ExitCode = r.ExitCode
					tv.Failure = r.Failure
					tv.Warnings = r.Warnings
				}
			}
			// A failed or lost target carries the last lines of stderr
			// inline so the caller sees the failure reason without a
			// separate `vci logs` round trip. Gone for terminal
			// successes, aborts, and unavailable targets — their
			// context is either empty or already explained by state.
			if child.State == model.RunFailed || child.State == model.RunLost {
				tv.ErrorContext = tailErrorContext(l, child.ID)
			}
		}
		switch child.State {
		case model.RunSucceeded:
			sum.Succeeded++
		case model.RunFailed:
			sum.Failed++
		case model.RunLost:
			sum.Lost++
		case model.RunUnavailable:
			sum.Unavailable++
		case model.RunAborted:
			sum.Aborted++
		}
		sum.Targets = append(sum.Targets, tv)
	}
	state, noMachine := store.AggregateState(children, parent.State == model.RunAborted)
	sum.State = state
	sum.NoMachineResponded = noMachine
	switch {
	case noMachine:
		sum.Warnings = append(sum.Warnings, "every attached machine was unavailable")
	case sum.Unavailable > 0 && state == model.RunSucceeded:
		sum.Warnings = append(sum.Warnings, fmt.Sprintf("%d of %d machines unavailable", sum.Unavailable, len(children)))
	}
	return sum, nil
}

// tailErrorContext returns bounded tails of a failed run's logs for inline
// display in the public check summary. When both streams have content, it
// keeps both because a warning on stderr must not hide a fatal diagnostic on
// stdout. The output is capped per stream so a runaway log cannot bloat the
// summary. Mirrors `gh run view`'s inline failure context: enough to
// recognise the error without a separate log fetch.
func tailErrorContext(l model.Layout, id model.RunID) string {
	stdout := tailLogStream(l, id, "stdout")
	stderr := tailLogStream(l, id, "stderr")
	switch {
	case stdout == "":
		return stderr
	case stderr == "":
		return stdout
	default:
		return "[stdout]\n" + stdout + "\n\n[stderr]\n" + stderr
	}
}

func tailLogStream(l model.Layout, id model.RunID, stream string) string {
	reader, _, err := ReadLog(l, id, stream)
	if err != nil {
		return ""
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || len(data) == 0 {
		return ""
	}
	const maxBytes = 4096
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	const maxLines = 20
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func legacySummary(l model.Layout, record store.RunRecord) (any, error) {
	if model.IsTerminal(record.State) {
		if data, err := (store.Store{Layout: l}).ReadResult(record.ID); err == nil {
			var result any
			if json.Unmarshal(data, &result) == nil {
				return result, nil
			}
		}
	}
	return record, nil
}

// DispatchPending reserves and launches queued target runs whose own machine
// has free capacity. It is the single coordinator-owned dispatch point: a new
// build and every maintenance pass call it. A busy machine leaves the run
// queued; an existing reservation is skipped so a run launches at most once.
// Launch failures release the claim so the next pass retries.
func DispatchPending(l model.Layout) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return
	}
	now := time.Now().UTC()
	runStore := store.Store{Layout: l}
	queued, err := runStore.LoadQueuedChildren()
	if err != nil {
		return
	}
	for _, child := range queued {
		if err := scheduler.Reserve(l, runStore, cfg, child.Machine, child.ID, now); err != nil {
			continue
		}
		if err := Launch(child.ID); err != nil {
			_ = scheduler.Release(l, child.Machine, child.ID)
		}
	}
}

// ResolveTarget selects the target run id for logs or artifacts. A build
// request requires --machine; a legacy single-machine run returns its own id.
func ResolveTarget(l model.Layout, id model.RunID, machine string) (model.RunID, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		if os.IsNotExist(err) {
			return "", model.ErrRunNotFound
		}
		return "", err
	}
	if record.ParentRunID != "" {
		if parent, perr := runStore.Load(record.ParentRunID); perr == nil {
			record = parent
		}
	}
	if len(record.Children) == 0 {
		return id, nil
	}
	children, err := runStore.LoadChildren(record.ID)
	if err != nil {
		return "", err
	}
	if machine == "" {
		names := make([]string, 0, len(children))
		for _, c := range children {
			names = append(names, c.Machine)
		}
		return "", fmt.Errorf("%w: run %s targets %d machines (%s): select one with --machine", ErrTargetSelect, record.ID, len(children), strings.Join(names, ", "))
	}
	for _, c := range children {
		if c.Machine == machine {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("%w: machine %q is not a target of run %s", ErrTargetSelect, machine, record.ID)
}

// ErrBuildBusy marks a build admission that was rejected because the
// coordinator already has a build in flight. Live names the parent build
// request that owns the busy state so the operator can wait for the right
// thing.
type ErrBuildBusy struct{ Live model.RunID }

func (e ErrBuildBusy) Error() string {
	return fmt.Sprintf("coordinator is running another build (%s); run \"vci wait-ready\" to block until it finishes", e.Live)
}

// admitAndStage serializes build admission. It takes the scheduler lock,
// rejects with ErrBuildBusy when a live build is in flight, and otherwise
// runs stage to persist the new build request. Holding the lock across the
// busy check and record creation prevents two concurrent submissions from
// both passing the check. DispatchPending takes the same lock independently,
// so the lock is released before stage returns.
func admitAndStage(l model.Layout, stage func() (store.RunRecord, error)) (store.RunRecord, error) {
	unlock, err := store.Acquire(l.SchedulerLockPath())
	if err != nil {
		return store.RunRecord{}, err
	}
	defer unlock()
	if live, busy := (store.Store{Layout: l}).HasLiveBuild(); busy {
		return store.RunRecord{}, ErrBuildBusy{Live: live}
	}
	return stage()
}
