package scheduler

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
)

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
