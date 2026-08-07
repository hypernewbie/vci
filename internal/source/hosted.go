package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/process"
)

// safeCheckoutEnv returns the merged environment for every pinned
// git command. `process.Command.Env` REPLACES the inherited
// environment in os/exec (see internal/process/process.go:44-45), so
// we MUST start from os.Environ() to preserve PATH, HOME, and
// SSH_AUTH_SOCK; without PATH, git cannot be located. The three
// overrides disable terminal prompts and force any askpass lookup to
// fail rather than block on a passphrase prompt a coordinator never
// answers. No credential value is read here.
func safeCheckoutEnv() []string {
	overrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "true",
		"GIT_ASKPASS_REQUIRE": "force",
	}
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, override := overrides[name]; override {
			// Skip the inherited value; the override wins.
			continue
		}
		out = append(out, kv)
	}
	// Append overrides in deterministic order so a recorded env list
	// is comparable across runs.
	for _, k := range []string{"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_ASKPASS_REQUIRE"} {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// Checkout performs a pinned, single-branch, single-commit Git fetch
// and a detached checkout of the pinned commit into a fresh
// coordinator-owned temp directory. The returned root is
// `l.TempDir()/vci-hosted-<rand>/<projectName>`; that nested layout is
// what `source.Discover` expects to see so the existing source-graph
// pipeline runs unmodified.
//
// IMPORTANT: the caller must consume the checkout (read all bytes,
// materialize the snapshot, copy blobs) BEFORE the deferred
// CleanUnder runs. In the current PrepareHosted flow, CleanupUnder
// is deferred inside PrepareHosted, so once PrepareHosted returns,
// the checkout is gone — and the blob store already holds the
// materialized manifest + blob bytes.
//
// The checkout is created in a 0700 parent that `l.Ensure()` already
// protected. A hostile git error therefore cannot create world-readable
// artifacts. Every git command is composed as `process.Command` from
// exact arg slices — no shell, no string concatenation, no template
// interpolation except for the validated URL and commit (both already
// normalized by config.Validate()).
//
// The pinned commit is verified by reading `git rev-parse --verify
// HEAD` and comparing for exact lowercase equality. Any mismatch
// produces ErrHostedSourceIntegrityFailed and removes the checkout.
// Fetch, checkout, or context-cancellation failures wrap
// ErrHostedSourceUnavailable and remove the checkout. The caller must
// defer Clean(root) for every success path.
//
// The runner MUST honor `cmd.Env` verbatim. The hosted test fake
// replaces Native{}, so the production path is exercised end-to-end.
func Checkout(ctx context.Context, runner process.Runner, l layout.Layout, projectName string, h config.ValidatedHosted) (string, error) {
	if !layout.ValidName(projectName) {
		return "", fmt.Errorf("%w: project name %q is invalid", config.ErrHostedFallbackInvalid, projectName)
	}
	if h.URL == "" || h.Commit == "" {
		return "", fmt.Errorf("%w: validated URL or commit is empty", config.ErrHostedFallbackInvalid)
	}
	// The temp dir must exist and be Vci-owned before MkdirTemp runs.
	if err := l.Ensure(); err != nil {
		return "", fmt.Errorf("prepare hosted checkout dir: %w", err)
	}
	parent, err := os.MkdirTemp(l.TempDir(), HostedPrefix)
	if err != nil {
		return "", fmt.Errorf("create hosted parent: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		_ = os.RemoveAll(parent)
		return "", fmt.Errorf("protect hosted parent: %w", err)
	}
	// The project-named subdirectory is what `source.Discover` resolves
	// to. We Mkdir it explicitly so `git init` operates on a clean
	// directory owned by Vci, not on a rand-suffix name.
	checkoutDir := filepath.Join(parent, projectName)
	if err := os.Mkdir(checkoutDir, 0o700); err != nil {
		_ = os.RemoveAll(parent)
		return "", fmt.Errorf("create hosted checkout: %w", err)
	}

	env := safeCheckoutEnv()
	rollback := func() { _ = os.RemoveAll(parent) }

	// git init -q <checkoutDir> — bare-bones, no initial commit, no
	// branch. -q silences the "hint:" advice; the init succeeds in
	// every modern git and the args are positional.
	if err := runHostedGit(ctx, runner, env, []string{"init", "-q", checkoutDir}, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git init: %v", config.ErrHostedSourceUnavailable, err)
	}

	// git remote add origin <validated URL> — single shell-free arg
	// slice. The URL was normalized by config.Validate(), so a path
	// containing whitespace, query, or fragment has already been
	// rejected. No flag is permitted to inject -c here.
	if err := runHostedGit(ctx, runner, env, []string{"-C", checkoutDir, "remote", "add", "origin", h.URL}, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git remote add: %v", config.ErrHostedSourceUnavailable, err)
	}

	// git -c core.hooksPath=/dev/null -c protocol.file.allow=never
	//   -c protocol.version=2 fetch --depth=1 --no-tags origin <commit>
	// The three -c flags MUST precede the subcommand: hooks disabled,
	// file protocol forbidden, and a stable protocol version. The
	// pinned commit is the only ref requested.
	fetchArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.version=2",
		"-C", checkoutDir,
		"fetch", "--depth=1", "--no-tags", "origin", h.Commit,
	}
	if err := runHostedGit(ctx, runner, env, fetchArgs, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git fetch: %v", config.ErrHostedSourceUnavailable, err)
	}

	// git -c core.hooksPath=/dev/null checkout --detach --quiet FETCH_HEAD
	checkoutArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-C", checkoutDir,
		"checkout", "--detach", "--quiet", "FETCH_HEAD",
	}
	if err := runHostedGit(ctx, runner, env, checkoutArgs, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git checkout: %v", config.ErrHostedSourceUnavailable, err)
	}

	// git rev-parse --verify HEAD — must equal pinned commit.
	head, err := runHostedGitRead(ctx, runner, env, []string{"-C", checkoutDir, "rev-parse", "--verify", "HEAD"})
	if err != nil {
		rollback()
		return "", fmt.Errorf("%w: git rev-parse HEAD: %v", config.ErrHostedSourceUnavailable, err)
	}
	head = strings.TrimSpace(head)
	if head == "" {
		rollback()
		return "", fmt.Errorf("%w: empty HEAD after checkout", config.ErrHostedSourceIntegrityFailed)
	}
	// Exact lowercase equality. Both sides are validated lowercase
	// hex by config.Validate(), so a case-mismatched pin is a
	// genuine integrity failure (e.g. a hostile redirect or a
	// remote default-branch resolution); EqualFold would have
	// concealed it.
	if head != h.Commit {
		rollback()
		return "", fmt.Errorf("%w: HEAD %s does not match pinned %s", config.ErrHostedSourceIntegrityFailed, head, h.Commit)
	}
	return checkoutDir, nil
}

