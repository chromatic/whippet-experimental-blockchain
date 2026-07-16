// uap.js -- build and sign OP_MINT / OP_MINT_TRANSFER transactions in
// plain JavaScript, for use in a browser or Node. No build step required.
//
// This library does NOT talk to the network. It only builds scripts,
// serializes transactions, and signs them; you supply UTXO details
// (e.g. from a uap-indexer /positions query and your node's listunspent)
// and are responsible for broadcasting the resulting hex yourself (e.g.
// via your own backend calling sendrawtransaction, or a proxy you trust
// -- never expose raw node RPC directly to a public website).
//
// Elliptic-curve signing is delegated to the caller: pass in a `secp`
// object exposing `getPublicKey(privKey, compressed)` and
// `signSync(msgHash, privKey, opts)` returning a DER-encoded signature,
// matching the API of @noble/secp256k1 v1.x. This keeps uap.js itself
// dependency-free and lets you choose/audit/pin your own EC library.
//
// SECURITY: never send a private key to a server you don't control, and
// never paste a real private key into a demo page. Signing must happen
// wherever the key lives (ideally the user's own device).

import { hash256, hmacSha256 } from './sha256.js';

/**
 * @noble/secp256k1 v1.x's synchronous signSync() requires the caller to
 * supply an HMAC-SHA256 implementation (it doesn't bundle one, to stay
 * environment-agnostic). Call this once with your secp256k1 module
 * before using signSpend/buildTransferTx.
 */
function configureSecp(secp) {
  secp.utils.hmacSha256Sync = (key, ...msgs) => hmacSha256(key, ...msgs);
  return secp;
}

// ---- opcodes (must match src/script/script.h) ----
const OP_MINT = 0xb5;
const OP_MINT_TRANSFER = 0xba;
const OP_PUSHDATA1 = 0x4c;
const OP_PUSHDATA2 = 0x4d;
const OP_0 = 0x00;
const OP_1NEGATE = 0x4f;
const OP_1 = 0x51;

const SIGHASH_ALL = 1;
const COIN = 100000000;

// ---- byte helpers ----

function hexToBytes(hex) {
  if (hex.length % 2 !== 0) throw new Error('invalid hex string');
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.substr(i * 2, 2), 16);
  }
  return out;
}

function bytesToHex(bytes) {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
}

function concatBytes(...arrays) {
  let len = 0;
  for (const a of arrays) len += a.length;
  const out = new Uint8Array(len);
  let offset = 0;
  for (const a of arrays) {
    out.set(a, offset);
    offset += a.length;
  }
  return out;
}

function reverseBytes(bytes) {
  return Uint8Array.from(bytes).reverse();
}

// ---- script building ----

/** Minimal-push encoding of arbitrary data, matching CScript::operator<<(vector<uchar>). */
function pushData(data) {
  const n = data.length;
  if (n < OP_PUSHDATA1) {
    return concatBytes(Uint8Array.of(n), data);
  } else if (n <= 0xff) {
    return concatBytes(Uint8Array.of(OP_PUSHDATA1, n), data);
  } else if (n <= 0xffff) {
    return concatBytes(Uint8Array.of(OP_PUSHDATA2, n & 0xff, (n >> 8) & 0xff), data);
  }
  throw new Error('push too large for this helper');
}

/** Encode an integer as a minimal CScriptNum push, matching CScript::operator<<(int64_t). */
function scriptNumBytes(n) {
  if (n === 0) return new Uint8Array(0);
  const neg = n < 0;
  let abs = neg ? -n : n;
  const out = [];
  while (abs > 0) {
    out.push(abs & 0xff);
    abs = Math.floor(abs / 256);
  }
  if (out[out.length - 1] & 0x80) {
    out.push(neg ? 0x80 : 0x00);
  } else if (neg) {
    out[out.length - 1] |= 0x80;
  }
  return Uint8Array.from(out);
}

/** Minimal push of an integer, using OP_0/OP_1..OP_16/OP_1NEGATE where possible. */
function pushInt(n) {
  if (n === 0) return Uint8Array.of(OP_0);
  if (n === -1) return Uint8Array.of(OP_1NEGATE);
  if (n >= 1 && n <= 16) return Uint8Array.of(OP_1 + (n - 1));
  return pushData(scriptNumBytes(n));
}

