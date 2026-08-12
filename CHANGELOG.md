# Changelog

## Unreleased

- Introduce application semantic versioning starting at `0.1.0`. The
  canonical version is `internal/version/VERSION`; `vci version` reports local
  build identity and `vci version --coordinator` queries the configured
  coordinator without changing protocol or state schema versions. Release
  tags use the matching `v` prefix and are validated by the tag workflow.
  See `docs/RELEASING.md` for compatibility and release rules.
- Fan-out failure context now preserves bounded tails from both stdout and
  stderr when both streams contain output, with deterministic `[stdout]` and
  `[stderr]` labels. A warning on stderr no longer hides compiler diagnostics
  on stdout; one-stream output remains unchanged.
- Source transfer uses Git reconciliation instead of whole-tree snapshots.
  A remote build sends its Git identity and local changes plus a Git bundle of
  the objects the coordinator lacks; the coordinator reconstructs a workspace
  from its configured local seed (or the bundle alone when no seed is
  configured), advances it to the client HEAD, applies the local changes,
  initializes submodules, and prunes coordinator-owned exclusions. A local
  build copies the working tree and prunes exclusions. Adds `vci probe-seed
  <project>` (the coordinator reports its seed commit) and `vci build
  --from-submission <project>` (the coordinator receiver, reading a submission
  tar on stdin). The whole-tree snapshot, digest, and source cache no longer
  participate in the client-to-coordinator path.
- Add `vci watch <run-id>` to poll a run until it reaches a terminal
  state. State changes are written to stderr and the final result is
  emitted as one JSON envelope on stdout. `--interval` controls the
  polling period (1..3600 seconds, default 3); `--exit-status` returns
  exit code 1 for failed, lost, or aborted runs.
- Plan 17 adds a read-only run log CLI. `vci logs <run-id>` streams
  the run's durable `stdout.log` bytes to stdout (binary-safe);
  `vci logs <run-id> --stderr` selects `stderr.log`; `--tail <n>`
  prints the last `n` lines with `n` bounded to 1..100000 (reads the
  whole file, no follow/tail daemon). `logs` is the second non-JSON
  stdout path (after `artifacts get`): raw bytes stream directly so
  binary/garbled output survives, while every failure still returns a
  JSON envelope. New `app.ReadLog` validates the stream
  (`ErrInvalidLogStream` → `invalid_arguments`), `Lstat`s to reject
  symlinks, and maps missing runs and missing log files to
  `not_found` (`model.ErrRunNotFound`/`ErrLogNotFound`). On a client
  root `logs` proxies via `runSSHRaw` exactly like `artifacts get`
  (`RemoteLog`), so the tail is applied on the coordinator and only
  the requested window crosses the wire. The durable log files are
  unchanged: still `state/runs/<id>/{stdout,stderr}.log`, still
  referenced by `vci check`'s `stdout_path`/`stderr_path`. No new
  source protocol, relay, daemon, framed protocol, or Go SSH client.
- Plan 16 makes collected artifacts observable and bounded. `vci
  artifacts ls <run-id>` returns the collected relative paths plus
  the durable 64 MiB cap flag as JSON; `vci artifacts get <run-id>
  <rel>` streams the artifact's exact raw bytes to stdout (the only
  non-JSON stdout path in the CLI, so binary content survives), and
  every failure still returns a JSON envelope. On a client root both
  proxy to the coordinator over ordinary ssh (`RemoteCommand` for
  `ls`, a raw `ssh` stream for `get`), exactly like `check`. The
  relative path is validated before any filesystem access (no `..`,
  no absolute path, no control/whitespace, no `.git`/`.vci` segment);
  `GetArtifact` `Lstat`s to reject symlinks swapped in after
  collection. `reaper.ReapArtifacts` removes the artifacts directory
  of `lost`/`aborted` runs older than 30 minutes, while `succeeded`
  and `failed` runs keep their artifacts until the run itself is
  reaped; `app.Maintain` surfaces `artifacts_reaped`. No put/push/
  registry surface, no new source protocol, no relay or daemon.
