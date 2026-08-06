package app

// Blocker 2 regression: the documented coordinator default
// (500 MB) must apply to admission before every publication, not only
// to the reaper. A config that omits retention.source_cache_bytes must
// not turn admission off.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// TestSourceCacheQuotaDefaultApplied proves the shared quota rule
// returns the documented default when retention.source_cache_bytes is
// omitted and the configured value when it is present. Prepare uses
// this same rule, so admission is bounded for default configs.
func TestSourceCacheQuotaDefaultApplied(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = config.OrchestratorSelf
	if got := reaper.SourceCacheQuota(cfg); got != reaper.DefaultSourceCacheBytes {
		t.Fatalf("omitted source_cache_bytes must default to %d, got %d", reaper.DefaultSourceCacheBytes, got)
	}
	cfg.Retention.SourceCacheBytes = 8 << 20
	if got := reaper.SourceCacheQuota(cfg); got != 8<<20 {
		t.Fatalf("configured quota must be honored, got %d", got)
	}
}

// TestDefaultQuotaRejectsOversizeSource proves admission under the
// documented default quota rejects an oversize incoming source: the
// entry is not published, and the rejection is an admission rejection,
// not a build failure.
func TestDefaultQuotaRejectsOversizeSource(t *testing.T) {
	root := t.TempDir()
	// A sparse source tree larger than the documented default quota.
	// treeSize uses stat only, so no bytes are read to reject it.
	src := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(reaper.DefaultSourceCacheBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	digest, err := snapshotDigestOf(t, src)
	if err != nil {
		t.Fatal(err)
	}
	published, err := sourcecache.PublishTree(root, digest, "demo", src, reaper.DefaultSourceCacheBytes)
	if err != sourcecache.ErrAdmissionRejected {
		t.Fatalf("oversize source must be rejected by the default quota admission, got err=%v", err)
	}
	if published {
		t.Fatalf("rejected entry must not be published")
	}
	if hit, _, _ := sourcecache.IsHit(root, digest, "demo"); hit {
		t.Fatalf("rejected entry must not be a hit")
	}
}

// TestOmittedSourceCacheBytesBoundedOverSSH proves the coordinator
// applies the documented default quota end-to-end when the config omits
// source_cache_bytes: a normal source still publishes (the default is a
// finite bound, not zero), and the maintenance report's limit equals the
// documented default.
func TestOmittedSourceCacheBytesBoundedOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	// Config without retention.source_cache_bytes (initCoordinatorRoot
	// format, which omits it).
	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "bounded by default\n")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, data.RunID)
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("a within-default-quota source must publish, got %v", digests)
	}
	assertCompleteEntry(t, fixture, digests[0], "demo")

	// The remote maintenance report must state the documented default
	// as the effective limit, proving admission was bounded on the
	// coordinator, not unbounded.
	out, stderr, err := fixture.ExecSSHCommand(ctx, "vci setup reap")
	if err != nil {
		t.Fatalf("remote setup reap failed: %v\nstderr:\n%s", err, stderr)
	}
	var report struct {
		OK   bool `json:"ok"`
		Data struct {
			Reaped struct {
				SourceCacheLimitBytes int64 `json:"source_cache_limit_bytes"`
			} `json:"reaped"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("decode reap report: %v\n%s", err, out)
	}
	if !report.OK {
		t.Fatalf("reap report not ok: %s", out)
	}
	if report.Data.Reaped.SourceCacheLimitBytes != reaper.DefaultSourceCacheBytes {
		t.Fatalf("omitted source_cache_bytes must yield the documented default limit, got %d", report.Data.Reaped.SourceCacheLimitBytes)
	}
}

// snapshotDigestOf computes the canonical snapshot digest of a plain
// tree directory (no git selection).
func snapshotDigestOf(t *testing.T, root string) (string, error) {
	t.Helper()
	canonical, err := source.CanonicalizeSnapshot(root)
	if err != nil {
		return "", err
	}
	return source.CanonicalDigest(canonical), nil
}
