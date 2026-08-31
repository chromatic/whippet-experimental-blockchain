// signing.test.js -- externally-anchored tests for the ECDSA/secp256k1
// signing path used by uap.js.
//
// WHY THIS FILE EXISTS
// --------------------
// The rest of the suite verifies signing against itself: uap.test.js signs
// with a stub that returns a fixed fake DER blob, the sighash vectors never
// touch the curve, and real signing is only exercised by integration tests
// that need a live daemon. A signature could therefore be malformed --
// high-S, mis-encoded DER, wrong sighash byte -- and every existing test
// would still pass.
//
// So nothing here is anchored to values this codebase produced. The anchors
// are:
//
//   (a) Published constants: the secp256k1 base point and its small
//       multiples; the RFC 4231 HMAC-SHA-256 test vectors; and the RFC 6979
//       Appendix A.2.5 deterministic-ECDSA vectors.
//
//       A caveat on that last one, because it is easy to get wrong and is
//       widely mis-cited: RFC 6979 A.2.5 is NIST P-256, not secp256k1. RFC
//       6979 publishes no secp256k1 vectors at all. They are used here only
//       on the curve they were actually published for, to pin the reference
//       implementation's deterministic-k and ECDSA equations -- never as an
//       expected value for anything secp256k1.
//
//   (b) A deliberately independent reference implementation, written below
//       straight from the specifications (SEC 2 curve parameters, RFC 6979
//       section 3.2, SEC 1 ECDSA), using BigInt arithmetic and node:crypto's
//       HMAC-SHA256. It shares no code with uap.js, sha256.js or noble.
//
// The reference implementation is first pinned to the published constants
// in (a); only then is it used as an oracle for the values in (b) that no
// published vector covers (arbitrary transaction sighashes). Two independent
// derivations agreeing -- a published document and code written from the
// spec -- is the strongest anchor available without a live node.
//
// Everything that can go through uap.js goes through uap.js: configureSecp()
// for the RFC 6979 HMAC injection, and signSpend()/signP2PKHInput() for
// producing signatures. See the note on signDer() below for the one place
// this file has to speak to the EC library itself.

import assert from 'node:assert';
import { createHmac } from 'node:crypto';
// secp.js is this repo's single secp256k1 entry point: it re-exports the
// vendored noble build and adds back the v1-shaped signSync() -- including
// the DER encoder, which noble v2 dropped. Importing '@noble/secp256k1' bare
// here would be a second, unpinned path to the library and would test noble's
// encoding rather than ours.
import * as secpLib from './secp.js';
import * as UAP from './uap.js';
import { hex, fromHex, fromUtf8 } from './test-helpers.js';
import { sha256, hmacSha256, hash256 } from './sha256.js';

// ---------------------------------------------------------------------------
// assertion counting
// ---------------------------------------------------------------------------

let assertions = 0;

function check(cond, msg) {
  assertions++;
  assert.ok(cond, msg);
}

function eq(actual, expected, msg) {
  assertions++;
  assert.strictEqual(actual, expected, msg);
}

// ---------------------------------------------------------------------------
// Reference secp256k1 -- written from the specs, shares nothing with the
// code under test.
//
// Curve parameters: SEC 2 v2, section 2.4.1 ("Recommended Parameters
// secp256k1"). Also reproduced in BIP 340 and in every secp256k1
// implementation in existence.
// ---------------------------------------------------------------------------

const K1 = {
  name: 'secp256k1',
  p: 0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2fn,
  a: 0n,
  b: 7n,
  n: 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n,
  Gx: 0x79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798n,
  Gy: 0x483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8n,
};
K1.G = { x: K1.Gx, y: K1.Gy };

// NIST P-256 / secp256r1. Source: FIPS 186-4 Appendix D.1.2.3 and SEC 2 v2
// section 2.4.2. Present ONLY so the RFC 6979 known-answer vectors (which
// are specified over the NIST curves, not over secp256k1) can be used to
// validate the reference deterministic-k and ECDSA code below on the curve
// they were actually published for.
const P256 = {
  name: 'P-256',
  p: 0xffffffff00000001000000000000000000000000ffffffffffffffffffffffffn,
  a: -3n,
  b: 0x5ac635d8aa3a93e7b3ebbd55769886bc651d06b0cc53b0f63bce3c3e27d2604bn,
  n: 0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551n,
  Gx: 0x6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296n,
  Gy: 0x4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5n,
};
P256.G = { x: P256.Gx, y: P256.Gy };

// Short aliases for the curve actually under test.
const P = K1.p;
const N = K1.n;
const N_HALF = N >> 1n;
const GX = K1.Gx;
const GY = K1.Gy;
const G = K1.G;

function mod(a, m) {
  const r = a % m;
  return r < 0n ? r + m : r;
}

/** Extended Euclid. Modular inverse of a mod m. */
function invMod(a, m) {
  let [old_r, r] = [mod(a, m), m];
  let [old_s, s] = [1n, 0n];
  while (r !== 0n) {
    const q = old_r / r;
    [old_r, r] = [r, old_r - q * r];
    [old_s, s] = [s, old_s - q * s];
  }
  if (old_r !== 1n) throw new Error('not invertible');
  return mod(old_s, m);
}

// Affine point arithmetic on y^2 = x^3 + ax + b. `null` is the point at
// infinity. Slow, and that is fine: clarity matters more than speed in an
// oracle. `curve` defaults to secp256k1.
function pointAdd(a, b, curve = K1) {
  const m = curve.p;
  if (a === null) return b;
  if (b === null) return a;
  if (a.x === b.x && mod(a.y + b.y, m) === 0n) return null;
  let lam;
  if (a.x === b.x && a.y === b.y) {
    lam = mod((3n * a.x * a.x + curve.a) * invMod(2n * a.y, m), m);
  } else {
    lam = mod((b.y - a.y) * invMod(b.x - a.x, m), m);
  }
  const x = mod(lam * lam - a.x - b.x, m);
  const y = mod(lam * (a.x - x) - a.y, m);
  return { x, y };
}

