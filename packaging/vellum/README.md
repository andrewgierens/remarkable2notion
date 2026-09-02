# Vellum packaging / local testing via reManager

[reManager](https://github.com/rmitchellscott/reManager) is a desktop front
end for the [Vellum](https://github.com/vellum-dev/vellum) package manager,
and it's the easiest way to test rm-notion on a real device. Two routes,
fastest first.

## Prerequisites (both routes)

In reManager, install from the Vellum index: **xovi**,
**qt-resource-rebuilder**, and **qt-command-executor**. All three are hard
requirements — if qt-command-executor is missing, the UI patch's QML import
fails and xochitl crash-loops until the .qmd is removed from
`/home/root/xovi/exthome/qt-resource-rebuilder/`. Verify with
`ls /home/root/xovi/extensions.d/` (both .so files must be present).

## Route A — sideload the release tarball (fastest)

1. Grab `rm-notion-rm2-*.tar.gz` (armv7: rM1/rM2) or
   `rm-notion-rmpp-*.tar.gz` (aarch64: Paper Pure, Paper Pro, Paper Pro
   Move) from the GitHub release — or build one locally:

   ```sh
   CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
     go -C app/daemon build -o ../../dist/rm2/notion-bridge ./cmd/notion-bridge
   ./app/package/build.sh rm2 v0.0.0-dev
   ```

2. Transfer it to the device with reManager's **file browser** (or scp),
   then in reManager's **terminal**:

   ```sh
   cd /home/root && tar xzf rm-notion-rm2-*.tar.gz
   cd rm-notion-rm2-* && ./install.sh
   systemctl restart xochitl
   ```

The installer uses the same paths as the Vellum package
(`/home/root/.vellum/bin`, `/home/root/xovi/exthome/qt-resource-rebuilder/`),
so a later proper package install upgrades over it cleanly.

## Route B — build the .apk and install with `vellum add`

Needs Docker or Podman on your desktop, and a GitHub release to fetch
(update `pkgver` + `sha512sums` in `rm-notion/VELBUILD` first).

```sh
git clone https://github.com/vellum-dev/vellum
cp -r packaging/vellum/rm-notion vellum/packages/
cd vellum
./scripts/build-package.sh rm-notion armv7      # or aarch64 for Paper Pure / Pro
# copy the dev signing key once:
scp keys/vellum-dev.rsa.pub root@<device>:/home/root/.vellum/etc/apk/keys/
scp dist/armv7/rm-notion-*.apk root@<device>:/home/root/
ssh root@<device> vellum add /home/root/rm-notion-*.apk
```

`vellum del rm-notion` removes it (the pre-deinstall hook stops the service).

## Smoke-testing once installed

From reManager's terminal on the device:

```sh
systemctl status notion-bridge
notion-bridge -call status          # → {"result":{"authed":false,...}}
notion-bridge -call pair.start      # → QR PNG path + verification URL
```

If the daemon answers but nothing shows in xochitl's menu, the qmd anchors
need re-anchoring for your firmware — that's expected on unpinned firmware
(plan risk #3) and only touches `send-to-notion.qmd`.

## Anchoring the UI patch to your firmware

`send-to-notion.qmd` is written in [QMLDiff](https://github.com/asivery/qmldiff)
with the UI inlined in SLOT blocks (firmware-independent) and two `AFFECT`
anchor blocks that must target your firmware's actual QML (xochitl's
component names change between OS versions and are distributed only in
hashed form by community mods).

To resolve the anchors:

1. On the device, build the hashtable (required once per OS/extension
   update anyway): run `/home/root/xovi/rebuild_hashtable`. It restarts
   xochitl once and writes
   `/home/root/xovi/exthome/qt-resource-rebuilder/hashtab`.
2. That hashtab maps QMLDiff hashes ↔ your firmware's real identifiers.
   With the `qmldiff` CLI (`cargo build --release` in the qmldiff repo):
   - `qmldiff dump-hashtab <hashtab>` lists the real names;
   - `qmldiff hash-diffs <hashtab> <some-community-diff.qmd> -r` unhashes
     an existing mod (e.g. from rm-hacks-qmd or xovi-qmd-extensions for
     your OS version) to reveal the menu components it anchors to.
3. Rewrite the two `AFFECT` blocks in `send-to-notion.qmd` against those
   names, drop the file into
   `/home/root/xovi/exthome/qt-resource-rebuilder/`, and re-run
   `/home/root/xovi/start`. Iterate until the entry shows up —
   qt-resource-rebuilder logs parse errors to xochitl's journal
   (`journalctl -u xochitl`).

   Note `qmldiff hash-diffs` rewrites the file **in place**: hash a copy in
   `app/qmd/hashed/<osver>/`, never the plain source.

## Boot behaviour: /etc does not persist

`/etc` is restored on every boot on these devices, which is why xovi mounts
its systemd drop-ins on a tmpfs and has to be re-started after each boot
(triple-tap the power button, or run `/home/root/xovi/start`). A unit
installed to `/etc/systemd/system` disappears with it.

The package therefore keeps a persistent copy of the unit at
`/home/root/.vellum/share/rm-notion/notion-bridge.service` and installs
`10-notion-bridge.sh` into `/home/root/xovi/scripts/post-start/`, which
reinstalls and starts it every time xovi comes up. `systemctl restart
xochitl` reboots this device, so use `/home/root/xovi/start` to iterate —
it restarts xochitl and re-runs the post-start hooks.

The diff itself is validated in CI against the mock QML tree in
`app/qmd/testdata/` (structure only, not firmware names).
