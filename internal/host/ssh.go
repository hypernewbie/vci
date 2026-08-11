// Package host transfers workspaces over SSH and runs commands remotely.
// It stages workspace state and handles remote execution via system tools.
package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
)

// ValidateHost checks the SSH host argument before use.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("remote host is empty")
	}
	return config.ValidateMachineHost(host)
}

// ValidateRemotePath checks remote paths are valid `~/.vci/state/work/<run_id>` values.
func ValidateRemotePath(p string) error {
	if p == "" {
		return fmt.Errorf("remote path is empty")
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("remote path %q looks like an option flag", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("remote path %q contains control characters", p)
		}
	}
	segments := strings.Split(p, "/")
	if len(segments) != 5 {
		return fmt.Errorf("remote path %q is not under ~/.vci/state/work", p)
	}
	for i, fixed := range []string{"~", ".vci", "state", "work"} {
		if segments[i] != fixed {
			return fmt.Errorf("remote path %q is not under ~/.vci/state/work", p)
		}
	}
	if !model.ValidName(segments[4]) {
		return fmt.Errorf("remote path %q has invalid run segment %q", p, segments[4])
	}
	return nil
}

// RemoteWorkDir returns the remote `~/.vci/state/work/<run>` path for a run.
// Layout matches the coordinator's local work directory layout.
func RemoteWorkDir(id model.RunID) (string, error) {
	if !model.ValidRunID(id) {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return "~/.vci/state/work/" + string(id), nil
}

// Client runs remote commands over SSH through a process.Runner.
// Callers must set Runner; the zero value has no runner to invoke.
type Client struct {
	Runner process.Runner
}

// RunRemote executes a remote command over SSH.
// It returns the remote exit code; transport failures return an error.
// osKind selects the remote shell wrapper ("windows" uses cmd.exe;
// empty or anything else uses the POSIX sh/bash wrapper).
func (c Client) RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, osKind string, stdout, stderr io.Writer) (int, error) {
	if err := ValidateHost(host); err != nil {
		return 0, err
	}
	shell, err := composeShell(workDir, argv, env, osKind)
	if err != nil {
		return 0, err
	}
	if c.Runner == nil {
		return 0, fmt.Errorf("ssh runner is required")
	}
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, shell}, Stdout: stdout, Stderr: stderr})
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return result.ExitCode, nil
		}
		return 0, fmt.Errorf("ssh %s: %w", host, err)
	}
	return result.ExitCode, nil
}

// RunRemote executes a remote command over SSH with the native runner.
func RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, osKind string, stdout, stderr io.Writer) (int, error) {
	return (Client{Runner: process.Native{}}).RunRemote(ctx, host, workDir, argv, env, osKind, stdout, stderr)
}

// ProbeSeedHead asks a remote machine which commit is checked out at
// its seed path, running `ssh <host> git -C <seed> rev-parse HEAD`
// through the runner, where the seed is rendered as a `$HOME`-rooted
// shell word. It returns the trimmed stdout on success. A
// nonzero remote exit means the seed is not a Git checkout and yields
// an empty head; only ssh-level failures with no remote exit are
// reported as transport errors.
func (c Client) ProbeSeedHead(ctx context.Context, host, seed string) (string, error) {
	if err := ValidateHost(host); err != nil {
		return "", err
	}
	if err := validateSeed(seed); err != nil {
		return "", err
	}
	if c.Runner == nil {
		return "", fmt.Errorf("ssh runner is required")
	}
	var stdout bytes.Buffer
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, "git", "-C", shellSeed(seed), "rev-parse", "HEAD"},
		Stdout:     &stdout,
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil && result.ExitCode == 0 {
		return "", fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(stdout.String()), nil
}

// validateSeed rejects seed paths that could be misread as ssh option
// flags or carry shell-injected control bytes.
func validateSeed(seed string) error {
	if seed == "" {
		return fmt.Errorf("seed path is empty")
	}
	if strings.HasPrefix(seed, "-") {
		return fmt.Errorf("seed path %q looks like an option flag", seed)
	}
	for _, r := range seed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("seed path %q contains control characters", seed)
		}
	}
	return nil
}

