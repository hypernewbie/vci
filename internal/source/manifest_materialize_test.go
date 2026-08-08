package source

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

func TestManifestIsDeterministicAndDoesNotDereferenceSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	first, blobs, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest changed: %s %s", first.Digest, second.Digest)
	}
	if len(blobs) != 1 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	for _, entry := range first.Entries {
		if entry.Path == "link" && entry.Target != outside {
			t.Fatalf("link: %+v", entry)
		}
	}
}

func TestManifestTracksGitWorkingTreeStates(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Vci Test", "GIT_AUTHOR_EMAIL=vci@example.invalid", "GIT_COMMITTER_NAME=Vci Test", "GIT_COMMITTER_EMAIL=vci@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deleted.txt"), []byte("gone"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("not a blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt", "deleted.txt", "link")
	git("commit", "-qm", "initial")
	clean, _, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	if entry := findEntry(clean, "tracked.txt"); entry == nil || entry.Kind != "file" {
		t.Fatalf("clean tracked entry: %+v", entry)
	}
	if entry := findEntry(clean, "link"); entry == nil || entry.Kind != "symlink" || entry.Target != target {
		t.Fatalf("clean symlink entry: %+v", entry)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	changed, blobs, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	if findEntry(changed, "deleted.txt") != nil || findEntry(changed, "untracked.txt") == nil {
		t.Fatalf("working tree entries: %+v", changed.Entries)
	}
	modified := findEntry(changed, "tracked.txt")
	if modified == nil || !bytes.Equal(blobs[modified.Digest], []byte("two")) {
		t.Fatalf("modified tracked entry: %+v", modified)
	}
	if link := findEntry(changed, "link"); link == nil || link.Kind != "symlink" {
		t.Fatalf("working symlink entry: %+v", link)
	}
}

func TestManifestRejectsChangeDuringFileCapture(t *testing.T) {
	for attempt := 0; attempt < 4; attempt++ {
		root := t.TempDir()
		file := filepath.Join(root, "large.bin")
		if err := os.WriteFile(file, bytes.Repeat([]byte("x"), 32<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			close(started)
			for {
				select {
				case <-stop:
					return
				default:
					_ = os.Chtimes(file, time.Now(), time.Now())
				}
			}
		}()
		<-started
		_, _, err := Build(root)
		close(stop)
		<-done
		if err != nil {
			return
		}
	}
	t.Fatal("source changes during capture were not detected")
}

func findEntry(manifest Manifest, name string) *Entry {
	for i := range manifest.Entries {
		if manifest.Entries[i].Path == name {
			return &manifest.Entries[i]
		}
	}
	return nil
}

func TestBlobStoreProtectsAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	l := model.Layout{Root: root}
	manifest, blobs, err := Build(filepath.Join("..", "..", "testdata", "repos", "go-pass"))
	if err != nil {
		t.Fatal(err)
	}
	store := BlobStore{Layout: l}
	if err := store.PutManifestAndBlobs(manifest, blobs); err != nil {
		t.Fatal(err)
	}
	if err := store.PutManifestAndBlobs(manifest, blobs); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeUsesIndependentWorkspaceFiles(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "testdata", "repos", "go-pass")
	manifest, blobs, err := Build(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := BlobStore{Layout: model.Layout{Root: t.TempDir()}}
	if err := store.PutManifestAndBlobs(manifest, blobs); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := store.Materialize(manifest, workspace); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(file, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Layout.BlobsDir(), manifestEntry(manifest, "main.go").Digest))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "changed" {
		t.Fatal("workspace mutation changed blob")
	}
}

func manifestEntry(m Manifest, path string) Entry {
	for _, entry := range m.Entries {
		if entry.Path == path {
			return entry
		}
	}
	return Entry{}
}