function pointMul(k, point, curve = K1) {
  let acc = null;
  let addend = point;
  let n = k;
  while (n > 0n) {
    if (n & 1n) acc = pointAdd(acc, addend, curve);
    addend = pointAdd(addend, addend, curve);
    n >>= 1n;
  }
  return acc;
}

function onCurve(pt, curve = K1) {
  if (pt === null) return false;
  return mod(pt.y * pt.y - pt.x * pt.x * pt.x - curve.a * pt.x - curve.b, curve.p) === 0n;
}

function bytesToBig(b) {
  let v = 0n;
  for (const byte of b) v = (v << 8n) | BigInt(byte);
  return v;
}

function bigTo32(v) {
  const out = new Uint8Array(32);
  let n = v;
  for (let i = 31; i >= 0; i--) {
    out[i] = Number(n & 0xffn);
    n >>= 8n;
  }
  return out;
}

function hmac(key, ...parts) {
  const h = createHmac('sha256', Buffer.from(key));
  for (const p of parts) h.update(Buffer.from(p));
  return new Uint8Array(h.digest());
}

/**
 * RFC 6979 section 3.2 deterministic k, specialised to secp256k1 + SHA-256
 * (qlen = hlen = 256, so bits2int is a plain big-endian read and needs no
 * truncation). `h1` is the already-hashed message.
 */
function rfc6979k(h1, privBytes, curve = K1) {
  const q = curve.n;
  const x = bytesToBig(privBytes);
  // bits2octets(h1) = int2octets(bits2int(h1) mod q)
  const h1oct = bigTo32(mod(bytesToBig(h1), q));
  const xoct = bigTo32(x);

  let V = new Uint8Array(32).fill(0x01);
  let K = new Uint8Array(32).fill(0x00);
  K = hmac(K, V, Uint8Array.of(0x00), xoct, h1oct);
  V = hmac(K, V);
  K = hmac(K, V, Uint8Array.of(0x01), xoct, h1oct);
  V = hmac(K, V);
  for (;;) {
    V = hmac(K, V);
    const k = bytesToBig(V);
    if (k >= 1n && k < q) return k;
    K = hmac(K, V, Uint8Array.of(0x00));
    V = hmac(K, V);
  }
}

/**
 * Reference ECDSA sign (SEC 1 v2 section 4.1.3) with RFC 6979 k.
 * Returns the RAW (un-normalised) r and s, i.e. exactly what RFC 6979's
 * own test vectors quote. Low-S normalisation is a Bitcoin/BIP-62 policy
 * applied on top, and is asserted separately below.
 */
function refSign(hashBytes, privBytes, curve = K1) {
  const q = curve.n;
  const x = bytesToBig(privBytes);
  const z = mod(bytesToBig(hashBytes), q);
  const k = rfc6979k(hashBytes, privBytes, curve);
  for (;;) {
    const Rp = pointMul(k, curve.G, curve);
    const r = mod(Rp.x, q);
    if (r !== 0n) {
      const s = mod(invMod(k, q) * (z + r * x), q);
      if (s !== 0n) return { r, s, k };
    }
    // RFC 6979 section 3.2 step h: on r == 0 or s == 0, keep generating.
    // Unreachable in practice; present so the oracle is the real algorithm.
    throw new Error('degenerate r/s -- not reachable for these vectors');
  }
}

/** Reference ECDSA verify (SEC 1 v2 section 4.1.4). */
function refVerifyOn(pub, hashBytes, r, s, curve = K1) {
  const q = curve.n;
  if (r <= 0n || r >= q || s <= 0n || s >= q) return false;
  const z = mod(bytesToBig(hashBytes), q);
  const sInv = invMod(s, q);
  const u1 = mod(z * sInv, q);
  const u2 = mod(r * sInv, q);
  const Rp = pointAdd(pointMul(u1, curve.G, curve), pointMul(u2, pub, curve), curve);
  if (Rp === null) return false;
  return mod(Rp.x, q) === r;
}

/** refVerifyOn, fixed to secp256k1 -- the curve under test. */
function refVerify(pub, hashBytes, r, s) {
  return refVerifyOn(pub, hashBytes, r, s, K1);
}

function decompressOrParse(pubBytes) {
  if (pubBytes.length === 65 && pubBytes[0] === 0x04) {
    return { x: bytesToBig(pubBytes.subarray(1, 33)), y: bytesToBig(pubBytes.subarray(33)) };
  }
  if (pubBytes.length === 33 && (pubBytes[0] === 0x02 || pubBytes[0] === 0x03)) {
    const x = bytesToBig(pubBytes.subarray(1));
    // y = sqrt(x^3+7) mod p; p = 3 mod 4 so the root is v^((p+1)/4).
    const alpha = mod(x * x * x + 7n, P);
    let y = powMod(alpha, (P + 1n) / 4n, P);
    if (mod(y, 2n) !== BigInt(pubBytes[0] & 1)) y = mod(P - y, P);
    return { x, y };
  }
  throw new Error('unrecognised public key encoding: ' + hex(pubBytes));
}

function powMod(base, exp, m) {
  let result = 1n;
  let b = mod(base, m);
  let e = exp;
  while (e > 0n) {
    if (e & 1n) result = mod(result * b, m);
    b = mod(b * b, m);
    e >>= 1n;
  }
  return result;
}

// ---------------------------------------------------------------------------
// Wiring to the code under test
// ---------------------------------------------------------------------------

// uap.js's own injection point. noble ships no hash implementation, so this
// is what supplies the HMAC-SHA256 that RFC 6979 needs -- meaning sha256.js's
// hmacSha256 is the function that actually chooses every nonce. Going through
// configureSecp() rather than poking secp.etc by hand keeps both that
// dependency and configureSecp's version-probing under test.
const secp = UAP.configureSecp(secpLib);

