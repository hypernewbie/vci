package source

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestPackageUnpackageSubmissionReconstructs(t *testing.T) {
	seed := t.TempDir()
	runGit(t, seed, "init", "-q")
	if err := os.WriteFile(filepath.Join(seed, "src.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "secret.env"), []byte("leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "src.txt", "secret.env")
	runGit(t, seed, "commit", "-q", "-m", "base")
	base := gitSha(t, seed, "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "build.out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := t.TempDir()
	runGit(t, t.TempDir(), "clone", "-q", seed, client)
	if err := os.WriteFile(filepath.Join(client, "src.txt"), []byte("head"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, client, "add", "src.txt")
	runGit(t, client, "commit", "-q", "-m", "head")
	if err := os.WriteFile(filepath.Join(client, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := CaptureIdentity(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	lc, err := CaptureLocalChanges(context.Background(), client, process.Native{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	bundleRC, err := CreateBundle(context.Background(), client, base, id.Head, process.Native{})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	bundle, err := io.ReadAll(bundleRC)
	_ = bundleRC.Close()
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	sub := Submission{Head: id.Head, Base: id.Base, RemoteURL: id.RemoteURL, Have: base, Bundle: bundle, LocalChanges: lc}
	rc, err := PackageSubmission(sub)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	got, err := UnpackageSubmission(rc)
	if err != nil {
		t.Fatalf("unpackage: %v", err)
	}
	if got.Head != id.Head || got.Base != id.Base || got.Have != base {
		t.Fatalf("submission meta mismatch: %+v", got)
	}

	w := t.TempDir()
	if err := ReconstructWorkspace(context.Background(), seed, w, got.Head, bytes.NewReader(got.Bundle), got.LocalChanges, []string{"*.env"}, process.Native{}); err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	mustRead := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(w, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if got := mustRead("src.txt"); got != "head" {
		t.Fatalf("src.txt = %q, want head", got)
	}
	if got := mustRead("build.out"); got != "cached" {
		t.Fatalf("build.out = %q, want cached", got)
	}
	if got := mustRead("untracked.txt"); got != "new" {
		t.Fatalf("untracked.txt = %q, want new", got)
	}
	if _, err := os.Stat(filepath.Join(w, "secret.env")); !os.IsNotExist(err) {
		t.Fatalf("secret.env should be excluded: %v", err)
	}
}
