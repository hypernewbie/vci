package app

// ssh_transport_suite_test.go consolidates the direct SSH transport
// test suite across three domains:
//
//   - direct_build_input_integration_test.go: end-to-end SSH path
//     verifying content, mode, symlink target, and path exclusion;
//   - direct_staging_integrity_test.go: the staged tree received by
//     the remote public command when `source.Discover` runs;
//   - remote_exec_test.go: fake ssh/scp stubs plus a loopback sshd
//     remote-bare build.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

// findStagingArtifacts lists staging dirs left under a coordinator
// state/tmp root (vci-source-* / vci-source.*). A completed build must
// leave none: the staging shell trap removes them on exit.
func findStagingArtifacts(tmpDir string) []string {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil
	}
	var left []string
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), "vci-source-") || strings.HasPrefix(e.Name(), "vci-source.")) {
			left = append(left, e.Name())
		}
	}
	return left
}

// ---- Source-integrity over SSH (direct_build_input_integration_test.go) ----

// TestDirectBuildInputSourceIntegrityOverSSH runs the full SSH path and
// verifies content, mode, symlink target, and path exclusion remotely.
func TestDirectBuildInputSourceIntegrityOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Coordinator script checks content, mode, symlink, and exclusions.
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test \"$(cat modified.txt)\" = 'modified content' && "+
			"test \"$(cat untracked.txt)\" = 'untracked' && "+
			"test ! -f deleted.txt && test ! -f ignored.log && test ! -d ignored_dir && "+
			"test ! -f .git/config && test -x script.sh && test -L link.txt && "+
			"test \"$(readlink link.txt)\" = 'tracked.txt' && "+
			"echo 'SOURCE INTEGRITY VERIFIED'")

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}

	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, ".git", "config"), "[user]\n  name = PrivateSentinelUser\n  email = sentinel@example.com\n")
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustWriteFile(t, filepath.Join(sourceDir, "tracked.txt"), "tracked")
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "original")
	mustWriteFile(t, filepath.Join(sourceDir, "deleted.txt"), "deleted")
	mustGitAddCommit(t, sourceDir, "init")

	// 1. Modify tracked file
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "modified content")
	// 2. Delete tracked file locally
	if err := os.Remove(filepath.Join(sourceDir, "deleted.txt")); err != nil {
		t.Fatalf("remove deleted.txt: %v", err)
	}
	// 3. Untracked non-ignored file
	mustWriteFile(t, filepath.Join(sourceDir, "untracked.txt"), "untracked")
	// 4. Ignored file and directory
	mustWriteFile(t, filepath.Join(sourceDir, ".gitignore"), "ignored.log\nignored_dir/\n")
	mustWriteFile(t, filepath.Join(sourceDir, "ignored.log"), "secret")
	if err := os.MkdirAll(filepath.Join(sourceDir, "ignored_dir"), 0o755); err != nil {
		t.Fatalf("mkdir ignored_dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(sourceDir, "ignored_dir", "cached.bin"), "bin")
	// 5. Executable file
	execPath := filepath.Join(sourceDir, "script.sh")
	mustWriteFile(t, execPath, "#!/bin/sh\necho hi")
	if err := os.Chmod(execPath, 0o755); err != nil {
		t.Fatalf("chmod script.sh: %v", err)
	}
	// 6. Symlink
	if err := os.Symlink("tracked.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build submission failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}

	// Read public check response through client
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build worker timed out")
		}
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
		if checkEnv.OK {
			var checkData struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					break
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build failed (observed facts check failed)")
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify no staging directory remains under remote Vci root
	rootTmp := filepath.Join(fixture.coordinatorRoot, "state", "tmp")
	stagingLeft := findStagingArtifacts(rootTmp)
	if len(stagingLeft) > 0 {
		t.Fatalf("staging directories were not cleaned up: %v", stagingLeft)
	}
}

func TestDirectBuildInputSpecialFilenamesOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Verify nested paths with spaces, quotes, and Unicode metacharacters
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test -f \"sub dir/file with space.txt\" && "+
			"test -f \"sub dir/special_quote'name.txt\" && "+
			"test -f \"sub dir/unicode_🚀_file.txt\" && "+
			"echo 'SPECIAL FILENAMES VERIFIED'")

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	subDir := filepath.Join(sourceDir, "sub dir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir sub dir: %v", err)
	}

	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustWriteFile(t, filepath.Join(subDir, "file with space.txt"), "space")
	mustWriteFile(t, filepath.Join(subDir, "special_quote'name.txt"), "quotes")
	mustWriteFile(t, filepath.Join(subDir, "unicode_🚀_file.txt"), "unicode")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build submission failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build worker timed out")
		}
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
		if checkEnv.OK {
			var checkData struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					break
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build failed for special filenames")
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- Staged-tree integrity (direct_staging_integrity_test.go) ----

// Phase 0 — direct staging recovery proof.
//
// These tests pin the staged tree the remote `vci build .` receives.
// The staging shell must extract the tar into the Vci project dir and
// run the public command from there, so the staged tree is the repo
// root when `source.Discover` runs.
//
// The probe replaces only the final remote `vci` invocation with a
// script that asserts the tree; everything before it is the real path.

// TestStagedTreeRecognizedBySourceDiscover runs the real remote
// `vci build .` to prove `source.Discover` accepts the staged .git
// markers.
func TestStagedTreeRecognizedBySourceDiscover(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "sh", "-c", "echo 'DISCOVER ACCEPTED STAGED TREE'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("real remote build must accept the staged tree; got %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}
	// A fan-out parent's persisted state stays `queued` until the
	// reaper terminalizes it; the live aggregate is recomputed on
	// demand by the coordinator's `check` path. Poll the public
	// envelope, not the on-disk parent record.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build %s did not succeed", data.RunID)
		}
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
		if checkEnv.OK {
			var checkData struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					break
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build %s failed against the staged tree", data.RunID)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- Remote-exec argv pinning (remote_exec_test.go) ----

// Plan 15 Phase 2/3 remote-execution tests: fake ssh/scp stubs in
// PATH plus one end-to-end remote-bare build over loopback sshd.
// No real docker daemon, tart VM, or remote machine needed.

// writePathStub writes an executable shell stub into dir and prepends
// dir to PATH.
func writePathStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
}

