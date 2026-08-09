package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
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
