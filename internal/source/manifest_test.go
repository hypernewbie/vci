package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/layout"
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

func TestBlobStoreProtectsAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root}
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
