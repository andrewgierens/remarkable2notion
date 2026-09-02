# rm-notion — Project Plan

Send a reMarkable drawing to a Notion page, from the device, as SVG or PDF.

Two deliverables: an on-device xovi extension (`app/`) and a small OAuth pairing
broker (`server/`). CI on every push to `main` builds both, cuts a GitHub
Release with the device packages attached, and pushes the broker image to GHCR.

---

## 1. Repo layout

```
rm-notion/
├── app/
│   ├── daemon/                 # Go — notion-bridge, static binary, runs on device
│   │   ├── cmd/notion-bridge/
│   │   └── internal/
│   │       ├── notion/         # API client: search, file_uploads, block append
│   │       ├── rm/             # .rm v6 parser
│   │       ├── render/         # scene → SVG, scene → PDF
│   │       ├── pair/           # QR + broker polling
│   │       └── socket/         # unix socket JSON-RPC surface
│   ├── qmd/                    # QML patches (menu entry, picker, settings page)
│   │   ├── send-to-notion.qmd
│   │   └── scripts/validate.sh
│   └── package/
│       └── build.sh            # assembles the installable tarball per device
├── server/                     # JavaScript — pairing broker
│   ├── src/broker.js           # the whole broker: routes, codes, OAuth
│   ├── src/node.js             # server / container entry
│   ├── src/worker.js           # Cloudflare Workers entry + Durable Objects
│   ├── test/
│   └── Dockerfile
└── .github/workflows/release.yml
```

The daemon is Go; the broker is JavaScript, one implementation for every
deployment. They share nothing but the pairing wire format — duplicate the
three structs rather than introducing a shared module.

---

## 2. Components

### 2.1 `app/daemon` — notion-bridge

Static Go binary (`CGO_ENABLED=0`), installed to `~/.vellum/bin/notion-bridge`.
Owns everything that needs network, TLS, or file parsing. Listens on
`/run/notion-bridge.sock`.

Responsibilities:

- Read source files from `/home/root/.local/share/remarkable/xochitl/<uuid>/`
- Parse `.rm` v6 into a scene tree (layers, strokes, pen colour/width, text blocks)
- Render to SVG or PDF
- Talk to the Notion API (search, file upload, block append)
- Store and use the access token
- Drive the pairing flow

**Socket API** (newline-delimited JSON, one request per connection):

| Method | Request | Response |
|---|---|---|
| `status` | `{}` | `{authed: bool, workspace: string}` |
| `pair.start` | `{}` | `{device_code, verification_url, qr_png_path}` |
| `pair.poll` | `{device_code}` | `{state: "pending"\|"ok"\|"expired", workspace?}` |
| `targets.list` | `{query?: string}` | `{pages: [{id, title, icon}], recent: [...]}` |
| `send` | `{doc_uuid, page_range, format: "svg"\|"pdf", target_page_id}` | `{ok, block_url}` |
| `logout` | `{}` | `{ok}` |

Token at `/home/root/.config/notion-bridge/token`, mode 0600. Recent targets
cached alongside it as JSON, capped at 10.

### 2.2 `app/qmd` — the UI patch

Depends on `xovi`, `qt-resource-rebuilder`, `qt-command-executor`, and
`xovi-settings-main`.

- Adds **Send to Notion** to the document More menu
- Format toggle (SVG / PDF) and page-range in the send sheet
- Target picker: list from `targets.list`, recents pinned at top, search box
- Settings page: pair / unpair, shows connected workspace, broker URL override,
  paste-a-token fallback

All of it shells out to `notion-bridge` via qt-command-executor. **Resolve this
before writing any QML** (see Risks): if qt-command-executor is fire-and-forget
rather than returning stdout, the picker moves into an AppLoad window and the
QMD patch shrinks to a launcher.

### 2.3 `server` — pairing broker

Go, single binary, container. Holds the OAuth client secret; the device never
sees it. Stateless apart from a TTL store (KV, Redis, or an in-process map with
persistence — pairings live ~10 minutes).

| Route | Purpose |
|---|---|
| `POST /pair` | Mint `device_code` + `verification_url`, TTL 600s |
| `GET /pair/{code}` | Device polls; returns `pending`, or the token once |
| `GET /go/{code}` | Human-facing; 302 to Notion's authorize endpoint with `state={code}` |
| `GET /callback` | Notion redirect target; exchanges code, stores against `state` |
| `GET /healthz` | Liveness |

The token is handed to the device **once** and deleted from the broker
immediately. The broker never retains user tokens at rest.

Deploy behind a stable HTTPS hostname — Notion rejects redirect URIs that are
public IPs, contain wildcards, or contain URL fragments.

---

## 3. Auth flow

```
device                broker                 notion            phone
  │ POST /pair          │                       │                │
  │────────────────────>│  mint device_code     │                │
  │<────────────────────│  {code, url}          │                │
  │ render QR           │                       │                │
  │                     │                       │   scan ────────│
  │                     │<──── GET /go/{code} ──────────────────  │
  │                     │  302 /v1/oauth/authorize?state={code}   │
  │                     │                       │<───────────────│
  │                     │                       │  consent +     │
  │                     │                       │  page scoping  │
  │                     │<─ GET /callback?code=&state= ──────────│
  │                     │  POST /v1/oauth/token │                │
  │                     │  Basic b64(id:secret) │                │
  │                     │<──── access_token ────│                │
  │ GET /pair/{code}    │                       │                │
  │────────────────────>│                       │                │
  │<─── access_token ───│  (then deleted)       │                │
```

