// rm-notion pairing broker — a hand-rolled RFC 8628 device-flow equivalent
// over Notion's OAuth code grant:
//
//   POST /pair          mint device_code + verification_url (TTL 600s)
//   GET  /pair/{code}   device polls with the DEVICE code: pending | ok | expired
//   GET  /go/{code}     phone lands here with the USER code → confirmation page
//   POST /go/{code}     confirmed → 303 to Notion authorize, state={user code}
//   GET  /callback      Notion redirect → exchange code, store token
//   GET  /healthz       liveness
//
// Each pairing carries two independent secrets: the device code never leaves
// the device and is the only credential that can claim the token; the user
// code travels through the QR, the phone's browser and Notion's OAuth state,
// and can only start a consent flow.
//
// This module is the whole broker and runs unchanged on every deployment.
// Everything environment-specific is behind two arguments: `env` (config) and
// `store` (where pairings live — Durable Objects on Workers, memory on a
// server). See worker.js and node.js.

import { CODE_RE, DEVICE_CODE_LEN, USER_CODE_LEN, randomCode } from "./codes.js";
import { CONFIRM_HEADERS, SECURITY_HEADERS, confirmPage, json, page, truncate } from "./pages.js";

const NOTION = "https://api.notion.com";

// handleRequest serves one request. clientIP is supplied by the caller because
// only the adapter knows how to find it: a Workers header, or a socket address
// plus whatever proxy header the operator has told us to trust.
export async function handleRequest(request, env, store, clientIP = "unknown") {
  const url = new URL(request.url);
  const { pathname } = url;
  const method = request.method;

  if (method === "GET" && pathname === "/healthz") {
    return new Response("ok\n", {
      headers: { "Content-Type": "text/plain; charset=utf-8", ...SECURITY_HEADERS },
    });
  }

  // POST /pair — device mints a pairing for its QR.
  if (method === "POST" && pathname === "/pair") {
    if (!(await store.take(clientIP))) {
      return json(429, { error: "slow down" }, { "Retry-After": "5" });
    }
    const deviceCode = randomCode(DEVICE_CODE_LEN);
    const userCode = randomCode(USER_CODE_LEN);
    await store.mint(deviceCode, userCode);
    // The QR is built from PUBLIC_URL. Falling back to the request's Host
    // would let a caller choose the host the user's phone is sent to.
    const base = (env.PUBLIC_URL || url.origin).replace(/\/+$/, "");
    return json(200, { device_code: deviceCode, verification_url: base + "/go/" + userCode });
  }

  // GET /pair/{deviceCode} — device polls; the token is delivered exactly once.
  let m = method === "GET" && pathname.match(/^\/pair\/([a-z0-9]+)$/);
  if (m && CODE_RE.test(m[1])) {
    const r = await store.claim(m[1]);
    if (r.state === "ok") {
      return json(200, { state: "ok", access_token: r.token, workspace: r.workspace });
    }
    if (r.state === "pending") return json(200, { state: "pending" });
    return json(404, { state: "expired" });
  }

  // /go/{userCode} — human leg. GET shows a confirmation page; POST starts
  // the consent flow.
  m = pathname.match(/^\/go\/([a-z0-9]+)$/);
  if (m && (method === "GET" || method === "POST") && CODE_RE.test(m[1])) {
    const userCode = m[1];
    const device = await store.resolveUser(userCode);
    if (!device || !(await store.pending(device))) {
      return page(404, "Code expired",
        "This pairing code has expired. Start pairing again on your reMarkable.");
    }
    if (!env.NOTION_CLIENT_ID || !env.NOTION_CLIENT_SECRET) {
      return page(503, "Broker not configured",
        "This broker has no Notion OAuth credentials configured.");
    }
    if (method === "GET") return confirmPage(userCode);

    const q = new URLSearchParams({
      client_id: env.NOTION_CLIENT_ID,
      response_type: "code",
      owner: "user",
      redirect_uri: env.REDIRECT_URI || url.origin + "/callback",
      state: userCode,
    });
    return new Response(null, {
      status: 303,
      headers: { Location: NOTION + "/v1/oauth/authorize?" + q, ...CONFIRM_HEADERS },
    });
  }

  // GET /callback — Notion redirect target: exchange and store the token.
  if (method === "GET" && pathname === "/callback") {
    const state = url.searchParams.get("state") || "";
    const err = url.searchParams.get("error");
    if (err) {
      return page(400, "Connection cancelled",
        "Notion reported: " + truncate(err, 100) +
        ". Start pairing again on your reMarkable if this wasn't you.");
    }
    const code = url.searchParams.get("code");
    if (!state || !code || !CODE_RE.test(state)) {
      return page(400, "Invalid callback", "Missing code or state.");
    }

    const device = await store.resolveUser(state);
    if (!device || !(await store.pending(device))) {
      return page(404, "Code expired",
        "This pairing expired before the connection completed. Start again on your reMarkable.");
    }

    const tokenResp = await fetch((env.NOTION_API || NOTION) + "/v1/oauth/token", {
      method: "POST",
      headers: {
        Authorization: "Basic " + btoa(env.NOTION_CLIENT_ID + ":" + env.NOTION_CLIENT_SECRET),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        grant_type: "authorization_code",
        code,
        redirect_uri: env.REDIRECT_URI || url.origin + "/callback",
      }),
    });
    if (!tokenResp.ok) {
      // Status only: the body is a Notion error document that can echo
      // request material, and logs are not the place for it.
      console.log("callback: token exchange failed with HTTP", tokenResp.status);
      return page(502, "Connection failed",
        "Notion did not accept the authorization. Start pairing again on your reMarkable.");
    }
    const token = await tokenResp.json();
    if (!token.access_token) {
      return page(502, "Connection failed",
        "Notion did not accept the authorization. Start pairing again on your reMarkable.");
    }

    if (!(await store.complete(device, token.access_token, token.workspace_name || ""))) {
      return page(404, "Code expired",
        "This pairing expired before the connection completed. Start again on your reMarkable.");
    }
    return page(200, "Connected",
      "Your reMarkable is now connected to " + (token.workspace_name || "your workspace") +
      ". You can close this tab — the device will finish up on its own.");
  }

  return new Response("not found\n", { status: 404, headers: SECURITY_HEADERS });
}
