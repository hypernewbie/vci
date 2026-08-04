package lease

import (
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
