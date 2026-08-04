package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestDiscoverCurrentRepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"init", "-q", dir}}); err != nil {
		t.Fatal(err)
	}
	repo, err := Discover(context.Background(), dir, process.Native{})
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := filepath.EvalSymlinks(dir)
	if repo.Root != expected || repo.Name != filepath.Base(expected) {
		t.Fatalf("repo: %+v", repo)
	}
}

func TestMatchProjectIsCaseInsensitiveFallback(t *testing.T) {
	got, err := MatchProject("vci", []string{"Vci"})
	if err != nil || got != "Vci" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestDiscoverRejectsNonRepository(t *testing.T) {
	dir := t.TempDir()
	if _, err := Discover(context.Background(), dir, process.Native{}); err == nil {
		t.Fatal("non-repository accepted")
	}
}
