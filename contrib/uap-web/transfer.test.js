// Transfer tests - sending UAP tokens to another holder.
//
// The invariant these tests exist for: TOKENS ARE CONSERVED. A transfer
// that quietly burns or invents token value is the worst bug this module
// could have, so the conservation assertions here are written against the
// *built and serialized* transaction's outputs -- parsed back out of the
// scripts -- not against the plan's own arithmetic, which could agree with
// itself while being wrong.

import {
  planTransfer,
  buildTransferTx,
  OUTPUT_TOKEN_RECIPIENT,
  OUTPUT_TOKEN_CHANGE,
  OUTPUT_WHIP_CHANGE
} from './transfer.js';
import * as uap from '../uap-js/uap.js';
import * as secp256k1 from '../uap-js/secp.js';
import * as addr from '../uap-js/addr.js';

import { test, assert, assertEqual, assertThrows, run } from './test-harness.js';

// =============================================================================
// FIXTURES
// =============================================================================

// Real keys, because buildTransferTx really signs. A fake secp would let a
// build that cannot produce a spendable transaction pass.
uap.configureSecp(secp256k1);

const COIN = uap.COIN;
// The relay-standardness floor is the HARD dust limit -- IsStandardTx in
// src/policy/policy.cpp checks nHardDustLimit. The softer DEFAULT_DUST_LIMIT
// only drives fee bumping, and pinning these tests to it made the UI reject
// transfers the network would have accepted.
const DUST = uap.DEFAULT_HARD_DUST_LIMIT;
const SOFT_DUST = uap.DEFAULT_DUST_LIMIT;

const MY_PRIVKEY = new Uint8Array(32).fill(0x11);
const MY_PUBKEY = secp256k1.getPublicKey(MY_PRIVKEY, true);
const MY_PUBKEY_HEX = uap.bytesToHex(MY_PUBKEY);

const THEIR_PRIVKEY = new Uint8Array(32).fill(0x22);
const THEIR_PUBKEY = secp256k1.getPublicKey(THEIR_PRIVKEY, true);
const THEIR_PUBKEY_HEX = uap.bytesToHex(THEIR_PUBKEY);
const THEIR_ADDRESS = addr.pubkeyToAddress(THEIR_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
const THEIR_P2SH_ADDRESS = addr.base58checkEncode(
  addr.hash160(THEIR_PUBKEY), addr.VERSIONS.regtest.SCRIPT_ADDRESS);

const MULTIPLIER = 100;

// An OP_MINT_TRANSFER position worth 100 coins, addressed to us.
function makePosition(overrides = {}) {
  return {
    txid: 'aa'.repeat(32),
    vout: 1,
    value: 100 * COIN,
    multiplier: MULTIPLIER,
    pubkey: MY_PUBKEY_HEX,
    is_mint: false,
    height: 500,
    spent: false,
    ...overrides
  };
}

// Ordinary P2PKH UTXOs, used only to pay the fee.
function makeFunding(values = [5 * COIN]) {
  return values.map((value, i) => ({
    txid: 'bb'.repeat(32),
    vout: i,
    value,
    height: 501
  }));
}

function validInput(overrides = {}) {
  return {
    position: makePosition(),
    ownPubKey: MY_PUBKEY,
    toPubKey: THEIR_PUBKEY_HEX,
    network: 'regtest',
    amountSats: 40 * COIN,
    fundingUtxos: makeFunding(),
    feeRate: 1000,
    ...overrides
  };
}

function planOrThrow(input) {
  const result = planTransfer(input);
  if (!result.ok) {
    throw new Error(`expected a valid plan, got errors: ${JSON.stringify(result.errors)}`);
  }
  return result.plan;
}

function errorFields(result) {
  assertEqual(result.ok, false, `expected rejection, got: ${JSON.stringify(result.plan)}`);
  assert(Array.isArray(result.errors) && result.errors.length > 0, 'rejection must carry errors');
  return result.errors.map((e) => e.field);
}

function assertRejects(input, field, message) {
  const result = planTransfer(input);
  const fields = errorFields(result);
  assert(fields.includes(field), `${message}\n  expected an error on "${field}", got: ${JSON.stringify(result.errors)}`);
  return result.errors;
}

// ---------------------------------------------------------------------------
// A minimal reader for the scripts we build, so conservation is asserted
// against the actual output bytes rather than against the plan that
// produced them. Recognizes `<pubkey> <multiplier> OP_MINT_TRANSFER` and
// P2PKH; anything else comes back as {kind: 'other'}.
// ---------------------------------------------------------------------------
const OP_MINT_TRANSFER = 0xba;

function parseOutputScript(script) {
  if (script.length === 25 && script[0] === 0x76 && script[1] === 0xa9 &&
      script[2] === 0x14 && script[23] === 0x88 && script[24] === 0xac) {
    return { kind: 'p2pkh', hash160: script.slice(3, 23) };
  }
  let pc = 0;
  const pushLen = script[pc];
  if (pushLen !== 33 && pushLen !== 65) return { kind: 'other' };
  pc += 1;
  const pubkey = script.slice(pc, pc + pushLen);
  pc += pushLen;
  const multByte = script[pc];
  let multiplier;
  if (multByte === 0x00) {
    multiplier = 0;
    pc += 1;
  } else if (multByte >= 0x51 && multByte <= 0x60) {
    multiplier = multByte - 0x50;
    pc += 1;
  } else if (multByte >= 1 && multByte <= 4) {
    let n = 0;
    for (let i = multByte; i >= 1; i--) n = (n << 8) | script[pc + i];
    multiplier = n;
    pc += 1 + multByte;
  } else {
    return { kind: 'other' };
  }
  if (script[pc] !== OP_MINT_TRANSFER || pc + 1 !== script.length) return { kind: 'other' };
  return { kind: 'covenant', pubkey, multiplier };
}

async function buildOrThrow(plan) {
  return buildTransferTx({ secp: secp256k1, plan, privKey: MY_PRIVKEY });
}

// =============================================================================
// VALID PLANS
// =============================================================================

test('a valid partial transfer plans successfully', () => {
  const plan = planOrThrow(validInput());
  assertEqual(plan.amountSats, 40 * COIN);
  assertEqual(plan.remainderSats, 60 * COIN);
  assertEqual(plan.multiplier, MULTIPLIER);
});

test('a partial transfer emits recipient, token change, and WHIP change in that order', () => {
  const plan = planOrThrow(validInput());
  const kinds = plan.outputs.map((o) => o.kind);
  assertEqual(kinds.length, 3, `expected 3 outputs, got ${JSON.stringify(kinds)}`);
  assertEqual(kinds[0], OUTPUT_TOKEN_RECIPIENT, 'the recipient must be output 0');
  assertEqual(kinds[1], OUTPUT_TOKEN_CHANGE, 'the token remainder must be output 1');
  assertEqual(kinds[2], OUTPUT_WHIP_CHANGE, 'WHIP change comes last');
});

test('sending the whole position emits no token change output', () => {
  const plan = planOrThrow(validInput({ amountSats: 100 * COIN }));
  assertEqual(plan.remainderSats, 0);
  const kinds = plan.outputs.map((o) => o.kind);
  assert(!kinds.includes(OUTPUT_TOKEN_CHANGE), `no token change expected, got ${JSON.stringify(kinds)}`);
  assertEqual(kinds[0], OUTPUT_TOKEN_RECIPIENT);
});

test('the recipient output pays the recipient, and the token change pays us', () => {
  const plan = planOrThrow(validInput());
  const recipient = plan.outputs.find((o) => o.kind === OUTPUT_TOKEN_RECIPIENT);
  const change = plan.outputs.find((o) => o.kind === OUTPUT_TOKEN_CHANGE);
  assertEqual(uap.bytesToHex(recipient.pubKey), THEIR_PUBKEY_HEX, 'recipient output must carry THEIR pubkey');
  assertEqual(uap.bytesToHex(change.pubKey), MY_PUBKEY_HEX, 'token change must come back to us');
});

test('a hex recipient pubkey and a byte recipient pubkey plan identically', () => {
  const fromHex = planOrThrow(validInput({ toPubKey: THEIR_PUBKEY_HEX }));
  const fromBytes = planOrThrow(validInput({ toPubKey: THEIR_PUBKEY }));
  assertEqual(uap.bytesToHex(fromHex.toPubKey), uap.bytesToHex(fromBytes.toPubKey));
});

// =============================================================================
// CONSERVATION -- the reason this module has tests at all
// =============================================================================

test('a partial transfer conserves the position value exactly, to the satoshi', () => {
  const plan = planOrThrow(validInput());
  const tokenOut = plan.outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + o.value, 0);
  assertEqual(tokenOut, 100 * COIN, 'covenant outputs must sum to the position value: no burn, no inflation');
  assertEqual(plan.tokenValueIn, plan.tokenValueOut, 'plan must agree that value in == value out');
});

