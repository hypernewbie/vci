package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCreatesPrivateLayout(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{l.Root, l.StateDir(), l.RunsDir(), l.BlobsDir(), l.WorkDir()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode %o, want 700", path, mode)
		}
	}
}

func TestRunDirRejectsTraversal(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	for _, id := range []string{"", ".", "..", "../escape", "run/id", "run id"} {
		if _, err := l.RunDir(id); err == nil {
			t.Errorf("accepted unsafe id %q", id)
		}
	}
	path, err := l.RunDir("run_01")
	if err != nil || path != filepath.Join(l.RunsDir(), "run_01") {
		t.Fatalf("run path: %q, %v", path, err)
	}
}
