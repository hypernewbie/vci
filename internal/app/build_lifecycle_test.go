package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/store"
)

func TestInitializeIsIdempotent(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	first, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	second, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != second.SchemaVersion {
		t.Fatalf("init changed config")
	}
}

func TestPrepareFromSubmissionPersistsLocalChangesArchive(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "lc-archive-seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitIn := func(dir string, args ...string) {
		t.Helper()
		cmd := process.Command{Executable: "git", Args: append([]string{"-C", dir}, args...)}
		if _, err := (process.Native{}).Run(context.Background(), cmd); err != nil {
			t.Fatalf("git -C %s %v: %v", dir, args, err)
		}
	}
	runGitIn(seed, "init", "-q")
	runGitIn(seed, "config", "user.email", "vci-test@example.com")
	runGitIn(seed, "config", "user.name", "vci-test")
	if err := os.WriteFile(filepath.Join(seed, "app.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitIn(seed, "add", "app.txt")
	runGitIn(seed, "commit", "-q", "-m", "base")

	client := filepath.Join(t.TempDir(), "lc-archive-client")
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"clone", "-q", seed, client}}); err != nil {
		t.Fatal(err)
	}
	runGitIn(client, "config", "user.email", "vci-test@example.com")
	runGitIn(client, "config", "user.name", "vci-test")
	if err := os.WriteFile(filepath.Join(client, "app.txt"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "note.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var baseOut strings.Builder
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"-C", seed, "rev-parse", "HEAD"}, Stdout: &baseOut}); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(baseOut.String())

	id, err := source.CaptureIdentity(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture identity: %v", err)
	}
	lc, err := source.CaptureLocalChanges(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture local changes: %v", err)
	}
	subReader, err := source.PackageSubmission(source.Submission{Head: id.Head, Base: id.Base, RemoteURL: id.RemoteURL, Have: base, LocalChanges: lc})
	if err != nil {
		t.Fatalf("package submission: %v", err)
	}
	submissionBytes, err := io.ReadAll(subReader)
	_ = subReader.Close()
	if err != nil {
		t.Fatalf("read submission: %v", err)
	}
	expected, err := lcArchiveMember(submissionBytes)
	if err != nil {
		t.Fatalf("extract lc.tar member: %v", err)
	}

	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "lc-archive-seed", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateMachine(l, "mac-local", func(m *config.Machine) error {
		m.SourcePaths = map[string]string{"lc-archive-seed": seed}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareFromSubmission(context.Background(), l, "lc-archive-seed", bytes.NewReader(submissionBytes))
	if err != nil {
		t.Fatalf("prepare from submission: %v", err)
	}
	lcPath, err := submissionLCPath(l, prepared.Record.ID)
	if err != nil {
		t.Fatalf("lc path: %v", err)
	}
	stored, err := os.ReadFile(lcPath)
	if err != nil {
		t.Fatalf("stored lc archive unavailable after prepare: %v", err)
	}
	if !bytes.Equal(stored, expected) {
		t.Fatalf("stored lc archive differs from the packaged submission lc member")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(lcPath)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("lc archive mode %o, want 600", mode)
		}
	}
}

// lcArchiveMember returns the literal bytes of the lc.tar entry inside a
// packaged submission, the exact archive PrepareFromSubmission must persist
// for the run.
func lcArchiveMember(submission []byte) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(submission))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("submission tar has no lc.tar member")
		}
		if err != nil {
			return nil, err
		}
		if h.Name != "lc.tar" {
			continue
		}
		return io.ReadAll(tr)
	}
}

func TestBuildAbortTerminatesOwnedCommand(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "abort-fixture")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture/abort\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"init", "-q", repo}}); err != nil {
		t.Fatal(err)
	}
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "abort-fixture", config.Project{Machines: []string{"mac-local"}, Command: []string{"sh", "-c", "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	var result BuildResult
	var buildErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		prepared, prepErr := Prepare(context.Background(), l, repo)
		if prepErr != nil {
			buildErr = prepErr
			return
		}
		result, buildErr = ExecutePrepared(context.Background(), l, prepared.Record.ID)
	}()
	deadline := time.Now().Add(10 * time.Second)
	var id model.RunID
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(l.RunsDir())
		if len(entries) > 0 {
			id = model.RunID(entries[0].Name())
			if record, err := (store.Store{Layout: l}).Load(id); err == nil && record.State == model.RunRunning {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("build never reached running")
	}
	if _, err := Abort(l, id); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if buildErr != nil {
		t.Fatalf("build: %v", buildErr)
	}
	if result.State != model.RunAborted {
		t.Fatalf("result: %+v", result)
	}
	checked, err := Check(l, id)
	if err != nil {
		t.Fatal(err)
	}
	if data, ok := checked.(map[string]any); !ok || data["state"] != string(model.RunAborted) {
		t.Fatalf("checked: %#v", checked)
	}
}

func TestBuildFixtureFailureIsAJobFailure(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "failure-fixture")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":       "module fixture/failure\n\ngo 1.26\n",
		"main.go":      "package main\nfunc main() {}\n",
		"main_test.go": "package main\nimport \"testing\"\nfunc TestFailure(t *testing.T) { t.Fatal(\"expected fixture failure\") }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"init", "-q", repo}}); err != nil {
		t.Fatal(err)
	}
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "failure-fixture", config.Project{Machines: []string{"mac-local"}, Command: []string{"go", "test", "./..."}}); err != nil {
		t.Fatal(err)
	}
	prepared, prepErr := Prepare(context.Background(), l, repo)
	if prepErr != nil {
		t.Fatal(prepErr)
	}
	result, err := ExecutePrepared(context.Background(), l, prepared.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || result.Failure != "job" || result.ExitCode == 0 {
		t.Fatalf("result: %+v", result)
	}
}

