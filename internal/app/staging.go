package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagingMeta stores cache identity for a staging run, not project input.
type stagingMeta struct {
	FormatVersion string
	Digest        string
	Project       string
}

// reconcileSourceCache selects how to source input for a run.
// It handles: complete cache entry, direct staging input, or local build.
// Non-nil release function must be deferred after capture completes.
func reconcileSourceCache(ctx context.Context, l model.Layout, quota int64, repoRoot, projectName string) (func(), error) {
	// 1) Completed cache entry.
	if digest, ok := cacheEntryDigestAt(l, repoRoot, projectName); ok {
		cacheRoot := l.SourceCacheDir()
		hit, _, err := sourcecache.IsHit(cacheRoot, digest, projectName)
		if err != nil {
			return nil, fmt.Errorf("source cache check failed: %w", err)
		}
		if !hit {
			return nil, fmt.Errorf("source cache entry %s is not complete", digest)
		}
		// Validate the tree still canonicalizes to its recorded digest before capture.
		if err := source.VerifySnapshot(repoRoot, digest); err != nil {
			return nil, fmt.Errorf("source cache entry %s failed verification: %w", digest, err)
		}
		claimID, err := newClaimID()
		if err != nil {
			return nil, fmt.Errorf("source cache claim: %w", err)
		}
		if err := sourcecache.AcquireActiveClaim(cacheRoot, digest, projectName, claimID); err != nil {
			return nil, fmt.Errorf("source cache claim: %w", err)
		}
		// Refresh last-use metadata; the reaper uses this metadata, not file mtimes.
		_ = sourcecache.UpdateLastUse(cacheRoot, digest, projectName, time.Now().UTC())
		return func() { sourcecache.ReleaseActiveClaim(cacheRoot, digest, projectName, claimID) }, nil
	}

	// 2) Direct staging tree in Vci temp directory.
	if metaPath, ok := stagingMetaPathAt(l, repoRoot); ok {
		meta, err := readStagingMeta(metaPath)
		if err != nil {
			return nil, fmt.Errorf("read staging meta: %w", err)
		}
		if meta.Project != projectName {
			return nil, fmt.Errorf("staged project %q does not match repository %q", meta.Project, projectName)
		}
		if meta.FormatVersion != sourcecache.FormatVersion {
			return nil, fmt.Errorf("staging format version %q is not supported", meta.FormatVersion)
		}
		// Recompute and verify the snapshot digest; mismatch is fatal to staging path.
		if err := source.VerifySnapshot(repoRoot, meta.Digest); err != nil {
			return nil, fmt.Errorf("source cache digest mismatch: %w", err)
		}
		// Publish the verified tree; non-admission failures are logged and
		// otherwise left non-fatal.
		if _, pubErr := sourcecache.PublishTree(l.SourceCacheDir(), meta.Digest, meta.Project, repoRoot, quota); pubErr != nil && !errors.Is(pubErr, sourcecache.ErrAdmissionRejected) {
			fmt.Fprintf(os.Stderr, "vci: source cache publish skipped: %v\n", pubErr)
		}
		return nil, nil
	}

	// 3) Ordinary local build.
	return nil, nil
}

// cacheEntryDigestAt returns the digest when repoRoot is exactly a
// Vci-owned cache entry under state/source-cache/v1/<digest>/<project>/<project>.
// Symlinks are resolved to prevent path-indirection hiding matches.
func cacheEntryDigestAt(l model.Layout, repoRoot, projectName string) (string, bool) {
	cacheRoot, err := filepath.EvalSymlinks(l.SourceCacheDir())
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(cacheRoot, realRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	// Expected layout is <ver>/<digest>/<project>/<project>; root is its parent.
	if len(parts) != 4 || parts[0] != sourcecache.FormatVersion {
		return "", false
	}
	if parts[2] != projectName || parts[3] != projectName || !sourcecache.ValidDigest(parts[1]) {
		return "", false
	}
	return parts[1], true
}

// stagingMetaPathAt returns the staging meta path for direct Vci temp trees
// in state/tmp/vci-source-*/<project>.
func stagingMetaPathAt(l model.Layout, repoRoot string) (string, bool) {
	tmpDir, err := filepath.EvalSymlinks(l.TempDir())
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", false
	}
	parent := filepath.Dir(realRoot)
	if filepath.Dir(parent) != tmpDir {
		return "", false
	}
	if !strings.HasPrefix(filepath.Base(parent), source.StagingPrefix) {
		return "", false
	}
	meta := filepath.Join(parent, source.StagingMetaName)
	if _, err := os.Stat(meta); err != nil {
		return "", false
	}
	return meta, true
}

