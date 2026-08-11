---
name: vci
description: Run CI through Vci and report where it ran, whether it passed, and why it failed. Use for build, check, logs, artifacts, abort, and for explicitly configuring this client to use an already-working remote coordinator. Do not use for coordinator, machine, or project administration.
---

# Vci: run CI on an already configured installation

Vci runs the coordinator-configured build command for this repository on
every attached machine and keeps the run ID, per-target exit code, logs,
and artifacts with the result. It is a single binary with no web service,
dashboard, queue, or custom protocol. The coordinator runs at most one
build at a time across all attached machines.

This skill runs builds and can bootstrap **only this client** to use an
already-working remote coordinator. It never administers a coordinator.

## Hard boundary — client bootstrap only

- Never run `vci setup ...` (including `setup init`, `setup machine add`,
  `setup project add`, or `setup reap`).
- Never create or change coordinator, machine, project, runtime, or retention
  configuration. Never invoke `vci-orchestrator` for client bootstrap.
- Never configure SSH, networking, host keys, runtimes, PATH, shell config, or
  Vci installation. The user-supplied coordinator destination already works.
- If `vci` is not on PATH, report that and stop.
- A missing client profile is bootstrapable only when the user explicitly asks
  to configure this client and supplies an existing coordinator destination.
  Other configuration errors are reported unchanged.

## Bootstrap this client for an existing coordinator

Use this branch only for a request such as “configure this client to use
`jupiter.local`.” Do not use it to initialize or administer a coordinator.

1. Use the user-specified client root, or `~/.vci` when none is specified.
2. Inspect the target first: if `<client-root>/config.toml` already exists and is a coordinator root (`orchestrator = "self"` or any `[machines]`/`[projects]`), **stop and ask**. That path owns coordinator state — do not overwrite it. Offer a distinct client root such as `~/.vci-client` or `VCI_ROOT=/tmp/vci-client` instead. Only proceed when the target is absent or already a client profile.
3. Create or replace only `<client-root>/config.toml` with:

   ```toml
   schema_version = 1
   orchestrator = "<user-supplied destination>"
   ```

   Preserve normal private file permissions. The destination is an opaque,
   already-working SSH alias or `user@host`; do not test or alter it.
4. Do not run inventory or build commands as bootstrap validation because they
   contact the coordinator. Report the configured client root and destination.

If the user does not explicitly provide both client intent and a coordinator
destination, ask for the missing fact and make no change.

`vci projects` and `vci machines` are safe, non-configuration inventory
commands during ordinary use. They can perform scheduler housekeeping, so do
not run them as mandatory preflight.

## Workflow

### 1. Wait for the coordinator to be ready

The coordinator runs at most one build at a time. When a previous build
may still be running, block on it so the submit does not need to poll:

```sh
vci wait-ready
vci wait-ready --interval 2   # seconds, 1..3600, default 1
```

`wait-ready` returns one JSON envelope with `data.ready: true` once the
coordinator has no live build. Skip it when the coordinator is known
to be idle; it is the safe gate before a build that follows a prior run.

### 2. Submit a build

```sh
vci build .
```

- Run from the repository checkout root. The coordinator matches the
  directory name to its configured project; no project name, machine, or
  command arguments are needed or allowed here.
- **Capture and retain the run ID.** The command returns one JSON
  envelope on stdout; the ID is `data.run_id`. Treat it as opaque
  (`run_...`). There is no command to list or search past runs.
- The CLI exits 0 when the build is **submitted**, not when it finishes.
  The build's own process exit code appears later in `check` output.
- If the build fails immediately with error code `coordinator_busy`
  and `retryable:true`, another build is already in flight: run
  `vci wait-ready` and resubmit. A retry is always a **new** run with a
  new run ID.

### 3. Inspect once

```sh
vci check <run-id>
```

Returns the aggregate for the build request. `data` carries the overall
`state`, an ordered `targets` array (one entry per attached machine, each
with `machine`, `state`, `exit_code`, `failure`, `error_context`, and
`warnings`), and aggregate counts (`succeeded`, `failed`, `lost`,
`unavailable`, `aborted`, `no_machine_responded`). A `lost` or
early-aborted target can have only its terminal record; treat absent
result fields as unavailable.

