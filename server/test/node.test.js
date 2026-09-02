// The Node adapter: everything between a socket and the shared broker.
import assert from "node:assert/strict";
import test from "node:test";
import { createBroker, parseAddr } from "../src/node.js";
import { MemoryStore } from "../src/store-memory.js";

const ENV = {
  PUBLIC_URL: "https://broker.example",
  REDIRECT_URI: "https://broker.example/callback",
  NOTION_CLIENT_ID: "cid",
  NOTION_CLIENT_SECRET: "csecret",
};

// serve starts the broker on an ephemeral port and returns a fetch bound to it.
async function serve(t, env = ENV, store = new MemoryStore()) {
  const server = createBroker(env, store);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const base = "http://127.0.0.1:" + server.address().port;
  return { base, call: (path, init) => fetch(base + path, init), store };
}

test("parseAddr accepts Go's :port as well as host:port", () => {
  assert.deepEqual(parseAddr(""), { port: 8080 });
  assert.deepEqual(parseAddr(undefined), { port: 8080 });
  assert.deepEqual(parseAddr(":9000"), { port: 9000 });
  assert.deepEqual(parseAddr("127.0.0.1:9000"), { host: "127.0.0.1", port: 9000 });
  assert.deepEqual(parseAddr("8080"), { port: 8080 });
});

test("serves the flow over real HTTP", async (t) => {
  const { call } = await serve(t);

  let res = await call("/healthz");
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "ok\n");
  assert.equal(res.headers.get("Referrer-Policy"), "no-referrer");

  res = await call("/pair", { method: "POST" });
  assert.equal(res.status, 200);
  const body = await res.json();
  // The QR URL comes from PUBLIC_URL, never the Host header the caller sent.
  assert.match(body.verification_url, /^https:\/\/broker\.example\/go\//);

  const userCode = body.verification_url.split("/").pop();
  res = await call("/go/" + userCode);
  assert.equal(res.status, 200);
  assert.match(await res.text(), /Connect your reMarkable/);

  res = await call("/pair/" + body.device_code);
  assert.deepEqual(await res.json(), { state: "pending" });
});

test("a redirect is not followed into Notion by the adapter", async (t) => {
  const { call } = await serve(t);
  const body = await (await call("/pair", { method: "POST" })).json();
  const userCode = body.verification_url.split("/").pop();

  const res = await call("/go/" + userCode, { method: "POST", redirect: "manual" });
  assert.equal(res.status, 303);
  assert.match(res.headers.get("Location"), /^https:\/\/api\.notion\.com\/v1\/oauth\/authorize\?/);
});

// The rate limiter keys on the client address. A proxy header is only
// honoured when the operator names one, or anyone could spoof their way
// around the limit by sending a header.
test("the proxy header is ignored unless TRUST_PROXY_HEADER names it", async (t) => {
  const seen = [];
  const spy = new MemoryStore();
  spy.take = async (ip) => {
    seen.push(ip);
    return true;
  };

  const plain = await serve(t, ENV, spy);
  await plain.call("/pair", { method: "POST", headers: { "X-Forwarded-For": "203.0.113.9" } });
  assert.equal(seen.at(-1), "127.0.0.1", "spoofed header must not be believed");

  const trusting = await serve(t, { ...ENV, TRUST_PROXY_HEADER: "X-Forwarded-For" }, spy);
  await trusting.call("/pair", {
    method: "POST",
    headers: { "X-Forwarded-For": "203.0.113.9, 10.0.0.1" },
  });
  assert.equal(seen.at(-1), "203.0.113.9", "the client is the first entry");
});

test("rate limiting applies over HTTP too", async (t) => {
  const { call } = await serve(t);
  let limited = 0;
  for (let i = 0; i < 15; i++) {
    const res = await call("/pair", { method: "POST" });
    if (res.status === 429) {
      limited++;
      assert.equal(res.headers.get("Retry-After"), "5");
    }
  }
  assert.ok(limited > 0);
});