- Plan 15 lets a machine declare a remote worker host. A machine
  gains an optional `host` field — a strict system `ssh` destination
  (alias or `user@host`; no leading `-`, no whitespace/control
  characters, no `://` scheme, no `..` segment). Empty means the
  worker runs on the coordinator host. `setup machine add --host`
  accepts only validated values, and `vci machines` surfaces `host`
  in the JSON envelope. Bare, docker, and vm runtimes all work
  locally or on the named host: the detached worker stages the
  materialized workspace into the remote
  `~/.vci/state/work/<run>` tree via the existing `tar | ssh`
  channel, runs the same runtime argv there via `ssh <host>
  <sh -c ...>`, and fetches artifacts back with `scp` over the same
  ssh channel before the coordinator's own collector publishes the
  matches. Only the workspace path and the project environment
  cross the ssh boundary. No relay, daemon, framed protocol, Go SSH
  client, or new subcommand is introduced; `orchestrator` still
  selects the single coordinator, and client roots still reject all
  `[machines.*]` tables. The reaper owns every temp root: it sweeps
  per-run `state/work/<run>/.tmp` and `.home` older than 30 minutes
  and removes the whole workspace once the run is terminal and its
  lease is gone, and `reaper.CleanupRemote` removes the mirrored
  remote `~/.vci/state/work/<run>` tree via `ssh <host> rm -rf`.
  `scripts/self-check.sh` and `scripts/detach-check.sh` now wait,
  bounded, for the detached worker to release its owned state
  (workspace, lease, scheduler claim) before the EXIT trap runs, so
  the trap's `chmod -R u+rwx` + `rm -rf` (with `|| true`) can never
  race a still-writing worker and leave a `.../state: Directory not
  empty` turd.
- Plan 14 adds a third executable machine runtime (`runtime = "vm"`)
  alongside `runtime = "docker"`, and a bounded per-project artifact
  collection. The VM runner shells out to the system `tart` binary
  (configurable `Binary` field for tests; default `tart`) without a
  Go SDK; exact arg slice:
  `tart run --no-gui --dir <workspace>:/vci/work --workdir
  /vci/work --cpus 2 --memory 4g <snapshot> -- <command...>`.
  The `--dir` flag is the documented tart directory mount: the
  host workspace is shared read-write at `/vci/work`. The
  workspace is the only host path
  the guest sees; `~/.vci`, `state/`, and `~/.ssh` are never
  mounted. The snapshot reference is verbatim and validated through
  the same allow-list as docker images. `config.ValidateMachineRuntime`
  rejects `runtime = "vm"` without a snapshot, and rejects a docker
  machine that carries a stray snapshot field. `selectExecutor`
  dispatches `runtime = "vm"` to `runtime.VM`; the bare host remains
  the default when the runtime is empty. Artifact collection adds
  a `Project.Artifacts []string` field (TOML `artifacts`, JSON
  `artifacts`); `setup project add` accepts repeated
  `--artifact <glob>` flags. Globs match per path segment: a
  trailing bare `*` collects the whole subtree (`build/*` matches
  `build/sub/file.txt`), while a constrained final segment is
  single-level (`build/*.bin` matches only files directly inside
  `build/`); `**` has no special recursive meaning. After the
  executor returns and before
  result publication, every matched regular file is copied to
  `state/runs/<run_id>/artifacts/<rel>` with the source's
  permission bits; symlinks, `.git`, `.vci`, and `..` paths are
  rejected. The per-run cap is 64 MiB; matches beyond the cap are
  dropped and `artifacts_truncated` is set on the build envelope.
  `vci check <run_id>` surfaces `artifacts` and `artifacts_truncated`.
  The source-cache identity `(format_version, digest, project)` is
  unchanged: bare, docker, and VM runners produce byte-equal
  selected bytes so cache hits are shared. No relay, daemon,
  custom protocol, source receiver, Docker socket, privileged
  mount, host network, or VM hypervisor SDK is added; the runner
  is the host's responsibility.
- Plan 13 Fix repairs four Plan 13 defects the original tests did
  not pin. (1) `TestDockerRunsViaStub` flagged any `state/`
  substring in the docker `run` arg string as a dangerous mount
  leak, but the runner only ever mounts the per-run workspace,
  which legitimately lives under `<root>/state/work/<run>/` and
  `<root>/state/work/<run>/.tmp/`. The banned-mount check now
  distinguishes the per-run workspace mount from a real
  state-root, `.vci`, or `.ssh` mount, so `vci build .` runs the
  full test suite inside its own per-run workspace without a
  false-positive failure. (2) `runtime.Docker` now uses
  `os.Getuid()/os.Getgid()` so the container identity matches the
  coordinator host (previously always `0:0`). (3) The image
  validator accepts an optional `host:port/` registry prefix
  (`myregistry:5000/repo:tag`,
  `myregistry:5000/repo@sha256:...`) while keeping the strict
  no-shell, no-scheme, no-path allow-list. (4) `selectExecutor`
  reads the durable `snapshot.Machine` (the reserved machine),
  falling back to `ProjectConfig.Machines[0]` only for legacy
  records; a multi-machine project that reserves a docker
  machine always selects the docker runner.
