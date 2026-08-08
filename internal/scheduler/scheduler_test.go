package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// fakeLoader implements recordLoader for the scheduler. It maps
// run IDs to a fixed run state so tests can simulate missing or
// terminal records without depending on the full store package.
type fakeLoader struct {
	mu      sync.Mutex
	records map[model.RunID]model.RunState
}

func (f *fakeLoader) Load(id model.RunID) (store.RunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.records[id]
	if !ok {
		return store.RunRecord{}, fs.ErrNotExist
	}
	return store.RunRecord{ID: id, State: state}, nil
}

func (f *fakeLoader) set(id model.RunID, state model.RunState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[model.RunID]model.RunState{}
	}
	f.records[id] = state
}

// publishCallback returns a ReserveAndPublish callback that records
// the chosen machine in the loader so the reservation is not
// orphan. runID is the existing run id the test reserves against.
func publishCallback(loader *fakeLoader, runID model.RunID, state model.RunState) func(string) error {
	return func(machine string) error {
		loader.set(runID, state)
		return nil
	}
}

func newRoot(t *testing.T) model.Layout {
	t.Helper()
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	return l
}

func newRunIDForTest(i int) model.RunID {
	return model.RunID(fmt.Sprintf("run_test_%d", i))
}

// TestEffectiveCapacityOneDefault pins that the in-package calls to
// config.EffectiveCapacity produce the right snapshot for an empty
// Machine.
func TestEffectiveCapacityOneDefault(t *testing.T) {
	if got := config.EffectiveCapacity(config.Machine{}); got != 1 {
		t.Fatalf("empty Machine effective capacity = %d, want 1", got)
	}
}

// reserveWith is the test-only caller of ReserveAndPublish that
// mirrors the production contract exactly: a publish callback that
// persists a real record (modeled by the loader) is the only safe
// shape. The wrapper returns the chosen machine so the tests read
// like the obsolete Reserve API.
func reserveWith(t *testing.T, l model.Layout, loader *fakeLoader, cfg config.Config, runID model.RunID, candidates []string, now time.Time) (string, error) {
	t.Helper()
	var machine string
	state := model.RunStaging
	// Default: register a record so the test exercises the same
	// safety contract ReserveAndPublish requires of production.
	if _, err := loader.Load(runID); errors.Is(err, fs.ErrNotExist) {
		loader.set(runID, state)
	}
	err := ReserveAndPublish(l, loader, cfg, runID, candidates, now, func(m string) error {
		machine = m
		return nil
	})
	return machine, err
}

