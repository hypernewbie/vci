package app

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
)

// gitRun runs git in dir with a test-safe identity and returns trimmed stdout.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Vci Test",
		"GIT_AUTHOR_EMAIL=vci-test@example.com",
		"GIT_COMMITTER_NAME=Vci Test",
		"GIT_COMMITTER_EMAIL=vci-test@example.com",
	)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// gitRunErr is gitRun that reports failure only to the caller, for checks
// that must observe a nonzero git exit.
func gitRunErr(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Vci Test",
		"GIT_AUTHOR_EMAIL=vci-test@example.com",
		"GIT_COMMITTER_NAME=Vci Test",
		"GIT_COMMITTER_EMAIL=vci-test@example.com",
	)
	return cmd.Run()
}

// newTwoCommitRepo creates a fresh Git repo with a "base" commit that
// writes a.txt as "one\n" and a "head" commit that rewrites it as
// "two\n", returning the repo path, the base hash, and the head hash.
func newTwoCommitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "a.txt")
	gitRun(t, root, "commit", "-q", "-m", "base")
	base := gitRun(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "a.txt")
	gitRun(t, root, "commit", "-q", "-m", "head")
	head := gitRun(t, root, "rev-parse", "HEAD")
	return root, base, head
}

// payloadMembers reads a worker payload tar and returns its members in order.
func payloadMembers(t *testing.T, payload io.ReadCloser) []struct{ Name, Data string } {
	t.Helper()
	defer payload.Close()
	var members []struct{ Name, Data string }
	tr := tar.NewReader(payload)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read payload tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member %q: %v", h.Name, err)
		}
		members = append(members, struct{ Name, Data string }{h.Name, string(data)})
	}
	return members
}

