package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestCopyWorkspacePreservesTree(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "regular.txt"), []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "exec.sh"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/target", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "build.out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "marker"), []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".vci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".vci", "marker"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	skip := func(name string) bool { return name == ".git" || name == ".vci" }
	if err := CopyWorkspace(src, dst, skip); err != nil {
		t.Fatalf("CopyWorkspace: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "regular.txt")); err != nil || string(got) != "r" {
		t.Fatalf("regular.txt: %q %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dst, "exec.sh"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec.sh should keep its execute bit: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "/tmp/target" {
		t.Fatalf("link = %q, %v", target, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "sub", "nested.txt")); err != nil || string(got) != "n" {
		t.Fatalf("nested.txt: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "build.out")); err != nil || string(got) != "cached" {
		t.Fatalf("build.out: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git should be skipped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".vci")); !os.IsNotExist(err) {
		t.Fatalf(".vci should be skipped: %v", err)
	}
}

func TestReconstructSeededReproducesClientState(t *testing.T) {
	seed := t.TempDir()
	runGit(t, seed, "init", "-q")
	if err := os.WriteFile(filepath.Join(seed, "src.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "src.txt")
	runGit(t, seed, "commit", "-q", "-m", "base")
	base := gitSha(t, seed, "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "build.out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The client shares the seed's history (clone) so the base object id matches,
	// then advances to head and adds an untracked local change.
	client := t.TempDir()
	runGit(t, t.TempDir(), "clone", "-q", seed, client)
	if err := os.WriteFile(filepath.Join(client, "src.txt"), []byte("head"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, client, "add", "src.txt")
	runGit(t, client, "commit", "-q", "-m", "head")
	head := gitSha(t, client, "HEAD")
	if err := os.WriteFile(filepath.Join(client, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	lc, err := CaptureLocalChanges(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	bundle, err := CreateBundle(context.Background(), client, base, head, process.Native{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}

	w := t.TempDir()
	if err := CopyWorkspace(seed, w, func(name string) bool { return name == ".vci" }); err != nil {
		t.Fatalf("copy seed: %v", err)
	}
	if err := AdvanceToHead(context.Background(), w, head, bundle, process.Native{}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := ApplyLC(context.Background(), w, lc, process.Native{}); err != nil {
		t.Fatalf("apply lc: %v", err)
	}

	mustRead := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(w, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if got := mustRead("src.txt"); got != "head" {
		t.Fatalf("src.txt = %q, want head", got)
	}
	if got := mustRead("build.out"); got != "cached" {
		t.Fatalf("build.out = %q, want cached (seed build output must survive)", got)
	}
	if got := mustRead("untracked.txt"); got != "new" {
		t.Fatalf("untracked.txt = %q, want new (client local change not applied)", got)
	}
}

func TestReconstructWorkspaceAppliesExclusions(t *testing.T) {
	seed := t.TempDir()
	runGit(t, seed, "init", "-q")
	if err := os.WriteFile(filepath.Join(seed, "src.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "secret.env"), []byte("leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "src.txt", "secret.env")
	runGit(t, seed, "commit", "-q", "-m", "base")
	base := gitSha(t, seed, "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "build.out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := t.TempDir()
	runGit(t, t.TempDir(), "clone", "-q", seed, client)
	if err := os.WriteFile(filepath.Join(client, "src.txt"), []byte("head"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, client, "add", "src.txt")
	runGit(t, client, "commit", "-q", "-m", "head")
	head := gitSha(t, client, "HEAD")
	if err := os.WriteFile(filepath.Join(client, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle, err := CreateBundle(context.Background(), client, base, head, process.Native{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	lc, err := CaptureLocalChanges(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	w := t.TempDir()
	if err := ReconstructWorkspace(context.Background(), seed, w, head, bundle, lc, []string{"*.env"}, process.Native{}); err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	mustRead := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(w, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if got := mustRead("src.txt"); got != "head" {
		t.Fatalf("src.txt = %q, want head", got)
	}
	if got := mustRead("build.out"); got != "cached" {
		t.Fatalf("build.out = %q, want cached", got)
	}
	if got := mustRead("untracked.txt"); got != "new" {
		t.Fatalf("untracked.txt = %q, want new", got)
	}
	if _, err := os.Stat(filepath.Join(w, "secret.env")); !os.IsNotExist(err) {
		t.Fatalf("secret.env should be excluded: %v", err)
	}
}