- Plan 13 introduces an optional per-machine container runtime.
  A coordinator machine may declare `runtime = "docker"` and a
  verbatim image reference; the project's command then runs
  inside the container via the system `docker` binary. The
  default path is bare execution: a machine with no runtime
  fields runs exactly as before. The runner is selected from
  the durable run snapshot (not live config), so historical
  runs remain explainable after config changes. The executor
  arg slice is exact (no shell, no template interpolation):
  `docker run --rm -v <workspace>:/vci/work:ro -w /vci/work
  --network none --user <uid>:<gid> --cpus 2 --memory 4g
  <image> <command...>`. The workspace is bind-mounted
  read-only; Vci never mounts `~/.vci`, `state/`, or `~/.ssh`.
  The image validator is strict: a verbatim
  `[A-Za-z0-9._-]` registry/repo reference with optional tag
  and `@sha256:<64hex>` digest; flag-like values, scheme-bearing
  values, paths, whitespace, and unknown runtime values are
  rejected at config-load time. `runtime = "vm"` is reserved
  for a future slice and rejected as `unsupported_runtime`.
  Three typed envelopes are mapped via `errors.Is` before the
  substring classifier: `runtime_unavailable` (infrastructure
  retryable when docker is missing or refuses), `runtime_image_not_found`
  (configuration non-retryable when docker exits 125), and
  other docker failures are classified by exit code. Client
  roots continue to reject `[machines.*]` tables. The source
  cache, scheduler, hosted fallback, and direct-SSH transport
  behaviour are unchanged. No relay, daemon, custom protocol,
  source relay, source receiver, or remote worker was added.
- Plan 12 Fix repairs three Plan 12 defects that the original
  tests did not pin. The materialized snapshot now allow-lists
  the three top-level minimal git markers (`.git/HEAD`,
  `.git/objects`, `.git/refs`) so the client tar list references
  files that actually exist in the snapshot. Nested `.git` at
  any depth, `.gitmodules` at any depth, and arbitrary `.git`
  content (config, hooks, packed-refs, objects/pack) stay
  excluded because gitlinks, not the `.git` directory, are the
  contracted path-restoration signal. The committed `Plan 12`
  also broke the default direct-SSH client path (`TestStagingTrapLeavesExternalSymlinkTargetUnchanged`
  failed deterministically with `tar: .git/HEAD: Cannot stat`).
  The host class of the URL regex is tightened so the host
  segment cannot legally contain characters that match an
  alphabetic port, the path is required (the optional `/...`
  group is now mandatory), and an explicit pre-check rejects any
  userinfo containing `:` so a future regex change cannot
  regress the password rejection. The optional port is validated
  against the TCP range 1..65535. The hosted integrity check
  switches from `strings.EqualFold` to exact equality after
  `TrimSpace`; both sides are validated lowercase hex, so a
  case-mismatched pin is a genuine integrity failure (hostile
  redirect or default-branch resolution) that the explicit
  comparison will surface. The env-inheritance test now
  pre-seeds conflicting `GIT_TERMINAL_PROMPT`/`GIT_ASKPASS`/`GIT_ASKPASS_REQUIRE`
  values so a future regression that forgets to drop the
  inherited value before appending the override will fail. README
  is updated to reflect that the hosted-Git fallback is an
  explicit coordinator-only pinned source mode, not a fallback
  from a failed direct build.
