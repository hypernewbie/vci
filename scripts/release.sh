#!/bin/sh
set -eu

usage() {
	printf '%s\n' "Usage: scripts/release.sh vMAJOR.MINOR.PATCH [new-output-dir]" >&2
	exit 2
}

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
tag=$1
requested_out=${2:-dist}

semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if ! printf '%s\n' "$tag" | grep -E "$semver_re" >/dev/null; then
	printf '%s\n' "release tag must be strict SemVer with a v prefix: $tag" >&2
	exit 1
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || {
	printf '%s\n' 'release must run inside the Vci Git repository' >&2
	exit 1
}
repo_root=$(CDPATH= cd "$repo_root" && pwd -P)
invocation_dir=$(pwd -P)
case "$requested_out" in
	/*) raw_out=$requested_out ;;
	*) raw_out=$invocation_dir/$requested_out ;;
esac
if [ -e "$raw_out" ] || [ -L "$raw_out" ]; then
	printf '%s\n' "output directory already exists; remove it explicitly or choose a new path: $raw_out" >&2
	exit 1
fi
# Resolve existing symlinks in the parent without creating or deleting any
# caller-owned path. The final output directory must be new.
out=$(python3 - "$raw_out" <<'PY'
from pathlib import Path
import sys
print(Path(sys.argv[1]).expanduser().resolve(strict=False))
PY
)
if [ -e "$out" ] || [ -L "$out" ]; then
	printf '%s\n' "output directory already exists; remove it explicitly or choose a new path: $out" >&2
	exit 1
fi
cd "$repo_root"

version=${tag#v}
if ! version_file=$(python3 - internal/version/VERSION <<'PY'
from pathlib import Path
import sys

data = Path(sys.argv[1]).read_bytes()
if len(data) < 2 or data.count(b"\n") != 1 or not data.endswith(b"\n") or b"\r" in data:
    raise SystemExit(1)
try:
    value = data[:-1].decode("ascii")
except UnicodeDecodeError:
    raise SystemExit(1)
print(value)
PY
); then
	printf '%s\n' 'internal/version/VERSION must contain exactly one ASCII SemVer line' >&2
	exit 1
fi
if [ "$version_file" != "$version" ]; then
	printf '%s\n' "tag $tag does not match internal/version/VERSION ($version_file)" >&2
	exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
	printf '%s\n' 'release requires a clean worktree' >&2
	exit 1
fi
head=$(git rev-parse HEAD)
if ! tag_commit=$(git rev-parse "$tag^{commit}" 2>/dev/null); then
	printf '%s\n' "tag $tag does not exist" >&2
	exit 1
fi
if [ "$head" != "$tag_commit" ]; then
	printf '%s\n' "HEAD $head is not the tagged commit $tag_commit" >&2
	exit 1
fi

source_epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct "$head")}
case "$source_epoch" in
	''|*[!0-9]*)
		printf '%s\n' "SOURCE_DATE_EPOCH must be a non-negative integer: $source_epoch" >&2
		exit 1
		;;
esac
commit_date=$(python3 - "$source_epoch" <<'PY'
import datetime
import sys

print(datetime.datetime.fromtimestamp(int(sys.argv[1]), datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z"))
PY
)
module=github.com/hypernewbie/vci/internal/version
build_root=$(mktemp -d "${TMPDIR:-/tmp}/vci-release.XXXXXX")
cleanup() { rm -rf "$build_root"; }
trap cleanup EXIT HUP INT TERM

package_archive() {
	stage=$1
	archive=$2
	format=$3
	python3 - "$stage" "$archive" "$format" "$source_epoch" <<'PY'
import datetime
import gzip
import io
import os
from pathlib import Path
import sys
import tarfile
import zipfile

stage = Path(sys.argv[1])
out = Path(sys.argv[2])
format_name = sys.argv[3]
epoch = int(sys.argv[4])
files = sorted(path for path in stage.rglob("*") if path.is_file())

if format_name == "tar.gz":
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.GNU_FORMAT) as archive:
        for path in files:
            name = path.relative_to(stage).as_posix()
            stat = path.stat()
            info = tarfile.TarInfo(name)
            info.mode = stat.st_mode & 0o7777
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            info.mtime = epoch
            data = path.read_bytes()
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
    compressed = io.BytesIO()
    with gzip.GzipFile(fileobj=compressed, mode="wb", filename="", mtime=epoch) as stream:
        stream.write(raw.getvalue())
    out.write_bytes(compressed.getvalue())
elif format_name == "zip":
    stamp = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc)
    year = max(1980, stamp.year)
    date_time = (year, stamp.month, stamp.day, stamp.hour, stamp.minute, stamp.second // 2 * 2)
    with zipfile.ZipFile(out, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for path in files:
            name = path.relative_to(stage).as_posix()
            info = zipfile.ZipInfo(name, date_time=date_time)
            info.create_system = 3
            info.external_attr = (path.stat().st_mode & 0o7777) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            info.extra = b""
            info.comment = b""
            archive.writestr(info, path.read_bytes())
else:
    raise SystemExit(f"unsupported archive format: {format_name}")
PY
}

for target in \
	'linux amd64' \
	'linux arm64' \
	'darwin amd64' \
	'darwin arm64' \
	'windows amd64'
do
	set -- $target
	goos=$1
	goarch=$2
	name="vci_${version}_${goos}_${goarch}"
	stage="$build_root/stage-$name"
	binary=vci
	format=tar.gz
	archive="$build_root/$name.tar.gz"
	if [ "$goos" = windows ]; then
		binary=vci.exe
		format=zip
		archive="$build_root/$name.zip"
	fi
	mkdir -p "$stage"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
		-trimpath \
		-ldflags "-buildid= -s -w -X ${module}.releaseVersion=${version} -X ${module}.releaseCommit=${head} -X ${module}.releaseDate=${commit_date}" \
		-o "$stage/$binary" ./cmd/vci
	cp LICENSE README.md CHANGELOG.md internal/version/VERSION "$stage/"
	package_archive "$stage" "$archive" "$format"
	rm -rf "$stage"
done

OUT_DIR=$build_root RELEASE_TAG=$tag RELEASE_VERSION=$version RELEASE_COMMIT=$head RELEASE_DATE=$commit_date python3 - <<'PY'
import hashlib
import json
import os
from pathlib import Path

out = Path(os.environ["OUT_DIR"])
archives = []
for path in sorted(out.iterdir()):
    if not path.is_file() or path.name in {"SHA256SUMS", "manifest.json"}:
        continue
    if path.name.endswith(".tar.gz"):
        stem = path.name[:-len(".tar.gz")]
    elif path.name.endswith(".zip"):
        stem = path.name[:-len(".zip")]
    else:
        continue
    parts = stem.split("_")
    entry = {
        "name": path.name,
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        "size": path.stat().st_size,
        "os": parts[-2],
        "arch": parts[-1],
    }
    archives.append(entry)
manifest = {
    "version": os.environ["RELEASE_VERSION"],
    "tag": os.environ["RELEASE_TAG"],
    "commit": os.environ["RELEASE_COMMIT"],
    "build_date": os.environ["RELEASE_DATE"],
    "artifacts": archives,
}
(out / "manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
lines = []
for path in sorted(out.iterdir()):
    if path.is_file() and path.name not in {"SHA256SUMS", "manifest.json"}:
        lines.append(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}")
(out / "SHA256SUMS").write_text("\n".join(lines) + "\n")
PY

mkdir -p "$(dirname "$out")"
mkdir "$out"
for file in "$build_root"/*; do
	[ -f "$file" ] || continue
	cp "$file" "$out/"
done
printf '%s\n' "Release $tag built in $out"
