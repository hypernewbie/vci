# Changelog

## Unreleased

- Restore the client/coordinator split: a coordinator root declares
  `orchestrator = "self"` and owns machines, projects, retention, and log
  limits; a client root declares any other value as the SSH destination and
  is rejected if it carries coordinator-owned fields. Public client commands
  (`build`, `check`, `abort`, `machines`, `projects`) proxy to the
  coordinator over the system `ssh` executable; the remote run identity is
  returned unchanged. Administrative `setup` mutations on a client root
  reject locally.
- Contain direct SSH staging under the remote Vci root's `state/tmp/` with
  a private, mktemp-randomized directory named after the project
  basename. The trap-driven `rm -rf` is flat over the staging root only —
  no recursive chmod, no symlink-following — so a hostile source tree
  cannot trick cleanup into touching external paths. The reaper sweeps
  stale `vci-source.*` and similar directory prefixes after a fixed age
  without signalling any process. The fixed 15-second top-level timeout
  is removed; cancellation is owned by the caller context and the real
  SSH failure.
- Preserve the remote JSON envelope when the remote `vci` exits non-zero
  with a valid response; only no-response, malformed-response, schema
  mismatch, or genuine SSH failure is classified as infrastructure.
- Define the literal source transfer behavior: a complete working-tree archive
  including committed files, `.git/`, untracked files, ignored content,
  symlinks, and executable bits is copied via direct tar-over-SSH, without
  status-aware filtering or incremental synchronization.
- Plan 6 measurement record (temp/PLAN5_PHASE5.md) confirms a literal
  tar-over-SSH transport with no delta, no remote cache, no manifest
  negotiation. Plan 6 ships option 1 from Phase 6 (keep tar and stop):
  there is no incremental path earned on this evidence. The deferred
  list (retained source caches, manifest comparison, hosted-Git
  fallback, Docker/VM runtimes, submodules/LFS, artifacts, multi-machine
  scheduling) remains deferred.
- Keep durable runs, worker-owned cancellation, leases, bounded logs and
  source retention, content-addressed source blobs, and strict configuration.

## 0.0.0-dev

- Initial local macOS agent-first CI harness.

