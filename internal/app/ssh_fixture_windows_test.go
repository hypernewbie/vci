//go:build windows

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// SSHFixture stub for Windows where the loopback sshd fixture is not supported.
// The coordinator is not supported on Windows; tests that require it are skipped.
type SSHFixture struct {
	t               *testing.T
	homeDir         string
	binDir          string
	coordinatorRoot string
	binary          string
	// include fields accessed by tests so vet compiles, even though never populated
	port   int
	alias  string
	config string
}

func NewSSHFixture(t *testing.T) *SSHFixture {
	t.Helper()
	t.Skip("controlled ssh fixture not supported on Windows")
	return &SSHFixture{t: t}
}

func (f *SSHFixture) SSHAlias() string        { return "windows-stub" }
func (f *SSHFixture) CoordinatorRoot() string { return "" }
func (f *SSHFixture) BinaryPath() string      { return "" }

func (f *SSHFixture) ExecSSHCommand(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}
func (f *SSHFixture) ExecSSHVerbose(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

func (f *SSHFixture) waitForSSHRoundtrip(ctx context.Context) error { return nil }

// repoRoot returns the module root for Windows stubs. Duplicated from the
// Unix fixture so tests that call repoRoot(t) compile on Windows.
func repoRoot(t *testing.T) string {
	t.Helper()
	if dir, err := os.Getwd(); err == nil {
		for d := dir; d != "." && d != filepath.Dir(d); d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				return d
			}
		}
	}
	return "."
}
