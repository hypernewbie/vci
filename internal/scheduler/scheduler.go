// Package scheduler implements coordinator-local capacity reservation.
//
// Vci is local-first: there is no remote-worker control plane. A
// coordinator project may attach multiple configured machine names,
// and each machine has a coordinator-owned local-slot capacity
// (`Machine.MaxConcurrent`). The scheduler reserves one available
// slot on a chosen eligible machine atomically before any durable run
// record is published and before any worker is spawned. The release
// path matches exactly: only the (machine, run-id) claim whose run
// record transitioned to a terminal state is removed.
//
// This is **not** a queue and not a daemon. There is no waiting,
// retry, streaming, or remote executor. A capacity exhaustion is a
// clean ErrNoCapacity admission failure that the CLI surfaces as
// `machine_unavailable`. The scheduler reaper walks only its own
// claim tree; it never signals a process and never infers liveness
// from claim age.
//
// Claim accounting model: an active reservation is a JSON claim file
// present at `state/machine-claims/<machine>/<run-id>.json`. The
// file's presence is the authoritative "slot taken" signal.
//
// Reservation/publish atomicity: the canonical `ReserveAndPublish`
// entry point holds the in-process guard and the on-disk lock from
// the initial reap through the caller's publish callback. If the
// callback fails, the transaction removes the exact claim it just
// wrote before returning. The `Reserve`/`Release`/`Status`/`Reap`
// entry points share one lock helper so no other operation can
// observe a half-completed transaction.
//
// Post-publish invariant: a claim with a missing or terminal record
// is orphan state. The reaper may safely remove it during a normal
// maintenance sweep. New public build paths therefore publish the
// run record (in `staging` state) inside the reservation transaction
// and never leave a freshly-reserved run id without a record.
package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lock"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// ErrNoCapacity reports that no eligible attached machine has an
// available slot. The caller treats it as a clean admission failure:
// no run ID is returned, no run record is published, no claim is
// created.
var ErrNoCapacity = errors.New("scheduler: no available capacity on any eligible machine")

// ClaimSchemaVersion is the durable claim metadata version. Bumping
// it invalidates in-flight claims of older shape; the reaper releases
// only claims matching the current schema.
const ClaimSchemaVersion = 1

