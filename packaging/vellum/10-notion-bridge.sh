#!/bin/sh
# rm-notion — install and start the notion-bridge daemon.
#
# Installed to /home/root/xovi/scripts/post-start/ so it runs every time xovi
# is started (triple-tap the power button after a boot). This is needed
# because /etc is restored on every boot on this device — xovi itself mounts
# its systemd drop-ins on a tmpfs for the same reason — so a unit written to
# /etc/systemd/system does not survive a reboot.
set -e

UNIT_SRC=/home/root/.vellum/share/rm-notion/notion-bridge.service
UNIT_DST=/etc/systemd/system/notion-bridge.service

[ -f "$UNIT_SRC" ] || exit 0

mkdir -p /etc/systemd/system
cp "$UNIT_SRC" "$UNIT_DST"
systemctl daemon-reload
systemctl restart notion-bridge
