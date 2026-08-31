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
// `signSync(msgHash, privKey, opts)` returning a DER-encoded signature.
// ./secp.js provides exactly that, on top of a vendored @noble/secp256k1
// v2 build. This keeps uap.js itself dependency-free and lets you
// choose/audit/pin your own EC library.
//
// SECURITY: never send a private key to a server you don't control, and
// never paste a real private key into a demo page. Signing must happen
// wherever the key lives (ideally the user's own device).

import { sha256, hash256, hmacSha256, concatBytes } from './sha256.js';

/**
 * @noble/secp256k1 deliberately ships no hash implementation, to stay
 * environment-agnostic: synchronous RFC6979 signing needs an HMAC-SHA256
 * supplied by the caller. Call this once with your secp256k1 module before
 * using signSpend/buildTransferTx.
 *
 * This is the one place that knows where the library keeps that hook, so
 * that no version-specific detail leaks into the signing paths: v2 exposes
 * it as `etc.hmacSha256Sync`, v1.x as `utils.hmacSha256Sync`. Whichever
 * object is present gets it.
 */
function configureSecp(secp) {
  const hmac = (key, ...msgs) => hmacSha256(key, ...msgs);
  const target = secp.etc || secp.utils;
  if (!target) {
    throw new Error('configureSecp: secp module exposes neither etc nor utils');
  }
  target.hmacSha256Sync = hmac;
  return secp;
}

// ---- opcodes (must match src/script/script.h) ----
const OP_MINT = 0xb5;
const OP_MINT_TRANSFER = 0xba;
const OP_PUSHDATA1 = 0x4c;
const OP_PUSHDATA2 = 0x4d;
const OP_PUSHDATA4 = 0x4e;
const OP_CODESEPARATOR = 0xab;
const OP_0 = 0x00;
const OP_1 = 0x51; // OP_1..OP_16 are 0x51..0x60

const SIGHASH_ALL = 1;
const COIN = 100000000;

// Policy constants from src/policy/policy.h and src/amount.h
const RECOMMENDED_MIN_TX_FEE = COIN / 100;  // 1000000 satoshis
const DEFAULT_DUST_LIMIT = RECOMMENDED_MIN_TX_FEE;  // 1000000 satoshis
const DEFAULT_HARD_DUST_LIMIT = DEFAULT_DUST_LIMIT / 10;  // 100000 satoshis

// src/validation.h: DEFAULT_TRANSACTION_MAXFEE = RECOMMENDED_MIN_TX_FEE * 10000.
// The node refuses to send a transaction paying more than this, because a fee
// rate is easy to get wrong by orders of magnitude -- sat/byte where sat/kB
// was meant, or a bad figure from a fee estimator -- and the rate itself
// gives no clue that anything is off. Only the resulting absolute fee does,
// and by the time a wallet has signed and broadcast, the money is a miner's.
const DEFAULT_TRANSACTION_MAXFEE = RECOMMENDED_MIN_TX_FEE * 10000;  // 100 coins

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

/**
 * Derive a token's lineage origin from the outpoint being spent.
 * origin = SHA256(prevout.hash_reversed || prevout.n as 4-byte LE)
 *
 * @param txid 64-character hex string (big-endian / display order)
 * @param vout output index (uint32)
 * @returns 32-byte Uint8Array origin
 */
function deriveOrigin(txid, vout) {
  const txidBytes = hexToBytes(txid);
  if (txidBytes.length !== 32) {
    throw new Error(`txid must be exactly 32 bytes, got ${txidBytes.length}`);
  }
  if (!Number.isInteger(vout) || vout < 0 || vout > 0xffffffff) {
    throw new Error('vout must be an unsigned 32-bit integer');
  }
  const reversed = reverseBytes(txidBytes);
  const voutLE = encodeUint32LE(vout);
  const preimage = concatBytes(reversed, voutLE);
  // Origin is SHA256(txid_reversed || vout_le), NOT the Bitcoin-style double hash256
  return sha256(preimage);
}

/**
 * The lineage the covenant outputs of a spend must carry.
 *
 * This is NOT always deriveOrigin(txid, vout), and getting that wrong is the
 * easiest way to build a transaction the network will reject:
 *
 *   - Spending a MINT is the lineage's first spend, and the moment its
 *     identity is created. The origin is derived from the outpoint being
 *     spent, because a mint output cannot contain its own outpoint (that
 *     depends on the txid, which depends on the script).
 *   - Spending a TRANSFER must carry that lineage's existing origin forward
 *     UNCHANGED. Deriving a fresh one from the transfer's own outpoint
 *     renames the position into a lineage that has no input in the
 *     transaction, and consensus rejects it (see the C++
 *     transfer_rejects_rewriting_the_origin test).
 *
 * Since every spend after the first spends a transfer, deriving from the
 * outpoint unconditionally produces a wallet that can make exactly one valid
 * transaction per token and then silently emits invalid ones forever.
 *
 * @param scriptCode the covenant being spent (this position's scriptPubKey)
 * @param txid,vout the outpoint being spent
 * @returns 32-byte origin
 */
