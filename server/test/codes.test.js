import assert from "node:assert/strict";
import test from "node:test";
import { ALPHABET, CODE_RE, DEVICE_CODE_LEN, USER_CODE_LEN, randomCode } from "../src/codes.js";

test("codes are the requested length and alphabet", () => {
  for (const n of [1, 16, 32, 64]) {
    const c = randomCode(n);
    assert.equal(c.length, n);
    for (const ch of c) assert.ok(ALPHABET.includes(ch), `${ch} not in alphabet`);
  }
});

test("codes pass the wire validation the routes apply", () => {
  assert.match(randomCode(DEVICE_CODE_LEN), CODE_RE);
  assert.match(randomCode(USER_CODE_LEN), CODE_RE);
});

test("the alphabet has no characters that are ambiguous on e-ink", () => {
  for (const ch of "0O1li") assert.ok(!ALPHABET.includes(ch), `${ch} should be excluded`);
  assert.equal(new Set(ALPHABET).size, ALPHABET.length, "alphabet must not repeat");
});

// Rejection sampling exists so no character is favoured. 256 - (256 % 31) is
// 248, exactly 8 x 31; a modulus over the full byte range would make the
// first 8 characters ~3% more likely.
test("rejection sampling leaves no modulo bias", () => {
  assert.equal((256 - (256 % ALPHABET.length)) % ALPHABET.length, 0);

  const counts = new Map();
  const draws = 62_000;
  for (const ch of randomCode(draws)) counts.set(ch, (counts.get(ch) ?? 0) + 1);
  assert.equal(counts.size, ALPHABET.length, "every character should appear");
  const expected = draws / ALPHABET.length;
  for (const [ch, n] of counts) {
    // Six sigma on a binomial with p = 1/31; a modulo bias would be a
    // systematic ~3% skew across a whole third of the alphabet.
    const sigma = Math.sqrt(draws * (1 / ALPHABET.length) * (1 - 1 / ALPHABET.length));
    assert.ok(Math.abs(n - expected) < 6 * sigma, `${ch} appeared ${n} times, expected ~${expected}`);
  }
});

test("codes do not repeat", () => {
  const seen = new Set();
  for (let i = 0; i < 500; i++) seen.add(randomCode(DEVICE_CODE_LEN));
  assert.equal(seen.size, 500);
});
