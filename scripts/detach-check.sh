#!/bin/sh
set -eu
root=$(mktemp -d)
bin=$(mktemp -t vci-bin)
cleanup() { chmod -R u+rwx "$root" 2>/dev/null || true; rm -rf "$root"; rm -f "$bin"; }
trap cleanup EXIT
go build -o "$bin" ./cmd/vci
export VCI_ROOT="$root"
"$bin" setup init >/dev/null
"$bin" setup machine add mac-local >/dev/null
"$bin" setup project add Vci --machine mac-local --command go --arg test --arg ./... >/dev/null
response=$("$bin" build .)
run_id=$(printf '%s\n' "$response" | python3 -c 'import json,sys; x=json.load(sys.stdin); assert x["ok"]; print(x["data"]["run_id"])')
state=staging
for _ in $(seq 1 300); do
	check=$("$bin" check "$run_id")
	state=$(printf '%s\n' "$check" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("state"))')
	[ "$state" = succeeded ] && break
	[ "$state" = failed ] && { printf '%s\n' "$check"; exit 1; }
	sleep 0.1
done
[ "$state" = succeeded ]
printf '%s\n' 'Detached Vci run verified.'