test('the fee never comes out of the token value', () => {
  const plan = planOrThrow(validInput());
  const tokenOut = plan.outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + o.value, 0);
  assert(plan.fee > 0, 'a transfer must actually pay a fee');
  assertEqual(tokenOut, plan.position.value,
    'the fee must be funded by the WHIP inputs, not skimmed off the covenant outputs');
  assertEqual(plan.fee + plan.change, plan.fundingTotal,
    'every funding satoshi must be accounted for as fee or change');
});

test('the BUILT transaction conserves token value across every covenant output', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);

  let covenantTotal = 0;
  let covenantCount = 0;
  for (const out of tx.vout) {
    const parsed = parseOutputScript(out.scriptPubKey);
    if (parsed.kind !== 'covenant') continue;
    covenantCount++;
    covenantTotal += out.value;
    assertEqual(parsed.multiplier, MULTIPLIER,
      'every covenant output must carry the SAME multiplier as the position it spends');
  }

  assertEqual(covenantCount, 2, 'a partial transfer has exactly two covenant outputs');
  assertEqual(covenantTotal, 100 * COIN,
    'the built transaction must move the position\'s full value into covenant outputs');
});

test('sending the whole position puts every satoshi in one covenant output', async () => {
  const plan = planOrThrow(validInput({ amountSats: 100 * COIN }));
  const { tx } = await buildOrThrow(plan);
  const covenants = tx.vout
    .map((o) => ({ parsed: parseOutputScript(o.scriptPubKey), value: o.value }))
    .filter((o) => o.parsed.kind === 'covenant');
  assertEqual(covenants.length, 1, 'exactly one covenant output');
  assertEqual(covenants[0].value, 100 * COIN, 'carrying the position\'s entire value');
  assertEqual(uap.bytesToHex(covenants[0].parsed.pubkey), THEIR_PUBKEY_HEX);
});

test('the covenant outputs never exceed the position value (consensus would reject that)', () => {
  for (const amount of [DUST, 10 * COIN, 50 * COIN, 99 * COIN, 100 * COIN]) {
    const plan = planOrThrow(validInput({ amountSats: amount }));
    const tokenOut = plan.outputs
      .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
      .reduce((sum, o) => sum + o.value, 0);
    assert(tokenOut <= plan.position.value,
      `sending ${amount}: covenant total ${tokenOut} exceeds input ${plan.position.value}`);
    assertEqual(tokenOut, plan.position.value, `sending ${amount}: value must be conserved, not merely bounded`);
  }
});

test('virtual balance is conserved exactly, fractional-coin splits included', () => {
  // Consensus computes balance as nValue * multiplier in RAW SATOSHIS
  // (interpreter.cpp OP_INSPECT selector 11) and enforces conservation on raw
  // satoshis (nValueOut > nValueIn fails). No division, no truncation. Balance
  // is therefore exactly proportional to satoshi value, and a split at ANY
  // boundary -- whole-coin or not -- conserves it.
  const clean = planOrThrow(validInput({ amountSats: 40 * COIN }));
  assertEqual(clean.virtualBalanceIn, 100 * MULTIPLIER);
  assertEqual(clean.virtualBalanceOut, 100 * MULTIPLIER);
  assertEqual(clean.virtualBalanceLost, 0, 'a whole-coin split loses nothing');

  // The case a truncating implementation gets wrong: 100 coins split into
  // 40.5 + 59.5. Under floor(value/COIN)*multiplier each piece rounds down and
  // a whole coin's worth of balance vanishes. Under the real rule, nothing does.
  const fractional = planOrThrow(validInput({ amountSats: 40 * COIN + COIN / 2 }));
  assertEqual(fractional.tokenValueIn, fractional.tokenValueOut, 'satoshis are conserved exactly');
  assertEqual(fractional.virtualBalanceIn, 100 * MULTIPLIER);
  assertEqual(fractional.virtualBalanceOut, 100 * MULTIPLIER,
    'a fractional split conserves balance; consensus does not truncate');
  assertEqual(fractional.virtualBalanceLost, 0,
    'nothing is rounded away, so the plan must not claim anything was');

  // Sub-coin amounts carry balance at all. A truncating rule would price a
  // half-coin send at zero tokens, which is the same bug seen from the far end.
  const dust = planOrThrow(validInput({ amountSats: COIN / 2 }));
  const recipient = dust.outputs.find((o) => o.kind === OUTPUT_TOKEN_RECIPIENT);
  assertEqual(recipient.value, COIN / 2);
  assertEqual(dust.virtualBalanceOut, 100 * MULTIPLIER, 'the position keeps its full balance');
  assertEqual(dust.virtualBalanceLost, 0);
});

