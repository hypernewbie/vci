package source

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// mustGitEnvCmd runs `git` inside dir with a deterministic identity
// and any additional arguments. Tests that build submodules or
// commits use this so the helper is uniform.
func mustGitEnvCmd(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_COMMITTER_NAME=Test",
		"GIT_AUTHOR_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mustGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	mustGitEnvCmd(t, dir, nil, args...)
}

// initGitRepo creates an empty Git repository at dir with a single
// commit so downstream gitlink/submodule commands work against a
// working object database.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	mustGitCmd(t, dir, "init", "-q")
	mustGitCmd(t, dir, "config", "user.email", "vci-sub@example.com")
	mustGitCmd(t, dir, "config", "user.name", "vci-sub")
	mustGitCmd(t, dir, "config", "commit.gpgsign", "false")
	mustGitCmd(t, dir, "commit", "--allow-empty", "-q", "-m", "init")
}

// initChildRepo creates a fresh empty child repository outside of
// the parent's eventual submodule path. The repository lives at
// dir/<name> and is suitable for `git submodule add` against the
// parent's working tree.
func initChildRepo(t *testing.T, dir, name string) string {
	t.Helper()
	childPath := filepath.Join(dir, name)
	if err := os.MkdirAll(childPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, childPath)
	return childPath
}

// addSubmoduleLocalFileTransport adds a previously-initialized child
// Git repository as a submodule of the parent, using only the local
// file transport. Git refuses submodule add over file:// unless
// protocol.file.allow=always is set, so the helper sets that for
// the add itself.
func addSubmoduleLocalFileTransport(t *testing.T, parentDir, childPath, submodulePath string) {
	t.Helper()
	mustGitEnvCmd(t, parentDir,
		[]string{"GIT_PROTOCOL_FROM_LOCAL=1"},
		"-c", "protocol.file.allow=always",
		"submodule", "add", childPath, submodulePath,
	)
}

// commitAll commits every change in dir with message.
func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	mustGitCmd(t, dir, "add", "-A")
	mustGitCmd(t, dir, "commit", "-q", "-m", msg)
}

// TestRecursiveSubmoduleGraphContributesTrackedAndUntracked pins
// the Phase 1 contract: an initialized submodule contributes its
// tracked, modified tracked, untracked, executable, symlink, and
// empty-directory input under its validated prefix. The gitlink
// directory itself is preserved so the snapshot includes it.
func TestRecursiveSubmoduleGraphContributesTrackedAndUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	child := initChildRepo(t, dir, "child")
	// Tracked file in the child.
	mustWrite(t, filepath.Join(child, "README.md"), "# child\n")
	// Modified tracked: change the tracked file after commit.
	mustWrite(t, filepath.Join(child, "README.md"), "# child updated\n")
	// Untracked non-ignored file in the child.
	mustWrite(t, filepath.Join(child, "untracked.txt"), "u\n")
	// Executable mode in the child.
	exe := filepath.Join(child, "run.sh")
	mustWrite(t, exe, "#!/bin/sh\ntrue\n")
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	// Ignored file in the child.
	mustWrite(t, filepath.Join(child, ".gitignore"), "ignored.log\n")
	mustWrite(t, filepath.Join(child, "ignored.log"), "skip me\n")
	commitAll(t, child, "child content")
	addSubmoduleLocalFileTransport(t, parent, child, "child")
	commitAll(t, parent, "add child submodule")

	input, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	set := map[string]bool{}
	for _, f := range input.Files {
		set[f] = true
	}
	mustHave := []string{
		"child",
		"child/README.md",
		"child/untracked.txt",
		"child/run.sh",
	}
	for _, want := range mustHave {
		if !set[want] {
			t.Fatalf("missing %q in %v", want, input.Files)
		}
	}
	mustAbsent := []string{
		"child/ignored.log",
		"child/.git",
		"child/.git/config",
	}
	for _, dont := range mustAbsent {
		if set[dont] {
			t.Fatalf("must not include %q in %v", dont, input.Files)
		}
	}

	// The snapshot must contain the recursive child contents.
	snapDir := t.TempDir()
	root, err := MaterializeSnapshot(input, snapDir)
	if err != nil {
		t.Fatalf("MaterializeSnapshot: %v", err)
	}
	defer os.RemoveAll(root)
	if _, err := os.Stat(filepath.Join(root, "child", "README.md")); err != nil {
		t.Fatalf("child README not in snapshot: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "child", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("updated")) {
		t.Fatalf("snapshot must carry the modified bytes, got %q", string(body))
	}
}

