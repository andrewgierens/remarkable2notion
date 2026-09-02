// Cloudflare Workers deployment of the shared broker.
//
// Storage is Durable Objects: one per pairing (keyed by device code, strongly
// consistent so the handoff stays exactly-once), one pointer object per user
// code, and one per client IP for mint rate limiting. Alarms wipe all of them
// at TTL, so the broker never retains user tokens beyond the pairing window.
//
// Secrets/vars: NOTION_CLIENT_ID, NOTION_CLIENT_SECRET, and optionally
// REDIRECT_URI (default <origin>/callback) and PUBLIC_URL (default origin).

import { handleRequest } from "./broker.js";
import { json } from "./pages.js";
import { LIMIT_IDLE_MS, MINT_BURST, MINT_REFILL_PER_SEC, TTL_MS } from "./store-memory.js";

// Pairings are keyed by their device code; a separate pointer object maps the
// public user code onto it, so /go and /callback never need the device code.
const userKey = (userCode) => "u:" + userCode;
const limitKey = (ip) => "r:" + ip;

// DurableStore implements the store contract on top of the Pairing object.
class DurableStore {
  constructor(env) {
    this.env = env;
  }

  async call(name, op, body) {
    const stub = this.env.PAIRINGS.get(this.env.PAIRINGS.idFromName(name));
    const resp = await stub.fetch("https://do/" + op, {
      method: "POST",
      body: body ? JSON.stringify(body) : null,
    });
    return resp.json();
  }

  async mint(deviceCode, userCode) {
    await this.call(deviceCode, "init", { userCode });
    await this.call(userKey(userCode), "point", { device: deviceCode });
  }

  async resolveUser(userCode) {
    return (await this.call(userKey(userCode), "resolve")).device || "";
  }

  async pending(deviceCode) {
    return (await this.call(deviceCode, "pending")).pending === true;
  }

  async complete(deviceCode, token, workspace) {
    return (await this.call(deviceCode, "complete", { token, workspace })).ok === true;
  }

  async claim(deviceCode) {
    return this.call(deviceCode, "claim");
  }

  async take(clientIP) {
    return (await this.call(limitKey(clientIP), "take")).ok === true;
  }
}

export default {
  async fetch(request, env) {
    const ip = request.headers.get("CF-Connecting-IP") || "unknown";
    return handleRequest(request, env, new DurableStore(env), ip);
  },
};

// Pairing is one Durable Object serving three roles, distinguished by the
// name it was addressed with:
//
//   <deviceCode>      the pairing: {expires, userCode, token?, workspace?}
//   "u:"<userCode>    a pointer to the pairing's device code
//   "r:"<ip>          a mint rate-limit bucket
//
// The alarm wipes each of them at TTL, and a successful claim wipes the
// pairing immediately so the token is never at rest after handoff.
export class Pairing {
  constructor(ctx) {
    this.ctx = ctx;
  }

  async alive() {
    const expires = await this.ctx.storage.get("expires");
    if (expires === undefined) return false;
    if (Date.now() > expires) {
      await this.ctx.storage.deleteAll();
      return false;
    }
    return true;
  }

  async fetch(request) {
    const op = new URL(request.url).pathname.slice(1);

    switch (op) {
      // --- pairing (addressed by device code) ---
      case "init": {
        const { userCode } = await request.json();
        await this.ctx.storage.put({ expires: Date.now() + TTL_MS, userCode });
        await this.ctx.storage.setAlarm(Date.now() + TTL_MS);
        return json(200, { ok: true });
      }
      case "pending": {
        const alive = await this.alive();
        const token = alive ? await this.ctx.storage.get("token") : undefined;
        return json(200, { pending: alive && token === undefined });
      }
      case "complete": {
        if (!(await this.alive())) return json(200, { ok: false });
        const { token, workspace } = await request.json();
        await this.ctx.storage.put({ token, workspace });
        return json(200, { ok: true });
      }
      case "claim": {
        if (!(await this.alive())) return json(200, { state: "expired" });
        const token = await this.ctx.storage.get("token");
        if (token === undefined) return json(200, { state: "pending" });
        const workspace = (await this.ctx.storage.get("workspace")) || "";
        await this.ctx.storage.deleteAll(); // exactly once
        await this.ctx.storage.deleteAlarm();
        return json(200, { state: "ok", token, workspace });
      }

      // --- user-code pointer (addressed by "u:"<userCode>) ---
      case "point": {
        const { device } = await request.json();
        await this.ctx.storage.put({ expires: Date.now() + TTL_MS, device });
        await this.ctx.storage.setAlarm(Date.now() + TTL_MS);
        return json(200, { ok: true });
      }
      case "resolve": {
        if (!(await this.alive())) return json(200, { device: "" });
        return json(200, { device: (await this.ctx.storage.get("device")) || "" });
      }

      // --- rate-limit bucket (addressed by "r:"<ip>) ---
      case "take": {
        const now = Date.now();
        const seen = (await this.ctx.storage.get("seen")) ?? now;
        let tokens = (await this.ctx.storage.get("tokens")) ?? MINT_BURST;
        tokens = Math.min(MINT_BURST, tokens + ((now - seen) / 1000) * MINT_REFILL_PER_SEC);
        if (tokens < 1) {
          await this.ctx.storage.put({ tokens, seen: now });
          return json(200, { ok: false });
        }
        await this.ctx.storage.put({ tokens: tokens - 1, seen: now });
        // Buckets are disposable state; let them expire so idle IPs do not
        // keep an object alive forever.
        await this.ctx.storage.setAlarm(now + LIMIT_IDLE_MS);
        return json(200, { ok: true });
      }

      default:
        return json(404, { error: "unknown op " + op });
    }
  }

  async alarm() {
    await this.ctx.storage.deleteAll();
  }
}
