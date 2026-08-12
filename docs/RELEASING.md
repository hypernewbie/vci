# Vci versioning and releases

## Application version

Vci uses Semantic Versioning for the application. The canonical version is
`internal/version/VERSION`. It contains a strict `MAJOR.MINOR.PATCH` value with
no `v` prefix. The first planned release is `0.1.0`; the Git tag is `v0.1.0`.

The file names the next intended release. After publishing a release, update
it in the next development commit. Do not use the file as a JSON protocol or
configuration schema version.

Before `1.0.0`:

| Version change | Use |
| --- | --- |
| Patch (`0.1.1`) | A compatible fix, security correction, or documentation correction |
| Minor (`0.2.0`) | A feature or any documented breaking change |
| Major (`1.0.0`) | The first stable public contract; later breaking changes use a major version |

A pre-1.0 minor release may change a public CLI, JSON, configuration, persisted
state, or distributed-operation contract. Every such change needs migration
notes and upgrade-order notes in the changelog.

## Querying identity

`vci version` is local and does not read `VCI_ROOT`, configuration, or state. It
works from any directory, including a machine with no initialized Vci root.

`vci version --coordinator` reports the configured coordinator's identity. With
`orchestrator = "self"`, it returns the local identity without SSH. With a
remote orchestrator, it invokes the remote `vci version` command and preserves
the remote response. It does not forward `--coordinator`, compare versions, or
reject a version difference. An old coordinator that does not know the command
returns its normal `unknown_command` error.

The data includes the application version, commit, build date, Go version, OS,
architecture, and named schema versions. Release linker metadata wins over Go
build information; Go module/VCS metadata wins over the embedded development
fallback. A local build normally reports the embedded version with a `-dev`
suffix, such as `0.1.0-dev`; `go install ...@v0.1.0` reports the tagged module
version.

Application versions are diagnostic identity. Existing command envelopes keep
their envelope schema and do not gain an application-version compatibility
check.

## Compatibility boundaries

The current schema values are:

- JSON envelope: `1`
- Configuration: current `2`, accepts `1` and `2`
- Run record: `1`
- Execution record: `1`
- Scheduler claim: `1`

These values are independent of application SemVer. A schema change requires
its own compatibility and migration decision; changing `VERSION` alone does
not authorize a state or protocol migration.

Coordinator and worker binaries initially must run the same Vci release. The
coordinator invokes private worker commands, so application SemVer is not a
sufficient mixed-version compatibility check. Upgrade workers and the
coordinator as one deployment. Upgrade the coordinator before client binaries;
clients can use `vci version --coordinator` to inspect the deployed boundary.

## Release procedure

1. Update `internal/version/VERSION` and add the release notes to
   `CHANGELOG.md`.
2. Run the full release gate locally:

   ```sh
   gofmt -l cmd/ internal/
   go vet ./...
   go test -p 2 ./... -count=1
   go test -race -p 2 ./...
   ./scripts/self-check.sh
   ./scripts/detach-check.sh
   git diff --check
   ```

3. Commit the release and create a tag whose value exactly matches
   `VERSION`:

   ```sh
   git tag -a v0.1.0 -m 'Vci v0.1.0'
   git push origin main v0.1.0
   ```

4. The tag workflow runs the same CI workflow before building. It rejects a
   dirty checkout, a tag that does not match `VERSION`, or a tag that does not
   point at `HEAD`.
5. To reproduce the release archives locally from the tagged checkout, run:

   ```sh
   ./scripts/release.sh v0.1.0 dist
   ```

   The script builds deterministic, CGO-disabled archives with `-trimpath` for
   Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. It embeds the
   version, full commit, and tagged commit time. It writes `SHA256SUMS` and a
   machine-readable `manifest.json`. The output directory must not already
   exist; the script never recursively removes a caller-supplied path.
6. Verify an archive before installation. Run its binary with `vci version` and
   confirm the reported version and commit. Verify the archive against
   `SHA256SUMS`.

The Windows archive is a client binary. Windows coordinator state operations
are not supported; a Windows machine can still be declared as a remote worker
when its operator-declared shell and runtime configuration support that path.