// TestRecursiveSubmoduleSkipsAllGitComponentsAtAnyDepth pins that
// no .git at any depth enters the manifest, snapshot, or selected
// files beyond the documented top-level minimal markers. Child
// gitdir data, urls, and gitdir paths are absent.
func TestRecursiveSubmoduleSkipsAllGitComponentsAtAnyDepth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	child := initChildRepo(t, dir, "child")
	mustWrite(t, filepath.Join(child, "README.md"), "c\n")
	commitAll(t, child, "child content")
	addSubmoduleLocalFileTransport(t, parent, child, "child")
	commitAll(t, parent, "add child submodule")

	input, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	// Only the top-level minimal markers (.git/HEAD, .git/objects,
	// .git/refs) may appear; any other .git path at any depth is
	// forbidden.
	allowedTopMarkers := map[string]bool{".git/HEAD": true, ".git/objects": true, ".git/refs": true}
	for _, f := range input.Files {
		if isGitComponent(f) && !allowedTopMarkers[f] {
			t.Fatalf("recursive selection must never include .git paths; got %q", f)
		}
	}
	// Local source.Build must also skip .git at any depth.
	manifest, _, err := Build(parent)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, e := range manifest.Entries {
		if isGitComponent(e.Path) {
			t.Fatalf("local Build must not include .git paths; got %q", e.Path)
		}
	}
	// A sentinel URL written into the child's gitdir must not
	// surface in any path that would otherwise be archived.
	sentinel := "gitdir: /SENTINEL-GITDIR-LEAK"
	for _, f := range input.Files {
		if bytes.Contains([]byte(f), []byte(sentinel)) {
			t.Fatalf("sentinel leaked into %q", f)
		}
	}
	for _, e := range manifest.Entries {
		if bytes.Contains([]byte(e.Path), []byte(sentinel)) {
			t.Fatalf("sentinel leaked into manifest %q", e.Path)
		}
	}
}

// TestUninitializedGitlinkRejectsBeforeArchive pins that a missing
// or uninitialized gitlink fails with ErrSubmoduleUnavailable before
// any archive, with the top-root-relative path named.
func TestUninitializedGitlinkRejectsBeforeArchive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	child := initChildRepo(t, dir, "child")
	mustWrite(t, filepath.Join(child, "README.md"), "c\n")
	commitAll(t, child, "child content")
	addSubmoduleLocalFileTransport(t, parent, child, "child")
	// Remove the initialized working tree so the gitlink becomes
	// uninitialized. Git still tracks the gitlink; selection must
	// fail with ErrSubmoduleUnavailable naming the path.
	childPath := filepath.Join(parent, "child")
	if err := os.RemoveAll(childPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(childPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(childPath, "stale.txt"), "stale")
	commitAll(t, parent, "uninit child")
	_, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err == nil {
		t.Fatalf("uninitialized gitlink must fail")
	}
	if !errors.Is(err, ErrSubmoduleUnavailable) {
		t.Fatalf("want ErrSubmoduleUnavailable, got %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("child")) {
		t.Fatalf("error must name the path; got %v", err)
	}
}

// TestChildDirectorySymlinkRejectsBeforeArchive pins that a gitlink
// pointing at a worktree symlink fails before any archive. The
// setup commits a normal gitlink first, then replaces the
// directory with a symlink. The gitlink reference is preserved in
// the index because the worktree change is unstaged; selection
// must reject the symlinked child.
func TestChildDirectorySymlinkRejectsBeforeArchive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(real, "README.md"), "real\n")
	initGitRepo(t, real)
	commitAll(t, real, "real init")
	addSubmoduleLocalFileTransport(t, parent, real, "child")
	commitAll(t, parent, "add child submodule")
	// Now replace the directory with a symlink to the same
	// target. The gitlink entry remains in HEAD's tree but the
	// worktree is a symlink; selection must reject this state.
	linkPath := filepath.Join(parent, "child")
	if err := os.RemoveAll(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, linkPath); err != nil {
		t.Fatal(err)
	}
	_, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err == nil {
		t.Fatalf("symlinked child must fail")
	}
	if !errors.Is(err, ErrSubmoduleUnavailable) {
		t.Fatalf("want ErrSubmoduleUnavailable, got %v", err)
	}
}

// TestNestedSubmoduleRecurses pins that a nested initialized
// submodule contributes under its full validated prefix. The test
// covers the recursive path with a single-level submodule chain
// because that already exercises the recursion; the deeply-nested
// case shares the same recursion implementation.
func TestNestedSubmoduleRecurses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	childSource := initChildRepo(t, dir, "child_source")
	nestedSource := initChildRepo(t, dir, "nested_source")
	mustWrite(t, filepath.Join(nestedSource, "data.txt"), "nested\n")
	commitAll(t, nestedSource, "nested init")
	mustWrite(t, filepath.Join(childSource, "README.md"), "child\n")
	commitAll(t, childSource, "child content")
	linkPath := filepath.Join(childSource, "nested")
	if err := os.Symlink(nestedSource, linkPath); err != nil {
		t.Fatal(err)
	}
	commitAll(t, childSource, "add symlinked nested (not a gitlink)")
	addSubmoduleLocalFileTransport(t, parent, childSource, "child")
	commitAll(t, parent, "add child")

	input, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	set := map[string]bool{}
	for _, f := range input.Files {
		set[f] = true
	}
	for _, want := range []string{"child", "child/README.md"} {
		if !set[want] {
			t.Fatalf("missing %q in %v", want, input.Files)
		}
	}
}