function originForSpend(scriptCode, txid, vout) {
  if (!scriptCode || scriptCode.length < 1) {
    throw new Error('originForSpend needs the scriptCode of the position being spent');
  }
  const last = scriptCode[scriptCode.length - 1];
  if (last === OP_MINT) {
    return deriveOrigin(txid, vout);
  }
  if (last === OP_MINT_TRANSFER) {
    const n = scriptCode.length;
    // ... <0x20> <32-byte origin> OP_MINT_TRANSFER
    if (n < 34 || scriptCode[n - 34] !== 0x20) {
      throw new Error('transfer scriptCode does not carry a 32-byte origin push');
    }
    return scriptCode.slice(n - 33, n - 1);
  }
  throw new Error('scriptCode is not a UAP mint or transfer covenant');
}

/**
 * Push a UAP multiplier in its canonical encoding: OP_0 for zero,
 * OP_1..OP_16 for 1..16, and a minimal data push of a minimal CScriptNum
 * above that. This is exactly what CScript::operator<<(int64_t) emits.
 *
 * There is only one accepted encoding per multiplier, and it is the same one
 * on both sides of the node's rulebook:
 *
 *   - Consensus (ParseUapOutputScript, src/script/script.cpp) accepts a UAP
 *     output only if every element uses its canonical push.
 *   - Standardness (SCRIPT_VERIFY_MINIMALDATA) requires that same canonical
 *     push when the covenant is *executed* at spend time.
 *
 * Those two rules used to disagree for 1..16 -- consensus insisted on a data
 * push, MINIMALDATA insisted on OP_N -- which made positions in that range
 * consensus-valid but only spendable by a non-standard transaction. That is
 * fixed at the node; this function must emit the canonical form so the two
 * stay in agreement. Anything else silently builds a position the network
 * will not relay a spend of.
 *
 * Verified against a regtest node -- see integration.test.js, "small
 * multipliers (1..16) round-trip end to end".
 */
function pushMultiplier(n) {
  if (!Number.isInteger(n) || n < 0 || n > 2147483647) {
    throw new Error('multiplier must be an integer in [0, 2147483647]');
  }
  if (n === 0) return Uint8Array.of(OP_0);
  if (n <= 16) return Uint8Array.of(OP_1 + (n - 1));
  return pushData(scriptNumBytes(n));
}

/**
 * Build a fresh OP_MINT output script:
 *   <recipient_pubkey> <multiplier> OP_MINT
 */
function buildMintScript(pubkey, multiplier) {
  return concatBytes(pushData(pubkey), pushMultiplier(multiplier), Uint8Array.of(OP_MINT));
}

/**
 * Build an OP_MINT_TRANSFER covenant output script:
 *   <recipient_pubkey> <multiplier> <origin32> OP_MINT_TRANSFER
 *
 * @param pubkey recipient's public key (Uint8Array)
 * @param multiplier token multiplier (integer)
 * @param origin 32-byte Uint8Array lineage identifier (required)
 */
function buildTransferScript(pubkey, multiplier, origin) {
  if (origin === undefined) {
    throw new Error('buildTransferScript requires origin parameter (32-byte Uint8Array)');
  }
  if (origin.length !== 32) {
    throw new Error(`origin must be exactly 32 bytes, got ${origin.length}`);
  }
  return concatBytes(pushData(pubkey), pushMultiplier(multiplier), pushData(origin), Uint8Array.of(OP_MINT_TRANSFER));
}

// A token's ticker lives in an OP_RETURN on its mint transaction:
//
//   OP_RETURN "WUAP" <version> <ticker> <metadata_hash>
//
// Exactly four pushes, in that order. Read back by uap-indexer/metadata.go,
// which is the authority on the format and rejects anything that does not
// match it exactly -- including a fifth push, so that two scripts cannot
// parse to the same metadata.
//
// There used to be a fifth field here, `name`, a free-text UTF-8 string.
// It is gone: a free-text field is exactly the kind of thing that grows
// unboundedly and invites disputes about what a node "should" have relayed,
// and nothing on chain depended on it being human-readable at parse time --
// richer, mutable metadata belongs off-chain, committed to by the hash
// field below. `ticker` is now restricted to [A-Z0-9] (see normalizeTicker)
// for the same reason: a wire format with no charset rule eventually has to
// answer what a lowercase ticker or an emoji ticker means, and the answer
// that scales is "it can't happen".
//
// An OP_RETURN output is provably unspendable, so it never enters the UTXO
// set. This is the reason the metadata goes here rather than into the
// covenant: the covenant is a live UTXO that every node holds for as long as
// the position exists, and metadata in it would be resident forever.
const METADATA_MAGIC = Uint8Array.of(0x57, 0x55, 0x41, 0x50); // "WUAP"
const METADATA_VERSION = 0x02;
const OP_RETURN = 0x6a;

