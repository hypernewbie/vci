package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyExclusions(t *testing.T) {
	dir := t.TempDir()
	mk := func(p, content string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("keep.txt", "k")
	mk("secret.env", "s")
	mk("config/db.key", "k")
	mk("creds", "c")
	mk("node_modules/react.js", "r")
	mk("build/out.tmp", "t")
	mk(".git/objects/xx", "g")
	mk(".vci/state.log", "v")

	if err := ApplyExclusions(dir, []string{"*.env", "*.key", "creds", "node_modules"}); err != nil {
		t.Fatalf("ApplyExclusions: %v", err)
	}
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(dir, p))
		return err == nil
	}
	if !exists("keep.txt") {
		t.Error("keep.txt removed")
	}
	if exists("secret.env") {
		t.Error("secret.env kept")
	}
	if exists("config/db.key") {
		t.Error("config/db.key kept")
	}
	if !exists("config") {
		t.Error("config dir removed (only db.key should be excluded)")
	}
	if exists("creds") {
		t.Error("creds kept")
	}
	if exists("node_modules") {
		t.Error("node_modules kept")
	}
	if exists("node_modules/react.js") {
		t.Error("node_modules/react.js kept")
	}
	if !exists("build/out.tmp") {
		t.Error("build/out.tmp removed (not matched by any glob)")
	}
	if !exists(".git/objects/xx") {
		t.Error(".git internals were traversed")
	}
	if !exists(".vci/state.log") {
		t.Error(".vci internals were traversed")
	}
}

func TestApplyExclusionsEmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyExclusions(dir, nil); err != nil {
		t.Fatalf("ApplyExclusions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.txt")); err != nil {
		t.Error("f.txt removed with no globs")
	}
}
