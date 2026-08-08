package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// dockerExit125Runner is a stub process.Runner that writes a fixed
// docker daemon message to stderr and returns a real
// *exec.ExitError with exit code 125, matching what the process
// runner returns when `docker run` itself fails on the daemon
// side. It never shells out to a real `docker` binary.
type dockerExit125Runner struct {
	stderr string
	err    error
}

func (r dockerExit125Runner) Run(_ context.Context, cmd process.Command) (process.Result, error) {
	if cmd.Stderr != nil && r.stderr != "" {
		_, _ = io.WriteString(cmd.Stderr, r.stderr)
	}
	return process.Result{ExitCode: 125}, r.err
}

// exitError125 manufactures a genuine *exec.ExitError with exit
// code 125. The docker executor classifies via errors.As on
// *exec.ExitError, so the stub must return the real type, not a
// look-alike.
func exitError125(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 125").Run()
	if err == nil {
		t.Fatal("expected sh to exit 125")
	}
	return err
}

// TestDockerExit125ImageNotFoundStderr pins that an exit-125 docker
// run whose stderr carries an image-not-found signature is
// classified ErrRuntimeImageNotFound (configuration,
// non-retryable). The stderr stream still reaches the caller's
// writer untouched.
func TestDockerExit125ImageNotFoundStderr(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "pull access denied",
			stderr: "docker: Error response from daemon: pull access denied for ghcr.io/org/ci:pin, repository does not exist or may require 'docker login': denied: requested access to the resource is denied.\n",
		},
		{
			name:   "manifest unknown",
			stderr: "docker: Error response from daemon: manifest for ghcr.io/org/ci:pin not found: manifest unknown: manifest unknown.\n",
		},
		{
			name:   "no such image",
			stderr: "docker: Error response from daemon: no such image: ghcr.io/org/ci:pin.\n",
		},
		{
			name:   "signatures match case-insensitively",
			stderr: "Docker: ERROR RESPONSE FROM DAEMON: PULL ACCESS DENIED for ghcr.io/org/ci:pin.\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			d := Docker{
				Image:  "ghcr.io/org/ci:pin",
				Runner: dockerExit125Runner{stderr: tc.stderr, err: exitError125(t)},
			}
			_, err := d.ExecuteSupervised(context.Background(), Request{
				Executable: "go",
				Workspace:  t.TempDir(),
				Stdout:     &out,
				Stderr:     &errOut,
			}, nil)
			if !errors.Is(err, ErrRuntimeImageNotFound) {
				t.Fatalf("err = %v, want ErrRuntimeImageNotFound", err)
			}
			if got := errOut.String(); got != tc.stderr {
				t.Fatalf("stderr not forwarded verbatim to caller: got %q want %q", got, tc.stderr)
			}
		})
	}
}

// TestDockerExit125OtherStderr pins that every exit-125 docker run
// failure without an image-not-found stderr signature — daemon
// outage, invalid mount source, registry timeout, empty stderr —
// is classified ErrRuntimeUnavailable (infrastructure, retryable),
// never ErrRuntimeImageNotFound.
func TestDockerExit125OtherStderr(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "daemon unreachable",
			stderr: "docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n",
		},
		{
			name:   "invalid mount source",
			stderr: "docker: Error response from daemon: create ghcr.io/org/ci:pin: not a directory\n",
		},
		{
			name:   "registry timeout",
			stderr: "docker: Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: request canceled while waiting for connection\n",
		},
		{
			name:   "empty stderr",
			stderr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			d := Docker{
				Image:  "ghcr.io/org/ci:pin",
				Runner: dockerExit125Runner{stderr: tc.stderr, err: exitError125(t)},
			}
			_, err := d.ExecuteSupervised(context.Background(), Request{
				Executable: "go",
				Workspace:  t.TempDir(),
				Stdout:     &out,
				Stderr:     &errOut,
			}, nil)
			if !errors.Is(err, ErrRuntimeUnavailable) {
				t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
			}
		})
	}
	// No stderr writer means the executor cannot observe any
	// signature, so even an image-not-found-shaped message must
	// fall back to the safe infrastructure class.
	t.Run("no stderr writer", func(t *testing.T) {
		d := Docker{
			Image: "ghcr.io/org/ci:pin",
			Runner: dockerExit125Runner{
				stderr: "docker: Error response from daemon: no such image: ghcr.io/org/ci:pin.\n",
				err:    exitError125(t),
			},
		}
		_, err := d.ExecuteSupervised(context.Background(), Request{
			Executable: "go",
			Workspace:  t.TempDir(),
		}, nil)
		if !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
		}
	})
}

// TestDockerExit125IsStillNotJobFailure pins that exit 125 is never
// swallowed as a job failure: both classifications return an error
// to the caller, so the build path can distinguish infrastructure
// from a completed command.
func TestDockerExit125IsStillNotJobFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	d := Docker{
		Image:  "ghcr.io/org/ci:pin",
		Runner: dockerExit125Runner{stderr: "docker: Error response from daemon: OCI runtime exec failed: container destroyed\n", err: exitError125(t)},
	}
	res, err := d.ExecuteSupervised(context.Background(), Request{
		Executable: "go",
		Workspace:  t.TempDir(),
		Stdout:     &out,
		Stderr:     &errOut,
	}, nil)
	if err == nil {
		t.Fatalf("expected an error, got exit %d", res.ExitCode)
	}
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
}
