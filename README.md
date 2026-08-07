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
are not required.

## First commands — coordinator

```sh
vci setup init                       # writes orchestrator = "self"
vci setup machine add mac-local                   # one local slot
vci setup machine add fast --capacity 4           # four local slots
vci setup project add Vci --machine mac-local --command go --arg test --arg ./...
vci setup project add big --machine mac-local --machine fast --command go --arg test --arg ./...
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
custom protocol, source receiver, run-ID map,
remote result replica, scheduled retry, or hosted-Git fallback is
used. Docker, VMs, hosted remotes, submodule/LFS handling, and
permanent workspaces remain out of scope.

Vci is MIT licensed. See `LICENSE`.
