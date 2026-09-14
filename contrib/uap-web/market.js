// market.js - Pure logic for order book and fill transactions
// No DOM, no async signing - all logic is testable and deterministic

import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';

const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;
// Relay standardness uses the HARD limit (IsStandardTx -> nHardDustLimit in
// src/policy/policy.cpp); the soft limit above only drives fee bumping and
// change-folding policy. Any value that becomes a real output is gated by
// this one.
const HARD_DUST_LIMIT = uap.DEFAULT_HARD_DUST_LIMIT;
const RECOMMENDED_MIN_TX_FEE = uap.RECOMMENDED_MIN_TX_FEE;

/**
 * Describe an order as a one-line summary, for the order book list.
 *
 * The asking price alone does not tell a buyer anything. A position is its
 * backing: the satoshis locked in it ARE the token, at a fixed ratio, and
 * anyone may melt one back down (see buildMeltTx). So the number that makes
 * an ask legible is what it is backed by -- 40 coins for a position holding
 * 1000 is a very different offer from 40 coins for a position holding 41 --
 * and without it the book is a list of prices for unnamed things.
 *
 * The relay serves that as `backing_value`, read off the position at publish
 * time so a maker cannot overstate it (see orders.go). It is used here rather
 * than fetched per row, which is what it was added for.
 *
 * Older relays, and any order stored before backing_value existed, may not
 * carry it. Those are described without it rather than as "backed by 0",
 * which would read as a specific and alarming claim rather than an absence.
 *
 * @param {Object} order - Order object from the API
 * @returns {string} Human-readable description
 */
export function describeOrder(order) {
  const maker = String(order.pubkey || '').slice(0, 16);
  const price = (order.payment_value / COIN).toFixed(8);
  const parts = [`SELL multiplier=${order.multiplier}`];

  if (typeof order.backing_value === 'number' && order.backing_value > 0) {
    parts.push(`backed by ${(order.backing_value / COIN).toFixed(8)} coins`);
    // Against par, because par is the floor: the holder can always melt.
    // An ask below par is a discount to what the position could be redeemed
    // for; one above it is a premium the buyer is choosing to pay.
    const ratio = order.payment_value / order.backing_value;
    parts.push(`@ ${price} coins (${ratio.toFixed(2)}x par)`);
  } else {
    parts.push(`@ ${price} coins`);
    parts.push('backing unknown -- this relay does not publish it');
  }
  parts.push(`${order.payment_value} sats`);

  return `${parts.join(' · ')} (maker: ${maker}...)`;
}

/**
 * Verify a position for ourselves instead of trusting the relay's
 * description of it.
 *
 * The relay (`apiClient.getPosition`) is the wallet's only source of chain
 * data, and most of its lies fail closed: a fabricated payment_value or
 * script_sig just makes the resulting fill transaction invalid, and the
 * node refuses it. `position.value` does not fail closed. Consensus for a
 * UAP covenant only requires outputs to sum to no more than inputs (see
 * CheckUapOutputConservation in src/script/interpreter.cpp) -- less is a
 * melt, which is allowed. So a relay that UNDER-reports a position's value
 * makes the taker build a covenant output smaller than the position it
 * actually spends; the difference is silently burned to miner fees, and
 * the taker pays full price for a position worth less than advertised.
 * `origin` and `multiplier` are exposed to the same trust problem on this
 * path (buildFillTx takes both from the order, not derived).
 *
 * The fix needs no trust in the relay at all. A txid is a commitment to
 * the bytes that hash to it, and the taker already has the position's
 * outpoint independently -- it is what the maker's own signature commits
 * to (script_sig), not something the relay could swap out without the
 * signature failing to verify at broadcast time. So:
 *
 *   1. Fetch the raw bytes of the transaction that created the position.
 *   2. Hash them and confirm the result IS that txid. A relay cannot
 *      produce different bytes with the same hash, so this step alone
 *      makes every field read out of the bytes below trustworthy.
 *   3. Parse the verified bytes and read the claimed output's value and
 *      scriptPubKey from them -- not from the relay's JSON.
 *   4. Decode the scriptPubKey and compare its multiplier and origin
 *      against what the order claims. Any disagreement, in value,
 *      multiplier, or origin, means the relay's JSON does not match the
 *      chain, and every one of those numbers came from the same
 *      untrustworthy source -- so ANY mismatch refuses the whole fill
 *      rather than guessing which field to believe.
 *
 * Throws with a specific, user-facing reason on any failure. Never
 * returns a "close enough" answer.
 *
 * @param {Object} options
 * @param {Object} options.api - API client exposing getRawTx(txid)
 * @param {Object} options.order - Order object from the relay
 * @param {Object} options.position - Position object from the relay
 * @returns {Promise<Object>} { value, scriptPubKey, multiplier, origin, pubkey } -- all read from the verified bytes, not the relay's JSON
 */
