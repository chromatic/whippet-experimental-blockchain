// Cancelling a standing sell order: the maker's side of
// DELETE /orders/{txid}/{vout}?script_sig=<hex> (see CancelOrder in
// contrib/uap-indexer/orders.go and api.js's cancelOrder).
//
// There is nothing to sign here. Cancelling doesn't create a new signed
// statement -- it withdraws one that was already published. The relay's
// only check is that the caller reproduces the exact scriptSig on file for
// that outpoint, and that scriptSig is sitting right there in the order
// object `GET /orders` already handed back, so this module never needs the
// wallet's private key.
//
// IMPORTANT: that also means the relay's check is not real authentication.
// Every open order's script_sig is public -- it comes back in the body of
// `GET /orders`, to anyone -- so anyone who has looked at the order book can
// reproduce it and cancel someone else's order. The maker's real recourse
// against an unwanted fill is unilateral: spend the position elsewhere.
// Filtering to `myOrders` below is a UI convenience so a wallet only shows
// (and offers a cancel button for) orders it believes are its own; it adds
// no protection the server doesn't already have, and the server has very
// little. See the marketplace security report for the full writeup.

/**
 * Filter a list of open orders down to the ones this wallet published.
 *
 * `order.pubkey` is set server-side from the position's own pubkey (see
 * PublishOrder in orders.go), not taken from the client, so matching on it
 * is a reasonable "is this mine" signal for display purposes -- though see
 * the module comment above for why it is not an access-control boundary.
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
 * Check that an order carries what a cancel request needs, before making
 * the request. Mirrors planSell's "refuse before acting" shape in sell.js.
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

  return { ok: errors.length === 0, errors };
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
  if (m.includes('does not match the published order')) {
    return 'The relay could not verify this cancellation against the order it has on file.';
  }
  return `Could not cancel this order: ${m}`;
}