Notes that shape the design:

- Notion has **no device authorization grant**, hence the broker. This is a
  hand-rolled equivalent of RFC 8628.
- Token exchange is `POST /v1/oauth/token` with HTTP Basic
  `base64(client_id:client_secret)`.
- Access tokens **do not expire on a schedule** and there is **no refresh
  token** — valid until the user revokes the connection in Notion settings.
  Pair once, never re-auth. Handle 401 by dropping the token and prompting to
  re-pair.
- Page scoping happens on Notion's own consent screen. Don't build a permission
  UI.
- QR: generate PNG in the daemon, display in a QML `Image`, force a full e-ink
  refresh so it scans first try.

---

## 4. Export pipeline

Source: `/home/root/.local/share/remarkable/xochitl/<uuid>/` — `.content`,
`.metadata`, and one `.rm` per page.

**Parser.** Existing Go line parsers are all 2020–21 and target v3/v5. Port the
v6 block structure from `ricklupton/rmscene` (Python, the reference
implementation). Scope: scene tree, layers, stroke points with pen type/colour/
width, text blocks. Skip anything not needed to render.

**SVG.** One `<g>` per layer, strokes as `<path>` with cubic segments, stroke
colour and width from the scene items.

**PDF.** No library. SVG paths and PDF content streams are the same primitives
(`m`, `c`, `l`, `S`) — emit directly. Keeps the binary small and avoids a
dependency that may not cross-compile cleanly.

**Upload.** `POST /v1/file_uploads` (`mode: single_part`, filename,
content_type) → `POST /v1/file_uploads/{id}/send` as multipart with the form
field named exactly `file` → attach the returned `file_upload_id` as a file
block on the target page. Header `Notion-Version: 2026-03-11`. Single-part caps
at 20 MB, which page renders will not approach.

---

## 5. Milestones

| # | Deliverable | Acceptance | Est. |
|---|---|---|---|
| **M0** | Repo skeleton, both modules, CI green | Push to main produces a release with two empty-ish device tarballs and a broker image on GHCR | 0.5d |
| **M1** | Network spike | Hardcoded token + hardcoded page ID + raw `.rmdoc` uploaded and visible in Notion, run over SSH on the device | 0.5d |
| **M2** | Daemon + trigger | `notion-bridge` runs as a service, socket API answers `status`/`send`; **Send to Notion** appears in the More menu and sends to a configured default page | 1.5d |
| **M3** | Target picker | `targets.list` backed by `POST /v1/search` filtered to pages; QML list with recents pinned and a search box | 1d |
| **M4** | Real export | `.rm` v6 → SVG and → PDF, format toggle wired, page-range selection | 3d |
| **M5** | Pairing | Broker deployed, QR renders on device, full pair → send round trip with no config file editing | 1.5d |

Ship order matters. **M1 before anything else** — if TLS from the device or the
upload flow fights us, we want to know before there's a scene parser to throw
away. M4 is the long pole and is fully independent of M3/M5, so it parallelises
if there's a second pair of hands.

Post-M5 backlog: lasso-selection send, whole-notebook send, append-to-database
rather than page, `.rmdoc` as a third format option, Vellum index submission.

---

## 6. CI/CD

Same shape as our standard release pipeline, with one deliberate change:
artefacts are built **before** the release job so they can be attached to it,
rather than the release being cut first. Version is computed once in its own job
and consumed everywhere. See `.github/workflows/release.yml` for the
implementation.

`app/package/build.sh` takes `<device> <version>` and emits
`dist/rm-notion-<device>-<version>.tar.gz` containing the daemon binary, the
`.qmd` files, and an `install.sh` that drops them into `~/.vellum/` and
registers the extension. Vellum's own index is apk-based
(`vellum-dev/apk-tools`) — **confirm the APKBUILD spec against
`vellum-dev/vellum-cli` before targeting it.** GitHub Releases is the primary
distribution channel either way; index submission is post-M5.

Broker secrets (`NOTION_CLIENT_ID`, `NOTION_CLIENT_SECRET`, `REDIRECT_URI`) are
runtime env on the deployment host, not build-time. Nothing secret enters the
image or the device package.

---

## 7. Risks and open questions

1. **qt-command-executor return semantics.** Does it return stdout
   synchronously into QML, or is it fire-and-forget? This decides whether the
   picker is a native QML dialog or an AppLoad window. Answer it in the first
   hour of M0 — it changes the shape of `app/qmd` entirely.
2. **`.rm` v6 parser scope creep.** rmscene handles a lot we don't need. Cut
   hard to render-only. Budget for undocumented pen types appearing in real
   notebooks and degrade gracefully rather than failing the send.
3. **Firmware coupling.** QMD patches target specific xochitl QML and break on
   OS updates. Pin a tested firmware range in the README and expect to chase it.
4. **Broker as a single point of failure.** If it's down, nobody can pair —
   though already-paired devices keep working indefinitely, since tokens don't
   expire. Paste-a-token fallback is not optional; ship it in M5.
5. **Text boxes.** Typed text in notebooks is a separate scene item from
   strokes, and existing converters have known bugs positioning strokes when
   text boxes are present. Test with a mixed page early in M4.
6. **Device arch matrix.** Confirm which device is the primary target before
   M0; it determines which matrix leg gets tested on real hardware.