export async function verifyPosition({ api, order, position }) {
  if (!order || typeof order.txid !== 'string' || typeof order.vout !== 'number') {
    throw new Error('verifyPosition: order is missing a txid/vout to verify against');
  }

  const rawHex = await api.getRawTx(order.txid);

  // Step 1+2: the bytes must actually hash to the txid we asked for. This
  // is the whole trick -- everything past this line is read from bytes we
  // have independently confirmed the relay cannot have forged, no matter
  // how hostile it is.
  const actualTxid = uap.txidFromBytes(rawHex);
  if (actualTxid !== order.txid) {
    throw new Error(
      `Position verification failed: the relay served transaction bytes that ` +
      `hash to ${actualTxid}, not the order's txid ${order.txid}. Refusing to ` +
      `trust anything else this relay says about this position.`
    );
  }

  let tx;
  try {
    tx = uap.deserializeTx(rawHex);
  } catch (e) {
    throw new Error(
      `Position verification failed: could not parse the (hash-verified) ` +
      `transaction ${order.txid}: ${e.message}`
    );
  }

  // Step 3: read the output the order claims to be selling, from the
  // verified bytes -- not from apiClient.getPosition's JSON.
  const output = tx.vout[order.vout];
  if (!output) {
    throw new Error(
      `Position verification failed: transaction ${order.txid} has no output ` +
      `${order.vout}, but the order claims to be selling it.`
    );
  }

  const parsed = uap.parseUapScript(output.scriptPubKey);
  if (!parsed) {
    throw new Error(
      `Position verification failed: ${order.txid}:${order.vout} is not a UAP ` +
      `mint or transfer covenant on chain. Refusing to fill an order against it.`
    );
  }

  // A position for sale may be a fresh, never-transferred mint (script
  // OP_MINT) or an already-transferred covenant (OP_MINT_TRANSFER); both
  // are legitimately sellable, and fillOrder builds an OP_MINT_TRANSFER
  // output from either. Only a transfer script carries its origin as
  // bytes -- a mint output carries none at all, because a mint IS the
  // start of a lineage. Its origin is derived the same way originForSpend
  // (uap-js) derives it when signing: SHA256(the outpoint being spent).
  // Comparing against anything else here would reject every legitimate
  // first sale of a freshly minted token.
  const actualOrigin = parsed.isMint
    ? uap.bytesToHex(uap.deriveOrigin(order.txid, order.vout))
    : uap.bytesToHex(parsed.origin);

  // Step 4: cross-check every field the fill path takes on trust from the
  // relay against the verified truth. This covers `position.value` (the
  // defect this function exists to close) as well as `order.origin` and
  // `order.multiplier`, which buildFillTx also takes from the relay
  // unverified. All three came from the same untrustworthy source, so any
  // one of them disagreeing with the chain is reason enough to refuse the
  // whole fill -- there is no way to know which of the relay's other
  // claims (payment_value, script_sig, ...) are still good.
  if (typeof order.origin !== 'string' || order.origin.toLowerCase() !== actualOrigin) {
    throw new Error(
      `Position verification failed: order claims origin ${order.origin}, but ` +
      `the position's real scriptPubKey carries ${actualOrigin}. Refusing to ` +
      `fill -- this relay cannot be trusted about this order.`
    );
  }
  if (order.multiplier !== parsed.multiplier) {
    throw new Error(
      `Position verification failed: order claims multiplier ${order.multiplier}, ` +
      `but the position's real scriptPubKey carries ${parsed.multiplier}. ` +
      `Refusing to fill -- this relay cannot be trusted about this order.`
    );
  }
  if (position && typeof position.value === 'number' && position.value !== output.value) {
    throw new Error(
      `Position verification failed: the relay reported this position as worth ` +
      `${position.value} satoshis, but its real, on-chain value is ${output.value} ` +
      `satoshis. Refusing to fill -- building against the relay's figure would ` +
      `either burn the difference to fees (if it under-reports) or fail at the ` +
      `node (if it over-reports).`
    );
  }

  return {
    value: output.value,
    scriptPubKey: output.scriptPubKey,
    multiplier: parsed.multiplier,
    origin: actualOrigin,
    pubkey: uap.bytesToHex(parsed.pubkey)
  };
}