// =============================================================================
// SATOSHI FIGURES THAT A JAVASCRIPT NUMBER CANNOT HOLD
//
// MAX_MONEY is 1e18 satoshis (src/amount.h), about 111x
// Number.MAX_SAFE_INTEGER. Above 2^53 the planner's own arithmetic stops
// being reversible -- (V - a) + a is not V -- so a plan can report perfect
// conservation while its outputs burn or invent satoshis. Both directions
// really happen, and neither is visible from inside the plan, because every
// figure it checks itself against is computed the same lossy way. The only
// safe answer is to refuse.
// =============================================================================

const UNSAFE_SATS = Number.MAX_SAFE_INTEGER + 2;   // 9007199254740993, not representable

test('a position value above 2^53-1 is refused rather than silently mis-split', () => {
  // The exact reproduction: V = 2e17, a = 300000001. The plan's own
  // tokenValueIn/tokenValueOut agree and virtualBalanceLost is 0, while the
  // exact sum of the covenant outputs is V + 1 -- one satoshi MORE than the
  // input, which consensus rejects (nValueOut > nValueIn, interpreter.cpp).
  const V = 200000000000000000;
  const a = 300000001;
  // The exact sum of the two output values the planner would emit. It is a
  // satoshi OVER the input, even though the planner's own double arithmetic
  // says `(V - a) + a === V` -- which is precisely why the plan cannot catch
  // this and the input has to be refused.
  assertEqual((V - a) + a === V, true, 'sanity: the plan\'s own arithmetic sees nothing wrong');
  assertEqual(BigInt(V - a) + BigInt(a) - BigInt(V), 1n,
    'sanity: the outputs this pair would really encode overshoot the input by one satoshi');
  const errors = assertRejects(
    validInput({ position: makePosition({ value: V }), amountSats: a }),
    'position',
    'a value this wallet cannot split without losing or inventing satoshis must not be planned'
  );
  assert(errors.some((e) => /larger than 9007199254740991/.test(e.message)),
    `the error must name the limit that was exceeded, got ${JSON.stringify(errors)}`);
});

test('the understating direction is refused too, not just the overstating one', () => {
  // V = 470455846868959700, a = 767191370684 loses 4 satoshis: the outputs
  // total LESS than the input, consensus accepts the transaction, and the
  // difference leaves the covenant as extra fee. Tokens destroyed silently,
  // with a confirmation to show for it -- the direction that has no alarm.
  const V = 470455846868959700;
  const a = 767191370684;
  const exactSum = BigInt(V - a) + BigInt(a);
  assert(exactSum < BigInt(V),
    `sanity: this pair must understate, got ${exactSum} against ${BigInt(V)}`);
  assertRejects(validInput({ position: makePosition({ value: V }), amountSats: a }), 'position',
    'the silent direction is the one that costs the user tokens');
});

test('an amount above 2^53-1 is refused on its own terms', () => {
  const errors = assertRejects(
    validInput({ position: makePosition({ value: 100 * COIN }), amountSats: UNSAFE_SATS }),
    'amountSats',
    'an unsafe amount must be named as such, not merely as "more than the position holds"'
  );
  assert(errors.some((e) => e.field === 'amountSats' && /larger than 9007199254740991/.test(e.message)),
    `the amount error must name the limit, got ${JSON.stringify(errors)}`);
});

test('the largest safe value is still planned, so the bound is a bound and not a wall', () => {
  // 2^53-1 satoshis exactly. Every figure here round-trips, so there is
  // nothing to refuse -- a limit that also rejected the last legal value
  // would look identical to a correct one under the tests above.
  const V = Number.MAX_SAFE_INTEGER;
  const plan = planOrThrow(validInput({ position: makePosition({ value: V }), amountSats: 40 * COIN }));
  assertEqual(plan.tokenValueIn, V);
  assertEqual(plan.tokenValueOut, V, 'the outputs must still account for every satoshi');
  const tokenOut = plan.outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + o.value, 0);
  assertEqual(tokenOut, V);
});

// =============================================================================
// AMOUNT VALIDATION
// =============================================================================

test('an amount larger than the position is rejected', () => {
  assertRejects(validInput({ amountSats: 101 * COIN }), 'amountSats',
    'must not plan a transfer of more tokens than the position holds');
});

test('a zero or negative amount is rejected', () => {
  assertRejects(validInput({ amountSats: 0 }), 'amountSats', 'zero is not a transfer');
  assertRejects(validInput({ amountSats: -1 }), 'amountSats', 'negative amounts are nonsense');
});

test('a non-integer amount is rejected rather than silently truncated', () => {
  assertRejects(validInput({ amountSats: 1.5 * DUST + 0.5 }), 'amountSats',
    'fractional satoshis cannot be serialized and must not be rounded for the user');
});

test('an amount below the dust limit is rejected', () => {
  assertRejects(validInput({ amountSats: DUST - 1 }), 'amountSats',
    'a dust covenant output would not relay');
});

test('an amount between the hard and soft dust limits is accepted', () => {
  // The bug this replaces: these values relay fine, but the UI refused them.
  const result = planTransfer(validInput({ amountSats: SOFT_DUST - 1 }));
  assertEqual(result.ok, true, JSON.stringify(result.errors));
});

test('an amount exactly at the hard dust limit is accepted', () => {
  const result = planTransfer(validInput({ amountSats: DUST }));
  assertEqual(result.ok, true, JSON.stringify(result.errors));
});

test('an amount that would leave a dust remainder is rejected', () => {
  const result = planTransfer(validInput({ amountSats: 100 * COIN - (DUST - 1) }));
  const errors = assertRejects(validInput({ amountSats: 100 * COIN - (DUST - 1) }), 'amountSats',
    'leaving dust behind in the position is a trap');
  assert(errors.some((e) => /dust remainder/.test(e.message)),
    `error should explain the dust remainder, got ${JSON.stringify(errors)}`);
  assertEqual(result.ok, false);
});

// =============================================================================
// POSITION / OWNERSHIP VALIDATION
// =============================================================================

test('a missing position is rejected', () => {
  assertRejects(validInput({ position: null }), 'position', 'cannot send from nothing');
});