/**
 * Sign a bare 32-byte hash, DER-encoded and low-S, for the cases where a
 * published vector fixes the message hash and so it cannot come out of
 * signatureHash(). This is the ONE place that does not go through uap.js --
 * but it is still our code: secp.js's signSync() is the exact call
 * signSpend()/signP2PKHInput() make, DER encoder included.
 */
function signDer(hash, priv) {
  return secp.signSync(hash, priv, { canonical: true, der: true });
}

function getPub(priv, compressed) {
  return Uint8Array.from(secp.getPublicKey(priv, compressed));
}

// ---------------------------------------------------------------------------
// DER structure checking (BIP 66 shape rules)
// ---------------------------------------------------------------------------

/**
 * Strictly parse a DER-encoded ECDSA signature and assert every structural
 * rule, then return { r, s, rBytes, sBytes }. Deliberately hand-rolled and
 * pedantic: the point is to catch encodings that a lenient parser would let
 * through and a Bitcoin-lineage node would reject.
 */
function parseDerStrict(der, label) {
  check(der.length >= 8, `${label}: DER too short (${der.length})`);
  check(der.length <= 72, `${label}: DER too long (${der.length})`);
  eq(der[0], 0x30, `${label}: must start with SEQUENCE tag 0x30`);
  eq(der[1], der.length - 2, `${label}: SEQUENCE length must cover the rest exactly`);
  eq(der[2], 0x02, `${label}: r must be tagged INTEGER 0x02`);
  const lenR = der[3];
  check(lenR > 0, `${label}: r length must be non-zero`);
  check(4 + lenR + 2 <= der.length, `${label}: r length overruns the signature`);
  const rBytes = der.subarray(4, 4 + lenR);
  eq(der[4 + lenR], 0x02, `${label}: s must be tagged INTEGER 0x02`);
  const lenS = der[5 + lenR];
  check(lenS > 0, `${label}: s length must be non-zero`);
  eq(6 + lenR + lenS, der.length, `${label}: r+s lengths must account for all bytes`);
  const sBytes = der.subarray(6 + lenR, 6 + lenR + lenS);

  for (const [name, v] of [['r', rBytes], ['s', sBytes]]) {
    // DER INTEGERs are signed and big-endian: the high bit of the first
    // byte would make the value negative, so a 0x00 pad is required exactly
    // then -- and forbidden otherwise (that is the "unnecessary leading
    // zero" a naive implementation emits).
    eq(v[0] & 0x80, 0, `${label}: ${name} must be encoded as positive (no high bit on first byte)`);
    if (v[0] === 0x00) {
      check(v.length > 1, `${label}: ${name} must not be a bare 0x00`);
      check(
        (v[1] & 0x80) !== 0,
        `${label}: ${name} has an unnecessary leading zero byte (0x${hex(v)})`
      );
    }
    check(v.length <= 33, `${label}: ${name} is longer than 33 bytes`);
  }

  return { r: bytesToBig(rBytes), s: bytesToBig(sBytes), rBytes, sBytes };
}

/** Pull the single push out of a signSpend() scriptSig: <len> <der||hashType>. */
function unwrapSinglePush(scriptSig, label) {
  const len = scriptSig[0];
  check(len < 0x4c, `${label}: expected a direct push opcode, got 0x${len.toString(16)}`);
  eq(scriptSig.length, len + 1, `${label}: scriptSig must be exactly one push`);
  return scriptSig.subarray(1, 1 + len);
}

// ---------------------------------------------------------------------------
// Test fixtures: transactions to sign
// ---------------------------------------------------------------------------

function makeTx(seed) {
  return {
    version: 1,
    locktime: seed & 0xffff,
    vin: [
      {
        txid: hex(sha256(fromUtf8('uap-signing-test-input-' + seed))),
        vout: seed % 7,
        scriptSig: new Uint8Array(0),
        sequence: 0xffffffff,
      },
    ],
    vout: [{ value: 100000 + seed * 13, scriptPubKey: Uint8Array.of(0x51) }],
  };
}

function privFor(seed) {
  // Any 32 bytes in [1, n) will do; SHA-256 of a label gives us a spread of
  // keys without pulling in randomness (these tests must be reproducible).
  const p = sha256(fromUtf8('uap-signing-test-key-' + seed));
  const v = mod(bytesToBig(p), N - 1n) + 1n;
  return bigTo32(v);
}

// ===========================================================================
// 1. Reference implementation pinned to published constants
// ===========================================================================
{
  // Source: SEC 2 v2 section 2.4.1 -- the secp256k1 generator G.
  const g = pointMul(1n, G);
  check(onCurve(G), 'G must satisfy y^2 = x^3 + 7 mod p');
  eq(g.x.toString(16), GX.toString(16), 'reference 1*G x');

  // Source: the standard small multiples of G, reproduced identically in
  // many independent references (e.g. the "secp256k1 base point multiples"
  // tables shipped with libsecp256k1 test data, Bitcoin StackExchange
  // canonical answers, and the Bitcoin wiki).
  const G2 = {
    x: 0xc6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5n,
    y: 0x1ae168fea63dc339a3c58419466ceaeef7f632653266d0e1236431a950cfe52an,
  };
  const G3 = {
    x: 0xf9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9n,
    y: 0x388f7b0f632de8140fe337e62a37f3566500a99934c2231b6cb9fd7584b8e672n,
  };
  const c2 = pointMul(2n, G);
  const c3 = pointMul(3n, G);
  eq(c2.x.toString(16), G2.x.toString(16), 'reference 2*G x matches published value');
  eq(c2.y.toString(16), G2.y.toString(16), 'reference 2*G y matches published value');
  eq(c3.x.toString(16), G3.x.toString(16), 'reference 3*G x matches published value');
  eq(c3.y.toString(16), G3.y.toString(16), 'reference 3*G y matches published value');
  check(onCurve(c2) && onCurve(c3), '2G and 3G must be on the curve');

  // n*G = infinity, the defining property of the group order.
  eq(pointMul(N, G), null, 'n*G must be the point at infinity');
}

