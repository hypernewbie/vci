//go:build !windows

package app

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseScriptRejectsExistingOutputAndMissingTag(t *testing.T) {
	repo := releaseFixtureRepo(t)
	script := filepath.Join(repo, "scripts", "release.sh")
	cmd := exec.Command(script, "v0.1.0", ".")
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("release script accepted an existing output directory")
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("existing-output error = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatalf("release attempt damaged repository: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "internal", "version", "VERSION"), []byte("0.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", repo, "add", "internal/version/VERSION"},
		{"-C", repo, "-c", "user.name=Vci test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "next version"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("advance release fixture: %v\n%s", err, output)
		}
	}
	missing := filepath.Join(t.TempDir(), "new-output")
	cmd = exec.Command(script, "v0.1.1", missing)
	cmd.Dir = repo
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("release script accepted a missing tag")
	}
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Fatalf("missing-tag error = %q", stderr.String())
	}
}

func TestReleaseScriptProducesDeterministicArchivesAndManifest(t *testing.T) {
	repo := releaseFixtureRepo(t)
	script := filepath.Join(repo, "scripts", "release.sh")
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	for _, out := range []string{first, second} {
		cmd := exec.Command(script, "v0.1.0", out)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=1234567890")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("release %s: %v\n%s", out, err, output)
		}
	}
	firstEntries, err := os.ReadDir(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range firstEntries {
		if entry.IsDir() {
			t.Fatalf("temporary directory leaked into release output: %s", entry.Name())
		}
		left, err := os.ReadFile(filepath.Join(first, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(second, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("archive output %s is not deterministic", entry.Name())
		}
	}
	var manifest struct {
		Version   string `json:"version"`
		Artifacts []struct {
			Name string `json:"name"`
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"artifacts"`
	}
	data, err := os.ReadFile(filepath.Join(first, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "0.1.0" || len(manifest.Artifacts) != 5 {
		t.Fatalf("manifest = %+v", manifest)
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Name == "" || artifact.OS == "" || artifact.Arch == "" {
			t.Fatalf("incomplete artifact metadata: %+v", artifact)
		}
	}
}

func releaseFixtureRepo(t *testing.T) string {
	t.Helper()
	sourceRepo := repoRoot(t)
	fixture := filepath.Join(t.TempDir(), "repo")
	if err := copyReleaseTree(sourceRepo, fixture); err != nil {
		t.Fatalf("copy release fixture: %v", err)
	}
	for _, args := range [][]string{
		{"-C", fixture, "init", "-q"},
		{"-C", fixture, "config", "user.name", "Vci test"},
		{"-C", fixture, "config", "user.email", "test@example.invalid"},
		{"-C", fixture, "add", "-A"},
		{"-C", fixture, "commit", "-q", "-m", "release fixture"},
		{"-C", fixture, "tag", "v0.1.0"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("prepare release fixture git %v: %v\n%s", args, err, output)
		}
	}
	return fixture
}

func copyReleaseTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(destination, 0o755)
		}
		first := rel
		if index := strings.IndexRune(first, os.PathSeparator); index >= 0 {
			first = first[:index]
		}
		if first == ".git" || first == ".vci" || first == ".home" || first == ".tmp" || first == "state" || first == "dist" || first == "temp" || first == ".pi-subagents" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm())
	})
}
