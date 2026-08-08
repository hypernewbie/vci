# Vci

Vci lets coding agents run CI on machines you control. An agent can test the current working tree before it pushes a branch or asks for review.

Build commands and machines stay on the coordinator, outside the repository. A checkout cannot change its own test environment.

Ask your agent:

> Run Vci on this repository. Tell me where it ran, whether it passed, and why it failed.

The agent turns Vci's run IDs, logs, and artifacts into a plain-language result.

## Why use Vci

- Test staged, unstaged, and untracked work before a push.
- Run the same approved command on a Mac, in Docker, in a Tart VM, or on an SSH host.
- Keep the selected machine, exit code, logs, and artifacts with each failure.
- Keep build machines clean with bounded caches and temporary workspaces.

Vci uses one binary and ordinary system tools. It has no web service, dashboard, queue, relay, or custom protocol.

## Start on one Mac

Install Vci and add this repository:

```sh
go install github.com/hypernewbie/vci/cmd/vci@main

vci setup init
vci setup machine add local
vci setup project add vci \
    --machine local \
    --command go \
    --arg test \
    --arg ./...
```

The project name must match the repository directory name. Vci checks an exact match first, then ignores case.

The agent can now run the job:

```sh
vci build .
vci check <run-id>
vci logs <run-id> --tail 100
```

`build` returns a run ID before the worker finishes. The agent can reconnect later and read the final result. `vci abort <run-id>` stops a live run.

## Choose where jobs run

The local setup keeps every Vci role on one Mac. Add another target for jobs that need it:

```sh
vci setup machine add linux --runtime docker --image <image>
vci setup machine add mac-vm --runtime vm --snapshot <snapshot>
vci setup machine add remote --host builder --runtime docker --image <image>
```

Docker gets a read-only workspace with no network, while Tart gets a read-write workspace. Both use two CPUs and 4 GiB of memory.

Each machine gets one slot by default. Use `--capacity` for parallel jobs. When all eligible slots are busy, Vci returns the retryable `machine_unavailable` error immediately.

## Keep source under control

Vci captures the current working tree, including changes beyond `HEAD`. It includes tracked changes, staged changes, untracked non-ignored files, executable bits, symlinks, and initialized submodule files.

Ignored files, deleted tracked files, `.gitmodules`, and private Git data stay behind. Vci rejects uninitialized submodules and unresolved Git LFS pointers. It never fetches either one.

Before transfer, Vci copies the selected bytes into a private snapshot and checks its digest. Later edits cannot change an accepted run.

The coordinator reuses snapshots from a bounded cache. Vci protects active entries and removes inactive entries in least-recently-used order.

For a build without a local checkout, Vci can use one configured Git commit. Hosted builds require a full object ID. Vci rejects branches, tags, short hashes, and `HEAD`.

## Read the result

The agent reads durable results from the coordinator:

```sh
vci check <run-id>
vci logs <run-id> [--stderr] [--tail <n>]
vci artifacts ls <run-id>
vci artifacts get <run-id> <path>
```

Log streams have a 4 MiB default limit, and each run can retain 64 MiB of regular-file artifacts. `vci setup reap` removes stale state and applies retention limits.

When a worker disappears, Vci marks the run `lost`. Partial results never appear as final results.

Most commands write structured JSON to stdout and diagnostics to stderr. Log reads and artifact downloads return raw bytes on success.

## Use a remote coordinator

A client needs one SSH destination in `~/.vci/config.toml`:

```toml
schema_version = 1
orchestrator = "builder"
```

`builder` can be an SSH alias or `user@host`. Install `vci` in that host's remote shell path. The client sends source over system SSH and keeps no coordinator state.

## Development

```sh
go build -o ./vci ./cmd/vci
go test ./...
go test -race ./...
go vet ./...
./scripts/self-check.sh
```

Vci is available under the MIT License. See [`LICENSE`](LICENSE).