// StreamReconstruct sends a worker payload tar over SSH stdin to the remote
// `vci internal-reconstruct` subcommand. The worker seeds a VCI-owned work
// directory from the machine's seed checkout, imports the bundle the payload
// carries (when present), checks out the submitted head, applies the durable
// local-change archive, and removes the extracted payload. Any remote failure
// is an error so the caller can fall back to full workspace staging.
func (c Client) StreamReconstruct(ctx context.Context, host, seed, workDir string, payload io.Reader) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if payload == nil {
		return fmt.Errorf("reconstruction payload is required")
	}
	if c.Runner == nil {
		return fmt.Errorf("ssh runner is required")
	}
	if err := validateSeed(seed); err != nil {
		return err
	}
	if err := ValidateRemotePath(workDir); err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, "vci", "internal-reconstruct", workDir, shellSeed(seed)},
		Stdin:      payload,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
		}
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
	}
	return nil
}

// ---- Worker bundle-cache transport ----

// cacheLayoutVersion is the on-disk layout segment of the worker bundle
// cache, mirroring bundlecache.Version so coordinator-composed shells address
// the same entries the bundlecache package manages.
const cacheLayoutVersion = "v1"

// NoSeedCacheSpec describes how one no-seed reconstruction stream treats the
// worker's bundle cache. An empty Root disables every cache behavior and
// yields a plain one-shot reconstruction.
type NoSeedCacheSpec struct {
	Root        string    // worker-side cache root (e.g. ~/.vci/state/bundle-cache)
	Project     string    // cache project segment
	Base        string    // cache entry key (a commit id)
	UseCached   bool      // hit: seed the repository from the cached bundle
	Admit       bool      // miss: write the streamed bundle into the entry
	Evict       bool      // run LRU eviction after admission
	MaxEntries  int       // per-project entry cap; <=0 means unlimited
	MaxBytes    int64     // per-project byte cap; <=0 means unlimited
	BundleBytes int64     // bundle size written into meta.json on admission
	Now         time.Time // admission timestamp; zero means time.Now
}

// ValidCacheSegment reports whether s is a single safe cache path segment:
// letters, digits, '.', '-', '_', no slashes, and not "." or "..". Project,
// base, and claim ids must all be such segments before they are composed
// into a remote path. The worker-side cache subcommands validate their
// segment arguments with the same rule the coordinator applies.
func ValidCacheSegment(s string) bool { return validCacheSegment(s) }

func validCacheSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `\/`) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func validateCacheSegment(what, s string) error {
	if !validCacheSegment(s) {
		return fmt.Errorf("cache %s %q must be a single path segment of letters, digits, '.', '-', '_'", what, s)
	}
	return nil
}

// cacheProjectDir returns the remote cache project directory
// <root>/v1/<project>, validating the root and the project segment the same
// way cacheEntryPath does.
func cacheProjectDir(cacheRoot, project string) (string, error) {
	if cacheRoot == "" {
		return "", fmt.Errorf("cache root is empty")
	}
	if err := validateCacheSegment("project", project); err != nil {
		return "", err
	}
	return cacheRoot + "/" + cacheLayoutVersion + "/" + project, nil
}

// cacheEntryPath returns the remote cache entry directory
// <root>/v1/<project>/<base>, validating every segment. The path is composed
// with "/" because it is a remote shell word, never a local filesystem path.
func cacheEntryPath(cacheRoot, project, base string) (string, error) {
	projDir, err := cacheProjectDir(cacheRoot, project)
	if err != nil {
		return "", err
	}
	if err := validateCacheSegment("base", base); err != nil {
		return "", err
	}
	return projDir + "/" + base, nil
}