/**
 * Validate that a taker can fill an order and return a plan.
 *
 * Returns { ok: true, plan } or { ok: false, errors: [...] }.
 * Reports all validation failures at once.
 *
 * Fields:
 * - order.payment_value is the satoshis the taker must pay to the maker
 * - position.value is the token satoshis the maker is selling
 * - takerUtxos are the taker's payment inputs
 *
 * @param {Object} input
 * @param {Object} input.order - Order object from API
 * @param {Object} input.position - Position object (the token being sold)
 * @param {Array} input.takerUtxos - Taker's available UTXOs for payment
 * @param {Uint8Array} input.takerPubkey - Taker's public key (compressed)
 * @param {number} input.feeRate - Fee rate in sat/kB
 * @returns {Object} { ok: true, plan } or { ok: false, errors: [...] }
 */
export function planFill({
  order,
  position,
  takerUtxos,
  takerPubkey,
  feeRate
}) {
  const errors = [];

  // ========== ORDER VALIDATION ==========
  if (!order) {
    errors.push('Order is required');
  }
  if (!position) {
    errors.push('Position is required');
  }
  if (!takerUtxos || takerUtxos.length === 0) {
    errors.push('Taker UTXOs required');
  }
  if (!takerPubkey) {
    errors.push('Taker pubkey required');
  }

  // Early exit if basic requirements missing
  if (errors.length > 0) {
    return { ok: false, errors };
  }

  // ========== FUNDS VALIDATION ==========
  const totalUtxoValue = takerUtxos.reduce((sum, u) => sum + u.value, 0);
  const paymentRequired = order.payment_value;

  // Estimate fee for this fill transaction:
  // Inputs: 1 maker input (already signed) + N taker inputs
  // Outputs: 1 payment + 1 token transfer + 1 change (if needed)
  const nTakerInputs = takerUtxos.length;
  const nInputs = 1 + nTakerInputs;  // maker + taker inputs
  const nOutputs = 2;  // payment + token transfer (change calculated separately)
  const estimatedSize = estimateTxSize(nInputs, nOutputs + 1);  // +1 for potential change
  const estimatedFee = Math.ceil(estimatedSize * feeRate / 1000);
  const minFee = Math.max(estimatedFee, RECOMMENDED_MIN_TX_FEE);

  const totalNeeded = paymentRequired + minFee;

  if (totalUtxoValue < totalNeeded) {
    const shortfall = totalNeeded - totalUtxoValue;
    errors.push(`Insufficient funds: need ${totalNeeded} sats for payment + fee, have ${totalUtxoValue} sats. Shortfall: ${shortfall} sats`);
  }

  // ========== PAYMENT DUST CHECK ==========
  if (order.payment_value < HARD_DUST_LIMIT) {
    errors.push(`Payment value ${order.payment_value} sats is below dust limit ${HARD_DUST_LIMIT} sats`);
  }

  // ========== CHANGE VALIDATION ==========
  if (totalUtxoValue >= totalNeeded) {
    const change = totalUtxoValue - totalNeeded;
    if (change > 0 && change < DEFAULT_DUST_LIMIT) {
      errors.push(`Transaction produces dust change (${change} sats). Need more input or different payment amount.`);
    }
  }

  if (errors.length > 0) {
    return { ok: false, errors };
  }

  // ========== PLAN CREATION ==========
  const fee = minFee;
  const change = totalUtxoValue - paymentRequired - fee;

  const plan = {
    takerInputs: takerUtxos,
    paymentValue: paymentRequired,
    fee,
    change,
    feeRate,
    tokenValue: position.value
  };

  return { ok: true, plan };
}