func TestWorkerPayloadDeltaMembersAndBytes(t *testing.T) {
	ctx := context.Background()
	workspace, have, head := newTwoCommitRepo(t)

	// The durable LC archive is what PrepareFromSubmission persists: the
	// packaged local changes of a real client delta.
	client := t.TempDir()
	gitRun(t, client, "init", "-q")
	if err := os.WriteFile(filepath.Join(client, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, client, "add", "a.txt")
	gitRun(t, client, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(client, "a.txt"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "note.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc, err := source.CaptureLocalChanges(ctx, client, process.Native{})
	if err != nil {
		t.Fatalf("capture local changes: %v", err)
	}
	lcRC, err := source.PackageLC(lc)
	if err != nil {
		t.Fatalf("package local changes: %v", err)
	}
	lcBytes, err := io.ReadAll(lcRC)
	_ = lcRC.Close()
	if err != nil {
		t.Fatalf("read packaged local changes: %v", err)
	}
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	payload, err := workerPayload(ctx, workspace, have, head, lcPath)
	if err != nil {
		t.Fatalf("workerPayload: %v", err)
	}
	members := assertPayload(t, payload, head, lcBytes, true)

	// The bundle member must be a real Git bundle that reconstructs head in a
	// worker seeded only with have.
	bundleFile := filepath.Join(t.TempDir(), "worker.bundle")
	if err := os.WriteFile(bundleFile, []byte(members[1].Data), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := t.TempDir()
	gitRun(t, worker, "init", "-q")
	baseRC, err := source.CreateBundle(ctx, workspace, "", have, process.Native{})
	if err != nil {
		t.Fatalf("create base bundle: %v", err)
	}
	baseBytes, err := io.ReadAll(baseRC)
	_ = baseRC.Close()
	if err != nil {
		t.Fatalf("read base bundle: %v", err)
	}
	baseFile := filepath.Join(t.TempDir(), "base.bundle")
	if err := os.WriteFile(baseFile, baseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, worker, "bundle", "unbundle", baseFile)
	if err := gitRunErr(t, worker, "cat-file", "-e", head); err == nil {
		t.Fatal("head object present in worker before delta bundle applied")
	}
	gitRun(t, worker, "bundle", "verify", bundleFile)
	gitRun(t, worker, "bundle", "unbundle", bundleFile)
	if err := gitRunErr(t, worker, "cat-file", "-e", head); err != nil {
		t.Fatalf("head object missing after delta bundle: %v", err)
	}
}

// TestWorkerPayloadUsesSourceRootForDelta proves the delta bundle is derived
// from the submitted Git sourceRoot, not from the git-less submission
// workspace: workerPayload is handed the real checkout, and the payload still
// carries head, the delta bundle, and the durable lc.tar byte-for-byte.
func TestWorkerPayloadUsesSourceRootForDelta(t *testing.T) {
	ctx := context.Background()

	// The submitted sourceRoot is a real Git checkout holding base and head.
	sourceRoot, have, head := newTwoCommitRepo(t)

	// The submission workspace is a separate directory with no .git. The
	// current workerPayload signature requires no workspace value, so the
	// delta bundle must come entirely from sourceRoot.
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(err) {
		t.Fatal("workspace unexpectedly contains a .git entry")
	}

	// The durable local-change archive is a regular file on disk.
	lcBytes := []byte("durable-lc-archive\n")
	lcPath := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	payload, err := workerPayload(ctx, sourceRoot, have, head, lcPath)
	if err != nil {
		t.Fatalf("workerPayload: %v", err)
	}
	assertPayload(t, payload, head, lcBytes, true)
}

func TestWorkerPayloadOmitsBundleWhenWorkerHasHead(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "a.txt")
	gitRun(t, workspace, "commit", "-q", "-m", "base")
	head := gitRun(t, workspace, "rev-parse", "HEAD")

	lcBytes := []byte("not-a-real-lc-tar\n")
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	payload, err := workerPayload(ctx, workspace, head, head, lcPath)
	if err != nil {
		t.Fatalf("workerPayload: %v", err)
	}
	assertPayload(t, payload, head, lcBytes, false)
}

func TestWorkerPayloadErrors(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "a.txt")
	gitRun(t, workspace, "commit", "-q", "-m", "base")
	head := gitRun(t, workspace, "rev-parse", "HEAD")
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if payload, err := workerPayload(ctx, workspace, head, head, filepath.Join(t.TempDir(), "missing.tar")); err == nil {
		payload.Close()
		t.Fatal("workerPayload succeeded with a missing durable archive")
	}
	if payload, err := workerPayload(ctx, t.TempDir(), "", head, lcPath); err == nil {
		payload.Close()
		t.Fatal("workerPayload succeeded in a non-git workspace")
	}
}

// TestNoSeedPayloadCarriesFullBundle pins that noSeedPayload returns the
// submitted head, a full bundle covering the entire reachable history (a
// fresh worker with no seed can import it and reach head), and the durable
// lc.tar byte-for-byte, plus the bundle byte size.
func TestNoSeedPayloadCarriesFullBundle(t *testing.T) {
	ctx := context.Background()
	workspace, _, head := newTwoCommitRepo(t)

	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	payload, size, err := noSeedPayload(ctx, workspace, head, lcPath)
	if err != nil {
		t.Fatalf("noSeedPayload: %v", err)
	}
	members := assertPayload(t, payload, head, lcBytes, true)
	bundleFile := filepath.Join(t.TempDir(), "full.bundle")
	if err := os.WriteFile(bundleFile, []byte(members[1].Data), 0o600); err != nil {
		t.Fatal(err)
	}
	if int64(len(members[1].Data)) != size {
		t.Fatalf("bundle size = %d, want %d", size, len(members[1].Data))
	}

	// A fresh worker with no seed must be able to import the bundle and
	// check out head.
	worker := t.TempDir()
	gitRun(t, worker, "init", "-q")
	gitRun(t, worker, "bundle", "verify", bundleFile)
	gitRun(t, worker, "bundle", "unbundle", bundleFile)
	gitRun(t, worker, "checkout", "-q", head)
	if got := gitRun(t, worker, "rev-parse", "HEAD"); got != head {
		t.Fatalf("worker HEAD = %s, want %s", got, head)
	}
}

// memberNames flattens payload members to names for failure messages.
func memberNames(members []struct{ Name, Data string }) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
}

// assertPayload verifies a worker payload's member order, head member, and
// byte-for-byte lc.tar equality, and that the bundle member is present and
// non-empty when requireBundle is true, or forbidden when it is false. It
// returns the members so callers can assert distinct bundle content.
func assertPayload(t *testing.T, payload io.ReadCloser, head string, lcBytes []byte, requireBundle bool) []struct{ Name, Data string } {
	t.Helper()
	members := payloadMembers(t, payload)
	want := []string{"head", "bundle", "lc.tar"}
	if !requireBundle {
		want = []string{"head", "lc.tar"}
	}
	got := memberNames(members)
	if len(got) != len(want) {
		t.Fatalf("payload has %d members %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("payload member order %v, want %v", got, want)
		}
	}
	if members[0].Data != head+"\n" {
		t.Fatalf("payload head %q, want %q", members[0].Data, head+"\n")
	}
	if members[len(members)-1].Data != string(lcBytes) {
		t.Fatalf("payload %s member differs from the durable archive byte-for-byte", want[len(want)-1])
	}
	if requireBundle && members[1].Data == "" {
		t.Fatal("payload bundle member is empty; want a nonempty bundle")
	}
	return members
}

func TestSeededReconstructionEligible(t *testing.T) {
	lcFile := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcFile, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lcDir := t.TempDir()

	tests := []struct {
		name        string
		machine     config.Machine
		project     config.Project
		projectName string
		lcPath      string
		want        bool
	}{
		{
			name:        "valid",
			machine:     config.Machine{SourcePaths: map[string]string{"Vci": "/src/vci"}},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcFile,
			want:        true,
		},
		{
			name:        "missing source path",
			machine:     config.Machine{SourcePaths: map[string]string{"Other": "/src/other"}},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcFile,
			want:        false,
		},
		{
			name:        "exclusions present",
			machine:     config.Machine{SourcePaths: map[string]string{"Vci": "/src/vci"}},
			project:     config.Project{ExcludedPaths: []string{"vendor/"}},
			projectName: "Vci",
			lcPath:      lcFile,
			want:        false,
		},
		{
			name:        "missing LC path",
			machine:     config.Machine{SourcePaths: map[string]string{"Vci": "/src/vci"}},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      filepath.Join(t.TempDir(), "missing.tar"),
			want:        false,
		},
		{
			name:        "directory at LC path",
			machine:     config.Machine{SourcePaths: map[string]string{"Vci": "/src/vci"}},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcDir,
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seededReconstructionEligible(tt.machine, tt.project, tt.projectName, tt.lcPath); got != tt.want {
				t.Fatalf("seededReconstructionEligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNoSeedCacheEligibilityAndPolicy pins the no-seed bundle-cache gate and
// the effective bundle-cache policy resolution: a machine without a seeded
// source path qualifies only when the project declares no exclusions and the
// durable local-change archive is a regular file; and the machine's policy
// falls back to the documented defaults when bundle_cache is omitted (zero
// value) while overriding only the fields it sets.
func TestNoSeedCacheEligibilityAndPolicy(t *testing.T) {
	lcFile := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcFile, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lcDir := t.TempDir()

	eligibilityTests := []struct {
		name        string
		machine     config.Machine
		project     config.Project
		projectName string
		lcPath      string
		head        string
		want        bool
	}{
		{
			name:        "valid no-seed eligibility",
			machine:     config.Machine{},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcFile,
			head:        "abc123",
			want:        true,
		},
		{
			name:        "false with source path",
			machine:     config.Machine{SourcePaths: map[string]string{"Vci": "/src/vci"}},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcFile,
			head:        "abc123",
			want:        false,
		},
		{
			name:        "false with exclusions",
			machine:     config.Machine{},
			project:     config.Project{ExcludedPaths: []string{"vendor/"}},
			projectName: "Vci",
			lcPath:      lcFile,
			head:        "abc123",
			want:        false,
		},
		{
			name:        "false missing LC",
			machine:     config.Machine{},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      filepath.Join(t.TempDir(), "missing.tar"),
			head:        "abc123",
			want:        false,
		},
		{
			name:        "false directory LC",
			machine:     config.Machine{},
			project:     config.Project{},
			projectName: "Vci",
			lcPath:      lcDir,
			head:        "abc123",
			want:        false,
		},
	}
	for _, tt := range eligibilityTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := noSeedCacheEligible(tt.machine, tt.project, tt.projectName, tt.lcPath, tt.head); got != tt.want {
				t.Fatalf("noSeedCacheEligible() = %v, want %v", got, tt.want)
			}
		})
	}

	def := config.DefaultBundleCache()
	policyTests := []struct {
		name    string
		machine config.Machine
		want    config.BundleCachePolicy
	}{
		{
			name:    "policy default when machine.BundleCache is zero",
			machine: config.Machine{},
			want:    def,
		},
		{
			name:    "machine policy override",
			machine: config.Machine{BundleCache: config.BundleCachePolicy{MaxBytes: 1 << 20, AdmissionBytes: 1 << 10}},
			want:    config.BundleCachePolicy{MaxEntries: def.MaxEntries, MaxBytes: 1 << 20, AdmissionBytes: 1 << 10},
		},
	}
	for _, tt := range policyTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.EffectiveBundleCache(tt.machine.BundleCache); got != tt.want {
				t.Fatalf("config.EffectiveBundleCache() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestStageNoSeedReconstructOversizedBundleOneShot pins the no-seed
// reconstruction path when the full-history bundle exceeds the machine's
// bundle_cache.max_bytes: the transfer succeeds one-shot (zero cache spec)
// and returns an actionable warning naming the project, host, byte cap, and
// the seeded-reconstruction alternative, while the streamed shell stays
// entirely off the worker bundle-cache path.
func TestStageNoSeedReconstructOversizedBundleOneShot(t *testing.T) {
	ctx := context.Background()

	// A real small Git source root with base and head, so the cache key (the
	// submitted head's first parent) is non-empty and the full bundle covers
	// the reachable history.
	sourceRoot, _, head := newTwoCommitRepo(t)

	// The durable local-change archive is a regular file on disk; make it a
	// real tar so the coordinator-side archive is well-formed.
	var lcBuf bytes.Buffer
	lcTw := tar.NewWriter(&lcBuf)
	patch := []byte("diff --git a/a.txt b/a.txt\n")
	if err := lcTw.WriteHeader(&tar.Header{Name: "patch", Mode: 0o644, Size: int64(len(patch)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := lcTw.Write(patch); err != nil {
		t.Fatal(err)
	}
	if err := lcTw.Close(); err != nil {
		t.Fatal(err)
	}
	lcBytes := lcBuf.Bytes()
	lcPath := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Measure the full-history bundle the coordinator will emit so the test
	// can prove it is nonempty and therefore larger than the small constant
	// cache cap below.
	bundleRC, err := source.CreateBundle(ctx, sourceRoot, "", head, process.Native{})
	if err != nil {
		t.Fatalf("create full bundle: %v", err)
	}
	bundleBytes, err := io.ReadAll(bundleRC)
	_ = bundleRC.Close()
	if err != nil {
		t.Fatalf("read full bundle: %v", err)
	}
	if len(bundleBytes) == 0 {
		t.Fatal("full bundle is empty; want a nonempty bundle")
	}

	// The machine is no-seed eligible for the project (no source path seeded,
	// no exclusions, durable lc.tar present) but its bundle cache cannot hold
	// the emitted bundle: the cap is a small constant that the nonempty
	// full-history bundle provably exceeds.
	const maxBundleBytes = 64
	machine := config.Machine{
		Host:        "builder",
		BundleCache: config.BundleCachePolicy{MaxBytes: maxBundleBytes},
	}
	projectName := "Vci"

	// The bundle-cache probe reports a miss, so the full bundle is streamed
	// one-shot.
	runner := &noSeedRouteRunner{probeHit: false}
	warning, err := stageNoSeedReconstruct(ctx, runner, machine, projectName, lcPath, sourceRoot, head, "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageNoSeedReconstruct: %v", err)
	}

	// The oversized transfer must succeed and return an actionable warning
	// naming the project, host, byte cap, source path, and the one-shot
	// not-cached outcome.
	if warning == "" {
		t.Fatal("expected a non-empty warning for the oversized one-shot bundle")
	}
	for _, want := range []string{
		`project "Vci"`,
		`machine "builder" bundle_cache.max_bytes`,
		"source path",
		"one-shot",
		"not cached",
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning missing %q: %s", want, warning)
		}
	}

	// The scripted runner saw exactly the probe then the one-shot stream.
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2 (probe + stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) != 4 || probe.Args[0] != "builder" || probe.Args[1] != "vci" || probe.Args[2] != "internal-probe-cache" || !strings.HasPrefix(probe.Args[3], "~/.vci/state/bundle-cache/v1/Vci/") {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	stream := runner.commands[1]
	if stream.Executable != "ssh" || len(stream.Args) != 5 || stream.Args[0] != "builder" || stream.Args[2] != "internal-reconstruct" || stream.Args[4] != "--no-seed" {
		t.Errorf("unexpected stream command: %+v", stream)
	}

	// The streamed payload carries head, a nonempty full-history bundle, and
	// the durable lc.tar byte-for-byte.
	members := assertPayload(t, io.NopCloser(bytes.NewReader(runner.payload)), head, lcBytes, true)

	// The warning must cite the actual emitted byte sizes so an operator can
	// act on it.
	if !strings.Contains(warning, fmt.Sprintf("bundle is %d bytes", len(members[1].Data))) {
		t.Errorf("warning missing emitted bundle size %d: %s", len(members[1].Data), warning)
	}
	if !strings.Contains(warning, fmt.Sprintf("bundle_cache.max_bytes of %d", maxBundleBytes)) {
		t.Errorf("warning missing the configured max_bytes %d: %s", maxBundleBytes, warning)
	}

	// The one-shot command imports the payload bundle entry-free and never
	// touches the worker bundle cache: it carries no --cache flag and
	// references no workerCacheRoot path at all.
	for _, want := range []string{"vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed"} {
		if !slices.Contains(stream.Args, want) {
			t.Errorf("one-shot command missing %q: %q", want, stream.Args)
		}
	}
	if slices.Contains(stream.Args, "--cache") {
		t.Errorf("one-shot command must not reference the worker bundle cache: %q", stream.Args)
	}
	if strings.Contains(strings.Join(stream.Args, " "), workerCacheRoot) {
		t.Errorf("one-shot command must not reference the worker bundle cache (%q): %q", workerCacheRoot, stream.Args)
	}
}

// noSeedRouteRunner scripts the ssh invocations of the no-seed bundle-cache
// routes: the probe outcome is configured up front (zero exit is a hit,
// nonzero a miss), and the reconstruction stream is the only command that
// carries the payload on stdin, which is captured so tests can assert what
// was emitted. A configured streamErr makes that stream invocation fail so
// the caller falls back to full workspace staging.
type noSeedRouteRunner struct {
	probeHit  bool
	streamErr error
	commands  []process.Command
	payload   []byte
}

func (r *noSeedRouteRunner) Run(_ context.Context, command process.Command) (process.Result, error) {
	r.commands = append(r.commands, command)
	if len(r.commands) == 1 { // ProbeBundleCache: `ssh <host> vci internal-probe-cache <entry>`.
		if r.probeHit {
			return process.Result{ExitCode: 0}, nil
		}
		return process.Result{ExitCode: 1}, nil
	}
	if command.Stdin != nil { // StreamNoSeedReconstruct carries the payload on stdin.
		data, err := io.ReadAll(command.Stdin)
		if err != nil {
			return process.Result{}, err
		}
		r.payload = data
		if r.streamErr != nil {
			return process.Result{ExitCode: 0}, r.streamErr
		}
	}
	return process.Result{ExitCode: 0}, nil
}

// TestStageNoSeedReconstructRoutesCacheHitAndMiss pins the two worker
// bundle-cache routes of the no-seed reconstruction path against a real Git
// source root: a cache hit acquires and releases an active claim around the
// stream and runs a shell seeded from the cached entry bundle (no bundle
// payload requirement), while a cache miss whose full bundle clears the
// machine's AdmissionBytes streams the full bundle and runs admission plus
// LRU eviction shell steps. Neither route returns a warning.
func TestStageNoSeedReconstructRoutesCacheHitAndMiss(t *testing.T) {
	ctx := context.Background()

	// A real small Git source root with base and head, so the cache key (the
	// submitted head's first parent) is non-empty and the full bundle covers
	// the reachable history.
	sourceRoot, base, head := newTwoCommitRepo(t)

	// The durable local-change archive is a regular file on disk.
	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		probeHit     bool
		machine      config.Machine
		wantCommands int
	}{
		{
			name:         "cache hit acquires and releases a claim and streams the cached entry",
			probeHit:     true,
			machine:      config.Machine{Host: "builder"},
			wantCommands: 4,
		},
		{
			name:         "cache miss above admission bytes streams a full bundle with admit and evict",
			probeHit:     false,
			machine:      config.Machine{Host: "builder", BundleCache: config.BundleCachePolicy{MaxBytes: 1 << 30, AdmissionBytes: 1}},
			wantCommands: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &noSeedRouteRunner{probeHit: tt.probeHit}
			warning, err := stageNoSeedReconstruct(ctx, runner, tt.machine, "Vci", lcPath, sourceRoot, head, "~/.vci/state/work/run_abc")
			if err != nil {
				t.Fatalf("stageNoSeedReconstruct: %v", err)
			}
			if warning != "" {
				t.Fatalf("expected no warning, got %q", warning)
			}
			if len(runner.commands) != tt.wantCommands {
				t.Fatalf("runner invoked %d times, want %d", len(runner.commands), tt.wantCommands)
			}

			// Both routes begin with the same bundle-cache probe for the
			// submitted head's first parent.
			probe := runner.commands[0]
			if probe.Executable != "ssh" || len(probe.Args) != 4 || probe.Args[0] != "builder" || probe.Args[1] != "vci" || probe.Args[2] != "internal-probe-cache" || probe.Args[3] != "~/.vci/state/bundle-cache/v1/Vci/"+base {
				t.Errorf("unexpected probe command: %+v", probe)
			}

			if tt.probeHit {
				// A hit acquires an active claim before the stream and
				// releases it after, so eviction skips the entry while the
				// worker imports it.
				acquire := runner.commands[1]
				stream := runner.commands[2]
				release := runner.commands[3]
				entry := "~/.vci/state/bundle-cache/v1/Vci/" + base
				wantAcquire := []string{"builder", "vci", "internal-acquire-claim", entry, "run_abc"}
				if acquire.Executable != "ssh" || len(acquire.Args) != len(wantAcquire) {
					t.Errorf("unexpected acquire command: %+v", acquire)
				} else {
					for i := range wantAcquire {
						if acquire.Args[i] != wantAcquire[i] {
							t.Errorf("acquire arg %d: %q want %q", i, acquire.Args[i], wantAcquire[i])
						}
					}
				}
				wantRelease := []string{"builder", "vci", "internal-release-claim", entry, "run_abc"}
				if release.Executable != "ssh" || len(release.Args) != len(wantRelease) {
					t.Errorf("unexpected release command: %+v", release)
				} else {
					for i := range wantRelease {
						if release.Args[i] != wantRelease[i] {
							t.Errorf("release arg %d: %q want %q", i, release.Args[i], wantRelease[i])
						}
					}
				}
				wantStream := []string{"builder", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed", "--cache", entry, "--use-cached"}
				if stream.Executable != "ssh" || len(stream.Args) != len(wantStream) {
					t.Errorf("unexpected stream command: %+v", stream)
				} else {
					for i := range wantStream {
						if stream.Args[i] != wantStream[i] {
							t.Errorf("stream arg %d: %q want %q", i, stream.Args[i], wantStream[i])
						}
					}
				}
				// The hit command never asks the worker to admit or evict.
				for _, banned := range []string{"--admit", "--evict"} {
					if slices.Contains(stream.Args, banned) {
						t.Errorf("hit command must not admit or evict (%q): %q", banned, stream.Args)
					}
				}
				if runner.payload == nil {
					t.Error("cache hit did not stream a payload")
				}
				return
			}

			// A miss above AdmissionBytes streams the full bundle and runs
			// admission plus LRU eviction on the worker.
			stream := runner.commands[1]
			members := assertPayload(t, io.NopCloser(bytes.NewReader(runner.payload)), head, lcBytes, true)
			entry := "~/.vci/state/bundle-cache/v1/Vci/" + base
			wantPrefix := []string{
				"builder", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed",
				"--cache", entry,
				"--admit", fmt.Sprintf("%d", len(members[1].Data)),
			}
			if stream.Executable != "ssh" || len(stream.Args) < len(wantPrefix)+3 {
				t.Errorf("unexpected stream command: %+v", stream)
			} else {
				for i := range wantPrefix {
					if stream.Args[i] != wantPrefix[i] {
						t.Errorf("stream arg %d: %q want %q", i, stream.Args[i], wantPrefix[i])
					}
				}
				// The admission timestamp is generated at call time; the
				// eviction limits come from the machine's effective policy.
				tail := stream.Args[len(wantPrefix):]
				if len(tail) != 4 || tail[1] != "--evict" || tail[2] != "5" || tail[3] != "1073741824" {
					t.Errorf("miss command must run LRU eviction with the policy limits: %q", stream.Args)
				}
				if _, err := time.Parse(time.RFC3339, tail[0]); err != nil {
					t.Errorf("admission timestamp %q is not RFC3339: %v", tail[0], err)
				}
			}
		})
	}
}

// TestStageNoSeedReconstructBelowAdmissionOneShot pins the no-seed
// reconstruction path when the full-history bundle is below the machine's
// bundle_cache.admission_bytes: the probe reports a cache miss, the bundle
// fits MaxBytes, and because the admission threshold sits above the emitted
// bundle the stream is one-shot with a zero cache spec — no admission, no
// LRU eviction, and no warning — while the payload still carries the full
// bundle the worker needs.
func TestStageNoSeedReconstructBelowAdmissionOneShot(t *testing.T) {
	ctx := context.Background()

	// A real small Git source root with base and head, so the cache key (the
	// submitted head's first parent) is non-empty and the full bundle covers
	// the reachable history.
	sourceRoot, base, head := newTwoCommitRepo(t)

	// The durable local-change archive is a regular file on disk.
	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Measure the full-history bundle the coordinator will emit so the
	// machine's thresholds are provably relative to it: AdmissionBytes must
	// sit above the nonempty bundle while MaxBytes remains greater too, so
	// the bundle is cacheable by size yet below the admission threshold.
	bundleRC, err := source.CreateBundle(ctx, sourceRoot, "", head, process.Native{})
	if err != nil {
		t.Fatalf("create full bundle: %v", err)
	}
	bundleBytes, err := io.ReadAll(bundleRC)
	_ = bundleRC.Close()
	if err != nil {
		t.Fatalf("read full bundle: %v", err)
	}
	if len(bundleBytes) == 0 {
		t.Fatal("full bundle is empty; want a nonempty bundle")
	}

	// The machine is no-seed eligible for the project (no source path seeded,
	// no exclusions, durable lc.tar present). Its admission threshold exceeds
	// the emitted bundle, so the cache miss is streamed one-shot.
	machine := config.Machine{
		Host: "builder",
		BundleCache: config.BundleCachePolicy{
			MaxBytes:       1 << 30,
			AdmissionBytes: int64(len(bundleBytes)) + 1,
		},
	}
	projectName := "Vci"

	// The bundle-cache probe reports a miss, so the full bundle is streamed
	// one-shot below the admission threshold.
	runner := &noSeedRouteRunner{probeHit: false}
	warning, err := stageNoSeedReconstruct(ctx, runner, machine, projectName, lcPath, sourceRoot, head, "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageNoSeedReconstruct: %v", err)
	}
	if warning != "" {
		t.Fatalf("expected an empty warning below the admission threshold, got %q", warning)
	}

	// The scripted runner saw exactly the probe then the one-shot stream.
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2 (probe + stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) != 4 || probe.Args[0] != "builder" || probe.Args[1] != "vci" || probe.Args[2] != "internal-probe-cache" || probe.Args[3] != "~/.vci/state/bundle-cache/v1/Vci/"+base {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	stream := runner.commands[1]
	if stream.Executable != "ssh" || len(stream.Args) != 5 || stream.Args[0] != "builder" || stream.Args[2] != "internal-reconstruct" || stream.Args[4] != "--no-seed" {
		t.Errorf("unexpected stream command: %+v", stream)
	}

	// The streamed payload carries head, a nonempty full-history bundle, and
	// the durable lc.tar byte-for-byte.
	assertPayload(t, io.NopCloser(bytes.NewReader(runner.payload)), head, lcBytes, true)

	// The one-shot command imports the payload bundle entry-free and never
	// touches the worker bundle cache: no --cache flag, no admission, and no
	// LRU eviction.
	for _, want := range []string{"vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed"} {
		if !slices.Contains(stream.Args, want) {
			t.Errorf("one-shot command missing %q: %q", want, stream.Args)
		}
	}
	if slices.Contains(stream.Args, "--cache") || strings.Contains(strings.Join(stream.Args, " "), workerCacheRoot) {
		t.Errorf("one-shot command must not reference the worker bundle cache (%q): %q", workerCacheRoot, stream.Args)
	}
}

// TestStageNoSeedReconstructStreamErrorFallsBack pins that a no-seed
// reconstruction stream failure falls back to full workspace staging: for an
// otherwise eligible machine the bundle-cache probe reports a miss, the full
// bundle stream then fails with a configured error, and stageOrReconstruct
// returns an empty warning and nil error after StageRemote stages the whole
// workspace.
func TestStageNoSeedReconstructStreamErrorFallsBack(t *testing.T) {
	ctx := context.Background()

	// Sentinel ssh/tar stubs on PATH: the real StageRemote fallback execs
	// `tar` and `ssh` through PATH lookup, so the fallback appends to these
	// logs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	tarLog := filepath.Join(dir, "tar.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+tarLog+"\nexit 0\n")

	// A real small Git source root with base and head, so the cache key (the
	// submitted head's first parent) is non-empty and the full bundle covers
	// the reachable history.
	sourceRoot, base, head := newTwoCommitRepo(t)

	// The durable local-change archive is a regular file on disk.
	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// The machine is no-seed eligible for the project (no source path seeded,
	// no exclusions, durable lc.tar present), so stageOrReconstruct takes the
	// no-seed probe/stream path. The probe reports a miss; the stream then
	// fails with the configured error.
	runner := &noSeedRouteRunner{probeHit: false, streamErr: fmt.Errorf("stream failed")}
	machine := config.Machine{Host: "builder"}
	project := config.Project{}
	projectName := "Vci"
	workspace := t.TempDir()

	warning, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, sourceRoot, workspace, head, "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}
	if warning != "" {
		t.Fatalf("expected an empty warning on the fallback, got %q", warning)
	}

	// The runner saw exactly the probe then the failed stream.
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2 (probe + failed stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) != 4 || probe.Args[0] != "builder" || probe.Args[1] != "vci" || probe.Args[2] != "internal-probe-cache" || probe.Args[3] != "~/.vci/state/bundle-cache/v1/Vci/"+base {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	stream := runner.commands[1]
	if stream.Executable != "ssh" || len(stream.Args) != 5 || stream.Args[0] != "builder" || stream.Args[2] != "internal-reconstruct" || stream.Args[4] != "--no-seed" {
		t.Errorf("unexpected stream command: %+v", stream)
	}
	if len(runner.payload) == 0 {
		t.Fatal("StreamNoSeedReconstruct stdin was empty; expected the reconstruction payload tar")
	}

	// The failed stream fell back to StageRemote: ssh runs the
	// mkdir/cd/tar-extract shell and tar archives the local workspace.
	sshOut, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("ssh stub log: %v", err)
	}
	for _, want := range []string{"builder", "vci", "internal-stage", "~/.vci/state/work/run_abc"} {
		if !strings.Contains(string(sshOut), want) {
			t.Errorf("stage command missing %q: %s", want, sshOut)
		}
	}
	tarOut, err := os.ReadFile(tarLog)
	if err != nil {
		t.Fatalf("tar stub log: %v", err)
	}
	for _, want := range []string{"-cf", "-", "-C", workspace, "."} {
		if !strings.Contains(string(tarOut), want) {
			t.Errorf("tar invocation missing %q: %s", want, tarOut)
		}
	}
}