// ProbeBundleCache asks a worker whether its bundle cache holds a complete
// entry for project/base by running `ssh <host> vci internal-probe-cache
// <root>/v1/<project>/<base>`. A zero remote exit is a hit; any nonzero
// remote exit is a miss; only a runner failure without a remote exit is a
// wrapped transport error.
func (c Client) ProbeBundleCache(ctx context.Context, host, cacheRoot, project, base string) (bool, error) {
	if err := ValidateHost(host); err != nil {
		return false, err
	}
	if c.Runner == nil {
		return false, fmt.Errorf("ssh runner is required")
	}
	entry, err := cacheEntryPath(cacheRoot, project, base)
	if err != nil {
		return false, err
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, "vci", "internal-probe-cache", entry},
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil && result.ExitCode == 0 {
		return false, fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return true, nil
}

// AcquireBundleClaim writes an active-claim marker for entry project/base on
// the worker so LRU eviction skips the entry while the coordinator uses it,
// via `ssh <host> vci internal-acquire-claim <entry> <claimID>`. The worker
// requires the complete marker to already exist, mirrors
// bundlecache.AcquireActiveClaim.
func (c Client) AcquireBundleClaim(ctx context.Context, host, cacheRoot, project, base, claimID string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if c.Runner == nil {
		return fmt.Errorf("ssh runner is required")
	}
	if err := validateCacheSegment("claimID", claimID); err != nil {
		return err
	}
	entry, err := cacheEntryPath(cacheRoot, project, base)
	if err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, "vci", "internal-acquire-claim", entry, claimID}})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return fmt.Errorf("acquire claim %s: remote exit %d", host, result.ExitCode)
		}
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("acquire claim %s: remote exit %d", host, result.ExitCode)
	}
	return nil
}

// ReleaseBundleClaim removes an active-claim marker on the worker via
// `ssh <host> vci internal-release-claim <entry> <claimID>`. A missing marker
// is not an error, mirroring bundlecache.ReleaseActiveClaim.
func (c Client) ReleaseBundleClaim(ctx context.Context, host, cacheRoot, project, base, claimID string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if c.Runner == nil {
		return fmt.Errorf("ssh runner is required")
	}
	if err := validateCacheSegment("claimID", claimID); err != nil {
		return err
	}
	entry, err := cacheEntryPath(cacheRoot, project, base)
	if err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, "vci", "internal-release-claim", entry, claimID}})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return fmt.Errorf("release claim %s: remote exit %d", host, result.ExitCode)
		}
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("release claim %s: remote exit %d", host, result.ExitCode)
	}
	return nil
}

// ReapBundleCacheResult reports what one remote reaping pass removed for a
// project: Stale counts incomplete entries and claim markers dropped for age,
// Evicted counts complete entries dropped to enforce the LRU limits.
type ReapBundleCacheResult struct {
	Stale   int
	Evicted int
}

// ReapBundleCache runs the worker-side bundle-cache maintenance pass for one
// project over SSH by invoking `vci internal-reap-cache`: it removes
// incomplete entries and claim markers untouched for the 30-minute windows,
// evicts claim-free complete entries beyond the policy limits, then removes
// the reap reference directory. The worker prints `stale=N evicted=M`, which
// the client parses into ReapBundleCacheResult. A nonzero remote exit is a
// reaping failure; a runner failure without a remote exit is a wrapped
// transport error.
func (c Client) ReapBundleCache(ctx context.Context, host, cacheRoot, project string, policy config.BundleCachePolicy, now time.Time) (ReapBundleCacheResult, error) {
	if err := ValidateHost(host); err != nil {
		return ReapBundleCacheResult{}, err
	}
	if c.Runner == nil {
		return ReapBundleCacheResult{}, fmt.Errorf("ssh runner is required")
	}
	projDir, err := cacheProjectDir(cacheRoot, project)
	if err != nil {
		return ReapBundleCacheResult{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	refDir := projDir + "/.vci-reap"
	partialRef := refDir + "/partial"
	claimRef := refDir + "/claim"
	partialCutoff := now.Add(-bundleCachePartialTTL).Format(time.RFC3339)
	claimCutoff := now.Add(-bundleCacheClaimTTL).Format(time.RFC3339)
	var stdout bytes.Buffer
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args: []string{
			host, "vci", "internal-reap-cache",
			projDir, refDir, partialRef, claimRef,
			partialCutoff, claimCutoff,
			strconv.Itoa(policy.MaxEntries),
			strconv.FormatInt(policy.MaxBytes, 10),
		},
		Stdout: &stdout,
	})
	if ctx.Err() != nil {
		return ReapBundleCacheResult{}, ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return ReapBundleCacheResult{}, fmt.Errorf("reap cache %s: remote exit %d", host, result.ExitCode)
		}
		return ReapBundleCacheResult{}, fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return ReapBundleCacheResult{}, fmt.Errorf("reap cache %s: remote exit %d", host, result.ExitCode)
	}
	return parseReapOutput(stdout.String())
}

