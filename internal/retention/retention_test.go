package retention

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

func TestEnforceEvictsOldestBlob(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.BlobsDir(), "old"), []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.BlobsDir(), "new"), []byte("new"), 0o400); err != nil {
		t.Fatal(err)
	}
	r, err := Enforce(l, config.Retention{MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if r.RemovedEntries != 1 {
		t.Fatalf("report: %+v", r)
	}
}
