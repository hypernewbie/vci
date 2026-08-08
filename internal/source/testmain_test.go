package source

import (
	"os"
	"testing"
)

// TestMain pins GIT_LFS_SKIP_SMUDGE=1 for the whole test process so
// git-lfs' smudge filter never runs during any git operation in this
// package, even on hosts where git-lfs is installed and configured
// globally (the Linux CI runner is one such host). LFS-touching tests
// commit and check out files attributed filter=lfs whose bytes are
// fake pointers with nonexistent oids; without this guard a host
// git-lfs would try to download the fake oid during clone/checkout
// (e.g. `git submodule add` cloning the child repo) and fail the
// setup before vci's own pointer detection runs. Skipping smudge
// keeps the pointer bytes as plain text so the pinned detection and
// rejection paths are what get exercised.
func TestMain(m *testing.M) {
	os.Setenv("GIT_LFS_SKIP_SMUDGE", "1")
	os.Exit(m.Run())
}
