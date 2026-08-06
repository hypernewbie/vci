# Changelog

## Unreleased

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

