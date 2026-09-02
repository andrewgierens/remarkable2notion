// Exercises the shared broker end to end against the in-memory store. This is
// the same module the Workers deployment runs, so everything here except the
// storage backend is the code that faces the internet.
import assert from "node:assert/strict";
import test, { mock } from "node:test";
import { handleRequest } from "../src/broker.js";
import { MemoryStore } from "../src/store-memory.js";
import { TTL_MS } from "../src/store-memory.js";

const ENV = {
  PUBLIC_URL: "https://broker.example",
  REDIRECT_URI: "https://broker.example/callback",
  NOTION_CLIENT_ID: "cid",
  NOTION_CLIENT_SECRET: "csecret",
  NOTION_API: "https://notion.test",
};

// clock lets a test move time forward instead of sleeping on the TTL.
function clock() {
  let now = 1_700_000_000_000;
  const fn = () => now;
  fn.advance = (ms) => (now += ms);
  return fn;
}

function setup(env = ENV) {
  const now = clock();
  const store = new MemoryStore(now);
  const call = (method, path, ip = "1.2.3.4") =>
    handleRequest(new Request("https://broker.example" + path, { method }), env, store, ip);
  return { store, call, now };
}

// mockNotionToken stubs the one outbound call the broker makes.
function mockNotionToken(response) {
  mock.method(globalThis, "fetch", async () => response);
}

async function mint(call) {
  const res = await call("POST", "/pair");
  assert.equal(res.status, 200);
  const body = await res.json();
  const userCode = new URL(body.verification_url).pathname.split("/").pop();
  return { deviceCode: body.device_code, userCode, body };
}

test("mint returns a device code and a verification URL on PUBLIC_URL", async () => {
  const { call } = setup();
  const { body, deviceCode, userCode } = await mint(call);
  assert.equal(body.verification_url, "https://broker.example/go/" + userCode);
  assert.notEqual(deviceCode, userCode);
  // The device code must never appear in what the phone sees.
  assert.ok(!body.verification_url.includes(deviceCode));
});

test("polling before consent is pending, and an unknown code is expired", async () => {
  const { call } = setup();
  const { deviceCode } = await mint(call);

  let res = await call("GET", "/pair/" + deviceCode);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { state: "pending" });

  res = await call("GET", "/pair/" + "z".repeat(32));
  assert.equal(res.status, 404);
  assert.equal((await res.json()).state, "expired");
});