/**
 * Build and sign a fill transaction from a plan.
 *
 * Uses uap.fillOrder to construct the transaction with:
 * - Maker's signed input at index 0 (SIGHASH_SINGLE requirement)
 * - Payment output at index 0 (committed to by maker's signature)
 * - Token transfer output at index 1 (to taker)
 * - Taker's change output (if change >= dust)
 *
 * The maker's input is already signed (from the order). The taker must sign
 * their own inputs using P2PKH signing.
 *
 * @param {Object} options
 * @param {Object} options.secp - secp256k1 instance with getPublicKey and signSync
 * @param {Object} options.plan - plan object from planFill
 * @param {Uint8Array} options.privKey - taker's private key
 * @param {Uint8Array} options.pubKey - taker's public key (compressed)
 * @param {Object} options.order - Order object (with scriptSig already filled)
 * @param {Object} options.position - Position object (the token)
 * @param {Array} options.takerUtxos - Taker's UTXOs to spend from
 * @returns {Promise<Object>} { rawHex, tx }
 */
export async function buildFillTx({
  secp,
  plan,
  privKey,
  pubKey,
  order,
  position,
  takerUtxos
}) {
  // Build the maker's order object in the format fillOrder expects:
  // { input: {txid, vout}, scriptSig, paymentScript, paymentValue }
  if (!order.origin || !/^[0-9a-f]{64}$/.test(order.origin)) {
    throw new Error(
      'this order does not publish its position\'s lineage origin, so the ' +
      'token output cannot be built: the relay must serve `origin` on every ' +
      'order (a taker cannot derive it -- see fillOrder in uap-js)'
    );
  }
  const makerOrder = {
    input: {
      txid: order.txid,
      vout: order.vout
    },
    origin: uap.hexToBytes(order.origin),
    scriptSig: uap.hexToBytes(order.script_sig),
    paymentScript: uap.hexToBytes(order.payment_script),
    paymentValue: order.payment_value
  };

  // Prepare taker inputs (just txid/vout pairs for fillOrder)
  const takerInputRefs = takerUtxos.map(u => ({ txid: u.txid, vout: u.vout }));

  // Build the unsigned transaction using fillOrder. It builds the taker's
  // covenant output itself from toPubkey + multiplier, so there is nothing
  // to construct here -- passing the multiplier through unchanged is what
  // preserves it across the transfer.
  const tx = uap.fillOrder(makerOrder, {
    toPubkey: pubKey,
    multiplier: order.multiplier,
    tokenValue: position.value,
    takerInputs: takerInputRefs,
    changeScript: plan.change >= DEFAULT_DUST_LIMIT ? addr.buildP2PKHScript(pubKey) : null,
    changeValue: plan.change >= DEFAULT_DUST_LIMIT ? plan.change : 0
  });

  // Sign the taker's inputs (starting from index 1, since 0 is maker's already-signed input)
  for (let i = 1; i < tx.vin.length; i++) {
    const takerUtxoIndex = i - 1;  // Map back to takerUtxos array
    const utxo = takerUtxos[takerUtxoIndex];

    // Build scriptCode for this UTXO (P2PKH)
    const scriptCode = addr.buildP2PKHScript(pubKey);

    // Sign with SIGHASH_ALL
    tx.vin[i].scriptSig = uap.signP2PKHInput(secp, scriptCode, privKey, pubKey, tx, i);
  }

  return {
    rawHex: uap.txToHex(tx),
    tx
  };
}

/**
 * Estimate transaction size in bytes for fee calculation.
 * @private
 */
function estimateTxSize(nInputs, nOutputs) {
  // Base: version(4) + input count varint + output count varint + locktime(4)
  let size = 10;
  // Each input: ~150 bytes
  size += nInputs * 150;
  // Each output: ~34 bytes
  size += nOutputs * 34;
  return size;
}
