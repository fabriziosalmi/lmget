#!/bin/sh
# release-build.sh VERSION OUTDIR
#
# Builds the release archives the same way on a laptop and in GitHub Actions:
# static binaries (CGO off, -trimpath, no VCS stamp) for four targets,
# tarballs with fixed order, owner and mtime (the commit time), and
# checksums.txt. Two runs on the same commit give identical bytes.
set -eu

version=${1:?usage: release-build.sh VERSION OUTDIR}
out=${2:?usage: release-build.sh VERSION OUTDIR}
case "$version" in
v[0-9]*) ;;
*) echo "version must look like v0.1.0, got $version" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
# the exact Go of go.mod, whatever Go runs this script: a newer local Go
# would build other bytes than the release
GOTOOLCHAIN=go$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$root/go.mod")
export GOTOOLCHAIN
epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct)}
rm -rf "$out"
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name="lmget_${version#v}_${os}_${arch}"
	mkdir -p "$work/$name"
	(cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid= -X github.com/fabriziosalmi/lmget.Version=$version" \
		-o "$work/$name/lmget" ./cmd/lmget)
	cp "$root/LICENSE" "$root/README.md" "$work/$name/"
	python3 -I "$root/scripts/archive.py" "$epoch" "$work/$name" "$out/$name.tar.gz"
done

(cd "$out" && shasum -a 256 ./*.tar.gz | sed 's| \./| |' > checksums.txt)
cat "$out/checksums.txt"
