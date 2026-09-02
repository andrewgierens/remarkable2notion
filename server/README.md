# rm-notion pairing broker

Notion has no device authorization grant, so this broker hand-rolls the RFC
8628 equivalent: it holds the OAuth client secret, runs the consent flow in
your phone's browser, and hands the resulting token to the device exactly
once.

**It is only a relay.** It never stores your token (a claimed pairing is
deleted immediately, an unclaimed one within 10 minutes), never reads or
writes anything in Notion, and has no database.

## One implementation, two deployments

`src/broker.js` is the entire broker — routes, codes, pages, the OAuth
exchange — and runs unchanged everywhere. Only two things differ per
environment, and both are arguments to it:

| | Cloudflare Workers | Server (VPS, container, dev) |
|---|---|---|
| Entry point | `src/worker.js` | `src/node.js` |
| Storage | Durable Objects, one per pairing | `src/store-memory.js` |
| Client IP | `CF-Connecting-IP` | socket, or `TRUST_PROXY_HEADER` |

Anything that must hold for both belongs in `src/broker.js`, so a fix cannot
land in one deployment and miss the other.

## Running a server

```sh
PUBLIC_URL=http://localhost:8080 REDIRECT_URI=http://localhost:8080/callback \
NOTION_CLIENT_ID=... NOTION_CLIENT_SECRET=... \
  npm start
```

Point the daemon at it with `-broker http://localhost:8080`. Non-loopback
brokers must be HTTPS — the pairing response carries an access token, and the
server refuses to start otherwise.

| Variable | Required | Purpose |
|---|---|---|
| `PUBLIC_URL` | yes | The broker's own HTTPS base URL; goes into the pairing QR |
| `REDIRECT_URI` | yes | Must be `$PUBLIC_URL/callback` and registered with Notion |
| `NOTION_CLIENT_ID` / `NOTION_CLIENT_SECRET` | yes | Your public integration's OAuth credentials |
| `LISTEN_ADDR` | no | Listen address, default `:8080` |
| `TRUST_PROXY_HEADER` | no | Header carrying the real client IP (e.g. `X-Forwarded-For`). Set only if a proxy you control always overwrites it — rate limiting reads it |

The container image is `ghcr.io/andrewgierens/remarkable2notion-broker`, built from
`Dockerfile`. It has no runtime dependencies.

## Deploying to Cloudflare Workers

The hosted broker is bound to `rmk2notion.tonytheprwn.dev` as a Worker custom
domain (`wrangler.jsonc`); wrangler creates the DNS record and certificate on
first deploy, provided `tonytheprwn.dev` is a zone on the deploying account.
`PUBLIC_URL` / `REDIRECT_URI` are pinned there too, and the daemon's built-in
default broker points at the same hostname.

1. Create the Worker's secrets, using the OAuth credentials of your **public**
   Notion integration:

   ```sh
   npx wrangler secret put NOTION_CLIENT_ID
   npx wrangler secret put NOTION_CLIENT_SECRET
   ```

2. In the integration's OAuth settings, register the redirect URI exactly:
   `https://rmk2notion.tonytheprwn.dev/callback`. Notion rejects public IPs,
   wildcards, and URL fragments.

3. `npm run deploy`. Nothing to configure on the device — the daemon defaults
   to this broker; self-hosters override it with `NOTION_BRIDGE_BROKER=` in
   the daemon's systemd unit.

## Tests

```sh
npm test
```

No toolchain beyond Node 20+: the tests drive `src/broker.js` — the code both
deployments run — through the whole pairing flow against the memory store,
and `src/node.js` over a real socket.

## Routes

```
POST /pair          mint device_code + verification_url (TTL 600s)
GET  /pair/{code}   device polls with the DEVICE code: pending | ok | expired
GET  /go/{code}     phone lands here with the USER code → confirmation page
POST /go/{code}     confirmed → 303 to Notion authorize, state={user code}
GET  /callback      Notion redirect → exchange code, store token
GET  /healthz       liveness
```

Each pairing carries two independent secrets: the device code (~158 bits)
never leaves the device and is the only credential that can claim the token;
the user code (~79 bits) travels through the QR, the phone's browser and
Notion's OAuth state, and can only start a consent flow.