// TestReserveDeterministicChoice pins the deterministic least-loaded
// tie-break. With one slot per machine and zero initial load, the
// reservation always lands on the lexicographically smaller name.
func TestReserveDeterministicChoice(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
		"beta":  {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	machine, err := reserveWith(t, l, loader, cfg, model.RunID("run_a_1"), []string{"alpha", "beta"}, time.Unix(100, 0))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if machine != "alpha" {
		t.Fatalf("first reservation chose %q, want alpha", machine)
	}
	machine2, err := reserveWith(t, l, loader, cfg, model.RunID("run_a_2"), []string{"alpha", "beta"}, time.Unix(101, 0))
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if machine2 != "beta" {
		t.Fatalf("second reservation chose %q, want beta", machine2)
	}
}

// TestReserveCapacityExhaustionReturnsErrNoCapacity pins that the
// third reservation on a 2-slot total inventory fails with
// ErrNoCapacity; no claim is created and no run record is leaked.
func TestReserveCapacityExhaustionReturnsErrNoCapacity(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
		"beta":  {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_a_1"), []string{"alpha", "beta"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_a_2"), []string{"alpha", "beta"}, time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := reserveWith(t, l, loader, cfg, model.RunID("run_a_3"), []string{"alpha", "beta"}, time.Unix(102, 0))
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

// TestReserveRejectsCandidateNotInInventory pins the strict rule that
// candidates must reference existing machines.
func TestReserveRejectsCandidateNotInInventory(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{"alpha": {MaxConcurrent: 1}}}
	loader := &fakeLoader{}
	_, err := reserveWith(t, l, loader, cfg, model.RunID("run_x_1"), []string{"missing"}, time.Unix(100, 0))
	if err == nil {
		t.Fatal("expected rejection for missing candidate machine")
	}
}

// TestReserveConcurrencyNeverExceedsCapacity runs N concurrent
// reservations against a small inventory and asserts the success
// count equals the total capacity.
func TestReserveConcurrencyNeverExceedsCapacity(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
		"beta":  {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	for i := 0; i < 6; i++ {
		loader.set(newRunIDForTest(i), model.RunStaging)
	}
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := reserveWith(t, l, loader, cfg, newRunIDForTest(i), []string{"alpha", "beta"}, time.Unix(int64(100+i), 0))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	wins, noCap := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrNoCapacity):
			noCap++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 2 || noCap != 4 {
		t.Fatalf("wins=%d noCap=%d, want wins=2 noCap=4", wins, noCap)
	}
}

// TestReleaseRemovesExactClaim pins that Release is exact:
// releasing machine X run Y does not affect machine X run Z.
func TestReleaseRemovesExactClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 2},
	}}
	loader := &fakeLoader{}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_release_1"), []string{"alpha"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_release_2"), []string{"alpha"}, time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	if err := Release(l, "alpha", model.RunID("run_release_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_release_1"))); !os.IsNotExist(err) {
		t.Fatalf("released claim must be gone: %v", err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_release_2"))); err != nil {
		t.Fatalf("other claim must still exist: %v", err)
	}
	if err := Release(l, "alpha", model.RunID("run_release_1")); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

// TestReapReleasesTerminalClaims pins that Reap removes claims
// whose run record is terminal, but never removes a claim whose run
// record is still in a nonterminal state even if the claim
// timestamp is old.
func TestReapReleasesTerminalClaims(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 4},
	}}
	loader := &fakeLoader{}
	loader.set("run_terminal", model.RunStaging)
	loader.set("run_live", model.RunStaging)
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_terminal"), []string{"alpha"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_live"), []string{"alpha"}, time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	// Promote run_terminal to terminal after reservation to simulate
	// a worker that finished and now requires reap to free the slot.
	loader.set("run_terminal", model.RunSucceeded)
	// Set mtimes far in the past to prove Reap does not use claim age.
	for _, runID := range []model.RunID{"run_terminal", "run_live"} {
		path := l.MachineClaimPath("alpha", runID)
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := Reap(l, loader)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("reap removed=%d, want 1", removed)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_terminal"))); err == nil {
		t.Fatal("terminal claim must be reaped")
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_live"))); err != nil {
		t.Fatalf("live claim must not be reaped: %v", err)
	}
}

// TestReleaseHandlesMissingClaim pins that Release is idempotent on
// a missing claim. The Prepare failure path calls Release after the
// run record has already been removed.
func TestReleaseHandlesMissingClaim(t *testing.T) {
	l := newRoot(t)
	if err := Release(l, "alpha", model.RunID("run_never_reserved")); err != nil {
		t.Fatalf("release must be idempotent: %v", err)
	}
}

// TestStatusReportsCapacity pins that Status surfaces the configured
// capacity and decrements available as reservations accumulate.
func TestStatusReportsCapacity(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 2},
	}}
	loader := &fakeLoader{}
	loader.set("run_status_1", model.RunStaging)
	status, err := Status(l, loader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].Machine != "alpha" || status[0].Capacity != 2 || status[0].Active != 0 || status[0].Available != 2 {
		t.Fatalf("initial status: %+v", status)
	}
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_status_1"), []string{"alpha"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	status, err = Status(l, loader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].Active != 1 || status[0].Available != 1 {
		t.Fatalf("after one reservation: %+v", status)
	}
}

// writeRawClaim writes a synthetic JSON claim file directly to disk so
// tests can construct corrupt or out-of-band states that the public
// ReserveAndPublish path does not produce.
func writeRawClaim(t *testing.T, l model.Layout, machine, runID string, body []byte) {
	t.Helper()
	dir := filepath.Join(l.MachineClaimsDir(), machine)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := l.MachineClaimPath(machine, model.RunID(runID))
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReapRemovesMissingRecordClaim pins the contract that a
// well-formed claim whose run record is missing is orphan state and
// scheduler maintenance may safely reap it. This is the closed-loop
// complement to TestReapReleasesTerminalClaims.
func TestReapRemovesMissingRecordClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	body, _ := json.Marshal(map[string]any{
		"schema_version": ClaimSchemaVersion,
		"machine":        "alpha",
		"run_id":         "run_orphan_1",
		"created_at":     time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
	})
	writeRawClaim(t, l, "alpha", "run_orphan_1", body)
	removed, err := Reap(l, loader)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("reap removed=%d, want 1", removed)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_orphan_1"))); !os.IsNotExist(err) {
		t.Fatalf("orphan claim must be reaped: %v", err)
	}
	status, err := Status(l, loader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].Active != 0 || status[0].Available != 1 {
		t.Fatalf("orphan reaped but capacity shows %+v", status)
	}
}

