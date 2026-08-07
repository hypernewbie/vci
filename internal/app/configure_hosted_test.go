package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

// TestSetHostedFallbackRoundTripsThroughValidate pins that a
// well-formed URL+commit round-trips through Validate, is persisted
// to disk, and reloads as the same pair.
func TestSetHostedFallbackRoundTripsThroughValidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/owner/repo.git"
	commit := "0123456789abcdef0123456789abcdef01234567"
	if err := SetHostedFallback(l, "demo", url, commit); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Projects["demo"].HostedFallback
	if got.URL != url || got.Commit != commit {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestSetHostedFallbackRejectsBadCommit pins that an invalid commit
// fails Validate before any disk write, returning
// ErrHostedFallbackInvalid so the operator can correct the typo.
func TestSetHostedFallbackRejectsBadCommit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	err := SetHostedFallback(l, "demo", "https://example.com/o/r.git", "main")
	if !errors.Is(err, config.ErrHostedFallbackInvalid) {
		t.Fatalf("want ErrHostedFallbackInvalid, got %v", err)
	}
	// Confirm the config was NOT mutated.
	cfg, _ := config.Load(l.ConfigPath())
	if cfg.Projects["demo"].HostedFallback.URL != "" || cfg.Projects["demo"].HostedFallback.Commit != "" {
		t.Fatalf("invalid commit persisted: %+v", cfg.Projects["demo"].HostedFallback)
	}
}

// TestClearHostedFallbackRequiresProject pins that clearing a
// missing project name is refused, not silently no-op'd.
func TestClearHostedFallbackRequiresProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	err := ClearHostedFallback(l, "missing")
	if err == nil {
		t.Fatal("missing project must fail")
	}
}

// TestSetAndClearHostedFallbackRequiresCoordinator pins that both
// helpers refuse to mutate a client root. The mutation callback is
// the rejection point; the pre-mutation Validate() runs first so
// SetHostedFallback also fails fast on bad URL/commit even from a
// client root.
func TestSetAndClearHostedFallbackRequiresCoordinator(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `schema_version = 1
orchestrator = "builder"
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	l := layout.Layout{Root: root}
	url := "https://example.com/o/r.git"
	commit := "0123456789abcdef0123456789abcdef01234567"
	if err := SetHostedFallback(l, "demo", url, commit); err == nil {
		t.Fatal("client root must be refused by Set")
	}
	if err := ClearHostedFallback(l, "demo"); err == nil {
		t.Fatal("client root must be refused by Clear")
	}
}