- Plan 12 introduces an explicit coordinator-configured pinned
  hosted Git fallback source mode. `vci build --hosted <project>`
  is a coordinator-only two-argument form that pins one configured
  full lowercase hex commit (40- or 64-char) on one configured
  `https://host/path` or `ssh://[user@]host/path` URL. Direct
  `build <path>` remains the default and preferred path; no
  automatic transition exists from a failed direct build to hosted
  Git, and no fallthrough from any error. Client roots proxy the
  command through ordinary `RemoteCommand` over SSH; the client
  sends no tar, no URL, no commit, no configuration mutation, and
  no run record. The new `setup project hosted set|clear` subcommands
  mutate only a coordinator root through `config.Mutate` and
  validate the URL+commit before any disk write. The CLI surface is
  closed: `--hosted` is the only allowed flag form, and every
  invalid shape returns `invalid_arguments`. The four typed error
  codes (`hosted_fallback_not_configured`,
  `hosted_fallback_invalid`, `hosted_source_unavailable`,
  `hosted_source_integrity_failed`) are mapped to stable envelopes
  with documented class and retryable flag, and a remote valid
  failure envelope is relayed unchanged rather than relabeled as
  SSH failure. The hosted checkout uses the system `git`
  executable directly via `process.Runner` — no shell, no template
  interpolation beyond the validated URL and commit — with
  `core.hooksPath=/dev/null`, `protocol.file.allow=never`, and
  `protocol.version=2`; the merged environment inherits `os.Environ`
  so `PATH`, `HOME`, and `SSH_AUTH_SOCK` are preserved. Every git
  command's env includes `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=true`,
  and `GIT_ASKPASS_REQUIRE=force` so no interactive prompt can
  block a coordinator build. The pinned commit is verified by
  reading `git rev-parse --verify HEAD` after checkout and
  comparing for exact lowercase equality; any mismatch is a hard
  integrity failure. The checkout lives in
  `l.TempDir()/vci-hosted-<rand>/<project>` and is removed on every
  exit path; stale `vci-hosted-*` roots are swept by the existing
  reaper prefix match. The staged run record gains an additive
  `source_provenance` block (`kind: "hosted_git"`, validated URL,
  pinned commit); direct/local builds keep their existing fields.
  No checkout path, credential, token, or query is ever placed
  in the snapshot. Hosted builds do not participate in
  source-cache admission: a hosted root matches neither the
  cache-entry shape nor the staging shape, so the source-cache
  path is intentionally skipped. Hosted fallback deliberately does
  not fetch submodules or LFS objects; a gitlink fails as
  `submodule_unavailable` and an unhydrated LFS pointer fails as
  `lfs_content_unavailable` through the existing typed errors. No
  `file://` URL, public network, source relay, daemon, or Go SSH
  implementation is added; no clone mirror, retry queue, branch /
  tag / HEAD resolution, or credential store. The full release
  gate must remain green.
- Plan 11 Fix repairs the Plan 11 scheduler, source, and proof
  surfaces after review found four residual correctness defects
  that the original tests did not pin. The new scheduler is
  fail-closed: a corrupt, symlink, wrong-body, or future-dated
  claim surfaces as a hard error from `Status`, `Reap`, and
  `ReserveAndPublish`; only `ReserveAndPublish` may create a claim,
  and only with a publish callback that persists a real run record.
  The `Reserve` wrapper is removed; the legacy no-callback shape
  could leak orphan claims. The `withLock` helper takes the
  in-process Go mutex first and then the on-disk flock; the order
  is documented and uniform. Claim files are written via a unique
  same-directory temporary file with `Sync` and parent-directory
  `Sync`, then atomic rename to an absent target. `Release`
  validates the exact claim body before removal; missing is
  idempotent, corrupt/mismatched is an error. `ExecutePrepared`
  requires `staging` state, validates the reservation, claims the
  worker lease, and re-checks the state before any workspace,
  manifest, or process work; a terminalized record skips all
  materialization. Future-dated `now` arguments and stored
  `created_at` values are rejected as corrupt state, so clock skew
  cannot grant an unbounded grace.
- Plan 11 Fix tightens source input validation. `ValidateInput`
  is the canonical entry helper that canonicalizes, deduplicates,
  sorts, and rejects `.gitmodules`, parent traversal, absolute
  paths, trailing slashes, empty segments, and newline-bearing
  filenames. `.gitmodules` and every `.git` component at any depth
  are excluded from snapshots, manifests, cache entries, and tar
  streams. The gitlink stage parser strictly parses mode/hash/
  stage triples; malformed records, non-zero stages, duplicate
  paths, and empty paths surface as wrapped
  `ErrSubmoduleUnavailable`. The recursive builder's
  `MaterializeSnapshot` always runs `ValidateInput` on its
  argument, so a `SourceInput` cannot reach the snapshot without
  passing the canonical contract.
