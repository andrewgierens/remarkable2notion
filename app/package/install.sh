#!/bin/sh
# On-device installer, run from inside the unpacked tarball over SSH (or via
# reManager's file browser + terminal):
#   ./install.sh
#
# Layout follows Vellum/xovi conventions so a later `vellum add` of the real
# package upgrades cleanly over a sideload:
#   binary   -> /home/root/.vellum/bin/notion-bridge
#   qmd+qml  -> /home/root/xovi/exthome/qt-resource-rebuilder/
#   service  -> /etc/systemd/system/notion-bridge.service
#   hook     -> /home/root/xovi/scripts/post-start/10-notion-bridge.sh
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
vellum="$HOME/.vellum"
exthome="$HOME/xovi/exthome/qt-resource-rebuilder"

missing=""
[ -d "$HOME/xovi" ] || missing="xovi"
[ -f "$HOME/xovi/extensions.d/qt-resource-rebuilder.so" ] || missing="$missing qt-resource-rebuilder"
[ -f "$HOME/xovi/extensions.d/qt-command-executor.so" ] || missing="$missing qt-command-executor"
if [ -n "$missing" ]; then
    echo "ERROR: missing required extension(s):$missing" >&2
    echo "Install them via reManager/Vellum first. Without qt-command-executor the" >&2
    echo "UI patch makes xochitl crash-loop (its QML import fails)." >&2
    exit 1
fi

mkdir -p "$vellum/bin" "$exthome" "$vellum/share/rm-notion"
cp "$here/bin/notion-bridge" "$vellum/bin/notion-bridge"
chmod 0755 "$vellum/bin/notion-bridge"
cp "$here/qmd/"*.qmd "$exthome/"
# An older sideload may have dropped the debug patch here; two patches against
# the same anchors put xochitl into a crash loop.
rm -f "$exthome/send-to-notion-debug.qmd"

# /etc is restored on every boot on these devices, so keep a persistent copy of
# the unit and reinstall it from an xovi post-start hook (which also ties the
# daemon's lifetime to xovi's). Without this the daemon is gone after a reboot.
cp "$here/notion-bridge.service" "$vellum/share/rm-notion/notion-bridge.service"
if [ -d "$HOME/xovi/scripts/post-start" ] || mkdir -p "$HOME/xovi/scripts/post-start"; then
    cp "$here/10-notion-bridge.sh" "$HOME/xovi/scripts/post-start/10-notion-bridge.sh"
    chmod 0755 "$HOME/xovi/scripts/post-start/10-notion-bridge.sh"
fi

if [ -d /etc/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    cp "$here/notion-bridge.service" /etc/systemd/system/notion-bridge.service
    systemctl daemon-reload
    systemctl enable --now notion-bridge.service
    echo "notion-bridge service installed and started."
else
    echo "systemd not found — start the daemon manually: $vellum/bin/notion-bridge &"
fi

echo "rm-notion installed. Restart xochitl (systemctl restart xochitl) to load the extension."
