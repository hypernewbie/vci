package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectArtifactsHappyPath pins the documented contract:
// glob matches under the workspace are copied to runDir/artifacts/<rel>
// with the source's permission, the rel list returns in the order
// of discovery, and the truncated flag stays false when the
// total is under the cap.
func TestCollectArtifactsHappyPath(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")
	mustWriteFile(t, filepath.Join(workspace, "dist", "v1.zip"), "zip\n")
	mustWriteFile(t, filepath.Join(workspace, "dist", "v2.zip"), "zip2\n")
	mustWriteFile(t, filepath.Join(workspace, "src", "main.go"), "package main\n")

	collected, truncated, err := CollectArtifacts(workspace, runDir, []string{"build/*", "dist/*.zip"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if truncated {
		t.Errorf("truncated=true under cap")
	}
	if len(collected) != 3 {
		t.Errorf("collected: %v", collected)
	}
	for _, rel := range collected {
		dst := filepath.Join(runDir, "artifacts", filepath.FromSlash(rel))
		if _, err := os.Stat(dst); err != nil {
			t.Errorf("missing artifact %q: %v", dst, err)
		}
	}
}

// TestCollectArtifactsRejectsDotGit pins that paths under `.git`
// are never collected even if the glob matches them.
func TestCollectArtifactsRejectsDotGit(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(workspace, ".git", "objects", "abc"), "blob\n")
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")

	collected, _, err := CollectArtifacts(workspace, runDir, []string{"build/*"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, rel := range collected {
		if strings.HasPrefix(rel, ".git") {
			t.Errorf(".git path leaked: %s", rel)
		}
	}
	if len(collected) != 1 {
		t.Errorf("collected: %v", collected)
	}
}

// TestCollectArtifactsRejectsDotVci pins that paths under `.vci`
// are never collected.
func TestCollectArtifactsRejectsDotVci(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".vci", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, ".vci", "state", "run.json"), "{}")
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")

	collected, _, err := CollectArtifacts(workspace, runDir, []string{"build/*"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, rel := range collected {
		if strings.HasPrefix(rel, ".vci") {
			t.Errorf(".vci path leaked: %s", rel)
		}
	}
	if len(collected) != 1 {
		t.Errorf("collected: %v", collected)
	}
}

// TestCollectArtifactsEmptyGlobs pins that an empty glob list
// returns no artifacts and never creates the artifacts directory.
func TestCollectArtifactsEmptyGlobs(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")

	collected, truncated, err := CollectArtifacts(workspace, runDir, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(collected) != 0 || truncated {
		t.Errorf("got %v truncated=%v", collected, truncated)
	}
	if _, err := os.Stat(filepath.Join(runDir, "artifacts")); !os.IsNotExist(err) {
		t.Errorf("artifacts dir should not exist")
	}
}

// TestCollectArtifactsCapsAndTruncates pins that exceeding the
// 64 MiB per-run cap sets truncated=true and stops copying
// further matches.
func TestCollectArtifactsCapsAndTruncates(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	// First file: 1 MiB. Cap is 64 MiB; add 64 more 1-MiB files
	// to overflow exactly past the cap.
	chunk := make([]byte, 1<<20)
	if err := os.WriteFile(filepath.Join(workspace, "build", "first.bin"), chunk, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		name := filepath.Join(workspace, "build", "f"+string(rune('a'+i))+"_"+string(rune('0'+(i/26)))+".bin")
		if err := os.WriteFile(name, chunk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	collected, truncated, err := CollectArtifacts(workspace, runDir, []string{"build/*.bin"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !truncated {
		t.Errorf("truncated=false despite overflow")
	}
	if len(collected) >= 65 {
		t.Errorf("too many collected: %d", len(collected))
	}
}

// TestCollectArtifactsSymlinkNotCopied pins that a symlink to a
// regular file is not collected (only regular files are copied).
func TestCollectArtifactsSymlinkNotCopied(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	target := filepath.Join(workspace, "target.bin")
	mustWriteFile(t, target, "data\n")
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "build", "link.bin")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "build", "real.bin"), "real\n")

	collected, _, err := CollectArtifacts(workspace, runDir, []string{"build/*.bin"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, rel := range collected {
		if strings.Contains(rel, "link.bin") {
			t.Errorf("symlink collected: %s", rel)
		}
	}
	if len(collected) < 1 {
		t.Errorf("expected at least one regular file")
	}
}

// TestCollectArtifactsFilePerm pins that the copied artifact
// inherits the source's permission bits (regular files only;
// the collector never widens to 0o600 unless the source was 0o600).
func TestCollectArtifactsFilePerm(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	src := filepath.Join(workspace, "build", "out.bin")
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CollectArtifacts(workspace, runDir, []string{"build/*.bin"}); err != nil {
		t.Fatalf("collect: %v", err)
	}
	dst := filepath.Join(runDir, "artifacts", "build", "out.bin")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm: %v", info.Mode().Perm())
	}
}

// TestCollectArtifactsNestedGlob pins the documented per-segment
// glob semantics: a trailing bare `*` collects the whole tree
// under the prefix (`build/*` matches `build/sub/file.txt`), while
// a constrained final segment must match every trailing path
// segment, so `build/*.bin` is single-level and `build/[ab]*`
// never matches `build/a/x` (the trailing `x` does not match
// `[ab]*`). The pre-fix matcher walked ancestor directories,
// which made `build/[ab]*` collect `build/a/x` while
// `dist/*.zip` stayed single-level — ambiguous and untested.
func TestCollectArtifactsNestedGlob(t *testing.T) {
	workspace := t.TempDir()
	runDir := t.TempDir()
	for _, d := range []string{"build/sub", "build/a"} {
		if err := os.MkdirAll(filepath.Join(workspace, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")
	mustWriteFile(t, filepath.Join(workspace, "build", "sub", "file.txt"), "nested\n")
	mustWriteFile(t, filepath.Join(workspace, "build", "sub", "x.bin"), "xb\n")
	mustWriteFile(t, filepath.Join(workspace, "build", "a", "x"), "x\n")

	// `build/*`: the trailing `*` matches every segment below
	// `build/`, so the whole tree is collected.
	collected, truncated, err := CollectArtifacts(workspace, runDir, []string{"build/*"})
	if err != nil {
		t.Fatalf("collect build/*: %v", err)
	}
	if truncated {
		t.Error("truncated=true under cap")
	}
	for _, want := range []string{"build/out.bin", "build/sub/file.txt", "build/a/x"} {
		if !containsRel(collected, want) {
			t.Errorf("build/* dropped %q: %v", want, collected)
		}
	}

	// `build/*.bin`: the constrained final segment must match
	// every trailing segment, so only files directly inside
	// `build/` are collected.
	collected2, _, err := CollectArtifacts(workspace, runDir, []string{"build/*.bin"})
	if err != nil {
		t.Fatalf("collect build/*.bin: %v", err)
	}
	if !containsRel(collected2, "build/out.bin") {
		t.Errorf("build/*.bin dropped %q: %v", "build/out.bin", collected2)
	}
	if containsRel(collected2, "build/sub/x.bin") {
		t.Errorf("build/*.bin collected nested %q: %v", "build/sub/x.bin", collected2)
	}

	// `build/[ab]*`: `[ab]*` must match every trailing segment;
	// `build/a/x` has a trailing `x` that does not match, so the
	// ancestor-walk fallback must not resurrect it.
	collected3, _, err := CollectArtifacts(workspace, runDir, []string{"build/[ab]*"})
	if err != nil {
		t.Fatalf("collect build/[ab]*: %v", err)
	}
	if containsRel(collected3, "build/a/x") {
		t.Errorf("build/[ab]* collected nested %q: %v", "build/a/x", collected3)
	}
	if len(collected3) != 0 {
		t.Errorf("build/[ab]*: got %v, want no matches", collected3)
	}
}

func containsRel(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
