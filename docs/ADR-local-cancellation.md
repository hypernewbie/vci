# ADR: Local cancellation ownership

## Decision

`vci abort` records a cancellation request under the run lock and returns. It
never signals a numeric PID or process group. The detached worker retains the
child process handle and supervises its process group: it sends TERM, waits a
bounded grace period, then sends KILL if the owned child remains active. The
worker reaps the child before publishing one terminal result.

Run locks use kernel-released `flock` and are never held across materialization,
command execution, signal grace, or process waits. Execution and lease files
are private, atomic JSON publications. A worker that loses its lease terminates
its own group and does not publish a competing result. A worker that died before
recovery cannot be safely signalled; its run converges to `lost` after ownership
is stale.

## Scope

This is the local macOS product only. PID start-time probing and external kill
paths are intentionally not implemented. Future remote work is not part of this
product; if approved later, it must begin with an explicit direct-SSH contract.
