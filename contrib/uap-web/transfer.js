// transfer.js - Pure logic for planning and building UAP token transfers.
// No DOM, no network. Modelled on mint.js: a pure planTransfer() that
// validates and computes every value, and an async buildTransferTx() that
// assembles and signs.
//
// ---------------------------------------------------------------------------
// WHY A TRANSFER LOOKS THE WAY IT DOES
// ---------------------------------------------------------------------------
//
// The covenant is `<recipient_pubkey> <multiplier> OP_MINT_TRANSFER`, built
// here by uap.buildTransferScript() -- the same function fillOrder() uses to
// build the taker's output, so there is exactly one implementation of the
// output format in this codebase and this file adds no second copy of it.
//
// Three consensus rules (src/script/interpreter.cpp, CheckUapOutputConservation
// and CheckUapOneShot) dictate the transaction's shape:
//
// 1. EXACTLY ONE UAP INPUT. The conservation check runs once per UAP input,
//    and each run demands that the total of *all* matching-multiplier
//    covenant outputs fit within THAT input's value. Two same-multiplier
//    positions therefore cannot be merged: the shared output total can
//    satisfy at most one input's ceiling. (For a fresh OP_MINT position the
//    one-shot rule forbids sibling UAP inputs outright.) So a transfer
//    spends one position, never several.
//
// 2. THE FEE IS NOT PAID OUT OF THE TOKEN. Value that leaves the covenant
//    -- to a fee, or to a plain P2PKH output -- stops being token-bearing.
//    Paying the fee from the position would therefore *destroy tokens on
//    every send*, silently. Instead an ordinary P2PKH UTXO is added as a
//    second input purely to fund the fee (non-UAP sibling inputs are
//    allowed for a transfer), and the covenant outputs carry the position's
//    full value across. Tokens in == tokens out, exactly.
//
// 3. THE REMAINDER GOES BACK INTO A COVENANT. Sending part of a position
//    means two covenant outputs: one to the recipient, one back to the
//    sender. A plain change output here would burn the difference.
//
// The recipient is identified by PUBLIC KEY, not by address: the covenant
// script embeds a real 33/65-byte pubkey (consensus rejects anything else --
// see ParseUapOutputScript in src/script/script.cpp), and an address is only
// a hash160 of one, which cannot be inverted. The optional `toAddress` field
// is a *cross-check*, not a destination: if supplied, hash160(toPubKey) must
// equal the address payload, which is what makes a scanned/typed address
// useful in the send flow as confirmation that the pasted pubkey really
// belongs to the person whose address the user has.

import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';
import * as secp from '../uap-js/secp.js';

const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;
// See market.js: outputs are gated by the hard limit, not the soft one.
const HARD_DUST_LIMIT = uap.DEFAULT_HARD_DUST_LIMIT;
const RECOMMENDED_MIN_TX_FEE = uap.RECOMMENDED_MIN_TX_FEE;
const MAX_MULTIPLIER = 2147483647;

/**
 * The largest satoshi figure this planner will reason about.
 *
 * Consensus allows far more: MAX_MONEY is 1e18 satoshis (src/amount.h), about
 * 111x Number.MAX_SAFE_INTEGER. Above 2^53 a JavaScript number is no longer
 * every integer, only every 2nd, 4th, 8th... one, and the arithmetic this
 * module does stops being reversible: `remainderSats = value - amount`
 * followed by `amount + remainderSats` no longer returns `value`. Both
 * directions of that error are real damage, and neither is visible from
 * inside the plan, because every figure the plan checks itself against is
 * computed with the same lossy arithmetic:
 *
 *   OVER  -- the covenant outputs total more than the input holds, and
 *     consensus rejects the transaction (nValueOut > nValueIn). Loud.
 *   UNDER -- the outputs total less, the transaction is valid, and the
 *     missing satoshis leave the covenant as extra fee. Tokens destroyed,
 *     silently, with a confirmation to show for it.
 *
 * Rather than carry every satoshi figure as a BigInt for the sake of values
 * no position on this chain holds, anything above 2^53-1 is refused outright:
 * a clear error beats an off-by-one-satoshi burn.
 */
const MAX_SAFE_SATS = Number.MAX_SAFE_INTEGER;

// Output "kinds" a plan can contain, in the order they are emitted. Named
// rather than positional so app.js and the tests can talk about them
// without hard-coding indices, and so buildTransferTx has a single switch
// mapping each to its script.
export const OUTPUT_TOKEN_RECIPIENT = 'token-recipient';
export const OUTPUT_TOKEN_CHANGE = 'token-change';
export const OUTPUT_WHIP_CHANGE = 'whip-change';

