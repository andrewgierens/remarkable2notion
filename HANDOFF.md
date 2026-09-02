# rm-notion — device UI handoff

The full path works end to end on the Paper Pure: share menu → overlay →
pairing → page picker → send → the PDF lands on the Notion page.

## Device / environment
- Paper Pure, reMarkable OS **3.27.3.0**, hostname `imx93-tatsu`,
  arch aarch64 (`rmpp`), `<device-ip>`, root. No other device has been
  tried — the Paper Pro included.
- xovi installed with **qt-resource-rebuilder** and **qt-command-executor**
  (both `.so` in `/home/root/xovi/extensions.d/`), plus **xovi-tripletap**.
- Daemon **notion-bridge** runs as a systemd service; the Cloudflare broker
  at `https://rmk2notion.tonytheprwn.dev` is live.
- QMLDiff patch dir on device: `/home/root/xovi/exthome/qt-resource-rebuilder/`.

## Device facts that are easy to get wrong

**`/etc` is restored on every boot.** Anything written to
`/etc/systemd/system` is gone after a reboot — xovi mounts its own drop-ins
on a tmpfs for exactly this reason. So:

- **xovi is off after every boot.** Triple-tap the power button, or run
  `/home/root/xovi/start`, to mount the drop-ins and restart xochitl. Without
  it, xochitl runs stock and no `[qmldiff]` lines appear in the journal.
- **The notion-bridge unit must be reinstalled the same way.** A persistent
  copy lives at `/home/root/.vellum/share/rm-notion/notion-bridge.service`,
  and `/home/root/xovi/scripts/post-start/10-notion-bridge.sh`
  (`packaging/vellum/10-notion-bridge.sh`) copies it back into `/etc`,
  daemon-reloads, and starts it whenever xovi starts.

**`systemctl restart xochitl` reboots this device.** Use
`/home/root/xovi/start` to iterate — it restarts xochitl *and* re-runs the
post-start hooks. Run it detached (`setsid nohup ... &`), since the restart
drops the SSH session.

## Install / iterate loop
```
scp app/qmd/hashed/3.27/send-to-notion.qmd \
    root@<device-ip>:/home/root/xovi/exthome/qt-resource-rebuilder/sendtonotion.qmd
ssh root@<device-ip> 'setsid nohup /home/root/xovi/start >/tmp/xovi-start.log 2>&1 </dev/null &'
# ~40s, then:
ssh root@<device-ip> 'journalctl -u xochitl -b --no-pager | grep -a qmldiff | tail'
```
Only ONE rm-notion qmd should be present at a time — if any diff fails to
apply to a file, qt-resource-rebuilder discards ALL edits to that file.

Before deploying, always run `app/qmd/scripts/validate.sh` with `QMLDIFF=`
pointing at the CLI. It catches diff-level syntax errors (unbalanced braces
and the like) that otherwise show up on device only as
`[qmldiff]: Failed to load file ...: Error while parsing`.

## Hashing
The device **hashtab** is required to (un)hash diffs. Build the `qmldiff` CLI
from `github.com/asivery/qmldiff` (`cargo build --release`). Get a fresh
hashtab with `/home/root/xovi/rebuild_hashtable`, then:

```
qmldiff hash-diffs <hashtab> <file.qmd>     # -r to unhash
```

**`hash-diffs` rewrites the file in place.** Copy the plain source into
`app/qmd/hashed/3.27/` first and hash the copy, or you will destroy the
source of truth.

## Debug build
`app/qmd/send-to-notion-debug.qmd` is **generated** from the plain source by
`python3 app/qmd/scripts/make-debug.py` — regenerate it after every change to
`send-to-notion.qmd`. It is the same patch with `console.warn` traces at
every link (row tapped → signal emitted → handler reached →
rmNotionOpenOverlay → overlay.rmnOpen → each bridge call → targets loaded).
Install the hashed debug file alone and read the trace with:
```
journalctl -u xochitl -b --no-pager | grep -a '\[rmn\]'
```
Also grep for plain QML errors — a thrown exception aborts the rest of a
function silently, and that is what hid two of the three bugs:
```
journalctl -u xochitl -b --no-pager | grep -aiE 'TypeError|ReferenceError|Error:'
```

## Patch structure (`app/qmd/send-to-notion.qmd`, plain source of truth)
Three `AFFECT` blocks, all applying cleanly on 3.27.3.0:
1. `Toolbar.qml` (`FocusScope#root`): adds `signal sendToNotionSelected`.
2. `ShareMenu.qml`: the row goes into the `ColumnLayout` bound to
   `ToolbarTool`'s `foldoutContent` (anchor `?#root > ColumnLayout`,
   `LOCATE AFTER SendByEmailButton`); `onClicked` calls
   `root.toolbar.sendToNotionSelected()`.
