# Vci

Vci is a minimal, agent-first local CI harness. One binary coordinates
reproducible builds and reports JSON.

Vci runs local commands on macOS. Configuration lives under `~/.vci` in
production and is injected by tests. The source path is the direct path passed
to `build`; hosted Git remotes are not required.

## First commands

```sh
vci setup init
vci setup machine add mac-local
vci setup project add Vci --machine mac-local --command go --arg test --arg ./...
vci machines
vci projects
vci build .
vci check <run-id>
vci abort <run-id>
vci setup reap
```

Stdout is machine-readable JSON. Diagnostics belong on stderr. Project and
machine configuration belongs to the coordinator; the source repository does
not supply a build-machine definition.

The current local result states are `queued`, `staging`, `running`,
`committing`, `succeeded`, `failed`, `lost`, and `aborted`. A command failure is
reported as a job failure with its exit code and preserved log paths. An
infrastructure or configuration failure is classified separately. `abort`
durably requests cancellation; the live worker owns TERM/KILL escalation. If a
worker is already dead, Vci will not signal a recycled process and reaping
resolves the run to `lost`.

## Configuration

`~/.vci/config.toml` is created by `setup init`. Tests set `VCI_ROOT` to an
isolated temporary root. The first configuration describes a local machine and
an explicit command vector:

```toml
schema_version = 1

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
```

Run workspaces are temporary and deleted after result publication. Source
blobs and manifests are content-addressed. Logs and byte-based source retention are bounded or reapable.
`./scripts/self-check.sh` exercises the complete local path without using a
hosted Git remote.

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

The current product is local-only. Future remote work, if approved, will use
the system `ssh` executable and a direct source transfer. It is not implemented.
Docker, VMs, hosted remotes, submodule/LFS handling, and permanent workspaces
remain out of scope.

Vci is MIT licensed. See `LICENSE`.