// The relay ceiling on a whole data-carrier scriptPubKey: MAX_OP_RETURN_RELAY
// in src/script/standard.h, checked by IsStandardTx against
// scriptPubKey.size(). A record over this is not merely discouraged -- no
// node will relay the mint at all, so the token would never reach the chain.
const MAX_METADATA_SCRIPT_BYTES = 83;

// What is left for the caller's own bytes once the fixed parts are paid for:
// OP_RETURN (1) + push"WUAP" (5) + push(version) (2) + the two push opcodes
// for ticker and hash (2) = 10.
const METADATA_FIXED_OVERHEAD = 10;
const MAX_METADATA_PAYLOAD_BYTES = MAX_METADATA_SCRIPT_BYTES - METADATA_FIXED_OVERHEAD; // 73

// Ticker length bound, enforced by normalizeTicker. Chosen independently of
// the OP_RETURN budget above (8 + 32 = 40 is nowhere near the 73-byte
// payload ceiling) -- it exists so a ticker reads like a ticker, not because
// the wire format needs it.
const MAX_TICKER_BYTES = 8;

/**
 * Uppercase-fold and validate a ticker string, matching the charset the wire
 * format allows: 1..8 bytes, every byte in [A-Z0-9]. This is the single
 * implementation of that rule -- callers (buildMetadataScript, the mint UI)
 * consume it rather than re-checking the charset themselves, so the rule
 * cannot drift between what is validated and what is signed.
 *
 * Folding happens here, in the wallet, before anything is built or shown:
 * folding on read (e.g. in an indexer) would let two different wire records
 * ("doge" and "DOGE") mean the same displayed ticker, which is the
 * collision this format exists to prevent.
 *
 * Throws an Error naming the specific problem (the offending character, or
 * the actual length) rather than a bare "invalid" -- this is the message a
 * user sees on a form field.
 */
function normalizeTicker(ticker) {
  const upper = (ticker || '').toUpperCase();
  if (upper.length < 1 || upper.length > MAX_TICKER_BYTES) {
    throw new Error(`ticker must be 1..${MAX_TICKER_BYTES} characters, got ${upper.length}`);
  }
  for (let i = 0; i < upper.length; i++) {
    const ch = upper[i];
    const code = upper.charCodeAt(i);
    const isDigit = code >= 0x30 && code <= 0x39;
    const isUpper = code >= 0x41 && code <= 0x5a;
    if (!isDigit && !isUpper) {
      throw new Error(`ticker contains invalid character ${JSON.stringify(ch)} at position ${i}; only A-Z and 0-9 are allowed`);
    }
  }
  return upper;
}

/**
 * Build the metadata OP_RETURN for a mint:
 *   OP_RETURN "WUAP" <version> <ticker> <metadata_hash>
 *
 * `ticker` is normalized here (uppercase-folded and charset-checked, via
 * normalizeTicker) rather than requiring an already-normalized string, so
 * this function is the one place that can build a wire-valid record no
 * matter what the caller passes in. `metadataHash` is an optional 32-byte
 * Uint8Array committing to richer off-chain metadata; the commitment is
 * on-chain so the content it names cannot be swapped later.
 *
 * Throws on an invalid ticker or hash length. The final length check below
 * is a cheap invariant, not a real limit at today's field sizes (8 + 32 is
 * nowhere near the 73-byte payload budget) -- it exists to catch a future
 * field addition that overflows the relay ceiling, the way the ticker+name
 * combination used to.
 */
