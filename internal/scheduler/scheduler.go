// Package scheduler manages local machine slot reservations.
// It atomically reserves a claim and publishes the run record for one machine.
// Claims are stored as local state/machine-claims files; there is no remote queue.
package scheduler

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// ErrNoCapacity means no eligible machine has an available slot.
// The caller treats it as clean admission failure: no run, no publish, no claim.
var ErrNoCapacity = errors.New("scheduler: no available capacity on any eligible machine")

// MachineStatus is one machine's capacity/availability snapshot.
type MachineStatus struct {
	Machine   string
	Capacity  int
	Active    int
	Available int
}

// recordLoader reads run records for scheduler state checks.
// Missing records are treated as orphan-claim candidates.
type recordLoader interface {
	Load(id model.RunID) (store.RunRecord, error)
}

// Reservation represents a validated claim file for (machine, runID).
// Workers compare CreatedAt against reaper and run records.
type Reservation struct {
	Machine   string
	RunID     model.RunID
	CreatedAt time.Time
}

// machineSnapshot is the per-machine view used by the chooser.
type machineSnapshot struct {
	name     string
	capacity int
	active   int
}

// computeMachineSnapshots returns machines sorted by active-to-capacity ratio,
// then by name for ties.
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

// schedulerGuard serializes reservation operations within this process.
// The on-disk lock handles cross-process serialization.
var schedulerGuard sync.Mutex

// withLock runs fn under process and on-disk locks.
// Lock order is fixed to avoid deadlocks: process mutex then flock.
func withLock(l model.Layout, fn func() error) error {
	schedulerGuard.Lock()
	defer schedulerGuard.Unlock()
	unlock, err := store.Acquire(l.SchedulerLockPath())
	if err != nil {
		return fmt.Errorf("scheduler: lock: %w", err)
	}
	defer unlock()
	return fn()
}

// validateRunAndCandidates checks run IDs and candidate machine names.
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

// chooseSlot picks one candidate machine with free capacity.
// It is pure and must run under withLock.
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

// ReserveAndPublish runs the reserve->publish flow under a single lock.
// It reaps stale claims, selects a machine, writes the claim,
// publishes run state, and rolls back the claim on publish failure.
// Publish must create a non-terminal run record or the claim is treated as orphan.
func ReserveAndPublish(l model.Layout, loader recordLoader, cfg config.Config, runID model.RunID, candidates []string, now time.Time, publish func(machine string) error) error {
	if err := validateRunAndCandidates(cfg, runID, candidates); err != nil {
		return err
	}
	now = now.UTC()
	// Reject future timestamps before writing a claim that would fail
	// validation later.
	if now.After(time.Now().UTC().Add(time.Minute)) {
		return fmt.Errorf("scheduler: now is in the future; refused to publish a doomed claim")
	}
	return withLock(l, func() error {
		claimRoot := l.MachineClaimsDir()
		if _, err := reapClaims(l, claimRoot, loader); err != nil {
			return fmt.Errorf("scheduler: reap claims: %w", err)
		}
		active, err := countActiveByMachine(claimRoot)
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
			// Roll back the claim written in this transaction.
			// This is the only in-transaction mutation path.
			if rmErr := os.Remove(l.MachineClaimPath(snap.name, runID)); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("scheduler: publish failed and rollback failed: publish=%w rollback=%v", err, rmErr)
			}
			return err
		}
		return nil
	})
}

// Release removes the exact (machine, runID) claim.
// The file must be validated before deletion.
// Missing claims are ignored; invalid claims fail fast.
// Deletion runs under scheduler lock.
func Release(l model.Layout, machine string, runID model.RunID) error {
	if !model.ValidName(machine) {
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

// ReservationFor returns the validated reservation for (machine, runID).
// Workers use it to verify scheduler reservation before leasing.
// Missing or corrupt claims return os.ErrNotExist or a wrapped error.
func ReservationFor(l model.Layout, machine string, runID model.RunID) (Reservation, error) {
	if !model.ValidName(machine) {
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

// Status returns per-machine capacity/availability snapshots.
// It is informational only and does not reserve capacity.
func Status(l model.Layout, loader recordLoader, cfg config.Config) ([]MachineStatus, error) {
	var out []MachineStatus
	err := withLock(l, func() error {
		claimRoot := l.MachineClaimsDir()
		if _, err := reapClaims(l, claimRoot, loader); err != nil {
			return fmt.Errorf("scheduler: reap claims: %w", err)
		}
		active, err := countActiveByMachine(claimRoot)
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

// Reap scans claims, removes those with missing or terminal run records,
// returns the number removed, and uses only run record state for liveness.
func Reap(l model.Layout, loader recordLoader) (int, error) {
	var removed int
	err := withLock(l, func() error {
		var innerErr error
		removed, innerErr = reapClaims(l, l.MachineClaimsDir(), loader)
		return innerErr
	})
	return removed, err
}