/**
 * Build a fresh OP_MINT output script:
 *   <recipient_pubkey> <multiplier> <salt> OP_MINT
 * salt must be >= 16 bytes (consensus rule).
 */
function buildMintScript(pubkey, multiplier, salt) {
  if (salt.length < 16) throw new Error('salt must be at least 16 bytes');
  return concatBytes(pushData(pubkey), pushInt(multiplier), pushData(salt), Uint8Array.of(OP_MINT));
}

/**
 * Build an OP_MINT_TRANSFER covenant output script:
 *   <recipient_pubkey> <multiplier> OP_MINT_TRANSFER
 */
function buildTransferScript(pubkey, multiplier) {
  return concatBytes(pushData(pubkey), pushInt(multiplier), Uint8Array.of(OP_MINT_TRANSFER));
}

/** Generate a cryptographically random 16+ byte salt for a new mint. */
function randomSalt(len) {
  len = len || 32;
  const out = new Uint8Array(len);
  if (typeof crypto === 'undefined' || !crypto.getRandomValues) {
    throw new Error('no cryptographically secure RNG available (need global crypto.getRandomValues)');
  }
  crypto.getRandomValues(out);
  return out;
}

// ---- varint / transaction (de)serialization (legacy, non-segwit) ----

function encodeVarInt(n) {
  if (n < 0xfd) return Uint8Array.of(n);
  if (n <= 0xffff) return Uint8Array.of(0xfd, n & 0xff, (n >> 8) & 0xff);
  if (n <= 0xffffffff) {
    return Uint8Array.of(0xfe, n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, (n >>> 24) & 0xff);
  }
  throw new Error('varint too large for this helper');
}

function encodeUint32LE(n) {
  return Uint8Array.of(n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, (n >>> 24) & 0xff);
}

/** Bitcoin's CAmount is a signed 64-bit little-endian integer. */
function encodeInt64LE(n) {
  const out = new Uint8Array(8);
  const view = new DataView(out.buffer);
  view.setBigInt64(0, BigInt(n), true);
  return out;
}

/**
 * A transaction input: { txid (hex, big-endian/display order), vout,
 * scriptSig (Uint8Array, default empty), sequence (default 0xffffffff) }.
 * A transaction output: { value (satoshis), scriptPubKey (Uint8Array) }.
 */
function serializeTx(tx) {
  const parts = [encodeUint32LE(tx.version || 1), encodeVarInt(tx.vin.length)];
  for (const vin of tx.vin) {
    const txidLE = reverseBytes(hexToBytes(vin.txid));
    const scriptSig = vin.scriptSig || new Uint8Array(0);
    parts.push(
      txidLE,
      encodeUint32LE(vin.vout),
      encodeVarInt(scriptSig.length),
      scriptSig,
      encodeUint32LE(vin.sequence === undefined ? 0xffffffff : vin.sequence)
    );
  }
  parts.push(encodeVarInt(tx.vout.length));
  for (const vout of tx.vout) {
    parts.push(encodeInt64LE(vout.value), encodeVarInt(vout.scriptPubKey.length), vout.scriptPubKey);
  }
  parts.push(encodeUint32LE(tx.locktime || 0));
  return concatBytes(...parts);
}

function txToHex(tx) {
  return bytesToHex(serializeTx(tx));
}

const SIGHASH_NONE = 2;
const SIGHASH_SINGLE = 3;
const SIGHASH_ANYONECANPAY = 0x80;

// The classic Satoshi-client degenerate case: SignatureHash() returns the
// 256-bit value 1 (little-endian byte order: 0x01 followed by 31 zero
// bytes) rather than erroring, for nIn out of range or SIGHASH_SINGLE with
// no matching output. Preserved for exact consensus compatibility with
// SignatureHash() in src/script/interpreter.cpp.
const SIGHASH_DEGENERATE_HASH = concatBytes(Uint8Array.of(1), new Uint8Array(31));

