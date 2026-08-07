// secp.js -- the secp256k1 entry point for everything in this repo.
//
// Import this, never '@noble/secp256k1' and never './noble-secp256k1.js'
// directly:
//
//   import * as secp from '../uap-js/secp.js';
//
// It exists for two reasons.
//
// 1. Bare specifiers don't work in a browser. The frontend is served as plain
//    ES modules by uap-indexer under a `script-src 'self'` CSP, so there is no
//    node_modules resolution and no way to add an inline import map. Every
//    specifier has to be a relative path to a file that was actually copied
//    into web/. ./noble-secp256k1.js is that file (see its header).
//
// 2. @noble/secp256k1 v2 dropped DER. v1's `signSync(hash, priv, {canonical:
//    true, der: true})` returned a DER-encoded signature; v2 renamed the
//    function to `sign()`, returns a Signature object, and only knows how to
//    emit the 64-byte compact form -- it even throws if the legacy `der` /
//    `canonical` options are so much as present. Whippet scriptSigs carry DER,
//    like every other Bitcoin-lineage chain, so the encoding lives here.
//
// The exported `signSync` keeps v1's exact signature and output, which is what
// uap.js's signSpend/signP2PKHInput call. secp-equivalence.test.js pins those
// bytes against a fixture generated with v1.7.1.
//
// Signing still requires an HMAC-SHA256 implementation to be injected (noble
// deliberately ships none, to stay environment-agnostic): call
// UAP.configureSecp(secp) once before signing. It wires up etc.hmacSha256Sync.

import * as noble from './noble-secp256k1.js';

export const {
  CURVE,
  Point,
  ProjectivePoint,
  Signature,
  etc,
  getPublicKey,
  getSharedSecret,
  sign,
  signAsync,
  utils,
  verify,
} = noble;

/**
 * DER-encode one of a signature's two integers: minimal big-endian bytes,
 * with a leading 0x00 when the high bit would otherwise read as negative.
 */
function derInt(value) {
  let hex = value.toString(16);
  if (hex.length % 2) hex = '0' + hex;
  const bytes = [];
  for (let i = 0; i < hex.length; i += 2) bytes.push(parseInt(hex.slice(i, i + 2), 16));
  while (bytes.length > 1 && bytes[0] === 0x00 && (bytes[1] & 0x80) === 0) bytes.shift();
  if (bytes[0] & 0x80) bytes.unshift(0x00);
  return Uint8Array.from([0x02, bytes.length, ...bytes]);
}

/** DER-encode a v2 Signature as SEQUENCE { INTEGER r, INTEGER s }. */
function toDER(sig) {
  const r = derInt(sig.r);
  const s = derInt(sig.s);
  const body = new Uint8Array(r.length + s.length);
  body.set(r, 0);
  body.set(s, r.length);
  const out = new Uint8Array(2 + body.length);
  out[0] = 0x30;
  out[1] = body.length;
  out.set(body, 2);
  return out;
}

/**
 * Synchronous RFC6979 ECDSA signing with @noble/secp256k1 v1's API and output.
 *
 * @param {Uint8Array} msgHash 32-byte message hash (not the message)
 * @param {Uint8Array} privKey 32-byte private key
 * @param {Object} [opts] `der` (default true) selects DER over 64-byte compact;
 *   `canonical` (default true) is v1's name for low-S normalisation, and maps
 *   to v2's `lowS`. `extraEntropy` is passed through.
 * @returns {Uint8Array} DER-encoded (or compact) signature
 */
export function signSync(msgHash, privKey, opts) {
  const o = opts || {};
  // v2 rejects the v1 option names outright, so translate rather than forward.
  const lowS = o.lowS !== undefined ? o.lowS : o.canonical !== undefined ? o.canonical : true;
  const nobleOpts = { lowS };
  if (o.extraEntropy !== undefined) nobleOpts.extraEntropy = o.extraEntropy;
  const sig = noble.sign(msgHash, privKey, nobleOpts);
  return o.der === false ? sig.toBytes() : toDER(sig);
}

export { toDER as signatureToDER };
