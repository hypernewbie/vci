package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// TestReconcilePublishesStagingTree exercises the coordinator's public
// staging path directly: a staging tree under the Vci-owned temp root
// with a valid vci-meta record is verified and published into the
// source cache.
func TestReconcilePublishesStagingTree(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Build a source tree and its settled snapshot.
	projectRoot := filepath.Join(l.TempDir(), "vci-source-demo.ABC123", "demo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git", "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := source.ComputeSnapshotDigest(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(filepath.Dir(projectRoot), "vci-meta")
	if err := os.WriteFile(meta, []byte(sourcecache.FormatVersion+" "+digest+" demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := reconcileSourceCache(context.Background(), l, 1<<20, projectRoot, "demo")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if release != nil {
		release()
	}
	hit, _, err := sourcecache.IsHit(l.SourceCacheDir(), digest, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatalf("staging tree must be published as a complete entry")
	}
}

// TestReconcileRejectsDigestMismatch proves a staging tree whose bytes
// do not match the claimed digest is an infrastructure failure and
// leaves no complete entry.
func TestReconcileRejectsDigestMismatch(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(l.TempDir(), "vci-source-demo.DEF456", "demo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(filepath.Dir(projectRoot), "vci-meta")
	// Claim a digest that cannot match the tree.
	wrong := "sha256-0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(meta, []byte(sourcecache.FormatVersion+" "+wrong+" demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileSourceCache(context.Background(), l, 1<<20, projectRoot, "demo"); err == nil {
		t.Fatalf("digest mismatch must be an error")
	}
	entries, _ := os.ReadDir(l.SourceCacheDir())
	if len(entries) != 0 {
		t.Fatalf("mismatch must leave no cache state; got %v", entries)
	}
}

// TestReconcileHitPathValidatesAndClaims proves the cache-hit path
// validates the entry, refreshes last-use, and holds an active claim
// until released.
func TestReconcileHitPathValidatesAndClaims(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Publish a real entry via PublishTree from a source dir.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("cached\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := source.ComputeSnapshotDigest(src)
	if err != nil {
		t.Fatal(err)
	}
	published, err := sourcecache.PublishTree(l.SourceCacheDir(), digest, "demo", src, 1<<20)
	if err != nil || !published {
		t.Fatalf("publish: %v published=%v", err, published)
	}
	// The cache entry tree is the repo root for a hit build.
	entryRoot := sourcecache.EntryTreePath(l.SourceCacheDir(), digest, "demo")
	release, err := reconcileSourceCache(context.Background(), l, 1<<20, entryRoot, "demo")
	if err != nil {
		t.Fatalf("reconcile hit: %v", err)
	}
	if release == nil {
		t.Fatalf("hit path must return a claim release")
	}
	active, err := sourcecache.ActiveClaimsExist(l.SourceCacheDir(), digest, "demo")
	if err != nil || !active {
		t.Fatalf("active claim must be held during capture: %v", err)
	}
	release()
	active, _ = sourcecache.ActiveClaimsExist(l.SourceCacheDir(), digest, "demo")
	if active {
		t.Fatalf("active claim must be released after capture")
	}
}

// TestReconcileOrdinaryPathDoesNothing proves a normal local build
// path gets no cache behavior.
func TestReconcileOrdinaryPathDoesNothing(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	normal := filepath.Join(t.TempDir(), "normal-repo")
	if err := os.MkdirAll(normal, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := reconcileSourceCache(context.Background(), l, 1<<20, normal, "demo")
	if err != nil {
		t.Fatalf("reconcile normal: %v", err)
	}
	if release != nil {
		t.Fatalf("normal path must not return a claim")
	}
}
