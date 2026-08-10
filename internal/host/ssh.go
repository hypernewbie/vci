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
func (c Client) RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	if err := ValidateHost(host); err != nil {
		return 0, err
	}
	shell, err := composeShell(workDir, argv, env)
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
func RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	return (Client{Runner: process.Native{}}).RunRemote(ctx, host, workDir, argv, env, stdout, stderr)
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

// StreamReconstruct sends a worker payload tar over SSH stdin to a remote
// reconstruction shell. The shell seeds a VCI-owned work directory from the
// machine's seed checkout with tar, imports the bundle the payload carries
// (when present), checks out the submitted head, applies the durable
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
	shell, err := reconstructShell(seed, workDir)
	if err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, shell},
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

// validCacheSegment mirrors bundlecache's single-segment rule: non-empty
// letters, digits, dot, dash, and underscore with no slashes, and not "." or
// "..". Project, base, and claim ids must all be such segments before they
// are composed into a remote shell path.
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
// entry for project/base by running `ssh <host> test -f
// <root>/v1/<project>/<base>/complete`. A zero remote exit is a hit; any
// nonzero remote exit is a miss; only a runner failure without a remote exit
// is a wrapped transport error.
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
		Args:       []string{host, "test", "-f", entry + "/complete"},
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
// the worker so LRU eviction skips the entry while the coordinator uses it.
// The complete marker must already exist; the claims dir is created on
// demand, mirroring bundlecache.AcquireActiveClaim.
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
	shell := "test -f " + entry + "/complete && mkdir -p " + entry + "/claims && : > " + entry + "/claims/" + claimID
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, shell}})
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

// ReleaseBundleClaim removes an active-claim marker on the worker. A missing
// marker is not an error, mirroring bundlecache.ReleaseActiveClaim.
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
	shell := "rm -f " + entry + "/claims/" + claimID
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, shell}})
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
// project over SSH: it removes incomplete entries and claim markers untouched
// for the 30-minute windows, evicts claim-free complete entries beyond the
// policy limits, then removes the reap reference directory before the shell
// prints `stale=N evicted=M`, which the client parses into
// ReapBundleCacheResult. A nonzero remote exit is a reaping failure; a runner
// failure without a remote exit is a wrapped transport error.
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
	var stdout bytes.Buffer
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, cacheReapShell(projDir, policy, now)},
		Stdout:     &stdout,
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

// StreamNoSeedReconstruct sends a worker payload tar over SSH stdin to a
// remote reconstruction shell for a machine with no seed. The shell
// initializes an empty repository, imports a full bundle (or, on a cache
// hit, the cached entry bundle plus the optional delta the payload carries),
// checks out the submitted head, applies the durable local-change archive,
// and — on a cache miss at or above the admission threshold — writes the
// bundle into the cache entry and evicts LRU entries. Any remote failure is
// an error so the caller can fall back to full workspace staging.
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
	shell, err := noSeedReconstructShell(workDir, cache)
	if err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, shell},
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

// noSeedReconstructShell builds the remote shell that turns a streamed worker
// payload into a reconstructed workspace on a machine with no seed. The
// payload tar carries head, a bundle, and the durable lc.tar. Without a cache
// entry the bundle must cover the full reachable history; on a cache hit the
// shell imports the cached entry bundle first and then the payload's delta
// bundle when present. On a miss with admission enabled the shell writes the
// streamed bundle into the cache entry (meta.json, then the complete marker
// last so an interrupted admission is never a hit) and evicts LRU entries
// beyond the policy limits. Every step is chained with && so any failure
// aborts the reconstruction instead of leaving a half-built workspace.
func noSeedReconstructShell(workDir string, cache NoSeedCacheSpec) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	entry := ""
	if cache.Root != "" {
		var err error
		entry, err = cacheEntryPath(cache.Root, cache.Project, cache.Base)
		if err != nil {
			return "", err
		}
	}
	if (cache.UseCached || cache.Admit) && entry == "" {
		return "", fmt.Errorf("no-seed cache reconstruction requires a cache entry")
	}

	var b strings.Builder
	b.WriteString("mkdir -p " + workDir)
	b.WriteString(" && cd " + workDir)
	b.WriteString(" && mkdir -p .vci-payload")
	b.WriteString(" && tar -xf - -C .vci-payload")
	b.WriteString(" && git init -q")
	if cache.UseCached {
		b.WriteString(" && touch " + entry + "/meta.json")
		b.WriteString(" && git bundle unbundle " + entry + "/bundle >/dev/null 2>&1")
		b.WriteString(" && if [ -s .vci-payload/bundle ]; then git bundle unbundle .vci-payload/bundle >/dev/null 2>&1; fi")
	} else {
		b.WriteString(" && git bundle unbundle .vci-payload/bundle >/dev/null 2>&1")
	}
	b.WriteString(" && head=$(cat .vci-payload/head)")
	b.WriteString(" && git checkout -q \"$head\"")
	b.WriteString(" && tar -tf .vci-payload/lc.tar >/dev/null")
	b.WriteString(" && if tar -xOf .vci-payload/lc.tar patch > .vci-payload/patch 2>/dev/null; then git apply --binary --whitespace=nowarn .vci-payload/patch; fi")
	b.WriteString(" && if tar -tf .vci-payload/lc.tar f/ >/dev/null 2>&1; then tar -xf .vci-payload/lc.tar -C . --strip-components=1 f/; fi")
	if cache.Admit {
		admit, err := cacheAdmitShell(entry, cache.BundleBytes, cache.Now)
		if err != nil {
			return "", err
		}
		b.WriteString(" && " + admit)
		if cache.Evict {
			evict, err := cacheEvictShell(entry, cache.MaxEntries, cache.MaxBytes)
			if err != nil {
				return "", err
			}
			b.WriteString(" && " + evict)
		}
	}
	b.WriteString(" && rm -rf .vci-payload")
	return b.String(), nil
}

