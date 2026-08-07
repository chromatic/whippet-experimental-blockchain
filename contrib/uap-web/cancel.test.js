import { myOrders, planCancel, describeCancelFailure } from './cancel.js';
import { test, assert, assertEqual, run } from './test-harness.js';

const OWN_PUBKEY = 'ab'.repeat(33);
const OTHER_PUBKEY = 'cd'.repeat(33);

const ORDER_A = {
  txid: 'a'.repeat(64),
  vout: 0,
  multiplier: 1000,
  pubkey: OWN_PUBKEY,
  script_sig: 'deadbeef83',
  payment_script: '76a914' + '11'.repeat(20) + '88ac',
  payment_value: 250000000,
  created_at: 1700000000,
};

const ORDER_B_OTHER = {
  ...ORDER_A,
  txid: 'b'.repeat(64),
  pubkey: OTHER_PUBKEY,
};

// =============================================================================
// myOrders: which open orders belong to this wallet
// =============================================================================

test('myOrders keeps only orders whose pubkey matches the wallet', () => {
  const result = myOrders([ORDER_A, ORDER_B_OTHER], OWN_PUBKEY);
  assertEqual(result.length, 1, 'only the matching order survives');
  assertEqual(result[0].txid, ORDER_A.txid, 'the surviving order is the maker\'s own');
});

test('myOrders returns an empty list when nothing matches', () => {
  const result = myOrders([ORDER_B_OTHER], OWN_PUBKEY);
  assertEqual(result.length, 0, 'no orders belong to this wallet');
});

test('myOrders tolerates a non-array or missing pubkey rather than throwing', () => {
  assertEqual(myOrders(null, OWN_PUBKEY).length, 0, 'null orders -> empty');
  assertEqual(myOrders(undefined, OWN_PUBKEY).length, 0, 'undefined orders -> empty');
  assertEqual(myOrders([ORDER_A], '').length, 0, 'empty own pubkey matches nothing');
  assertEqual(myOrders([ORDER_A], null).length, 0, 'null own pubkey matches nothing');
});

// =============================================================================
// planCancel: refuse before requesting, not after
// =============================================================================

test('a well-formed order plans cleanly for cancellation', () => {
  const p = planCancel(ORDER_A);
  assertEqual(p.ok, true, p.errors.join(' '));
  assertEqual(p.errors.length, 0, 'no errors on a good order');
});

test('a missing order is refused', () => {
  const p = planCancel(null);
  assertEqual(p.ok, false, 'null order must be refused');
});

test('an order missing its txid is refused', () => {
  const p = planCancel({ ...ORDER_A, txid: 'short' });
  assertEqual(p.ok, false, 'bad txid must be refused');
});

test('an order missing its vout is refused', () => {
  const p = planCancel({ ...ORDER_A, vout: -1 });
  assertEqual(p.ok, false, 'negative vout must be refused');
});

test('an order missing its script_sig is refused', () => {
  const p = planCancel({ ...ORDER_A, script_sig: '' });
  assertEqual(p.ok, false, 'empty script_sig must be refused');
  assert(p.errors.some((e) => e.includes('signature')), 'error should mention the missing signature');
});

// =============================================================================
// describeCancelFailure: turn the relay's error text into something
// specific rather than a generic failure (see CancelOrder in
// contrib/uap-indexer/orders.go for the exact strings matched against).
// =============================================================================

test('a "no such order" failure is described as no-longer-open, not a generic error', () => {
  const msg = describeCancelFailure('HTTP 400: no such order');
  assert(/no longer open/i.test(msg), `expected a "no longer open" style message, got: ${msg}`);
});

test('a script_sig mismatch is described distinctly from "no such order"', () => {
  const msg = describeCancelFailure('HTTP 400: script_sig does not match the published order');
  assert(!/no longer open/i.test(msg), 'a mismatch must not be reported as "no longer open"');
  assert(msg.length > 0, 'a message is produced');
});

test('an unrecognized failure still produces a readable message rather than throwing', () => {
  const msg = describeCancelFailure('HTTP 500: index unavailable');
  assert(msg.includes('index unavailable'), `expected the underlying reason to be preserved, got: ${msg}`);
});

run();