// ===========================================================================
// 2. Public key derivation against published base-point multiples
// ===========================================================================
{
  // Source: as above -- k*G for k = 1, 2, 3.
  const expected = [
    {
      k: 1,
      x: '79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798',
      y: '483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8',
    },
    {
      k: 2,
      x: 'c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5',
      y: '1ae168fea63dc339a3c58419466ceaeef7f632653266d0e1236431a950cfe52a',
    },
    {
      k: 3,
      x: 'f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9',
      y: '388f7b0f632de8140fe337e62a37f3566500a99934c2231b6cb9fd7584b8e672',
    },
  ];

  for (const v of expected) {
    const priv = bigTo32(BigInt(v.k));

    const unc = getPub(priv, false);
    eq(unc.length, 65, `k=${v.k}: uncompressed public key must be 65 bytes`);
    eq(unc[0], 0x04, `k=${v.k}: uncompressed public key must start with 0x04`);
    eq(hex(unc.subarray(1, 33)), v.x, `k=${v.k}: uncompressed X`);
    eq(hex(unc.subarray(33)), v.y, `k=${v.k}: uncompressed Y`);

    const comp = getPub(priv, true);
    eq(comp.length, 33, `k=${v.k}: compressed public key must be 33 bytes`);
    eq(hex(comp.subarray(1)), v.x, `k=${v.k}: compressed X`);

    // Prefix encodes the parity of Y: 0x02 even, 0x03 odd (SEC 1 v2, 2.3.3).
    const yOdd = BigInt('0x' + v.y) & 1n;
    eq(comp[0], yOdd === 1n ? 0x03 : 0x02, `k=${v.k}: compressed prefix must encode Y parity`);
  }

  // Both parities must actually occur, otherwise the parity assertion is
  // vacuous. Which small multiple has odd Y is not something to hard-code
  // from memory, so ask the reference implementation (already pinned to the
  // published constants above) and assert the derivation agrees.
  let sawEven = 0;
  let sawOdd = 0;
  for (let k = 1n; k <= 12n; k++) {
    const pt = pointMul(k, G);
    const prefix = getPub(bigTo32(k), true)[0];
    if (pt.y & 1n) {
      sawOdd++;
      eq(prefix, 0x03, `${k}*G has odd Y -> prefix must be 0x03`);
    } else {
      sawEven++;
      eq(prefix, 0x02, `${k}*G has even Y -> prefix must be 0x02`);
    }
  }
  check(sawEven > 0 && sawOdd > 0, 'both compressed prefixes must occur in k=1..12');

  // And a spread of arbitrary keys, cross-checked against the reference
  // scalar multiplication rather than against any stored value.
  for (let i = 0; i < 8; i++) {
    const priv = privFor(i);
    const expectedPt = pointMul(bytesToBig(priv), G);
    const unc = getPub(priv, false);
    const comp = getPub(priv, true);
    eq(hex(unc.subarray(1, 33)), hex(bigTo32(expectedPt.x)), `key ${i}: derived X`);
    eq(hex(unc.subarray(33)), hex(bigTo32(expectedPt.y)), `key ${i}: derived Y`);
    eq(comp[0], expectedPt.y & 1n ? 0x03 : 0x02, `key ${i}: compressed prefix parity`);
    eq(hex(comp.subarray(1)), hex(unc.subarray(1, 33)), `key ${i}: compressed X matches uncompressed X`);
  }
}

// ===========================================================================
// 3a. HMAC-SHA256, the primitive RFC 6979's k derivation is built from.
//
// configureSecp() hands sha256.js's hmacSha256 to the EC library, so under
// @noble/secp256k1 v1 THIS function is what chooses k. A broken HMAC would
// still produce signatures that verify (any k works), so nothing else in the
// suite would notice -- but the nonces would no longer be the RFC 6979 ones,
// and could be biased or repeated. Hence a published known-answer test.
//
// Source: RFC 4231 section 4, "Test Vectors" (HMAC-SHA-256 column).
// Each vector is additionally cross-checked against node:crypto, so a
// mis-transcribed expectation shows up as a disagreement rather than as a
// silent pass.
// ===========================================================================
{
  const vectors = [
    {
      name: 'RFC 4231 Test Case 1',
      key: fromHex('0b'.repeat(20)),
      data: fromUtf8('Hi There'),
      mac: 'b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7',
    },
    {
      name: 'RFC 4231 Test Case 2',
      key: fromUtf8('Jefe'),
      data: fromUtf8('what do ya want for nothing?'),
      mac: '5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843',
    },
    {
      name: 'RFC 4231 Test Case 3',
      key: fromHex('aa'.repeat(20)),
      data: fromHex('dd'.repeat(50)),
      mac: '773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe',
    },
    {
      name: 'RFC 4231 Test Case 4',
      key: fromHex('0102030405060708090a0b0c0d0e0f10111213141516171819'),
      data: fromHex('cd'.repeat(50)),
      mac: '82558a389a443c0ea4cc819899f2083a85f0faa3e578f8077a2e3ff46729665b',
    },
  ];

  for (const v of vectors) {
    eq(hex(hmacSha256(v.key, v.data)), v.mac, `${v.name}: sha256.js hmacSha256`);
    eq(hex(hmac(v.key, v.data)), v.mac, `${v.name}: node:crypto cross-check`);
    // RFC 6979 feeds the message in several pieces; the variadic form must
    // be equivalent to concatenating first.
    const mid = Math.floor(v.data.length / 2);
    eq(
      hex(hmacSha256(v.key, v.data.subarray(0, mid), v.data.subarray(mid))),
      v.mac,
      `${v.name}: variadic hmacSha256 equals the concatenated form`
    );
  }
}

