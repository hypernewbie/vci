package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// digestShape is the accepted form of a content-addressed source-cache
// key. Anything else is treated as missing and never used to build a
// remote shell fragment.
var digestShape = regexp.MustCompile(`^sha256-[0-9a-f]{64}$`)

// safeCacheKey is the validated, structured set of fields that may
// compose a remote cache shell fragment. Building remote text from a
// typed value keeps every shell-embedding call site from ever seeing
// client-controlled bytes. The format version is a single constant
// owned by the sourcecache package (sourcecache.FormatVersion); no
// second layout string exists.
type safeCacheKey struct {
	Digest  string // sha256-<64-lowercase-hex>
	Project string // layout.ValidName
}

// validateCacheKey returns a safeCacheKey ready for remote-shell
// composition or an error explaining which field failed. An empty or
// invalid digest is rejected so callers do not invent a fallback
// identifier.
func validateCacheKey(digest, project string) (safeCacheKey, error) {
	if !layout.ValidName(project) {
		return safeCacheKey{}, fmt.Errorf("repository name %q cannot name a remote project", project)
	}
	if !digestShape.MatchString(digest) {
		return safeCacheKey{}, fmt.Errorf("source digest %q is not sha256-<64-lowercase-hex>", digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256-")); err != nil {
		return safeCacheKey{}, fmt.Errorf("source digest %q has invalid hex: %w", digest, err)
	}
	return safeCacheKey{Digest: digest, Project: project}, nil
}

// RemoteConfigured reports whether this root selects direct SSH for any
// command. The answer is driven by the top-level `orchestrator`
// selector. `self` (the coordinator) returns false; any other value
// is treated as the SSH destination for a client root.
func RemoteConfigured(l layout.Layout) (bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return false, err
	}
	return host != "", nil
}

// remoteOrchestrator returns the SSH destination for a client root and an
// empty string for a coordinator root.
func remoteOrchestrator(l layout.Layout) (string, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return "", err
	}
	if cfg.Orchestrator == config.OrchestratorSelf {
		return "", nil
	}
	return cfg.Orchestrator, nil
}

// RemoteBuild materializes a private Vci-owned snapshot of the selected
// build input, computes the content digest from that settled snapshot,
// and archives exactly that snapshot over ordinary SSH into a remote
// staging directory under the coordinator's Vci root. If the
// coordinator already owns a completed immutable cache entry for the
// same (format version, digest, project) key, no tar producer starts:
// the public remote `vci build .` runs directly from the verified entry.
//
// The remote run identity is returned unchanged; no local run record is
// created and no remote run files are inspected.
func RemoteBuild(ctx context.Context, l layout.Layout, sourcePath string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	input, err := source.SelectBuildInput(ctx, sourcePath, process.Native{})
	if err != nil {
		return nil, true, fmt.Errorf("select build input: %w", err)
	}
	// Validate the repository basename immediately, before any
	// further work touches remote shell text or staging directories.
	if !layout.ValidName(input.ProjectName) {
		return nil, true, fmt.Errorf("repository name %q cannot name a remote project", input.ProjectName)
	}

	// Materialize the settled snapshot first. The digest is computed
	// over this snapshot and this exact snapshot is archived, so a
	// source mutation between digest computation and archive
	// production cannot change the bytes the coordinator verifies.
	if err := os.MkdirAll(l.TempDir(), 0o700); err != nil {
		return nil, true, fmt.Errorf("prepare client snapshot dir: %w", err)
	}
	snapshot, err := source.MaterializeSnapshot(input, l.TempDir())
	if err != nil {
		return nil, true, fmt.Errorf("materialize source snapshot: %w", err)
	}
	defer func() { _ = os.RemoveAll(snapshot) }()

	digest, err := source.ComputeSnapshotDigest(snapshot)
	if err != nil {
		return nil, true, fmt.Errorf("compute source digest: %w", err)
	}
	key, err := validateCacheKey(digest, input.ProjectName)
	if err != nil {
		return nil, true, err
	}

	// Try remote cache lookup over SSH first. The cache probe is built
	// from the validated cache key and the sourcecache layout helpers,
	// never from raw client text.
	cacheScript := buildCacheProbeScript(key)
	raw, err := runSSH(ctx, host, cacheScript)
	if err == nil && validEnvelope(raw) {
		fmt.Fprintln(os.Stderr, "vci: source cache hit")
		return raw, true, nil
	}

	raw, remote, tarBytes, err := buildOverStaging(ctx, host, input, snapshot, key)
	if tarBytes > 0 {
		fmt.Fprintf(os.Stderr, "vci: source tar bytes %d\n", tarBytes)
	}
	return raw, remote, err
}

