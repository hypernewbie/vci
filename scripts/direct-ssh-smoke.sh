#!/bin/sh
# Manual proof of the ordinary source-copy plus public Vci composition.
# Requires an operator-configured SSH target and a remote `vci` in PATH.
set -eu

target=${VCI_SSH_TARGET:?set VCI_SSH_TARGET to an existing SSH alias}
source=${1:-.}
source=$(cd "$source" && pwd)
repo_name=$(basename "$source")
remote_root="/tmp/vci-direct-smoke-$$"
remote_source="$remote_root/$repo_name"
cleanup() { ssh "$target" "rm -rf $remote_root" >/dev/null 2>&1 || true; }
trap cleanup EXIT

ssh "$target" "mkdir -p $remote_source $remote_root/state"
# Ordinary tar-over-SSH copy. No Vci command receives source data.
tar -C "$source" -cf - . | ssh "$target" "tar -C $remote_source -xf -"

# Configure the ordinary public Vci CLI in the fixture-owned remote root.
ssh "$target" "VCI_ROOT=$remote_root/state vci setup init" >/dev/null
ssh "$target" "VCI_ROOT=$remote_root/state vci setup machine add mac-local" >/dev/null
ssh "$target" "VCI_ROOT=$remote_root/state vci setup project add $repo_name --machine mac-local --command go --arg test --arg ./..." >/dev/null

# The remote Vci process runs its ordinary public build from the copied source.
response=$(ssh "$target" "cd $remote_source && VCI_ROOT=$remote_root/state vci build .")
printf '%s\n' "$response"
run_id=$(printf '%s\n' "$response" | python3 -c 'import json,sys; x=json.load(sys.stdin); assert x["ok"]; print(x["data"]["run_id"])')
for _ in $(seq 1 120); do
	check=$(ssh "$target" "VCI_ROOT=$remote_root/state vci check $run_id")
	state=$(printf '%s\n' "$check" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("state"))')
	[ "$state" = succeeded ] && { printf '%s\n' "$check"; exit 0; }
	[ "$state" = failed ] && { printf '%s\n' "$check"; exit 1; }
	sleep 0.1
done
printf '%s\n' "$check"
exit 1
