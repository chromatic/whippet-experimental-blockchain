// mint.js - Pure logic for planning and building mint transactions
// No DOM, no async signing - all logic is testable and deterministic

import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';

const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;

/**
 * Validate user input and plan a mint transaction.
 *
 * Returns { ok: true, plan } or { ok: false, errors: [{field, message}] }.
 * Reports all validation failures at once, not just the first.
 *
 * @param {Object} input
 * @param {string} input.ticker - 1..16 bytes UTF-8, no control chars, no leading/trailing whitespace
 * @param {string} input.name - 0..64 bytes UTF-8, no control chars
 * @param {number} input.multiplier - integer in [0, 2147483647]
 * @param {number} input.amountSats - >= 1000 * COIN
 * @param {Array} input.utxos - array of {txid, vout, value, height}
 * @param {number} input.feeRate - sat/kB for fee calculation
 * @param {string} input.address - change address
 * @returns {Object} { ok: true, plan } or { ok: false, errors }
 */
export function planMint({
  ticker,
  name,
  multiplier,
  amountSats,
  utxos,
  feeRate,
  address
}) {
  const errors = [];

  // ========== TICKER VALIDATION ==========
  if (!ticker || ticker.length === 0) {
    errors.push({
      field: 'ticker',
      message: 'Ticker is required and must be 1-16 bytes UTF-8'
    });
  } else {
    const tickerBytes = new TextEncoder().encode(ticker);
    if (tickerBytes.length > 16) {
      errors.push({
        field: 'ticker',
        message: 'Ticker must be at most 16 bytes UTF-8'
      });
    }
    // Check for control characters
    if (hasControlCharacters(ticker)) {
      errors.push({
        field: 'ticker',
        message: 'Ticker cannot contain control characters'
      });
    }
    // Check for leading/trailing whitespace
    if (ticker !== ticker.trim()) {
      errors.push({
        field: 'ticker',
        message: 'Ticker cannot have leading or trailing whitespace'
      });
    }
  }

  // ========== NAME VALIDATION ==========
  if (name !== undefined && name !== null) {
    const nameBytes = new TextEncoder().encode(name);
    if (nameBytes.length > 64) {
      errors.push({
        field: 'name',
        message: 'Name must be at most 64 bytes UTF-8'
      });
    }
    if (hasControlCharacters(name)) {
      errors.push({
        field: 'name',
        message: 'Name cannot contain control characters'
      });
    }
  }

  // ========== MULTIPLIER VALIDATION ==========
  if (!Number.isInteger(multiplier)) {
    errors.push({
      field: 'multiplier',
      message: 'Multiplier must be an integer'
    });
  } else if (multiplier < 0 || multiplier > 2147483647) {
    errors.push({
      field: 'multiplier',
      message: 'Multiplier must be in range [0, 2147483647]'
    });
  }

  // ========== AMOUNT VALIDATION ==========
  if (amountSats < 1000 * COIN) {
    errors.push({
      field: 'amountSats',
      message: `Amount must be at least 1000 coins (${1000 * COIN} satoshis), got ${amountSats} satoshis`
    });
  }

  // ========== 2^48 VIRTUAL BALANCE OVERFLOW CHECK ==========
  // (mint value in whole coins) × multiplier must not exceed 2^48
  if (Number.isInteger(multiplier) && multiplier >= 0 && amountSats >= 1000 * COIN) {
    const amountCoins = amountSats / COIN;
    const virtualBalance = amountCoins * multiplier;
    const maxBalance = Math.pow(2, 48);  // 281474976710656
    if (virtualBalance > maxBalance) {
      errors.push({
        field: 'multiplier',
        message: `This multiplier is too large for the amount you are locking (${amountCoins} coins × ${multiplier} = ${virtualBalance} > 2^48 = ${maxBalance})`
      });
    }
  }

  // ========== UTXO & FEE VALIDATION ==========
  // Calculate if we have enough UTXOs to cover amount + fee
  if (utxos && utxos.length > 0) {
    const totalUtxoValue = utxos.reduce((sum, u) => sum + u.value, 0);

    // Estimate transaction size for fee calculation
    const nInputs = utxos.length;
    const nOutputs = 2;  // mint output + change output
    const estimatedSize = estimateTxSize(nInputs, nOutputs);
    const estimatedFee = Math.ceil(estimatedSize * feeRate / 1000);
    const minFee = Math.max(estimatedFee, uap.RECOMMENDED_MIN_TX_FEE);

    const totalNeeded = amountSats + minFee;

    if (totalUtxoValue < totalNeeded) {
      const shortfallSats = totalNeeded - totalUtxoValue;
      const shortfallCoins = shortfallSats / COIN;
      errors.push({
        field: 'utxos',
        message: `Insufficient funds: need ${totalNeeded} satoshis (${amountSats} for mint + ${minFee} for fee), have ${totalUtxoValue} satoshis. Shortfall: ${shortfallCoins} coins (${shortfallSats} satoshis)`
      });
    } else {
      // Check for dust change
      const change = totalUtxoValue - totalNeeded;
      if (change > 0 && change < DEFAULT_DUST_LIMIT) {
        errors.push({
          field: 'utxos',
          message: `Transaction would produce dust change (${change} satoshis). Need either more input or slightly less output.`
        });
      }
    }
  } else {
    errors.push({
      field: 'utxos',
      message: 'No UTXOs provided'
    });
  }

  // If there are validation errors, return them all
  if (errors.length > 0) {
    return { ok: false, errors };
  }

  // ========== PLAN CREATION ==========
  // Generate a fresh salt
  const salt = uap.randomSalt(20);

  // Calculate final fee and change
  const totalUtxoValue = utxos.reduce((sum, u) => sum + u.value, 0);
  const nInputs = utxos.length;
  const nOutputs = 2;  // mint + change
  const estimatedSize = estimateTxSize(nInputs, nOutputs);
  const estimatedFee = Math.ceil(estimatedSize * feeRate / 1000);
  const fee = Math.max(estimatedFee, uap.RECOMMENDED_MIN_TX_FEE);
  const change = totalUtxoValue - amountSats - fee;

  // Build the mint script (we'll need pubKey from buildMintTx, but store for reference)
  const plan = {
    ticker,
    name,
    multiplier,
    amountSats,
    fee,
    change,
    salt,
    feeRate,
    // script will be built in buildMintTx when we have the pubKey
  };

  return { ok: true, plan };
}