- Plan 11 Fix binds LFS validation to the bytes that actually
  build. Local coordinator builds use `BuildWithValidation`, which
  checks every LFS-attributed regular file against the formal
  pointer format against the exact bytes that become a blob. A
  typed `ErrLFSContentUnavailable` is reported before any
  reservation is taken or any run record is created. The
  recursed-direct-snapshot path retains its settled
  post-materialization validation.
- Plan 11 Fix replaces the false-baseline tests with deterministic
  failures. `TestStatusDoesNotCountMalformedClaim` is gone; the
  new `TestStatusFailsClosedOnCorruptClaim` and
  `TestReserveAndPublishFailsClosedOnCorruptClaim` pin that
  corrupt claim state never silently frees capacity. The
  previously missed nested-gitlink symmetry test now uses a real
  nested gitlink, and the LFS-validation tests cover the
  hydrated-to-pointer race deterministically.
- Plan 11 extends the finite source input to initialized submodule
  working-tree files and hydrated Git LFS working-tree bytes.
  Selected input is built from a recursive graph: every verified
  repository (top-level plus every initialized submodule) contributes
  its own tracked, modified tracked, untracked non-ignored, and
  executable-mode files under its validated prefix. Every child
  gitlink is verified by `git rev-parse --show-toplevel` against the
  expected child directory before recursion; an uninitialized,
  missing, symlinked, escaping, conflicted, or otherwise
  unverifiable gitlink fails with typed `ErrSubmoduleUnavailable`,
  class `configuration`, retryable false, naming the top-root-
  relative path so the agent can run the ordinary user action
  `git submodule update --init --recursive`. Vci never executes
  that command, fetches a URL, reads `.git/config`, or contacts a
  host. The submodule's working-tree content is transferred, not
  its Git administration: every `.git` component at any depth
  (directory or file) is excluded from the manifest, snapshot,
  cache entry, and tar stream. There is no separate submodule
  cache, commit-ID key, or remote lookup — recursive bytes enter
  the settled snapshot digest naturally.
- Plan 11 validates LFS hydration: for every selected regular file
  with Git attribute `filter=lfs`, Vci reads the settled snapshot
  bytes and rejects a formal pointer (version line, lowercase
  sha256 OID, decimal size) with typed `ErrLFSContentUnavailable`,
  class `configuration`, retryable false, naming the path so the
  agent can run `git lfs pull`. Attribute semantics, not magic
  content alone, decide rejection. Hydrated LFS working-tree bytes
  are ordinary selected data and are transferred, snapshotted,
  digested, cached, and built exactly like any other file. Vci
  never invokes `git lfs`, reads `.git/lfs`, extracts LFS URLs or
  tokens, or downloads an object. An installed LFS client is not
  required when the working-tree bytes are already hydrated. The
  LFS check applies to the top repository and every initialized
  submodule for both local and direct-SSH paths.
- Plan 10 Fix repairs the Plan 10 scheduler after review found four
  crash-window, integrity, API-truth, and evidence defects. The
  scheduler holds a single transaction API (`ReserveAndPublish`) that
  publishes the staging record inside the reservation under one
  in-process guard and one on-disk lock; a crashed caller cannot
  leak an orphan claim and a record-publish failure rolls back the
  exact claim. The new pre-start grace (60 s) protects a freshly
  published staging run while its detached worker races to claim a
  normal worker lease; an active lease always overrides claim age.
  Legacy queued records with no lease terminalize as aborted through
  the new `queued_aborted` report counter and never hold scheduler
  capacity indefinitely. Claim validation rejects malformed JSON,
  wrong schema versions, mismatched machines or run ids, symlinks,
  non-regular files, and zero created_at values; corrupt state is
  retained on disk for the operator and never silently counted as
  free capacity. Claims are published with O_EXCL create+Sync+parent
  Sync and an exact (machine, runID) write refuses to overwrite an
  existing reservation. Successful local `build` JSON now carries
  the selected `machine`, and a `scheduler.Status` failure propagates
  to `vci machines` as a state error instead of fabricating
  availability. The worker (`ExecutePrepared`) verifies its
  reservation through the scheduler validator, not a raw `Stat`. A
  claim with a missing record is reaped as orphan state. Capacity
  exhaustion remains `machine_unavailable`, class `state`, retryable
  true, and returns no run ID. The scheduler is still not a queue,
  daemon, listener, remote executor, source receiver, retry loop, or
  hosted-Git fallback. Direct client→coordinator transport is
  unchanged.
