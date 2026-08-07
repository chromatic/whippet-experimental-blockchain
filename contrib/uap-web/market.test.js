// Market order book tests - order fill flow
// Strict red/green TDD: write tests, see them fail, implement, see them pass

import { describeOrder, planFill, buildFillTx } from './market.js';
import * as uap from '../uap-js/uap.js';
import * as secp256k1 from '../uap-js/secp.js';
import * as addr from '../uap-js/addr.js';

import { test, assert, assertEqual, assertDeepEqual, run } from './test-harness.js';

// =============================================================================
// FIXTURES & SETUP
// =============================================================================

uap.configureSecp(secp256k1);
const TEST_PRIVKEY = new Uint8Array(32).fill(0x11);
const TEST_PUBKEY = secp256k1.getPublicKey(TEST_PRIVKEY, true);
const TEST_ADDRESS = addr.pubkeyToAddress(TEST_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
const TEST_SCRIPTPUBKEY = addr.buildP2PKHScript(TEST_PUBKEY);

const TAKER_PRIVKEY = new Uint8Array(32).fill(0x22);
const TAKER_PUBKEY = secp256k1.getPublicKey(TAKER_PRIVKEY, true);
const TAKER_ADDRESS = addr.pubkeyToAddress(TAKER_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
const TAKER_SCRIPTPUBKEY = addr.buildP2PKHScript(TAKER_PUBKEY);

const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;

// A real order: maker sells a position for payment
const TEST_ORDER = {
  txid: '00'.repeat(32),
  vout: 0,
  multiplier: 100,
  pubkey: Array.from(TEST_PUBKEY).map(b => b.toString(16).padStart(2, '0')).join(''),
  script_sig: '47304402203e4516feda7f735057ce8cac3b1a50de0da3c8a7e5f77a8a7c6b5a4d9e8f7a6b02204a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f8301',
  payment_script: 'a914' + 'aa'.repeat(20) + '87',
  payment_value: 5000000,
  created_at: 1609459200
};

// A position with a matching UAP (the token for sale)
const TEST_POSITION = {
  txid: TEST_ORDER.txid,
  vout: TEST_ORDER.vout,
  multiplier: TEST_ORDER.multiplier,
  pubkey: TEST_ORDER.pubkey,
  value: 1000 * COIN,
  height: 100,
  scriptPubKey: Array.from(uap.buildTransferScript(TEST_PUBKEY, 100)).map(b => b.toString(16).padStart(2, '0')).join('')
};

// Taker's UTXOs for payment. This funds payment + fee exactly
// (6000000 - 5000000 payment - 1000000 fee = 0), so it produces NO change
// output. That makes it useless for testing change -- see
// TAKER_UTXOS_WITH_CHANGE.
const TAKER_UTXOS = [
  {
    txid: '11'.repeat(32),
    vout: 0,
    value: 6000000,
    height: 100
  }
];

// A taker funded above payment + fee, so a change output is genuinely
// required: 9000000 - 5000000 - 1000000 = 3000000, comfortably above the
// 1000000 dust limit.
const TAKER_UTXOS_WITH_CHANGE = [
  {
    txid: '22'.repeat(32),
    vout: 0,
    value: 9000000,
    height: 100
  }
];

// =============================================================================
// DESCRIBE ORDER TESTS
// =============================================================================

test('describeOrder formats a basic order correctly', () => {
  const desc = describeOrder(TEST_ORDER);
  assert(typeof desc === 'string', 'should return a string');
  assert(desc.includes('100'), 'should include multiplier');
  assert(desc.includes('5000000'), 'should include payment value');
});

test('describeOrder mentions selling or buying', () => {
  const desc = describeOrder(TEST_ORDER);
  assert(desc.toLowerCase().includes('sell') || desc.toLowerCase().includes('ask'),
    'should indicate this is a sell order');
});

test('describeOrder distinguishes token quantity from payment amount', () => {
  // The test order has multiplier=100 and payment_value=5000000
  // The description should make it clear which is which
  const desc = describeOrder(TEST_ORDER);
  assert(desc.includes('100'), 'should mention the multiplier');
  assert(desc.includes('5000000'), 'should mention the payment value');
  // Note: describeOrder receives only the order, which doesn't include position value.
  // The position value (token amount) is fetched separately by the UI.
});

// =============================================================================
// PLAN FILL TESTS
// =============================================================================

test('planFill accepts valid conditions', () => {
  const result = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(result.ok, 'should accept valid fill');
  assert(result.plan, 'should return a plan');
});

test('planFill rejects if taker lacks funds for payment', () => {
  const result = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: [{ txid: '11'.repeat(32), vout: 0, value: 100000, height: 100 }],
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(!result.ok, 'should reject insufficient funds');
  assert(result.errors && result.errors.length > 0, 'should report errors');
  assert(result.errors.some(e => e.includes('insufficient') || e.includes('Insufficient')),
    'error should mention insufficient funds');
});

test('planFill rejects if payment output would be dust', () => {
  const dustOrder = {
    ...TEST_ORDER,
    payment_value: 100  // Too small
  };

  const result = planFill({
    order: dustOrder,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(!result.ok, 'should reject dust payment');
  assert(result.errors && result.errors.length > 0, 'should report errors');
});

test('planFill reports all failures at once', () => {
  const badOrder = {
    ...TEST_ORDER,
    payment_value: 50  // Dust
  };

  const result = planFill({
    order: badOrder,
    position: TEST_POSITION,
    takerUtxos: [{ txid: '11'.repeat(32), vout: 0, value: 1000, height: 100 }],  // Insufficient
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(!result.ok, 'should reject');
  assert(result.errors.length >= 1, 'should report failures');
});

test('planFill includes change calculation in plan', () => {
  const result = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(result.ok, 'plan should be ok');
  assert(typeof result.plan.fee === 'number', 'should have fee');
  assert(typeof result.plan.change === 'number', 'should have change');
  assert(result.plan.change >= 0, 'change should be non-negative');
});

// =============================================================================
// BUILD FILL TX TESTS
// =============================================================================

test('buildFillTx produces a signed transaction', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  assert(planResult.ok, 'plan should be ok');

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  assert(buildResult.rawHex, 'should produce rawHex');
  assert(typeof buildResult.rawHex === 'string', 'rawHex should be a string');
  assert(buildResult.rawHex.length % 2 === 0, 'rawHex should be valid hex');
});

test('buildFillTx produces parseable hex', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  // Should be valid hex
  assert(/^[0-9a-f]*$/.test(buildResult.rawHex), 'hex should only contain 0-9a-f');
});

test('buildFillTx includes correct outputs', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  assert(buildResult.tx, 'should have tx object');
  assert(buildResult.tx.vout, 'should have outputs');
  // First output should be maker's payment
  assert(buildResult.tx.vout.length >= 1, 'should have at least payment output');
  assertEqual(buildResult.tx.vout[0].value, TEST_ORDER.payment_value,
    'first output should be maker payment');
});

test('buildFillTx includes token transfer output', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  assert(buildResult.tx.vout.length >= 2, 'should have token output and payment output');
  // Second output should be token transfer to taker
  assertEqual(buildResult.tx.vout[1].value, TEST_POSITION.value,
    'second output should be position value');
});

test('buildFillTx includes change when the taker overfunds', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS_WITH_CHANGE,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });
  assert(planResult.ok, `planFill should succeed: ${JSON.stringify(planResult.errors)}`);

  // Guard the premise. The previous version of this test wrapped its only
  // assertion in `if (change >= DUST)`, and the fixture it used produced
  // change of exactly 0 -- so the body never ran and the test passed even
  // when buildFillTx dropped the change output entirely. Asserting the
  // precondition means a fixture that stops producing change fails here
  // instead of silently going vacuous again.
  assert(planResult.plan.change >= DEFAULT_DUST_LIMIT,
    `fixture must produce above-dust change, got ${planResult.plan.change}`);

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS_WITH_CHANGE
  });

  // Unconditional, and on the value rather than just the output count: a
  // change output carrying the wrong amount silently burns the difference
  // to fee, which counting outputs would never notice.
  assertEqual(buildResult.tx.vout.length, 3, 'payment + covenant + change');
  const change = buildResult.tx.vout[2];
  assertEqual(change.value, planResult.plan.change, 'change output value');
  assertDeepEqual(Array.from(change.scriptPubKey),
    Array.from(addr.buildP2PKHScript(TAKER_PUBKEY)),
    'change must pay back to the taker');
});

