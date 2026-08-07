// Helpers shared by the integration test suites in this directory and in
// ../uap-web. These were previously copy-pasted, identically, into
// uap-js/integration.test.js, uap-web/market.integration.test.js and
// uap-web/mint.integration.test.js.
//
// This file is test-only, but it deliberately contains NO node-only imports
// (no 'node:child_process', no Buffer, no process): it pulls in nothing but
// uap.js and addr.js, both of which already run unchanged in a browser. Keep
// it that way -- the node-only machinery lives in test-node-harness.js.

import { COIN } from './uap.js';
import { base58checkDecode } from './addr.js';

/**
 * Node JSON-RPC returns amounts as JSON numbers in whole coins
 * (e.g. 500000.00000000). Round to avoid float noise.
 */
export function satoshisFromCoins(amount) {
  return Math.round(amount * COIN);
}

/** WIF (dumpprivkey output) -> raw 32-byte private key. */
export function privKeyFromWIF(wif) {
  const { payload } = base58checkDecode(wif);
  // Compressed-key WIF carries a trailing 0x01 after the 32 key bytes.
  if (payload.length === 33 && payload[32] === 0x01) return payload.slice(0, 32);
  if (payload.length === 32) return payload;
  throw new Error(`unexpected WIF payload length ${payload.length}`);
}

export function assert(cond, msg) {
  if (!cond) throw new Error(msg || 'assertion failed');
}
