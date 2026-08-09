---
name: vci-orchestrator
description: Configure a Vci coordinator workspace or client orchestrator profile. Use when the user asks to initialize Vci state, configure the coordinator root, add or remove Vci machines or projects, set machine capacity/runtime/host, or point a client root at an already-working SSH coordinator. Do not use for running builds or diagnosing SSH/network access.
---

# Vci coordinator and workspace configuration

Use this skill to configure Vci itself. It changes state outside the source
repository. Treat every mutation as an administrator action.

## Scope

- Configure a coordinator root, its machines, and its projects.
- Configure a client root to point at an already-working SSH destination.
- Use only existing Vci CLI commands and the minimal client `config.toml`
  profile described below.
- Do **not** install Vci, configure SSH, create keys, change host keys, probe
  network access, install Docker or Tart, pull images/snapshots, or modify
  shell startup files. Assume the supplied SSH alias and runtime already work.
- Do not configure hosted Git fallback unless the user explicitly asks.

## Before changing anything

1. Get the exact role, absolute Vci root, names, command, and machine details
   from the user. Do not guess a project command or runtime reference.
2. Inspect the intended root first. Never overwrite an existing `config.toml`
   without explicit user approval.
3. Keep coordinator and client roots separate. A coordinator root owns machine
   and project definitions; a client root only selects its orchestrator.
4. Use an inline `VCI_ROOT=<absolute-path>` for every command when the root is
   not the default `~/.vci`. Do not persist that environment variable yourself.
5. A remote coordinator must be able to run `vci` from a **non-interactive SSH
   session**. Its non-interactive PATH must include the Vci binary, and shell
   startup files must not emit output that corrupts Vci's JSON response. Ask
   the user to establish this prerequisite before client use; do not edit PATH,
   shell startup files, SSH configuration, or host keys yourself.

Vci accepts names matching `[A-Za-z0-9][A-Za-z0-9._-]*`. A project name must
match its checkout directory basename (case-insensitive fallback exists).

## Coordinator root

A coordinator root has `orchestrator = "self"` and owns all Vci state.
Initialize a new, empty root with:

```sh
VCI_ROOT=/absolute/vci-root vci setup init
```

`setup init` creates the root and default coordinator configuration. It is
idempotent for an already valid root; it does not reset it.

Inspect before and after a configuration change:

```sh
VCI_ROOT=/absolute/vci-root vci machines
VCI_ROOT=/absolute/vci-root vci projects
```

These are inventory commands, not configuration mutations. They may perform
scheduler housekeeping for stale claims.

### Add machines

Use a bare local machine unless the user specifies a runtime:

```sh
VCI_ROOT=/absolute/vci-root vci setup machine add mac-local
VCI_ROOT=/absolute/vci-root vci setup machine add linux \
  --runtime docker --image ghcr.io/example/ci:2026-08
VCI_ROOT=/absolute/vci-root vci setup machine add mac-vm \
  --runtime vm --snapshot ghcr.io/example/macos-ci:2026-08
```

Use `--capacity <positive-int>` only when the user requests concurrent slots.
For a machine that executes on an already-configured SSH host, add its supplied
SSH destination without testing or changing SSH:

```sh
VCI_ROOT=/absolute/vci-root vci setup machine add linux-remote \
  --host builder --runtime docker --image ghcr.io/example/ci:2026-08
```

Do not remove a machine until `vci projects` confirms no project still uses it:

```sh
VCI_ROOT=/absolute/vci-root vci setup machine remove <machine>
```

### Add projects

A project names the checkout directory and the approved command Vci runs.
Attach it to one or more existing machines:

```sh
VCI_ROOT=/absolute/vci-root vci setup project add my-repo \
  --machine mac-local \
  --command go --arg test --arg ./...
```

Add repeated `--machine` flags for eligible alternatives and repeated
`--artifact <glob>` flags only for artifacts the user wants retained:

```sh
VCI_ROOT=/absolute/vci-root vci setup project add my-repo \
  --machine linux --machine mac-local \
  --command make --arg test \
  --artifact 'build/*.zip'
```

The public setup CLI does not set project environment variables. Do not edit
TOML by hand to invent a workflow; report that the requested environment
configuration is not exposed by this setup interface.

## Client root

A client root contains no machine or project configuration. It only points to
an existing coordinator SSH alias or `user@host` value.

For a new client root, create exactly this minimal profile after the user gives
both the absolute root and orchestrator value:

```sh
install -d -m 700 /absolute/client-vci-root
cat > /absolute/client-vci-root/config.toml <<'EOF'
schema_version = 1
orchestrator = "builder"
EOF
chmod 600 /absolute/client-vci-root/config.toml
```

Replace `builder` only with the supplied, already-working SSH destination. Do
not add `[machines]`, `[projects]`, retention, or log-limit settings to a
client root. Do not run `vci setup machine ...` or `vci setup project ...`
there; Vci rejects client-side authority changes.

Confirm the profile through Vci using the same root:

```sh
VCI_ROOT=/absolute/client-vci-root vci machines
VCI_ROOT=/absolute/client-vci-root vci projects
```

If this fails, report the structured error. Do not troubleshoot networking or
change SSH configuration.

## Maintenance

Run this only when the user explicitly asks for cleanup:

```sh
VCI_ROOT=/absolute/vci-root vci setup reap
```

It reaps stale Vci-owned state and applies retention; it is not a harmless
read-only check.

## Report

After a configuration task, report:

```text
Role: coordinator | client
Vci root: <absolute path>
Changed: <machine/project/client profile details>
Inventory: <configured machines and projects, or coordinator alias>
Not changed: SSH, network, runtime installation, shell configuration
```

Keep Vci build commands and repository code out of this skill. Use the `vci`
skill to submit, watch, inspect, and abort runs.