// buildCacheProbeScript returns the small SSH-side fragment that asks
// the coordinator whether a completed cache entry exists. The shell
// text references only the validated cache key fields and the layout
// produced by sourcecache.EntryRootRel/EntryTreeRel, so the probe
// path can never diverge from the Go-side publication layout.
func buildCacheProbeScript(key safeCacheKey) string {
	if !layout.ValidName(key.Project) || !digestShape.MatchString(key.Digest) {
		// Caller error: validateCacheKey rejects this upstream.
		return "exit 1"
	}
	parent := sourcecache.EntryRootRel(key.Digest, key.Project)
	tree := sourcecache.EntryTreeRel(key.Digest, key.Project)
	return fmt.Sprintf(`set -eu
ROOT="${VCI_ROOT:-$HOME/.vci}"
CACHE="$ROOT/state/source-cache"
PARENT="$CACHE/%s"
TREE="$CACHE/%s"
if [ -f "$PARENT/complete" ] && [ -f "$PARENT/meta.json" ]; then
	cd "$TREE"
	vci build .
else
	exit 42
fi
`, parent, tree)
}

// buildOverStaging composes a single ssh session that owns the entire
// staging lifecycle: mkdir, populate from a client tar stream of the
// settled snapshot, run the public `vci build .` from the staged
// project directory, and trap-based cleanup. The validated cache key
// fields are the only inputs that reach the remote shell. The number of
// source bytes handed to the system tar is returned so the caller can
// report transport facts.
func buildOverStaging(ctx context.Context, host string, input source.SourceInput, snapshotRoot string, key safeCacheKey) ([]byte, bool, int64, error) {
	if !layout.ValidName(input.ProjectName) {
		return nil, true, 0, fmt.Errorf("repository name %q cannot name a remote project", input.ProjectName)
	}

	tarCmd := exec.CommandContext(ctx, "tar", "-cf", "-", "-C", snapshotRoot, "--null", "-T", "-", "--no-recursion")
	var pathBuf bytes.Buffer
	for _, p := range input.Files {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd.Stdin = &pathBuf

	script := stagingShellScript(key)
	sshCmd := exec.CommandContext(ctx, "ssh", host, script)
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		return nil, true, 0, fmt.Errorf("tar stdout: %w", err)
	}
	counter := &countingReader{r: tarOut}
	sshCmd.Stdin = counter
	var stdout, stderr bytes.Buffer
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return nil, true, 0, fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return nil, true, counter.n, fmt.Errorf("archive source: %w", tarErr)
	}
	if sshErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = sshErr.Error()
		}
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), true, counter.n, nil
		}
		return nil, true, counter.n, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, true, counter.n, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), true, counter.n, nil
}