// StreamNoSeedReconstruct sends a worker payload tar over SSH stdin to the
// remote `vci internal-reconstruct` subcommand for a machine with no seed.
// The worker initializes an empty repository, imports a full bundle (or, on a
// cache hit, the cached entry bundle plus the optional delta the payload
// carries), checks out the submitted head, applies the durable local-change
// archive, and — on a cache miss at or above the admission threshold — writes
// the bundle into the cache entry and evicts LRU entries. Any remote failure
// is an error so the caller can fall back to full workspace staging.
func (c Client) StreamNoSeedReconstruct(ctx context.Context, host, workDir string, cache NoSeedCacheSpec, payload io.Reader) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if payload == nil {
		return fmt.Errorf("reconstruction payload is required")
	}
	if c.Runner == nil {
		return fmt.Errorf("ssh runner is required")
	}
	if err := ValidateRemotePath(workDir); err != nil {
		return err
	}
	args := []string{host, "vci", "internal-reconstruct", workDir, "--no-seed"}
	entry := ""
	if cache.Root != "" {
		var err error
		entry, err = cacheEntryPath(cache.Root, cache.Project, cache.Base)
		if err != nil {
			return err
		}
	}
	if (cache.UseCached || cache.Admit) && entry == "" {
		return fmt.Errorf("no-seed cache reconstruction requires a cache entry")
	}
	if cache.UseCached {
		args = append(args, "--cache", entry, "--use-cached")
	}
	if cache.Admit {
		now := cache.Now
		if now.IsZero() {
			now = time.Now()
		}
		args = append(args, "--cache", entry, "--admit", strconv.FormatInt(cache.BundleBytes, 10), now.UTC().Format(time.RFC3339))
		if cache.Evict {
			args = append(args, "--evict", strconv.Itoa(cache.MaxEntries), strconv.FormatInt(cache.MaxBytes, 10))
		}
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       args,
		Stdin:      payload,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
		}
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
	}
	return nil
}

// bundleCachePartialTTL and bundleCacheClaimTTL bound how long an incomplete
// entry or a claim marker may go untouched before remote reaping removes it.
const (
	bundleCachePartialTTL = 30 * time.Minute
	bundleCacheClaimTTL   = 30 * time.Minute
)

// reapOutputRe matches the machine-readable removal line the reaping
// subcommand prints.
var reapOutputRe = regexp.MustCompile(`stale=(\d+) evicted=(\d+)`)

// parseReapOutput decodes the reaping shell's removal-count line. A missing
// or malformed line is an error so a truncated or changed remote shell cannot
// silently report zero removals.
func parseReapOutput(out string) (ReapBundleCacheResult, error) {
	m := reapOutputRe.FindStringSubmatch(out)
	if m == nil {
		return ReapBundleCacheResult{}, fmt.Errorf("reap cache: no removal counts in output %q", strings.TrimSpace(out))
	}
	stale, err := strconv.Atoi(m[1])
	if err != nil {
		return ReapBundleCacheResult{}, fmt.Errorf("reap cache: stale count %q: %w", m[1], err)
	}
	evicted, err := strconv.Atoi(m[2])
	if err != nil {
		return ReapBundleCacheResult{}, fmt.Errorf("reap cache: evicted count %q: %w", m[2], err)
	}
	return ReapBundleCacheResult{Stale: stale, Evicted: evicted}, nil
}

