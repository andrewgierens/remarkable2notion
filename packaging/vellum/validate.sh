#!/usr/bin/env bash
# Validate packaging/vellum/rm-notion/VELBUILD against vellum-dev/vellum's own
# linters — the same checks their CI runs on a package PR, so breakage shows up
# here rather than in review.
#
#   packaging/vellum/validate.sh
#
# Three layers, in order of how much they need:
#   1. pkgver/sha512sums consistency  — always runs, no dependencies
#   2. scripts/lint-packages.sh       — always runs, pure shell
#   3. vbuild gen + apkbuild-lint     — needs x86_64 Linux and docker/podman;
#                                       skipped elsewhere unless STRICT=1
#
# vbuild ships as an x86_64-only binary, so layer 3 cannot run on an arm64 Mac.
# CI sets STRICT=1 so a skip there is a failure rather than a silent pass.
set -euo pipefail

vellum_ref="${VELLUM_REF:-main}"
vbuild_version="${VBUILD_VERSION:-0.0.36}"
strict="${STRICT:-0}"

root="$(cd "$(dirname "$0")/../.." && pwd)"
velbuild="$root/packaging/vellum/rm-notion/VELBUILD"
[[ -f "$velbuild" ]] || { echo "validate: no VELBUILD at $velbuild" >&2; exit 1; }

fail() { echo "validate: $*" >&2; exit 1; }

# --- 1. internal consistency -------------------------------------------------
# The VELBUILD drifted once already: pkgver moved without the checksums, which
# only surfaces as a build failure on someone else's machine.
pkgver="$(sed -n 's/^pkgver=//p' "$velbuild" | head -1)"
[[ -n "$pkgver" ]] || fail "pkgver is not set"

for device in rm2 rmpp; do
    asset="rm-notion-$device-v$pkgver.tar.gz"
    grep -q "  $asset\$" "$velbuild" ||
        fail "sha512sums has no entry for $asset (pkgver=$pkgver — run update-velbuild.sh v$pkgver)"
    # source= builds the filename from $pkgver, so match the stem, not the
    # interpolated name.
    grep -q "rm-notion-$device-v\$pkgver.tar.gz" \
        <(sed -n '/^source=/,/^"$/p' "$velbuild") ||
        fail "source= does not reference the $device tarball"
done
echo "validate: pkgver $pkgver consistent with source= and sha512sums"

# --- 2. vellum's linters -----------------------------------------------------
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git clone --depth 1 --branch "$vellum_ref" -q \
    https://github.com/vellum-dev/vellum.git "$work/vellum" ||
    fail "could not clone vellum-dev/vellum@$vellum_ref"
cp -r "$root/packaging/vellum/rm-notion" "$work/vellum/packages/"

# Layer 3 only if vbuild can actually run here: it is x86_64-glibc only, and it
# drives a container runtime of its own to generate the APKBUILD.
lint_args=(rm-notion)
if [[ "$(uname -s)" == "Linux" && "$(uname -m)" == "x86_64" ]] &&
   { command -v docker >/dev/null || command -v podman >/dev/null; }; then
    curl -fsSL -o "$work/vbuild" \
        "https://github.com/Eeems/vbuild/releases/download/$vbuild_version/vbuild-x86_64-glibc"
    chmod +x "$work/vbuild"
    export PATH="$work:$PATH"
    lint_args=(--apkbuild-lint rm-notion)
    echo "validate: vbuild $vbuild_version installed — running apkbuild-lint too"
elif [[ "$strict" == "1" ]]; then
    fail "STRICT=1 but vbuild cannot run here (needs x86_64 Linux + docker/podman)"
else
    echo "validate: skipping apkbuild-lint (needs x86_64 Linux + docker/podman)"
fi

cd "$work/vellum"
./scripts/lint-packages.sh "${lint_args[@]}"
