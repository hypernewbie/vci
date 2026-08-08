# Vci

Vci is a minimal, agent-first local CI harness. One binary coordinates
reproducible builds and reports JSON.

Vci has exactly two configuration roles. A **coordinator** owns the
project, machine, and policy configuration and runs builds locally on
the configured machines. A **client** selects one coordinator and
forwards builds and queries to that coordinator over ordinary system
SSH. The same `vci` binary serves either role; the orchestrator
selector in the root configuration decides which.

The default path is the local coordinator on macOS. Configuration
lives under `~/.vci` in production and is injected by tests. The
source path is the direct path passed to `build`; hosted Git remotes
are not required. A machine may optionally declare a container
runtime (`docker`) so the project's command runs inside a
declared image with the per-run workspace bind-mounted read-only;
the bare host path remains the default.

## First commands — coordinator

```sh
vci setup init                       # writes orchestrator = "self"
vci setup machine add mac-local                   # one local slot
vci setup machine add fast --capacity 4           # four local slots
vci setup machine add linux-docker --runtime docker --image ghcr.io/org/ci:pin   # container runtime
vci setup project add Vci --machine mac-local --command go --arg test --arg ./...
vci setup project add big --machine mac-local --machine fast --command go --arg test --arg ./...
vci setup project add lintest --machine linux-docker --command go --arg test --arg ./...
vci machines
vci projects
vci build .
vci check <run-id>
vci abort <run-id>
vci setup reap
```

A coordinator machine is a named, coordinator-owned local capacity
pool. Each machine has a local-slot capacity (`max_concurrent`); zero
or omitted means one slot. A project may attach to one or more
machines; submission reserves one available slot atomically and the
detached worker runs locally on the selected machine. The reservation
and the staging record are published inside a single scheduler
transaction; a failed publish rolls back the exact claim and returns
no run id. A successful local `build` JSON includes the selected
`machine` so the operator does not need to read private coordinator
state. The reaper honors a 60-second pre-start grace for staging
runs that have a valid reservation but no worker lease yet; runs
that hold a live worker lease are governed by lease/renewal rules
alone. Capacity exhaustion is a stable `machine_unavailable` JSON
error, retryable true. The scheduler is fail-closed: a corrupt,
symlink, wrong-body, or future-dated claim surfaces as a hard error
from `Status`, `Reap`, and `ReserveAndPublish`; only
`ReserveAndPublish` may create a claim, and only with a publish
callback that persists a real run record. There is no queue, no
wait, no remote worker, no Docker, no VM — slots are local capacity
on this coordinator host.

## First commands — client

A client root is just the selector:

```toml
schema_version = 1
orchestrator = "builder"
```

Every public command (`build`, `check`, `abort`, `machines`,
`projects`) is proxied to the coordinator over ordinary system SSH.
Administrative `setup` mutations reject locally.

Stdout is machine-readable JSON. Diagnostics belong on stderr. Project
and machine configuration belongs to the coordinator; the source
repository does not supply a build-machine definition. A client never
carries coordinator state and never inspects the coordinator's run
files; the remote run identity is returned unchanged.

The coordinator-local slot model is the smallest honest next step
because Vci has no remote-worker control plane. Physical remote
execution would require a separate direct source-to-selected-builder
topology decision and is not part of this release.

The current local result states are `queued`, `staging`, `running`,
`committing`, `succeeded`, `failed`, `lost`, and `aborted`. A command
failure is reported as a job failure with its exit code and preserved
log paths. An infrastructure or configuration failure is classified
separately. `abort` durably requests cancellation; the live worker
owns TERM/KILL escalation. If a worker is already dead, Vci will not
signal a recycled process and reaping resolves the run to `lost`.

## Configuration

`~/.vci/config.toml` is created by `setup init`. Tests set `VCI_ROOT`
to an isolated temporary root. The coordinator role writes:

```toml
schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4194304
stderr_bytes = 4194304

[retention]
max_bytes = 536870912

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
```

A client role writes only the orchestrator selector. It may not declare
machines, projects, retention, or log limits — those belong to the
coordinator and would drift if duplicated on the client.

The orchestrator selector is strict: it must not be empty, may not
begin with `-`, and may not contain whitespace or control characters.
A missing selector decodes as `self` for compatibility with existing
local roots; `setup init` writes it explicitly.

Run workspaces are temporary and deleted after result publication.
Source blobs and manifests are content-addressed. Logs and byte-based
source retention are bounded or reapable. Direct-SSH transfer staging
lives under the remote Vci root's `state/tmp/` with a private,
mktemp-randomized name prefixed by `vci-source-` and trap-based cleanup; the
reaper sweeps stale staging directories matching the prefix after a fixed age
without signalling any process.

