package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
)

func TestBuildFromSubmissionUsage(t *testing.T) {
	cases := [][]string{
		{"build", "--from-submission"},
		{"build", "--from-submission", "demo", "extra"},
		{"build", "/some/path", "--from-submission", "demo"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		code := Run(args, &out, &errOut)
		if code == 0 {
			t.Fatalf("%v: expected non-zero exit", args)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%v: not JSON: %v", args, err)
		}
		if resp.Error == nil || resp.Error.Code != "invalid_arguments" {
			t.Fatalf("%v: want invalid_arguments, got %+v", args, resp)
		}
	}
}

func TestBuildFromSubmissionReconstructsOverStdin(t *testing.T) {
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
	subBytes, err := io.ReadAll(subReader)
	if err != nil {
		t.Fatalf("read submission: %v", err)
	}

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := app.AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	command := []string{"sh", "-c", "grep -q '^head$' app.txt && test -f note.txt && test -f build.out && test ! -e secret.env"}
	if err := app.AddProject(l, "sub-seed", config.Project{Machines: []string{"mac-local"}, Command: command, ExcludedPaths: []string{"*.env"}}); err != nil {
		t.Fatal(err)
	}
	if err := app.UpdateMachine(l, "mac-local", func(m *config.Machine) error {
		m.SourcePaths = map[string]string{"sub-seed": seed}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	subFile, err := os.CreateTemp("", "vci-sub-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subFile.Write(subBytes); err != nil {
		t.Fatal(err)
	}
	subFile.Close()
	subIn, err := os.Open(subFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer subIn.Close()
	defer os.Remove(subFile.Name())
	origStdin := os.Stdin
	os.Stdin = subIn
	defer func() { os.Stdin = origStdin }()

	var out, errOut bytes.Buffer
	code := Run([]string{"build", "--from-submission", "sub-seed"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("build --from-submission failed (code %d): %s %s", code, out.String(), errOut.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("expected ok response: %+v", resp)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected object data: %#v", resp.Data)
	}
	if data["state"] != string(model.RunSucceeded) {
		t.Fatalf("expected succeeded; data=%+v", data)
	}
}