test("the whole pairing flow hands the token over exactly once", async (t) => {
  const { call } = setup();
  const { deviceCode, userCode } = await mint(call);

  // The phone lands on a confirmation page rather than being bounced
  // straight to Notion — the device-code phishing mitigation.
  let res = await call("GET", "/go/" + userCode);
  assert.equal(res.status, 200);
  const page = await res.text();
  assert.match(page, /Only continue if you just scanned/);
  assert.match(page, new RegExp(`action="/go/${userCode}"`));

  // Confirming redirects to Notion with the user code as OAuth state.
  res = await call("POST", "/go/" + userCode);
  assert.equal(res.status, 303);
  const authorize = new URL(res.headers.get("Location"));
  assert.equal(authorize.origin + authorize.pathname, "https://api.notion.com/v1/oauth/authorize");
  assert.equal(authorize.searchParams.get("state"), userCode);
  assert.equal(authorize.searchParams.get("client_id"), "cid");
  assert.equal(authorize.searchParams.get("redirect_uri"), ENV.REDIRECT_URI);
  // The secret must not travel to Notion's authorize endpoint.
  assert.ok(!res.headers.get("Location").includes("csecret"));

  mockNotionToken(
    new Response(JSON.stringify({ access_token: "tok-live", workspace_name: "Acme" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  t.after(() => mock.restoreAll());

  res = await call("GET", "/callback?state=" + userCode + "&code=auth-code");
  assert.equal(res.status, 200);
  assert.match(await res.text(), /Acme/);

  // The device claims the token once...
  res = await call("GET", "/pair/" + deviceCode);
  assert.deepEqual(await res.json(), {
    state: "ok",
    access_token: "tok-live",
    workspace: "Acme",
  });

  // ...and the pairing is gone, so a replayed device code gets nothing.
  res = await call("GET", "/pair/" + deviceCode);
  assert.equal(res.status, 404);
  assert.equal((await res.json()).state, "expired");
});

test("a pairing expires at the TTL", async () => {
  const { call, now } = setup();
  const { deviceCode, userCode } = await mint(call);

  now.advance(TTL_MS + 1);

  let res = await call("GET", "/pair/" + deviceCode);
  assert.equal(res.status, 404);
  res = await call("GET", "/go/" + userCode);
  assert.equal(res.status, 404);
  assert.match(await res.text(), /expired/i);
});

test("the user code cannot claim a token and the device code cannot consent", async () => {
  const { call } = setup();
  const { deviceCode, userCode } = await mint(call);

  // Polling with the public user code must not find the pairing.
  let res = await call("GET", "/pair/" + userCode);
  assert.equal(res.status, 404);

  // And the secret device code is not a valid consent code.
  res = await call("GET", "/go/" + deviceCode);
  assert.equal(res.status, 404);
});

test("a callback for an unknown or malformed state is refused", async () => {
  const { call } = setup();
  await mint(call);

  let res = await call("GET", "/callback?state=" + "q".repeat(16) + "&code=x");
  assert.equal(res.status, 404);

  res = await call("GET", "/callback?code=x");
  assert.equal(res.status, 400);

  res = await call("GET", "/callback?state=NOT-A-CODE!&code=x");
  assert.equal(res.status, 400);

  res = await call("GET", "/callback?state=abc&error=access_denied");
  assert.equal(res.status, 400);
  assert.match(await res.text(), /cancelled/i);
});

test("a failed token exchange does not complete the pairing", async (t) => {
  const { call } = setup();
  const { deviceCode, userCode } = await mint(call);
  mockNotionToken(new Response("nope", { status: 400 }));
  t.after(() => mock.restoreAll());

  let res = await call("GET", "/callback?state=" + userCode + "&code=auth-code");
  assert.equal(res.status, 502);

  // Still pending, not stuck in some half-completed state.
  res = await call("GET", "/pair/" + deviceCode);
  assert.equal((await res.json()).state, "pending");
});

test("minting is rate limited per client IP", async () => {
  const { call } = setup();
  let limited = 0;
  for (let i = 0; i < 15; i++) {
    const res = await call("POST", "/pair", "9.9.9.9");
    if (res.status === 429) limited++;
  }
  assert.ok(limited > 0, "a burst from one IP should be limited");
  // Another IP is unaffected.
  assert.equal((await call("POST", "/pair", "8.8.8.8")).status, 200);
});

test("without OAuth credentials the human leg says so instead of redirecting", async () => {
  const { call } = setup({ PUBLIC_URL: ENV.PUBLIC_URL });
  const { userCode } = await mint(call);
  const res = await call("GET", "/go/" + userCode);
  assert.equal(res.status, 503);
  assert.match(await res.text(), /not configured/i);
});

test("every response carries the security headers", async () => {
  const { call } = setup();
  for (const [method, path] of [["GET", "/healthz"], ["POST", "/pair"], ["GET", "/nope"]]) {
    const res = await call(method, path);
    assert.equal(res.headers.get("Referrer-Policy"), "no-referrer", path);
    assert.equal(res.headers.get("Cache-Control"), "no-store", path);
    assert.equal(res.headers.get("X-Content-Type-Options"), "nosniff", path);
    assert.equal(res.headers.get("X-Frame-Options"), "DENY", path);
  }
});

test("unknown routes and methods are refused", async () => {
  const { call } = setup();
  assert.equal((await call("GET", "/")).status, 404);
  assert.equal((await call("GET", "/pair")).status, 404);
  assert.equal((await call("DELETE", "/pair/" + "a".repeat(32))).status, 404);
  assert.equal((await call("GET", "/healthz")).status, 200);
});

// The confirmation page's button posts to us and we answer with a 303 into
// Notion. Chrome checks form-action on every hop of a form submission's
// navigation, so if it does not name Notion's origins the redirect is
// silently dropped and the button appears to do nothing. Firefox does not
// block it, which is what makes the breakage look intermittent.
test("the confirm page and its redirect allow forms to reach Notion", async () => {
  const { call } = setup();
  const { userCode } = await mint(call);

  for (const res of [await call("GET", "/go/" + userCode), await call("POST", "/go/" + userCode)]) {
    const policy = res.headers.get("Content-Security-Policy");
    const formAction = policy.split(";").map((d) => d.trim()).find((d) => d.startsWith("form-action"));
    assert.ok(formAction.includes("'self'"), formAction);
    assert.ok(formAction.includes("https://api.notion.com"), formAction);
    assert.ok(formAction.includes("https://www.notion.so"), formAction);
    // The rest of the policy must stay locked down.
    assert.ok(policy.includes("default-src 'none'"), policy);
    assert.ok(policy.includes("frame-ancestors 'none'"), policy);
  }
});

// Only the pages in that flow are widened; everything else stays strict.
test("other responses keep form-action locked to self", async () => {
  const { call } = setup();
  for (const [method, path] of [["POST", "/pair"], ["GET", "/nope"], ["GET", "/callback?code=x"]]) {
    const policy = (await call(method, path)).headers.get("Content-Security-Policy");
    if (!policy) continue; // /healthz and JSON replies still carry it; guard anyway
    assert.ok(!policy.includes("notion.com"), `${path}: ${policy}`);
  }
});