/**
 * Build and sign a mint transaction from a plan.
 *
 * @param {Object} options
 * @param {Object} options.secp - secp256k1 instance with getPublicKey and signSync
 * @param {Object} options.plan - plan object from planMint
 * @param {Uint8Array} options.privKey - private key for signing
 * @param {Uint8Array} options.pubKey - public key (compressed)
 * @param {Array} options.utxos - UTXOs to spend from
 * @param {string} options.changeAddress - address for change output
 * @returns {Promise<Object>} { rawHex, txid?, mintScript, salt, value }
 */
export async function buildMintTx({
  secp,
  plan,
  privKey,
  pubKey,
  utxos,
  changeAddress
}) {
  const mintScript = uap.buildMintScript(pubKey, plan.multiplier, plan.salt);

  // Input selection, change and signing all belong to uap.js. That code is
  // exercised against a real node by contrib/uap-js/integration.test.js;
  // reimplementing any of it here would mean a second, unverified copy of
  // the signing path.
  const inputs = utxos.map((u) => ({
    txid: u.txid,
    vout: u.vout,
    value: u.value,
    // A UTXO from GET /utxos is P2PKH to this wallet's own hash160, so its
    // scriptCode is derivable; an explicit scriptPubKey wins if supplied.
    scriptCode: u.scriptPubKey
      ? (typeof u.scriptPubKey === 'string' ? uap.hexToBytes(u.scriptPubKey) : u.scriptPubKey)
      : addr.buildP2PKHScript(pubKey),
    privKey,
    pubKey
  }));

  const tx = uap.buildPaymentTx(secp, {
    inputs,
    outputs: [{ value: plan.amountSats, scriptPubKey: mintScript }],
    changeScript: addr.addressToScript(changeAddress),
    feeRate: plan.feeRate
  });

  return {
    rawHex: uap.txToHex(tx),
    tx,
    mintScript,
    salt: plan.salt,
    value: plan.amountSats
  };
}

/**
 * Estimate transaction size in bytes for fee calculation.
 * P2PKH inputs are ~150 bytes, outputs are ~34 bytes.
 * @private
 */
function estimateTxSize(nInputs, nOutputs) {
  // Base: version(4) + input count varint + output count varint + locktime(4)
  let size = 10;
  // Each input: ~150 bytes (txid 32 + vout 4 + sequence 4 + scriptSig ~107)
  size += nInputs * 150;
  // Each output: ~34 bytes (value 8 + scriptPubKey varint 1 + script ~25)
  size += nOutputs * 34;
  return size;
}

/**
 * Check if a string contains control characters (ASCII 0-31 except valid ones).
 * @private
 */
function hasControlCharacters(str) {
  for (let i = 0; i < str.length; i++) {
    const code = str.charCodeAt(i);
    // Allow only printable ASCII and valid UTF-8 multibyte (code > 127)
    // Reject control characters (0-31) and DEL (127)
    if (code < 32 || code === 127) {
      return true;
    }
  }
  return false;
}
