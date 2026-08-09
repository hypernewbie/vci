package source

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hypernewbie/vci/internal/process"
)

// ErrNotGitRepository is returned when a source has no resolvable Git HEAD:
// it is not a Git repository, or it is a repository with no commits.
var ErrNotGitRepository = errors.New("source is not a Git repository")

// Identity is the Git identity of a checkout, used as the reconciliation
// handle a coordinator compares against its own state and configured remote.
type Identity struct {
	RemoteURL string
	Head      string
	Base      string
}

// CaptureIdentity reads a checkout's Git identity without mutating it. Head is
// the full HEAD object id. Base is the full id of HEAD's first parent and is
// empty for a root commit. RemoteURL is the configured origin URL and is empty
// when origin is not configured.
func CaptureIdentity(ctx context.Context, repoRoot string, runner process.Runner) (Identity, error) {
	head, err := runGitOutput(ctx, runner, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNotGitRepository, err)
	}
	base, _ := runGitOutput(ctx, runner, repoRoot, "rev-parse", "HEAD^")
	remote, _ := runGitOutput(ctx, runner, repoRoot, "remote", "get-url", "origin")
	return Identity{RemoteURL: remote, Head: head, Base: base}, nil
}

// runGitOutput runs git in repoRoot and returns its trimmed stdout. A runner
// error or non-zero exit is returned for the caller to interpret; callers that
// treat a command as optional ignore the error.
func runGitOutput(ctx context.Context, runner process.Runner, repoRoot string, args ...string) (string, error) {
	var out strings.Builder
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       append([]string{"-C", repoRoot}, args...),
		Stdout:     &out,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git %s exited %d", strings.Join(args, " "), res.ExitCode)
	}
	return strings.TrimSpace(out.String()), nil
}
