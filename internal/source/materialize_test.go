package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/layout"
)

func TestMaterializeUsesIndependentWorkspaceFiles(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "testdata", "repos", "go-pass")
	manifest, blobs, err := Build(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := BlobStore{Layout: layout.Layout{Root: t.TempDir()}}
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