### 4. Wait / watch

Use the built-in watcher when you want Vci to wait for completion:

```sh
vci watch <run-id>
vci watch <run-id> --interval 5
vci watch <run-id> --exit-status
```

`watch` writes state changes to stderr and emits one final JSON envelope on
stdout. The default interval is 3 seconds and valid values are 1–3600
seconds. `--exit-status` returns exit code 1 when the final state is `failed`,
`lost`, or `aborted`.

For agents that need bounded control, poll `check` at a modest interval
every 5–10 seconds until `data.state` is terminal or a deadline passes:

- Active states: `queued`, `staging`, `running`, `committing`
- **Terminal states: `succeeded`, `failed`, `lost`, `aborted`**

Stop polling on any terminal state or when the deadline (for example 30
minutes, or a user-given cap) is hit; never busy-loop. `watch` itself has no
deadline; interrupt it or use bounded `check` polling when a deadline is
required.

### 5. Read logs

```sh
vci logs <run-id> --machine <name>              # that target's stdout
vci logs <run-id> --machine <name> --stderr     # that target's stderr
vci logs <run-id> --machine <name> --tail <n>   # last n lines (1..100000)
```

- Fan-out builds have one log stream per attached machine. The
  `--machine <name>` flag selects which target's log to read; pass the
  name from `data.targets[].machine` returned by `check`.
- Raw bytes on success; there is **no `logs --follow`**.
- A `lost` target has no published final result. Its logs can be absent
  or contain partial diagnostics written before the worker was lost.

### 6. Artifacts

```sh
vci artifacts ls <run-id>              # JSON: data.files, data.truncated
vci artifacts get <run-id> <rel>       # raw bytes to stdout
```

`artifacts get` streams the exact bytes; direct it to a file when saving.

### 7. Abort a known live run

```sh
vci abort <run-id>
```

Use for `queued`, `staging`, `running`, or `committing` runs. A queued
run becomes aborted immediately; other live runs receive a cancellation
request. Poll until any terminal state, normally `aborted`. Aborting an
already `lost` run currently succeeds as a no-op and leaves it `lost`.

### 8. Rerun

There is **no retry/rerun command**. To rerun, submit a new build
explicitly with `vci build .` and treat the new run ID as authoritative.

## JSON and stdout handling

- Commands normally print one JSON envelope to stdout:
  `{schema_version, command, ok, data, error}`. Vci-reported success exits
  0 and Vci-reported failure exits 2. Parse the envelope; never scrape
  human prose. A local output-I/O failure can instead report on stderr.
- Failure envelopes carry `error: {code, class, message, retryable}`
  (classes: `usage`, `configuration`, `infrastructure`, `state`).
- `logs` and `artifacts get` write **raw bytes** to stdout on success
  (exit 0); every failure still returns a JSON envelope on stdout
  (exit 2).
- Diagnostics and progress go to **stderr**; ignore them for result
  parsing.
- Do not add a `--json` flag — JSON is the default. `watch` is the polling
  command; there is no `logs --follow` stream.

## What a build tests

`vci build .` snapshots the current working tree, including non-ignored
local changes, rather than HEAD alone. The accepted snapshot is immutable;
edits made after submission do not change the run.

## Operational result report

After a run reaches a terminal state, report:

```text
Vci build <run-id>
Machines: <comma-separated list of data.targets[].machine>
Final state: <succeeded | failed | lost | aborted | aggregating>
Per-target: <machine>: <state> (exit_code=<n>, failure=<category>)
Cause: <first actionable error from targets[].error_context, if any>
Artifacts: <relevant files, none, or unavailable>
```

For failed targets, prefer `data.targets[].error_context` as the first
signal — it already carries the last lines of stderr (or stdout, when
stderr is empty). Fetch full logs only when the context is insufficient.
Use `data.failure` only as Vci's broad failure category. Keep the report
short and factual; do not dump full logs unless requested.