// setupRemoteRoot initializes a coordinator root with one remote
// machine (host=builder) and one project.
func setupRemoteRoot(t *testing.T, machine config.Machine, project config.Project) model.Layout {
	t.Helper()
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-remote", machine); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", project); err != nil {
		t.Fatal(err)
	}
	return l
}

// executeChild reserves the first child of a prepared build request and runs
// it to completion, since dispatch is a coordinator-owned step these tests do
// not exercise directly.
func executeChild(t *testing.T, ctx context.Context, l model.Layout, parentID model.RunID) (BuildResult, model.RunID, error) {
	t.Helper()
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return BuildResult{}, "", err
	}
	runStore := store.Store{Layout: l}
	children, err := runStore.LoadChildren(parentID)
	if err != nil || len(children) == 0 {
		return BuildResult{}, "", fmt.Errorf("no children: %v", err)
	}
	childID := children[0].ID
	if err := scheduler.Reserve(l, runStore, cfg, children[0].Machine, childID, time.Now().UTC()); err != nil {
		return BuildResult{}, childID, err
	}
	result, err := ExecutePrepared(ctx, l, childID)
	return result, childID, err
}

// TestRemoteBareBuildViaFakeSSH pins the remote-bare happy path:
// workspace staged via fake ssh, `true` run remotely, succeeded run on
// mac-remote. The ssh argv carries the host alias, workspace, and
// runtime binary.
func TestRemoteBareBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	l := setupRemoteRoot(t, config.Machine{Host: "builder"}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, childID, err := executeChild(t, context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	if result.Machine != "mac-remote" {
		t.Errorf("machine: %q", result.Machine)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{
		"builder",
		"~/.vci/state/work/" + string(childID),
		"'true'",
		"rm -rf -- ~/.vci/state/work/" + string(childID),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// No Vci state or ssh leakage beyond the workspace path.
	for _, banned := range []string{"VCI_ROOT", ".ssh"} {
		if strings.Contains(s, banned) {
			t.Errorf("leaked %q: %s", banned, s)
		}
	}
}

// TestRemoteDockerBuildViaFakeSSH pins the remote docker `docker run`
// argv with the remote workspace as the only mount.
func TestRemoteDockerBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	image := "ghcr.io/org/ci:pin"
	l := setupRemoteRoot(t, config.Machine{Host: "builder", Runtime: "docker", Image: image}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, childID, err := executeChild(t, context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	runID := string(childID)
	for _, want := range []string{
		"exec 'docker' 'run' '--rm'",
		`"$__vci_login_home"/.vci/state/work/` + runID + ":/vci/work:ro",
		"'ghcr.io/org/ci:pin'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// Mount source resolves via the captured login home, not isolated HOME.
	if strings.Contains(s, ".home/.vci") {
		t.Errorf("docker mount resolved through isolated HOME: %s", s)
	}
	// Exactly one mount, and it is the workspace.
	if strings.Count(s, "-v") != 1 {
		t.Errorf("expected exactly one -v mount: %s", s)
	}
}

// TestRemoteVMBuildViaFakeSSH pins the remote vm `tart run` argv.
func TestRemoteVMBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	snapshot := "ghcr.io/org/vm:pin"
	l := setupRemoteRoot(t, config.Machine{Host: "builder", Runtime: "vm", Snapshot: snapshot}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, childID, err := executeChild(t, context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	runID := string(childID)
	for _, want := range []string{"exec 'tart' 'run' '--no-gui'", `"$__vci_login_home"/.vci/state/work/` + runID + ":/vci/work", "'ghcr.io/org/vm:pin'", "--", "'true'"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// Mount source resolves via the captured login home, not isolated HOME.
	if strings.Contains(s, ".home/.vci") {
		t.Errorf("tart mount resolved through isolated HOME: %s", s)
	}
}

// TestRemoteBuildCollectsArtifactsViaFakeSCP pins the remote artifact
// contract: the workspace is fetched back with `scp` and the local
// collector publishes matches. The fake scp stub materializes the
// fetched tree so CollectArtifacts has real bytes.
func TestRemoteBuildCollectsArtifactsViaFakeSCP(t *testing.T) {
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	scpLog := filepath.Join(dir, "scp.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	// Fake scp: lay the fetched tree at <dest>/<run>/build/out.bin.
	scpStub := `#!/bin/sh
echo "$*" >> ` + scpLog + `
remote=""
for a in "$@"; do
  case "$a" in
    *:*) remote="$a" ;;
  esac
done
last=""
for a in "$@"; do last="$a"; done
run="${remote##*/}"
mkdir -p "$last/$run/build"
printf 'fake-zip-bytes' > "$last/$run/build/out.bin"
exit 0
`
	writePathStub(t, dir, "scp", scpStub)

	l := setupRemoteRoot(t, config.Machine{Host: "builder"}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}, Artifacts: []string{"build/*"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, childID, err := executeChild(t, context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	scpLogData, _ := os.ReadFile(scpLog)
	s := string(scpLogData)
	ws := "~/.vci/state/work/" + string(childID)
	if !strings.Contains(s, "builder:"+ws) {
		t.Errorf("scp log missing remote source %q: %s", ws, s)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0] != "build/out.bin" {
		t.Fatalf("artifacts: %v", result.Artifacts)
	}
	if result.ArtifactsTruncated {
		t.Errorf("artifacts_truncated=true under cap")
	}
	// The collected artifact is durable on the coordinator.
	runDir, err := l.RunDir(string(childID))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(runDir, "artifacts", "build", "out.bin")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("durable artifact missing: %v", err)
	}
}

// TestRemoteBareBuildViaSSHDFixture drives the real loopback sshd:
// stages the workspace, runs `true` remotely, publishes a succeeded
// run. Skips when ssh/sshd are unavailable.
func TestRemoteBareBuildViaSSHDFixture(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	l := setupRemoteRoot(t, config.Machine{Host: fixture.SSHAlias()}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, childID, err := executeChild(t, context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	if result.Machine != "mac-remote" {
		t.Errorf("machine: %q", result.Machine)
	}
	// Remote workspace must have been cleaned up.
	remoteWork := filepath.Join(fixture.homeDir, ".vci", "state", "work", string(childID))
	if _, err := os.Stat(remoteWork); !os.IsNotExist(err) {
		t.Errorf("remote workspace turd remains: %v", err)
	}
}
