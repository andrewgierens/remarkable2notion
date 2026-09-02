// Pairing codes. Shared by every deployment so the two legs of the flow have
// the same strength wherever the broker runs.

// Omits the character pairs that are ambiguous when a human reads a code off
// an e-ink screen: 0/O and 1/l/i.
export const ALPHABET = "abcdefghjkmnpqrstuvwxyz23456789";

export const DEVICE_CODE_LEN = 32; // ~158 bits — claims the token
export const USER_CODE_LEN = 16; // ~79 bits — only starts a consent flow

// What a code coming in off the wire is allowed to look like. Lookups are by
// exact key, so this only keeps obvious junk out of the store.
export const CODE_RE = /^[a-z0-9]{8,64}$/;

// randomCode draws n characters uniformly from ALPHABET. It rejects the top of
// each byte's range rather than taking a modulus, so no character is favoured:
// 256 - (256 % 31) is 248, exactly 8 x 31.
export function randomCode(n) {
  const limit = 256 - (256 % ALPHABET.length);
  let out = "";
  while (out.length < n) {
    const buf = new Uint8Array(n);
    crypto.getRandomValues(buf);
    for (const b of buf) {
      if (b >= limit) continue;
      out += ALPHABET[b % ALPHABET.length];
      if (out.length === n) break;
    }
  }
  return out;
}
