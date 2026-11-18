#!/usr/bin/env bash
# install.sh: the step of the setup action that installs lmget. It takes the
# release archive for this runner, checks it against checksums.txt and its
# build provenance (gh attestation verify, which fails closed), and puts
# lmget on the PATH. Inputs arrive as environment variables, never pasted
# into this script.
set -euo pipefail

repo=fabriziosalmi/lmget
die() { echo "::error::lmget setup: $*" >&2; exit 1; }

case "${RUNNER_OS:-}" in
	Linux) os=linux ;;
	macOS) os=darwin ;;
	Windows) os=windows ;;
	*) die "no release for runner OS ${RUNNER_OS:-unknown}" ;;
esac
case "${RUNNER_ARCH:-}" in
	X64) arch=amd64 ;;
	ARM64) arch=arm64 ;;
	*) die "no release for runner architecture ${RUNNER_ARCH:-unknown}" ;;
esac

tag=${LMGET_SETUP_VERSION:-latest}
if [ "$tag" = latest ]; then
	tag=$(gh api "repos/$repo/releases/latest" --jq .tag_name) || die "cannot find the latest release"
fi
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "not a release tag: $tag (expected vX.Y.Z or latest)"
ver=${tag#v}
archive="lmget_${ver}_${os}_${arch}.tar.gz"
tmp=$RUNNER_TEMP
if [ "$os" = windows ] && command -v cygpath >/dev/null 2>&1; then
	tmp=$(cygpath -u "$tmp") # D:\a\_temp: GNU tar would take "D:" for a remote host
fi
dir="$tmp/lmget-$ver-$os-$arch"
rm -rf "$dir"
mkdir -p "$dir"

gh release download "$tag" --repo "$repo" --pattern "$archive" --pattern checksums.txt --dir "$dir" ||
	die "cannot download $archive from $tag"
want=$(awk -v f="$archive" '$2 == f { print $1 }' "$dir/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
# from stdin: with a path that holds a backslash (any Windows runner),
# sha256sum escapes its output line and the hash starts with "\"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum <"$dir/$archive" | awk '{ print $1 }')
else
	got=$(shasum -a 256 <"$dir/$archive" | awk '{ print $1 }')
fi
[ "$got" = "$want" ] || die "sha256 mismatch for $archive: expected $want, got $got"
echo "lmget setup: $archive matches checksums.txt"

gh attestation verify "$dir/$archive" --repo "$repo" \
	--signer-workflow "$repo/.github/workflows/release.yml" --source-ref "refs/tags/$tag" >/dev/null ||
	die "the build provenance of $archive does not verify: not installing"
echo "lmget setup: build provenance verified ($repo, release.yml, $tag)"

tar -xzf "$dir/$archive" -C "$dir"
bin="$dir/lmget_${ver}_${os}_${arch}"
exe=lmget
[ "$os" = windows ] && exe=lmget.exe
got=$("$bin/$exe" --version)
[ "$got" = "lmget $tag" ] || die "the binary reports \"$got\", not lmget $tag"

store="$HOME/.lmget"
if [ "$os" = windows ] && command -v cygpath >/dev/null 2>&1; then
	bin=$(cygpath -w "$bin")
	store=$(cygpath -w "$store")
fi
echo "$bin" >>"$GITHUB_PATH"
echo "LMGET_HOME=$store" >>"$GITHUB_ENV"
{
	echo "version=$tag"
	echo "store=$store"
} >>"$GITHUB_OUTPUT"
echo "lmget setup: $got installed, store $store"
