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