// ===========================================================================
// 3b. RFC 6979 deterministic k and ECDSA -- published known answers.
//
// Source: RFC 6979 "Deterministic Usage of DSA and ECDSA", Appendix A.2.5,
// "ECDSA, 256 Bits (Prime Field)" -- which is the NIST P-256 curve. RFC 6979
// publishes NO secp256k1 vectors; the widely-circulated "RFC 6979 secp256k1"
// values are not in the document. So these vectors are used for what they
// genuinely pin down: the deterministic-k construction and the ECDSA
// signing equations in the reference implementation above, on the curve they
// were published for. Once that reference is anchored, it serves as the
// oracle for the secp256k1 signatures uap.js produces.
// ===========================================================================
{
  const PRIV = fromHex('c9afa9d845ba75166b5c215767b1d6934e50c3db36e89b127b8a622b120f6721');
  const PUB_X = '60fed4ba255a9d31c961eb74c6356d68c049b8923b61fa6ce669622e60f29fb6';
  const PUB_Y = '7903fe1008b8bc99a41ae9e95628bc64f2f1b20c2d7e9f5177a3c294d4462299';

  // The public key RFC 6979 A.2.5 states for this private key -- which also
  // confirms the P-256 curve parameters transcribed above.
  const U = pointMul(bytesToBig(PRIV), P256.G, P256);
  check(onCurve(U, P256), 'RFC 6979 A.2.5: U must be on P-256');
  eq(hex(bigTo32(U.x)), PUB_X, 'RFC 6979 A.2.5: public key Ux');
  eq(hex(bigTo32(U.y)), PUB_Y, 'RFC 6979 A.2.5: public key Uy');
  eq(pointMul(P256.n, P256.G, P256), null, 'P-256: n*G must be the point at infinity');

  // A.2.5, "With SHA-256" entries.
  const vectors = [
    {
      msg: 'sample',
      k: 'a6e3c57dd01abe90086538398355dd4c3b17aa873382b0f24d6129493d8aad60',
      r: 'efd48b2aacb6a8fd1140dd9cd45e81d69d2c877b56aaf991c34d0ea84eaf3716',
      s: 'f7cb1c942d657c41d436c7a1b6e29f65f3e900dbb9aff4064dc4ab2f843acda8',
    },
    {
      msg: 'test',
      k: 'd16b6ae827f17175e040871a1c7ec3500192c4c92677336ec2537acaee0008e0',
      r: 'f1abb023518351cd71d881567b1ea663ed3efcf6c5132b354f28d3b0b7d38367',
      s: '019f4113742a2b14bd25926b49c649155f267e60d3814b4c0cc84250e46f0083',
    },
  ];

  for (const v of vectors) {
    const h = sha256(fromUtf8(v.msg));
    const k = rfc6979k(h, PRIV, P256);
    eq(hex(bigTo32(k)), v.k, `RFC 6979 A.2.5 "${v.msg}": deterministic k`);
    const ref = refSign(h, PRIV, P256);
    eq(hex(bigTo32(ref.r)), v.r, `RFC 6979 A.2.5 "${v.msg}": r`);
    eq(hex(bigTo32(ref.s)), v.s, `RFC 6979 A.2.5 "${v.msg}": s`);
    check(
      refVerifyOn(U, h, ref.r, ref.s, P256),
      `RFC 6979 A.2.5 "${v.msg}": reference verify accepts the published signature`
    );
  }

  // Same construction, same private key, driven by sha256.js's HMAC rather
  // than node's: the k the library will actually use must be the RFC's.
  // (rfc6979k above uses node:crypto; this repeats it through sha256.js.)
  for (const v of vectors) {
    const h = sha256(fromUtf8(v.msg));
    const q = P256.n;
    const h1oct = bigTo32(mod(bytesToBig(h), q));
    const xoct = bigTo32(bytesToBig(PRIV));
    let V = new Uint8Array(32).fill(0x01);
    let K = new Uint8Array(32).fill(0x00);
    K = hmacSha256(K, V, Uint8Array.of(0x00), xoct, h1oct);
    V = hmacSha256(K, V);
    K = hmacSha256(K, V, Uint8Array.of(0x01), xoct, h1oct);
    V = hmacSha256(K, V);
    V = hmacSha256(K, V);
    eq(hex(V), v.k, `RFC 6979 A.2.5 "${v.msg}": k via sha256.js hmacSha256`);
  }
}

// ===========================================================================
// 3c. Determinism and low-S on secp256k1, at the library's own entry point.
//
// No published secp256k1 known-answer is used here (see 3b); correctness of
// r and s is established against the reference implementation, and
// independently by verifying the signature.
// ===========================================================================
{
  const PRIV = fromHex('c9afa9d845ba75166b5c215767b1d6934e50c3db36e89b127b8a622b120f6721');
  const h = sha256(fromUtf8('sample'));

  const a = signDer(h, PRIV);
  const b = signDer(h, PRIV);
  eq(hex(a), hex(b), 'RFC 6979 determinism: repeated signing must be byte-identical');

  const got = parseDerStrict(a, 'secp256k1 deterministic');
  const ref = refSign(h, PRIV, K1);
  const refLowS = ref.s > N_HALF ? N - ref.s : ref.s;
  eq(hex(bigTo32(got.r)), hex(bigTo32(ref.r)), 'secp256k1: r matches the reference');
  eq(hex(bigTo32(got.s)), hex(bigTo32(refLowS)), 'secp256k1: s matches the reference (low-S)');
  check(got.s <= N_HALF, 'secp256k1: s must be low-S');

  const pub = decompressOrParse(getPub(PRIV, true));
  check(refVerify(pub, h, got.r, got.s), 'secp256k1: signature must verify');

  // A different message must give a different signature -- otherwise
  // "deterministic" above would be satisfied by a constant.
  const other = signDer(sha256(fromUtf8('sample2')), PRIV);
  check(hex(other) !== hex(a), 'a different message must give a different signature');
}

// ===========================================================================
// 4. uap.js's own signing helpers: signSpend / signP2PKHInput
//
// Values here cannot come from a published vector -- the message hash is a
// transaction sighash. They are checked against the reference implementation
// (deterministic k from the spec, r/s from spec ECDSA) and by independent
// signature verification, neither of which shares code with uap.js.
// ===========================================================================

let shortR = 0;
let shortS = 0;
let highBitR = 0;
let highBitS = 0;
let signaturesChecked = 0;

