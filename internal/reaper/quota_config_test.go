package reaper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

// TestSourceCacheQuotaFallsBackToDefault proves the reaper reports the
// documented default when the coordinator-owned setting is omitted.
// The default lives in DefaultSourceCacheBytes so a reaper-call
// without an explicit config returns that documented value.
func TestSourceCacheQuotaFallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := sourceCacheQuota(layout.Layout{Root: root}); got != DefaultSourceCacheBytes {
		t.Fatalf("default quota: want %d got %d", DefaultSourceCacheBytes, got)
	}
}

// TestSourceCacheQuotaHonorsConfiguredValue proves the reaper honors
// the coordinator-owned SourceCacheBytes setting. Client roots that
// attempt to set it are rejected at Validate.
func TestSourceCacheQuotaHonorsConfiguredValue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\nsource_cache_bytes = 8388608\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := sourceCacheQuota(layout.Layout{Root: root}); got != 8388608 {
		t.Fatalf("configured quota: want 8388608 got %d", got)
	}
}

// TestClientCannotSetSourceCacheQuota ensures the role-aware
// validator rejects a client root's attempt to set the source-cache
// quota. A misconfigured client root cannot dictate cache policy.
func TestClientCannotSetSourceCacheQuota(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = "remote-host"
	cfg.Retention.MaxBytes = config.DefaultRetention.MaxBytes
	cfg.Retention.SourceCacheBytes = 1024
	if err := config.Validate(cfg); err == nil {
		t.Fatalf("client root setting source-cache quota must be rejected")
	}
}

// TestCoordinatorSourceCacheQuotaAcceptsReasonableValue ensures the
// coordinator validator accepts a reasonable configured quota and
// rejects values below the documented minimum.
func TestCoordinatorSourceCacheQuotaAcceptsReasonableValue(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = config.OrchestratorSelf
	cfg.Machines = map[string]config.Machine{"mac-local": {}}
	cfg.Retention.MaxBytes = 1 << 30
	cfg.Retention.SourceCacheBytes = 1 << 20
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("coordinator reasonable quota: %v", err)
	}
	cfg.Retention.SourceCacheBytes = 4
	if err := config.Validate(cfg); err == nil {
		t.Fatalf("coordinator undersized quota must be rejected")
	}
}
