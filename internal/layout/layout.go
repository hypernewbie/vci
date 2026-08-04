package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type Layout struct{ Root string }

func Default() (Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return Layout{Root: filepath.Join(home, ".vci")}, nil
}

func (l Layout) ConfigPath() string   { return filepath.Join(l.Root, "config.toml") }
func (l Layout) StateDir() string     { return filepath.Join(l.Root, "state") }
func (l Layout) RunsDir() string      { return filepath.Join(l.StateDir(), "runs") }
func (l Layout) SourcesDir() string   { return filepath.Join(l.StateDir(), "sources") }
func (l Layout) BlobsDir() string     { return filepath.Join(l.SourcesDir(), "blobs") }
func (l Layout) ManifestsDir() string { return filepath.Join(l.SourcesDir(), "manifests") }
func (l Layout) WorkDir() string      { return filepath.Join(l.StateDir(), "work") }
func (l Layout) LocksDir() string     { return filepath.Join(l.StateDir(), "locks") }
func (l Layout) TempDir() string      { return filepath.Join(l.StateDir(), "tmp") }

func (l Layout) Ensure() error {
	if l.Root == "" {
		return fmt.Errorf("vci root is empty")
	}
	for _, dir := range []string{l.Root, l.StateDir(), l.RunsDir(), l.SourcesDir(), l.BlobsDir(), l.ManifestsDir(), l.WorkDir(), l.LocksDir(), l.TempDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("protect %s: %w", dir, err)
		}
	}
	return nil
}

func (l Layout) RunDir(id string) (string, error) {
	if !ValidName(id) {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return filepath.Join(l.RunsDir(), id), nil
}

func ValidName(value string) bool { return value != "" && safeName.MatchString(value) }