for (let i = 0; i < 24; i++) {
  const priv = privFor(i);
  const pub = getPub(priv, true);
  const pubPoint = decompressOrParse(pub);
  const origin = new Uint8Array(32).fill(0x99); // dummy origin for signing tests
  const scriptCode = UAP.buildTransferScript(pub, 500 + i, origin);
  const tx = makeTx(i);
  const hashType = [UAP.SIGHASH_ALL, UAP.SIGHASH_NONE, UAP.SIGHASH_ALL | UAP.SIGHASH_ANYONECANPAY][i % 3];

  const scriptSig = UAP.signSpend(secp, scriptCode, priv, tx, 0, hashType);
  const push = unwrapSinglePush(scriptSig, `signSpend#${i}`);

  // 5. The sighash byte: last byte of the pushed blob, and the DER portion
  //    must account for everything before it.
  eq(push[push.length - 1], hashType, `signSpend#${i}: trailing sighash byte must equal hashType`);
  const der = push.subarray(0, push.length - 1);
  eq(der[1], der.length - 2, `signSpend#${i}: DER length must not include the sighash byte`);

  // 4. DER structure.
  const parsed = parseDerStrict(der, `signSpend#${i}`);

  // 3. Low-S / BIP 62.
  check(parsed.s >= 1n && parsed.s <= N_HALF, `signSpend#${i}: s must be in [1, n/2]`);
  check(parsed.r >= 1n && parsed.r < N, `signSpend#${i}: r must be in [1, n)`);

  // Cross-check against the reference implementation over the same sighash.
  const msgHash = UAP.signatureHash(scriptCode, tx, 0, hashType);
  const ref = refSign(msgHash, priv);
  const refLowS = ref.s > N_HALF ? N - ref.s : ref.s;
  eq(hex(bigTo32(parsed.r)), hex(bigTo32(ref.r)), `signSpend#${i}: r matches reference RFC 6979 ECDSA`);
  eq(hex(bigTo32(parsed.s)), hex(bigTo32(refLowS)), `signSpend#${i}: s matches reference (low-S)`);

  // Independent verification against the derived public key.
  check(refVerify(pubPoint, msgHash, parsed.r, parsed.s), `signSpend#${i}: signature must verify`);
  check(
    !refVerify(pubPoint, msgHash, parsed.r, mod(parsed.s + 1n, N)),
    `signSpend#${i}: a tampered s must NOT verify`
  );

  // Determinism through the library entry point.
  const again = UAP.signSpend(secp, scriptCode, priv, tx, 0, hashType);
  eq(hex(again), hex(scriptSig), `signSpend#${i}: must be deterministic`);

  if (parsed.rBytes.length < 32) shortR++;
  if (parsed.sBytes.length < 32) shortS++;
  if (parsed.rBytes[0] === 0x00) highBitR++;
  if (parsed.sBytes[0] === 0x00) highBitS++;
  signaturesChecked++;
}

check(signaturesChecked === 24, 'expected 24 signSpend signatures to be checked');

// signP2PKHInput: two pushes, <sig+hashType> <pubkey>.
for (let i = 0; i < 6; i++) {
  const priv = privFor(100 + i);
  const pub = getPub(priv, true);
  const pubPoint = decompressOrParse(pub);
  const origin = new Uint8Array(32).fill(0x99); // dummy origin for signing tests
  const scriptCode = UAP.buildTransferScript(pub, 1000 + i, origin);
  const tx = makeTx(200 + i);
  const hashType = UAP.SIGHASH_ALL;

  const scriptSig = UAP.signP2PKHInput(secp, scriptCode, priv, pub, tx, 0, hashType);
  const sigLen = scriptSig[0];
  check(sigLen < 0x4c, `signP2PKHInput#${i}: signature push must be a direct push`);
  const sigPush = scriptSig.subarray(1, 1 + sigLen);
  const rest = scriptSig.subarray(1 + sigLen);
  eq(rest[0], pub.length, `signP2PKHInput#${i}: second push must be the pubkey length`);
  eq(hex(rest.subarray(1)), hex(pub), `signP2PKHInput#${i}: second push must be the pubkey`);
  eq(scriptSig.length, 1 + sigLen + 1 + pub.length, `signP2PKHInput#${i}: exactly two pushes`);

  eq(sigPush[sigPush.length - 1], hashType, `signP2PKHInput#${i}: trailing sighash byte`);
  const parsed = parseDerStrict(sigPush.subarray(0, sigPush.length - 1), `signP2PKHInput#${i}`);
  check(parsed.s <= N_HALF, `signP2PKHInput#${i}: s must be low-S`);

  const msgHash = UAP.signatureHash(scriptCode, tx, 0, hashType);
  check(refVerify(pubPoint, msgHash, parsed.r, parsed.s), `signP2PKHInput#${i}: signature must verify`);
  signaturesChecked++;
}

// Every sighash type uap.js exposes must land in the trailing byte verbatim.
{
  const priv = privFor(7);
  const pub = getPub(priv, true);
  const origin = new Uint8Array(32).fill(0x99); // dummy origin for signing tests
  const scriptCode = UAP.buildTransferScript(pub, 42, origin);
  const tx = makeTx(7);
  for (const hashType of [
    UAP.SIGHASH_ALL,
    UAP.SIGHASH_NONE,
    UAP.SIGHASH_SINGLE,
    UAP.SIGHASH_ALL | UAP.SIGHASH_ANYONECANPAY,
    UAP.SIGHASH_NONE | UAP.SIGHASH_ANYONECANPAY,
    UAP.SIGHASH_SINGLE | UAP.SIGHASH_ANYONECANPAY,
  ]) {
    const push = unwrapSinglePush(
      UAP.signSpend(secp, scriptCode, priv, tx, 0, hashType),
      `sighash byte 0x${hashType.toString(16)}`
    );
    eq(push[push.length - 1], hashType, `sighash byte must be 0x${hashType.toString(16)}`);
    parseDerStrict(push.subarray(0, push.length - 1), `sighash 0x${hashType.toString(16)}`);
  }

  // The default (hashType omitted) must be SIGHASH_ALL.
  const dflt = unwrapSinglePush(UAP.signSpend(secp, scriptCode, priv, tx, 0), 'default hashType');
  eq(dflt[dflt.length - 1], UAP.SIGHASH_ALL, 'omitted hashType must default to SIGHASH_ALL');
}