/**
 * Normalize a pubkey given as hex or bytes. Returns null if it is not a real
 * secp256k1 public key.
 *
 * Shape is checked first -- compressed (33-byte, 0x02/0x03) or uncompressed
 * (65-byte, 0x04) -- and then the point itself, because shape alone is not
 * enough at either layer. ParseUapOutputScript accepts ANY 33/65-byte push as
 * "a pubkey", and a compressed key is only a prefix and an x coordinate:
 * roughly half of all 32-byte x values have no square root on the curve and
 * so name no point at all. Such a key has no private key, can never produce a
 * signature, and a covenant paid to it is unspendable by everyone forever.
 * That is indistinguishable from burning the tokens, so it is refused here
 * rather than discovered later.
 *
 * secp.js is the wrapper over the vendored noble bundle; Point.fromBytes
 * decompresses and calls assertValidity, throwing for anything off-curve.
 * @private
 */
function normalizePubKey(value) {
  let bytes = null;
  if (value instanceof Uint8Array) {
    bytes = value;
  } else if (typeof value === 'string') {
    const hex = value.trim();
    if (!/^[0-9a-fA-F]+$/.test(hex) || hex.length % 2 !== 0) return null;
    try {
      bytes = uap.hexToBytes(hex.toLowerCase());
    } catch (e) {
      return null;
    }
  } else {
    return null;
  }
  const shapeOk =
    (bytes.length === 33 && (bytes[0] === 0x02 || bytes[0] === 0x03)) ||
    (bytes.length === 65 && bytes[0] === 0x04);
  if (!shapeOk) return null;
  try {
    secp.Point.fromBytes(bytes);
  } catch (e) {
    return null;
  }
  return bytes;
}

