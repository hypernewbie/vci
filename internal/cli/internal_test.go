package cli

// Unit tests for the worker-side internal subcommands. These run the
// command bodies directly against t.TempDir() file trees — no SSH, no
// remote host. The worker path validators accept the temp roots because
// the tests build the same `.vci/state/work/<run>` and
// `.vci/state/bundle-cache/v1/<project>/<base>` suffixes the
// coordinator's remote paths carry after tilde expansion.

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// workPath returns a test-owned worker work path under a temp root.
func workPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".vci", "state", "work", "run_abc")
}

// cacheProjPath returns a test-owned worker bundle-cache project dir.
func cacheProjPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".vci", "state", "bundle-cache", "v1", "Vci")
}

// cacheEntryPath returns a test-owned worker cache entry dir for base.
func cacheEntryPath(t *testing.T, base string) string {
	t.Helper()
	return filepath.Join(cacheProjPath(t), base)
}

// gitRun runs git in dir with a test-safe identity and returns trimmed stdout.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Vci Test", "GIT_AUTHOR_EMAIL=vci-test@example.com",
		"GIT_COMMITTER_NAME=Vci Test", "GIT_COMMITTER_EMAIL=vci-test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// writeTar builds a tar archive from ordered entries: header fields
// followed by optional content for regular files.
type tarEntry struct {
	name     string
	mode     int64
	typeflag byte
	linkname string
	content  string
}

func writeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.linkname}
		if e.typeflag == tar.TypeReg || e.typeflag == tar.TypeRegA {
			h.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg || e.typeflag == tar.TypeRegA {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInternalStageRoundTrip pins that internal-stage clears an existing
// work dir and then extracts a workspace tar from stdin, preserving
// directories, file modes, and symlinks.
func TestInternalStageRoundTrip(t *testing.T) {
	dir := workPath(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := writeTar(t,
		tarEntry{name: "sub/", mode: 0o755, typeflag: tar.TypeDir},
		tarEntry{name: "sub/a.txt", mode: 0o644, typeflag: tar.TypeReg, content: "hello\n"},
		tarEntry{name: "run.sh", mode: 0o755, typeflag: tar.TypeReg, content: "#!/bin/sh\n"},
		tarEntry{name: "link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "run.sh"},
	)
	if err := internalStage(dir, bytes.NewReader(archive)); err != nil {
		t.Fatalf("internal-stage: %v", err)
	}

	// The stale file is gone; the staged tree is present with modes.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file survived staging: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Errorf("staged file: %q, %v", data, err)
	}
	runInfo, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil || runInfo.Mode().Perm() != 0o755 {
		t.Errorf("executable mode not preserved: %v, %v", runInfo, err)
	}
	linkInfo, err := os.Lstat(filepath.Join(dir, "link"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink not preserved: %v, %v", linkInfo, err)
	}
}

// TestInternalStageRejectsBadPath pins that internal-stage refuses a work
// path that is not under `.vci/state/work/<run>` before touching the disk.
func TestInternalStageRejectsBadPath(t *testing.T) {
	if err := internalStage(filepath.Join(t.TempDir(), "elsewhere"), bytes.NewReader(nil)); err == nil {
		t.Error("internal-stage accepted a path outside .vci/state/work")
	}
	if err := internalStage("relative/work", bytes.NewReader(nil)); err == nil {
		t.Error("internal-stage accepted a relative path")
	}
}

// TestInternalCacheClaimLifecycle pins the probe/acquire/release contract:
// probe reports a hit only while the complete marker exists, acquire guards
// on the complete marker and creates the claim file, and release removes it
// and tolerates a missing marker.
func TestInternalCacheClaimLifecycle(t *testing.T) {
	entry := cacheEntryPath(t, "abc123")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}

	found, err := internalProbeCache(entry)
	if err != nil || found {
		t.Fatalf("probe before completion: %v, %v", found, err)
	}
	if err := os.WriteFile(filepath.Join(entry, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	found, err = internalProbeCache(entry)
	if err != nil || !found {
		t.Fatalf("probe after completion: %v, %v", found, err)
	}

	if err := internalAcquireClaim(entry, "run_1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	marker := filepath.Join(entry, "claims", "run_1")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("claim marker missing: %v", err)
	}
	// A second acquire with a different claim id coexists.
	if err := internalAcquireClaim(entry, "run_2"); err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	if err := internalReleaseClaim(entry, "run_1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("claim marker survived release: %v", err)
	}
	// rm -f semantics: releasing a missing marker is not an error.
	if err := internalReleaseClaim(entry, "run_1"); err != nil {
		t.Fatalf("release missing marker: %v", err)
	}

	// The complete-marker guard mirrors `test -f` before the claims write.
	incomplete := cacheEntryPath(t, "zzz")
	if err := internalAcquireClaim(incomplete, "run_1"); err == nil {
		t.Error("acquire on an incomplete entry succeeded")
	}
	// Claim ids are validated as single safe path segments.
	if err := internalAcquireClaim(entry, "a/b"); err == nil {
		t.Error("acquire accepted a slash-bearing claim id")
	}
	if err := internalReleaseClaim(entry, ".."); err == nil {
		t.Error("release accepted a dotdot claim id")
	}
}

// TestInternalReapCacheCounts pins the reap contract: an incomplete entry
// older than the partial cutoff and a claim marker older than the claim
// cutoff count as stale, a claim-free complete entry beyond the positive
// limits is evicted oldest-first, the reference files are removed, and the
// printed line reports `stale=N evicted=M`.
func TestInternalReapCacheCounts(t *testing.T) {
	projDir := cacheProjPath(t)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)

	// A stale incomplete entry: meta.json untouched for two hours.
	old := filepath.Join(projDir, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	oldMeta := filepath.Join(old, "meta.json")
	if err := os.WriteFile(oldMeta, []byte(`{"bytes":10}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldMeta, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A complete entry with a stale claim marker.
	claimed := filepath.Join(projDir, "claimed")
	if err := os.MkdirAll(filepath.Join(claimed, "claims"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimed, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	claimMeta := filepath.Join(claimed, "meta.json")
	if err := os.WriteFile(claimMeta, []byte(`{"bytes":100}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(claimMeta, now, now); err != nil {
		t.Fatal(err)
	}
	staleClaim := filepath.Join(claimed, "claims", "run_old")
	if err := os.WriteFile(staleClaim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleClaim, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A fresh complete entry that must survive both passes.
	fresh := filepath.Join(projDir, "fresh")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "meta.json"), []byte(`{"bytes":50}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A claim-free complete entry old enough to be the LRU victim under
	// maxEntries=2.
	victim := filepath.Join(projDir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	victimMeta := filepath.Join(victim, "meta.json")
	if err := os.WriteFile(victimMeta, []byte(`{"bytes":200}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victimMeta, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	refDir := filepath.Join(projDir, ".vci-reap")
	partialRef := filepath.Join(refDir, "partial")
	claimRef := filepath.Join(refDir, "claim")
	var stdout bytes.Buffer
	code, err := runInternal("internal-reap-cache", []string{
		projDir, refDir, partialRef, claimRef,
		cutoff.Format(time.RFC3339), cutoff.Format(time.RFC3339),
		"2", "0",
	}, nil, &stdout)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if code != 0 {
		t.Fatalf("reap exit: %d", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "stale=2 evicted=1" {
		t.Fatalf("reap output %q, want stale=2 evicted=1", got)
	}

	// The stale entry and the stale claim are gone; the fresh entry stays;
	// the LRU victim was evicted; the reference directory is removed.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale entry not removed: %v", err)
	}
	if _, err := os.Stat(staleClaim); !os.IsNotExist(err) {
		t.Errorf("stale claim not removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh entry removed: %v", err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("LRU victim not evicted: %v", err)
	}
	if _, err := os.Stat(refDir); !os.IsNotExist(err) {
		t.Errorf("reference dir not cleaned up: %v", err)
	}
}

// TestInternalReapCacheRejectsUndeletable pins that a removal failure
// aborts the reap instead of silently under-counting: the stale entry holds
// an unreadable subdirectory, so its removal fails and the whole pass errors.
func TestInternalReapCacheRejectsUndeletable(t *testing.T) {
	projDir := cacheProjPath(t)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	old := filepath.Join(projDir, "old")
	if err := os.MkdirAll(filepath.Join(old, "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The locked subdirectory holds a file, so its removal needs to read
	// it, which the mode-0000 bits forbid.
	if err := os.WriteFile(filepath.Join(old, "locked", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(old, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(old, "locked"), 0o755) })
	oldMeta := filepath.Join(old, "meta.json")
	if err := os.WriteFile(oldMeta, []byte(`{"bytes":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldMeta, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	refDir := filepath.Join(projDir, ".vci-reap")
	_, _, err := internalReapCache(projDir, refDir, filepath.Join(refDir, "partial"), filepath.Join(refDir, "claim"), cutoff, cutoff, 0, 0)
	if err == nil {
		t.Fatal("reap succeeded despite an undeletable entry")
	}
	// The failed removal must not be counted: no partial stale line is
	// printed and the entry is still on disk.
	if _, statErr := os.Stat(old); statErr != nil {
		t.Errorf("undeletable entry was reported removed: %v", statErr)
	}
}

// TestInternalReapCacheRejectsBadPaths pins the reap's derived-path
// contract: reference files that do not derive from the project dir are
// refused before any filesystem access.
func TestInternalReapCacheRejectsBadPaths(t *testing.T) {
	projDir := cacheProjPath(t)
	now := time.Now().UTC()
	if _, _, err := internalReapCache(projDir, projDir+"/wrong", projDir+"/wrong/partial", projDir+"/wrong/claim", now, now, 0, 0); err == nil {
		t.Error("reap accepted reference paths that do not derive from the project dir")
	}
	if _, _, err := internalReapCache(filepath.Join(t.TempDir(), "nope"), projDir+"/.vci-reap", projDir+"/.vci-reap/partial", projDir+"/.vci-reap/claim", now, now, 0, 0); err == nil {
		t.Error("reap accepted a project dir outside .vci/state/bundle-cache")
	}
}

// TestInternalReconstructNoSeed pins the seedless reconstruction: a payload
// tar carrying head and a full bundle is extracted into a private payload
// dir, the repository is initialized and imported, the head is checked out,
// and the payload dir is removed.
func TestInternalReconstructNoSeed(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "init", "-q")
	gitRun(t, src, "add", "a.txt")
	gitRun(t, src, "commit", "-q", "-m", "base")
	head := gitRun(t, src, "rev-parse", "HEAD")

	bundlePath := filepath.Join(t.TempDir(), "full.bundle")
	gitRun(t, src, "bundle", "create", bundlePath, "--all")
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}

	payload := writeTar(t,
		tarEntry{name: "head", mode: 0o644, typeflag: tar.TypeReg, content: head + "\n"},
		tarEntry{name: "bundle", mode: 0o644, typeflag: tar.TypeReg, content: string(bundle)},
		tarEntry{name: "lc.tar", mode: 0o644, typeflag: tar.TypeReg},
	)
	dir := workPath(t)
	if err := internalReconstruct([]string{dir, "--no-seed"}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("internal-reconstruct: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Errorf("checked-out file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vci-payload")); !os.IsNotExist(err) {
		t.Errorf("payload dir survived reconstruction: %v", err)
	}
}

// TestInternalReconstructSeeded pins the seeded reconstruction: the seed
// checkout is copied into the work dir with its own `.vci` state excluded,
// the durable local-change patch is applied before the f/ overlay is
// restored, and modes and symlinks survive.
func TestInternalReconstructSeeded(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".vci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".vci", "secret"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "seed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "init", "-q")
	gitRun(t, seed, "add", "seed.txt")
	gitRun(t, seed, "commit", "-q", "-m", "seed")
	head := gitRun(t, seed, "rev-parse", "HEAD")

	// A real diff for the durable patch: modify seed.txt in the working
	// tree, capture the diff, then restore. gitRun trims the trailing
	// newline, which git apply rejects, so it is restored.
	if err := os.WriteFile(filepath.Join(seed, "seed.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := gitRun(t, seed, "diff", "HEAD", "--binary") + "\n"
	gitRun(t, seed, "checkout", "--", "seed.txt")

	var lcBuf bytes.Buffer
	tw := tar.NewWriter(&lcBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "patch", Mode: 0o644, Size: int64(len(patch)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, patch); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "f/untracked.txt", Mode: 0o755, Size: 10, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("untracked\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "f/slink", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "seed.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	payload := writeTar(t,
		tarEntry{name: "head", mode: 0o644, typeflag: tar.TypeReg, content: head + "\n"},
		tarEntry{name: "lc.tar", mode: 0o644, typeflag: tar.TypeReg, content: lcBuf.String()},
	)
	dir := workPath(t)
	if err := internalReconstruct([]string{dir, seed}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("internal-reconstruct: %v", err)
	}

	// The patch applied, the f/ overlay landed with mode and symlink, the
	// seed's own .vci state was excluded, and the payload is gone.
	data, err := os.ReadFile(filepath.Join(dir, "seed.txt"))
	if err != nil || string(data) != "modified\n" {
		t.Errorf("patched file: %q, %v", data, err)
	}
	untrackedInfo, err := os.Stat(filepath.Join(dir, "untracked.txt"))
	if err != nil || untrackedInfo.Mode().Perm() != 0o755 {
		t.Errorf("f/ mode not preserved: %v, %v", untrackedInfo, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "slink")); err != nil {
		t.Errorf("f/ symlink missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vci", "secret")); !os.IsNotExist(err) {
		t.Errorf("seed .vci state leaked into the workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vci-payload")); !os.IsNotExist(err) {
		t.Errorf("payload dir survived reconstruction: %v", err)
	}
}

// TestInternalReconstructCacheFlags pins the cache-hit and admit paths:
// --use-cached seeds the repository from the entry bundle, --admit writes
// meta.json and the bundle and creates the complete marker last, and
// --evict drops the oldest claim-free entry beyond the limits.
func TestInternalReconstructCacheFlags(t *testing.T) {
	// Build a source repo and a bundle for a second commit, so the cached
	// entry holds base history and the payload adds head.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "init", "-q")
	gitRun(t, src, "add", "a.txt")
	gitRun(t, src, "commit", "-q", "-m", "base")
	base := gitRun(t, src, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", "a.txt")
	gitRun(t, src, "commit", "-q", "-m", "head")
	head := gitRun(t, src, "rev-parse", "HEAD")

	baseBundle := filepath.Join(t.TempDir(), "base.bundle")
	gitRun(t, src, "bundle", "create", baseBundle, "--all")
	bundleBytes, err := os.ReadFile(baseBundle)
	if err != nil {
		t.Fatal(err)
	}

	// A cache hit: the entry already holds the base history and the
	// complete marker, so --use-cached imports it and checks out head.
	entry := cacheEntryPath(t, base)
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "bundle"), bundleBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "meta.json"), []byte(`{"bytes":1,"last_used":"2026-08-09T12:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	payload := writeTar(t,
		tarEntry{name: "head", mode: 0o644, typeflag: tar.TypeReg, content: head + "\n"},
		tarEntry{name: "lc.tar", mode: 0o644, typeflag: tar.TypeReg},
	)
	dir := workPath(t)
	args := []string{dir, "--no-seed", "--cache", entry, "--use-cached"}
	if err := internalReconstruct(args, bytes.NewReader(payload)); err != nil {
		t.Fatalf("cache-hit reconstruct: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(data) != "head\n" {
		t.Errorf("cache-hit checkout: %q, %v", data, err)
	}
	// A hit refreshes the entry's recency.
	metaInfo, err := os.Stat(filepath.Join(entry, "meta.json"))
	if err != nil || time.Since(metaInfo.ModTime()) > time.Minute {
		t.Errorf("entry recency not refreshed on hit: %v, %v", metaInfo, err)
	}

	// A miss with admission: the entry starts empty, and --admit writes the
	// bundle, meta.json, and complete marker; --evict then drops an older
	// claim-free entry so the project stays within maxEntries.
	admitEntry := cacheEntryPath(t, head)
	projDir := filepath.Dir(admitEntry)
	old := filepath.Join(projDir, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	oldMeta := filepath.Join(old, "meta.json")
	if err := os.WriteFile(oldMeta, []byte(`{"bytes":9}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldMeta, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	admitPayload := writeTar(t,
		tarEntry{name: "head", mode: 0o644, typeflag: tar.TypeReg, content: head + "\n"},
		tarEntry{name: "bundle", mode: 0o644, typeflag: tar.TypeReg, content: string(bundleBytes)},
		tarEntry{name: "lc.tar", mode: 0o644, typeflag: tar.TypeReg},
	)
	dir2 := workPath(t)
	lastUsed := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	args = []string{dir2, "--no-seed", "--cache", admitEntry, "--admit", "42", lastUsed.Format(time.RFC3339), "--evict", "1", "0"}
	if err := internalReconstruct(args, bytes.NewReader(admitPayload)); err != nil {
		t.Fatalf("admit reconstruct: %v", err)
	}
	if _, err := os.Stat(filepath.Join(admitEntry, "complete")); err != nil {
		t.Errorf("complete marker missing after admission: %v", err)
	}
	metaData, err := os.ReadFile(filepath.Join(admitEntry, "meta.json"))
	if err != nil || string(metaData) != `{"bytes":42,"last_used":"2026-08-10T12:00:00Z"}` {
		t.Errorf("meta.json: %q, %v", metaData, err)
	}
	admitted, err := os.ReadFile(filepath.Join(admitEntry, "bundle"))
	if err != nil || !bytes.Equal(admitted, bundleBytes) {
		t.Errorf("admitted bundle differs: %v", err)
	}
	// The oldest claim-free entry beyond maxEntries was evicted.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("LRU victim not evicted: %v", err)
	}
}

// TestInternalReconstructRejectsBadInput pins the reconstruct argument and
// path validation: flags without values, cache flags without an entry, a
// missing seed, and out-of-tree work paths are all refused before any
// filesystem work.
func TestInternalReconstructRejectsBadInput(t *testing.T) {
	dir := workPath(t)
	entry := cacheEntryPath(t, "abc123")
	cases := []struct {
		name string
		args []string
	}{
		{"no workdir", []string{"--no-seed"}},
		{"too many positionals", []string{dir, "a", "b", "--no-seed"}},
		{"missing seed", []string{dir}},
		{"cache without value", []string{dir, "--no-seed", "--cache"}},
		{"use-cached without entry", []string{dir, "--no-seed", "--use-cached"}},
		{"admit without entry", []string{dir, "--no-seed", "--admit", "1", "2026-08-09T12:00:00Z"}},
		{"bad bundle bytes", []string{dir, "--no-seed", "--cache", entry, "--admit", "x", "2026-08-09T12:00:00Z"}},
		{"bad timestamp", []string{dir, "--no-seed", "--cache", entry, "--admit", "1", "not-a-time"}},
		{"bad work path", []string{filepath.Join(t.TempDir(), "x"), "--no-seed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := internalReconstruct(tc.args, bytes.NewReader(nil)); err == nil {
				t.Errorf("accepted args %q", tc.args)
			}
		})
	}
}

// TestWorkerPathResolutionHandlesTilde pins the tilde-rooted form a Windows
// cmd.exe ssh session passes through unexpanded: the worker resolves it
// against its own home and validates the resolved suffix.
func TestWorkerPathResolutionHandlesTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := resolveWorkerWorkDir("~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("resolve tilde work path: %v", err)
	}
	if want := filepath.Join(home, ".vci", "state", "work", "run_abc"); got != want {
		t.Errorf("work path = %q, want %q", got, want)
	}
	if _, err := resolveWorkerWorkDir("~/.vci/state/work/nope"); err == nil {
		t.Error("tilde work path with an invalid run segment accepted")
	}
	entry, err := resolveWorkerCacheEntry("~/.vci/state/bundle-cache/v1/Vci/abc123")
	if err != nil {
		t.Fatalf("resolve tilde cache entry: %v", err)
	}
	if want := filepath.Join(home, ".vci", "state", "bundle-cache", "v1", "Vci", "abc123"); entry != want {
		t.Errorf("cache entry = %q, want %q", entry, want)
	}
	if _, err := resolveWorkerCacheEntry("~/.vci/state/bundle-cache/v1/Vci/../bad"); err == nil {
		t.Error("cache entry with a dotdot segment accepted")
	}
}

// TestRunInternalProbeExitCode pins the probe miss as an intentional exit
// code: exit 1 with no error, while a hit exits 0.
func TestRunInternalProbeExitCode(t *testing.T) {
	entry := cacheEntryPath(t, "abc123")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := runInternal("internal-probe-cache", []string{entry}, nil, &stdout)
	if err != nil || code != 1 {
		t.Fatalf("probe miss: code=%d err=%v, want exit 1", code, err)
	}
	if err := os.WriteFile(filepath.Join(entry, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, err = runInternal("internal-probe-cache", []string{entry}, nil, &stdout)
	if err != nil || code != 0 {
		t.Fatalf("probe hit: code=%d err=%v, want exit 0", code, err)
	}
	if _, err := runInternal("internal-probe-cache", nil, nil, &stdout); err == nil {
		t.Error("probe accepted no entry argument")
	}
}

// TestRunInternalCommandWritesPlainOutput pins the non-JSON surface: a
// stage failure writes a diagnostic to stderr and returns a non-zero code
// without writing a JSON envelope to stdout.
func TestRunInternalCommandWritesPlainOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInternalCommand("internal-stage", []string{filepath.Join(t.TempDir(), "x")}, bytes.NewReader(nil), &stdout, &stderr)
	if code == 0 {
		t.Fatal("internal-stage on a bad path succeeded")
	}
	if stdout.Len() != 0 {
		t.Errorf("internal command wrote to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "internal-stage") {
		t.Errorf("stderr diagnostic missing command name: %q", stderr.String())
	}
}