test('a position addressed to someone else is rejected', () => {
  assertRejects(
    validInput({ position: makePosition({ pubkey: THEIR_PUBKEY_HEX }) }),
    'position',
    'this wallet holds no key for that position'
  );
});

test('an already-spent position is rejected', () => {
  assertRejects(validInput({ position: makePosition({ spent: true }) }), 'position',
    'spending a spent position produces a transaction the node will reject');
});

test('a fresh mint position without its script is rejected, naming the salt', () => {
  const errors = assertRejects(
    validInput({ position: makePosition({ is_mint: true }) }),
    'position',
    'the salt is not recoverable from the indexer, so the scriptCode is unknown'
  );
  assert(errors.some((e) => /salt/.test(e.message)),
    `error should name the salt, got ${JSON.stringify(errors)}`);
});

test('a fresh mint position WITH an explicit script_hex is accepted', () => {
  const salt = new Uint8Array(20).fill(0x33);
  const mintScript = uap.buildMintScript(MY_PUBKEY, MULTIPLIER, salt);
  const plan = planOrThrow(validInput({
    position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(mintScript) })
  }));
  assertEqual(plan.amountSats, 40 * COIN);
});

test('a malformed position txid is rejected', () => {
  assertRejects(validInput({ position: makePosition({ txid: 'not-a-txid' }) }), 'position',
    'a bad outpoint would build a transaction spending nothing');
});

test('a non-integer position vout is rejected', () => {
  assertRejects(validInput({ position: makePosition({ vout: 1.5 }) }), 'position',
    'vout must be a whole index');
});

test('an out-of-range position multiplier is rejected', () => {
  assertRejects(validInput({ position: makePosition({ multiplier: 2147483648 }) }), 'position',
    'consensus caps the multiplier and would reject the covenant');
  assertRejects(validInput({ position: makePosition({ multiplier: -1 }) }), 'position',
    'a negative multiplier is not encodable');
});

// =============================================================================
// RECIPIENT VALIDATION
// =============================================================================

test('a missing recipient pubkey is rejected', () => {
  assertRejects(validInput({ toPubKey: undefined }), 'toPubKey', 'there is nowhere to send to');
});

test('a recipient "pubkey" of the wrong length is rejected', () => {
  assertRejects(validInput({ toPubKey: 'ab'.repeat(32) }), 'toPubKey',
    '32 bytes is not a pubkey; consensus would reject the covenant outright');
});

test('a right-length recipient pubkey with a bad prefix is rejected', () => {
  // 33 bytes starting 0x07 is not a valid compressed point. Consensus
  // accepts any 33-byte push as "a pubkey", so this would build a
  // permanently unspendable covenant if it got through.
  const bogus = '07' + 'ab'.repeat(32);
  assertRejects(validInput({ toPubKey: bogus }), 'toPubKey',
    'a 33-byte non-key would make an unspendable output');
});

test('a non-hex recipient pubkey is rejected', () => {
  assertRejects(validInput({ toPubKey: 'zz'.repeat(33) }), 'toPubKey', 'not hex at all');
});

test('an uncompressed (65-byte) recipient pubkey is accepted', () => {
  const uncompressed = secp256k1.getPublicKey(THEIR_PRIVKEY, false);
  const plan = planOrThrow(validInput({ toPubKey: uncompressed }));
  assertEqual(plan.toPubKey.length, 65);
});

test('a matching recipient address passes the cross-check', () => {
  const plan = planOrThrow(validInput({ toAddress: THEIR_ADDRESS }));
  assertEqual(uap.bytesToHex(plan.toPubKey), THEIR_PUBKEY_HEX);
});

test('an address that does not match the recipient pubkey is rejected', () => {
  const myAddress = addr.pubkeyToAddress(MY_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
  const errors = assertRejects(validInput({ toAddress: myAddress }), 'toAddress',
    'the pubkey and the address name different people; one of them is wrong');
  assert(errors.some((e) => /hash160 mismatch/.test(e.message)),
    `error should explain the mismatch, got ${JSON.stringify(errors)}`);
});

test('a P2SH recipient address is rejected as unsupported, not as a mismatch', () => {
  // The P2SH address here hashes THEIR pubkey, so its payload matches
  // hash160(toPubKey) exactly -- only the version byte differs. It must
  // still be refused: this wallet cannot build a spendable P2SH output
  // (addr.addressToScript throws rather than mis-encoding one), and a
  // P2PKH-shaped output built from a script hash would be unspendable.
  const errors = assertRejects(validInput({ toAddress: THEIR_P2SH_ADDRESS }), 'toAddress',
    'P2SH must not be quietly accepted just because its payload matches');
  assert(errors.some((e) => /P2SH/.test(e.message)),
    `error should name P2SH, got ${JSON.stringify(errors)}`);
});

test('an address from another network is rejected', () => {
  const testnetAddress = addr.pubkeyToAddress(THEIR_PUBKEY, addr.VERSIONS.testnet.PUBKEY_ADDRESS);
  assertRejects(validInput({ toAddress: testnetAddress, network: 'regtest' }), 'toAddress',
    'a testnet address must not be accepted on regtest');
});

test('a garbage recipient address is rejected', () => {
  assertRejects(validInput({ toAddress: 'definitely not an address' }), 'toAddress',
    'checksum validation must apply here too');
});

test('an omitted recipient address simply skips the cross-check', () => {
  const plan = planOrThrow(validInput({ toAddress: '' }));
  assertEqual(uap.bytesToHex(plan.toPubKey), THEIR_PUBKEY_HEX);
});

// =============================================================================
// FEE / FUNDING
// =============================================================================

test('no funding UTXOs is rejected with an explanation', () => {
  const errors = assertRejects(validInput({ fundingUtxos: [] }), 'fundingUtxos',
    'without a WHIP input the fee could only come out of the tokens');
  assert(errors.some((e) => /fee/.test(e.message)),
    `error should mention the fee, got ${JSON.stringify(errors)}`);
});

test('funding too small to cover the fee is rejected', () => {
  assertRejects(validInput({ fundingUtxos: makeFunding([1000]) }), 'fundingUtxos',
    '1000 satoshis cannot pay a 0.01-coin minimum fee');
});

test('the fee is at least the recommended minimum', () => {
  const plan = planOrThrow(validInput());
  assert(plan.fee >= uap.RECOMMENDED_MIN_TX_FEE,
    `fee ${plan.fee} is below the ${uap.RECOMMENDED_MIN_TX_FEE} floor`);
});

test('a higher fee rate produces a higher fee', () => {
  const cheap = planOrThrow(validInput({ feeRate: 1000 }));
  const dear = planOrThrow(validInput({ feeRate: 5000000 }));
  assert(dear.fee > cheap.fee, `fee should scale with feeRate: ${cheap.fee} vs ${dear.fee}`);
});

test('a non-positive fee rate is rejected', () => {
  assertRejects(validInput({ feeRate: 0 }), 'feeRate', 'a zero fee rate is not a rate');
  assertRejects(validInput({ feeRate: -1 }), 'feeRate', 'a negative fee rate is nonsense');
});

test('dust-sized WHIP change is absorbed into the fee rather than emitted', () => {
  // Fund with exactly (minimum fee + a sliver): the leftover is below the
  // dust limit, so there must be no WHIP change output and the reported fee
  // must be the real, larger one.
  const funding = makeFunding([uap.RECOMMENDED_MIN_TX_FEE + 1000]);
  const plan = planOrThrow(validInput({ fundingUtxos: funding, feeRate: 1000 }));
  assertEqual(plan.change, 0, 'sub-dust change must not become an output');
  assert(!plan.outputs.some((o) => o.kind === OUTPUT_WHIP_CHANGE), 'no WHIP change output');
  assertEqual(plan.fee, funding[0].value, 'the reported fee must be what is actually paid');
});

test('funding selection reports the fee actually paid, never the estimate', () => {
  const plan = planOrThrow(validInput());
  assertEqual(plan.fundingTotal - plan.change, plan.fee,
    'fee must equal funding minus change by construction');
});

test('malformed funding UTXOs are rejected rather than silently skipped', () => {
  assertRejects(
    validInput({ fundingUtxos: [{ txid: 'bb'.repeat(32), vout: 0, value: 'lots' }] }),
    'fundingUtxos',
    'a non-numeric value would poison the fee arithmetic'
  );
});

// =============================================================================
// BUILD / SIGN
// =============================================================================

test('the built transaction spends the position first, then the funding inputs', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  assertEqual(tx.vin.length, 2, 'one covenant input plus one funding input');
  assertEqual(tx.vin[0].txid, plan.position.txid, 'the position must be input 0');
  assertEqual(tx.vin[0].vout, plan.position.vout);
  assertEqual(tx.vin[1].txid, plan.fundingInputs[0].txid, 'funding inputs follow');
});