/**
 * Legacy (pre-segwit) SignatureHash, matching SignatureHash() in
 * src/script/interpreter.cpp for SIGVERSION_BASE. Supports all four base
 * types (ALL/NONE/SINGLE) combined with the ANYONECANPAY flag.
 */
function signatureHash(scriptCode, tx, nIn, hashType) {
  hashType = hashType === undefined ? SIGHASH_ALL : hashType;
  const baseType = hashType & 0x1f;
  const anyoneCanPay = !!(hashType & SIGHASH_ANYONECANPAY);

  if (nIn >= tx.vin.length) {
    return SIGHASH_DEGENERATE_HASH;
  }
  if (baseType === SIGHASH_SINGLE && nIn >= tx.vout.length) {
    return SIGHASH_DEGENERATE_HASH;
  }

  const inputIndices = anyoneCanPay ? [nIn] : tx.vin.map((_, i) => i);
  const vinOut = inputIndices.map((i) => {
    const vin = tx.vin[i];
    const isSigned = i === nIn;
    const zeroSequence = !isSigned && (baseType === SIGHASH_SINGLE || baseType === SIGHASH_NONE);
    return {
      txid: vin.txid,
      vout: vin.vout,
      scriptSig: isSigned ? scriptCode : new Uint8Array(0),
      sequence: zeroSequence ? 0 : vin.sequence,
    };
  });

  let voutOut;
  if (baseType === SIGHASH_NONE) {
    voutOut = [];
  } else if (baseType === SIGHASH_SINGLE) {
    // Null (value=-1, empty script) placeholders for every output before
    // nIn, then the one real output being pinned. Outputs after nIn are
    // dropped entirely (not committed to at all).
    voutOut = [];
    for (let i = 0; i < nIn; i++) {
      voutOut.push({ value: -1, scriptPubKey: new Uint8Array(0) });
    }
    voutOut.push(tx.vout[nIn]);
  } else {
    voutOut = tx.vout;
  }

  const tmp = { version: tx.version, locktime: tx.locktime, vin: vinOut, vout: voutOut };
  const preimage = concatBytes(serializeTx(tmp), encodeUint32LE(hashType));
  return hash256(preimage);
}

/**
 * Sign a spend of a UAP position. scriptCode is the exact scriptPubKey
 * of the output being spent (the mint or transfer script). Returns the
 * scriptSig bytes: a single push of <DER signature + sighash byte>.
 *
 * `secp` must expose signSync(msgHash, privKey, opts) -> Uint8Array
 * (DER-encoded, canonical/low-S), matching @noble/secp256k1 v1.x.
 */
function signSpend(secp, scriptCode, privKey, tx, nIn, hashType) {
  hashType = hashType === undefined ? SIGHASH_ALL : hashType;
  const hash = signatureHash(scriptCode, tx, nIn, hashType);
  const der = secp.signSync(hash, privKey, { canonical: true, der: true });
  // scriptSig must consist of push opcodes, not raw bytes -- wrap the
  // signature+hashtype as a single minimal push, matching CScript([sig]).
  return pushData(concatBytes(der, Uint8Array.of(hashType)));
}

/**
 * Convenience: build+sign a mint spend that forwards the full input
 * value (minus fee) to a single OP_MINT_TRANSFER covenant output.
 *
 * @param secp EC library, see signSpend.
 * @param opts {
 *   input: { txid, vout, scriptCode (the OP_MINT script being spent),
 *            value (satoshis), privKey (spender's private key) },
 *   toPubkey, multiplier: covenant parameters for the new output,
 *   fee: satoshis to leave as miner fee (default 50000),
 * }
 */
function buildTransferTx(secp, opts) {
  const outValue = opts.input.value - (opts.fee === undefined ? 50000 : opts.fee);
  if (outValue <= 0) throw new Error('fee exceeds input value');
  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: opts.input.txid, vout: opts.input.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: outValue, scriptPubKey: buildTransferScript(opts.toPubkey, opts.multiplier) }],
  };
  tx.vin[0].scriptSig = signSpend(secp, opts.input.scriptCode, opts.input.privKey, tx, 0);
  return tx;
}