function sameBytes(a, b) {
  if (!a || !b || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

// Opcodes needed to read a covenant script back apart. Kept local and few:
// this module reads scripts, it does not write them (uap.js does that).
const OP_PUSHDATA1 = 0x4c;
const OP_PUSHDATA2 = 0x4d;
const OP_PUSHDATA4 = 0x4e;
const OP_0 = 0x00;
const OP_1 = 0x51;
const OP_16 = 0x60;
const OP_MINT = 0xb5;
const OP_MINT_TRANSFER = 0xba;

/**
 * Read one instruction, mirroring CScript::GetOp. Returns
 * {opcode, data, next} -- `data` is null for a non-push opcode -- or null if
 * the script ends mid-instruction.
 * @private
 */
function readOp(script, pc) {
  if (pc >= script.length) return null;
  const opcode = script[pc++];
  if (opcode > OP_PUSHDATA4) return { opcode, data: null, next: pc };
  let size = opcode;
  if (opcode === OP_PUSHDATA1) {
    if (script.length - pc < 1) return null;
    size = script[pc++];
  } else if (opcode === OP_PUSHDATA2) {
    if (script.length - pc < 2) return null;
    size = script[pc] | (script[pc + 1] << 8);
    pc += 2;
  } else if (opcode === OP_PUSHDATA4) {
    if (script.length - pc < 4) return null;
    size = (script[pc] | (script[pc + 1] << 8) | (script[pc + 2] << 16) | (script[pc + 3] << 24)) >>> 0;
    pc += 4;
  }
  if (script.length - pc < size) return null;
  return { opcode, data: script.subarray(pc, pc + size), next: pc + size };
}

/** Decode a CScriptNum (little-endian, sign-magnitude top bit). @private */
function scriptNum(bytes) {
  if (bytes.length === 0) return 0;
  if (bytes.length > 4) return null;
  let magnitude = 0;
  for (let i = 0; i < bytes.length; i++) magnitude += bytes[i] * Math.pow(256, i);
  const top = bytes[bytes.length - 1];
  if (top & 0x80) return -(magnitude - 0x80 * Math.pow(256, bytes.length - 1));
  return magnitude;
}

/**
 * Parse a UAP covenant script, mirroring ParseUapOutputScript in
 * src/script/script.cpp. Returns {pubkey, multiplier, isMint} or null.
 *
 * Canonicality is enforced by re-encoding rather than by a second copy of
 * CheckMinimalPush: uap.js's builders emit exactly the one encoding
 * consensus accepts (a direct push for the pubkey and the salt, OP_0 /
 * OP_1..OP_16 / a minimal CScriptNum push for the multiplier), so a script
 * that does not re-serialize to itself was not canonically encoded and is
 * not a valid covenant. That keeps the accepted set defined by the same code
 * that writes the scripts, instead of by a parser that could drift from it.
 * @private
 */
function parseUapCovenant(script) {
  if (!(script instanceof Uint8Array)) return null;

  const keyOp = readOp(script, 0);
  if (!keyOp || !keyOp.data) return null;
  if (keyOp.data.length !== 33 && keyOp.data.length !== 65) return null;
  const pubkey = keyOp.data;

  const multOp = readOp(script, keyOp.next);
  if (!multOp) return null;
  let multiplier;
  if (multOp.opcode === OP_0) {
    multiplier = 0;
  } else if (multOp.opcode >= OP_1 && multOp.opcode <= OP_16) {
    multiplier = multOp.opcode - (OP_1 - 1);
  } else if (multOp.data) {
    multiplier = scriptNum(multOp.data);
    if (multiplier === null) return null;
  } else {
    return null;
  }
  if (!Number.isInteger(multiplier) || multiplier < 0 || multiplier > MAX_MULTIPLIER) return null;

  const tailOp = readOp(script, multOp.next);
  if (!tailOp) return null;

  if (tailOp.opcode === OP_MINT_TRANSFER) {
    if (tailOp.next !== script.length) return null;
    if (!sameBytes(script, uap.buildTransferScript(pubkey, multiplier))) return null;
    return { pubkey, multiplier, isMint: false };
  }

  // Otherwise the third element must have been the salt push, then OP_MINT.
  if (!tailOp.data || tailOp.data.length < 16) return null;
  const mintOp = readOp(script, tailOp.next);
  if (!mintOp || mintOp.opcode !== OP_MINT || mintOp.next !== script.length) return null;
  // uap.js's pushData throws above 0xffff bytes, and consensus does NOT: a
  // PUSHDATA4 of a 64KiB+ salt is a minimal push as far as CheckMinimalPush
  // is concerned (script.cpp), so such a script can really exist and can
  // really be served to this wallet. A parser must answer "not a covenant I
  // can verify" for it, not throw -- planTransfer's contract is
  // {ok: false, errors}, and app.js's planSendTx does not catch.
  let reencoded;
  try {
    reencoded = uap.buildMintScript(pubkey, multiplier, tailOp.data);
  } catch (e) {
    return null;
  }
  if (!sameBytes(script, reencoded)) return null;
  return { pubkey, multiplier, isMint: true };
}

/**
 * Decode and fully validate a position's `script_hex` before it can be used
 * as a sighash scriptCode.
 *
 * THIS IS THE ONE CHECK THAT STOPS THE WALLET BEING A SIGNING ORACLE.
 * `script_hex` comes off the wire from an indexer, and whatever is in it is
 * exactly what the resulting signature authorises -- consensus checks the
 * signature against the scriptCode, not against anything the user was shown.
 * An unvalidated string here lets whoever serves the position choose what
 * the user's key signs. So it is proven to be a genuine UAP covenant, for
 * the pubkey and multiplier the position claims, before a byte of it reaches
 * signatureHash().
 *
 * `expectIsMint` may be null to skip the mint/transfer form check.
 * Returns {ok: true, script, parsed} or {ok: false, message}.
 * @private
 */
function validatePositionScript(scriptHex, expectedPubKey, expectedMultiplier, expectIsMint) {
  let bytes = null;
  if (scriptHex instanceof Uint8Array) {
    bytes = scriptHex;
  } else if (typeof scriptHex === 'string') {
    const hex = scriptHex.trim();
    if (!/^[0-9a-fA-F]+$/.test(hex) || hex.length % 2 !== 0) {
      return { ok: false, message: 'Position script_hex is not an even-length hex string' };
    }
    try {
      bytes = uap.hexToBytes(hex.toLowerCase());
    } catch (e) {
      return { ok: false, message: 'Position script_hex is not an even-length hex string' };
    }
  } else {
    return { ok: false, message: 'Position script_hex must be a hex string or bytes' };
  }

  const parsed = parseUapCovenant(bytes);
  if (!parsed) {
    return {
      ok: false,
      message: 'Position script_hex is not a canonically encoded UAP covenant ' +
        '(<pubkey> <multiplier> OP_MINT_TRANSFER, or <pubkey> <multiplier> <salt> OP_MINT). ' +
        'Signing over it would authorise a script this wallet cannot verify.'
    };
  }
  if (expectIsMint !== null && parsed.isMint !== expectIsMint) {
    return {
      ok: false,
      message: parsed.isMint
        ? 'Position script_hex is a fresh mint script but the position is not marked as a mint'
        : 'Position script_hex is a transfer covenant but the position is marked as a fresh mint'
    };
  }
  if (expectedPubKey && !sameBytes(parsed.pubkey, expectedPubKey)) {
    return {
      ok: false,
      message: 'Position script_hex is a covenant for a different public key than the position records; ' +
        'refusing to sign over it'
    };
  }
  if (expectedMultiplier !== null && parsed.multiplier !== expectedMultiplier) {
    return {
      ok: false,
      message: `Position script_hex carries multiplier ${parsed.multiplier}, but the position records ` +
        `${expectedMultiplier}; refusing to sign over it`
    };
  }
  return { ok: true, script: bytes, parsed };
}

/**
 * Estimate the size in bytes of a transfer transaction.
 *
 * A covenant output is bigger than a P2PKH one (a 33-byte pubkey push plus
 * the multiplier plus the opcode, ~40 bytes of script against P2PKH's 25),
 * so they are counted separately rather than folded into one "outputs"
 * number the way mint.js does -- under-estimating size means
 * under-estimating fee, which means a transaction the network will not
 * relay.
 * @private
 */
function estimateTransferSize(nInputs, nCovenantOutputs, nP2PKHOutputs) {
  let size = 10;                      // version + varints + locktime
  size += nInputs * 150;              // worst case: P2PKH-sized scriptSig
  size += nCovenantOutputs * 49;      // value(8) + varint(1) + script(~40)
  size += nP2PKHOutputs * 34;         // value(8) + varint(1) + script(25)
  return size;
}

function feeFor(size, feeRate) {
  const estimated = Math.ceil(size * feeRate / 1000);
  return Math.max(estimated, RECOMMENDED_MIN_TX_FEE);
}

/**
 * The virtual balance an output carries, expressed in COIN-denominated token
 * units for display.
 *
 * Consensus computes balance as `nValue * multiplier` in RAW SATOSHIS
 * (interpreter.cpp OP_INSPECT selector 11), and enforces conservation on raw
 * satoshis too (`nValueOut > nValueIn` fails). There is no division and no
 * truncation anywhere in that path, so balance is exactly proportional to
 * satoshi value: a split at a fractional-coin boundary rounds NOTHING away.
 *
 * The `/ COIN` here is a presentation scale factor, not consensus, and it
 * COSTS exactness rather than preserving it. COIN is 1e8, not a power of two,
 * so dividing by it is not representable: 1/1e8 is 1.0000000000000000209e-8
 * and 3/1e8 is 2.9999999999999997319e-8. Real plans do diverge -- value
 * 507015174288 sent as 58769259105 gives an "in" of 5070.15174288 and an
 * "out" of 5070.151742880001. Nothing here is exact, and nothing downstream
 * may treat it as though it were.
 *
 * It is done anyway because the alternative is worse for the one job these
 * figures have, which is being shown to a person: the exact satoshi product
 * is `value * multiplier`, and with consensus capping (value/COIN) *
 * multiplier at 2^48 that product reaches ~2^48 * COIN, far past
 * Number.MAX_SAFE_INTEGER, where the error is whole satoshis rather than
 * float dust.
 *
 * The invariant that actually gets enforced is therefore the satoshi one --
 * integer, exact, and checked against the built outputs (tokenValueIn ==
 * tokenValueOut). These coin-denominated numbers are for display only.
 * @private
 */
function virtualBalanceOf(valueSats, multiplier) {
  return (valueSats / COIN) * multiplier;
}

/**
 * Validate a token transfer and plan it.
 *
 * Returns { ok: true, plan } or { ok: false, errors: [{field, message}] },
 * reporting every validation failure at once rather than just the first.
 *
 * @param {Object} input
 * @param {Object} input.position - the UAP position being spent:
 *   {txid, vout, value, multiplier, pubkey (hex), is_mint} as returned by
 *   GET /api/positions. `pubkey` is required: without it there is no
 *   ownership to check. `script_hex` may be supplied to spend a position
 *   whose script this wallet cannot reconstruct (see below); it is validated
 *   against `pubkey` and `multiplier` before it is signed over. Set
 *   `value_verified: true` if `value` came from a source better than the
 *   indexer, to suppress the warning about it.
 * @param {Uint8Array|string} input.ownPubKey - this wallet's pubkey; must be
 *   the one embedded in the position, or the position is not ours to spend
 * @param {Uint8Array|string} input.toPubKey - recipient's pubkey (33/65 bytes)
 * @param {string} [input.toAddress] - recipient's address, cross-checked
 *   against toPubKey when present
 * @param {string} [input.network='mainnet'] - network for that cross-check
 * @param {number} input.amountSats - how much of the position's value to send
 * @param {Array} input.fundingUtxos - ordinary P2PKH UTXOs used to pay the fee
 * @param {number} input.feeRate - sat/kB
 * @returns {Object} { ok: true, plan } or { ok: false, errors }. A plan
 *   carries `warnings`: [{code, field, message}] -- things true of a
 *   perfectly valid transfer that the user should still be told.
 */
export function planTransfer({
  position,
  ownPubKey,
  toPubKey,
  toAddress,
  network = 'mainnet',
  amountSats,
  fundingUtxos,
  feeRate
}) {
  const errors = [];

  // ========== POSITION ==========
  let multiplier = null;
  let positionValue = null;
  let positionOwner = null;
  if (!position || typeof position !== 'object') {
    errors.push({ field: 'position', message: 'No position selected to send from' });
  } else {
    if (typeof position.txid !== 'string' || !/^[0-9a-fA-F]{64}$/.test(position.txid)) {
      errors.push({ field: 'position', message: 'Position txid is not a 64-character hex string' });
    }
    if (!Number.isInteger(position.vout) || position.vout < 0) {
      errors.push({ field: 'position', message: 'Position vout must be a non-negative integer' });
    }
    if (!Number.isInteger(position.value) || position.value <= 0) {
      errors.push({ field: 'position', message: 'Position value must be a positive integer number of satoshis' });
    } else if (position.value > MAX_SAFE_SATS) {
      // See MAX_SAFE_SATS: past this point splitting a value and adding the
      // pieces back together does not return the value, so a plan can report
      // perfect conservation while building outputs that burn or invent
      // satoshis.
      errors.push({
        field: 'position',
        message: `Position value of ${position.value} satoshis is larger than ${MAX_SAFE_SATS}, ` +
          'which this wallet cannot split without losing or inventing satoshis. It refuses to try.'
      });
    } else {
      positionValue = position.value;
    }
    if (!Number.isInteger(position.multiplier) || position.multiplier < 0 || position.multiplier > MAX_MULTIPLIER) {
      errors.push({
        field: 'position',
        message: `Position multiplier must be an integer in [0, ${MAX_MULTIPLIER}]`
      });
    } else {
      multiplier = position.multiplier;
    }
    if (position.spent === true) {
      errors.push({ field: 'position', message: 'That position has already been spent' });
    }

    // Who the position is addressed to. Absence of evidence is not evidence
    // of ownership: with no pubkey there is nothing to compare our own key
    // against, and treating that as a pass means the wallet will happily
    // plan -- and sign -- a spend of a covenant it may hold no key for. The
    // failure is silent (a signature that simply does not verify, or worse,
    // a position that was never ours appearing spendable), so it is an
    // error here rather than a skipped check.
    if (position.pubkey === undefined || position.pubkey === null || position.pubkey === '') {
      errors.push({
        field: 'position',
        message: 'Position does not record which public key it is addressed to, ' +
          'so this wallet cannot verify that it owns it'
      });
    } else {
      positionOwner = normalizePubKey(position.pubkey);
      if (!positionOwner) {
        errors.push({ field: 'position', message: 'Position has a malformed recipient pubkey' });
      }
    }

    // A fresh OP_MINT position's script is `<pubkey> <mult> <salt> OP_MINT`,
    // and the salt is not recoverable from anything the indexer serves
    // (see the Position struct in contrib/uap-indexer/index.go -- no salt,
    // no script hex). The scriptCode is what the signature commits to, so
    // without it a signature over a guessed script is simply invalid. Say
    // so instead of building an unspendable transaction; `script_hex` is
    // the escape hatch for a caller that does know the script.
    if (position.is_mint === true && !position.script_hex) {
      errors.push({
        field: 'position',
        message: 'This is a freshly minted position and its salt is not available from the indexer, ' +
          'so this wallet cannot sign a spend of it. Transfer it once from a wallet that has the salt first.'
      });
    }

    // The scriptCode gate. See validatePositionScript: an unvalidated
    // script_hex makes this wallet a signing oracle for whoever served the
    // position, so it is checked here (to report a readable error) and
    // again in positionScriptOf (which is where it would actually be used).
    if (position.script_hex) {
      const check = validatePositionScript(
        position.script_hex,
        positionOwner,
        multiplier,
        position.is_mint === true
      );
      if (!check.ok) {
        errors.push({ field: 'position', message: check.message });
      }
    }
  }

  // ========== OWNERSHIP ==========
  const own = normalizePubKey(ownPubKey);
  if (!own) {
    errors.push({ field: 'ownPubKey', message: 'Wallet public key is missing or malformed' });
  } else if (positionOwner && !sameBytes(positionOwner, own)) {
    errors.push({
      field: 'position',
      message: 'This position is addressed to a different public key; this wallet cannot spend it'
    });
  }

  // ========== RECIPIENT ==========
  const to = normalizePubKey(toPubKey);
  if (!to) {
    errors.push({
      field: 'toPubKey',
      message: 'Recipient public key must be 33 bytes (starting 02/03) or 65 bytes (starting 04), in hex'
    });
  }

  if (toAddress !== undefined && toAddress !== null && String(toAddress).trim() !== '') {
    const trimmed = String(toAddress).trim();
    let decoded = null;
    try {
      decoded = addr.base58checkDecode(trimmed);
    } catch (e) {
      errors.push({ field: 'toAddress', message: 'Recipient address is not a valid Whippet address' });
    }
    if (decoded) {
      const versions = addr.VERSIONS[network];
      if (!versions) {
        errors.push({ field: 'network', message: `Unknown network: ${network}` });
      } else if (decoded.version === versions.SCRIPT_ADDRESS) {
        // Deliberately distinct from "wrong network": nothing in this
        // wallet can build a spendable P2SH output (addr.addressToScript
        // throws rather than mis-encoding one), so this address can never
        // be paid, on any network.
        errors.push({
          field: 'toAddress',
          message: 'P2SH addresses are not supported by this wallet; ask for a regular (pay-to-pubkey-hash) address'
        });
      } else if (decoded.version !== versions.PUBKEY_ADDRESS) {
        errors.push({
          field: 'toAddress',
          message: `That address is not a ${network} address`
        });
      } else if (to && !sameBytes(addr.hash160(to), decoded.payload)) {
        // The whole point of accepting both: an address the user obtained
        // out of band proves the pasted pubkey belongs to the intended
        // recipient. A mismatch means one of the two is wrong, and sending
        // to the wrong covenant is unrecoverable.
        errors.push({
          field: 'toAddress',
          message: 'The recipient public key does not match that address (hash160 mismatch). ' +
            'Do not send: one of the two belongs to someone else.'
        });
      }
    }
  }

  // ========== AMOUNT ==========
  let remainderSats = null;
  if (!Number.isInteger(amountSats) || amountSats <= 0) {
    errors.push({ field: 'amountSats', message: 'Amount must be a positive whole number of satoshis' });
  } else if (amountSats > MAX_SAFE_SATS) {
    // Same reason as the position value above, and checked separately: an
    // amount can be unsafe on its own even when it is under a (rejected or
    // absent) position value.
    errors.push({
      field: 'amountSats',
      message: `Amount of ${amountSats} satoshis is larger than ${MAX_SAFE_SATS}, ` +
        'which this wallet cannot compute a remainder against without losing or inventing satoshis.'
    });
  } else if (positionValue !== null) {
    if (amountSats > positionValue) {
      errors.push({
        field: 'amountSats',
        message: `Amount ${amountSats} satoshis exceeds the position's value of ${positionValue} satoshis`
      });
    } else {
      remainderSats = positionValue - amountSats;
      if (amountSats < HARD_DUST_LIMIT) {
        errors.push({
          field: 'amountSats',
          message: `Amount ${amountSats} satoshis is below the dust limit of ${HARD_DUST_LIMIT} satoshis`
        });
      }
      if (remainderSats > 0 && remainderSats < HARD_DUST_LIMIT) {
        errors.push({
          field: 'amountSats',
          message: `Sending this much would leave a dust remainder of ${remainderSats} satoshis in your position. ` +
            `Send at most ${positionValue - HARD_DUST_LIMIT} satoshis, or send the whole position.`
        });
      }
    }
  }

  // ========== FEE FUNDING ==========
  if (!Number.isFinite(feeRate) || feeRate <= 0) {
    errors.push({ field: 'feeRate', message: 'Fee rate must be a positive number of satoshis per kB' });
  }
  if (!Array.isArray(fundingUtxos) || fundingUtxos.length === 0) {
    errors.push({
      field: 'fundingUtxos',
      message: 'A token transfer needs an ordinary WHIP input to pay the fee, so the tokens themselves are not spent on it. ' +
        'This wallet has no spendable WHIP UTXOs.'
    });
  }

  // Anything below needs valid numbers to work with.
  if (errors.length > 0) {
    return { ok: false, errors };
  }

  const nCovenantOutputs = remainderSats > 0 ? 2 : 1;
  const selection = selectFunding(fundingUtxos, feeRate, nCovenantOutputs);
  if (!selection.ok) {
    errors.push({ field: 'fundingUtxos', message: selection.message });
    return { ok: false, errors };
  }

  const outputs = [
    { kind: OUTPUT_TOKEN_RECIPIENT, value: amountSats, pubKey: to, multiplier }
  ];
  if (remainderSats > 0) {
    outputs.push({ kind: OUTPUT_TOKEN_CHANGE, value: remainderSats, pubKey: own, multiplier });
  }
  if (selection.change > 0) {
    outputs.push({ kind: OUTPUT_WHIP_CHANGE, value: selection.change, pubKey: own });
  }

  const balanceIn = virtualBalanceOf(positionValue, multiplier);
  const covenantSatsOut = outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + o.value, 0);
  // Scaled PER OUTPUT and then summed, exactly as consensus evaluates each
  // output separately -- not scaled once off the satoshi total. Because the
  // scaling is inexact (see virtualBalanceOf) the two do not always agree to
  // the last bit; that divergence is float dust in a display figure, and it
  // is deliberately not hidden by summing first.
  const balanceOut = outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + virtualBalanceOf(o.value, multiplier), 0);
  // Derived from the satoshi difference rather than from balanceIn -
  // balanceOut, which would leave float residue. Note that this is
  // `V - (a + (V - a))` and so is structurally zero for every plan this
  // function emits: it is a reported figure, not a check. Conservation is
  // checked on tokenValueIn/tokenValueOut, which are integers.
  const balanceLost = virtualBalanceOf(positionValue - covenantSatsOut, multiplier);

  // Things that are true of this plan but are not errors: it can still be
  // signed and broadcast, and the user is the one who should decide.
  const warnings = [];
  if (position.value_verified !== true) {
    // F2. The position's value is whatever the indexer said it was, and
    // nothing in this module can check it: the authority is the UTXO set,
    // and confirming it needs the funding transaction's own bytes or a node
    // query -- neither of which a pure planner has. The two directions of
    // error are not symmetric, which is why this is a warning and not a
    // refusal:
    //
    //   OVERSTATED -> the covenant outputs total more than the real input,
    //     consensus fails the transaction (nValueOut > nValueIn) and nothing
    //     is lost. Loud, and safe.
    //   UNDERSTATED -> the outputs total less than the real input, the
    //     transaction is perfectly valid, and the difference leaves the
    //     covenant as extra fee. Tokens destroyed, silently, with a
    //     confirmation to show for it.
    //
    // Only the silent direction needs saying, so it is said. A caller that
    // obtained the value from a source it trusts (its own UTXO scan, a node)
    // marks the position `value_verified: true` and this goes away.
    warnings.push({
      code: 'position-value-unverified',
      field: 'position',
      message: `This position's value of ${positionValue} satoshis is as reported by the indexer and ` +
        'cannot be verified here. If the real output holds more than that, the difference will be ' +
        'paid to miners as fee instead of moving with your tokens.'
    });
  }

  const plan = {
    position,
    multiplier,
    warnings,
    toPubKey: to,
    ownPubKey: own,
    amountSats,
    remainderSats,
    // Satoshi conservation is the invariant that matters, and it is exact
    // because both figures are integers no larger than MAX_SAFE_SATS (see
    // there for why anything larger is refused rather than planned). `out` is
    // summed off the OUTPUT LIST that buildTransferTx will actually encode,
    // not off `amountSats + remainderSats`, so it cannot agree with the
    // planner's intent while disagreeing with the transaction.
    tokenValueIn: positionValue,
    tokenValueOut: covenantSatsOut,
    // Display figures, in coin-denominated token units. NOT exact -- see
    // virtualBalanceOf -- and not the conservation check: virtualBalanceLost
    // is `V - (a + (V - a))` scaled, which is structurally zero for any plan
    // shape this function can emit, so it can never catch anything. The real
    // check is tokenValueIn === tokenValueOut above.
    virtualBalanceIn: balanceIn,
    virtualBalanceOut: balanceOut,
    virtualBalanceLost: balanceLost,
    fundingInputs: selection.selected,
    fundingTotal: selection.total,
    fee: selection.fee,
    change: selection.change,
    feeRate,
    outputs
  };

  return { ok: true, plan };
}

