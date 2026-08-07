import { planSell, buildSellOrder } from './sell.js';
import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';
import { VERSIONS } from '../uap-js/addr.js';
import * as secp from '../uap-js/secp.js';
import { test, assert, assertEqual, assertThrows, run } from './test-harness.js';

// Wire noble's RFC6979 HMAC, as every other signing test does.
uap.configureSecp(secp);

const PRIV = new Uint8Array(32).fill(7);
const PUB = secp.getPublicKey(PRIV, true);
const ADDRESS = addr.pubkeyToAddress(PUB, VERSIONS.mainnet.PUBKEY_ADDRESS);

const POSITION = {
  txid: 'a'.repeat(64),
  vout: 1,
  multiplier: 1000,
  value: 500000000,
};

// =============================================================================
// planSell: refuse before signing, not after
// =============================================================================

test('a well-formed sale plans cleanly', () => {
  const p = planSell({ position: POSITION, priceSats: 250000000, ownAddress: ADDRESS });
  assertEqual(p.ok, true, p.errors.join(' '));
  assertEqual(p.summary.priceSats, 250000000, 'price carried through');
  assert(p.summary.paymentScript instanceof Uint8Array, 'payment script built');
});

test('a zero or negative price is refused', () => {
  for (const price of [0, -1, -250000000]) {
    const p = planSell({ position: POSITION, priceSats: price, ownAddress: ADDRESS });
    assertEqual(p.ok, false, `price ${price} must be refused`);
  }
});

test('a non-integer price is refused rather than rounded', () => {
  const p = planSell({ position: POSITION, priceSats: 1.5, ownAddress: ADDRESS });
  assertEqual(p.ok, false, 'fractional satoshis are not a thing');
});

test('a price below the dust limit is refused', () => {
  // The order would look fine in the book and be unfillable, because no
  // node relays a transaction with a dust output.
  const p = planSell({ position: POSITION, priceSats: 1, ownAddress: ADDRESS });
  assertEqual(p.ok, false, 'dust payment must be refused');
  assert(p.errors.some((e) => e.includes('dust')), 'error should name the dust limit');
});

test('a price just below the true (hard) dust limit is refused', () => {
  // The node's relay-blocking threshold is DEFAULT_HARD_DUST_LIMIT
  // (nHardDustLimit in src/policy/policy.cpp), not the softer
  // DEFAULT_DUST_LIMIT used elsewhere for change outputs and fee bumping.
  // A price of 1 satoshi passes this check regardless of which constant
  // is used, so it does not pin the boundary -- this does.
  const p = planSell({
    position: POSITION,
    priceSats: uap.DEFAULT_HARD_DUST_LIMIT - 1,
    ownAddress: ADDRESS,
  });
  assertEqual(p.ok, false, 'a price one satoshi below the hard dust limit must be refused');
  assert(p.errors.some((e) => e.includes('dust')), 'error should name the dust limit');
});

test('a price exactly at the true (hard) dust limit is accepted', () => {
  const p = planSell({
    position: POSITION,
    priceSats: uap.DEFAULT_HARD_DUST_LIMIT,
    ownAddress: ADDRESS,
  });
  assertEqual(p.ok, true, p.errors.join(' '));
});

test('a price just above the true (hard) dust limit is accepted', () => {
  const p = planSell({
    position: POSITION,
    priceSats: uap.DEFAULT_HARD_DUST_LIMIT + 1,
    ownAddress: ADDRESS,
  });
  assertEqual(p.ok, true, p.errors.join(' '));
});

test('a legitimate ask between the hard and soft dust limits is accepted', () => {
  // This is the actual bug: prices in [100000, 999999) were wrongly
  // refused because the check used DEFAULT_DUST_LIMIT (1000000) instead
  // of DEFAULT_HARD_DUST_LIMIT (100000).
  assert(
    uap.DEFAULT_HARD_DUST_LIMIT < uap.DEFAULT_DUST_LIMIT,
    'sanity: hard limit is the smaller of the two'
  );
  const p = planSell({
    position: POSITION,
    priceSats: uap.DEFAULT_DUST_LIMIT - 1,
    ownAddress: ADDRESS,
  });
  assertEqual(p.ok, true, p.errors.join(' '));
});