// readStagingMeta parses and validates staging metadata.
func readStagingMeta(path string) (stagingMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stagingMeta{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return stagingMeta{}, fmt.Errorf("malformed staging meta at %s", path)
	}
	meta := stagingMeta{FormatVersion: fields[0], Digest: fields[1], Project: fields[2]}
	if meta.FormatVersion != sourcecache.FormatVersion {
		return stagingMeta{}, fmt.Errorf("unsupported staging format version %q", meta.FormatVersion)
	}
	if !sourcecache.ValidDigest(meta.Digest) {
		return stagingMeta{}, fmt.Errorf("staging meta has invalid digest %q", meta.Digest)
	}
	if !model.ValidName(meta.Project) {
		return stagingMeta{}, fmt.Errorf("staging meta has invalid project %q", meta.Project)
	}
	return meta, nil
}

// newClaimID returns a fresh random claim identifier for an active
// cache capture.
func newClaimID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "claim-" + hex.EncodeToString(raw[:]), nil
}

// safeCacheKey is a validated field set used to compose remote shell text.
type safeCacheKey struct {
	Digest  string // sha256-<64-lowercase-hex>
	Project string // model.ValidName
}

// validateCacheKey returns a safe key for remote-shell composition,
// or an error for invalid project/digest fields.
func validateCacheKey(digest, project string) (safeCacheKey, error) {
	if !model.ValidName(project) {
		return safeCacheKey{}, fmt.Errorf("repository name %q cannot name a remote project", project)
	}
	if !sourcecache.ValidDigest(digest) {
		return safeCacheKey{}, fmt.Errorf("source digest %q is not sha256-<64-lowercase-hex>", digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256-")); err != nil {
		return safeCacheKey{}, fmt.Errorf("source digest %q has invalid hex: %w", digest, err)
	}
	return safeCacheKey{Digest: digest, Project: project}, nil
}

// buildCacheProbeScript returns SSH text that checks for a completed cache entry.
// It uses validated key fields and the same sourcecache layout as publication.
func buildCacheProbeScript(key safeCacheKey) string {
	if !model.ValidName(key.Project) || !sourcecache.ValidDigest(key.Digest) {
		// Caller error: validateCacheKey rejects this upstream.
		return "exit 1"
	}
	parent := sourcecache.EntryRootRel(key.Digest, key.Project)
	tree := sourcecache.EntryTreeRel(key.Digest, key.Project)
	return fmt.Sprintf(`set -eu
ROOT="${VCI_ROOT:-$HOME/.vci}"
CACHE="$ROOT/state/source-cache"
PARENT="$CACHE/%s"
TREE="$CACHE/%s"
if [ -f "$PARENT/complete" ] && [ -f "$PARENT/meta.json" ]; then
	cd "$TREE"
	vci build .
else
	exit 42
fi
`, parent, tree)
}

// stagingShellScript returns shell steps for staging input and build.
// Staging directories are always cleaned on exit.
// `key` fields are validated before composition.
func stagingShellScript(key safeCacheKey) string {
	if !model.ValidName(key.Project) || !sourcecache.ValidDigest(key.Digest) {
		// Caller error: validateCacheKey rejects invalid keys upstream.
		return "exit 1"
	}
	prefix := "vci-source-" + key.Project
	return fmt.Sprintf(`set -eu
ROOT="${VCI_ROOT:-$HOME/.vci}"
TMP_PARENT="$ROOT/state/tmp"
mkdir -m 700 -p "$TMP_PARENT"
STAGING=$(mktemp -d -p "$TMP_PARENT" -t %s.XXXXXXXX)
chmod 700 "$STAGING"
trap 'rm -rf "$STAGING" 2>/dev/null || true' EXIT INT TERM
PROJECT="$STAGING/%s"
mkdir -m 700 "$PROJECT"
printf '%%s %%s %%s\n' '%s' '%s' '%s' > "$STAGING/vci-meta"
chmod 600 "$STAGING/vci-meta"
tar -C "$PROJECT" -xpf -
cd "$PROJECT"
vci build .
`, prefix, key.Project, sourcecache.FormatVersion, key.Digest, key.Project)
}