/**
 * Greedily select ordinary UTXOs to cover the fee.
 *
 * Smallest-first, like uap.js's selectCoins: it consolidates small UTXOs
 * rather than shredding a large one. If the leftover would be dust it is
 * dropped into the fee instead of becoming an unrelayable output -- and the
 * reported fee is then the real one (total - change), never the estimate,
 * so the review screen cannot show a number the transaction does not pay.
 * @private
 */
function selectFunding(fundingUtxos, feeRate, nCovenantOutputs) {
  const usable = fundingUtxos.filter(
    (u) => u && Number.isInteger(u.value) && u.value > 0 && typeof u.txid === 'string' && Number.isInteger(u.vout)
  );
  if (usable.length === 0) {
    return { ok: false, message: 'No usable WHIP UTXOs to pay the fee with (each needs txid, vout and a positive integer value)' };
  }

  const sorted = [...usable].sort((a, b) => a.value - b.value);
  const selected = [];
  let total = 0;

  for (const utxo of sorted) {
    selected.push(utxo);
    total += utxo.value;

    // 1 covenant input + the funding inputs; covenant outputs plus a
    // possible P2PKH change output.
    const feeWithChange = feeFor(
      estimateTransferSize(1 + selected.length, nCovenantOutputs, 1), feeRate);

    if (total >= feeWithChange) {
      let change = total - feeWithChange;
      let fee = feeWithChange;
      if (change < DEFAULT_DUST_LIMIT) {
        // Not worth an output: it becomes fee. Recompute nothing -- the
        // fee simply is whatever is left over.
        change = 0;
        fee = total;
      }
      return { ok: true, selected, total, fee, change };
    }
  }

  const needed = feeFor(estimateTransferSize(1 + selected.length, nCovenantOutputs, 1), feeRate);
  return {
    ok: false,
    message: `Insufficient WHIP to pay the transfer fee: need about ${needed} satoshis, have ${total} satoshis`
  };
}