Direct-SSH client build inputs are finite local file selections: tracked files
(HEAD, modified, staged), untracked non-ignored files, executable permission
modes, symlinks, and minimal repository markers (`.git/HEAD`, `.git/objects`,
`.git/refs`) are copied over direct tar-over-SSH, while ignored files
(`.gitignore` entries), locally deleted tracked files, private `.git/config`,
and `.git/objects` pack history are excluded. Initialized submodule
working-tree files are included recursively — every verified submodule
contributes its own tracked, modified, untracked non-ignored, and
executable-mode files under its validated prefix; an uninitialized or
otherwise unverifiable gitlink fails locally before any archive, and Vci
never executes `git submodule update`, fetches a URL, or contacts a host.
Hydrated Git LFS working-tree bytes are ordinary selected data and are
transferred, snapshotted, digested, cached, and built exactly like any
other file; a formal LFS pointer (`filter=lfs` attribution with the
pointer bytes still on disk) is rejected locally so the agent can run
`git lfs pull` first. Vci never invokes `git lfs`, reads `.git/lfs`,
extracts LFS URLs or tokens, or downloads an object. Child `.git`
metadata at any depth is excluded from the manifest, snapshot, cache
entry, and tar stream. `.gitmodules` is excluded at every depth: the
submodule's gitlink reference is the only approved path-restoration
signal, and a tracked `.gitmodules` cannot leak remote URLs or
embedded credentials into the build. The client first materializes a
private Vci-owned snapshot of the selection, computes the content
digest from that settled snapshot, and archives exactly that snapshot
— a source mutation between digest computation and archive production
cannot change the bytes the coordinator verifies.

Cache reuse is coordinator-local and content-addressed. Cache identity is
`(format_version, digest, project)` everywhere: each project owns an
independent entry root
`state/source-cache/v1/<digest>/<project>/` with its own metadata,
completion marker, active claims, and source tree (nested at
`<project>/<project>/` so the tree's final basename is the configured project
name). Two projects with identical selected content therefore keep separate
valid entries under the shared digest and cannot invalidate each other. The
client probes for a verified complete entry; on a hit the public remote
`vci build .` runs directly from the verified entry with no tar producer
at all. On a miss the staging shell records the expected key in a Vci-owned
`vci-meta` sibling file, the public remote `vci build .` recomputes the
canonical snapshot digest from the received bytes, and publishes only on
equality — a mismatch is an infrastructure failure that creates no complete
entry and returns no run ID. Publication is atomic (meta first, source tree,
`complete` marker last), serialized per key with bounded stale-lock reaping,
and never removes or overwrites an existing completed entry. The coordinator
enforces `retention.source_cache_bytes` before every publication admission;
when the setting is omitted the documented default (500 MB) applies, so a
default config cannot publish without a bound. Inactive
least-recently-used entries are evicted first, and an oversize incoming entry
or insufficient inactive capacity is rejected without over-publishing. Active
entries are counted in total capacity while a live public build captures
them, and the reaper removes only exact Vci-owned stale partials and locks. Any
read or archive failure rejects client submission before returning a remote
run ID. Unsupported source forms (linked worktrees, filenames containing
newlines) are rejected locally before network transmission. Local coordinator
builds retain their existing source-manifest behavior.
`./scripts/self-check.sh` exercises the complete local path without
using a hosted Git remote.

## Hosted Git fallback (explicit, coordinator-only)

`vci build --hosted <project>` is an explicit, second source mode for a
coordinator-configured pinned Git commit when an agent intentionally has
no local source path. It is not automatic, not a fallback from a failed
direct build, and not a submodule/LFS download.

```toml
[projects.Vidl.hosted_fallback]
url = "ssh://git@github.com/hypernewbie/vidl.git"
commit = "<40-or-64-lowercase-hex-object-id>"
```

```sh
vci setup project hosted set Vidl --url <url> --commit <object-id>
vci build --hosted Vidl
```

The URL is restricted to `https://host/path` or
`ssh://[user@]host/path`; the commit is a full lowercase 40- or
64-char hex object ID. Anything else — branch, tag, HEAD, refspec,
short SHA, mixed case, whitespace, `file://`, query, fragment — is
rejected at config-load time as `hosted_fallback_invalid` and never
reaches a checkout. The checkout runs `git init`, `git remote add
origin <url>`, `git fetch --depth=1 --no-tags origin <commit>`,
`git checkout --detach FETCH_HEAD`, and `git rev-parse HEAD` with
`core.hooksPath=/dev/null`, `protocol.file.allow=never`, and
`protocol.version=2`. Terminal prompts and askpass are forced to
fail non-interactively. The pinned commit is verified by exact
equality against `HEAD`; any mismatch surfaces as
`hosted_source_integrity_failed` and removes the checkout. The
checkout lives at `state/tmp/vci-hosted-<rand>/<project>`, is
removed on every exit path, and is swept by the existing temp
reaper on the `vci-hosted-` prefix. The staged run record gains an
additive `source_provenance.kind = "hosted_git"` block carrying
the validated URL and pinned commit; direct/local builds retain
their existing fields. No checkout path, credential, token, or
query is ever placed in the snapshot. The hosted path is
coordinator-only; a client root proxies `vci build --hosted
<project>` through ordinary `RemoteCommand` over SSH and never
holds a checkout or a run record.