// runHostedGit executes one git command with the safe env. It
// captures stderr so a fake-runner failure surfaces as a typed error
// with context rather than an opaque exit code.
func runHostedGit(ctx context.Context, runner process.Runner, env, args []string, dir string) error {
	var stderr bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       args,
		Dir:        dir,
		Env:        env,
		Stderr:     &stderr,
	})
	if err != nil {
		return fmt.Errorf("%v (stderr=%q)", err, strings.TrimSpace(stderr.String()))
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d (stderr=%q)", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runHostedGitRead is runHostedGit plus stdout capture for read-only
// git commands. The caller trims the result.
func runHostedGitRead(ctx context.Context, runner process.Runner, env, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       args,
		Env:        env,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil {
		return "", fmt.Errorf("%v (stderr=%q)", err, strings.TrimSpace(stderr.String()))
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d (stderr=%q)", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Clean removes a previously-checked-out hosted root. It validates
// the root is under l.TempDir() (resolved through EvalSymlinks so
// macOS /tmp-style indirection cannot hide a match) and starts with
// HostedPrefix before chmod-then-RemoveAll. The chmod-then-remove
// shape is required because `.git/objects` files are read-only
// (`internal/app/cleanup.go:9-24`); plain RemoveAll fails on macOS
// when the directory is held under tight git perms.
//
// Defensive containment: Clean refuses to walk or remove any path that
// does not satisfy the prefix check, so a stray pointer to an
// adjacent file cannot coerce the cleanup helper into touching it.
// `tempDir` is the Vci-owned temp parent that owns the checkout;
// when empty, only the prefix match is applied (best-effort) but
// tests/callers always pass it.
func Clean(root string) error {
	return cleanUnder(root, "")
}

func cleanUnder(root, tempDir string) error {
	if root == "" {
		return fmt.Errorf("%w: empty root", config.ErrHostedSourceUnavailable)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: resolve root: %v", config.ErrHostedSourceUnavailable, err)
	}
	// Walk up to the vci-hosted-<rand> parent. The caller passes
	// either the project-named child (Checkout's return value) or
	// the parent; both shapes must be accepted. The basename check
	// is performed against the walked-up target so a forged pointer
	// to an arbitrary sibling file is still refused.
	target := real
	for {
		base := filepath.Base(target)
		if strings.HasPrefix(base, HostedPrefix) {
			break
		}
		parent := filepath.Dir(target)
		if parent == target {
			return fmt.Errorf("%w: %s is not inside a vci-hosted root", config.ErrHostedSourceIntegrityFailed, root)
		}
		target = parent
	}
	// When the caller knows the temp dir, also assert the resolved
	// target sits inside it. A path like /tmp/vci-hosted-decoy created
	// outside the Vci root is refused.
	if tempDir != "" {
		realTmp, terr := filepath.EvalSymlinks(tempDir)
		if terr == nil {
			rel, rerr := filepath.Rel(realTmp, target)
			if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: %s is outside the vci temp dir", config.ErrHostedSourceIntegrityFailed, target)
			}
		}
	}
	_ = filepath.WalkDir(target, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("%w: remove %s: %v", config.ErrHostedSourceUnavailable, target, err)
	}
	return nil
}

// CleanUnder is Clean scoped to a specific Vci-owned temp parent.
// The caller (typically PrepareHosted's defer) supplies the same
// `l.TempDir()` value used during Checkout so a forged basename
// cannot reach a sibling Vci root.
func CleanUnder(root, tempDir string) error {
	return cleanUnder(root, tempDir)
}

// errHostedCheckoutRemoved is a sentinel for the cleanup branch
// of Checkout when the parent cleanup succeeds. It is never returned
// to callers; it exists only to make the rollback chain testable.
var errHostedCheckoutRemoved = errors.New("hosted checkout removed")
