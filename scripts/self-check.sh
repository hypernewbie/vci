#!/bin/sh
set -eu
root=$(mktemp -d)
cleanup() { chmod -R u+rwx "$root" 2>/dev/null || true; rm -rf "$root" 2>/dev/null || true; }
trap cleanup EXIT
export VCI_ROOT="$root"
go run ./cmd/vci setup init >/dev/null
go run ./cmd/vci setup machine add mac-local >/dev/null
go run ./cmd/vci setup project add Vci --machine mac-local --command go --arg test --arg ./... >/dev/null
result=$(go run ./cmd/vci build .)
run_id=$(printf '%s\n' "$result" | python3 -c 'import json,sys; x=json.load(sys.stdin); assert x["ok"]; print(x["data"]["run_id"])')
state=staging
# Bounded deadline for the real `go test ./...` job: the job compiles and
# runs the full Go suite, which takes well over 30 seconds on a warm
# cache. 300 seconds is a fixed, finite bound.
deadline=$(( $(date +%s) + 300 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  check=$(go run ./cmd/vci check "$run_id")
  state=$(printf '%s\n' "$check" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("state"))')
  [ "$state" = succeeded ] && break
  [ "$state" = failed ] && {
    printf '%s\n' "$check"
    # The run record names the job's stdout/stderr logs under Vci
    # state; print their tails so a CI failure names the failing
    # test instead of only the envelope.
    printf '%s\n' "$check" | python3 -c '
import json, sys
d = json.load(sys.stdin)["data"]
for key in ("stdout_path", "stderr_path"):
    path = d.get(key)
    if not path:
        continue
    print("--- %s: %s (last 100 lines) ---" % (key, path))
    try:
        with open(path, "rb") as f:
            text = f.read().decode("utf-8", "replace")
        lines = text.splitlines()
        print("\n".join(lines[-100:]))
    except OSError as exc:
        print("(unreadable: %s)" % exc)
'
    exit 1
  }
  sleep 1
done
if [ "$state" != succeeded ]; then
  printf '%s\n' "Vci self-build still $state after the 300s deadline." >&2
  exit 1
fi
# The run record becomes terminal before the detached worker finishes
# releasing its owned state (workspace removal, lease release, scheduler
# claim release). Wait, bounded, for the worker to settle so the EXIT
# trap's `rm -rf` never races a still-writing worker and leaves a
# `.../state: Directory not empty` turd. Each predicate is a Vci-owned
# path the worker removes itself; once all three hold no worker write can
# still land under state/. (Lock files under state/locks and
# state/runs/<run>/run.lock are persistent by design and are not
# predicates; the claim removal is the worker's last write.)
worker_settled() {
  [ ! -e "$root/state/work/$run_id" ] &&
    [ ! -e "$root/state/runs/$run_id/lease.json" ] &&
    [ -z "$(find "$root/state/machine-claims" -name "$run_id.json" 2>/dev/null)" ]
}
deadline=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  worker_settled && break
  sleep 0.1
done
if ! worker_settled; then
  printf '%s\n' "Vci worker state did not settle for $run_id." >&2
  exit 1
fi
printf '%s\n' 'Vci self-build verified.'
