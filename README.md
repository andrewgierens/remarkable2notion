# remarkable2notion

Send a handwritten page from a reMarkable straight to a Notion page, from
the device, as SVG or PDF. Tap **Send to Notion** in the document menu, pick
a page, done — no cable, no desktop app, no cloud sync round-trip.

Pair once by scanning a QR with your phone. After that the device talks to
Notion directly.

## Device compatibility

| Device | Arch | Package | Status |
|---|---|---|---|
| reMarkable Paper Pure | aarch64 | `rmpp` | **Tested** — the only device this has run on |
| reMarkable Paper Pro | aarch64 | `rmpp` | Untested |
| reMarkable Paper Pro Move | aarch64 | `rmpp` | Untested |
| reMarkable 2 | armv7 | `rm2` | Untested |
| reMarkable 1 | armv7 | `rm2` | Untested |

Everything is built and released for both architectures, but the Paper Pure
is the only device any of it has been run on — on reMarkable OS 3.27.3.0.
Every other row is inference from the architecture, including the Paper Pro
itself. (The `rmpp` package name predates that: it is simply the aarch64
build, and the release assets keep the name for continuity.)

On an untested device expect the daemon to work and the **UI patch to need
re-anchoring**: the menu entry is a QMLDiff patch against xochitl's QML,
whose component names change between firmware versions. The shipped anchors
target reMarkable OS 3.27.x — re-anchoring instructions are in
[`packaging/vellum/README.md`](packaging/vellum/README.md).

Requires [xovi](https://github.com/asivery/xovi) with `qt-resource-rebuilder`
and `qt-command-executor`, all installable via
[reManager](https://github.com/rmitchellscott/reManager)/Vellum.

## How it works

`app/` is an on-device xovi extension: **notion-bridge**, a static Go daemon
that parses `.rm` v6 notebooks, renders them to SVG or PDF, and calls the
Notion API — plus a QML patch adding *Send to Notion* to xochitl's share
menu. The UI talks to the daemon over a unix socket.

`server/` is the pairing broker. Notion has no device authorization grant,
so the broker hand-rolls the RFC 8628 equivalent: it holds the OAuth client
secret, runs the consent flow in your phone's browser, and hands the
resulting token to the device exactly once.

**The broker is only a relay.** It never stores your token (a claimed
pairing is deleted immediately, an unclaimed one within 10 minutes), never
reads or writes anything in Notion, and has no database. Once paired, your
notes go from the device to Notion without passing through it. What you
share with the integration is chosen on Notion's own consent screen.

Built with Go (the daemon, no CGO), QML/QMLDiff for the UI patch, and
JavaScript for the broker — one implementation that runs both as a server and
on Cloudflare Workers + Durable Objects. No PDF library — page content
streams are emitted directly.

## Running it locally

```sh
# Tests for everything
(cd app/daemon && go vet ./... && go test ./...)
(cd server     && npm test)
./app/qmd/scripts/validate.sh
```

**The daemon** runs on a desktop against a directory of notebooks:

```sh
go -C app/daemon run ./cmd/notion-bridge \
    -socket /tmp/notion-bridge.sock \
    -config-dir /tmp/nb-config \
    -data-dir /path/to/xochitl/files

# It is also its own client:
go -C app/daemon run ./cmd/notion-bridge -socket /tmp/notion-bridge.sock -call status
```

Without a broker, connect an [internal
integration](https://www.notion.so/profile/integrations) token directly:

```sh
printf %s "$NOTION_TOKEN" | notion-bridge -config-dir /tmp/nb-config -set-token -
```

**The broker:**

```sh
cd server
PUBLIC_URL=http://localhost:8080 REDIRECT_URI=http://localhost:8080/callback \
NOTION_CLIENT_ID=... NOTION_CLIENT_SECRET=... \
  npm start
```

Point the daemon at it with `-broker http://localhost:8080`. Non-loopback
brokers must be HTTPS — the pairing response carries an access token.

**Building a device package:**

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go -C app/daemon build -o ../../dist/rmpp/notion-bridge ./cmd/notion-bridge
./app/package/build.sh rmpp v0.0.0-dev     # rmpp = aarch64; use rm2 + GOARCH=arm GOARM=7 for rM1/rM2
```

Copy the tarball to the device, unpack, run `./install.sh`, then restart
xochitl. Full walkthrough in
[`packaging/vellum/README.md`](packaging/vellum/README.md).

## Self-hosting the broker

You only need this if you don't want to use the hosted broker at
`rmk2notion.tonytheprwn.dev`. You will need:

1. **A public Notion integration** ([Notion →
   Integrations](https://www.notion.so/profile/integrations) → *Public*),
   which gives you a client ID and client secret.
2. **A stable HTTPS hostname.** Notion rejects redirect URIs that are public
   IPs. Register `https://your.host/callback` as the integration's redirect
   URI, exactly.
3. **One of the two deployments.** `server/src/broker.js` is the whole
   broker and runs unchanged in both; only storage and how the client IP is
   found differ. See [`server/README.md`](server/README.md).
   - **Server / container**: `npm start`, or the image at
     `ghcr.io/andrewgierens/remarkable2notion-broker`. Stateless apart from
     in-memory pairings, so run it behind your own TLS terminator.
   - **Cloudflare Worker**: `npm run deploy`; pairings live in Durable
     Objects.
4. **Point the device at it** — set `NOTION_BRIDGE_BROKER` in the daemon's
   systemd unit (`app/package/notion-bridge.service`).

Server environment, never baked into the image:

| Variable | Required | Purpose |
|---|---|---|
| `PUBLIC_URL` | yes | The broker's own HTTPS base URL; goes into the pairing QR |
| `REDIRECT_URI` | yes | Must be `$PUBLIC_URL/callback` and registered with Notion |
| `NOTION_CLIENT_ID` / `NOTION_CLIENT_SECRET` | yes | Your public integration's OAuth credentials |
| `LISTEN_ADDR` | no | Listen address, default `:8080` |
| `TRUST_PROXY_HEADER` | no | Header carrying the real client IP (e.g. `X-Forwarded-For`). Set only if a proxy you control always overwrites it — rate limiting reads it |

## Known limitations

- Typed text renders as plain text; rich formatting is out of scope.
- The `.rm` parser skips blocks it cannot parse rather than failing the
  send, so undocumented pen types degrade gracefully.
- The wire format is covered by round-trip tests against a synthetic writer;
  a real-device `.rm` corpus should still be added.