// ===========================================================================
// 4b. DER edge encodings: short integers and high-bit integers.
//
// A 31-byte-or-shorter r or s appears in roughly 1 signature in 128; the
// 24 above will not reliably contain one. Search deterministically until
// both edge shapes have been seen, so the strict parser is exercised on the
// encodings that break naive implementations.
// ===========================================================================
{
  const priv = privFor(999);
  const pub = getPub(priv, true);
  const origin = new Uint8Array(32).fill(0x99); // dummy origin for signing tests
  const scriptCode = UAP.buildTransferScript(pub, 7, origin);

  const found = {
    shortR: null,   // r encoded in fewer than 32 bytes
    shortS: null,   // s encoded in fewer than 32 bytes
    paddedR: null,  // r whose top byte has the high bit set -> 0x00 pad
    paddedS: null,  // ditto for s
    fullPadR: null, // the 33-byte case: a full 32-byte r with the high bit set
  };
  let attempts = 0;

  for (let i = 0; i < 4000; i++) {
    attempts++;
    const tx = makeTx(1000 + i);
    const push = unwrapSinglePush(
      UAP.signSpend(secp, scriptCode, priv, tx, 0, UAP.SIGHASH_ALL),
      `edge-search#${i}`
    );
    const der = push.subarray(0, push.length - 1);
    const p = parseDerStrict(der, `edge-search#${i}`);
    const rec = { i, der, p };
    if (!found.shortR && p.rBytes.length < 32) found.shortR = rec;
    if (!found.shortS && p.sBytes.length < 32) found.shortS = rec;
    if (!found.paddedR && p.rBytes[0] === 0x00) found.paddedR = rec;
    if (!found.paddedS && p.sBytes[0] === 0x00) found.paddedS = rec;
    if (!found.fullPadR && p.rBytes.length === 33) found.fullPadR = rec;
    if (Object.values(found).every(Boolean)) break;
  }

  for (const [name, rec] of Object.entries(found)) {
    check(rec !== null, `edge search: never produced case "${name}" in ${attempts} signatures`);
  }

  // Padded case: a 0x00 lead byte must be present exactly because the next
  // byte has its high bit set, and the total length must be one more than
  // the value's own significant-byte count.
  for (const [name, rec, which] of [
    ['r', found.paddedR, 'rBytes'],
    ['s', found.paddedS, 'sBytes'],
    ['r(33-byte)', found.fullPadR, 'rBytes'],
  ]) {
    const v = rec.p[which];
    eq(v[0], 0x00, `padded ${name}: must carry a single 0x00 pad`);
    check((v[1] & 0x80) !== 0, `padded ${name}: pad must be justified by a set high bit`);
    check(v[1] !== 0x00, `padded ${name}: must not carry two pad bytes`);
    const value = rec.p[name.startsWith('r') ? 'r' : 's'];
    check(
      value >= 1n << BigInt(8 * (v.length - 1) - 8) && value < 1n << BigInt(8 * (v.length - 1)),
      `padded ${name}: value must occupy exactly ${v.length - 1} significant bytes`
    );
  }
  eq(found.fullPadR.p.rBytes.length, 33, 'the 33-byte r case must really be 33 bytes');

  // s can never need the 33-byte form: low-S caps it at n/2 < 2^255, so its
  // top byte can never have the high bit set with 32 significant bytes.
  check(
    found.paddedS.p.sBytes.length <= 32,
    's must never require a 33-byte DER INTEGER (low-S caps it below 2^255)'
  );

  // Short case: fewer than 32 bytes means the value really is small, and no
  // leading zero may be present (an unnecessary pad is exactly the bug).
  for (const [name, rec, which] of [
    ['r', found.shortR, 'rBytes'],
    ['s', found.shortS, 'sBytes'],
  ]) {
    const v = rec.p[which];
    check(v.length < 32, `short ${name}: INTEGER must be under 32 bytes`);
    check(v[0] !== 0x00, `short ${name}: must not be zero-padded`);
    const value = rec.p[name];
    check(value < 1n << BigInt(8 * v.length), `short ${name}: value must fit its declared length`);
    check(
      value >= 1n << BigInt(8 * (v.length - 1)),
      `short ${name}: encoding must be minimal (top byte non-zero)`
    );
    // A short integer shrinks the whole signature; the SEQUENCE length must
    // have followed it down rather than staying at the usual 0x45/0x46.
    eq(rec.der[1], rec.der.length - 2, `short ${name}: SEQUENCE length must track the shrunk body`);
  }

  // Also verify the edge-case signatures actually verify -- a truncated
  // r or s that merely *looks* short would fail here.
  const pubPoint = decompressOrParse(pub);
  for (const rec of new Set(Object.values(found))) {
    const msgHash = UAP.signatureHash(scriptCode, makeTx(1000 + rec.i), 0, UAP.SIGHASH_ALL);
    check(refVerify(pubPoint, msgHash, rec.p.r, rec.p.s), `edge-case signature #${rec.i} must verify`);
    check(rec.p.s <= N_HALF, `edge-case signature #${rec.i} must be low-S`);
  }
}