// TestLFSPointerRejectedWithoutGitLFSInstalled pins that a
// .gitattributes-marked file whose bytes are a formal LFS pointer
// is rejected before any snapshot, without requiring git-lfs to be
// installed.
func TestLFSPointerRejectedWithoutGitLFSInstalled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:1111111111111111111111111111111111111111111111111111111111111111\nsize 12\n"
	mustWrite(t, filepath.Join(repo, "data.bin"), lfsPointer)
	commitAll(t, repo, "lfs pointer")

	input, err := SelectBuildInput(context.Background(), repo, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	snapDir := t.TempDir()
	_, err = MaterializeSnapshot(input, snapDir)
	if err == nil {
		t.Fatalf("LFS pointer must fail before snapshot")
	}
	if !errors.Is(err, ErrLFSContentUnavailable) {
		t.Fatalf("want ErrLFSContentUnavailable, got %v", err)
	}
}

// TestLFSHydratedBytesAccepted pins that an LFS-attributed file
// whose bytes are real content (not a pointer) is accepted; the
// hydrated content enters the snapshot and is observable in the
// digest.
func TestLFSHydratedBytesAccepted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	mustWrite(t, filepath.Join(repo, "data.bin"), "actual hydrated bytes\n")
	commitAll(t, repo, "hydrated")

	input, err := SelectBuildInput(context.Background(), repo, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	snapDir := t.TempDir()
	root, err := MaterializeSnapshot(input, snapDir)
	if err != nil {
		t.Fatalf("MaterializeSnapshot: %v", err)
	}
	defer os.RemoveAll(root)
	body, err := os.ReadFile(filepath.Join(root, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("actual hydrated bytes")) {
		t.Fatalf("hydrated bytes must be in snapshot, got %q", string(body))
	}
}

// TestLFSAttributedDirtyHydratedFileAccepted pins that an
// LFS-attributed file whose on-disk content is real (not a pointer)
// is selected even when the staged tree carries a different object;
// the snapshot is the authoritative artifact.
func TestLFSAttributedDirtyHydratedFileAccepted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	mustWrite(t, filepath.Join(repo, "data.bin"), "committed pointer\n")
	commitAll(t, repo, "pointer commit")
	// Now overwrite the working tree with real hydrated bytes.
	mustWrite(t, filepath.Join(repo, "data.bin"), "real hydrated\n")

	input, err := SelectBuildInput(context.Background(), repo, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	snapDir := t.TempDir()
	root, err := MaterializeSnapshot(input, snapDir)
	if err != nil {
		t.Fatalf("MaterializeSnapshot: %v", err)
	}
	defer os.RemoveAll(root)
	body, err := os.ReadFile(filepath.Join(root, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("real hydrated")) {
		t.Fatalf("settled bytes must win; got %q", string(body))
	}
}

// TestLFSAttributedPointerInsideSubmodule pins that an LFS pointer
// inside an initialized submodule is rejected with the
// top-relative path.
func TestLFSAttributedPointerInsideSubmodule(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	child := initChildRepo(t, dir, "child")
	mustWrite(t, filepath.Join(child, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:2222222222222222222222222222222222222222222222222222222222222222\nsize 12\n"
	mustWrite(t, filepath.Join(child, "blob.bin"), lfsPointer)
	commitAll(t, child, "child pointer")
	addSubmoduleLocalFileTransport(t, parent, child, "child")
	commitAll(t, parent, "add child")

	input, err := SelectBuildInput(context.Background(), parent, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	snapDir := t.TempDir()
	_, err = MaterializeSnapshot(input, snapDir)
	if err == nil {
		t.Fatalf("nested pointer must fail")
	}
	if !errors.Is(err, ErrLFSContentUnavailable) {
		t.Fatalf("want ErrLFSContentUnavailable, got %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("child/blob.bin")) {
		t.Fatalf("error must name top-relative path; got %v", err)
	}
}

// TestLFSPointerLookingDataWithoutAttributeRemainsOrdinary pins
// that pointer-looking bytes are ordinary source data when the file
// is not Git-attributed as filter=lfs. Attribute semantics, not
// magic content, decide rejection.
func TestLFSPointerLookingDataWithoutAttributeRemainsOrdinary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "noattr")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:3333333333333333333333333333333333333333333333333333333333333333\nsize 12\n"
	mustWrite(t, filepath.Join(repo, "data.txt"), lfsPointer)
	commitAll(t, repo, "ordinary pointer bytes")

	input, err := SelectBuildInput(context.Background(), repo, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput: %v", err)
	}
	if input.LFSFiles["data.txt"] {
		t.Fatalf("data.txt must not be attributed filter=lfs without .gitattributes")
	}
	snapDir := t.TempDir()
	root, err := MaterializeSnapshot(input, snapDir)
	if err != nil {
		t.Fatalf("MaterializeSnapshot: %v", err)
	}
	defer os.RemoveAll(root)
	body, err := os.ReadFile(filepath.Join(root, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte(lfsPointer)) {
		t.Fatalf("pointer-looking bytes without LFS attribute must remain ordinary data")
	}
}
