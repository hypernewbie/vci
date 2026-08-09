package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

func TestTerminalSelfTransitionIsRejected(t *testing.T) {
	s := Store{Layout: model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Now().UTC()
	record, err := NewRun("p", "m", []string{"true"}, "s", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunSucceeded, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunSucceeded, now.Add(time.Second)); err == nil {
		t.Fatal("terminal self-transition accepted")
	}
}

func TestPublishResultIsSingleAndCheckableOnlyAfterTerminal(t *testing.T) {
	s := Store{Layout: model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Now().UTC()
	record, err := NewRun("p", "m", []string{"true"}, "s", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishResult(record.ID, map[string]any{"state": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishResult(record.ID, map[string]any{"state": "succeeded"}); err == nil {
		t.Fatal("result overwrite accepted")
	}
	data, err := s.ReadResult(record.ID)
	if err != nil || len(data) == 0 {
		t.Fatalf("result: %v", err)
	}
}

func TestRunRecordPersistsSnapshotAndTransitions(t *testing.T) {
	s := Store{Layout: model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Unix(100, 0)
	record, err := NewRun("Vci", "mac-local", []string{"go", "test"}, "source", map[string]any{"machine": "mac-local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigDigest == "" || len(loaded.ConfigSnapshot) == 0 || loaded.State != model.RunStaging {
		t.Fatalf("loaded: %+v", loaded)
	}
}

// TestLegacyRunRecordFieldsAreTolerated pins read-compatibility with
// persisted run.json files written by historical Vci versions that
// carried a `result` field and a `cancellation_phase` field on the
// run record itself. Both fields are absent from the live RunRecord
// schema; plain `json.Unmarshal` must ignore them so old files still
// decode into the current record.
func TestLegacyRunRecordFieldsAreTolerated(t *testing.T) {
	dir := t.TempDir()
	l := model.Layout{Root: dir}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runID := model.RunID("run_legacy_1")
	runDir, err := l.RunDir(string(runID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "schema_version": 1,
  "run_id": "run_legacy_1",
  "project": "demo",
  "machine": "mac-local",
  "state": "queued",
  "command": ["true"],
  "queued_at": "2020-01-01T00:00:00Z",
  "updated_at": "2020-01-01T00:00:00Z",
  "cancellation_phase": "requested",
  "result": {"state": "succeeded", "exit_code": 0}
}`
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Store{Layout: l}
	loaded, err := s.Load(runID)
	if err != nil {
		t.Fatalf("legacy run.json with result+cancellation_phase must decode: %v", err)
	}
	if loaded.State != model.RunQueued || loaded.Project != "demo" {
		t.Fatalf("loaded record: %+v", loaded)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(loaded.ConfigSnapshot, &probe); err != nil && len(loaded.ConfigSnapshot) != 0 {
		t.Fatalf("config snapshot: %v", err)
	}
}

func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not enforced on Windows (coordinator not supported)")
	}
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := model.RunID("run_parallel")
	dir, _ := l.RunDir(string(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(owner string) { defer wg.Done(); results <- Claim(l, id, owner, time.Now(), time.Minute) }(owner)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("claim wins: %d", wins)
	}
}

func TestLeaseClaimRenewRelease(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := model.RunID("run_lease")
	if dir, err := l.RunDir(string(id)); err != nil {
		t.Fatal(err)
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10, 0)
	if err := Claim(l, id, "worker", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Claim(l, id, "other", now, time.Minute); err == nil {
		t.Fatal("duplicate claim accepted")
	}
	if err := Renew(l, id, "worker", now.Add(time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Release(l, id, "worker"); err != nil {
		t.Fatal(err)
	}
}

// TestReleaseRejectsWrongOwner pins that store.Release only removes a
// lease owned by the caller: a different owner's release attempt is
// an error and the lease survives for the true owner.
func TestReleaseRejectsWrongOwner(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := model.RunID("run_lease_owner")
	dir, err := l.RunDir(string(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10, 0)
	if err := Claim(l, id, "worker", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Release(l, id, "intruder"); err == nil {
		t.Fatal("release by a non-owner accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "lease.json")); err != nil {
		t.Fatalf("lease removed by a non-owner: %v", err)
	}
	if err := Release(l, id, "worker"); err != nil {
		t.Fatalf("owner release: %v", err)
	}
}

// TestLegacyLeaseAttemptIsTolerated pins read-compatibility with
// persisted lease.json files written by historical Vci versions that
// carried an `attempt` field. The field is absent from the live Lease
// schema; plain `json.Unmarshal` must ignore it so a legacy lease can
// still be Renewed or Released by the same owner.
func TestLegacyLeaseAttemptIsTolerated(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := model.RunID("run_lease_legacy")
	dir, err := l.RunDir(string(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(20, 0)
	expiry := now.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	legacy := `{"run_id":"run_lease_legacy","owner":"worker-abcdefgh","expires_at":"` + expiry + `","attempt":3}`
	if err := os.WriteFile(filepath.Join(dir, "lease.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(l, id)
	if err != nil {
		t.Fatalf("legacy lease.json with attempt must decode: %v", err)
	}
	if loaded.Owner != "worker-abcdefgh" {
		t.Fatalf("owner: %q", loaded.Owner)
	}
	if err := Renew(l, id, "worker-abcdefgh", now.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("renew legacy lease: %v", err)
	}
	var probe map[string]json.RawMessage
	raw, _ := os.ReadFile(filepath.Join(dir, "lease.json"))
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("post-renew decode: %v", err)
	}
	if _, ok := probe["attempt"]; ok {
		t.Fatalf("post-renew lease must not resurrect attempt field: %s", raw)
	}
	if err := Release(l, id, "worker-abcdefgh"); err != nil {
		t.Fatalf("release legacy lease: %v", err)
	}
}