- Plan 10 introduces coordinator-local multi-machine scheduling. A
  configured machine is a named, coordinator-owned local capacity
  pool; each machine has an optional `max_concurrent` slot capacity
  (omitted means one slot for compatibility). A project may attach
  to multiple machines; submission reserves one available slot
  atomically, stores the selected machine in the durable run record,
  and starts the existing detached local worker on that machine.
  Capacity exhaustion returns a stable `machine_unavailable` JSON
  error, retryable true, with no run ID and no leaked claim. The
  scheduler is **not** a queue, a daemon, or a remote executor — it
  is coordinator-local capacity on this host. There is no Docker, VM,
  remote worker, source relay, hosted Git fallback, retry queue, or
  hosted control plane.
- Plan 9 Fix repairs four concrete review findings after Plan 9: the
  legacy `process.CancellationKilled` value is restored as a read-
  compatible accepted phase in `Execution.Validate`; the unexported
  `validDigest` alias in `sourcecache` is removed so every package
  call site routes directly to `ValidDigest`; the public CLI
  rejection table for malformed run IDs covers `run_/../../x` on
  `check`/`abort`/`internal-run`; and `TestSnapshotDigestIndependentOfSelectedMtimes`
  sets distinct mtimes on the materialized snapshot files
  themselves, not only on the working-tree source file. Legacy
  `run.json` (with removed `result` and `cancellation_phase` fields)
  and legacy `lease.json` (with removed `attempt`) are tolerated via
  plain `json.Unmarshal`.
- Plan 9 subtracts dead code from the source-cache implementation and
  consolidates duplicates. The removed surfaces had zero production
  callers: `SaveResultState`, `Store.lockPath`, `Store.loadUnlocked`,
  `Reaper.leaseExpired`, `executor.Executor`, `executor.Local.Execute`,
  `lease.Expired`, `config.Save`, `app.RemoveProject`, `app.UpdateProject`,
  `app.Build`, `model.ProjectName`, `model.MachineName`,
  `VciError.Details`, `Lease.Attempt`, `RunRecord.Result`,
  `RunRecord.CancellationPhase`, the list-form `source.ComputeDigest`
  and `source.Canonicalize`, `NewTarExtract`, `StashPartialProject`,
  `PurgePartial`, `sourcecache.validDigest`'s shadow duplicate,
  `app.digestShape`, and `cli.validRunID`'s prefix-only rule. Safe
  merges: the digest-shape rule is owned by `sourcecache.ValidDigest`;
  run-id validation is owned by `model.ValidRunID`; the
  staging/snapshot prefixes are exported as `source.StagingPrefix`,
  `source.SnapshotPrefix`, and `source.StagingMetaName`. The remaining
  tree is exactly the public minimal Vci contract and its bounded
  direct source cache.
- Plan 8 Fix Fix repairs the source cache implementation after review
  found `main` failed real SSH builds (`source is not a Git repository`)
  and the Plan 8 Fix cache was disconnected from production. The
  staging shell regression is fixed: the remote public `vci build .`
  runs from the staged project directory, not the SSH session home.
- Cache publication is now reached by the real public build path, not
  only by unit tests. The client materializes a private Vci-owned
  snapshot of the selected build input, computes the digest from that
  settled snapshot, and archives exactly that snapshot. The coordinator's
  public `vci build .` detects the direct staging tree under its
  Vci-owned `state/tmp/` root, reads the Vci-owned `vci-meta` identity,
  recomputes the canonical snapshot digest from the received bytes, and
  publishes only on equality — a mismatch is an infrastructure failure
  that leaves no complete entry and returns no run ID.
- Cache identity is `(format_version, digest, project)` everywhere: the
  lock, metadata, completion marker, active claim, publication target,
  lookup probe, and eviction unit all share one key. Each project owns
  an independent entry root `v1/<digest>/<project>/` containing its own
  meta.json, complete marker, active claims, and source tree (nested at
  `<project>/<project>/` so the tree's final basename is the configured
  project name). Two projects with identical selected content keep
  separate valid entries under the shared digest and cannot clobber
  each other's metadata. The probe and the staging fragments are
  composed from the sourcecache layout helpers
  (`EntryRootRel`/`EntryTreeRel`), so no separately formatted shell
  path can diverge from the Go-side layout. A verified `complete`
  marker plus matching metadata is required for a hit; a bare
  directory or an incomplete/corrupt path is a miss, never a build
  source.