// cacheAdmitShell writes the streamed payload bundle into a cache entry and
// marks it complete, writing the complete marker last so an interrupted
// admission is never a hit. meta.json carries the byte size and an RFC3339
// last-used timestamp in the same shape bundlecache.Admit persists.
func cacheAdmitShell(entry string, bundleBytes int64, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	meta := fmt.Sprintf(`{"bytes":%d,"last_used":%q}`, bundleBytes, now.UTC().Format(time.RFC3339))
	return "mkdir -p " + entry +
		" && cp .vci-payload/bundle " + entry + "/bundle" +
		" && printf '" + meta + "' > " + entry + "/meta.json" +
		" && : > " + entry + "/complete", nil
}

// cacheEvictLoop returns a POSIX sh loop that removes the
// least-recently-used claim-free complete entries of projDir until both
// positive limits are satisfied, mirroring bundlecache.EvictLRU. A non-empty
// counter tallies removals into that shell variable and aborts the shell when
// a removal fails so the loop cannot spin on an undeletable entry; an empty
// counter keeps the bare rm shape the reconstruction shell relies on. With no
// positive limits the loop is the no-op ":", matching EvictLRU's
// within-limits short-circuit.
func cacheEvictLoop(projDir string, maxEntries int, maxBytes int64, counter string) string {
	parts := make([]string, 0, 2)
	if maxEntries > 0 {
		parts = append(parts, `[ "$n" -le `+strconv.Itoa(maxEntries)+` ]`)
	}
	if maxBytes > 0 {
		parts = append(parts, `[ "$b" -le `+strconv.FormatInt(maxBytes, 10)+` ]`)
	}
	if len(parts) == 0 {
		return ":"
	}
	within := parts[0]
	if len(parts) == 2 {
		within = parts[0] + " && " + parts[1]
	}
	remove := `rm -rf "$o"`
	if counter != "" {
		remove = counter + "=$((" + counter + "+1)); " + remove + " || exit"
	}
	return `while :; do n=0; b=0; for d in ` + projDir + `/*/; do [ -f "$d/complete" ] || continue; n=$((n+1)); v=$(sed -n 's/.*"bytes":\([0-9][0-9]*\).*/\1/p' "$d/meta.json"); [ -n "$v" ] || v=0; b=$((b+v)); done; if ` + within + `; then break; fi; o=; for d in ` + projDir + `/*/; do [ -f "$d/complete" ] || continue; [ -n "$(ls -A "$d/claims" 2>/dev/null)" ] && continue; if [ -z "$o" ] || [ "$d/meta.json" -ot "$o/meta.json" ]; then o=$d; fi; done; [ -n "$o" ] || break; ` + remove + `; done`
}

// cacheEvictShell returns a POSIX sh loop that removes the
// least-recently-used claim-free complete entries of a project until both
// positive policy limits are satisfied, mirroring bundlecache.EvictLRU. entry
// is <root>/v1/<project>/<base>; the loop operates on the project directory
// that holds it.
func cacheEvictShell(entry string, maxEntries int, maxBytes int64) (string, error) {
	slash := strings.LastIndex(entry, "/")
	if slash < 0 {
		return "", fmt.Errorf("invalid cache entry path %q", entry)
	}
	return cacheEvictLoop(entry[:slash], maxEntries, maxBytes, ""), nil
}

// bundleCachePartialTTL and bundleCacheClaimTTL bound how long an incomplete
// entry or a claim marker may go untouched before remote reaping removes it.
const (
	bundleCachePartialTTL = 30 * time.Minute
	bundleCacheClaimTTL   = 30 * time.Minute
)

