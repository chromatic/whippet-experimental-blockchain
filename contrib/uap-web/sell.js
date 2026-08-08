// Building a sell order: the maker half of the marketplace.
//
// The counterpart to market.js, which only ever handled the taker half.
// A maker signs SIGHASH_SINGLE|ANYONECANPAY over their position and the
// single output that pays them, and publishes that signature. Anyone can
// then complete the trade without the maker being online.
//
// What the maker is actually agreeing to is narrower than it looks, and
// the review screen has to say so: the signature commits to the position
// being spent and to the payment output, and to nothing else. Where the
// token ends up is not covered -- consensus guarantees a valid covenant
// continues to exist, but not who holds it. The maker is saying "anyone
// may take this position if this exact payment is made", which is the
// point, but it is not the same as choosing a counterparty.
import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';
import { positionScriptOf } from './transfer.js';

/**
 * Check a proposed sale and describe it, without signing anything.
 *
 * @param {Object} o
 * @param {Object} o.position  { txid, vout, multiplier, value } from the indexer
 * @param {number} o.priceSats asking price in satoshis
 * @param {string} o.ownAddress address the maker wants paid
 * @returns {{ok: boolean, errors: string[], summary: Object}}
 */
export function planSell({ position, priceSats, ownAddress }) {
  const errors = [];

  if (!position || typeof position !== 'object') {
    errors.push('No position selected.');
    return { ok: false, errors, summary: null };
  }
  if (typeof position.txid !== 'string' || position.txid.length !== 64) {
    errors.push('Position is missing a valid transaction id.');
  }
  if (!Number.isInteger(position.vout) || position.vout < 0) {
    errors.push('Position is missing a valid output index.');
  }
  if (!Number.isInteger(position.multiplier) || position.multiplier < 0) {
    errors.push('Position has no valid multiplier.');
  }

  if (!Number.isInteger(priceSats)) {
    errors.push('Enter a price in whole satoshis.');
  } else if (priceSats <= 0) {
    errors.push('The asking price must be greater than zero.');
  } else if (priceSats < uap.DEFAULT_HARD_DUST_LIMIT) {
    // A payment output below the *hard* dust limit makes the completed
    // swap non-standard, so no node would relay the fill (see
    // IsStandardTx in src/policy/policy.cpp, which checks nHardDustLimit --
    // the soft nDustLimit only affects fee bumping, not relay). The order
    // would sit in the book looking valid and could never be taken.
    errors.push(
      `The asking price is below the dust limit (${uap.DEFAULT_HARD_DUST_LIMIT} satoshis); ` +
      'no node would relay a transaction paying it.'
    );
  }

  let paymentScript = null;
  try {
    paymentScript = addr.addressToScript(ownAddress);
  } catch (e) {
    errors.push(`Cannot pay to this address: ${e.message}`);
  }

  return {
    ok: errors.length === 0,
    errors,
    // The position itself, carried through rather than flattened into
    // `summary`. buildSellOrder has to sign over the covenant's real script,
    // and summary's txid/vout/multiplier are not enough to rebuild it: a
    // freshly minted position also needs script_hex, because its salt is not
    // derivable from anything else here.
    position,
    summary: {
      txid: position && position.txid,
      vout: position && position.vout,
      multiplier: position && position.multiplier,
      tokenValue: position && position.value,
      priceSats,
      payTo: ownAddress,
      paymentScript,
    },
  };
}

/**
 * Sign a sell order and return the payload the indexer expects.
 *
 * Signing is the irreversible step: once this signature is published,
 * anyone who can see it may take the position at this price until it is
 * cancelled or the position is spent. Callers must not reach here before
 * the user has confirmed a review screen.
 *
 * @param {Object} o
 * @param {Object} o.secp     secp256k1 instance (see uap.signSpend)
 * @param {Object} o.position { txid, vout, multiplier }
 * @param {Uint8Array} o.privKey maker's private key
 * @param {Uint8Array} o.pubKey  maker's compressed public key
 * @param {number} o.priceSats
 * @param {string} o.ownAddress
 * @returns {Object} body for POST /orders
 */
export function buildSellOrder({ secp, position, privKey, pubKey, priceSats, ownAddress }) {
  const plan = planSell({ position, priceSats, ownAddress });
  if (!plan.ok) {
    throw new Error(plan.errors.join(' '));
  }

  // The scriptCode signed against is the position's own scriptPubKey.
  //
  // This used to be built unconditionally as a TRANSFER covenant, which is
  // wrong for a freshly minted position: a mint is `<pubkey> <mult> <salt>
  // OP_MINT`, different bytes entirely, and the salt is not derivable. The
  // signature therefore committed to a script the output did not have, and
  // the node rejected every attempt to fill the order with NULLFAIL
  // ("Signature must be zero for failed CHECK(MULTI)SIG operation"). The
  // order published and listed perfectly; only the buyer ever saw it fail.
  //
  // positionScriptOf is transfer.js's, deliberately shared rather than
  // reimplemented: it re-verifies that the script really is this maker's
  // covenant with this multiplier and this mint/transfer form before signing
  // over it, which is what stops a mutated position object from getting the
  // maker's key to sign a preimage over an attacker's blob.
  const scriptCode = positionScriptOf({
    position,
    ownPubKey: pubKey,
    multiplier: position.multiplier
  });

  const order = uap.signMakerOrder(secp, {
    input: { txid: position.txid, vout: position.vout, scriptCode },
    privKey,
    paymentScript: plan.summary.paymentScript,
    paymentValue: priceSats,
  });

  return {
    txid: position.txid,
    vout: position.vout,
    multiplier: position.multiplier,
    pubkey: uap.bytesToHex(pubKey),
    script_sig: uap.bytesToHex(order.scriptSig),
    payment_script: uap.bytesToHex(plan.summary.paymentScript),
    payment_value: priceSats,
    created_at: Math.floor(Date.now() / 1000),
  };
}
