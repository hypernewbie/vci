package lease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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

// TestLegacyLeaseAttemptIsTolerated pins read-compatibility with
// persisted lease.json files written by historical Vci versions that
// carried an `attempt` field. The field is absent from the live Lease
// schema; plain `json.Unmarshal` must ignore it so a legacy lease can
// still be Renewed or Released by the same owner.
func TestLegacyLeaseAttemptIsTolerated(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
