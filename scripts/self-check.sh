#!/bin/sh
set -eu
root=$(mktemp -d)
cleanup() { chmod -R u+w "$root" 2>/dev/null || true; rm -rf "$root"; }
trap cleanup EXIT
export VCI_ROOT="$root"
go run ./cmd/vci setup init >/dev/null
go run ./cmd/vci setup machine add mac-local >/dev/null
go run ./cmd/vci setup project add Vci --machine mac-local --command go --arg test --arg ./... >/dev/null
result=$(go run ./cmd/vci build .)
run_id=$(printf '%s\n' "$result" | python3 -c 'import json,sys; x=json.load(sys.stdin); assert x["ok"]; print(x["data"]["run_id"])')
state=staging
for _ in $(seq 1 120); do
  check=$(go run ./cmd/vci check "$run_id")
  state=$(printf '%s\n' "$check" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("state"))')
  [ "$state" = succeeded ] && break
  [ "$state" = failed ] && { printf '%s\n' "$check"; exit 1; }
  sleep 0.1
done
[ "$state" = succeeded ]
printf '%s\n' 'Vci self-build verified.'