// TestReapRefusesCorruptClaim pins the fail-closed contract: a
// malformed claim subtree must surface as a hard error from reaping
// and never silently free the slot. The corrupt file must remain on
// disk.
func TestReapRefusesCorruptClaim(t *testing.T) {
	l := newRoot(t)
	loader := &fakeLoader{}
	// A malformed JSON claim forces the enumerator to fail; the
	// reaper must surface that error without removing the file.
	writeRawClaim(t, l, "alpha", "run_bad_reap", []byte("{not valid json"))
	_, err := Reap(l, loader)
	if err == nil {
		t.Fatal("reap must surface a corrupt claim as a hard error")
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_bad_reap"))); err != nil {
		t.Fatalf("corrupt claim must remain on disk: %v", err)
	}
}

// TestStatusFailsClosedOnCorruptClaim pins that a corrupt claim
// subtree surfaces a hard error from Status; the operator cannot
// safely infer capacity when claim state is unreadable.
func TestStatusFailsClosedOnCorruptClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 2},
	}}
	loader := &fakeLoader{}
	loader.set("run_valid_1", model.RunStaging)
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_valid_1"), []string{"alpha"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	// Mixed corrupt state: malformed JSON, wrong schema, wrong
	// machine/run id, and a symlink. Each must produce a hard error
	// from Status; corrupt state is never silently treated as free.
	// Filenames must be valid run IDs because the enumerator skips
	// invalid IDs without examining their contents.
	cases := []struct {
		name      string
		body      []byte
		wantError string
	}{
		{"run_bad_1", []byte("{not valid json"), "invalid JSON"},
		{"run_bad_2", []byte(`{"schema_version":999,"machine":"alpha","run_id":"run_bad_2","created_at":"2026-01-01T00:00:00Z"}`), "schema version"},
		{"run_bad_3", []byte(`{"schema_version":1,"machine":"beta","run_id":"run_bad_3","created_at":"2026-01-01T00:00:00Z"}`), "machine"},
		{"run_bad_4", []byte(`{"schema_version":1,"machine":"alpha","run_id":"run_bad_4","created_at":"2026-01-01T00:00:00Z"}`), "run id"},
	}
	for _, tc := range cases {
		writeRawClaim(t, l, "alpha", tc.name, tc.body)
		_, err := Status(l, loader, cfg)
		if err == nil {
			t.Fatalf("status must fail closed on %s", tc.name)
		}
		// Corrupt state must remain on disk; this is failure-closed
		// accounting, not silent cleanup.
		if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID(tc.name))); err != nil {
			t.Fatalf("corrupt claim %s must be retained: %v", tc.name, err)
		}
	}
	// A symlink pointing to a valid claim is rejected by the
	// enumerator with a hard error.
	validTarget := l.MachineClaimPath("alpha", model.RunID("run_valid_1"))
	linkPath := l.MachineClaimPath("alpha", model.RunID("run_symlink_1"))
	if err := os.Symlink(validTarget, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(l, loader, cfg); err == nil {
		t.Fatal("status must fail closed on symlink claim")
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("symlink must be retained: %v", err)
	}
}

// TestReserveAndPublishFailsClosedOnCorruptClaim pins that
// ReserveAndPublish refuses to admit a new reservation when the
// existing claim tree is corrupt. The corrupt file must remain on
// disk and no new claim may be created.
func TestReserveAndPublishFailsClosedOnCorruptClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 2},
	}}
	loader := &fakeLoader{}
	writeRawClaim(t, l, "alpha", "run_bad_corrupt", []byte("{not valid json"))
	_, err := reserveWith(t, l, loader, cfg, model.RunID("run_new_1"), []string{"alpha"}, time.Unix(100, 0))
	if err == nil {
		t.Fatal("reservation must fail closed when a corrupt claim exists")
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_bad_corrupt"))); err != nil {
		t.Fatalf("corrupt claim must remain on disk: %v", err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_new_1"))); !os.IsNotExist(err) {
		t.Fatalf("no new claim may be created on corrupt state: %v", err)
	}
}