// cacheReapShell builds the remote POSIX sh body that reaps one cache
// project: it drops incomplete entries and claim markers older than the
// 30-minute partial and claim windows, evicts claim-free complete entries
// beyond the policy limits, removes the reference directory, and prints
// `stale=N evicted=M`. Each window is a reference file touched to exactly
// now-cutoff under a dot directory the `*/` entry globs never match, so
// staleness uses plain POSIX `-ot` mtime comparisons — no GNU-only find
// time flags and no remote scripting runtime. Removals abort the shell on
// failure (`|| exit`) so a stuck rm cannot spin the eviction loop or
// silently under-count.
func cacheReapShell(projDir string, policy config.BundleCachePolicy, now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	refDir := projDir + "/.vci-reap"
	partialRef := refDir + "/partial"
	claimRef := refDir + "/claim"
	partialCutoff := now.Add(-bundleCachePartialTTL).Format("200601021504.05")
	claimCutoff := now.Add(-bundleCacheClaimTTL).Format("200601021504.05")

	var b strings.Builder
	b.WriteString("mkdir -p " + refDir)
	b.WriteString(" && TZ=UTC touch -t " + partialCutoff + " " + partialRef)
	b.WriteString(" && TZ=UTC touch -t " + claimCutoff + " " + claimRef)
	b.WriteString(" && p=0 && for d in " + projDir + `/*/; do [ -d "$d" ] || continue; [ -f "$d/complete" ] && continue; if [ -f "$d/meta.json" ]; then [ "$d/meta.json" -ot ` + partialRef + ` ] || continue; else [ "$d" -ot ` + partialRef + ` ] || continue; fi; rm -rf "$d" || exit; p=$((p+1)); done`)
	b.WriteString(" && q=0 && for d in " + projDir + `/*/; do [ -d "$d" ] || continue; [ -d "$d/claims" ] || continue; for c in "$d"/claims/*; do [ -f "$c" ] || continue; [ "$c" -ot ` + claimRef + ` ] || continue; rm -f "$c" || exit; q=$((q+1)); done; done`)
	b.WriteString(" && e=0 && " + cacheEvictLoop(projDir, policy.MaxEntries, policy.MaxBytes, "e"))
	b.WriteString(" && rm -f " + partialRef + " " + claimRef + " && rmdir " + refDir)
	b.WriteString(` && printf 'stale=%d evicted=%d\n' "$((p+q))" "$e"`)
	return b.String()
}

// reapOutputRe matches the machine-readable removal line the reaping shell
// prints.
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
// workspace and streams it to remoteWorkDir over SSH. The command uses
// tar data only, with no shell content from the workspace payload.
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
	sshCmd := exec.CommandContext(ctx, "ssh", host, "rm -rf "+remoteWorkDir+" && mkdir -p "+remoteWorkDir+" && cd "+remoteWorkDir+" && tar -xpf -")
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout: %w", err)
	}
	sshCmd.Stdin = tarOut
	var stderr bytes.Buffer
	sshCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return fmt.Errorf("archive workspace: %w", tarErr)
	}
	if sshErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = sshErr.Error()
		}
		return fmt.Errorf("ssh %s: %s", host, message)
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

// reconstructShell builds the remote `sh -c` body that turns a streamed
// worker payload into a reconstructed workspace at workDir. The payload tar
// carries head, an optional bundle, and the durable lc.tar; the shell extracts
// it into a private dot-directory, seeds the workspace from the machine's
// checkout with tar, imports the bundle when present, checks out the submitted
// head, validates the complete lc.tar archive with `tar -tf` redirected to
// /dev/null, applies the tracked-change patch, restores untracked files by
// stripping the lc.tar "f/" prefix, and removes the payload directory. Every
// step is chained with && so any failure aborts the reconstruction instead of
// leaving a half-built workspace.
func reconstructShell(seed, workDir string) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	if err := validateSeed(seed); err != nil {
		return "", err
	}
	return "mkdir -p " + workDir +
		" && cd " + workDir +
		" && mkdir -p .vci-payload" +
		" && tar -xf - -C .vci-payload" +
		" && tar -cf - -C " + shellSeed(seed) + " . | tar -xf -" +
		" && head=$(cat .vci-payload/head)" +
		" && if [ -s .vci-payload/bundle ]; then git bundle unbundle .vci-payload/bundle >/dev/null 2>&1; fi" +
		" && git checkout -q \"$head\"" +
		" && tar -tf .vci-payload/lc.tar >/dev/null" +
		" && if tar -xOf .vci-payload/lc.tar patch > .vci-payload/patch 2>/dev/null; then git apply --binary --whitespace=nowarn .vci-payload/patch; fi" +
		" && if tar -tf .vci-payload/lc.tar f/ >/dev/null 2>&1; then tar -xf .vci-payload/lc.tar -C . --strip-components=1 f/; fi" +
		" && rm -rf .vci-payload", nil
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

// composeShell builds a single remote shell command.
// It validates the workDir, sets HOME/TMPDIR, applies env vars, and execs argv.
// It preserves safe `~`-based expansion for staged-workspace paths.
func composeShell(workDir string, argv []string, env map[string]string) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("remote argv is empty")
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

// remoteWord rewrites `~`-prefixed argv words to use captured login home.
func remoteWord(arg string) string {
	if !strings.HasPrefix(arg, "~") {
		return arg
	}
	return `"$__vci_login_home"` + strings.TrimPrefix(arg, "~")
}

// shellSafe checks whether a token is safe to pass unquoted to the remote shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@~-]+$`).MatchString

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
