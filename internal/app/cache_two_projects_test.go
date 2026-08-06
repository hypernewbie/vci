package app

// Blocker 1 regression: cache identity must be
// (format_version, digest, project) everywhere. Two repositories with
// identical selected content and different basenames must own
// independent entries under the shared digest: each builds, each
// retains a valid entry, each gets a cache hit on resubmission without
// a tar producer, and neither can invalidate the other.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// initTwoProjectCoordinatorRoot writes a coordinator config that owns
// two projects, demo and demo2, both running `true`.
func initTwoProjectCoordinatorRoot(t *testing.T, fixture *SSHFixture) {
	t.Helper()
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n\n[projects.demo2]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(fixture.coordinatorRoot, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write coordinator config: %v", err)
	}
}

// makeRepo creates a git repository with the given basename and a
// README.md with the given content, returning its path.
func makeRepo(t *testing.T, parent, basename, content string) string {
	t.Helper()
	sourceDir := filepath.Join(parent, basename)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), content)
	mustGitAddCommit(t, sourceDir, "init")
	return sourceDir
}

// clientSnapshotDigest replicates the client's settled-snapshot digest
// for a repo, returning the digest and the local snapshot root.
func clientSnapshotDigest(t *testing.T, sourceDir string) (string, string) {
	t.Helper()
	input, err := source.SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	snapshotParent := t.TempDir()
	snapshot, err := source.MaterializeSnapshot(input, snapshotParent)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	digest, err := source.ComputeSnapshotDigest(snapshot)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest, snapshot
}

// TestTwoProjectsSameContentIndependentEntriesOverSSH proves two
// repositories with identical selected content and different project
// basenames build and cache independently over the public client
// command: each publishes its own complete entry under the shared
// digest, each gets a cache hit on resubmission with no tar producer,
// and neither clobbers the other's metadata or marker.
func TestTwoProjectsSameContentIndependentEntriesOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initTwoProjectCoordinatorRoot(t, fixture)
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	parent := t.TempDir()
	repoA := makeRepo(t, parent, "demo", "identical content\n")
	repoB := makeRepo(t, parent, "demo2", "identical content\n")

	// Precondition: the two repos share one digest.
	digestA, snapshotA := clientSnapshotDigest(t, repoA)
	defer os.RemoveAll(snapshotA)
	digestB, snapshotB := clientSnapshotDigest(t, repoB)
	defer os.RemoveAll(snapshotB)
	if digestA != digestB {
		t.Fatalf("fixture requires identical selected content digests; got %s vs %s", digestA, digestB)
	}

	// First submission of each project publishes its own entry.
	envA1 := runClientBinary(t, fixture, clientRoot, "build", repoA)
	if !envA1.OK {
		t.Fatalf("build demo failed: %s", pretty(envA1))
	}
	var dataA1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envA1.Data, &dataA1); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataA1.RunID)

	envB1 := runClientBinary(t, fixture, clientRoot, "build", repoB)
	if !envB1.OK {
		t.Fatalf("build demo2 failed: %s", pretty(envB1))
	}
	var dataB1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envB1.Data, &dataB1); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataB1.RunID)

	// Exactly one digest dir, with two independent complete entries.
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 || digests[0] != digestA {
		t.Fatalf("expected one digest dir %s, got %v", digestA, digests)
	}
	assertCompleteEntry(t, fixture, digestA, "demo")
	assertCompleteEntry(t, fixture, digestA, "demo2")

	// The per-project metas must each name their own project.
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	for _, project := range []string{"demo", "demo2"} {
		hit, meta, err := sourcecache.IsHit(cacheRoot, digestA, project)
		if err != nil || !hit {
			t.Fatalf("project %s must retain a valid entry: hit=%v err=%v", project, hit, err)
		}
		if meta.Project != project {
			t.Fatalf("project %s meta must name %s, got %q", project, project, meta.Project)
		}
	}

	// Resubmission of each project must be a cache hit with no tar
	// producer, and neither resubmission may invalidate the other.
	flag := filepath.Join(t.TempDir(), "tar-called.flag")
	realTar := "/usr/bin/tar"
	if _, err := os.Stat(realTar); err != nil {
		t.Fatalf("locate real tar: %v", err)
	}
	fakeTar := filepath.Join(fixture.binDir, "tar")
	fakeBody := "#!/bin/sh\ntouch '" + flag + "'\nexec " + realTar + " \"$@\"\n"
	if err := os.WriteFile(fakeTar, []byte(fakeBody), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}

	envA2, stderrA2 := runClientBinaryCapture(t, fixture, clientRoot, "build", repoA)
	if !envA2.OK {
		t.Fatalf("demo resubmission failed: %s", pretty(envA2))
	}
	var dataA2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envA2.Data, &dataA2); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataA2.RunID)

	envB2, stderrB2 := runClientBinaryCapture(t, fixture, clientRoot, "build", repoB)
	if !envB2.OK {
		t.Fatalf("demo2 resubmission failed: %s", pretty(envB2))
	}
	var dataB2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envB2.Data, &dataB2); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataB2.RunID)

	if !strings.Contains(stderrA2, "source cache hit") || !strings.Contains(stderrB2, "source cache hit") {
		t.Fatalf("both resubmissions must be cache hits; A stderr:\n%s\nB stderr:\n%s", stderrA2, stderrB2)
	}
	if _, err := os.Stat(flag); err == nil {
		t.Fatalf("cache-hit resubmissions must not start the tar producer")
	}

	// Neither project invalidated the other: both entries are still
	// complete with their own metadata.
	assertCompleteEntry(t, fixture, digestA, "demo")
	assertCompleteEntry(t, fixture, digestA, "demo2")
	for _, project := range []string{"demo", "demo2"} {
		hit, meta, err := sourcecache.IsHit(cacheRoot, digestA, project)
		if err != nil || !hit || meta.Project != project {
			t.Fatalf("project %s entry must remain valid after the other project's resubmission: hit=%v meta=%v err=%v", project, hit, meta, err)
		}
	}
}