// ===========================================================================
// 3b. Low-S over a wide range, asserted on its own.
//
// High-S is non-standard on Bitcoin-lineage chains (BIP 62) and a node will
// reject it, so this must hold for every key/message pair, not just one.
// Roughly half of all raw ECDSA signatures are high-S, so a path that failed
// to normalise would be caught within a handful of iterations.
// ===========================================================================
{
  let normalisationsObserved = 0;
  for (let i = 0; i < 40; i++) {
    const priv = privFor(500 + i);
    const pub = getPub(priv, true);
    const origin = new Uint8Array(32).fill(0x99); // dummy origin for signing tests
    const scriptCode = UAP.buildTransferScript(pub, 1 + i, origin);
    const tx = makeTx(500 + i);
    const push = unwrapSinglePush(UAP.signSpend(secp, scriptCode, priv, tx, 0), `lowS#${i}`);
    const p = parseDerStrict(push.subarray(0, push.length - 1), `lowS#${i}`);
    check(p.s <= N_HALF, `lowS#${i}: s (0x${p.s.toString(16)}) must be <= n/2`);

    // Count how often normalisation actually had to fire, so we know the
    // assertion above is not passing by luck.
    const rawS = refSign(UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_ALL), priv).s;
    if (rawS > N_HALF) normalisationsObserved++;
  }
  check(
    normalisationsObserved > 5,
    `expected the low-S normalisation to fire often; it fired ${normalisationsObserved}/40 times`
  );
}

// ===========================================================================
// 6. signCancelOrder / cancelMessage -- the cancellation-authorization fix.
//
// Cancelling an order used to be "authorized" by echoing back the order's
// own script_sig, which GET /orders publishes to every visitor -- not a
// real credential. A cancellation is now a real ECDSA signature, checked
// the same way signSpend is checked above: DER structure, low-S,
// determinism, and independent verification against the reference
// implementation, plus (since what's actually novel here is the message,
// not the signing primitive) that the message really binds every field it
// claims to -- changing any one of txid/vout/scriptSig/cancelNonce must
// change both the message and the hash that gets signed. See
// contrib/uap-indexer/cancel_auth.go for the scheme this must match
// byte-for-byte.
// ===========================================================================
{
  let cancelSigsChecked = 0;
  for (let i = 0; i < 12; i++) {
    const priv = privFor(1000 + i);
    const pub = getPub(priv, true);
    const pubPoint = decompressOrParse(pub);
    const txid = 'a'.repeat(63) + (i % 16).toString(16);
    const vout = i;
    const scriptSig = hex(Uint8Array.from([0x30, 0x06, 0x02, 0x01, i + 1, 0x02, 0x01, i + 2, 0x83]));
    // A nanosecond-scale value, deliberately past Number.MAX_SAFE_INTEGER,
    // carried only as a string -- exactly the shape orders.go's
    // `cancel_nonce,string` field sends.
    const cancelNonce = String(1700000000000000000n + BigInt(i));

    const sigHex = UAP.signCancelOrder(secp, { txid, vout, scriptSig, cancelNonce, privKey: priv });
    const der = fromHex(sigHex);
    const parsed = parseDerStrict(der, `signCancelOrder#${i}`);

    check(parsed.s >= 1n && parsed.s <= N_HALF, `signCancelOrder#${i}: s must be in [1, n/2]`);
    check(parsed.r >= 1n && parsed.r < N, `signCancelOrder#${i}: r must be in [1, n)`);

    const msgHash = hash256(UAP.cancelMessage(txid, vout, scriptSig, cancelNonce));
    const ref = refSign(msgHash, priv);
    const refLowS = ref.s > N_HALF ? N - ref.s : ref.s;
    eq(hex(bigTo32(parsed.r)), hex(bigTo32(ref.r)), `signCancelOrder#${i}: r matches reference RFC 6979 ECDSA`);
    eq(hex(bigTo32(parsed.s)), hex(bigTo32(refLowS)), `signCancelOrder#${i}: s matches reference (low-S)`);

    check(refVerify(pubPoint, msgHash, parsed.r, parsed.s), `signCancelOrder#${i}: signature must verify`);
    check(
      !refVerify(pubPoint, msgHash, parsed.r, mod(parsed.s + 1n, N)),
      `signCancelOrder#${i}: a tampered s must NOT verify`
    );

    // Binding: perturbing any one bound field must change the message, and
    // therefore the hash actually signed -- otherwise a signature captured
    // for one order/state could verify against another.
    const base = UAP.cancelMessage(txid, vout, scriptSig, cancelNonce);
    const variants = {
      txid: UAP.cancelMessage('b'.repeat(64), vout, scriptSig, cancelNonce),
      vout: UAP.cancelMessage(txid, vout + 1, scriptSig, cancelNonce),
      scriptSig: UAP.cancelMessage(txid, vout, scriptSig.slice(0, -2) + 'ff', cancelNonce),
      cancelNonce: UAP.cancelMessage(txid, vout, scriptSig, String(BigInt(cancelNonce) + 1n)),
    };
    for (const [field, variant] of Object.entries(variants)) {
      check(hex(variant) !== hex(base), `signCancelOrder#${i}: perturbing ${field} must change the message`);
      check(
        hex(hash256(variant)) !== hex(hash256(base)),
        `signCancelOrder#${i}: perturbing ${field} must change the signed hash`
      );
    }

    // Determinism through the library entry point.
    const again = UAP.signCancelOrder(secp, { txid, vout, scriptSig, cancelNonce, privKey: priv });
    eq(again, sigHex, `signCancelOrder#${i}: must be deterministic`);

    cancelSigsChecked++;
  }
  check(cancelSigsChecked === 12, 'expected 12 signCancelOrder signatures to be checked');

  // A Number cancelNonce must be rejected outright rather than silently
  // stringified -- silently accepting one is exactly the interop bug the
  // `,string` JSON tag on the Go side exists to prevent (a nanosecond
  // UnixNano value loses precision as a JS double once it exceeds 2^53,
  // which happened well before this code was written).
  assert.throws(
    () => UAP.cancelMessage('a'.repeat(64), 0, 'aa', 12345),
    /cancelNonce must be/,
    'cancelMessage must reject a numeric cancelNonce rather than silently coercing it'
  );
}

// ===========================================================================
// Done
// ===========================================================================

console.log(`signing.test.js: OK -- ${assertions} assertions passed`);
console.log(
  `  (${signaturesChecked} signatures produced through uap.js helpers and independently verified)`
);
