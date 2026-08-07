// Cancelling a standing sell order: the maker's side of
// DELETE /orders/{txid}/{vout}?sig=<hex> (see CancelOrder in
// contrib/uap-indexer/orders.go and api.js's cancelOrder).
//
// Cancelling now creates a new signed statement: a real ECDSA signature by
// the position's own key, over a message that binds the exact order
// (outpoint + script_sig) and a relay-assigned nonce that changes on every
// publish (see contrib/uap-indexer/cancel_auth.go for the full scheme, and
// contrib/uap-js's signCancelOrder for the function that produces it).
//
// This replaces an earlier, broken contract: the relay used to accept a
// cancellation whose only "proof" was reproducing the order's own
// script_sig -- a value that comes back in the body of every `GET /orders`
// response, to anyone. Anyone who had looked at the order book could
// reproduce it and cancel someone else's order, repeatedly, with the
// maker getting no signal. Signing a cancellation properly closes that:
// only whoever holds the position's private key can produce a valid one.
//
// This module stays free of the actual signing call (no uap-js import
// here) so it can be unit-tested without an EC library in the loop; it
// only shapes the inputs. app.js -- which already holds the wallet's
// private key and already imports uap-js/secp.js -- is what calls
// uap.signCancelOrder(secp256k1, { ...cancelSignInput(order), privKey })
// and passes the result to API#cancelOrder.
//
// Filtering to `myOrders` below is a UI convenience so a wallet only shows
// (and offers a cancel button for) orders it believes are its own; with a
// real cancel signature required, it is no longer load-bearing for
// security the way it once had to be -- the relay itself now refuses a
// cancellation from anyone but the position's own key.

/**
 * Filter a list of open orders down to the ones this wallet published.
 *
 * `order.pubkey` is set server-side from the position's own pubkey (see
 * PublishOrder in orders.go), not taken from the client, so matching on it
 * is a reliable "is this mine" signal -- and, unlike before, only orders
 * this wallet's key can actually produce a valid cancel signature for
 * anyway.
 *
 * @param {Array} orders - result of API#listOrders
 * @param {string} ownPubKeyHex - the wallet's own compressed pubkey, hex
 * @returns {Array} orders whose pubkey matches ownPubKeyHex
 */
export function myOrders(orders, ownPubKeyHex) {
  if (!Array.isArray(orders)) return [];
  if (typeof ownPubKeyHex !== 'string' || ownPubKeyHex.length === 0) return [];
  return orders.filter((o) => o && typeof o.pubkey === 'string' && o.pubkey === ownPubKeyHex);
}

/**
 * Check that an order carries what a cancel signature needs to be
 * computed over, before making the request. Mirrors planSell's "refuse
 * before acting" shape in sell.js.
 *
 * @param {Object} order - an order object, as returned by listOrders/getOrder
 * @returns {{ok: boolean, errors: string[]}}
 */
export function planCancel(order) {
  const errors = [];

  if (!order || typeof order !== 'object') {
    errors.push('No order selected.');
    return { ok: false, errors };
  }
  if (typeof order.txid !== 'string' || order.txid.length !== 64) {
    errors.push('Order is missing a valid transaction id.');
  }
  if (!Number.isInteger(order.vout) || order.vout < 0) {
    errors.push('Order is missing a valid output index.');
  }
  if (typeof order.script_sig !== 'string' || order.script_sig.length === 0) {
    errors.push('Order is missing its signature; cannot request cancellation.');
  }
  // cancel_nonce must be a string, not a number: it is a nanosecond-scale
  // value that a JSON number cannot carry past 2^53 without losing
  // precision (see orders.go's `cancel_nonce,string` tag and
  // cancelMessage's doc comment in uap-js/uap.js). API#listOrders and
  // API#getOrder already enforce this on the response, so an order that
  // reached here with the wrong type indicates a caller bypassing that --
  // still worth refusing rather than silently signing something the relay
  // can never match.
  if (typeof order.cancel_nonce !== 'string' || order.cancel_nonce.length === 0) {
    errors.push('Order is missing the data needed to authorize a cancellation.');
  }

  return { ok: errors.length === 0, errors };
}

/**
 * Pluck the fields a cancel signature must be computed over out of an
 * order, in exactly the shape contrib/uap-js's signCancelOrder expects as
 * its opts (minus privKey, which only the caller holding the wallet's key
 * should ever touch):
 *
 *   uap.signCancelOrder(secp256k1, { ...cancelSignInput(order), privKey })
 *
 * Call planCancel(order) first and check `ok` -- this does not re-validate
 * shape, only extracts it, matching the trust-cancel.js-with-parsing /
 * app.js-with-secrets split described in the module comment above.
 *
 * @param {Object} order - an order object, as returned by listOrders/getOrder
 * @returns {{txid: string, vout: number, scriptSig: string, cancelNonce: string}}
 */
export function cancelSignInput(order) {
  return {
    txid: order.txid,
    vout: order.vout,
    scriptSig: order.script_sig,
    cancelNonce: order.cancel_nonce,
  };
}

/**
 * Turn the relay's cancel-failure text into a specific, user-facing reason
 * instead of a generic "could not cancel". The strings matched here are the
 * exact ones CancelOrder in contrib/uap-indexer/orders.go returns; if that
 * code changes its wording, this stops matching and falls through to the
 * generic case rather than mis-describing the failure -- update both
 * together.
 *
 * @param {string} message - the Error#message from a failed API#cancelOrder call
 * @returns {string} a message safe to show the user directly
 */
export function describeCancelFailure(message) {
  const m = String(message || '');

  if (m.includes('no such order')) {
    // Reached whether the order was never there, was already cancelled
    // (elsewhere, or by this wallet in a previous session), was filled, or
    // its position got spent some other way -- ApplyBlock deletes the order
    // row the moment the position it sells is spent (index.go), so by the
    // time this response comes back the relay genuinely has nothing left to
    // withdraw. There is no way to tell those apart from this error alone.
    return 'This order is no longer open. It may already have been filled, ' +
      'cancelled elsewhere, or the underlying position has been spent.';
  }
  if (m.includes('cancel signature does not authorize this order')) {
    // Covers a wrong key, a signature captured for a different order, and
    // a replayed/stale signature (the order was cancelled and republished
    // since the signature was made, so its cancel_nonce moved on) --
    // CancelOrder does not distinguish these itself, and neither does
    // this message.
    return 'This cancellation could not be verified against the order on file. ' +
      'If the order was recently republished, try again with a fresh signature.';
  }
  return `Could not cancel this order: ${m}`;
}