/**
 * Build and sign a transfer transaction from a plan.
 *
 * Input 0 is the UAP position, signed with uap.signSpend (a bare `<sig>`
 * scriptSig -- a covenant output is not P2PKH and must not be signed like
 * one). The remaining inputs are ordinary P2PKH and are signed with
 * uap.signP2PKHInput. Every input uses SIGHASH_ALL, so each signature
 * commits to the full output set: the covenant outputs cannot be swapped
 * out after signing.
 *
 * @param {Object} options
 * @param {Object} options.secp - secp256k1 instance (see uap.signSpend)
 * @param {Object} options.plan - plan from planTransfer
 * @param {Uint8Array} options.privKey - this wallet's private key
 * @returns {Promise<Object>} { rawHex, tx, outputs }
 */
export async function buildTransferTx({ secp, plan, privKey }) {
  if (!plan || !Array.isArray(plan.outputs) || plan.outputs.length === 0) {
    throw new Error('buildTransferTx requires a plan from planTransfer');
  }
  if (!privKey) throw new Error('buildTransferTx requires a private key');

  const positionScript = positionScriptOf(plan);

  const vout = plan.outputs.map((out) => {
    if (out.kind === OUTPUT_WHIP_CHANGE) {
      return { value: out.value, scriptPubKey: addr.buildP2PKHScript(out.pubKey) };
    }
    // Both token outputs are the same covenant format, built by the same
    // uap-js helper fillOrder() uses. Nothing about the script is
    // assembled here.
    return { value: out.value, scriptPubKey: uap.buildTransferScript(out.pubKey, out.multiplier) };
  });

  const tx = {
    version: 1,
    locktime: 0,
    vin: [
      {
        txid: plan.position.txid,
        vout: plan.position.vout,
        scriptSig: new Uint8Array(0),
        sequence: 0xffffffff
      },
      ...plan.fundingInputs.map((u) => ({
        txid: u.txid,
        vout: u.vout,
        scriptSig: new Uint8Array(0),
        sequence: 0xffffffff
      }))
    ],
    vout
  };

  // The covenant input: a bare signature push, over the position's own script.
  tx.vin[0].scriptSig = uap.signSpend(secp, positionScript, privKey, tx, 0);

  // The fee-funding inputs: ordinary P2PKH.
  const ownScript = addr.buildP2PKHScript(plan.ownPubKey);
  for (let i = 1; i < tx.vin.length; i++) {
    const utxo = plan.fundingInputs[i - 1];
    const scriptCode = utxo.scriptPubKey
      ? (typeof utxo.scriptPubKey === 'string' ? uap.hexToBytes(utxo.scriptPubKey) : utxo.scriptPubKey)
      : ownScript;
    tx.vin[i].scriptSig = uap.signP2PKHInput(secp, scriptCode, privKey, plan.ownPubKey, tx, i);
  }

  return {
    rawHex: uap.txToHex(tx),
    tx,
    outputs: plan.outputs
  };
}

