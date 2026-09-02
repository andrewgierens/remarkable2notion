// Responses the broker hands back: JSON for the device, HTML for the phone.

// Referrer-Policy is the load-bearing one: /go/{code} leads to Notion, and
// without it the browser would put the pairing URL — user code and all — in
// that request's Referer. no-store keeps the token-bearing poll response out
// of any intermediary cache.
const csp = (formAction) =>
  `default-src 'none'; style-src 'unsafe-inline'; form-action ${formAction}; frame-ancestors 'none'; base-uri 'none'`;

export const SECURITY_HEADERS = {
  "Referrer-Policy": "no-referrer",
  "Cache-Control": "no-store",
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "Content-Security-Policy": csp("'self'"),
};

// The confirmation page's form posts to us and we answer with a 303 into
// Notion's consent flow. Chrome applies form-action to every hop of a form
// submission's navigation, redirects included, so `form-action 'self'` would
// silently drop that redirect and the button would appear to do nothing.
// Firefox does not block it, which is what makes this look intermittent.
// The origins below are exactly where that flow legitimately goes: Notion's
// authorize endpoint, and the notion.so pages it forwards to for login and
// consent.
export const CONFIRM_HEADERS = {
  ...SECURITY_HEADERS,
  "Content-Security-Policy": csp("'self' https://api.notion.com https://www.notion.so https://notion.so"),
};

export function json(status, body, extraHeaders) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...SECURITY_HEADERS, ...extraHeaders },
  });
}

export function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

const PAGE_STYLE =
  "font-family:system-ui,sans-serif;max-width:26rem;margin:15vh auto 0;padding:0 1rem;color:#222";

export function html(status, body, headers = SECURITY_HEADERS) {
  return new Response(body, {
    status,
    headers: { "Content-Type": "text/html; charset=utf-8", ...headers },
  });
}

export function page(status, title, body) {
  const t = escapeHtml(title);
  const b = escapeHtml(body);
  return html(
    status,
    `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${t}</title>
<body style="${PAGE_STYLE}">
<h1 style="font-size:1.4rem">${t}</h1><p style="line-height:1.5">${b}</p></body>`,
  );
}

// confirmPage names what is about to happen before the consent flow starts.
// A bare GET can be triggered by anyone who gets a link in front of the user,
// so the flow only begins on an explicit click — the standing mitigation for
// device-code phishing, where an attacker shows a victim a pairing URL for the
// attacker's own device.
export function confirmPage(userCode) {
  return html(
    200,
    `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect Notion</title>
<body style="${PAGE_STYLE}">
<h1 style="font-size:1.4rem">Connect your reMarkable to Notion</h1>
<p style="line-height:1.5">You are about to give a reMarkable device access to the Notion
pages you pick on the next screen.</p>
<p style="line-height:1.5"><strong>Only continue if you just scanned this code from your own
device.</strong> If you reached this page from a link or a message, close this tab.</p>
<form method="post" action="/go/${escapeHtml(userCode)}">
<button type="submit" style="font:inherit;padding:.7rem 1.2rem;border:1px solid #222;background:#222;color:#fff;border-radius:.4rem">Continue to Notion</button>
</form></body>`,
    CONFIRM_HEADERS,
  );
}

export function truncate(s, n) {
  return s.length <= n ? s : s.slice(0, n) + "…";
}
