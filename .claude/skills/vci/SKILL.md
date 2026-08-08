---
name: vci
description: Run CI on this repository with an already configured Vci installation and report where it ran, whether it passed, and why it failed. Trigger when the user asks to run/check CI or a build with Vci ("run vci", "vci build", "run CI on this repo", "check this build", "did the build pass"), when a run ID from a previous Vci build must be checked, logged, aborted, or its artifacts retrieved, or when Vci results should be turned into a plain-language report. Never trigger setup, installation, or configuration tasks; if Vci is missing or unconfigured, report that and stop.
---

# Vci: run CI on an already configured installation

Vci runs the coordinator-configured build command for this repository on a
configured machine and keeps the run ID, machine, exit code, logs, and
artifacts with the result. It is a single binary with no web service,
dashboard, queue, or custom protocol.

This skill only **uses** an already configured Vci installation. It never
sets one up.

## Hard boundary — do not configure, ever

- Never run `vci setup ...` (including `setup init`, `setup machine add`,
  `setup project add`, `setup reap`). Never write to or create `~/.vci`
  (or any `VCI_ROOT`) yourself.
- Never change coordinator or machine/project configuration.
- Never install Vci or modify PATH or shell config for it.
- If `vci` is not on PATH or reports a configuration error, report the
  exact problem and stop. Do not attempt to fix it.
- `vci projects` and `vci machines` are safe, non-configuration
  inventory commands when diagnosis needs them. They can perform
  scheduler housekeeping, so do not run them as mandatory preflight.

## Workflow

### 1. Submit a build

```sh
vci build .
```

- Run from the repository checkout root. The coordinator matches the
  directory name to its configured project; no project name, machine, or
  command arguments are needed or allowed here.
- **Capture and retain the run ID.** The command returns one JSON
  envelope on stdout; the ID is `data.run_id`. Treat it as opaque
  (`run_...`). There is no command to list or search past runs.
- The current submission response has `data.state: "staging"` and
  `data.machine` for the selected machine.
- The CLI exits 0 when the build is **submitted**, not when it finishes.
  The build's own process exit code appears later in `check` output.
- If the build fails immediately with error code `machine_unavailable`
  and `retryable:true`, all eligible machine slots are busy: wait a
  short time, then submit a new build. A retry is always a **new** run
  with a new run ID.

### 2. Inspect once

```sh
vci check <run-id>
```

Returns the current run record. For a terminal run with a published
result—normally `succeeded`, `failed`, and some `aborted` runs—`data`
also includes exit code, failure category, truncation flags, and artifact
fields. A `lost` or early-aborted run can have only its terminal record;
treat absent result fields as unavailable.

### 3. Wait / watch (bounded polling)

There is no `--follow`, `--watch`, or streaming mode. Poll `check` at a
modest interval (every 5–10 seconds) until `data.state` is terminal or a
deadline passes:

- Active states: `queued`, `staging`, `running`, `committing`
- **Terminal states: `succeeded`, `failed`, `lost`, `aborted`**

Stop polling on any terminal state or when the deadline (for example 30
minutes, or a user-given cap) is hit; never busy-loop.

### 4. Read logs

```sh
vci logs <run-id>              # stdout stream
vci logs <run-id> --stderr     # stderr stream
vci logs <run-id> --tail <n>   # last n lines (1..100000)
```

- Raw bytes on success; there is **no `logs --follow`**.
- A `lost` run has no published final result. Its logs can be absent or
  contain partial diagnostics written before the worker was lost.

### 5. Artifacts

```sh
vci artifacts ls <run-id>              # JSON: data.files, data.truncated
vci artifacts get <run-id> <rel>       # raw bytes to stdout
```

`artifacts get` streams the exact bytes; direct it to a file when saving.

### 6. Abort a known live run

```sh
vci abort <run-id>
```

Use for `queued`, `staging`, `running`, or `committing` runs. A queued
run becomes aborted immediately; other live runs receive a cancellation
request. Poll until any terminal state, normally `aborted`. Aborting an
already `lost` run currently succeeds as a no-op and leaves it `lost`.

### 7. Rerun

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
- Do not add a `--json` flag — JSON is the default and flags like
  `--watch`/`--follow` do not exist.

## What a build tests

`vci build .` snapshots the current working tree, including non-ignored
local changes, rather than HEAD alone. The accepted snapshot is immutable;
edits made after submission do not change the run.

## Operational result report

After a run reaches a terminal state, report:

```text
Vci build <run-id>
Machine: <data.machine>
Final state: <succeeded | failed | lost | aborted>
Exit code: <data.exit_code> or unavailable
Failure category: <data.failure> or unavailable
Cause: <first actionable error from a bounded stderr/stdout tail, if needed>
Artifacts: <relevant files, none, or unavailable>
```

For failed runs, read a bounded stderr tail and, if needed, a stdout tail.
Use `data.failure` only as Vci's broad failure category. Keep the report
short and factual; do not dump full logs unless requested.