/**
 * Sign a standing, fillable sell order for a UAP position: "I'll give up
 * this token IF the final transaction pays me exactly this much." Uses
 * SIGHASH_SINGLE|ANYONECANPAY, which commits only to the maker's own
 * input and the output at the *same index* (their payment, not the
 * token's eventual destination -- that's unconstrained, since
 * CheckUapOutputConservation independently guarantees a valid covenant
 * exists without caring who it's addressed to). Any taker can later
 * complete the trade with fillOrder() without further input from the
 * maker. See doc/uap-marketplace-design.md for the full design.
 *
 * @param secp EC library, see signSpend.
 * @param opts {
 *   input: { txid, vout, scriptCode (the position being sold) },
 *   privKey: maker's private key (must match the pubkey in scriptCode),
 *   paymentScript: scriptPubKey the maker wants paid (Uint8Array),
 *   paymentValue: asking price in satoshis,
 * }
 * @returns { scriptSig, input: {txid, vout}, paymentScript, paymentValue }
 *   -- everything a taker needs to fill the order. Publish this whole
 *   object to an order relay.
 */
function signMakerOrder(secp, opts) {
  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: opts.input.txid, vout: opts.input.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: opts.paymentValue, scriptPubKey: opts.paymentScript }],
  };
  const hashType = SIGHASH_SINGLE | SIGHASH_ANYONECANPAY;
  const scriptSig = signSpend(secp, opts.input.scriptCode, opts.privKey, tx, 0, hashType);
  return {
    scriptSig,
    input: { txid: opts.input.txid, vout: opts.input.vout },
    paymentScript: opts.paymentScript,
    paymentValue: opts.paymentValue,
  };
}

/**
 * Complete a signed maker order (see signMakerOrder): append the taker's
 * own payment input and a covenant output sending the token to
 * themselves, plus change, and return the finished (but not yet
 * taker-signed) transaction. The maker's input/payment output are kept
 * at index 0 -- the same index the maker signed against -- since
 * SIGHASH_SINGLE ties the signature to output N when the signed input
 * ends up at index N in the final transaction; everything the taker adds
 * comes after.
 *
 * The taker must still sign their own input(s) themselves (e.g. via
 * their wallet's normal signing for an ordinary payment UTXO) before
 * broadcasting -- this only assembles the transaction, it doesn't
 * complete it.
 *
 * @param order the object returned by signMakerOrder
 * @param opts {
 *   toPubkey, multiplier: the covenant the taker wants the token sent to,
 *   tokenValue: the position's full value (satoshis) -- the maker's
 *     order doesn't carry this, since SIGHASH_SINGLE|ANYONECANPAY didn't
 *     commit to it; the taker must know it independently (e.g. from
 *     uap-indexer) and gets rejected by consensus if wrong,
 *   takerInputs: [{ txid, vout }] the taker's own payment UTXO(s),
 *   changeScript, changeValue: the taker's change output,
 * }
 */
function fillOrder(order, opts) {
  const tx = {
    version: 1,
    locktime: 0,
    vin: [
      { txid: order.input.txid, vout: order.input.vout, scriptSig: order.scriptSig, sequence: 0xffffffff },
      ...opts.takerInputs.map((inp) => ({ txid: inp.txid, vout: inp.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff })),
    ],
    vout: [
      order.paymentScript !== undefined ? { value: order.paymentValue, scriptPubKey: order.paymentScript } : null,
      { value: opts.tokenValue, scriptPubKey: buildTransferScript(opts.toPubkey, opts.multiplier) },
    ].filter(Boolean),
  };
  if (opts.changeValue) {
    tx.vout.push({ value: opts.changeValue, scriptPubKey: opts.changeScript });
  }
  return tx;
}

export {
  OP_MINT,
  OP_MINT_TRANSFER,
  SIGHASH_ALL,
  SIGHASH_NONE,
  SIGHASH_SINGLE,
  SIGHASH_ANYONECANPAY,
  COIN,
  hexToBytes,
  bytesToHex,
  randomSalt,
  configureSecp,
  buildMintScript,
  buildTransferScript,
  serializeTx,
  txToHex,
  signatureHash,
  signSpend,
  buildTransferTx,
  signMakerOrder,
  fillOrder,
};