// Claim is the durable reservation record persisted under
// state/machine-claims/<machine>/<run-id>.json.
type Claim struct {
	SchemaVersion int       `json:"schema_version"`
	Machine       string    `json:"machine"`
	RunID         string    `json:"run_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// MachineStatus is one machine's capacity/availability snapshot.
type MachineStatus struct {
	Machine   string
	Capacity  int
	Active    int
	Available int
}

// recordLoader is the minimum interface the scheduler needs from the
// run store: it loads a record and returns its state. Missing
// records are reported as errors; the scheduler treats them as a
// hint that an orphan claim may be safely reaped.
type recordLoader interface {
	Load(id model.RunID) (store.RunRecord, error)
}

// runRecordState classifies a run record relative to its claim.
type runRecordState int

const (
	runStateActive runRecordState = iota
	runStateTerminal
	runStateMissing
)

// claimInfo describes one claim file observed on disk.
type claimInfo struct {
	machine   string
	runID     model.RunID
	createdAt time.Time
}

// enumerateClaims walks the claim root and returns every claim file
// whose contents validate. The list is produced by the validator so a
// corrupt or out-of-band file is surfaced as a hard error: the
// scheduler must fail closed when claim state is unreadable, not
// silently treat it as free capacity. The file is left on disk for the
// operator. Must be called under the scheduler lock.
func enumerateClaims(claimRoot string) ([]claimInfo, error) {
	machines, err := os.ReadDir(claimRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []claimInfo
	for _, m := range machines {
		if !m.IsDir() || !layout.ValidName(m.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(claimRoot, m.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			runID := model.RunID(strings.TrimSuffix(f.Name(), ".json"))
			if !model.ValidRunID(runID) {
				continue
			}
			path := filepath.Join(claimRoot, m.Name(), f.Name())
			validated, verr := validateClaimFile(path, m.Name(), runID)
			if verr != nil {
				return nil, fmt.Errorf("scheduler: corrupt claim %s: %w", path, verr)
			}
			out = append(out, claimInfo{machine: m.Name(), runID: runID, createdAt: validated.CreatedAt})
		}
	}
	return out, nil
}

// Reservation is the validated state of one (machine, runID) claim
// file. The CreatedAt is the durable creation time the reaper and
// the worker compare against.
type Reservation struct {
	Machine   string
	RunID     model.RunID
	CreatedAt time.Time
}

// validateClaimFile is the single source of truth for what counts as
// a usable claim. It uses Lstat (so symlinks are rejected rather
// than followed), requires a regular file, decodes JSON, and verifies
// the schema version, the directory machine, the filename run id, the
// payload machine and run id, and a nonzero UTC created_at. A
// failure here means the file is corrupt or has been tampered with;
// it is never silently counted as free capacity.
func validateClaimFile(path, machine string, runID model.RunID) (Reservation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Reservation{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Reservation{}, fmt.Errorf("claim %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return Reservation{}, fmt.Errorf("claim %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return Reservation{}, fmt.Errorf("claim %s is empty", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Reservation{}, err
	}
	var c Claim
	if err := json.Unmarshal(data, &c); err != nil {
		return Reservation{}, fmt.Errorf("claim %s has invalid JSON: %w", path, err)
	}
	if c.SchemaVersion != ClaimSchemaVersion {
		return Reservation{}, fmt.Errorf("claim %s has schema version %d, want %d", path, c.SchemaVersion, ClaimSchemaVersion)
	}
	if !layout.ValidName(c.Machine) || c.Machine != machine {
		return Reservation{}, fmt.Errorf("claim %s has machine %q, want %q", path, c.Machine, machine)
	}
	if !model.ValidRunID(model.RunID(c.RunID)) || c.RunID != string(runID) {
		return Reservation{}, fmt.Errorf("claim %s has run id %q, want %q", path, c.RunID, runID)
	}
	if c.CreatedAt.IsZero() {
		return Reservation{}, fmt.Errorf("claim %s has zero created_at", path)
	}
	createdAt := c.CreatedAt.UTC()
	// A future-dated created_at is a corrupt-state error: clock skew
	// must not grant an unbounded grace window. Reject strictly so the
	// reaper and the worker can both rely on a bounded comparison.
	if createdAt.After(time.Now().UTC().Add(time.Minute)) {
		return Reservation{}, fmt.Errorf("claim %s has future created_at %s", path, createdAt.Format(time.RFC3339Nano))
	}
	return Reservation{Machine: c.Machine, RunID: model.RunID(c.RunID), CreatedAt: createdAt}, nil
}

// countActiveByMachine returns the per-machine count of valid claim
// files. Corrupt or out-of-band claim files are reported as a hard
// error: the scheduler must fail closed when claim state is
// unreadable, not silently treat it as free capacity. The file is
// left on disk for the operator.
func countActiveByMachine(l layout.Layout, claimRoot string) (map[string]int, error) {
	machines, err := os.ReadDir(claimRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]int{}, nil
		}
		return nil, err
	}
	counts := map[string]int{}
	for _, m := range machines {
		if !m.IsDir() || !layout.ValidName(m.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(claimRoot, m.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			runID := model.RunID(strings.TrimSuffix(f.Name(), ".json"))
			if !model.ValidRunID(runID) {
				continue
			}
			path := filepath.Join(claimRoot, m.Name(), f.Name())
			if _, verr := validateClaimFile(path, m.Name(), runID); verr != nil {
				return nil, fmt.Errorf("scheduler: corrupt claim %s: %w", path, verr)
			}
			counts[m.Name()]++
		}
	}
	return counts, nil
}

func isTerminal(state model.RunState) bool {
	return state == model.RunSucceeded || state == model.RunFailed || state == model.RunLost || state == model.RunAborted
}

// reapClaims removes claim files whose run record is missing or
// terminal. A missing record is orphan state: the new build path
// publishes the record inside the reservation transaction, so a
// claim that survived without a record is safe to reap. A corrupt or
// unreadable record is retained on disk and reported as an error so
// the operator can decide whether to repair it. Must be called under
// the scheduler lock.
func reapClaims(l layout.Layout, claimRoot string, loader recordLoader) (removed int, err error) {
	claims, err := enumerateClaims(claimRoot)
	if err != nil {
		return 0, err
	}
	for _, c := range claims {
		rec, loadErr := loader.Load(c.runID)
		if loadErr != nil {
			if !errors.Is(loadErr, os.ErrNotExist) {
				return removed, fmt.Errorf("scheduler: reap %s/%s: %w", c.machine, c.runID, loadErr)
			}
			// Missing record is orphan state; remove the exact claim.
			path := l.MachineClaimPath(c.machine, c.runID)
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return removed, rmErr
			}
			removed++
			continue
		}
		if !isTerminal(rec.State) {
			continue
		}
		path := l.MachineClaimPath(c.machine, c.runID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// machineSnapshot is the per-machine view used by the chooser.
type machineSnapshot struct {
	name     string
	capacity int
	active   int
}

// computeMachineSnapshots returns a deterministic (sorted) list of
// per-machine capacity and active-claim counts. The list is sorted
// by active/capacity ratio (cross-multiplied to avoid floats), then
// by machine name.
func computeMachineSnapshots(active map[string]int, cfg config.Config) []machineSnapshot {
	out := make([]machineSnapshot, 0, len(cfg.Machines))
	for name, machine := range cfg.Machines {
		out = append(out, machineSnapshot{name: name, capacity: config.EffectiveCapacity(machine), active: active[name]})
	}
	sort.Slice(out, func(i, j int) bool {
		// Compare active[i]/capacity[i] < active[j]/capacity[j]
		// by cross-multiplication: active[i]*capacity[j] < active[j]*capacity[i].
		li, ri := out[i].active*out[j].capacity, out[j].active*out[i].capacity
		if li != ri {
			return li < ri
		}
		return out[i].name < out[j].name
	})
	return out
}

// writeClaim publishes a claim file atomically and refuses to
// overwrite an existing exact claim. Publication is O_EXCL+Sync+rename
// to a unique same-directory temporary file followed by a parent
// directory sync, so a concurrent reader never observes a partial
// claim and a crash cannot leave a half-written finalized path on
// disk. The target goes from absent to complete in one observer-
// visible cycle. On any error the temporary file is removed so failure
// cannot leave scratch behind.
func writeClaim(l layout.Layout, machine string, claim Claim) error {
	return atomicWriteClaim(l, machine, claim)
}

// atomicWriteClaim is the implementation behind writeClaim. It writes
// to a unique temporary file in the same directory, fsyncs the file,
// then renames into place, refusing to overwrite an existing target.
// The parent directory is fsynced to durably publish the rename.
func atomicWriteClaim(l layout.Layout, machine string, claim Claim) error {
	machineDir := filepath.Join(l.MachineClaimsDir(), machine)
	if err := os.MkdirAll(machineDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(machineDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(&claim)
	if err != nil {
		return err
	}
	target := l.MachineClaimPath(machine, model.RunID(claim.RunID))
	tmp, err := os.CreateTemp(machineDir, ".vci-claim-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		if os.IsExist(err) {
			return fmt.Errorf("claim %s already exists", target)
		}
		return err
	}
	parent, err := os.Open(machineDir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

// schedulerGuard serializes the inspect/sweep/select/claim/write
// sequence in-process so two concurrent Reserve calls on the same
// process cannot both observe the same "available" slot. The
// on-disk scheduler.lock serializes across processes.
var schedulerGuard sync.Mutex

// withLock runs fn under the in-process guard and the on-disk
// scheduler lock. Lock order is fixed and documented: the in-process
// Go mutex (schedulerGuard) is always taken first, then the on-disk
// flock. Every protected operation MUST use this helper so the lock
// order can never invert; doing otherwise risks deadlock or a
// half-observed transaction.
func withLock(l layout.Layout, fn func() error) error {
	schedulerGuard.Lock()
	defer schedulerGuard.Unlock()
	unlock, err := lock.Acquire(l.SchedulerLockPath())
	if err != nil {
		return fmt.Errorf("scheduler: lock: %w", err)
	}
	defer unlock()
	return fn()
}

// validateRunAndCandidates pins the cross-cutting input contract
// shared by every reservation entry point.
func validateRunAndCandidates(cfg config.Config, runID model.RunID, candidates []string) error {
	if !model.ValidRunID(runID) {
		return fmt.Errorf("scheduler: invalid run id %q", runID)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("scheduler: no candidate machines")
	}
	for _, name := range candidates {
		if _, ok := cfg.Machines[name]; !ok {
			return fmt.Errorf("scheduler: candidate machine %q is not in the inventory", name)
		}
	}
	return nil
}

// chooseSlot returns the chosen machine for a fresh reservation, or
// ErrNoCapacity when every candidate is at capacity. The function is
// pure: it does not read or write disk. Must be called under withLock.
func chooseSlot(active map[string]int, cfg config.Config, candidates []string) (machineSnapshot, error) {
	snaps := computeMachineSnapshots(active, cfg)
	candidateSet := map[string]bool{}
	for _, c := range candidates {
		candidateSet[c] = true
	}
	for _, snap := range snaps {
		if !candidateSet[snap.name] {
			continue
		}
		if snap.active >= snap.capacity {
			continue
		}
		return snap, nil
	}
	return machineSnapshot{}, ErrNoCapacity
}

// ReserveAndPublish holds the scheduler lock from observe-through-
// publish so a crashed caller cannot leave an orphan claim on disk.
// The call sequence is:
//
//  1. validate inputs and acquire the in-process guard + on-disk lock;
//  2. reap missing-record / terminal-record claims;
//  3. choose one eligible machine with available capacity;
//  4. write the durable claim;
//  5. invoke publish(machine) to persist the run record (typically
//     in `staging` state) under the same machine;
//  6. if publish fails, remove only the exact claim just written
//     before returning the error to the caller.
//
// The publish callback must persist a run record before returning.
// Without a real record, the claim is orphan state and the very next
// sweep would reap it; this is the only safe shape. The callback
// must not call scheduler APIs or wait for processes. ErrNoCapacity
// is returned when no candidate has a free slot; no claim is written.
func ReserveAndPublish(l layout.Layout, loader recordLoader, cfg config.Config, runID model.RunID, candidates []string, now time.Time, publish func(machine string) error) error {
	if err := validateRunAndCandidates(cfg, runID, candidates); err != nil {
		return err
	}
	now = now.UTC()
	// A future-dated now would publish a claim whose created_at the
	// validator rejects on the next read; reject up-front so the
	// caller can fix the clock instead of writing a doomed claim.
	if now.After(time.Now().UTC().Add(time.Minute)) {
		return fmt.Errorf("scheduler: now is in the future; refused to publish a doomed claim")
	}
	return withLock(l, func() error {
		claimRoot := l.MachineClaimsDir()
		if _, err := reapClaims(l, claimRoot, loader); err != nil {
			return fmt.Errorf("scheduler: reap claims: %w", err)
		}
		active, err := countActiveByMachine(l, claimRoot)
		if err != nil {
			return fmt.Errorf("scheduler: count active: %w", err)
		}
		snap, err := chooseSlot(active, cfg, candidates)
		if err != nil {
			return err
		}
		claim := Claim{SchemaVersion: ClaimSchemaVersion, Machine: snap.name, RunID: string(runID), CreatedAt: now}
		if err := writeClaim(l, snap.name, claim); err != nil {
			return fmt.Errorf("scheduler: write claim: %w", err)
		}
		if err := publish(snap.name); err != nil {
			// Roll back the exact claim just written. This is the
			// only path that mutates the claim tree from inside the
			// transaction; every other release goes through Release.
			if rmErr := os.Remove(l.MachineClaimPath(snap.name, runID)); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("scheduler: publish failed and rollback failed: publish=%w rollback=%v", err, rmErr)
			}
			return err
		}
		return nil
	})
}

// Release removes the exact (machine, runID) claim. The exact claim
// body is validated before removal: a missing claim is idempotent
// (no-op), a corrupt or mismatched claim is an error so the operator
// can decide whether to repair it. Remove race is decided by the
// scheduler lock; the file is removed only after the body has been
// read and validated.
func Release(l layout.Layout, machine string, runID model.RunID) error {
	if !layout.ValidName(machine) {
		return fmt.Errorf("scheduler: invalid machine name %q", machine)
	}
	if !model.ValidRunID(runID) {
		return fmt.Errorf("scheduler: invalid run id %q", runID)
	}
	return withLock(l, func() error {
		path := l.MachineClaimPath(machine, runID)
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if _, verr := validateClaimFile(path, machine, runID); verr != nil {
			return fmt.Errorf("scheduler: refuse release of invalid claim: %w", verr)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// ReservationFor returns the validated reservation for (machine,
// runID). The worker uses it before lease.Claim to prove the slot is
// reserved through the scheduler, not through a raw filesystem check.
// A missing claim surfaces as os.ErrNotExist; a corrupt claim surfaces
// as a wrapped error.
func ReservationFor(l layout.Layout, machine string, runID model.RunID) (Reservation, error) {
	if !layout.ValidName(machine) {
		return Reservation{}, fmt.Errorf("scheduler: invalid machine name %q", machine)
	}
	if !model.ValidRunID(runID) {
		return Reservation{}, fmt.Errorf("scheduler: invalid run id %q", runID)
	}
	var res Reservation
	err := withLock(l, func() error {
		validated, verr := validateClaimFile(l.MachineClaimPath(machine, runID), machine, runID)
		if verr != nil {
			return verr
		}
		res = validated
		return nil
	})
	return res, err
}

// Status returns a per-machine capacity/availability snapshot. It
// is informational only; it does not count as a reservation.
func Status(l layout.Layout, loader recordLoader, cfg config.Config) ([]MachineStatus, error) {
	var out []MachineStatus
	err := withLock(l, func() error {
		claimRoot := l.MachineClaimsDir()
		if _, err := reapClaims(l, claimRoot, loader); err != nil {
			return fmt.Errorf("scheduler: reap claims: %w", err)
		}
		active, err := countActiveByMachine(l, claimRoot)
		if err != nil {
			return err
		}
		snaps := computeMachineSnapshots(active, cfg)
		out = make([]MachineStatus, 0, len(snaps))
		for _, snap := range snaps {
			out = append(out, MachineStatus{Machine: snap.name, Capacity: snap.capacity, Active: snap.active, Available: maxInt(0, snap.capacity-snap.active)})
		}
		return nil
	})
	return out, err
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Reap walks the claim tree, releases claims whose run record is
// missing or terminal, and returns the count of removed claims. It
// never signals a process and never infers liveness from claim age;
// only the run record state decides.
func Reap(l layout.Layout, loader recordLoader) (int, error) {
	var removed int
	err := withLock(l, func() error {
		var innerErr error
		removed, innerErr = reapClaims(l, l.MachineClaimsDir(), loader)
		return innerErr
	})
	return removed, err
}
