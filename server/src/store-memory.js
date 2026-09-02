// In-memory pairing store, for a single-process deployment (VPS, container,
// local dev). Pairings are deliberately not persisted: the broker is a relay,
// and a restart losing an in-flight pairing costs one retry.
//
// Implements the same contract as the Durable Object store in worker.js:
//   mint(deviceCode, userCode)  create the pairing and its user-code pointer
//   resolveUser(userCode)       -> device code, or ""
//   pending(deviceCode)         -> alive and not yet completed
//   complete(deviceCode, ...)   -> false if it expired first
//   claim(deviceCode)           -> {state: ok|pending|expired, token, workspace}
//   take(clientIP)              -> false when rate limited

export const TTL_MS = 10 * 60 * 1000;

// A real device mints once per pairing attempt.
export const MINT_BURST = 10;
export const MINT_REFILL_PER_SEC = 0.2;
export const LIMIT_IDLE_MS = 10 * 60 * 1000;

export class MemoryStore {
  // now is injectable so tests can advance time instead of sleeping.
  constructor(now = () => Date.now()) {
    this.now = now;
    this.pairings = new Map(); // device code → {expires, userCode, token?, workspace?}
    this.users = new Map(); // user code → {expires, device}
    this.buckets = new Map(); // client ip → {tokens, seen}
  }

  // reap drops everything past its TTL. Called on every operation, so an
  // abandoned pairing cannot outlive its window even with no traffic for it.
  reap() {
    const now = this.now();
    for (const [k, v] of this.pairings) if (now > v.expires) this.pairings.delete(k);
    for (const [k, v] of this.users) if (now > v.expires) this.users.delete(k);
    for (const [k, v] of this.buckets) if (now - v.seen > LIMIT_IDLE_MS) this.buckets.delete(k);
  }

  async mint(deviceCode, userCode) {
    this.reap();
    const expires = this.now() + TTL_MS;
    this.pairings.set(deviceCode, { expires, userCode });
    this.users.set(userCode, { expires, device: deviceCode });
  }

  async resolveUser(userCode) {
    this.reap();
    return this.users.get(userCode)?.device || "";
  }

  async pending(deviceCode) {
    this.reap();
    const p = this.pairings.get(deviceCode);
    return !!p && p.token === undefined;
  }

  async complete(deviceCode, token, workspace) {
    this.reap();
    const p = this.pairings.get(deviceCode);
    if (!p) return false;
    p.token = token;
    p.workspace = workspace;
    return true;
  }

  async claim(deviceCode) {
    this.reap();
    const p = this.pairings.get(deviceCode);
    if (!p) return { state: "expired" };
    if (p.token === undefined) return { state: "pending" };
    // Exactly once: the token is never at rest after handoff.
    this.pairings.delete(deviceCode);
    this.users.delete(p.userCode);
    return { state: "ok", token: p.token, workspace: p.workspace || "" };
  }

  async take(clientIP) {
    this.reap();
    const now = this.now();
    const b = this.buckets.get(clientIP) ?? { tokens: MINT_BURST, seen: now };
    b.tokens = Math.min(MINT_BURST, b.tokens + ((now - b.seen) / 1000) * MINT_REFILL_PER_SEC);
    b.seen = now;
    if (b.tokens < 1) {
      this.buckets.set(clientIP, b);
      return false;
    }
    b.tokens -= 1;
    this.buckets.set(clientIP, b);
    return true;
  }
}
