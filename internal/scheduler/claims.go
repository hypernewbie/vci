package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

// ClaimSchemaVersion is the claim record format version.
// Bumping it invalidates older claim files; the reaper reads only
// this version.
const ClaimSchemaVersion = 1

// Claim is a durable reservation record persisted under
// state/machine-claims/<machine>/<run-id>.json.
type Claim struct {
	SchemaVersion int       `json:"schema_version"`
	Machine       string    `json:"machine"`
	RunID         string    `json:"run_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// runRecordState tracks a run record's state relative to its claim.
type runRecordState int

const (
	runStateActive runRecordState = iota
	runStateTerminal
	runStateMissing
)

// claimInfo holds one validated claim file.
type claimInfo struct {
	machine   string
	runID     model.RunID
	createdAt time.Time
}

// enumerateClaims walks the claim root and returns validated claim files.
// Corrupt or unreadable claim files are hard errors and are not counted as
// free capacity. Corrupt files remain on disk for operator repair.
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
		if !m.IsDir() || !model.ValidName(m.Name()) {
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

// validateClaimFile validates a claim for scheduler use.
// It rejects symlinks, non-regular files, invalid JSON/schema,
// machine/runID mismatches, and bad created_at values.
// Failed validation marks the claim corrupt and excludes it from free-capacity accounting.
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
	if !model.ValidName(c.Machine) || c.Machine != machine {
		return Reservation{}, fmt.Errorf("claim %s has machine %q, want %q", path, c.Machine, machine)
	}
	if !model.ValidRunID(model.RunID(c.RunID)) || c.RunID != string(runID) {
		return Reservation{}, fmt.Errorf("claim %s has run id %q, want %q", path, c.RunID, runID)
	}
	if c.CreatedAt.IsZero() {
		return Reservation{}, fmt.Errorf("claim %s has zero created_at", path)
	}
	createdAt := c.CreatedAt.UTC()
	// Future created_at values are invalid except for 1m clock skew.
	if createdAt.After(time.Now().UTC().Add(time.Minute)) {
		return Reservation{}, fmt.Errorf("claim %s has future created_at %s", path, createdAt.Format(time.RFC3339Nano))
	}
	return Reservation{Machine: c.Machine, RunID: model.RunID(c.RunID), CreatedAt: createdAt}, nil
}

// countActiveByMachine returns per-machine counts of validated claim files.
// Corrupt or out-of-band files are hard errors and do not count as
// free capacity.
func countActiveByMachine(claimRoot string) (map[string]int, error) {
	claims, err := enumerateClaims(claimRoot)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.machine]++
	}
	return counts, nil
}

// reapClaims removes claims whose run record is missing or terminal.
// Missing records indicate orphaned claims and are removed.
// Corrupt or unreadable records are returned as errors for operator
// repair.
func reapClaims(l model.Layout, claimRoot string, loader recordLoader) (removed int, err error) {
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
			// Missing record means orphaned claim; remove it.
			path := l.MachineClaimPath(c.machine, c.runID)
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return removed, rmErr
			}
			removed++
			continue
		}
		if !model.IsTerminal(rec.State) {
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

// writeClaim persists a claim file atomically and refuses overwrite.
// It writes via same-directory temp+rename plus fsyncs; partial files are
// cleaned up on failure.
func writeClaim(l model.Layout, machine string, claim Claim) error {
	return atomicWriteClaim(l, machine, claim)
}

// atomicWriteClaim writes to a unique temp file in the same directory,
// syncs it, renames it atomically, and syncs the parent directory.
func atomicWriteClaim(l model.Layout, machine string, claim Claim) error {
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
	if err := parent.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}