- The documented default source-cache quota (500 MB) is applied to
  admission before every publication, not only to the reaper:
  `Prepare` and the reaper share `reaper.SourceCacheQuota`, so a
  coordinator config that omits `source_cache_bytes` remains bounded
  and an oversize incoming source is rejected instead of publishing
  without a limit.
- Publication is atomic (meta.json first, project tree rename without
  clobbering, complete-marker last), serialized per key with a bounded
  wait and stale-lock reaping, and never removes or overwrites an
  existing completed entry; a same-key competitor discards its own
  partial after observing the winner.
- The retention policy gains a `source_cache_bytes` quota (default
  500 MB) owned by the coordinator; client roots cannot set it. Admission
  runs before publication: inactive least-recently-used entries are
  evicted first, an oversize incoming entry is rejected, and
  insufficient inactive capacity is a rejection, not an over-quota
  publish. LRU eviction follows the recorded `LastUse` field updated on
  real cache hits, not file mtimes inside the project tree. Active
  entries are counted in total capacity while a live public build
  captures them and are never evicted. The reaper removes only exact
  Vci-owned stale partials and locks and reports cache bytes, quota,
  evictions, scratch removals, and oversized-entry rejections.
- Direct-SSH transport is instrumented at the real boundary: the client
  reports measured source tar bytes on a miss and no tar producer starts
  on a hit (proved by a fake-tar sentinel over controlled SSH).
  Measurement evidence is in `temp/PLAN8_FIX_FIX_MEASUREMENT.md`.
- Restore the client/coordinator split: a coordinator root declares
  `orchestrator = "self"` and owns machines, projects, retention, and
  log limits; a client root declares any other value as the SSH
  destination and is rejected if it carries coordinator-owned fields.
  Public client commands (`build`, `check`, `abort`, `machines`,
  `projects`) proxy to the coordinator over the system `ssh`
  executable; the remote run identity is returned unchanged.
  Administrative `setup` mutations on a client root reject locally.
- Contain direct SSH staging under the remote Vci root's
  `state/tmp/` with a private, mktemp-randomized directory named
  after the project basename. The trap-driven `rm -rf` is flat over
  the staging root only — no recursive chmod, no symlink-following
  — so a hostile source tree cannot trick cleanup into touching
  external paths. The reaper sweeps stale `vci-source.*`,
  `vci-source-*`, and `vci-snapshot-*` directories after a fixed age
  without signalling any process. The fixed 15-second top-level
  timeout is removed; cancellation is owned by the caller context and
  the real SSH failure.
- Preserve the remote JSON envelope when the remote `vci` exits
  non-zero with a valid response; only no-response,
  malformed-response, schema mismatch, or genuine SSH failure is
  classified as infrastructure. Source-cache verification failures are
  classified as infrastructure.
- Implement finite direct-SSH client build input selection
  (`source.SelectBuildInput`): tracked files (HEAD, modified,
  staged), untracked non-ignored files, executable modes, symlinks,
  and minimal repository markers (`.git/HEAD`, `.git/objects`,
  `.git/refs`) are selected locally prior to SSH transfer. Ignored
  files (`.gitignore` entries), locally deleted tracked files,
  private `.git/config`, and `.git/objects` pack history are excluded.
  Linked worktrees (`.git` file) and filenames containing newlines are
  rejected locally before network transmission. Local coordinator
  builds retain their existing source-manifest behavior.
- Plan 8 Fix completion claims (sealed verified cache on `main`) were
  not release evidence: the review found real SSH builds failing and
  the cache disconnected. Those claims are corrected here; the current
  record describes only behavior exercised by the controlled-SSH
  integration suite.
- Keep durable runs, worker-owned cancellation, leases, bounded logs
  and source retention, content-addressed source blobs, and strict
  configuration. The deferred list (relay, hosted-Git fallback,
  Docker/VM runtimes, submodules/LFS, artifacts, multi-machine
  scheduling) remains deferred.

## 0.0.0-dev

- Initial local macOS agent-first CI harness.