// countingReader counts the bytes read from an underlying reader. The
// client wraps the tar stream with it so the source bytes supplied to
// SSH are a measured fact, not an inference from elapsed time.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// stagingShellScript composes the remote-side fragment that owns the
// staging directory, populates it from the client tar stream, records
// the expected cache identity in a Vci-owned sibling metadata file, and
// invokes the public remote `vci build .` from inside the staged
// project directory. The staging dir is removed wholesale on every exit
// path so a hostile source-tree symlink cannot trick the trap into
// touching an external path.
//
// Every field in `key` is validated; nothing else is embedded in the
// script. Nested source filenames remain tar data; they never reach
// the remote shell.
func stagingShellScript(key safeCacheKey) string {
	if !layout.ValidName(key.Project) || !digestShape.MatchString(key.Digest) {
		// Caller error: this fragment can never be reached with an
		// unsafe key because validateCacheKey rejects it upstream.
		return "exit 1"
	}
	prefix := "vci-source-" + key.Project
	return fmt.Sprintf(`set -eu
ROOT="${VCI_ROOT:-$HOME/.vci}"
TMP_PARENT="$ROOT/state/tmp"
mkdir -m 700 -p "$TMP_PARENT"
STAGING=$(mktemp -d -p "$TMP_PARENT" -t %s.XXXXXXXX)
chmod 700 "$STAGING"
trap 'rm -rf "$STAGING" 2>/dev/null || true' EXIT INT TERM
PROJECT="$STAGING/%s"
mkdir -m 700 "$PROJECT"
printf '%%s %%s %%s\n' '%s' '%s' '%s' > "$STAGING/vci-meta"
chmod 600 "$STAGING/vci-meta"
tar -C "$PROJECT" -xpf -
cd "$PROJECT"
vci build .
`, prefix, key.Project, sourcecache.FormatVersion, key.Digest, key.Project)
}

// RemoteCheck and RemoteAbort use the one configured remote target directly.
// They take the run ID returned by the remote public CLI unchanged. No
// fixed timeout is applied: cancellation is owned by the caller context
// and the controlled test deadline.
func RemoteCheck(ctx context.Context, l layout.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci check "+shellQuote(string(id)))
	return raw, true, err
}

func RemoteAbort(ctx context.Context, l layout.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci abort "+shellQuote(string(id)))
	return raw, true, err
}

// remoteTarget returns the configured remote host from the orchestrator
// selector. A client root carries only the selector; the coordinator
// owned project and machine configuration that the build needs lives
// on the remote side and is resolved by the remote `vci build .`
// itself.
func remoteTarget(l layout.Layout) (string, bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return "", false, err
	}
	return host, host != "", nil
}

// runSSH executes `ssh <host> <command>` via the ordinary system ssh
// binary and returns the remote stdout. When the remote process
// exits non-zero, a valid Vci JSON envelope is preserved so the caller
// can report the remote error rather than relabeling it as SSH
// failure. Only no response, malformed response, schema mismatch, or
// genuine SSH failure is classified as infrastructure.
func runSSH(ctx context.Context, host, command string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", host, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), nil
}

// validEnvelope returns true when the given bytes decode as a Vci response
// envelope with the expected schema version. Used to decide whether a
// non-zero remote exit should be propagated as an envelope or treated as an
// SSH infrastructure failure.
func validEnvelope(data []byte) bool {
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Command       *string         `json:"command"`
		OK            *bool           `json:"ok"`
		Data          json.RawMessage `json:"data"`
		Error         json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false
	}
	if envelope.SchemaVersion != model.SchemaVersion || envelope.Command == nil || *envelope.Command == "" || envelope.OK == nil || len(envelope.Data) == 0 {
		return false
	}
	if *envelope.OK {
		return len(envelope.Error) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null"))
	}
	if len(envelope.Error) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
		return false
	}
	var remoteErr struct {
		Code    string `json:"code"`
		Class   string `json:"class"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope.Error, &remoteErr); err != nil {
		return false
	}
	return remoteErr.Code != "" && remoteErr.Class != "" && remoteErr.Message != ""
}

// RemoteCommand runs an ordinary public Vci command on the configured
// remote host. It returns the remote JSON envelope unchanged; a non-zero
// remote exit with a valid envelope is preserved. Only no response,
// malformed response, schema mismatch, or genuine SSH failure is
// classified as infrastructure.
func RemoteCommand(ctx context.Context, l layout.Layout, name string, args ...string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, buildRemoteCommand(name, args...))
	return raw, true, err
}

// buildRemoteCommand composes a shell-safe `vci <name> <args...>` line
// for the system `ssh` executable. It quotes each argument so the
// remote shell sees the same set of arguments the client passed.
func buildRemoteCommand(name string, args ...string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, shellQuote(name))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return "vci " + strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