func TestLocalBuildReconstructsAndAppliesExclusions(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "excl-fixture")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture/excl\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "secret.env"), []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"init", "-q", repo}}); err != nil {
		t.Fatal(err)
	}
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	command := []string{"sh", "-c", "test ! -e secret.env && test -f main.go"}
	if err := AddProject(l, "excl-fixture", config.Project{Machines: []string{"mac-local"}, Command: command, ExcludedPaths: []string{"*.env"}}); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), l, repo)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Record.SourcePath == "" {
		t.Fatalf("local run should record a source path for reconstruction")
	}
	result, err := ExecutePrepared(context.Background(), l, prepared.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("expected reconstructed workspace without secret.env; state=%s failure=%s", result.State, result.Failure)
	}
}

func TestBuildFromSubmissionReconstructsFromSeedBundleAndLocalChanges(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "sub-seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitIn := func(dir string, args ...string) {
		t.Helper()
		cmd := process.Command{Executable: "git", Args: append([]string{"-C", dir}, args...)}
		if _, err := (process.Native{}).Run(context.Background(), cmd); err != nil {
			t.Fatalf("git -C %s %v: %v", dir, args, err)
		}
	}
	runGitIn(seed, "init", "-q")
	runGitIn(seed, "config", "user.email", "vci-test@example.com")
	runGitIn(seed, "config", "user.name", "vci-test")
	if err := os.WriteFile(filepath.Join(seed, "app.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "secret.env"), []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("build.out\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitIn(seed, "add", "app.txt", "secret.env", ".gitignore")
	runGitIn(seed, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(seed, "build.out"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := filepath.Join(t.TempDir(), "sub-client")
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"clone", "-q", seed, client}}); err != nil {
		t.Fatal(err)
	}
	runGitIn(client, "config", "user.email", "vci-test@example.com")
	runGitIn(client, "config", "user.name", "vci-test")
	if err := os.WriteFile(filepath.Join(client, "app.txt"), []byte("head"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitIn(client, "add", "app.txt")
	runGitIn(client, "commit", "-q", "-m", "head")
	if err := os.WriteFile(filepath.Join(client, "note.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var baseOut strings.Builder
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"-C", seed, "rev-parse", "HEAD"}, Stdout: &baseOut}); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(baseOut.String())

	id, err := source.CaptureIdentity(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture identity: %v", err)
	}
	lc, err := source.CaptureLocalChanges(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture local changes: %v", err)
	}
	bundleRC, err := source.CreateBundle(context.Background(), client, base, id.Head, process.Native{})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	bundle, err := io.ReadAll(bundleRC)
	_ = bundleRC.Close()
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	subReader, err := source.PackageSubmission(source.Submission{Head: id.Head, Base: id.Base, RemoteURL: id.RemoteURL, Have: base, Bundle: bundle, LocalChanges: lc})
	if err != nil {
		t.Fatalf("package submission: %v", err)
	}

	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	command := []string{"sh", "-c", "grep -q '^head$' app.txt && test -f note.txt && test -f build.out && test ! -e secret.env"}
	if err := AddProject(l, "sub-seed", config.Project{Machines: []string{"mac-local"}, Command: command, ExcludedPaths: []string{"*.env"}}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateMachine(l, "mac-local", func(m *config.Machine) error {
		m.SourcePaths = map[string]string{"sub-seed": seed}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := BuildFromSubmission(context.Background(), l, "sub-seed", subReader)
	if err != nil {
		t.Fatalf("build from submission: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("expected reconstructed build to succeed; state=%s failure=%s", result.State, result.Failure)
	}
}
