#!/bin/sh
# lmget installer: https://lmget.org/install.sh
#
#   curl -fsSL https://lmget.org/install.sh | sh
#
# Downloads the latest lmget release for this OS and CPU from GitHub, checks
# the archive against the release's checksums.txt, and, when the GitHub CLI
# (gh) is installed, verifies its build provenance attestation too. Then it
# installs the lmget binary into $LMGET_INSTALL_DIR (default ~/.local/bin).
# Nothing else is written, and nothing runs with sudo.
#
# Set LMGET_VERSION=v0.1.0 to install a given release instead of the latest.
set -eu

repo=fabriziosalmi/lmget
dir=${LMGET_INSTALL_DIR:-$HOME/.local/bin}

say() { printf 'lmget-install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"

case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "no release for $(uname -s): build from source, see https://github.com/$repo" ;;
esac
case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "no release for $(uname -m): build from source, see https://github.com/$repo" ;;
esac

tag=${LMGET_VERSION:-}
if [ -z "$tag" ]; then
	# the latest release page redirects to its tag: no API call, no rate limit
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
		die "cannot reach github.com"
	tag=${url##*/}
fi
case $tag in
	v[0-9]*) ;;
	*) die "unexpected release tag: $tag" ;;
esac

archive="lmget_${tag#v}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$tag"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $archive ($tag)"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
	got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ "$got" = "$want" ] || die "sha256 mismatch for $archive: expected $want, got $got"
say "sha256 matches checksums.txt"

if command -v gh >/dev/null 2>&1; then
	gh attestation verify "$tmp/$archive" --repo "$repo" \
		--signer-workflow "$repo/.github/workflows/release.yml" \
		--source-ref "refs/tags/$tag" >/dev/null 2>&1 ||
		die "the build provenance attestation does not verify: not installing"
	say "build provenance verified: built by release.yml from $tag"
else
	say "gh not found: provenance not checked. To check it: gh attestation verify $archive --repo $repo"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
bin="$tmp/lmget_${tag#v}_${os}_${arch}/lmget"
[ -f "$bin" ] || die "the archive holds no lmget binary"
mkdir -p "$dir"
cp "$bin" "$dir/lmget.new"
chmod 0755 "$dir/lmget.new"
mv -f "$dir/lmget.new" "$dir/lmget"
say "installed $("$dir/lmget" --version) at $dir/lmget"

case :$PATH: in
	*":$dir:"*) ;;
	*) say "add $dir to your PATH, for example: export PATH=\"$dir:\$PATH\"" ;;
esac
