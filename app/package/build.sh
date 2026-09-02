#!/usr/bin/env bash
# Assemble the installable device tarball.
#
# Usage: build.sh <device> <version>
#   device   rm2 | rmpp
#   version  e.g. v0.1.42
#
# Expects the daemon binary at dist/<device>/notion-bridge (built by CI or by
# hand) and emits dist/rm-notion-<device>-<version>.tar.gz containing the
# binary, the release .qmd, the systemd unit, the xovi post-start hook, and
# install.sh.
set -euo pipefail

device="${1:?usage: build.sh <device> <version>}"
version="${2:?usage: build.sh <device> <version>}"

root="$(cd "$(dirname "$0")/../.." && pwd)"
bin="$root/dist/$device/notion-bridge"

if [[ ! -f "$bin" ]]; then
    echo "build.sh: missing daemon binary at $bin — build app/daemon first" >&2
    exit 1
fi

pkg="rm-notion-$device-$version"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

mkdir -p "$stage/$pkg/bin" "$stage/$pkg/qmd"
install -m 0755 "$bin" "$stage/$pkg/bin/notion-bridge"
# Only the release patch. send-to-notion-debug.qmd is the same overlay with
# tracing added and the same AFFECT anchors, so shipping both would have
# qt-resource-rebuilder apply two conflicting patches to one QML file.
install -m 0644 "$root/app/qmd/send-to-notion.qmd" "$stage/$pkg/qmd/send-to-notion.qmd"
install -m 0644 "$root/app/package/notion-bridge.service" "$stage/$pkg/notion-bridge.service"
# /etc does not survive a reboot on these devices, so the unit is reinstalled
# from an xovi post-start hook. The Vellum package installs this too.
install -m 0755 "$root/packaging/vellum/10-notion-bridge.sh" "$stage/$pkg/10-notion-bridge.sh"
install -m 0755 "$root/app/package/install.sh" "$stage/$pkg/install.sh"
# Vellum packages install the licence alongside the binary, so it has to be
# in the tarball they are built from.
install -m 0644 "$root/LICENSE" "$stage/$pkg/LICENSE"

tar -czf "$root/dist/$pkg.tar.gz" -C "$stage" "$pkg"
echo "build.sh: wrote dist/$pkg.tar.gz"