/**
 * The scriptPubKey of the position being spent -- the scriptCode the
 * signature must commit to.
 *
 * For an OP_MINT_TRANSFER position this is fully determined by fields the
 * indexer publishes, and rebuilding it with the same helper that created it
 * is safer than trusting a hex string over the wire. An explicit
 * `script_hex` wins where one is supplied (the only way to spend a fresh
 * mint, whose salt is not published) -- but only after it has been proven to
 * be the covenant this plan is about.
 *
 * That proof is repeated here even though planTransfer already made it. The
 * plan holds a *reference* to the caller's position object, so anything that
 * can touch that object between planning and signing chooses the scriptCode
 * otherwise; and buildTransferTx is an exported function that can be handed
 * a plan this module never produced. The check belongs at the point of use,
 * which is here, and it throws rather than returning an error because there
 * is no safe way to continue.
 *
 * It is the SAME check planTransfer makes, mint/transfer form included. That
 * last part is the whole point: a mint covenant is `<pubkey> <mult> <salt>
 * OP_MINT`, and the salt is arbitrary bytes of arbitrary length. Skipping the
 * form check here (by passing null) would let an attacker who can mutate the
 * position object swap the transfer covenant for a mint-form one carrying a
 * blob of their choosing, and get the user's key to sign a preimage over it
 * -- exactly the substitution this function exists to stop.
 * @private
 */
function positionScriptOf(plan) {
  const position = plan.position;
  if (position.script_hex) {
    const check = validatePositionScript(
      position.script_hex, plan.ownPubKey, plan.multiplier, position.is_mint === true);
    if (!check.ok) {
      throw new Error(`refusing to sign over this position's script: ${check.message}`);
    }
    return check.script;
  }
  if (position.is_mint === true) {
    throw new Error('cannot reconstruct a fresh mint position\'s script without its salt');
  }
  return uap.buildTransferScript(plan.ownPubKey, plan.multiplier);
}