test('a transfer spends EXACTLY ONE UAP position', async () => {
  // Consensus runs the conservation check once per UAP input, each demanding
  // the whole covenant output total fit within that input's value -- so two
  // UAP inputs can never both be satisfied. The builder must never produce
  // more than one.
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  const uapInputs = tx.vin.filter((vin) => vin.txid === plan.position.txid && vin.vout === plan.position.vout);
  assertEqual(uapInputs.length, 1, 'exactly one UAP input');
});

test('every input is signed', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  for (let i = 0; i < tx.vin.length; i++) {
    assert(tx.vin[i].scriptSig && tx.vin[i].scriptSig.length > 0, `input ${i} is unsigned`);
  }
});

test('the covenant input is signed as a bare signature, the funding input as P2PKH', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);

  // A covenant scriptSig is a single push of <sig+hashtype>; a P2PKH
  // scriptSig is that push followed by a push of the pubkey. Signing the
  // covenant input the P2PKH way leaves a stray pubkey on the stack.
  const covenantSig = tx.vin[0].scriptSig;
  assertEqual(covenantSig[0] + 1, covenantSig.length,
    'the covenant scriptSig must be exactly one push (no trailing pubkey)');

  const p2pkhSig = tx.vin[1].scriptSig;
  const firstPush = p2pkhSig[0];
  assertEqual(p2pkhSig[firstPush + 1], MY_PUBKEY.length,
    'the P2PKH scriptSig must push the pubkey after the signature');
});

test('the built transaction serializes to hex', async () => {
  const plan = planOrThrow(validInput());
  const { rawHex } = await buildOrThrow(plan);
  assert(typeof rawHex === 'string' && /^[0-9a-f]+$/.test(rawHex), 'rawHex must be lowercase hex');
  assert(rawHex.length > 100, 'a signed transfer is not a short string');
});

test('the WHIP change output pays back to our own hash160', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  const change = tx.vout
    .map((o) => ({ parsed: parseOutputScript(o.scriptPubKey), value: o.value }))
    .find((o) => o.parsed.kind === 'p2pkh');
  assert(change, 'a WHIP change output was planned and must be built');
  assertEqual(uap.bytesToHex(change.parsed.hash160), uap.bytesToHex(addr.hash160(MY_PUBKEY)));
});

test('the built output values match the plan output values, in order', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  assertEqual(tx.vout.length, plan.outputs.length);
  for (let i = 0; i < plan.outputs.length; i++) {
    assertEqual(tx.vout[i].value, plan.outputs[i].value, `output ${i} value`);
  }
});

test('buildTransferTx refuses a missing or planless call', async () => {
  await assertThrows(
    () => buildTransferTx({ secp: secp256k1, plan: null, privKey: MY_PRIVKEY }),
    'requires a plan'
  );
  await assertThrows(
    () => buildTransferTx({ secp: secp256k1, plan: planOrThrow(validInput()), privKey: null }),
    'requires a private key'
  );
});

test('two transfers of the same position produce the same output scripts', async () => {
  // No randomness anywhere in a transfer (unlike a mint, which generates a
  // salt), so the covenant outputs must be byte-identical run to run.
  const a = await buildOrThrow(planOrThrow(validInput()));
  const b = await buildOrThrow(planOrThrow(validInput()));
  for (let i = 0; i < a.tx.vout.length; i++) {
    assertEqual(
      uap.bytesToHex(a.tx.vout[i].scriptPubKey),
      uap.bytesToHex(b.tx.vout[i].scriptPubKey),
      `output ${i} script must be deterministic`
    );
  }
});

test('the recipient covenant script is exactly what uap.buildTransferScript emits', async () => {
  // Not a tautology worth skipping: it pins that this module reuses the
  // shared builder rather than assembling its own copy of the format.
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  const expected = uap.buildTransferScript(THEIR_PUBKEY, MULTIPLIER);
  assertEqual(uap.bytesToHex(tx.vout[0].scriptPubKey), uap.bytesToHex(expected));
});

// =============================================================================
// WHAT THE SIGNATURE COMMITS TO
//
// The two things a signature is: a hash type, and a scriptCode. Neither is
// visible in the plan and neither shows up in any output value, so nothing
// above these tests would notice if either changed. A SIGHASH_NONE signature
// on the covenant input authorises spending the position to ANY outputs at
// all -- the whole conservation story above becomes decoration. A wrong
// scriptCode is a signature over a script the wallet was never shown.
// =============================================================================

