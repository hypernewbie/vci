package source

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
)

// safeCheckoutEnv builds a deterministic env for hosted git commands.
// Start from os.Environ() to preserve base vars, then override Git prompt behavior.
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
			// Override values replace inherited entries.
			continue
		}
		out = append(out, kv)
	}
	// Append overrides in fixed order for reproducible env snapshots.
	for _, k := range []string{"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_ASKPASS_REQUIRE"} {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// Checkout performs a pinned hosted checkout in a Vci-owned temp directory.
// Returns `l.TempDir()/vci-hosted-<rand>/<projectName>`.
// Caller must consume path before cleanup defer removes it.
//
// Uses fixed git args (no shell) and verifies HEAD equals pinned commit; any
// mismatch or command failure returns an error.
func Checkout(ctx context.Context, runner process.Runner, l model.Layout, projectName string, h config.ValidatedHosted) (string, error) {
	if !model.ValidName(projectName) {
		return "", fmt.Errorf("%w: project name %q is invalid", config.ErrHostedFallbackInvalid, projectName)
	}
	if h.URL == "" || h.Commit == "" {
		return "", fmt.Errorf("%w: validated URL or commit is empty", config.ErrHostedFallbackInvalid)
	}
	// Ensure Vci-owned temp root exists before checkout.
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
	// Create the project child directory under the temp parent.
	checkoutDir := filepath.Join(parent, projectName)
	if err := os.Mkdir(checkoutDir, 0o700); err != nil {
		_ = os.RemoveAll(parent)
		return "", fmt.Errorf("create hosted checkout: %w", err)
	}

	env := safeCheckoutEnv()
	rollback := func() { _ = os.RemoveAll(parent) }

	// Run git init with fixed args.
	if err := runHostedGit(ctx, runner, env, []string{"init", "-q", checkoutDir}, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git init: %v", config.ErrHostedSourceUnavailable, err)
	}

	// Register origin with fixed args; URL is already validated.
	if err := runHostedGit(ctx, runner, env, []string{"-C", checkoutDir, "remote", "add", "origin", h.URL}, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git remote add: %v", config.ErrHostedSourceUnavailable, err)
	}

	// Fetch pinned commit with fixed flags:
	// - disable hooks
	// - forbid file protocol
	// - force protocol v2
	// - depth=1, no tags
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

	// Checkout FETCH_HEAD in detached mode.
	checkoutArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-C", checkoutDir,
		"checkout", "--detach", "--quiet", "FETCH_HEAD",
	}
	if err := runHostedGit(ctx, runner, env, checkoutArgs, ""); err != nil {
		rollback()
		return "", fmt.Errorf("%w: git checkout: %v", config.ErrHostedSourceUnavailable, err)
	}

	// Read HEAD and compare against pinned commit.
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
	// Enforce exact commit equality; mismatch is integrity failure.
	if head != h.Commit {
		rollback()
		return "", fmt.Errorf("%w: HEAD %s does not match pinned %s", config.ErrHostedSourceIntegrityFailed, head, h.Commit)
	}
	return checkoutDir, nil
}

// runHostedGit executes one git command with safe env and returns
// context-rich errors from stderr/exit status.
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

// runHostedGitRead executes read-only git command with stdout capture.
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

// Clean validates hosted checkout path, normalizes removal permissions,
// and deletes only within expected hosted prefix.
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
	// Walk upward to vci-hosted-<rand> root; accept project child or parent.
	// Basename enforcement blocks sibling path traversal.
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
	// If tempDir is supplied, require target to resolve inside it.
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

// CleanUnder scopes cleanup to a specific Vci temp parent.
// Caller passes matching l.TempDir() to prevent traversal to sibling roots.
func CleanUnder(root, tempDir string) error {
	return cleanUnder(root, tempDir)
}
