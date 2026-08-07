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

import { hash256, hmacSha256, concatBytes } from './sha256.js';

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
 *   <recipient_pubkey> <multiplier> <salt> OP_MINT
 * salt must be >= 16 bytes (consensus rule).
 */
function buildMintScript(pubkey, multiplier, salt) {
  if (salt.length < 16) throw new Error('salt must be at least 16 bytes');
  return concatBytes(pushData(pubkey), pushMultiplier(multiplier), pushData(salt), Uint8Array.of(OP_MINT));
}

/**
 * Build an OP_MINT_TRANSFER covenant output script:
 *   <recipient_pubkey> <multiplier> OP_MINT_TRANSFER
 */
function buildTransferScript(pubkey, multiplier) {
  return concatBytes(pushData(pubkey), pushMultiplier(multiplier), Uint8Array.of(OP_MINT_TRANSFER));
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
  RECOMMENDED_MIN_TX_FEE,
  DEFAULT_DUST_LIMIT,
  DEFAULT_HARD_DUST_LIMIT,
  DEFAULT_TRANSACTION_MAXFEE,
  hexToBytes,
  bytesToHex,
  concatBytes,
  randomSalt,
  configureSecp,
  buildMintScript,
  buildTransferScript,
  serializeTx,
  txToHex,
  signatureHash,
  signSpend,
  signP2PKHInput,
  buildPaymentTx,
  buildTransferTx,
  signMakerOrder,
  fillOrder,
};