// The scriptSig of the covenant input is a single push of <DER sig || hashtype>,
// so the hash type is the last byte. A P2PKH scriptSig is <sig||hashtype>
// <pubkey>, so its hash type sits at the end of the first push: index
// script[0], counting the length byte at index 0.
function covenantHashType(scriptSig) {
  return scriptSig[scriptSig.length - 1];
}

function p2pkhHashType(scriptSig) {
  return scriptSig[scriptSig[0]];
}

test('the covenant input is signed with SIGHASH_ALL over the position\'s own covenant script', async () => {
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);

  assertEqual(covenantHashType(tx.vin[0].scriptSig), uap.SIGHASH_ALL,
    'the covenant signature must commit to every output; anything but SIGHASH_ALL lets the ' +
    'outputs be replaced after signing');

  // RFC6979 makes signing deterministic, so the correct signature can be
  // recomputed here from first principles: the position's own covenant
  // script as scriptCode, SIGHASH_ALL, input 0. Byte equality pins both.
  const expected = uap.signSpend(
    secp256k1, uap.buildTransferScript(MY_PUBKEY, MULTIPLIER), MY_PRIVKEY, tx, 0, uap.SIGHASH_ALL);
  assertEqual(uap.bytesToHex(tx.vin[0].scriptSig), uap.bytesToHex(expected),
    'the covenant input must be signed over <ownPubKey> <multiplier> OP_MINT_TRANSFER, with SIGHASH_ALL');
});

test('the covenant signature really does depend on its scriptCode and hash type', async () => {
  // The negative control for the test above: if either of the two things
  // that test pins were not load-bearing, these signatures would collide
  // with the real one and the equality assertion would prove nothing.
  const plan = planOrThrow(validInput());
  const { tx } = await buildOrThrow(plan);
  const real = uap.bytesToHex(tx.vin[0].scriptSig);
  const ownScript = uap.buildTransferScript(MY_PUBKEY, MULTIPLIER);

  const wrongScriptCode = uap.bytesToHex(
    uap.signSpend(secp256k1, uap.buildTransferScript(THEIR_PUBKEY, MULTIPLIER), MY_PRIVKEY, tx, 0, uap.SIGHASH_ALL));
  assert(wrongScriptCode !== real,
    'signing over the recipient\'s script instead of the position\'s must produce a different signature');

  const wrongHashType = uap.bytesToHex(
    uap.signSpend(secp256k1, ownScript, MY_PRIVKEY, tx, 0, uap.SIGHASH_NONE));
  assert(wrongHashType !== real, 'SIGHASH_NONE must produce a different signature from SIGHASH_ALL');
});

test('every fee-funding input is signed with SIGHASH_ALL too', async () => {
  const plan = planOrThrow(validInput({ fundingUtxos: makeFunding([2 * COIN, 3 * COIN]) }));
  const { tx } = await buildOrThrow(plan);
  assert(tx.vin.length >= 2, 'there must be at least one funding input to check');
  for (let i = 1; i < tx.vin.length; i++) {
    assertEqual(p2pkhHashType(tx.vin[i].scriptSig), uap.SIGHASH_ALL,
      `funding input ${i} must also commit to the outputs, or it can be reused against a different tx`);
  }
});

test('a fresh mint is signed over the exact script_hex given, not a reconstructed transfer script', async () => {
  const salt = new Uint8Array(20).fill(0x33);
  const mintScript = uap.buildMintScript(MY_PUBKEY, MULTIPLIER, salt);
  const plan = planOrThrow(validInput({
    position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(mintScript) })
  }));
  const { tx } = await buildOrThrow(plan);

  const expected = uap.signSpend(secp256k1, mintScript, MY_PRIVKEY, tx, 0, uap.SIGHASH_ALL);
  assertEqual(uap.bytesToHex(tx.vin[0].scriptSig), uap.bytesToHex(expected),
    'the mint script -- salt and all -- is the scriptCode consensus will use');

  const asTransfer = uap.bytesToHex(
    uap.signSpend(secp256k1, uap.buildTransferScript(MY_PUBKEY, MULTIPLIER), MY_PRIVKEY, tx, 0, uap.SIGHASH_ALL));
  assert(asTransfer !== uap.bytesToHex(expected),
    'negative control: a mint script and a transfer script must not sign to the same bytes');
});

// =============================================================================
// script_hex IS A SIGNING ORACLE UNLESS IT IS VALIDATED
//
// script_hex arrives from the indexer and is used verbatim as the scriptCode.
// Whatever is in it is what the wallet's signature authorises. It must
// therefore be proven to be the covenant the position claims to be before a
// single byte of it reaches signatureHash().
// =============================================================================

function withScript(script) {
  return validInput({
    position: makePosition({
      script_hex: script instanceof Uint8Array ? uap.bytesToHex(script) : script
    })
  });
}

test('a script_hex that is not a UAP covenant at all is rejected', () => {
  assertRejects(withScript(addr.buildP2PKHScript(MY_PUBKEY)), 'position',
    'a P2PKH script is not a covenant and must never be used as a covenant scriptCode');
  assertRejects(withScript(new Uint8Array([0xba])), 'position', 'a bare opcode is not a covenant');
  assertRejects(withScript('not hex'), 'position', 'unparseable hex must not reach the signer');
  assertRejects(withScript(uap.bytesToHex(uap.buildTransferScript(MY_PUBKEY, MULTIPLIER)) + 'ff'),
    'position', 'trailing bytes after OP_MINT_TRANSFER make it a different script');
});

test('a script_hex whose "pubkey" is not 33 or 65 bytes is rejected as not-a-covenant', () => {
  // Consensus requires exactly 33 or 65 (ParseUapOutputScript, script.cpp:316)
  // because no signature can ever be produced for anything else, so such an
  // output is permanently unspendable while still counting as a "continuing
  // covenant" for conservation. The assertion is on the MESSAGE, not merely
  // on rejection: a 32-byte key also fails the pubkey-mismatch check, so
  // asserting only "rejected" would pass with no length check at all.
  //
  // Nor does the re-encode check cover this: uap.buildTransferScript happily
  // emits a script around a 32-byte push, so the script round-trips to itself.
  const shortScript = uap.buildTransferScript(MY_PUBKEY.slice(1), MULTIPLIER);
  const errors = assertRejects(withScript(shortScript), 'position',
    'a wrong-length pubkey push is not a UAP covenant, however well-formed the rest is');
  assert(errors.some((e) => /canonically encoded UAP covenant/.test(e.message)),
    `it must be refused as an unparseable covenant, not as someone else's key, got ${JSON.stringify(errors)}`);

  // 66 bytes: past the uncompressed length, same rule.
  const longScript = uap.buildTransferScript(uap.concatBytes(MY_PUBKEY, MY_PUBKEY), MULTIPLIER);
  const longErrors = assertRejects(withScript(longScript), 'position', 'nor is an over-long one');
  assert(longErrors.some((e) => /canonically encoded UAP covenant/.test(e.message)),
    `the same for an over-long push, got ${JSON.stringify(longErrors)}`);
});