test('a bad payout address is refused', () => {
  const p = planSell({ position: POSITION, priceSats: 250000000, ownAddress: 'not-an-address' });
  assertEqual(p.ok, false, 'garbage address must be refused');
});

test('a malformed position is refused', () => {
  const p = planSell({ position: { txid: 'short', vout: 0, multiplier: 1 }, priceSats: 250000000, ownAddress: ADDRESS });
  assertEqual(p.ok, false, 'bad txid must be refused');
});

test('planSell signs nothing', () => {
  // Guards the ordering the review screen depends on: planning is safe to
  // run on every keystroke, signing is not.
  const p = planSell({ position: POSITION, priceSats: 250000000, ownAddress: ADDRESS });
  assertEqual(p.summary.scriptSig, undefined, 'no signature may appear in a plan');
});

// =============================================================================
// buildSellOrder
// =============================================================================

test('a built order carries every field the indexer requires', () => {
  const o = buildSellOrder({
    secp, position: POSITION, privKey: PRIV, pubKey: PUB,
    priceSats: 250000000, ownAddress: ADDRESS,
  });
  for (const field of ['txid', 'vout', 'multiplier', 'pubkey', 'script_sig', 'payment_script', 'payment_value', 'created_at']) {
    assert(o[field] !== undefined, `missing field ${field}`);
  }
  assertEqual(o.txid, POSITION.txid, 'txid');
  assertEqual(o.vout, POSITION.vout, 'vout');
  assertEqual(o.multiplier, POSITION.multiplier, 'multiplier');
  assertEqual(o.payment_value, 250000000, 'payment value in satoshis');
  assertEqual(o.pubkey, uap.bytesToHex(PUB), 'maker pubkey');
});

test('the payment script pays the address the maker asked for', () => {
  const o = buildSellOrder({
    secp, position: POSITION, privKey: PRIV, pubKey: PUB,
    priceSats: 250000000, ownAddress: ADDRESS,
  });
  const back = addr.scriptToAddress(uap.hexToBytes(o.payment_script), VERSIONS.mainnet.PUBKEY_ADDRESS);
  assertEqual(back, ADDRESS, 'payment must go to the maker');
});

test('the signature is over SIGHASH_SINGLE|ANYONECANPAY', () => {
  const o = buildSellOrder({
    secp, position: POSITION, privKey: PRIV, pubKey: PUB,
    priceSats: 250000000, ownAddress: ADDRESS,
  });
  // scriptSig is a single push of <DER sig || hashtype>; the last byte of
  // the pushed data is the hash type. 0x83 = SIGHASH_SINGLE|ANYONECANPAY.
  const bytes = uap.hexToBytes(o.script_sig);
  assertEqual(bytes[bytes.length - 1], 0x83, 'hash type byte');
});

test('a refused plan never reaches the signer', () => {
  assertThrows(
    () => buildSellOrder({
      secp, position: POSITION, privKey: PRIV, pubKey: PUB,
      priceSats: 0, ownAddress: ADDRESS,
    }),
    'greater than zero'
  );
});

test('two sales of the same position at different prices differ', () => {
  const a = buildSellOrder({ secp, position: POSITION, privKey: PRIV, pubKey: PUB, priceSats: 250000000, ownAddress: ADDRESS });
  const b = buildSellOrder({ secp, position: POSITION, privKey: PRIV, pubKey: PUB, priceSats: 260000000, ownAddress: ADDRESS });
  assert(a.script_sig !== b.script_sig, 'price is committed to by the signature');
});

run();
