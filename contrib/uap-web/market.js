// market.js - Pure logic for order book and fill transactions
// No DOM, no async signing - all logic is testable and deterministic

import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';

const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;
const RECOMMENDED_MIN_TX_FEE = uap.RECOMMENDED_MIN_TX_FEE;

/**
 * Describe an order as a human-readable summary.
 *
 * The order model from orders.go specifies:
 * - The token being sold (position: txid, vout, multiplier, value)
 * - The payment the maker wants (payment_script, payment_value)
 *
 * This function produces a string like:
 *   "Sell 1000 coins (mult=100) @ 5000000 sats"
 *
 * Fields:
 * - Token quantity is in the position (not in the order itself)
 * - Payment amount is order.payment_value (in satoshis)
 *
 * @param {Object} order - Order object from the API
 * @returns {string} Human-readable description
 */
export function describeOrder(order) {
  const paymentBtc = (order.payment_value / COIN).toFixed(8);
  const side = 'SELL';

  return `${side} multiplier=${order.multiplier} @ ${order.payment_value} sats (maker: ${order.pubkey.slice(0, 16)}...)`;
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
  if (order.payment_value < DEFAULT_DUST_LIMIT) {
    errors.push(`Payment value ${order.payment_value} sats is below dust limit ${DEFAULT_DUST_LIMIT} sats`);
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
  const makerOrder = {
    input: {
      txid: order.txid,
      vout: order.vout
    },
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