test('the mint salt length boundary is exactly consensus\'s: 15 bytes out, 16 bytes in', () => {
  // vch.size() < 16 in ParseUapOutputScript (script.cpp:357). uap.js will not
  // BUILD a script below that, so the short-salt case is assembled by hand --
  // which is also how it would arrive, off the wire from an indexer.
  const shortSalt = new Uint8Array(15).fill(0x33);
  const handBuilt = uap.concatBytes(
    Uint8Array.of(33), MY_PUBKEY,
    Uint8Array.of(0x01, MULTIPLIER),
    Uint8Array.of(shortSalt.length), shortSalt,
    Uint8Array.of(0xb5)
  );
  const errors = assertRejects(
    validInput({ position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(handBuilt) }) }),
    'position',
    'consensus will not parse this as a covenant, so this wallet must not sign over it as one'
  );
  assert(errors.some((e) => /canonically encoded UAP covenant/.test(e.message)),
    `a short salt makes it not-a-covenant, got ${JSON.stringify(errors)}`);

  // 16 bytes exactly is the boundary and must be ACCEPTED. Without this half
  // the test above would pass just as well against a guard that refused every
  // mint script, or one set at any threshold above 16.
  planOrThrow(validInput({
    position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(uap.buildMintScript(MY_PUBKEY, MULTIPLIER, new Uint8Array(16).fill(0x33))) })
  }));
});

test('a salt too large for uap.js to re-encode is reported, not thrown', () => {
  // uap.js's pushData throws above 0xffff bytes; consensus does not
  // (CheckMinimalPush returns true for data.size() > 65535, script.cpp:287).
  // Such a script really can be served to this wallet, and planTransfer's
  // contract is {ok:false, errors} -- app.js's planSendTx has no try/catch, so
  // a throw here is an unhandled rejection and a UI that silently does nothing.
  const hugeSalt = new Uint8Array(65536).fill(0x33);
  const pubkeyPush = uap.concatBytes(Uint8Array.of(33), MY_PUBKEY);
  const multPush = Uint8Array.of(0x01, 0x64);   // MULTIPLIER = 100
  const saltPush = uap.concatBytes(
    Uint8Array.of(0x4e, 0x00, 0x00, 0x01, 0x00),  // OP_PUSHDATA4, 65536
    hugeSalt
  );
  const script = uap.concatBytes(pubkeyPush, multPush, saltPush, Uint8Array.of(0xb5));

  let result;
  try {
    result = planTransfer(validInput({
      position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(script) })
    }));
  } catch (e) {
    throw new Error(`planTransfer must return errors, not throw, on attacker-supplied script_hex: ${e.message}`);
  }
  const fields = errorFields(result);
  assert(fields.includes('position'), `expected a position error, got ${JSON.stringify(result.errors)}`);
});

test('a script_hex addressed to someone else is rejected', () => {
  const errors = assertRejects(withScript(uap.buildTransferScript(THEIR_PUBKEY, MULTIPLIER)), 'position',
    'signing over a covenant that pays someone else is the signing-oracle case');
  assert(errors.some((e) => /public key/i.test(e.message)),
    `error should name the pubkey mismatch, got ${JSON.stringify(errors)}`);
});

test('a script_hex carrying a different multiplier than the position is rejected', () => {
  const errors = assertRejects(withScript(uap.buildTransferScript(MY_PUBKEY, MULTIPLIER + 1)), 'position',
    'the multiplier the signature commits to must be the one the plan reasoned about');
  assert(errors.some((e) => /multiplier/i.test(e.message)),
    `error should name the multiplier, got ${JSON.stringify(errors)}`);
});

test('a script_hex with a non-canonical multiplier push is rejected', () => {
  // 5 must be OP_5 (0x55), not a one-byte data push of 0x05. Consensus
  // rejects the non-canonical form outright (ParseUapOutputScript ->
  // CheckMinimalPush), so a position "spendable" only via such a script is
  // not a position at all.
  const pubkeyPush = uap.concatBytes(Uint8Array.of(33), MY_PUBKEY);
  const nonCanonical = uap.concatBytes(pubkeyPush, Uint8Array.of(0x01, 0x05), Uint8Array.of(0xba));
  assertRejects(
    validInput({ position: makePosition({ multiplier: 5, script_hex: uap.bytesToHex(nonCanonical) }) }),
    'position',
    'a non-canonically encoded multiplier is not a valid covenant'
  );
});

test('a script_hex whose form contradicts is_mint is rejected', () => {
  const salt = new Uint8Array(20).fill(0x33);
  assertRejects(
    validInput({ position: makePosition({ is_mint: false, script_hex: uap.bytesToHex(uap.buildMintScript(MY_PUBKEY, MULTIPLIER, salt)) }) }),
    'position',
    'a mint script under a position the indexer says is not a mint means one of the two is lying'
  );
  assertRejects(
    validInput({ position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(uap.buildTransferScript(MY_PUBKEY, MULTIPLIER)) }) }),
    'position',
    'and the same the other way round'
  );
});

test('a good script_hex is still accepted, and is what gets signed', async () => {
  const script = uap.buildTransferScript(MY_PUBKEY, MULTIPLIER);
  const plan = planOrThrow(withScript(script));
  const { tx } = await buildOrThrow(plan);
  const expected = uap.signSpend(secp256k1, script, MY_PRIVKEY, tx, 0, uap.SIGHASH_ALL);
  assertEqual(uap.bytesToHex(tx.vin[0].scriptSig), uap.bytesToHex(expected));
});

test('buildTransferTx re-validates script_hex and refuses to sign a tampered plan', async () => {
  // planTransfer's check is not enough on its own: the plan holds a
  // reference to the caller's position object, so anything that can mutate
  // it between plan and build gets its script signed. The signer must
  // check for itself.
  const plan = planOrThrow(validInput());
  plan.position = { ...plan.position, script_hex: uap.bytesToHex(uap.buildTransferScript(THEIR_PUBKEY, MULTIPLIER)) };
  await assertThrows(
    () => buildTransferTx({ secp: secp256k1, plan, privKey: MY_PRIVKEY }),
    'refusing to sign'
  );
});