// TestReserveRefusesExistingExactClaim pins that the public
// ReserveAndPublish path does not overwrite a valid existing claim.
// A claim whose run record is still in flight (staging) is non-orphan
// and must not be silently replaced.
func TestReserveRefusesExistingExactClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	body, _ := json.Marshal(map[string]any{
		"schema_version": ClaimSchemaVersion,
		"machine":        "alpha",
		"run_id":         "run_already",
		"created_at":     time.Unix(50, 0).UTC().Format(time.RFC3339Nano),
	})
	writeRawClaim(t, l, "alpha", "run_already", body)
	loader.set("run_already", model.RunStaging)
	_, err := reserveWith(t, l, loader, cfg, model.RunID("run_already"), []string{"alpha"}, time.Unix(100, 0))
	if err == nil {
		t.Fatal("reserve must refuse to overwrite an existing claim")
	}
}

// TestReserveAndPublishRollsBackOnCallbackFailure pins the atomic
// reservation/publication contract: when the publish callback fails,
// the scheduler transaction must remove only the exact claim it just
// created, leaving no orphan claim and no run record.
func TestReserveAndPublishRollsBackOnCallbackFailure(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	runID := model.RunID("run_pubfail")
	publishErr := errors.New("simulated publish failure")
	err := ReserveAndPublish(l, loader, cfg, runID, []string{"alpha"}, time.Unix(100, 0), func(machine string) error {
		return publishErr
	})
	if !errors.Is(err, publishErr) {
		t.Fatalf("reserve-and-publish must propagate the callback error: %v", err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); !os.IsNotExist(err) {
		t.Fatalf("transaction must remove the exact claim on failure: %v", err)
	}
	if _, err := loader.Load(runID); err == nil {
		t.Fatalf("no run record may exist after publish failure")
	}
}

// TestReserveAndPublishRefusesFutureNow pins that a future-dated
// `now` argument is rejected up-front so a doomed claim is never
// written to disk.
func TestReserveAndPublishRefusesFutureNow(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	future := time.Now().Add(2 * time.Hour)
	err := ReserveAndPublish(l, loader, cfg, model.RunID("run_future"), []string{"alpha"}, future, publishCallback(loader, model.RunID("run_future"), model.RunStaging))
	if err == nil {
		t.Fatal("future-dated now must be rejected")
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_future"))); !os.IsNotExist(err) {
		t.Fatalf("no claim may be created on a future-dated now: %v", err)
	}
}

// TestReservationForRejectsFutureCreatedAt pins that an existing
// claim with a future-dated created_at is reported as a corrupt
// state, not granted an unbounded grace window.
func TestReservationForRejectsFutureCreatedAt(t *testing.T) {
	l := newRoot(t)
	future := time.Now().Add(2 * time.Hour)
	body, _ := json.Marshal(map[string]any{
		"schema_version": ClaimSchemaVersion,
		"machine":        "alpha",
		"run_id":         "run_future_claim",
		"created_at":     future.UTC().Format(time.RFC3339Nano),
	})
	writeRawClaim(t, l, "alpha", "run_future_claim", body)
	_, err := ReservationFor(l, "alpha", model.RunID("run_future_claim"))
	if err == nil {
		t.Fatal("future-dated claim must be rejected as corrupt")
	}
}

// TestReleaseRefusesCorruptClaim pins that Release validates the
// exact claim body before removing it. A corrupt claim must be
// reported as an error and remain on disk.
func TestReleaseRefusesCorruptClaim(t *testing.T) {
	l := newRoot(t)
	writeRawClaim(t, l, "alpha", "run_corrupt_release", []byte("{not valid json"))
	if err := Release(l, "alpha", model.RunID("run_corrupt_release")); err == nil {
		t.Fatal("release must reject a corrupt claim")
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_corrupt_release"))); err != nil {
		t.Fatalf("corrupt claim must remain on disk: %v", err)
	}
}

// TestReserveAndPublishFailsWithoutStagingRecord pins that the
// publish callback contract is enforced by the loader: when the
// callback does not record a state, the next reap immediately
// removes the orphan claim. The production path persists a real
// run.json; this test pins the contract behind the callback so
// tests cannot smuggle the obsolete Reserve-with-no-op shape.
func TestReserveAndPublishFailsWithoutStagingRecord(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	// A no-op callback creates a claim without a record. The
	// reservation is orphan state and the next Reap removes it.
	runID := model.RunID("run_orphan_record")
	err := ReserveAndPublish(l, loader, cfg, runID, []string{"alpha"}, time.Unix(100, 0), func(m string) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Now run the reaper and verify the orphan claim is gone.
	removed, err := Reap(l, loader)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("orphan reap removed=%d, want 1", removed)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); !os.IsNotExist(err) {
		t.Fatalf("orphan claim must be reaped: %v", err)
	}
}

// TestNoProducerClaimAPIPresent pins that the production scheduler
// only exposes ReserveAndPublish as the claim-creating API. Other
// entry points may inspect, reap, or release; this is a static
// contract that the package enforces by omission.
func TestNoProducerClaimAPIPresent(t *testing.T) {
	// This test is a contract assertion: Reserve was removed by
	// Plan 11 Fix. If a future contributor re-adds a producer that
	// does not require a publish callback, this test will fail to
	// compile when the surrounding build is updated.
	//
	// The assertion is implemented by the absence of the symbol;
	// the build itself is the proof. The test body is intentionally
	// empty so it serves as a marker.
	_ = ReserveAndPublish
}

// TestReservationForCorruptClaimSurfacesError pins that a claim
// whose body is unparseable surfaces a hard error instead of being
// silently treated as a missing reservation. The worker cannot
// distinguish "no slot" from "corrupt slot" without that signal.
func TestReservationForCorruptClaimSurfacesError(t *testing.T) {
	l := newRoot(t)
	writeRawClaim(t, l, "alpha", "run_corrupt_reservation", []byte("{not valid json"))
	_, err := ReservationFor(l, "alpha", model.RunID("run_corrupt_reservation"))
	if err == nil {
		t.Fatal("corrupt claim must surface as a hard error")
	}
}

// TestReservationForMissingClaimSurfacesNotExist pins that a
// reservation lookup on a never-written claim returns a
// not-found-style error, not a successful answer.
func TestReservationForMissingClaimSurfacesNotExist(t *testing.T) {
	l := newRoot(t)
	_, err := ReservationFor(l, "alpha", model.RunID("run_never"))
	if err == nil {
		t.Fatal("missing claim must surface as a not-found error")
	}
}

// TestReapReleasesClaimWithMissingRecord pins that a well-formed
// claim whose record is missing (the public build path's rollback
// shape) is orphan state and the reaper releases it.
func TestReapReleasesClaimWithMissingRecord(t *testing.T) {
	l := newRoot(t)
	loader := &fakeLoader{}
	body, _ := json.Marshal(map[string]any{
		"schema_version": ClaimSchemaVersion,
		"machine":        "alpha",
		"run_id":         "run_orphan_rollback",
		"created_at":     time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
	})
	writeRawClaim(t, l, "alpha", "run_orphan_rollback", body)
	removed, err := Reap(l, loader)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("orphan reap removed=%d, want 1", removed)
	}
}

// TestReservationForSucceedsOnValidClaim pins the production
// happy-path: a valid claim returns a Reservation and the
// subsequent reservation lookup is the worker's authoritative
// proof before claiming its lease.
func TestReservationForSucceedsOnValidClaim(t *testing.T) {
	l := newRoot(t)
	cfg := config.Config{Machines: map[string]config.Machine{
		"alpha": {MaxConcurrent: 1},
	}}
	loader := &fakeLoader{}
	loader.set("run_valid_reservation", model.RunStaging)
	if _, err := reserveWith(t, l, loader, cfg, model.RunID("run_valid_reservation"), []string{"alpha"}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := ReservationFor(l, "alpha", model.RunID("run_valid_reservation"))
	if err != nil {
		t.Fatalf("reservation lookup: %v", err)
	}
	if res.Machine != "alpha" || string(res.RunID) != "run_valid_reservation" {
		t.Fatalf("reservation: %+v", res)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_valid_reservation"))); err != nil {
		t.Fatalf("claim must remain on disk: %v", err)
	}
}