function buildMetadataScript({ ticker, metadataHash = null }) {
  const tickerBytes = new TextEncoder().encode(normalizeTicker(ticker));
  const hashBytes = metadataHash || new Uint8Array(0);

  if (hashBytes.length !== 0 && hashBytes.length !== 32) {
    throw new Error(`metadata hash must be empty or exactly 32 bytes, got ${hashBytes.length}`);
  }

  const script = concatBytes(
    Uint8Array.of(OP_RETURN),
    pushData(METADATA_MAGIC),
    pushData(Uint8Array.of(METADATA_VERSION)),
    pushData(tickerBytes),
    pushData(hashBytes)
  );

  if (script.length > MAX_METADATA_SCRIPT_BYTES) {
    throw new Error(
      `internal error: built a ${script.length}-byte metadata script, over the ` +
      `${MAX_METADATA_SCRIPT_BYTES}-byte relay ceiling`
    );
  }

  return script;
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
    const txidBytes = hexToBytes(vin.txid);
    if (txidBytes.length !== 32) {
      throw new Error(`txid must be exactly 32 bytes, got ${txidBytes.length} (${JSON.stringify(vin.txid)})`);
    }
    const txidLE = reverseBytes(txidBytes);
    const scriptSig = vin.scriptSig || new Uint8Array(0);
    // scriptSigLength exists only for signatureHash's scriptCode, where the
    // node writes a length that can disagree with the bytes that follow.
    // See serializeScriptCode.
    const declaredLength = vin.scriptSigLength === undefined ? scriptSig.length : vin.scriptSigLength;
    parts.push(
      txidLE,
      encodeUint32LE(vin.vout),
      encodeVarInt(declaredLength),
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
 * Advance one instruction, mirroring CScript::GetOp2 in src/script/script.h.
 * Returns { ok, pos, opcode }. On failure pos still reports how far the walk
 * got, because SerializeScriptCode writes up to exactly that point.
 */
function nextOp(script, pc) {
  if (pc >= script.length) return { ok: false, pos: pc, opcode: -1 };
  const opcode = script[pc++];
  if (opcode <= OP_PUSHDATA4) {
    let size = opcode;
    if (opcode === OP_PUSHDATA1) {
      if (script.length - pc < 1) return { ok: false, pos: pc, opcode };
      size = script[pc++];
    } else if (opcode === OP_PUSHDATA2) {
      if (script.length - pc < 2) return { ok: false, pos: pc, opcode };
      size = script[pc] | (script[pc + 1] << 8);
      pc += 2;
    } else if (opcode === OP_PUSHDATA4) {
      if (script.length - pc < 4) return { ok: false, pos: pc, opcode };
      size = (script[pc] | (script[pc + 1] << 8) | (script[pc + 2] << 16) | (script[pc + 3] << 24)) >>> 0;
      pc += 4;
    }
    if (script.length - pc < size) return { ok: false, pos: pc, opcode };
    pc += size;
  }
  return { ok: true, pos: pc, opcode };
}

/**
 * Serialize a scriptCode for signing, matching
 * CTransactionSignatureSerializer::SerializeScriptCode in
 * src/script/interpreter.cpp: every OP_CODESEPARATOR is dropped from the
 * signed preimage.
 *
 * Returns { bytes, declaredLength } because the node computes the length
 * prefix and the emitted bytes by two different routes -- the prefix from
 * scriptCode.size() minus the separator count, the bytes from an opcode
 * walk. For any well-formed script the two agree. They diverge only when the
 * script ends mid-push, where the walk stops early and the node deliberately
 * writes a length longer than what follows it. That case cannot be reached
 * by anything this library builds, and the C++ fixture in
 * src/test/data/sighash.json contains no such script, so the divergence is
 * reproduced here on the strength of reading the node, not of a test. A
 * plain filter of 0xab bytes would pass every assertion we have and be
 * wrong here.
 */
function serializeScriptCode(scriptCode) {
  const runs = [];
  let separators = 0;
  let begin = 0;
  let pc = 0;
  for (;;) {
    const step = nextOp(scriptCode, pc);
    if (!step.ok) {
      pc = step.pos;
      break;
    }
    pc = step.pos;
    if (step.opcode === OP_CODESEPARATOR) {
      runs.push(scriptCode.subarray(begin, pc - 1));
      begin = pc;
      separators++;
    }
  }
  if (begin !== scriptCode.length) {
    runs.push(scriptCode.subarray(begin, pc));
  }
  return { bytes: concatBytes(...runs), declaredLength: scriptCode.length - separators };
}

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

  const signedScript = serializeScriptCode(scriptCode);
  const inputIndices = anyoneCanPay ? [nIn] : tx.vin.map((_, i) => i);
  const vinOut = inputIndices.map((i) => {
    const vin = tx.vin[i];
    const isSigned = i === nIn;
    const zeroSequence = !isSigned && (baseType === SIGHASH_SINGLE || baseType === SIGHASH_NONE);
    return {
      txid: vin.txid,
      vout: vin.vout,
      scriptSig: isSigned ? signedScript.bytes : new Uint8Array(0),
      scriptSigLength: isSigned ? signedScript.declaredLength : undefined,
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
 * (DER-encoded, canonical/low-S); ./secp.js is that module.
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
 * Sign a P2PKH input. Returns the scriptSig bytes: a push of the DER
 * signature with sighash byte, followed by a push of the public key.
 * This is the standard scriptSig format for spending P2PKH outputs.
 *
 * `secp` must expose signSync(msgHash, privKey, opts) -> Uint8Array
 * (DER-encoded, canonical/low-S); ./secp.js is that module.
 */
function signP2PKHInput(secp, scriptCode, privKey, pubKey, tx, nIn, hashType) {
  hashType = hashType === undefined ? SIGHASH_ALL : hashType;
  const hash = signatureHash(scriptCode, tx, nIn, hashType);
  const der = secp.signSync(hash, privKey, { canonical: true, der: true });
  // P2PKH scriptSig is: <signature+hashtype> <pubkey>, two separate pushes
  return concatBytes(pushData(concatBytes(der, Uint8Array.of(hashType))), pushData(pubKey));
}

/**
 * Estimate transaction size in bytes.
 * Used for fee calculation. P2PKH inputs are ~150 bytes, outputs are ~34 bytes.
 * @private
 */
function estimateTxSize(nInputs, nOutputs) {
  // Base: version(4) + input count varint + output count varint + locktime(4)
  let size = 10;
  // Inputs: each P2PKH scriptSig is roughly ~72 bytes (sig) + 1 (length) + 33 (pubkey) + 1 (length) = ~107 bytes
  // Plus txid(32) + vout(4) + sequence(4) + scriptSig varint(1) = 41 bytes overhead
  // Total per input: ~150 bytes
  size += nInputs * 150;
  // Outputs: scriptPubKey(25 for P2PKH) + value(8) + scriptPubKey varint(1) = 34 bytes
  size += nOutputs * 34;
  return size;
}

/**
 * Select UTXOs from candidates to cover a target amount plus fee.
 * Uses a greedy approach: sorts inputs by value ascending and selects
 * the smallest UTXOs that sum to target + estimated fee. Note that this
 * selection strategy favors smaller inputs (maximizing input count),
 * which consolidates dust but increases fees relative to selecting fewer
 * large inputs.
 *
 * @param candidateInputs array of { txid, vout, value, scriptCode, privKey, pubKey }
 * @param targetAmount satoshis to send
 * @param changeScriptSize size of change script in bytes (for fee calculation)
 * @param feeRate satoshis per kilobyte (default 1000 sat/kB; applied with minimum floor)
 * @param nOutputs number of payment outputs (excluding change)
 * @returns { selected: [inputs], totalInput: satoshis, change: satoshis, fee: satoshis }
 * @throws if insufficient funds
 * @private
 */
function selectCoins(candidateInputs, targetAmount, changeScriptSize, feeRate, nOutputs) {
  feeRate = feeRate === undefined ? 1000 : feeRate;
  nOutputs = nOutputs === undefined ? 1 : nOutputs;
  const changeScriptLen = changeScriptSize === undefined ? 25 : changeScriptSize;  // assume P2PKH-like

  // Sort by value ascending (greedy approach: select smallest sufficient UTXOs)
  const sorted = [...candidateInputs].sort((a, b) => a.value - b.value);

  // Iteratively try selecting inputs, accounting for the size of additional inputs
  let selected = [];
  let totalInput = 0;

  for (const input of sorted) {
    selected.push(input);
    totalInput += input.value;

    // Estimate fee for this selection
    // Account for all requested payment outputs plus optional change
    const estimatedOutputs = nOutputs + (totalInput > targetAmount && changeScriptLen > 0 ? 1 : 0);
    const estimatedSize = estimateTxSize(selected.length, estimatedOutputs);
    let estimatedFee = Math.ceil(estimatedSize * feeRate / 1000);  // rounding up (sat/kB)

    // Apply minimum fee floor: never go below RECOMMENDED_MIN_TX_FEE
    if (estimatedFee < RECOMMENDED_MIN_TX_FEE) {
      estimatedFee = RECOMMENDED_MIN_TX_FEE;
    }

    if (totalInput >= targetAmount + estimatedFee) {
      const change = totalInput - targetAmount - estimatedFee;
      return { selected, totalInput, change, fee: estimatedFee };
    }
  }

  throw new Error('insufficient funds for transaction (cannot select enough UTXOs to cover target + fee)');
}

/**
 * Build a complete P2PKH payment transaction: select inputs from candidates,
 * add requested outputs, add change output, and sign all inputs.
 * Ensures fee is at least RECOMMENDED_MIN_TX_FEE.
 *
 * @param secp EC library, see signSpend.
 * @param opts {
 *   inputs: array of { txid, vout, value, scriptCode, privKey, pubKey },
 *   outputs: array of { value, scriptPubKey },
 *   changeScript (optional): Uint8Array for change output; if omitted and change >= DEFAULT_DUST_LIMIT, throws error,
 *   feeRate (optional): satoshis per kilobyte (default 1000 sat/kB),
 * }
 * @returns signed transaction
 * @throws if changeScript omitted and change >= DEFAULT_DUST_LIMIT, or insufficient funds
 */
function buildPaymentTx(secp, opts) {
  const candidateInputs = opts.inputs;
  const requestedOutputs = opts.outputs;
  const changeScript = opts.changeScript;
  const feeRate = opts.feeRate || 1000;
  // Pass an explicit maxFee to authorise a larger one; Infinity disables the
  // guard entirely. It throws rather than clamping, because quietly paying a
  // different fee than the caller asked for is its own failure.
  const maxFee = opts.maxFee === undefined ? DEFAULT_TRANSACTION_MAXFEE : opts.maxFee;

  // Calculate total requested output value
  let totalOutputValue = 0;
  for (const out of requestedOutputs) {
    totalOutputValue += out.value;
  }

  // Select coins (passing actual number of payment outputs)
  const { selected, totalInput, change, fee } = selectCoins(
    candidateInputs,
    totalOutputValue,
    changeScript ? changeScript.length : 0,
    feeRate,
    requestedOutputs.length
  );

  // Build transaction skeleton
  const vin = selected.map((input) => ({
    txid: input.txid,
    vout: input.vout,
    scriptSig: new Uint8Array(0),  // will be filled by signing
    sequence: 0xffffffff
  }));

  // Gather outputs
  const vout = [...requestedOutputs];  // Copy requested outputs

  // Check for silent fund loss: if change is spendable but no change script provided, error
  if (change >= DEFAULT_DUST_LIMIT && !changeScript) {
    throw new Error(`change of ${change} satoshis exceeds dust limit but no changeScript provided (refusing silent loss of funds)`);
  }

  // Add change output if change >= dust limit
  if (change >= DEFAULT_DUST_LIMIT && changeScript) {
    vout.push({
      value: change,
      scriptPubKey: changeScript
    });
  }

  // The fee actually paid is what is left over, which includes any change
  // too small to be worth an output. Check that, not the estimate: the
  // estimate is the number that was wrong in the first place.
  let totalOutputAfterChange = 0;
  for (const out of vout) totalOutputAfterChange += out.value;
  const actualFee = totalInput - totalOutputAfterChange;
  if (actualFee > maxFee) {
    throw new Error(
      `refusing to build a transaction paying ${actualFee} satoshis in fees, ` +
      `above the ${maxFee} satoshi cap. Check that feeRate is in satoshis per ` +
      `kilobyte, not per byte. Pass maxFee to authorise a larger fee.`
    );
  }

  const tx = {
    version: 1,
    locktime: 0,
    vin,
    vout
  };

  // Sign each input
  for (let i = 0; i < selected.length; i++) {
    const input = selected[i];
    tx.vin[i].scriptSig = signP2PKHInput(
      secp,
      input.scriptCode,
      input.privKey,
      input.pubKey,
      tx,
      i
    );
  }

  return tx;
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
  const fee = opts.fee === undefined ? 50000 : opts.fee;
  const maxFee = opts.maxFee === undefined ? DEFAULT_TRANSACTION_MAXFEE : opts.maxFee;
  if (fee > maxFee) {
    throw new Error(
      `refusing to build a transfer paying ${fee} satoshis in fees, above the ` +
      `${maxFee} satoshi cap. Pass maxFee to authorise a larger fee.`
    );
  }
  const outValue = opts.input.value - fee;
  if (outValue <= 0) throw new Error('fee exceeds input value');

  // First spend of a mint assigns the lineage; a transfer carries its own
  // forward. See originForSpend -- deriving unconditionally is wrong.
  const origin = originForSpend(opts.input.scriptCode, opts.input.txid, opts.input.vout);

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: opts.input.txid, vout: opts.input.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: outValue, scriptPubKey: buildTransferScript(opts.toPubkey, opts.multiplier, origin) }],
  };
  tx.vin[0].scriptSig = signSpend(secp, opts.input.scriptCode, opts.input.privKey, tx, 0);
  return tx;
}

/**
 * Build a melt transaction: redeem a position's backing to spendable WHIP.
 * Spends the position and produces two outputs:
 *   1. A dust-valued OP_MINT_TRANSFER covenant with the same pubkey and multiplier
 *      (this satisfies CheckUapOutputConservation's covenant requirement)
 *   2. A plain output taking the remainder (input - dust - fee)
 *
 * A position can never be melted to zero, because a covenant output always
 * survives; the dust remainder is permanent. A position is only "fully
 * redeemable" to the extent of (its value - dust - fee).
 *
 * @param secp EC library, see signSpend.
 * @param opts {
 *   input: { txid, vout, scriptCode (the position being melted),
 *            value (satoshis), privKey (spender's private key) },
 *   toPubkey, multiplier: the pubkey and multiplier for the new covenant output,
 *   remainderScript (optional): Uint8Array for the remainder output; if omitted,
 *     the remainder is discarded (leaves it as miner fee), which is an error if
 *     remainder >= DEFAULT_DUST_LIMIT,
 *   fee: satoshis to leave as miner fee (default 50000),
 * }
 * @returns signed transaction with covenant output at index 0, remainder at index 1
 * @throws if the position is too small to satisfy dust + fee
 */
function buildMeltTx(secp, opts) {
  const fee = opts.fee === undefined ? 50000 : opts.fee;
  const maxFee = opts.maxFee === undefined ? DEFAULT_TRANSACTION_MAXFEE : opts.maxFee;
  if (fee > maxFee) {
    throw new Error(
      `refusing to build a melt paying ${fee} satoshis in fees, above the ` +
      `${maxFee} satoshi cap. Pass maxFee to authorise a larger fee.`
    );
  }

  // remainderScript is required, not optional. Melting exists to recover the
  // backing to something spendable; a melt with nowhere to send the remainder
  // is not a melt, it is a burn that pays the difference to the miner. Making
  // it mandatory means that transaction cannot be built by accident.
  if (!opts.remainderScript || opts.remainderScript.length === 0) {
    throw new Error(
      'buildMeltTx requires remainderScript: the recovered backing needs a ' +
      'destination, and omitting it would pay the whole remainder to the miner'
    );
  }

  // The covenant output is what satisfies consensus (at least one
  // same-multiplier covenant must survive), so it is pinned at dust: it is
  // pure overhead, and every satoshi above dust left in it is backing that
  // did not come out.
  const dustValue = DEFAULT_DUST_LIMIT;
  const remainder = opts.input.value - dustValue - fee;

  // The remainder must itself clear dust. Testing only for `remainder < 0`
  // would happily build a transaction whose payout is a sub-dust output, which
  // no node will relay -- so the melt would look successful right up until
  // broadcast. A position therefore needs two dust limits plus the fee to be
  // meltable at all.
  if (remainder < DEFAULT_DUST_LIMIT) {
    throw new Error(
      `position value ${opts.input.value} is insufficient to melt: need at ` +
      `least ${dustValue + fee + DEFAULT_DUST_LIMIT} satoshis (${dustValue} for ` +
      `the surviving covenant, ${fee} fee, and ${DEFAULT_DUST_LIMIT} so the ` +
      `remainder itself clears dust), but only ${opts.input.value} available`
    );
  }

  // First spend of a mint assigns the lineage; a transfer carries its own
  // forward. See originForSpend.
  const origin = originForSpend(opts.input.scriptCode, opts.input.txid, opts.input.vout);

  // Covenant at index 0, remainder at index 1.
  const vout = [
    { value: dustValue, scriptPubKey: buildTransferScript(opts.toPubkey, opts.multiplier, origin) },
    { value: remainder, scriptPubKey: opts.remainderScript }
  ];

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: opts.input.txid, vout: opts.input.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout
  };

  // Sign the input
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
 * @param order the object returned by signMakerOrder. Its `origin` is the
 *   32-byte lineage of the position being sold. The taker CANNOT derive this:
 *   for a transfer it lives in the maker's scriptPubKey, which the taker never
 *   sees (an order carries only the maker's scriptSig), and deriving it from
 *   the maker's outpoint is right only when the position is a fresh mint. So
 *   the lineage has to travel with the order -- the relay reads it off the
 *   position and publishes it, the same way it publishes backing_value.
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
  // The lineage comes from the order, not from the maker's outpoint: see the
  // note on @param order. Deriving it here would silently produce a fillable-
  // looking transaction that consensus rejects for every position except a
  // fresh mint's first sale.
  const origin = order.origin !== undefined
    ? order.origin
    : originForSpend(order.input.scriptCode, order.input.txid, order.input.vout);
  if (!(origin instanceof Uint8Array) || origin.length !== 32) {
    throw new Error('fillOrder needs the order\'s 32-byte lineage origin');
  }

  const tx = {
    version: 1,
    locktime: 0,
    vin: [
      { txid: order.input.txid, vout: order.input.vout, scriptSig: order.scriptSig, sequence: 0xffffffff },
      ...opts.takerInputs.map((inp) => ({ txid: inp.txid, vout: inp.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff })),
    ],
    vout: [
      order.paymentScript !== undefined ? { value: order.paymentValue, scriptPubKey: order.paymentScript } : null,
      { value: opts.tokenValue, scriptPubKey: buildTransferScript(opts.toPubkey, opts.multiplier, origin) },
    ].filter(Boolean),
  };
  if (opts.changeValue) {
    tx.vout.push({ value: opts.changeValue, scriptPubKey: opts.changeScript });
  }
  return tx;
}

// CANCEL_DOMAIN and cancelMessage together are the exact scheme
// contrib/uap-indexer/cancel_auth.go implements server-side; the two must
// stay byte-for-byte identical or every cancel signature will fail to
// verify there. See that file for the full reasoning (what's bound, replay
// handling, cross-relay scope, clock dependence).
const CANCEL_DOMAIN = 'whippet-uap-cancel-order-v1';

/**
 * The exact byte string a maker signs to authorize withdrawing order
 * (txid, vout) whose current script_sig and cancel_nonce (both taken from
 * the order as the relay reports it, not recomputed locally) are as given.
 *
 * cancelNonce must be passed as the string the relay sent, not a Number:
 * it is a nanosecond timestamp and by 2026 already exceeds
 * Number.MAX_SAFE_INTEGER, so round-tripping it through a JS Number
 * silently rounds it -- which would make every cancel signature mismatch
 * what the relay reconstructs and verification would always fail. This is
 * exactly why Order.CancelNonce is JSON-encoded as a string
 * (`cancel_nonce,string` in orders.go) rather than a bare number.
 */
function cancelMessage(txid, vout, scriptSigHex, cancelNonce) {
  if (typeof cancelNonce !== 'string') {
    throw new Error('cancelMessage: cancelNonce must be the exact string the relay sent, never a Number');
  }
  const s = `${CANCEL_DOMAIN}\n${txid}\n${vout}\n${scriptSigHex}\n${cancelNonce}`;
  return new TextEncoder().encode(s);
}

/**
 * Sign a cancellation for a standing sell order (see signMakerOrder):
 * authorizes contrib/uap-indexer's DELETE /orders/{txid}/{vout}.
 *
 * This replaces the original (broken) cancellation contract, which
 * "authorized" a withdrawal by echoing the order's own script_sig back --
 * a value GET /orders publishes to every visitor, so it was never a real
 * credential; anyone who had viewed the order book could cancel anyone's
 * order. A cancellation is now a real ECDSA signature by the position's
 * own key (the same key that made the order), over a message that binds
 * the exact order -- outpoint and script_sig -- and a relay-assigned
 * nonce that changes on every publish, so a captured cancel signature
 * cannot be replayed against a different order or a later republish of
 * this same one. See contrib/uap-indexer/cancel_auth.go for the full
 * scheme and its reasoning.
 *
 * @param secp EC library, see signSpend.
 * @param opts {
 *   txid, vout: the position's outpoint (order.txid / order.vout),
 *   scriptSig: the order's script_sig, as the exact hex string the relay
 *     has on file (order.script_sig from a GET/POST /orders response --
 *     not recomputed locally),
 *   cancelNonce: the order's cancel_nonce, as the exact string the relay
 *     sent (order.cancel_nonce -- see cancelMessage's note on why this
 *     must stay a string),
 *   privKey: the maker's private key (must match the position's pubkey),
 * }
 * @returns {string} hex-encoded DER signature; pass as the `sig` query
 *   parameter to DELETE /orders/{txid}/{vout}.
 */
function signCancelOrder(secp, opts) {
  const message = cancelMessage(opts.txid, opts.vout, opts.scriptSig, opts.cancelNonce);
  const hash = hash256(message);
  const der = secp.signSync(hash, opts.privKey, { canonical: true, der: true });
  return bytesToHex(der);
}

export {
  OP_MINT,
  OP_MINT_TRANSFER,
  SIGHASH_ALL,
  SIGHASH_NONE,
  SIGHASH_SINGLE,
  SIGHASH_ANYONECANPAY,
  COIN,
  RECOMMENDED_MIN_TX_FEE,
  DEFAULT_DUST_LIMIT,
  DEFAULT_HARD_DUST_LIMIT,
  DEFAULT_TRANSACTION_MAXFEE,
  hexToBytes,
  bytesToHex,
  concatBytes,
  deriveOrigin,
  originForSpend,
  configureSecp,
  buildMintScript,
  buildTransferScript,
  buildMetadataScript,
  normalizeTicker,
  MAX_METADATA_PAYLOAD_BYTES,
  MAX_METADATA_SCRIPT_BYTES,
  MAX_TICKER_BYTES,
  serializeTx,
  txToHex,
  signatureHash,
  signSpend,
  signP2PKHInput,
  buildPaymentTx,
  buildTransferTx,
  buildMeltTx,
  signMakerOrder,
  fillOrder,
  cancelMessage,
  signCancelOrder,
};