// StageRemote clears the validated remote work dir, then tars a local
// workspace and streams it to the remote `vci internal-stage` subcommand over
// SSH. The command uses tar data only, with no shell content from the
// workspace payload.
func StageRemote(ctx context.Context, host, remoteWorkDir, localWorkspace string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateRemotePath(remoteWorkDir); err != nil {
		return err
	}
	if localWorkspace == "" {
		return fmt.Errorf("local workspace is required")
	}
	info, err := os.Stat(localWorkspace)
	if err != nil {
		return fmt.Errorf("local workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local workspace is not a directory")
	}
	tarCmd := exec.CommandContext(ctx, "tar", "-cf", "-", "-C", localWorkspace, ".")
	sshCmd := exec.CommandContext(ctx, "ssh", host, "vci", "internal-stage", remoteWorkDir)
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout: %w", err)
	}
	sshCmd.Stdin = tarOut
	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return fmt.Errorf("archive workspace: %w", tarErr)
	}
	if sshErr != nil {
		return fmt.Errorf("ssh %s: %w", host, sshErr)
	}
	return nil
}

// FetchRemote copies remote workdir contents back to the local destination using scp.
// Command format: `scp -r -q <host>:<workDir> <localDest>`.
func FetchRemote(ctx context.Context, host, remoteWorkDir, localDest string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateRemotePath(remoteWorkDir); err != nil {
		return err
	}
	if localDest == "" {
		return fmt.Errorf("local fetch destination is required")
	}
	cmd := exec.CommandContext(ctx, "scp", "-r", "-q", host+":"+remoteWorkDir, localDest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("scp %s: %s", host, message)
	}
	return nil
}

// shellSeed renders a seed path as a remote shell word. A leading `~/` is
// rewritten to `$HOME` so the tilde is never single-quoted and a suffix
// holding spaces or metacharacters stays intact; the suffix is single-quoted
// only when it is not shell-safe. Other seeds pass through unquoted when
// shell-safe.
func shellSeed(seed string) string {
	if strings.HasPrefix(seed, "~/") {
		suffix := strings.TrimPrefix(seed, "~")
		if shellSafe(suffix) {
			return "$HOME" + suffix
		}
		return "$HOME" + shellQuote(suffix)
	}
	if shellSafe(seed) {
		return seed
	}
	return shellQuote(seed)
}

// IsWindowsOS reports whether a machine OS declaration selects the
// Windows (cmd.exe) login shell the coordinator must compose for. Any
// case of "windows" is Windows; every other value — including empty —
// is POSIX (sh/bash).
func IsWindowsOS(osKind string) bool {
	return strings.EqualFold(strings.TrimSpace(osKind), "windows")
}

// WindowsRemotePath renders the canonical ~/.vci/state/work/<run> path
// for a Windows cmd.exe login shell: cmd.exe does not expand ~ or /
// separators, so ~ becomes %USERPROFILE% and / becomes \. The
// canonical POSIX form is preserved on the wire; only the shell
// rendering changes.
func WindowsRemotePath(workDir string) string {
	return strings.Replace(strings.ReplaceAll(workDir, "/", "\\"), "~", "%USERPROFILE%", 1)
}