test('buildFillTx omits change when there is none', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });
  assert(planResult.ok, 'planFill should succeed');
  assertEqual(planResult.plan.change, 0, 'this fixture is the exact-funding case');

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  assertEqual(buildResult.tx.vout.length, 2, 'payment + covenant only');
});

test('buildFillTx maker input is first (SIGHASH_SINGLE requirement)', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  assert(buildResult.tx.vin, 'should have inputs');
  assert(buildResult.tx.vin.length >= 1, 'should have at least maker input');
  // First input should be the maker's position (from the order)
  assertEqual(buildResult.tx.vin[0].txid, TEST_ORDER.txid,
    'first input should be maker position');
  assertEqual(buildResult.tx.vin[0].vout, TEST_ORDER.vout,
    'first input vout should match order');
});

test('buildFillTx properly signs taker inputs', async () => {
  const planResult = planFill({
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS,
    takerPubkey: TAKER_PUBKEY,
    feeRate: 1000
  });

  const buildResult = await buildFillTx({
    secp: secp256k1,
    plan: planResult.plan,
    privKey: TAKER_PRIVKEY,
    pubKey: TAKER_PUBKEY,
    order: TEST_ORDER,
    position: TEST_POSITION,
    takerUtxos: TAKER_UTXOS
  });

  // Taker's inputs should have scriptSig (not empty)
  assert(buildResult.tx.vin.length > 1, 'should have taker input');
  const takerInput = buildResult.tx.vin[1];
  assert(takerInput.scriptSig, 'taker input should be signed');
  assert(takerInput.scriptSig.length > 0, 'scriptSig should not be empty');
});

// =============================================================================
// RUN ALL TESTS
// =============================================================================

run({ banner: 'Running market tests...\n', style: 'compact' }).catch(e => {
  console.error('Test harness error:', e);
  process.exit(1);
});