test('buildTransferTx refuses a mint-form script swapped in under a transfer position', async () => {
  // The tampered script here keeps OUR pubkey and OUR multiplier, so every
  // check except the mint/transfer form one passes. What changes is the
  // FORM: `<pubkey> <mult> <salt> OP_MINT` instead of `<pubkey> <mult>
  // OP_MINT_TRANSFER`. The salt is unconstrained in length and content, so
  // skipping the form check at the point of use gets the user's key to sign
  // a preimage containing an attacker-chosen blob -- which is the whole
  // substitution this re-validation exists to stop. planTransfer catches it;
  // so must the signer, because the plan only holds a reference to the
  // caller's position object.
  const attackerBlob = new Uint8Array(64).fill(0x41);
  const plan = planOrThrow(validInput());
  plan.position = {
    ...plan.position,
    script_hex: uap.bytesToHex(uap.buildMintScript(MY_PUBKEY, MULTIPLIER, attackerBlob))
  };
  await assertThrows(
    () => buildTransferTx({ secp: secp256k1, plan, privKey: MY_PRIVKEY }),
    'refusing to sign'
  );
});

test('buildTransferTx refuses a transfer-form script swapped in under a mint position', async () => {
  // The mirror image, and not redundant: it is the branch that proves the
  // signer compares the form against THIS position rather than hard-coding
  // one form as acceptable.
  const salt = new Uint8Array(20).fill(0x33);
  const plan = planOrThrow(validInput({
    position: makePosition({ is_mint: true, script_hex: uap.bytesToHex(uap.buildMintScript(MY_PUBKEY, MULTIPLIER, salt)) })
  }));
  plan.position = {
    ...plan.position,
    script_hex: uap.bytesToHex(uap.buildTransferScript(MY_PUBKEY, MULTIPLIER))
  };
  await assertThrows(
    () => buildTransferTx({ secp: secp256k1, plan, privKey: MY_PRIVKEY }),
    'refusing to sign'
  );
});

// =============================================================================
// PUBKEYS MUST BE POINTS ON THE CURVE
// =============================================================================

// 0x02 followed by x = 5: a perfectly well-formed compressed pubkey prefix
// over an x with no square root on secp256k1. There is no private key for
// it, so a covenant paid here is unspendable forever.
const OFF_CURVE_COMPRESSED = '02' + '00'.repeat(31) + '05';
const OFF_CURVE_UNCOMPRESSED = '04' + '00'.repeat(31) + '05' + '11'.repeat(32);

test('a recipient pubkey that is not a point on the secp256k1 curve is rejected', () => {
  assertRejects(validInput({ toPubKey: OFF_CURVE_COMPRESSED }), 'toPubKey',
    'the length and prefix are right, but no key exists for this point: the tokens would be burned');
  assertRejects(validInput({ toPubKey: OFF_CURVE_UNCOMPRESSED }), 'toPubKey',
    'an uncompressed off-curve point is just as unspendable');
});

test('a position pubkey that is not a point on the curve is rejected as malformed', () => {
  // Specifically as MALFORMED, not merely as "addressed to someone else":
  // an off-curve key would also fail the ownership comparison, so asserting
  // only that the plan is rejected would pass without any curve check at all.
  const errors = assertRejects(validInput({ position: makePosition({ pubkey: OFF_CURVE_COMPRESSED }) }), 'position',
    'a position that claims an impossible owner is not one this wallet owns');
  assert(errors.some((e) => /malformed/i.test(e.message)),
    `the position pubkey must be rejected as malformed, got ${JSON.stringify(errors)}`);
});

test('real pubkeys, compressed and uncompressed, still pass the curve check', () => {
  planOrThrow(validInput({ toPubKey: THEIR_PUBKEY_HEX }));
  planOrThrow(validInput({ toPubKey: secp256k1.getPublicKey(THEIR_PRIVKEY, false) }));
});

// =============================================================================
// ABSENT OWNERSHIP EVIDENCE IS NOT OWNERSHIP
// =============================================================================

test('a position that does not say who it is addressed to is rejected', () => {
  const { pubkey, ...noPubKey } = makePosition();
  const errors = assertRejects(validInput({ position: noPubKey }), 'position',
    'with no pubkey there is nothing to check ours against, and the check must not be skipped');
  assert(errors.some((e) => /public key/i.test(e.message)),
    `error should say the pubkey is missing, got ${JSON.stringify(errors)}`);

  assertRejects(validInput({ position: makePosition({ pubkey: null }) }), 'position', 'null is not evidence');
  assertRejects(validInput({ position: makePosition({ pubkey: '' }) }), 'position', 'nor is an empty string');
});

test('a position pubkey given as bytes is accepted, like every other pubkey field', () => {
  const plan = planOrThrow(validInput({ position: makePosition({ pubkey: MY_PUBKEY }) }));
  assertEqual(uap.bytesToHex(plan.ownPubKey), MY_PUBKEY_HEX);
});

// =============================================================================
// THE POSITION'S VALUE IS TAKEN ON TRUST
// =============================================================================

test('a plan warns that the position value came from the indexer unverified', () => {
  const plan = planOrThrow(validInput());
  assert(Array.isArray(plan.warnings), 'a plan must carry a warnings array');
  const warning = plan.warnings.find((w) => w.code === 'position-value-unverified');
  assert(warning, `expected a value-unverified warning, got ${JSON.stringify(plan.warnings)}`);
  assert(/fee/i.test(warning.message),
    `the warning must say where an understated value goes, got ${JSON.stringify(warning)}`);
});

test('a position whose value the caller has verified itself raises no such warning', () => {
  const plan = planOrThrow(validInput({ position: makePosition({ value_verified: true }) }));
  assert(!plan.warnings.some((w) => w.code === 'position-value-unverified'),
    `a verified value needs no warning, got ${JSON.stringify(plan.warnings)}`);
});

// =============================================================================
// MULTIPLE ERRORS AT ONCE
// =============================================================================

test('every validation failure is reported at once, not just the first', () => {
  const result = planTransfer(validInput({
    toPubKey: 'nonsense',
    amountSats: 999 * COIN,
    fundingUtxos: []
  }));
  const fields = errorFields(result);
  for (const field of ['toPubKey', 'amountSats', 'fundingUtxos']) {
    assert(fields.includes(field), `expected an error on ${field}, got ${JSON.stringify(fields)}`);
  }
});

run({ style: 'compact' });
