#!/usr/bin/env bash
# Point the VELBUILD at a published release: rewrite pkgver and recompute the
# source checksums from the actual release assets.
#
#   packaging/vellum/update-velbuild.sh v0.1.5
#
# Run this after cutting a release. Without it the VELBUILD drifts — it sat at
# a version that was never released, with placeholder checksums, because
# nothing kept it in step.
set -euo pipefail

version="${1:?usage: update-velbuild.sh <version>   e.g. v0.1.5}"
pkgver="${version#v}"

root="$(cd "$(dirname "$0")/../.." && pwd)"
velbuild="$root/packaging/vellum/rm-notion/VELBUILD"
repo="andrewgierens/remarkable2notion"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Vellum itself fetches these over plain HTTPS, so they must be publicly
# downloadable for the package to build on anyone else's machine. gh is used
# here so this script also works while the repo is private.
for device in rm2 rmpp; do
    asset="rm-notion-$device-$version.tar.gz"
    url="https://github.com/$repo/releases/download/$version/$asset"
    echo "fetching $asset"
    if ! curl -fsSL -o "$tmp/$asset" "$url"; then
        echo "  not public; falling back to gh (vellum will need it public)"
        gh release download "$version" -R "$repo" -p "$asset" -D "$tmp" --clobber ||
            { echo "update-velbuild: no such release asset: $url" >&2; exit 1; }
    fi
done

sum() {
    # sha512sum on Linux, shasum -a 512 on macOS.
    if command -v sha512sum >/dev/null 2>&1; then sha512sum "$1" | cut -d' ' -f1
    else shasum -a 512 "$1" | cut -d' ' -f1; fi
}

rm2sum="$(sum "$tmp/rm-notion-rm2-$version.tar.gz")"
rmppsum="$(sum "$tmp/rm-notion-rmpp-$version.tar.gz")"

python3 - "$velbuild" "$pkgver" "$rm2sum" "$rmppsum" <<'PY'
import re, sys
path, pkgver, rm2, rmpp = sys.argv[1:5]
s = open(path).read()
s = re.sub(r"^pkgver=.*$", f"pkgver={pkgver}", s, count=1, flags=re.M)
s = re.sub(
    r'sha512sums="\n.*?\n"',
    f'sha512sums="\n{rm2}  rm-notion-rm2-v{pkgver}.tar.gz\n'
    f'{rmpp}  rm-notion-rmpp-v{pkgver}.tar.gz\n"',
    s, count=1, flags=re.S)
open(path, "w").write(s)
PY

echo "update-velbuild: VELBUILD now targets $version"
grep -E "^pkgver=" "$velbuild"
