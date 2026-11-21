#!/bin/sh
# Refresh packaging/aur/PKGBUILD from a published GitHub release.
#
#   ./packaging/aur/update-aur.sh 0.5.6
#
# Reads the release's checksums.txt to get the source tarball hash, rewrites
# pkgver/sha256sums, and (if makepkg is available) regenerates .SRCINFO.
set -eu

version="${1:-}"
repo="davidcrumpton/mcwsd"

if [ -z "$version" ]; then
    echo "usage: $0 <version without leading v, e.g. 0.5.6>" >&2
    exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
pkgbuild="$here/PKGBUILD"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

url="https://github.com/${repo}/releases/download/v${version}/checksums.txt"
echo "fetching ${url}"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmp"
else
    wget -qO "$tmp" "$url"
fi

# The source archive is not in checksums.txt (it is the git tag archive), so
# compute it directly.
archive_url="https://github.com/${repo}/archive/v${version}.tar.gz"
echo "fetching ${archive_url}"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$archive_url" -o "$tmp.archive"
else
    wget -qO "$tmp.archive" "$archive_url"
fi

if command -v sha256sum >/dev/null 2>&1; then
    sum="$(sha256sum "$tmp.archive" | cut -d' ' -f1)"
else
    sum="$(shasum -a 256 "$tmp.archive" | cut -d' ' -f1)"
fi
rm -f "$tmp.archive"

sed -i.bak \
    -e "s/^pkgver=.*/pkgver=${version}/" \
    -e "s/^pkgrel=.*/pkgrel=1/" \
    -e "s/^sha256sums=.*/sha256sums=('${sum}')/" \
    "$pkgbuild"
rm -f "${pkgbuild}.bak"

echo "updated ${pkgbuild} to ${version} (sha256 ${sum})"

if command -v makepkg >/dev/null 2>&1; then
    (cd "$here" && makepkg --printsrcinfo > .SRCINFO)
    echo "regenerated ${here}/.SRCINFO"
else
    echo "makepkg not found; run 'makepkg --printsrcinfo > .SRCINFO' on Arch" >&2
fi