// composeShell builds a single remote shell command for the worker's
// declared OS. It validates the workDir, sets HOME/TMPDIR, applies env
// vars, and execs argv. A POSIX worker (sh/bash) gets the historical
// script; a Windows worker (cmd.exe) gets set/if/mkdir syntax with
// %USERPROFILE%-rooted paths, because cmd.exe cannot expand ~ or parse
// export/$VAR/exec/mkdir -p.
func composeShell(workDir string, argv []string, env map[string]string, osKind string) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("remote argv is empty")
	}
	if IsWindowsOS(osKind) {
		return composeShellWindows(workDir, argv, env), nil
	}
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(workDir)
	b.WriteString(" && __vci_login_home=$HOME && export HOME=")
	b.WriteString(workDir)
	b.WriteString("/.home TMPDIR=")
	b.WriteString(workDir)
	b.WriteString("/.tmp && mkdir -p ")
	b.WriteString(workDir)
	b.WriteString("/.home ")
	b.WriteString(workDir)
	b.WriteString("/.tmp")
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" {
			continue
		}
		b.WriteString(" && export ")
		b.WriteString(shellQuote(key))
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
	}
	b.WriteString(" && exec ")
	for i, arg := range argv {
		if i > 0 {
			b.WriteString(" ")
		}
		if strings.Contains(arg, workDir) && shellSafe(arg) {
			b.WriteString(remoteWord(arg))
		} else {
			b.WriteString(shellQuote(arg))
		}
	}
	return b.String(), nil
}

// composeShellWindows builds a cmd.exe-safe command string. cmd.exe
// cannot expand ~, parse export/$VAR/exec, or run mkdir -p, so the
// POSIX script is replaced with: cd /D into a %USERPROFILE%-rooted
// path, set "HOME/TMPDIR=<workDir>/.home|.tmp", guard each mkdir with
// `if not exist`, apply env vars as set "KEY=VALUE", then run argv. &&
// chains the steps the same way the POSIX branch does. The operator
// command itself is quoted only when it carries a metacharacter; bare
// tokens like python.exe or test.py pass through unchanged.
func composeShellWindows(workDir string, argv []string, env map[string]string) string {
	win := WindowsRemotePath(workDir)
	var b strings.Builder
	b.WriteString(`cd /D "`)
	b.WriteString(win)
	b.WriteString(`" && set "HOME=`)
	b.WriteString(win)
	b.WriteString(`\.home" && set "TMPDIR=`)
	b.WriteString(win)
	b.WriteString(`\.tmp" && if not exist "`)
	b.WriteString(win)
	b.WriteString(`\.home" mkdir "`)
	b.WriteString(win)
	b.WriteString(`\.home" && if not exist "`)
	b.WriteString(win)
	b.WriteString(`\.tmp" mkdir "`)
	b.WriteString(win)
	b.WriteString(`\.tmp"`)
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" {
			continue
		}
		b.WriteString(` && set "`)
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(env[key])
		b.WriteString(`"`)
	}
	b.WriteString(" && ")
	for i, arg := range argv {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(cmdWord(workDir, arg))
	}
	return b.String()
}

// cmdWord renders one argv element for cmd.exe. A token made of safe
// characters passes through bare; anything else is double-quoted so a
// space or metacharacter cannot break the command chain. A token that
// carries the canonical workDir is rewritten to its %USERPROFILE% form
// so cmd.exe can resolve it.
func cmdWord(workDir, arg string) string {
	if strings.Contains(arg, workDir) {
		return WindowsRemotePath(arg)
	}
	if arg == "" {
		return `""`
	}
	if cmdSafe(arg) {
		return arg
	}
	return `"` + arg + `"`
}

// remoteWord rewrites `~`-prefixed argv words to use captured login home.
func remoteWord(arg string) string {
	if !strings.HasPrefix(arg, "~") {
		return arg
	}
	return `"$__vci_login_home"` + strings.TrimPrefix(arg, "~")
}

// shellSafe checks whether a token is safe to pass unquoted to the remote shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@~-]+$`).MatchString

// cmdSafe checks whether a token is safe to pass unquoted to cmd.exe.
// Backslash and colon are included so Windows paths like python.exe and
// C:\Users\... pass through bare; everything else is double-quoted.
var cmdSafe = regexp.MustCompile(`^[A-Za-z0-9_./:\\@~-]+$`).MatchString

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
