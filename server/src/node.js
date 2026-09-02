// Server deployment of the shared broker: a plain Node HTTP server for a VPS,
// a container, or local development. Same routes and wire format as the
// Workers deployment — only the storage differs.
//
// Env:
//   PUBLIC_URL           required; the HTTPS base URL that goes into the QR
//   REDIRECT_URI         must be $PUBLIC_URL/callback, registered with Notion
//   NOTION_CLIENT_ID     the public integration's OAuth credentials
//   NOTION_CLIENT_SECRET
//   LISTEN_ADDR          default :8080; "host:port" or ":port"
//   TRUST_PROXY_HEADER   header carrying the real client IP, e.g. X-Forwarded-For

import { createServer } from "node:http";
import { handleRequest } from "./broker.js";
import { MemoryStore } from "./store-memory.js";

function loopback(u) {
  try {
    const h = new URL(u).hostname;
    return h === "localhost" || h === "127.0.0.1" || h === "::1" || h === "[::1]";
  } catch {
    return false;
  }
}

// clientIP finds the caller's address. The proxy header is only consulted when
// the operator names one, because anyone can send it: trusting it by default
// would make the rate limiter free to bypass.
function clientIP(req, trustHeader) {
  if (trustHeader) {
    const v = req.headers[trustHeader.toLowerCase()];
    if (v) {
      // X-Forwarded-For style: the client is the first entry.
      const first = String(v).split(",")[0].trim();
      if (first) return first;
    }
  }
  return req.socket.remoteAddress || "unknown";
}

// toRequest converts Node's IncomingMessage into the standard Request the
// shared broker takes. The broker never reads a request body, so this does
// not forward one.
function toRequest(req, base) {
  const headers = new Headers();
  for (const [k, v] of Object.entries(req.headers)) {
    if (Array.isArray(v)) v.forEach((one) => headers.append(k, one));
    else if (v !== undefined) headers.set(k, v);
  }
  return new Request(new URL(req.url, base), { method: req.method, headers });
}

async function send(res, response) {
  res.statusCode = response.status;
  for (const [k, v] of response.headers) res.setHeader(k, v);
  const body = await response.arrayBuffer();
  res.end(Buffer.from(body));
}

export function createBroker(env, store = new MemoryStore()) {
  const trustHeader = (env.TRUST_PROXY_HEADER || "").trim();
  const server = createServer((req, res) => {
    // The Host header only names the origin used to parse the URL; every
    // outward-facing URL comes from PUBLIC_URL / REDIRECT_URI instead.
    const base = env.PUBLIC_URL || "http://" + (req.headers.host || "localhost");
    handleRequest(toRequest(req, base), env, store, clientIP(req, trustHeader))
      .then((response) => send(res, response))
      .catch((err) => {
        console.log("request failed:", err?.message || err);
        res.statusCode = 500;
        res.end("internal error\n");
      });
  });
  // A client that opens a connection and dawdles must not be able to hold one
  // open indefinitely.
  server.headersTimeout = 10_000;
  server.requestTimeout = 30_000;
  server.keepAliveTimeout = 60_000;
  server.maxHeaderSize = 1 << 16;
  return server;
}

// parseAddr turns LISTEN_ADDR into listen() arguments, accepting Go's ":8080"
// as well as "host:port".
export function parseAddr(addr) {
  const value = (addr || "").trim() || ":8080";
  const idx = value.lastIndexOf(":");
  if (idx === -1) return { port: Number(value) };
  const host = value.slice(0, idx);
  const port = Number(value.slice(idx + 1));
  return host ? { host, port } : { port };
}

function main() {
  const env = process.env;
  const publicURL = (env.PUBLIC_URL || "").replace(/\/+$/, "");
  // The QR the device shows is built from PUBLIC_URL. Falling back to the
  // request's Host header would let a caller choose the host the user's phone
  // is sent to, so refuse to guess.
  if (!publicURL) {
    console.error("PUBLIC_URL is required: it is the HTTPS base URL that goes into the pairing QR");
    process.exit(1);
  }
  if (!publicURL.startsWith("https://") && !loopback(publicURL)) {
    console.error("PUBLIC_URL must be https:// — the pairing URL carries a credential");
    process.exit(1);
  }
  if (!env.NOTION_CLIENT_ID || !env.NOTION_CLIENT_SECRET || !env.REDIRECT_URI) {
    console.log("warning: NOTION_CLIENT_ID / NOTION_CLIENT_SECRET / REDIRECT_URI not fully set; pairing will fail at /go");
  }

  const server = createBroker({ ...env, PUBLIC_URL: publicURL });
  const { host, port } = parseAddr(env.LISTEN_ADDR);
  server.listen(port, host, () => {
    console.log(`broker listening on ${host || "0.0.0.0"}:${port}`);
  });
}

// Only run the server when executed directly, so tests can import the module.
if (process.argv[1] && import.meta.url === new URL(process.argv[1], "file:").href) {
  main();
}