## Container and VM runtime (per-machine, optional)

A machine may declare an optional runtime so the project's
command runs inside a declared image or VM snapshot instead of on
the coordinator host. The default path is bare execution. The
runtime selector is read from the durable run snapshot, so a run
whose machine config changes after submission still publishes the
runtime it was actually launched with.

```toml
[machines.linux-docker]
runtime = "docker"
image   = "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"

[machines.vm-linux]
runtime  = "vm"
snapshot = "ghcr.io/org/vm:pin"
```

```sh
vci setup machine add linux-docker --runtime docker --image ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000
vci setup machine add vm-linux     --runtime vm --snapshot ghcr.io/org/vm:pin
vci setup project add lintest --machine linux-docker --machine vm-linux \
    --command go --arg test --arg ./... \
    --artifact build/* --artifact dist/*.zip
vci build .
```

The image / snapshot reference is verbatim — Vci never builds,
pulls, or pins images from inside Vci. The host `docker` /
hypervisor config is used as-is. The validator rejects flag-like
values, scheme-bearing values, paths, whitespace, and an unknown
runtime.

The container runner shells out to the system `docker` binary
without a Go SDK. The exact arg slice is:

```
docker run --rm \
    -v <workspace>:/vci/work:ro \
    -w /vci/work \
    --network none \
    --user $(id -u):$(id -g) \
    --cpus 2 --memory 4g \
    <image> <command...>
```

The VM runner shells out to the system VM binary (`tart` on
macOS) without a Go SDK. The exact arg slice is:

```
tart run --no-gui \
    --dir <workspace>:/vci/work \
    --workdir /vci/work \
    --cpus 2 --memory 4g \
    <snapshot> -- <command...>
```

The `--dir` flag is the documented tart directory mount: the host
workspace is shared read-write with the guest at `/vci/work`. The
`tart` binary is the host's responsibility, exactly as the `docker`
binary is for the container runner.

In both runtimes the workspace is the only host path the guest
sees; Vci never mounts `~/.vci`, `state/`, or `~/.ssh`. Only the
per-run workspace is exposed. The runner respects the caller
context for cancellation and propagates the exit code as a job or
infrastructure failure. `runtime_unavailable` is an infrastructure
retryable envelope (binary missing or daemon refuses);
`runtime_image_not_found` is a configuration non-retryable
envelope (binary reports image-not-found).

The deferred extensions — image build/push inside Vci, automatic
registry auth, remote docker or remote VM, multi-host worker
selection — are not part of this release.

## Artifact collection (per-project, optional)

A project may declare workspace-relative glob patterns. Patterns
are matched per path segment: every segment of the glob except the
last matches exactly one path segment of the artifact, and the
final segment must match every remaining path segment. A trailing
bare `*` therefore collects the whole subtree (`build/*` includes
`build/sub/file.txt`), while a constrained final segment keeps the
match single-level (`build/*.bin` collects only files directly
inside `build/`). `**` has no special recursive meaning; it is
matched literally as an ordinary segment. After the
command finishes but before result publication, each matched
regular file is copied to `state/runs/<run_id>/artifacts/<rel>`
with the source's permission bits preserved. Symlinks, device
files, `..` escapes, and `.git`/`.vci` paths are rejected. The
per-run total byte cap is 64 MiB; matches beyond the cap are
dropped and `artifacts_truncated` is set on the build envelope.
`vci check <run_id>` surfaces `artifacts` and `artifacts_truncated`.

## Build and development

Build from source on the supported local macOS target:

```sh
go build -o ./vci ./cmd/vci
go test ./...
go test -race ./...
go vet ./...
./scripts/self-check.sh
```

After this module is published and tagged, an equivalent install is:

```sh
go install github.com/hypernewbie/vci/cmd/vci@<version>
```

The default product path is local. A client root that selects a
remote `orchestrator` forwards each public command to that
coordinator's ordinary public `vci` commands over the system SSH
executable. Source is copied into a Vci-owned staging directory on
the remote using ordinary tar-over-SSH, the public `vci build .`
runs from that staging directory, and the staging directory is
removed by a trap and stale-directory reaping. No relay, daemon,
custom protocol, source receiver, run-ID map, remote result
replica, or scheduled retry is used. The hosted-Git fallback
exists as an explicit, coordinator-only, pinned source mode
(`vci build --hosted <project>`); it is not a fallback from a
failed direct build, not automatic, and not a submodule/LFS
download. Docker, VMs, hosted remotes, submodule/LFS handling, and
permanent workspaces remain out of scope.

Vci is MIT licensed. See `LICENSE`.
