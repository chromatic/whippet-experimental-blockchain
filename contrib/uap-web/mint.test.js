// Mint tests - in-browser token creation flow
// Strict red/green TDD: write tests, see them fail, implement, see them pass

import { planMint, buildMintTx } from './mint.js';
import * as uap from '../uap-js/uap.js';
import * as secp256k1 from '../uap-js/secp.js';
import * as addr from '../uap-js/addr.js';

import { test, assert, assertEqual, run } from './test-harness.js';

// =============================================================================
// FIXTURES & SETUP
// =============================================================================

// A real key, because buildMintTx really signs. The signature bytes are not
// what these tests assert on, but a fake secp would let a build that cannot
// produce a spendable transaction pass -- which is exactly how the earlier
// stubbed buildMintTx went unnoticed.
uap.configureSecp(secp256k1);
const TEST_PRIVKEY = new Uint8Array(32).fill(0x11);
const TEST_PUBKEY = secp256k1.getPublicKey(TEST_PRIVKEY, true);
const TEST_ADDRESS = addr.pubkeyToAddress(TEST_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
const TEST_SCRIPTPUBKEY = addr.buildP2PKHScript(TEST_PUBKEY);

// Standard test fixtures
const COIN = uap.COIN;
const DEFAULT_DUST_LIMIT = uap.DEFAULT_DUST_LIMIT;
const VALID_PLAN_INPUT = {
  ticker: 'TEST',
  multiplier: 100,
  amountSats: 1000 * COIN,  // exactly minimum
  utxos: [
    { txid: '00'.repeat(32), vout: 0, value: 1010 * COIN, scriptPubKey: TEST_SCRIPTPUBKEY }
  ],
  feeRate: 1000,  // sat/kB
  address: TEST_ADDRESS
};

// =============================================================================
// VALID PLAN TESTS
// =============================================================================

test('a valid plan is accepted and returns a plan object', () => {
  const result = planMint(VALID_PLAN_INPUT);
  assert(result.ok, 'plan should be ok');
  assert(result.plan, 'plan should exist');
});

test('valid plan reports exact fee and change', () => {
  const result = planMint(VALID_PLAN_INPUT);
  assert(result.ok, 'plan should be ok');
  assert(typeof result.plan.fee === 'number', 'fee should be a number');
  assert(typeof result.plan.change === 'number', 'change should be a number');
  assert(result.plan.change >= 0, 'change should be non-negative');
  // Total should be: input - amount - fee = 1010*COIN - 1000*COIN - fee = 10*COIN - fee
  // So change + fee should equal 10*COIN (the extra we brought)
  const extra = 1010 * COIN - 1000 * COIN;
  assertEqual(result.plan.fee + result.plan.change, extra, 'fee + change should equal input surplus');
});

// The v1 plan carried a random 20-byte salt for the mint script. v2 dropped
// it -- lineage identity is derived from the outpoint at first spend -- so a
// plan that still produced one would mean mint.js was building a covenant
// nothing else in the stack can read.
test('a plan carries no salt: the v2 mint covenant has no room for one', () => {
  const result = planMint(VALID_PLAN_INPUT);
  assert(result.ok, 'plan should be ok');
  assertEqual(result.plan.salt, undefined, 'the v2 plan must not carry a salt');
});

// =============================================================================
// FIELD VALIDATION TESTS
// =============================================================================

test('ticker is required', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: '' };
  const result = planMint(input);
  assert(!result.ok, 'should reject empty ticker');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('ticker max 8 characters', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: 'A'.repeat(9) };
  const result = planMint(input);
  assert(!result.ok, 'should reject a 9-character ticker');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('ticker rejects non-ASCII characters', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: '🎉' };
  const result = planMint(input);
  assert(!result.ok, 'should reject a non-ASCII ticker');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('ticker min 1 byte', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: '' };
  const result = planMint(input);
  assert(!result.ok, 'should reject empty ticker');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('ticker rejects control characters', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: 'TEST\n' };
  const result = planMint(input);
  assert(!result.ok, 'should reject ticker with newline');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('ticker rejects leading/trailing whitespace', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: ' TEST' };
  const result = planMint(input);
  assert(!result.ok, 'should reject ticker with leading space');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

// Note: HTML-like strings are not rejected by validation; the UI is responsible
// for safe rendering via dom.js. Validation only checks consensus rules.

test('ticker rejects NUL characters', () => {
  const input = { ...VALID_PLAN_INPUT, ticker: 'TEST\x00BAD' };
  const result = planMint(input);
  assert(!result.ok, 'should reject ticker with NUL');
  assert(result.errors.some(e => e.field === 'ticker'), 'should have ticker error');
});

test('multiplier must be an integer', () => {
  const input = { ...VALID_PLAN_INPUT, multiplier: 100.5 };
  const result = planMint(input);
  assert(!result.ok, 'should reject non-integer multiplier');
  assert(result.errors.some(e => e.field === 'multiplier'), 'should have multiplier error');
});

test('multiplier min is 0', () => {
  const input = { ...VALID_PLAN_INPUT, multiplier: -1 };
  const result = planMint(input);
  assert(!result.ok, 'should reject negative multiplier');
  assert(result.errors.some(e => e.field === 'multiplier'), 'should have multiplier error');
});

test('multiplier max is 2147483647', () => {
  const input = { ...VALID_PLAN_INPUT, multiplier: 2147483648 };
  const result = planMint(input);
  assert(!result.ok, 'should reject multiplier > 2147483647');
  assert(result.errors.some(e => e.field === 'multiplier'), 'should have multiplier error');
});

test('amount must be at least 1000 * COIN', () => {
  const input = { ...VALID_PLAN_INPUT, amountSats: 1000 * COIN - 1 };
  const result = planMint(input);
  assert(!result.ok, 'should reject insufficient amount');
  assert(result.errors.some(e => e.field === 'amountSats'), 'should have amountSats error');
});

test('amount exactly 1000 * COIN is accepted', () => {
  const input = { ...VALID_PLAN_INPUT, amountSats: 1000 * COIN };
  const result = planMint(input);
  assert(result.ok, 'should accept exactly 1000 * COIN');
});

test('2^48 virtual balance overflow is detected', () => {
  // 2^48 = 281474976710656 satoshis = 2814749.76710656 coins
  // So multiplier * coins must be <= 2^48
  // At 1 COIN multiplied by 10^9 multiplier, we get 10^9 COIN which is ~ 10^9 / 10^8 = 10 coins
  // Let's use a large multiplier with a large amount
  const maxCoins = Math.pow(2, 48);  // 281474976710656
  const maxSatsWithLowMult = Math.floor(maxCoins / 1000) * COIN;  // Max for multiplier 1000

  const input = {
    ...VALID_PLAN_INPUT,
    amountSats: maxSatsWithLowMult,
    multiplier: 1001,
    utxos: [{ txid: '00'.repeat(32), vout: 0, value: maxSatsWithLowMult + 100000 }]
  };

  const result = planMint(input);
  assert(!result.ok, 'should reject when (coins * multiplier) > 2^48');
  assert(result.errors.some(e => e.field === 'multiplier' || e.field === 'amountSats'),
    'should indicate which field causes overflow');
});

test('2^48 guard passes for small multiplier with large amount', () => {
  // Large amount with multiplier 1 should work
  const input = {
    ...VALID_PLAN_INPUT,
    amountSats: 100000 * COIN,
    multiplier: 1,
    utxos: [{ txid: '00'.repeat(32), vout: 0, value: 110000 * COIN }]
  };

  const result = planMint(input);
  assert(result.ok, 'should accept when (coins * multiplier) <= 2^48');
});

test('insufficient funds is rejected with clear message', () => {
  const input = {
    ...VALID_PLAN_INPUT,
    amountSats: 1000 * COIN,
    utxos: [{ txid: '00'.repeat(32), vout: 0, value: 500 * COIN }]  // Not enough
  };

  const result = planMint(input);
  assert(!result.ok, 'should reject insufficient funds');
  assert(result.errors.some(e => e.field === 'utxos'), 'should have utxos error');
  // Check that error message mentions coins, not satoshis
  const utxoError = result.errors.find(e => e.field === 'utxos');
  assert(utxoError.message.includes('Insufficient') || utxoError.message.includes('insufficient'),
    'error should explain insufficient funds');
});

test('dust change is rejected', () => {
  // Calculate exactly what produces dust change
  // If we have 1000*COIN + (DEFAULT_DUST_LIMIT - 1) input, and spend 1000*COIN, we get dust
  const input = {
    ...VALID_PLAN_INPUT,
    amountSats: 1000 * COIN,
    utxos: [{
      txid: '00'.repeat(32),
      vout: 0,
      value: 1000 * COIN + DEFAULT_DUST_LIMIT - 1
    }],
    feeRate: 1000  // High fee to ensure change < dust
  };

  const result = planMint(input);
  assert(!result.ok, 'should reject plan producing dust change');
  assert(result.errors.some(e => e.field === 'utxos' || e.field === 'amountSats'),
    'should indicate the issue relates to inputs or amount');
});

// =============================================================================
// MULTIPLE FAILURES AT ONCE
// =============================================================================

test('multiple validation failures are all reported', () => {
  const input = {
    ticker: '',  // empty (invalid)
    multiplier: -5,  // negative (invalid)
    amountSats: 500 * COIN,  // below minimum (invalid)
    utxos: [],  // empty (invalid)
    feeRate: 1000,
    address: TEST_ADDRESS
  };

  const result = planMint(input);
  assert(!result.ok, 'should reject');
  assert(result.errors.length >= 3, 'should report all failures at once, got: ' +
    result.errors.map(e => e.field).join(', '));
  assert(result.errors.some(e => e.field === 'ticker'), 'should report ticker error');
  assert(result.errors.some(e => e.field === 'multiplier'), 'should report multiplier error');
  assert(result.errors.some(e => e.field === 'amountSats'), 'should report amountSats error');
});

// =============================================================================
// TRANSACTION BUILDING TESTS
// =============================================================================

test('buildMintTx produces a transaction with correct mint output', async () => {
  // First plan a mint
  const planResult = planMint(VALID_PLAN_INPUT);
  assert(planResult.ok, 'plan should be ok');
  const plan = planResult.plan;

  // Create minimal test data for building
  const privKey = TEST_PRIVKEY;
  const pubKey = TEST_PUBKEY;

  const buildResult = await buildMintTx({
    secp: secp256k1,
    plan,
    privKey,
    pubKey,
    utxos: VALID_PLAN_INPUT.utxos,
    changeAddress: VALID_PLAN_INPUT.address
  });

  assert(buildResult.rawHex, 'should produce rawHex');
  assert(buildResult.mintScript, 'should produce mintScript');
  assert(buildResult.metadataScript, 'should produce metadataScript');
});

test('built transaction mint output value is exactly amountSats', async () => {
  const planResult = planMint(VALID_PLAN_INPUT);
  const plan = planResult.plan;

  const privKey = TEST_PRIVKEY;
  const pubKey = TEST_PUBKEY;

  const buildResult = await buildMintTx({
    secp: secp256k1,
    plan,
    privKey,
    pubKey,
    utxos: VALID_PLAN_INPUT.utxos,
    changeAddress: VALID_PLAN_INPUT.address
  });

  assert(buildResult.value === VALID_PLAN_INPUT.amountSats,
    `mint output value should be ${VALID_PLAN_INPUT.amountSats}, got ${buildResult.value}`);
});

test('built transaction uses correct script', async () => {
  const planResult = planMint(VALID_PLAN_INPUT);
  const plan = planResult.plan;

  const privKey = TEST_PRIVKEY;
  const pubKey = TEST_PUBKEY;

  const buildResult = await buildMintTx({
    secp: secp256k1,
    plan,
    privKey,
    pubKey,
    utxos: VALID_PLAN_INPUT.utxos,
    changeAddress: VALID_PLAN_INPUT.address
  });

  // The script should exactly match buildMintScript(pubKey, multiplier)
  const expectedScript = uap.buildMintScript(pubKey, VALID_PLAN_INPUT.multiplier);

  // Compare as hex for better error messages
  const buildHex = Array.from(buildResult.mintScript).map(b => b.toString(16).padStart(2, '0')).join('');
  const expectedHex = Array.from(expectedScript).map(b => b.toString(16).padStart(2, '0')).join('');

  assertEqual(buildHex, expectedHex, 'mint script should match buildMintScript output');
});

// The v1 mint script was salted, so two mints of the same token were
// different bytes. v2 removed the salt, which flips the property: the mint
// covenant is now fully determined by (pubkey, multiplier), and a build that
// still varied run to run would mean something random had crept back into it.
test('two builds of the same plan produce a byte-identical mint script', async () => {
  const toHex = (b) => Array.from(b).map((x) => x.toString(16).padStart(2, '0')).join('');
  const build = async () => {
    const planResult = planMint(VALID_PLAN_INPUT);
    return buildMintTx({
      secp: secp256k1,
      plan: planResult.plan,
      privKey: TEST_PRIVKEY,
      pubKey: TEST_PUBKEY,
      utxos: VALID_PLAN_INPUT.utxos,
      changeAddress: VALID_PLAN_INPUT.address
    });
  };

  const build1 = await build();
  const build2 = await build();

  assertEqual(toHex(build1.mintScript), toHex(build2.mintScript), 'mint script must be deterministic');
  assertEqual(toHex(build1.metadataScript), toHex(build2.metadataScript), 'metadata script must be deterministic');
});

// =============================================================================
// RUN ALL TESTS
// =============================================================================

// =============================================================================
// TOKEN METADATA
//
// The ticker only reaches the chain if buildMintTx emits the OP_RETURN that
// carries it. Leave it out and everything still looks right: the mint
// confirms, the covenant is valid, the position shows up in the wallet -- as
// an anonymous token. The indexer reads metadata from that output and nowhere
// else, so these assertions are the only thing between a named token and a
// nameless one.
// =============================================================================

test('the mint transaction carries the ticker in an OP_RETURN', async () => {
  const plan = planMint(VALID_PLAN_INPUT).plan;
  const built = await buildMintTx({
    secp: secp256k1, plan, privKey: TEST_PRIVKEY, pubKey: TEST_PUBKEY,
    utxos: VALID_PLAN_INPUT.utxos, changeAddress: TEST_ADDRESS
  });

  const opReturns = built.tx.vout.filter((o) => o.scriptPubKey[0] === 0x6a);
  assertEqual(opReturns.length, 1, 'exactly one metadata output');
  assertEqual(opReturns[0].value, 0,
    'an OP_RETURN is unspendable, so paying it anything would burn those coins');

  const hex = uap.bytesToHex(opReturns[0].scriptPubKey);
  assert(hex.includes(uap.bytesToHex(new TextEncoder().encode('WUAP'))),
    'the record is tagged with the WUAP magic the indexer looks for');
  assert(hex.includes(uap.bytesToHex(new TextEncoder().encode('TEST'))),
    `the ticker is in the script: ${hex}`);
});

test('the metadata script stays inside the relay limit', () => {
  const script = uap.buildMetadataScript({ ticker: 'TEST' });
  assert(script.length <= uap.MAX_METADATA_SCRIPT_BYTES,
    `${script.length} bytes exceeds the ${uap.MAX_METADATA_SCRIPT_BYTES}-byte relay limit`);
});

test('the largest ticker (8 chars) is accepted', () => {
  const result = planMint({
    ...VALID_PLAN_INPUT,
    ticker: 'A'.repeat(8)
  });
  assertEqual(result.ok, true,
    'an 8-character ticker must be allowed: ' + JSON.stringify(result.errors));
});

test('a ticker with punctuation is rejected', () => {
  const result = planMint({ ...VALID_PLAN_INPUT, ticker: 'TE-ST' });
  assertEqual(result.ok, false, 'punctuation in a ticker must be rejected');
});

test('a lowercase ticker is folded to uppercase, not rejected', () => {
  const result = planMint({ ...VALID_PLAN_INPUT, ticker: 'test' });
  assertEqual(result.ok, true, 'lowercase input should fold rather than fail');
  assertEqual(result.plan.ticker, 'TEST', 'the plan should carry the normalized (uppercase) ticker');
});

run({ banner: 'Running mint tests...\n', style: 'compact' }).catch(e => {
  console.error('Test harness error:', e);
  process.exit(1);
});