3. `DocumentView.qml` (`FocusScope#root`): inserts the overlay (SLOT
   `rmNotionOverlay`) + `rmNotionOpenOverlay()`, and adds
   `onSendToNotionSelected` to `Item#_uiContainer > Toolbar`.

### Two constraints the overlay has to respect
- **No `Window.window`.** It is `undefined` in DocumentView's QML scope, so
  binding `parent` through it left the overlay unparented and invisible. The
  overlay is the last child of `FocusScope#root`, which is already
  full-screen (1404x1872), so filling the default parent is enough.
- **Everything the overlay declares is `rmn`-prefixed.** Generic names
  (`pages`, `mode`, `open`, `close`, …) collide with stock DocumentView
  members once the diff is spliced in; `pages = ...` threw *"left-hand side
  of assignment operator is not an lvalue"* and left the picker empty. Keep
  new properties, functions and ids namespaced.

### qt-command-executor's return shape
`CommandExecutor.executeCommand(program, args)` returns a **JSON string**
`{"stdout": "...", "stderr": "..."}` — not an object with a `.stdout`
property. `rmnCall` unwraps that envelope, then parses the daemon's own JSON
out of `stdout`.

## Accounts and what Notion will share

Several Notion accounts can be connected at once (personal and work, say).
The store keeps them in `accounts.json`; a device paired before this is
migrated from the old `token`/`workspace` files on first read, so it stays
paired. The API:

- `status` → `{authed, workspace, accounts:[{id, workspace}]}`; `workspace`
  is only filled in when exactly one account is connected.
- `targets.list` → `{accounts:[{account_id, workspace, pages, error}], recent}`.
  One group per account, so the picker can show pages under their workspace.
  A single revoked account carries its `error` in its group instead of
  failing the whole call.
- `targets.refresh` re-reads each connection's workspace name from Notion
  (it may have been renamed since pairing) and then returns the same payload
  as `targets.list`. This is the picker's Refresh link.
- `send` takes an `account_id`; it may be omitted only when one account is
  connected. The picker fills it in from the row's group. It also takes
  `attach_as` — `"embed"` (the default) renders the file on the page as a
  pdf or image block, `"file"` adds a download row.
- `logout` takes an optional `account_id` — bare, it still drops everything.

### Replacing a previous send (removed)
Re-sending used to be able to update the blocks a previous send created, so
a notebook did not pile up copies. It was removed: a notebook's page count
changes as it is edited, so the SVG path produced a different number of
blocks each time and the in-place path almost never applied. Reviving it
needs a target that survives editing — a single container block per
notebook, say — rather than a per-page block list. Every send appends today.

## Terminal test of the backend (bypasses the UI)
```
notion-bridge -call status
notion-bridge -call pair.start                       # scan the QR URL on a phone
notion-bridge -call pair.poll -params '{"device_code":"<code>"}'
notion-bridge -call targets.list                     # grouped by account
grep -l '"visibleName"' /home/root/.local/share/remarkable/xochitl/*.metadata
notion-bridge -call send -params '{"doc_uuid":"<uuid>","format":"pdf","target_page_id":"<id>","account_id":"<acc>"}'
```
Broker note: needs `NOTION_CLIENT_ID`/`NOTION_CLIENT_SECRET` set on the
Worker and `https://rmk2notion.tonytheprwn.dev/callback` registered in the
Notion public integration, or `/go` returns "Broker not configured".

## Repo map (see README.md / PLAN.md for full detail)
- `app/daemon/` Go daemon (parser, SVG/PDF render, Notion client, socket API,
  `-call` CLI mode). All tests green.
- `app/qmd/` the QMLDiff patch + `testdata/` mocks + `scripts/validate.sh`.
- `app/qmd/hashed/3.27/` device-ready hashed builds.
- `server/` the pairing broker: one JavaScript implementation
  (`src/broker.js`) with two entry points — `src/node.js` for a server or
  container, `src/worker.js` for Cloudflare Workers. `npm test` covers both.
- `packaging/vellum/` VELBUILD, the post-start hook, and anchoring notes.
- `ci/` release.yml and deploy-broker.yml (live in `ci/` because the push
  credential lacks the `workflow` scope; move to `.github/workflows/` to enable).
