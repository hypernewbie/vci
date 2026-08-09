package source

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func newBundleRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "a.txt")
	runGit(t, root, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "b.txt")
	runGit(t, root, "commit", "-q", "-m", "head")
	return root
}

func mustCreateBundle(t *testing.T, root, have, head string) io.ReadCloser {
	t.Helper()
	rc, err := CreateBundle(context.Background(), root, have, head, process.Native{})
	if err != nil {
		t.Fatalf("CreateBundle(%q, %q): %v", have, head, err)
	}
	return rc
}

func readBundle(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close bundle: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("bundle is empty")
	}
	return data
}

func writeBundleFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateBundleFullAppliesToEmptyRepo(t *testing.T) {
	root := newBundleRepo(t)
	head := gitSha(t, root, "HEAD")
	file := writeBundleFile(t, readBundle(t, mustCreateBundle(t, root, "", head)))
	dst := t.TempDir()
	runGit(t, dst, "init", "-q")
	runGit(t, dst, "bundle", "unbundle", file)
	if _, err := runGitOutput(context.Background(), process.Native{}, dst, "cat-file", "-e", head); err != nil {
		t.Fatalf("head %q not present after unbundle: %v", head, err)
	}
}

func TestCreateBundleDeltaAppliesOverBase(t *testing.T) {
	root := newBundleRepo(t)
	base := gitSha(t, root, "HEAD^")
	head := gitSha(t, root, "HEAD")
	dst := t.TempDir()
	runGit(t, dst, "init", "-q")
	// Seed dst with base only: a full bundle of the base commit.
	baseFile := writeBundleFile(t, readBundle(t, mustCreateBundle(t, root, "", base)))
	runGit(t, dst, "bundle", "unbundle", baseFile)
	if _, err := runGitOutput(context.Background(), process.Native{}, dst, "cat-file", "-e", base); err != nil {
		t.Fatalf("base missing after seed: %v", err)
	}
	if _, err := runGitOutput(context.Background(), process.Native{}, dst, "cat-file", "-e", head); err == nil {
		t.Fatal("head already present before delta bundle")
	}
	// Apply the delta bundle; dst now gains head without re-receiving base.
	deltaFile := writeBundleFile(t, readBundle(t, mustCreateBundle(t, root, base, head)))
	runGit(t, dst, "bundle", "unbundle", deltaFile)
	if _, err := runGitOutput(context.Background(), process.Native{}, dst, "cat-file", "-e", head); err != nil {
		t.Fatalf("head missing after delta: %v", err)
	}
}

func TestCreateBundleEmptyRange(t *testing.T) {
	root := newBundleRepo(t)
	head := gitSha(t, root, "HEAD")
	_, err := CreateBundle(context.Background(), root, head, head, process.Native{})
	if !errors.Is(err, ErrBundleEmpty) {
		t.Fatalf("err = %v, want ErrBundleEmpty", err)
	}
}

func TestCreateBundleLeavesNoRefs(t *testing.T) {
	root := newBundleRepo(t)
	base := gitSha(t, root, "HEAD^")
	head := gitSha(t, root, "HEAD")
	readBundle(t, mustCreateBundle(t, root, base, head))
	out, err := runGitOutput(context.Background(), process.Native{}, root, "for-each-ref", "refs/vci")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	if out != "" {
		t.Fatalf("leftover vci refs after CreateBundle:\n%s", out)
	}
}

func TestCreateBundleRemovesTempOnClose(t *testing.T) {
	root := newBundleRepo(t)
	head := gitSha(t, root, "HEAD")
	rc := mustCreateBundle(t, root, "", head)
	br, ok := rc.(*bundleReader)
	if !ok {
		t.Fatalf("reader type %T, want *bundleReader", rc)
	}
	path := br.path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("temp file missing before close: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("temp file still exists after close")
	}
}
