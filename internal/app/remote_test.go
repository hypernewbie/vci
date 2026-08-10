package app

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "a.txt")
	gitRun(t, workspace, "commit", "-q", "-m", "base")
	have := gitRun(t, workspace, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "a.txt")
	gitRun(t, workspace, "commit", "-q", "-m", "head")
	head := gitRun(t, workspace, "rev-parse", "HEAD")

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
	members := payloadMembers(t, payload)
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3 (head, bundle, lc.tar): %v", len(members), memberNames(members))
	}
	if members[0].Name != "head" || members[1].Name != "bundle" || members[2].Name != "lc.tar" {
		t.Fatalf("member order %v, want [head bundle lc.tar]", memberNames(members))
	}
	if members[0].Data != head+"\n" {
		t.Fatalf("head member %q, want %q", members[0].Data, head+"\n")
	}
	if members[2].Data != string(lcBytes) {
		t.Fatal("lc.tar member differs from the durable archive byte-for-byte")
	}

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
	sourceRoot := t.TempDir()
	gitRun(t, sourceRoot, "init", "-q")
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceRoot, "add", "a.txt")
	gitRun(t, sourceRoot, "commit", "-q", "-m", "base")
	have := gitRun(t, sourceRoot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceRoot, "add", "a.txt")
	gitRun(t, sourceRoot, "commit", "-q", "-m", "head")
	head := gitRun(t, sourceRoot, "rev-parse", "HEAD")

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
	members := payloadMembers(t, payload)
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3 (head, bundle, lc.tar): %v", len(members), memberNames(members))
	}
	if members[0].Name != "head" || members[1].Name != "bundle" || members[2].Name != "lc.tar" {
		t.Fatalf("member order %v, want [head bundle lc.tar]", memberNames(members))
	}
	if members[0].Data != head+"\n" {
		t.Fatalf("head member %q, want %q", members[0].Data, head+"\n")
	}
	if members[1].Data == "" {
		t.Fatal("bundle member is empty; expected a delta bundle derived from sourceRoot")
	}
	if members[2].Data != string(lcBytes) {
		t.Fatal("lc.tar member differs from the durable archive byte-for-byte")
	}
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
	members := payloadMembers(t, payload)
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (head, lc.tar): %v", len(members), memberNames(members))
	}
	if members[0].Name != "head" || members[1].Name != "lc.tar" {
		t.Fatalf("member order %v, want [head lc.tar]", memberNames(members))
	}
	if members[0].Data != head+"\n" {
		t.Fatalf("head member %q, want %q", members[0].Data, head+"\n")
	}
	if members[1].Data != string(lcBytes) {
		t.Fatal("lc.tar member differs from the durable archive byte-for-byte")
	}
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

// memberNames flattens payload members to names for failure messages.
func memberNames(members []struct{ Name, Data string }) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
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
