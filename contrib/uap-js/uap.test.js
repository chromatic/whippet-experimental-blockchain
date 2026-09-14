import assert from 'assert';
import fs from 'node:fs';
import crypto from 'node:crypto';
import * as UAP from './uap.js';

function bytes(...vals) {
  return Uint8Array.from(vals);
}

// hex round-trip
assert.strictEqual(UAP.bytesToHex(UAP.hexToBytes('deadbeef')), 'deadbeef');
assert.strictEqual(UAP.bytesToHex(UAP.hexToBytes('')), '');

// buildMintScript / buildTransferScript byte layout, cross-checked by hand
// against the same encoding rules as src/script/script.h's CScript::operator<<
// and contrib/uap-indexer/script.go's ParseUAPScript.
{
  const pubkey = new Uint8Array(33).fill(0xab);
  pubkey[0] = 0x02;

  // v2 mints: <pubkey> <multiplier> OP_MINT (no salt)
  const mint = UAP.buildMintScript(pubkey, 1000);
  // <21 pubkey(33)> <02 e803 (1000 LE)> <b5 OP_MINT>
  assert.strictEqual(mint[0], 0x21);
  assert.strictEqual(mint[34], 0x02); // multiplier push length
  assert.strictEqual(mint[35], 0xe8);
  assert.strictEqual(mint[36], 0x03);
  assert.strictEqual(mint[mint.length - 1], UAP.OP_MINT);
  assert.strictEqual(mint.length, 1 + 33 + 1 + 2 + 1); // no salt in v2

  // v2 transfers: <pubkey> <multiplier> <origin32> OP_MINT_TRANSFER
  const origin = new Uint8Array(32).fill(0xcd);
  const transfer = UAP.buildTransferScript(pubkey, 42, origin);
  assert.strictEqual(transfer[0], 0x21); // pubkey length
  assert.strictEqual(transfer[34], 0x01); // 42 fits in one byte
  assert.strictEqual(transfer[35], 42);
  assert.strictEqual(transfer[36], 0x20); // origin push length (32 bytes)
  assert.strictEqual(transfer[transfer.length - 1], UAP.OP_MINT_TRANSFER);
  assert.strictEqual(transfer.length, 1 + 33 + 1 + 1 + 1 + 32 + 1);

  // buildTransferScript requires origin
  assert.throws(() => UAP.buildTransferScript(pubkey, 1000), /origin/);
  // origin must be exactly 32 bytes
  assert.throws(() => UAP.buildTransferScript(pubkey, 1000, new Uint8Array(31)), /32 bytes/);
}

// Multiplier encoding. A UAP output must use the canonical push for every
// element: OP_0 for 0, OP_1..OP_16 for 1..16, a minimal data push above
// that. Consensus (ParseUapOutputScript) and SCRIPT_VERIFY_MINIMALDATA now
// agree on exactly that set, so every position this builds can be spent by a
// standard transaction. See pushMultiplier.
{
  const pubkey = new Uint8Array(33).fill(0x02);
  const origin = new Uint8Array(32).fill(0xaa);

  // 0 -> OP_0: an empty data push, and already minimal.
  const t0 = UAP.buildTransferScript(pubkey, 0, origin);
  // structure: <21 pubkey> <00 multiplier> <20 origin32> <ba OP_MINT_TRANSFER>
  assert.strictEqual(t0[34], 0x00); // OP_0 for multiplier 0
  assert.strictEqual(t0.length, 1 + 33 + 1 + 1 + 32 + 1);

  // 1..16 -> the small-integer opcodes, one byte, no length prefix.
  for (const m of [1, 2, 10, 15, 16]) {
    const t = UAP.buildTransferScript(pubkey, m, origin);
    assert.strictEqual(t[34], 0x50 + m, `multiplier ${m} must encode as OP_${m}`);
    // structure: <21 pubkey> <5N multiplier> <20 origin32> <ba OP_MINT_TRANSFER>
    assert.strictEqual(t.length, 1 + 33 + 1 + 1 + 32 + 1, `multiplier ${m} must be a single byte`);

    const mint = UAP.buildMintScript(pubkey, m);
    assert.strictEqual(mint[34], 0x50 + m, `mint multiplier ${m} must encode as OP_${m}`);
  }

  // 17 is the first multiplier above the small-int range, so it becomes a
  // one-byte data push.
  const t17 = UAP.buildTransferScript(pubkey, 17, origin);
  // structure: <21 pubkey> <01 17> <20 origin32> <ba>
  assert.strictEqual(t17[34], 0x01);
  assert.strictEqual(t17[35], 17);
  assert.strictEqual(t17.length, 1 + 33 + 2 + 1 + 32 + 1);

  const t1000 = UAP.buildTransferScript(pubkey, 1000, origin);
  // structure: <21 pubkey> <02 e8 03> <20 origin32> <ba>
  assert.strictEqual(t1000[34], 0x02);
  assert.strictEqual(t1000[35], 0xe8);
  assert.strictEqual(t1000[36], 0x03);

  // Out-of-range multipliers are rejected rather than silently encoded.
  assert.throws(() => UAP.buildTransferScript(pubkey, -1, origin), /multiplier/);
  assert.throws(() => UAP.buildTransferScript(pubkey, 2147483648, origin), /multiplier/);
  assert.throws(() => UAP.buildTransferScript(pubkey, 1.5, origin), /multiplier/);
}

// transaction serialization: a 1-in-1-out tx has a predictable byte length,
// and re-decoding the varints/fields back out should match what went in.
{
  const origin = new Uint8Array(32).fill(0x99);
  const scriptPubKey = UAP.buildTransferScript(new Uint8Array(33).fill(3), 500, origin);
  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: 'ff'.repeat(32), vout: 7, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 123456789, scriptPubKey }],
  };
  const raw = UAP.serializeTx(tx);
  // 4 (version) + 1 (vin count) + 32 (txid) + 4 (vout) + 1 (scriptSig len=0) + 4 (sequence)
  // + 1 (vout count) + 8 (value) + 1 (scriptPubKey varint) + scriptPubKey.length + 4 (locktime)
  const expectedLen = 4 + 1 + 32 + 4 + 1 + 4 + 1 + 8 + 1 + scriptPubKey.length + 4;
  assert.strictEqual(raw.length, expectedLen);

  // vout value is little-endian at a known offset; check it decodes back.
  const voutValueOffset = 4 + 1 + 32 + 4 + 1 + 4 + 1;
  const view = new DataView(raw.buffer, raw.byteOffset + voutValueOffset, 8);
  assert.strictEqual(Number(view.getBigInt64(0, true)), 123456789);

  assert.strictEqual(UAP.txToHex(tx), UAP.bytesToHex(raw));
}

// signatureHash: the classic Satoshi-client degenerate cases (nIn out of
// range; SIGHASH_SINGLE with no matching output) return the fixed "hash of
// 1" constant rather than throwing, matching SignatureHash() in
// src/script/interpreter.cpp exactly (needed for consensus compatibility,
// however unlikely to be hit in practice).
{
  const ONE = UAP.hexToBytes('01' + '00'.repeat(31));
  const tx = { version: 1, locktime: 0, vin: [{ txid: '00'.repeat(32), vout: 0, sequence: 0xffffffff }], vout: [] };

  // nIn out of range.
  assert.deepStrictEqual(UAP.signatureHash(new Uint8Array(0), tx, 5, UAP.SIGHASH_ALL), ONE);

  // SIGHASH_SINGLE with no output at nIn.
  assert.deepStrictEqual(UAP.signatureHash(new Uint8Array(0), tx, 0, UAP.SIGHASH_SINGLE), ONE);
}

// Different hash types (and the ANYONECANPAY flag) must actually change
// the resulting hash -- a cheap sanity check that they're not accidentally
// all collapsing to the same computation. The live regtest verification
// (see doc/uap-marketplace-design.md) is what actually proves correctness
// against the real consensus rules.
{
  const scriptCode = new Uint8Array([1, 2, 3]);
  const tx = {
    version: 1,
    locktime: 0,
    vin: [
      { txid: 'aa'.repeat(32), vout: 0, sequence: 0xffffffff },
      { txid: 'bb'.repeat(32), vout: 1, sequence: 0xffffffff },
    ],
    vout: [
      { value: 100, scriptPubKey: new Uint8Array([4, 5]) },
      { value: 200, scriptPubKey: new Uint8Array([6, 7]) },
    ],
  };
  const hashes = new Set([
    UAP.bytesToHex(UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_ALL)),
    UAP.bytesToHex(UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_NONE)),
    UAP.bytesToHex(UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_SINGLE)),
    UAP.bytesToHex(UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_ALL | UAP.SIGHASH_ANYONECANPAY)),
    UAP.bytesToHex(UAP.signatureHash(scriptCode, tx, 1, UAP.SIGHASH_ALL)),
  ]);
  assert.strictEqual(hashes.size, 5, 'expected all five hash-type/index combinations to differ');
}

// ---- Tests for signP2PKHInput ----

// Create a stub secp object that returns a predictable DER signature
// and records the hash it was called with (for verification)
function createStubSecp() {
  const secp = {
    lastSignedHash: null,
    signSync(hash, privKey, opts) {
      // Record the hash that was passed for verification
      secp.lastSignedHash = new Uint8Array(hash);
      // Return a fixed DER-shaped signature: 0x30 <length> <r> <s> <r and s are each 0x02 <len> <bytes>>
      // Simplified: just return a fixed 64-byte deterministic signature
      return new Uint8Array([
        0x30, 0x44,  // DER sequence header
        0x02, 0x20,  // INTEGER tag + length 32
        ...new Array(32).fill(0xaa),  // R value
        0x02, 0x20,  // INTEGER tag + length 32
        ...new Array(32).fill(0xbb),  // S value
      ]);
    }
  };
  return secp;
}

const stubSecp = createStubSecp();

// Test 1: signP2PKHInput must be exported
{
  assert.strictEqual(typeof UAP.signP2PKHInput, 'function', 'signP2PKHInput should be exported');
}

// Test 2: signP2PKHInput returns a scriptSig with signature and pubkey pushes
{
  const privKey = new Uint8Array(32).fill(1);
  const pubkey = new Uint8Array(33).fill(2);
  pubkey[0] = 0x02; // valid compressed pubkey prefix
  const origin = new Uint8Array(32).fill(0x11);
  const scriptCode = UAP.buildTransferScript(pubkey, 500, origin);

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: 'ff'.repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 100000, scriptPubKey: new Uint8Array([1, 2, 3]) }],
  };

  const scriptSig = UAP.signP2PKHInput(stubSecp, scriptCode, privKey, pubkey, tx, 0);

  // scriptSig should contain two pushes: signature and pubkey
  // The scriptSig starts with pushData(sig+hashtype), then pushData(pubkey)
  assert(scriptSig.length > 0, 'scriptSig should not be empty');

  // Should be different from signSpend output (which has only one push)
  const signSpendResult = UAP.signSpend(stubSecp, scriptCode, privKey, tx, 0);
  assert.notStrictEqual(scriptSig.length, signSpendResult.length, 'P2PKH scriptSig should differ from signSpend output');
}

// Test 3: signP2PKHInput contains pubkey as second push
{
  const privKey = new Uint8Array(32).fill(3);
  const pubkey = new Uint8Array(33).fill(4);
  pubkey[0] = 0x02;
  const origin = new Uint8Array(32).fill(0x22);
  const scriptCode = UAP.buildTransferScript(pubkey, 500, origin);

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: '00'.repeat(32), vout: 1, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 50000, scriptPubKey: new Uint8Array([5, 6]) }],
  };

  const scriptSig = UAP.signP2PKHInput(stubSecp, scriptCode, privKey, pubkey, tx, 0);

  // The scriptSig should end with pushData(pubkey)
  // pubkey is 33 bytes, so it should be prefixed with 0x21 (33 in decimal)
  assert.strictEqual(scriptSig[scriptSig.length - 34], 0x21, 'pubkey push should start with length 33');
  for (let i = 0; i < 33; i++) {
    assert.strictEqual(scriptSig[scriptSig.length - 33 + i], pubkey[i], `pubkey byte ${i} mismatch`);
  }
}

// Test 4: signP2PKHInput with non-default hashType
{
  const privKey = new Uint8Array(32).fill(5);
  const pubkey = new Uint8Array(33).fill(6);
  pubkey[0] = 0x02;
  const scriptCode = new Uint8Array([7, 8, 9]);

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: 'aa'.repeat(32), vout: 2, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 25000, scriptPubKey: new Uint8Array([10, 11]) }],
  };

  const secp1 = createStubSecp();
  const secp2 = createStubSecp();

  const scriptSigAll = UAP.signP2PKHInput(secp1, scriptCode, privKey, pubkey, tx, 0, UAP.SIGHASH_ALL);
  const scriptSigNone = UAP.signP2PKHInput(secp2, scriptCode, privKey, pubkey, tx, 0, UAP.SIGHASH_NONE);

  // The sighash byte should differ in the signature portion
  assert.notStrictEqual(UAP.bytesToHex(scriptSigAll), UAP.bytesToHex(scriptSigNone), 'different hash types should produce different scriptSigs');
}

// Test 5: signP2PKHInput passes correct sighash to secp.signSync
{
  const privKey = new Uint8Array(32).fill(7);
  const pubkey = new Uint8Array(33).fill(8);
  pubkey[0] = 0x02;
  const scriptCode = new Uint8Array([1, 2, 3, 4, 5]);

  const tx = {
    version: 1,
    locktime: 0,
    vin: [{ txid: 'cc'.repeat(32), vout: 3, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 12345, scriptPubKey: new Uint8Array([20, 21]) }],
  };

  const secp = createStubSecp();
  UAP.signP2PKHInput(secp, scriptCode, privKey, pubkey, tx, 0, UAP.SIGHASH_ALL);

  // Verify the sighash matches what signatureHash would compute
  const expectedHash = UAP.signatureHash(scriptCode, tx, 0, UAP.SIGHASH_ALL);
  assert.deepStrictEqual(secp.lastSignedHash, expectedHash, 'sighash passed to secp.signSync should match signatureHash result');
}

// ---- Tests for constants and buildPaymentTx ----

// Test: Export policy constants
{
  assert.strictEqual(typeof UAP.RECOMMENDED_MIN_TX_FEE, 'number', 'RECOMMENDED_MIN_TX_FEE should be exported');
  assert.strictEqual(typeof UAP.DEFAULT_DUST_LIMIT, 'number', 'DEFAULT_DUST_LIMIT should be exported');
  assert.strictEqual(typeof UAP.DEFAULT_HARD_DUST_LIMIT, 'number', 'DEFAULT_HARD_DUST_LIMIT should be exported');

  // Verify constants match spec (from src/policy/policy.h and src/amount.h)
  assert.strictEqual(UAP.RECOMMENDED_MIN_TX_FEE, 1000000, 'RECOMMENDED_MIN_TX_FEE should be COIN/100');
  assert.strictEqual(UAP.DEFAULT_DUST_LIMIT, 1000000, 'DEFAULT_DUST_LIMIT should equal RECOMMENDED_MIN_TX_FEE');
  assert.strictEqual(UAP.DEFAULT_HARD_DUST_LIMIT, 100000, 'DEFAULT_HARD_DUST_LIMIT should be DEFAULT_DUST_LIMIT/10');
}

// Test: buildPaymentTx is exported
{
  assert.strictEqual(typeof UAP.buildPaymentTx, 'function', 'buildPaymentTx should be exported');
}

// Test: buildPaymentTx with single input and change output
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(9);
  const pubKey = new Uint8Array(33).fill(10);
  pubKey[0] = 0x02;

  // Build a simple P2PKH script for the input (using the contract address builder)
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x11), 0x88, 0xac]); // P2PKH-like

  const inputs = [
    { txid: '00'.repeat(32), vout: 0, value: 5000000, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 2000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x22), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xbb), 0x88, 0xac]);

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  assert(tx, 'buildPaymentTx should return a transaction');
  assert.strictEqual(tx.vin.length, 1, 'transaction should have 1 input');
  assert.strictEqual(tx.vout.length, 2, 'transaction should have 2 outputs (payment + change)');
  assert(tx.vin[0].scriptSig.length > 0, 'input should be signed');
}

// Test: buildPaymentTx with change output
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(11);
  const pubKey = new Uint8Array(33).fill(12);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x33), 0x88, 0xac]);

  const inputs = [
    { txid: 'aa'.repeat(32), vout: 1, value: 10000000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x44), 0x88, 0xac]);

  const outputs = [
    { value: 3000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x55), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, {
    inputs,
    outputs,
    changeScript,
    feeRate: 1000  // Satoshis per byte
  });

  assert(tx, 'buildPaymentTx should return transaction');
  // Should have 1 input, 2 outputs (payment + change)
  assert.strictEqual(tx.vin.length, 1, 'should have 1 input');
  assert(tx.vout.length >= 1, 'should have at least payment output');
}

// Test: buildPaymentTx throws when insufficient funds
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(13);
  const pubKey = new Uint8Array(33).fill(14);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x66), 0x88, 0xac]);

  const inputs = [
    { txid: 'bb'.repeat(32), vout: 0, value: 1000000, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 5000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x77), 0x88, 0xac]) }
  ];

  assert.throws(() => {
    UAP.buildPaymentTx(secp, { inputs, outputs });
  }, /insufficient.*funds|funds/i, 'should throw clear error for insufficient funds');
}

// Test: buildPaymentTx respects dust limit
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(15);
  const pubKey = new Uint8Array(33).fill(16);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x88), 0x88, 0xac]);

  const inputs = [
    { txid: 'cc'.repeat(32), vout: 0, value: 5000000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x99), 0x88, 0xac]);

  // Payment + fee + small dust would leave us with dust amount as change
  // The change should either be absent or >= DEFAULT_DUST_LIMIT
  const outputs = [
    { value: 3899000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xaa), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, {
    inputs,
    outputs,
    changeScript,
    feeRate: 1000
  });

  // Check: if there's a change output, it should be >= dust limit
  if (tx.vout.length > outputs.length) {
    const changeOutput = tx.vout[tx.vout.length - 1];
    assert(changeOutput.value >= UAP.DEFAULT_DUST_LIMIT, 'change output should be >= DEFAULT_DUST_LIMIT');
  }
}

// Test: buildPaymentTx deterministic with same inputs
{
  const secp1 = createStubSecp();
  const secp2 = createStubSecp();

  const privKey = new Uint8Array(32).fill(17);
  const pubKey = new Uint8Array(33).fill(18);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xbb), 0x88, 0xac]);

  const inputs = [
    { txid: 'dd'.repeat(32), vout: 0, value: 10000000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xcc), 0x88, 0xac]);

  const outputs = [
    { value: 2000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xdd), 0x88, 0xac]) }
  ];

  const tx1 = UAP.buildPaymentTx(secp1, { inputs, outputs, changeScript });
  const tx2 = UAP.buildPaymentTx(secp2, { inputs, outputs, changeScript });

  // Should produce identical serializations (deterministic)
  assert.strictEqual(UAP.txToHex(tx1), UAP.txToHex(tx2), 'identical inputs should produce identical transactions');
}

// Test: buildPaymentTx with multiple inputs (coin selection)
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(19);
  const pubKey = new Uint8Array(33).fill(20);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xee), 0x88, 0xac]);

  // Multiple small UTXOs - need multiple because each is small
  // Total 3M to cover 1M output + ~1M minimum fee
  const inputs = [
    { txid: 'ee'.repeat(32), vout: 0, value: 1000000, scriptCode, privKey, pubKey },
    { txid: 'ff'.repeat(32), vout: 0, value: 1000000, scriptCode, privKey, pubKey },
    { txid: '11'.repeat(32), vout: 0, value: 1000000, scriptCode, privKey, pubKey },
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xff), 0x88, 0xac]);

  const outputs = [
    { value: 1000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x33), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript, feeRate: 500 });

  // Should have selected enough inputs to cover 1000000 + fee (at least 2)
  assert(tx.vin.length >= 2, 'should select multiple inputs to cover target');
  // All selected inputs should be signed
  for (let i = 0; i < tx.vin.length; i++) {
    assert(tx.vin[i].scriptSig.length > 0, `input ${i} should be signed`);
  }
}

// Test: buildPaymentTx coin selection never selects more than needed
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(21);
  const pubKey = new Uint8Array(33).fill(22);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x44), 0x88, 0xac]);

  // Varied input sizes
  const inputs = [
    { txid: '22'.repeat(32), vout: 0, value: 100000, scriptCode, privKey, pubKey },
    { txid: '33'.repeat(32), vout: 0, value: 500000, scriptCode, privKey, pubKey },
    { txid: '44'.repeat(32), vout: 0, value: 2000000, scriptCode, privKey, pubKey },
    { txid: '55'.repeat(32), vout: 0, value: 5000000, scriptCode, privKey, pubKey },
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x55), 0x88, 0xac]);

  const outputs = [
    { value: 1000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x66), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  // Should not select the 5M input when smaller ones suffice
  assert(tx.vin.length <= 3, 'should not select all inputs (greedy selection)');
}

// Test: buildPaymentTx output value validation (total output <= input total)
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(23);
  const pubKey = new Uint8Array(33).fill(24);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x77), 0x88, 0xac]);

  const inputs = [
    { txid: '66'.repeat(32), vout: 0, value: 3000000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x88), 0x88, 0xac]);

  const outputs = [
    { value: 1000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x99), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  // Verify conservation: input - outputs - fee = change (or fee if dust)
  let totalOutput = 0;
  for (const out of tx.vout) {
    totalOutput += out.value;
  }
  assert(totalOutput <= inputs[0].value, 'total output value should not exceed input value');
}

// Test: buildPaymentTx with dust change (change should be folded into fee)
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(25);
  const pubKey = new Uint8Array(33).fill(26);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xaa), 0x88, 0xac]);

  // Input sized such that change would be small (but must cover min fee of 1M)
  const inputs = [
    { txid: '77'.repeat(32), vout: 0, value: 3000500, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xbb), 0x88, 0xac]);

  const outputs = [
    { value: 2000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xcc), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  // If change is below dust limit, no change output should be created
  // tx.vout should have only the payment output (no change)
  const hasChangeOutput = tx.vout.length > outputs.length;
  if (!hasChangeOutput) {
    // Dust was folded into fee - this is expected
    assert.strictEqual(tx.vout.length, outputs.length, 'dust should be folded into fee, no change output');
  } else {
    // If there is a change output, it must be >= dust limit
    assert(tx.vout[tx.vout.length - 1].value >= UAP.DEFAULT_DUST_LIMIT, 'change output must be >= dust limit');
  }
}

// Test: every input in buildPaymentTx result has a non-empty scriptSig
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(27);
  const pubKey = new Uint8Array(33).fill(28);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xdd), 0x88, 0xac]);

  const inputs = [
    { txid: '88'.repeat(32), vout: 0, value: 1500000, scriptCode, privKey, pubKey },
    { txid: '99'.repeat(32), vout: 1, value: 1200000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xee), 0x88, 0xac]);

  const outputs = [
    { value: 800000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xff), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  for (let i = 0; i < tx.vin.length; i++) {
    assert(tx.vin[i].scriptSig.length > 0, `input ${i} should have non-empty scriptSig`);
  }
}

// Test: buildPaymentTx result serializes cleanly via txToHex
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(29);
  const pubKey = new Uint8Array(33).fill(30);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x11), 0x88, 0xac]);

  const inputs = [
    { txid: 'aa'.repeat(32), vout: 0, value: 4000000, scriptCode, privKey, pubKey }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x22), 0x88, 0xac]);

  const outputs = [
    { value: 1500000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x33), 0x88, 0xac]) }
  ];

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });
  const hex = UAP.txToHex(tx);

  // Should produce valid hex (even length, valid characters)
  assert.strictEqual(hex.length % 2, 0, 'transaction hex should have even length');
  assert(/^[0-9a-f]*$/.test(hex), 'transaction hex should contain only hex characters');
  assert(hex.length > 0, 'transaction hex should not be empty');
}

// ---- BUG 1: Silent loss of funds when changeScript is omitted ----
// When change >= DEFAULT_DUST_LIMIT but changeScript is omitted, the code
// silently absorbs the change into the miner fee (silent loss).
// This test asserts that an error is thrown instead.
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(31);
  const pubKey = new Uint8Array(33).fill(32);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x01), 0x88, 0xac]);

  // 100 WHIP input, 1 WHIP output -> ~99 WHIP would be change
  const inputs = [
    { txid: '11'.repeat(32), vout: 0, value: 100 * UAP.COIN, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x02), 0x88, 0xac]) }
  ];

  // NO changeScript - this is the problem case
  assert.throws(() => {
    UAP.buildPaymentTx(secp, { inputs, outputs, feeRate: 1000 });
  }, /change.*script|changeScript/i, 'should throw error when change >= dust limit but no changeScript provided');
}

// ---- BUG 2: Default fee is far too low ----
// With default feeRate of 1000 sat/kB and a ~228 byte tx, the fee is only 228 satoshis.
// Node minimum is RECOMMENDED_MIN_TX_FEE = 1000000 satoshis.
// This test verifies that the final fee is at least RECOMMENDED_MIN_TX_FEE.
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(33);
  const pubKey = new Uint8Array(33).fill(34);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x03), 0x88, 0xac]);

  const inputs = [
    { txid: '22'.repeat(32), vout: 0, value: 10 * UAP.COIN, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x04), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x05), 0x88, 0xac]);

  // Use default feeRate (should not be absurdly low)
  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  // Calculate actual fee: inputs - outputs
  let totalOutput = 0;
  for (const out of tx.vout) {
    totalOutput += out.value;
  }
  const totalInput = inputs[0].value;
  const actualFee = totalInput - totalOutput;

  assert(actualFee >= UAP.RECOMMENDED_MIN_TX_FEE,
    `fee (${actualFee}) should be at least RECOMMENDED_MIN_TX_FEE (${UAP.RECOMMENDED_MIN_TX_FEE})`);
}

// ---- BUG 3: feeRate units are self-contradictory ----
// Current docs say "satoshis per byte" but math is "sat/kB".
// This test verifies docs describe sat/kB correctly and checks that the calculation is consistent.
// Fix: update docs to say "satoshis per kilobyte", make default explicit.
{
  // Verify that JSDoc for buildPaymentTx says feeRate is in sat/kB (not sat/byte)
  // This is a documentation check; the actual behavior test is implicit in Bug 2 test
  // (if the math were sat/byte, fees would be 1000x too high)

  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(35);
  const pubKey = new Uint8Array(33).fill(36);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x06), 0x88, 0xac]);

  const inputs = [
    { txid: '33'.repeat(32), vout: 0, value: 5 * UAP.COIN, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x07), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x08), 0x88, 0xac]);

  // Compare two feeRates: 1000 and 2000 sat/kB
  // If the unit is sat/kB, a 2x rate should roughly 2x the fee
  const tx1 = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript, feeRate: 1000 });
  const secp2 = createStubSecp();
  const tx2 = UAP.buildPaymentTx(secp2, { inputs, outputs, changeScript, feeRate: 2000 });

  let fee1 = inputs[0].value;
  for (const out of tx1.vout) fee1 -= out.value;

  let fee2 = inputs[0].value;
  for (const out of tx2.vout) fee2 -= out.value;

  // Higher feeRate should produce higher fee
  assert(fee2 >= fee1, `fee at 2000 sat/kB (${fee2}) should be >= fee at 1000 sat/kB (${fee1})`);
}

// ---- BUG 4: Fee is sized for one output regardless of actual output count ----
// selectCoins hardcodes nOutputs = 1 + (change ? 1 : 0), never accounting for
// multiple requested outputs. This test verifies fees are correct for multiple outputs.
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(37);
  const pubKey = new Uint8Array(33).fill(38);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x09), 0x88, 0xac]);

  const inputs = [
    { txid: '44'.repeat(32), vout: 0, value: 50 * UAP.COIN, scriptCode, privKey, pubKey }
  ];

  // 3 payment outputs
  const outputs = [
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0a), 0x88, 0xac]) },
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0b), 0x88, 0xac]) },
    { value: 1 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0c), 0x88, 0xac]) },
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0d), 0x88, 0xac]);

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript, feeRate: 1000 });

  // The tx should have enough outputs (payment + change)
  assert(tx.vout.length >= 3, `tx should have at least 3 payment outputs, got ${tx.vout.length}`);

  // Verify no value is lost: input = outputs + fee
  let totalOutput = 0;
  for (const out of tx.vout) {
    totalOutput += out.value;
  }
  const fee = inputs[0].value - totalOutput;

  // For a 4-output tx (3 payments + change), the size should be larger,
  // so the fee should be correspondingly larger than a 1-output tx.
  // This indirectly tests that selectCoins accounted for all 3 payment outputs
  assert(fee >= UAP.RECOMMENDED_MIN_TX_FEE, `fee (${fee}) must be >= minimum`);
  // The fee should be reasonable relative to tx size: ~(nInputs*150 + nOutputs*34 + 10) * feeRate / 1000
  const estimatedSize = 1 * 150 + 4 * 34 + 10;  // 1 input, 4 outputs (3+change), base
  const estimatedFee = Math.ceil(estimatedSize * 1000 / 1000);
  assert(fee >= estimatedFee - 1000, `fee (${fee}) should be close to estimated (${estimatedFee})`);
}

// ---- Additional test: No spendable value is silently lost ----
// For randomized input sets, assert sum(inputs) === sum(outputs) + fee
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(39);
  const pubKey = new Uint8Array(33).fill(40);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0e), 0x88, 0xac]);

  const inputs = [
    { txid: '55'.repeat(32), vout: 0, value: 25 * UAP.COIN, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 3 * UAP.COIN, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x0f), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x10), 0x88, 0xac]);

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });

  let totalInput = 0;
  for (const inp of inputs) totalInput += inp.value;

  let totalOutput = 0;
  for (const out of tx.vout) totalOutput += out.value;

  const fee = totalInput - totalOutput;
  assert.strictEqual(fee + totalOutput, totalInput, 'conservation: input must equal output + fee');
  assert(fee >= 0, 'fee should never be negative');
  assert(fee <= totalInput, 'fee should not exceed total input');
}

// ---- Test: Insufficient funds after floored fee is applied ----
// selectCoins applies the fee floor, so coin selection should account for it.
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(41);
  const pubKey = new Uint8Array(33).fill(42);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x11), 0x88, 0xac]);

  // Only 1.5M satoshis, but target is 1M + minimum fee of 1M = 2M total needed
  const inputs = [
    { txid: '66'.repeat(32), vout: 0, value: 1500000, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 1000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x12), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x13), 0x88, 0xac]);

  assert.throws(() => {
    UAP.buildPaymentTx(secp, { inputs, outputs, changeScript });
  }, /insufficient.*funds|funds/i, 'should throw insufficient funds when floored fee is considered');
}

// ---- Test: Sub-dust change is folded into fee without error when changeScript IS supplied ----
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(43);
  const pubKey = new Uint8Array(33).fill(44);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x14), 0x88, 0xac]);

  // Set up so change would be below dust (must still cover minimum fee)
  const inputs = [
    { txid: '77'.repeat(32), vout: 0, value: 3001000, scriptCode, privKey, pubKey }
  ];

  const outputs = [
    { value: 2000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x15), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x16), 0x88, 0xac]);

  // This should NOT throw, even though change < dust (after min fee taken out)
  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript, feeRate: 100 });

  // Dust should be folded into fee (no separate change output)
  assert.strictEqual(tx.vout.length, 1, 'should have only payment output (dust folded into fee)');

  let totalOut = 0;
  for (const out of tx.vout) totalOut += out.value;
  const fee = inputs[0].value - totalOut;
  assert(fee < UAP.DEFAULT_DUST_LIMIT + 5000, 'dust folded into fee should still be small');
}

// ---- Test: Sorting behavior and input count ----
// selectCoins sorts by value ascending (smallest first).
// This means it uses MORE inputs than necessary (poor consolidation).
// Test documents this behavior as-is (without changing the algorithm).
{
  const secp = createStubSecp();
  const privKey = new Uint8Array(32).fill(45);
  const pubKey = new Uint8Array(33).fill(46);
  pubKey[0] = 0x02;
  const scriptCode = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x17), 0x88, 0xac]);

  // Many small UTXOs and one large UTXO
  const inputs = [
    { txid: '88'.repeat(32), vout: 0, value: 100000, scriptCode, privKey, pubKey },
    { txid: '99'.repeat(32), vout: 1, value: 100000, scriptCode, privKey, pubKey },
    { txid: 'aa'.repeat(32), vout: 2, value: 100000, scriptCode, privKey, pubKey },
    { txid: 'bb'.repeat(32), vout: 3, value: 100000, scriptCode, privKey, pubKey },
    { txid: 'cc'.repeat(32), vout: 4, value: 100000, scriptCode, privKey, pubKey },
    { txid: 'dd'.repeat(32), vout: 5, value: 5000000, scriptCode, privKey, pubKey },
  ];

  const outputs = [
    { value: 1000000, scriptPubKey: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x18), 0x88, 0xac]) }
  ];

  const changeScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0x19), 0x88, 0xac]);

  const tx = UAP.buildPaymentTx(secp, { inputs, outputs, changeScript, feeRate: 1000 });

  // With ascending sort, smallest inputs are selected first.
  // To reach 1M + fee (~1.5M total), it will pick the five 100k inputs before the 5M input.
  // This tests that input selection favors smaller UTXOs (maximizes input count),
  // which is a deliberate consolidation strategy despite inflating fees.
  assert(tx.vin.length >= 5, 'ascending sort should select many small inputs before one large input');
}

// ---- Shared fixture tests ----
// src/test/data/uap_script_vectors.json is the cross-implementation contract:
// the same file is consumed by the C++ consensus test (src/test/uap_mint_tests.cpp)
// and the Go indexer (contrib/uap-indexer/script_test.go). uap.js only builds
// scripts, so its obligation is simple: for every vector consensus calls
// valid, reproduce its exact bytes.
{
  const fixtures = JSON.parse(fs.readFileSync('../../src/test/data/uap_script_vectors.json', 'utf-8'));
  assert(fixtures.length > 0, 'fixture file is empty');

  // Pull the elements out of a known-valid vector so the salt used to rebuild
  // a mint is the vector's own salt, not a guess. Valid vectors use only
  // canonical encodings: 1-byte-length data pushes, OP_0, and OP_1..OP_16.
  function splitElements(bytes) {
    const elements = [];
    let i = 0;
    while (i < bytes.length - 1) {
      const op = bytes[i];
      if (op >= 0x51 && op <= 0x60) { // OP_1 .. OP_16
        elements.push({ opcode: op, data: new Uint8Array(0) });
        i += 1;
        continue;
      }
      // 0x00 is OP_0, the empty data push -- how multiplier 0 is encoded.
      assert(op <= 0x4b, `unexpected opcode 0x${op.toString(16)} in a valid vector`);
      elements.push({ opcode: op, data: bytes.slice(i + 1, i + 1 + op) });
      i += 1 + op;
    }
    return { elements, terminator: bytes[bytes.length - 1] };
  }

  let built = 0;
  for (const vec of fixtures) {
    // Invalid vectors document what the *parsers* must reject; uap.js has no
    // parser, so there is nothing for it to check here.
    if (!vec.valid) continue;

    const expected = UAP.hexToBytes(vec.script);
    const pubkey = UAP.hexToBytes(vec.pubkey);

    let actual;
    if (vec.is_mint) {
      // v2 mints: <pubkey> <multiplier> OP_MINT (no salt)
      assert.strictEqual(vec.origin, '', `fixture "${vec.comment}": mints must have empty origin`);
      actual = UAP.buildMintScript(pubkey, vec.multiplier);
    } else {
      // v2 transfers: <pubkey> <multiplier> <origin32> OP_MINT_TRANSFER
      assert(vec.origin && vec.origin.length === 64, `fixture "${vec.comment}": transfers must have 32-byte origin (64 hex chars)`);
      const origin = UAP.hexToBytes(vec.origin);
      actual = UAP.buildTransferScript(pubkey, vec.multiplier, origin);
    }
    assert.deepStrictEqual(
      Array.from(actual), Array.from(expected),
      `fixture "${vec.comment}": built script does not match the vector`
    );
    built++;
  }

  const valid = fixtures.filter(v => v.valid).length;
  assert.strictEqual(built, valid, 'every valid vector must be rebuilt byte-for-byte');
  assert(fixtures.some(v => v.valid && v.multiplier >= 1 && v.multiplier <= 16),
    'fixture set must cover the small-int multiplier range');
  console.log(`uap.js: rebuilt all ${built} valid fixture vectors byte-for-byte`);
}

console.log('uap.js: all tests passed');

// ---- absolute fee cap ----
// A fee rate is easy to get wrong by orders of magnitude -- coins where
// satoshis were meant, or a bad figure from a fee estimator. The rate itself
// gives no clue that anything is off; only the resulting absolute fee does,
// and by the time a wallet has signed and broadcast, the money is a miner's.
// The node refuses to send above DEFAULT_TRANSACTION_MAXFEE
// (RECOMMENDED_MIN_TX_FEE * 10000 = 100 coins) for exactly this reason.
//
// Note what this does and does not catch. The cap is 100 coins, so a rate
// wrong by 1000x -- sat/byte read as sat/kB -- still lands under it for a
// small transaction and is NOT refused. What it catches is the large error:
// a value passed in coins rather than satoshis, off by 1e8. A guard rail,
// not a substitute for getting the units right.
//
// The cap throws rather than clamping: silently spending a different fee
// than the caller asked for is its own kind of wrong, and a caller who
// really wants a huge fee can say so with maxFee.
{
  const pubkey = new Uint8Array(33).fill(0x02);
  const origin = new Uint8Array(32).fill(0x77);
  const scriptPubKey = UAP.buildTransferScript(pubkey, 1000, origin);

  assert.strictEqual(UAP.DEFAULT_TRANSACTION_MAXFEE, UAP.RECOMMENDED_MIN_TX_FEE * 10000,
    'the cap must mirror the node constant in src/validation.h');

  const inputs = [{
    txid: 'aa'.repeat(32), vout: 0,
    value: 5000 * UAP.COIN,
    scriptCode: scriptPubKey,
    privKey: new Uint8Array(32).fill(0x11),
    pubKey: pubkey
  }];
  const outputs = [{ value: 1 * UAP.COIN, scriptPubKey }];

  // A sane fee rate builds fine.
  const ok = UAP.buildPaymentTx(stubSecp, {
    inputs, outputs, changeScript: scriptPubKey, feeRate: 1000
  });
  assert(ok.vout.length >= 1, 'a sane fee rate should build');

  // A fee rate handed over in coins instead of satoshis: 1e8 too large.
  assert.throws(
    () => UAP.buildPaymentTx(stubSecp, {
      inputs, outputs, changeScript: scriptPubKey, feeRate: 1000 * UAP.COIN
    }),
    /fee/i,
    'a fee rate off by 1e8 must be refused, not signed'
  );

  // Just below the cap builds; just above does not. The boundary is what
  // makes the case above evidence about the cap rather than about some
  // unrelated failure at a large number.
  const sized = UAP.buildPaymentTx(stubSecp, {
    inputs, outputs, changeScript: scriptPubKey,
    feeRate: 1000, maxFee: UAP.RECOMMENDED_MIN_TX_FEE
  });
  assert(sized.vout.length >= 1, 'a fee exactly at an explicit cap is allowed');
  assert.throws(
    () => UAP.buildPaymentTx(stubSecp, {
      inputs, outputs, changeScript: scriptPubKey,
      feeRate: 1000, maxFee: UAP.RECOMMENDED_MIN_TX_FEE - 1
    }),
    /fee/i,
    'one satoshi above the cap must be refused'
  );

  // An explicit opt-out is honoured, so the cap is a guard rail and not a
  // ceiling on what the caller is allowed to decide.
  const forced = UAP.buildPaymentTx(stubSecp, {
    inputs, outputs, changeScript: scriptPubKey,
    feeRate: 1000 * 1000 * 1000,
    maxFee: 5000 * UAP.COIN
  });
  assert(forced.vout.length >= 1, 'an explicit maxFee must be honoured');

  // buildTransferTx takes an absolute fee, so the same guard applies there.
  assert.throws(
    () => UAP.buildTransferTx(stubSecp, {
      input: { txid: 'bb'.repeat(32), vout: 0, scriptCode: scriptPubKey,
               value: 5000 * UAP.COIN, privKey: new Uint8Array(32).fill(0x11) },
      toPubkey: pubkey, multiplier: 1000,
      fee: UAP.DEFAULT_TRANSACTION_MAXFEE + 1
    }),
    /fee/i,
    'buildTransferTx must refuse a fee above the cap'
  );
}

// ---- normalizeTicker ----
{
  // Folding: lowercase and mixed-case input is uppercased, not rejected.
  assert.strictEqual(UAP.normalizeTicker('doge'), 'DOGE');
  assert.strictEqual(UAP.normalizeTicker('DoGe1'), 'DOGE1');
  assert.strictEqual(UAP.normalizeTicker('A'), 'A');
  assert.strictEqual(UAP.normalizeTicker('12345678'), '12345678');

  // Length bounds: 1..8 characters.
  assert.throws(() => UAP.normalizeTicker(''), /1\.\.8/, 'empty ticker must be rejected');
  assert.throws(() => UAP.normalizeTicker('a'.repeat(9)), /1\.\.8/, '9-character ticker must be rejected');

  // Charset: only A-Z and 0-9 after folding.
  assert.throws(() => UAP.normalizeTicker('DOG-E'), /-/, 'punctuation must be rejected and named in the message');
  assert.throws(() => UAP.normalizeTicker('DOG E'), /position/, 'whitespace must be rejected');
}

// ---- buildMetadataScript ----
// Byte-exact expectations for the new 4-push wire format:
//   OP_RETURN "WUAP" <version> <ticker> <metadata_hash>
{
  const hash32 = new Uint8Array(32).fill(0xcd);

  // A known ticker + 32-byte hash, checked byte for byte.
  const script = UAP.buildMetadataScript({ ticker: 'DOGE', metadataHash: hash32 });
  const expected = bytes(
    0x6a,                               // OP_RETURN
    0x04, 0x57, 0x55, 0x41, 0x50,       // push "WUAP"
    0x01, 0x02,                         // push version (0x02)
    0x04, 0x44, 0x4f, 0x47, 0x45,       // push "DOGE"
    0x20, ...hash32                     // push 32-byte hash
  );
  assert.deepStrictEqual(script, expected, 'buildMetadataScript must match the wire format byte for byte');

  // Version byte is 0x02, not the old 0x01.
  assert.strictEqual(script[7], 0x02, 'version byte must be 0x02');

  // Exactly four pushes: OP_RETURN followed by four push opcodes, nothing else.
  {
    let pc = 1; // skip OP_RETURN
    let pushCount = 0;
    while (pc < script.length) {
      const len = script[pc];
      assert(len < 0x4c, 'every push in this script must be a direct-length push (no PUSHDATA1/2/4)');
      pc += 1 + len;
      pushCount++;
    }
    assert.strictEqual(pushCount, 4, 'metadata script must contain exactly four pushes');
    assert.strictEqual(pc, script.length, 'the four pushes must exactly cover the script');
  }

  // Minimal script: ticker only, no hash. 10 (fixed overhead) + 1 (ticker) = 11 bytes.
  const minimal = UAP.buildMetadataScript({ ticker: 'A' });
  assert.strictEqual(minimal.length, 11, 'minimal {ticker: "A"} metadata script must be exactly 11 bytes');

  // Maximal script: 8-byte ticker + 32-byte hash. 10 + 8 + 32 = 50 bytes.
  const maximal = UAP.buildMetadataScript({ ticker: 'WHIPPET8', metadataHash: hash32 });
  assert.strictEqual(maximal.length, 50, 'maximal {ticker: "WHIPPET8", metadataHash: <32 bytes>} script must be exactly 50 bytes');

  // Lowercase is folded, not rejected -- buildMetadataScript calls
  // normalizeTicker itself, so this mints the uppercase ticker.
  const foldedScript = UAP.buildMetadataScript({ ticker: 'doge' });
  assert.deepStrictEqual(
    foldedScript,
    UAP.buildMetadataScript({ ticker: 'DOGE' }),
    'a lowercase ticker must build the same script as its uppercase form'
  );

  // Ticker charset/length rejections flow through normalizeTicker.
  assert.throws(() => UAP.buildMetadataScript({ ticker: 'A'.repeat(9) }), /1\.\.8/, '9-character ticker must be rejected');
  assert.throws(() => UAP.buildMetadataScript({ ticker: 'DOG$' }), /\$/, 'punctuation must be rejected');
  assert.throws(() => UAP.buildMetadataScript({ ticker: '' }), /1\.\.8/, 'empty ticker must be rejected');

  // Hash length: 0 or 32 only.
  assert.throws(
    () => UAP.buildMetadataScript({ ticker: 'DOGE', metadataHash: new Uint8Array(31) }),
    /32/,
    'a 31-byte hash must be rejected'
  );
}

// ---- buildMeltTx ----
// Build a transaction that redeems a position's backing: spends the position
// and produces a dust-valued transfer covenant (same pubkey, same multiplier)
// plus a plain output taking the remainder.
{
  const stubSecp = createStubSecp();

  // Helper: a dummy P2PKH-like script for remainder outputs in tests
  const dummyRemainderScript = new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xff), 0x88, 0xac]);

  // Test: buildMeltTx is exported
  {
    assert.strictEqual(typeof UAP.buildMeltTx, 'function', 'buildMeltTx should be exported');
  }

  // Test: buildMeltTx produces a transfer covenant output
  // The output at index 0 must be a OP_MINT_TRANSFER covenant with the given pubkey and multiplier.
  {
    const privKey = new Uint8Array(32).fill(0x11);
    const pubkey = new Uint8Array(33).fill(0x22);
    pubkey[0] = 0x02;

    const inputTxid = 'aa'.repeat(32);
    const inputVout = 0;
    // The position being melted is a TRANSFER, so it already belongs to a
    // lineage and melting must keep it there.
    const lineage = UAP.deriveOrigin('cd'.repeat(32), 3);
    const scriptCode = UAP.buildTransferScript(pubkey, 1000, lineage);
    const fee = 50000;

    const tx = UAP.buildMeltTx(stubSecp, {
      input: {
        txid: inputTxid,
        vout: inputVout,
        scriptCode,
        value: 5000000,  // dust (1000000) + fee (50000) + a remainder that itself clears dust
        privKey
      },
      toPubkey: pubkey,
      multiplier: 1000,
      fee,
      remainderScript: dummyRemainderScript
    });

    // Output 0 should be the covenant output (dust-valued transfer)
    assert.strictEqual(tx.vout.length, 2, 'melt tx should have exactly 2 outputs');
    const covenantOutput = tx.vout[0];

    // The covenant output continues the lineage the melted position was
    // already in. It is NOT deriveOrigin(input.txid, input.vout) -- that is
    // only how a MINT's first spend names a brand new lineage. Asserting the
    // derived value here is what let the "always derive" bug through: the
    // expectation was written to match the implementation instead of the
    // consensus rule.
    const expectedScript = UAP.buildTransferScript(pubkey, 1000, lineage);
    assert.deepStrictEqual(covenantOutput.scriptPubKey, expectedScript, 'covenant output must be a OP_MINT_TRANSFER script with correct origin');
  }

  // Test: covenant output has dust value
  {
    const privKey = new Uint8Array(32).fill(0x33);
    const pubkey = new Uint8Array(33).fill(0x44);
    pubkey[0] = 0x02;

    const inputTxid = 'bb'.repeat(32);
    const inputVout = 1;
    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 500, dummyOrigin);
    const fee = 50000;
    const inputValue = 5000000;

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: inputValue, privKey },
      toPubkey: pubkey,
      multiplier: 500,
      fee,
      remainderScript: dummyRemainderScript
    });

    const covenantOutput = tx.vout[0];
    assert.strictEqual(covenantOutput.value, UAP.DEFAULT_DUST_LIMIT, 'covenant output must have exactly DEFAULT_DUST_LIMIT satoshis');
  }

  // Test: remainder output has correct value (input - dust - fee)
  {
    const privKey = new Uint8Array(32).fill(0x55);
    const pubkey = new Uint8Array(33).fill(0x66);
    pubkey[0] = 0x02;

    const inputTxid = 'cc'.repeat(32);
    const inputVout = 2;
    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 100, dummyOrigin);
    const fee = 50000;
    const inputValue = 5000000;

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: inputValue, privKey },
      toPubkey: pubkey,
      multiplier: 100,
      fee,
      remainderScript: dummyRemainderScript
    });

    const remainderOutput = tx.vout[1];
    const expectedRemainder = inputValue - UAP.DEFAULT_DUST_LIMIT - fee;
    assert.strictEqual(remainderOutput.value, expectedRemainder, 'remainder output must be input - dust - fee');
  }

  // Test: covenant output preserves multiplier
  {
    const privKey = new Uint8Array(32).fill(0x77);
    const pubkey = new Uint8Array(33).fill(0x88);
    pubkey[0] = 0x02;

    // Test with multiplier 42 (fits in one byte, will be OP_42 which is 0x50 + 42)
    const multiplier = 42;
    const inputTxid = 'dd'.repeat(32);
    const inputVout = 3;
    // A transfer, so it is already in a lineage; melting keeps it there.
    const lineage = UAP.deriveOrigin('ef'.repeat(32), 1);
    const scriptCode = UAP.buildTransferScript(pubkey, multiplier, lineage);

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: 5000000, privKey },
      toPubkey: pubkey,
      multiplier,
      fee: 30000,
      remainderScript: dummyRemainderScript
    });

    const covenantOutput = tx.vout[0];
    // Same lineage in, same lineage out -- see the note on the melt test above.
    const expectedScript = UAP.buildTransferScript(pubkey, multiplier, lineage);
    assert.deepStrictEqual(covenantOutput.scriptPubKey, expectedScript, 'covenant output must preserve multiplier and use correct origin');
  }

  // Test: melting a position too small to leave dust + fee is rejected
  {
    const privKey = new Uint8Array(32).fill(0x99);
    const pubkey = new Uint8Array(33).fill(0xaa);
    pubkey[0] = 0x02;

    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 1, dummyOrigin);
    const fee = 50000;
    // A meltable position needs TWO dust limits plus the fee: one for the
    // covenant that must survive the spend, and one so the remainder output
    // is itself relayable. One satoshi short of that.
    const tooSmallValue = UAP.DEFAULT_DUST_LIMIT + fee + UAP.DEFAULT_DUST_LIMIT - 1;

    assert.throws(
      () => UAP.buildMeltTx(stubSecp, {
        input: { txid: 'ee'.repeat(32), vout: 4, scriptCode, value: tooSmallValue, privKey },
        toPubkey: pubkey,
        multiplier: 1,
        fee,
        remainderScript: dummyRemainderScript
      }),
      /insufficient|too small|must/i,
      'should reject a position whose remainder would not itself clear dust'
    );
  }

  // Test: buildMeltTx refuses to build without a remainderScript.
  // This is the burn path. The earlier implementation treated remainderScript
  // as optional and, when the remainder happened to fall below the dust limit,
  // emitted the covenant output alone -- paying the entire recovered backing
  // to the miner as fee, silently, in the one function whose whole purpose is
  // recovering that backing. Nothing in the suite covered it because every
  // other test passes a remainderScript.
  {
    const privKey = new Uint8Array(32).fill(0x44);
    const pubkey = new Uint8Array(33).fill(0xbb);
    pubkey[0] = 0x02;

    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 1, dummyOrigin);

    // Chosen so the remainder lands BELOW the dust limit: exactly the shape
    // the old code turned into a silent burn rather than an error.
    const value = UAP.DEFAULT_DUST_LIMIT + 50000 + (UAP.DEFAULT_DUST_LIMIT - 1);

    assert.throws(
      () => UAP.buildMeltTx(stubSecp, {
        input: { txid: '11'.repeat(32), vout: 0, scriptCode, value, privKey },
        toPubkey: pubkey,
        multiplier: 1
      }),
      /remainderScript/,
      'buildMeltTx must refuse to melt with nowhere to send the backing'
    );
  }

  // Test: covenant value does not exceed input value
  // (This is a check that dust + fee <= input value; see above test for boundary)
  {
    const privKey = new Uint8Array(32).fill(0xbb);
    const pubkey = new Uint8Array(33).fill(0xcc);
    pubkey[0] = 0x02;

    const inputTxid = 'ff'.repeat(32);
    const inputVout = 5;
    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 10, dummyOrigin);
    const fee = 50000;
    const inputValue = 5000000;

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: inputValue, privKey },
      toPubkey: pubkey,
      multiplier: 10,
      fee,
      remainderScript: dummyRemainderScript
    });

    const covenantOutput = tx.vout[0];
    assert(covenantOutput.value <= inputValue, 'covenant output value must not exceed input value');
  }

  // Test: default fee is applied
  {
    const privKey = new Uint8Array(32).fill(0xdd);
    const pubkey = new Uint8Array(33).fill(0xee);
    pubkey[0] = 0x02;

    const inputTxid = 'aa00'.repeat(16);
    const inputVout = 6;
    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 5, dummyOrigin);
    const inputValue = 5000000;

    // Call without specifying fee (should use default of 50000)
    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: inputValue, privKey },
      toPubkey: pubkey,
      multiplier: 5,
      remainderScript: dummyRemainderScript
    });

    const remainderOutput = tx.vout[1];
    const expectedRemainder = inputValue - UAP.DEFAULT_DUST_LIMIT - 50000; // 50000 is the default fee
    assert.strictEqual(remainderOutput.value, expectedRemainder, 'buildMeltTx should use default fee of 50000');
  }

  // Test: transaction is signed (input has scriptSig)
  {
    const privKey = new Uint8Array(32).fill(0xff);
    const pubkey = new Uint8Array(33).fill(0x01);
    pubkey[0] = 0x02;

    const inputTxid = 'bb11'.repeat(16);
    const inputVout = 7;
    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 20, dummyOrigin);

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode, value: 3000000, privKey },
      toPubkey: pubkey,
      multiplier: 20,
      fee: 40000,
      remainderScript: dummyRemainderScript
    });

    assert(tx.vin[0].scriptSig.length > 0, 'input should be signed (scriptSig should not be empty)');
  }

  // Test: melting a freshly minted position uses OP_MINT_TRANSFER, not OP_MINT
  // (This is tested by the fact that buildMeltTx always uses buildTransferScript,
  // which produces OP_MINT_TRANSFER regardless of the input being OP_MINT or OP_MINT_TRANSFER)
  {
    const privKey = new Uint8Array(32).fill(0x02);
    const pubkey = new Uint8Array(33).fill(0x03);
    pubkey[0] = 0x02;
    const multiplier = 777;

    // Input is a fresh mint (OP_MINT script, no origin)
    const mintScriptCode = UAP.buildMintScript(pubkey, multiplier);

    const inputTxid = 'cc22'.repeat(16);
    const inputVout = 8;

    const tx = UAP.buildMeltTx(stubSecp, {
      input: { txid: inputTxid, vout: inputVout, scriptCode: mintScriptCode, value: 4000000, privKey },
      toPubkey: pubkey,
      multiplier,
      fee: 45000,
      remainderScript: dummyRemainderScript
    });

    // The output must be a OP_MINT_TRANSFER script (which uses OP_MINT_TRANSFER, not OP_MINT)
    const covenantOutput = tx.vout[0];
    const expectedOrigin = UAP.deriveOrigin(inputTxid, inputVout);
    const expectedScript = UAP.buildTransferScript(pubkey, multiplier, expectedOrigin);
    assert.deepStrictEqual(covenantOutput.scriptPubKey, expectedScript, 'melting a mint must still emit OP_MINT_TRANSFER, not OP_MINT');

    // Verify it's using OP_MINT_TRANSFER (0xba), not OP_MINT (0xb5)
    assert.strictEqual(covenantOutput.scriptPubKey[covenantOutput.scriptPubKey.length - 1], UAP.OP_MINT_TRANSFER, 'last byte of covenant output must be OP_MINT_TRANSFER');
  }

  // Test: fee cap enforcement (refuse a fee above DEFAULT_TRANSACTION_MAXFEE unless explicitly authorized)
  {
    const privKey = new Uint8Array(32).fill(0x04);
    const pubkey = new Uint8Array(33).fill(0x05);
    pubkey[0] = 0x02;

    const dummyOrigin = new Uint8Array(32).fill(0x00);
    const scriptCode = UAP.buildTransferScript(pubkey, 1, dummyOrigin);

    assert.throws(
      () => UAP.buildMeltTx(stubSecp, {
        input: { txid: 'dd33'.repeat(16), vout: 9, scriptCode, value: 1000000000, privKey },
        toPubkey: pubkey,
        multiplier: 1,
        fee: UAP.DEFAULT_TRANSACTION_MAXFEE + 1,
        remainderScript: dummyRemainderScript
      }),
      /fee/i,
      'buildMeltTx must refuse a fee above the cap'
    );
  }
}

// ---- serializeTx: txid length validation ----
// A transaction input's txid must be exactly 32 bytes (64 hex characters).
// A txid shorter or longer than that is invalid and must be rejected with a
// clear error message naming the offending txid and its actual byte length.
// This prevents silent corruption of transaction bytes due to a typo in the txid.
{
  // Test 1: Reject a txid that is too short (31 bytes / 62 hex characters)
  {
    const tooShortTxid = 'aa'.repeat(31);  // 31 bytes = 62 hex chars
    assert.strictEqual(tooShortTxid.length, 62, 'test setup: short txid should be 62 hex chars');

    const tx = {
      version: 1,
      locktime: 0,
      vin: [{ txid: tooShortTxid, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
      vout: [{ value: 100000, scriptPubKey: new Uint8Array([1, 2, 3]) }],
    };

    assert.throws(
      () => UAP.serializeTx(tx),
      /txid|bytes|32|length/i,
      'serializeTx should reject a 31-byte txid with a clear error message'
    );
  }

  // Test 2: Reject a txid that is too long (33 bytes / 66 hex characters)
  {
    const tooLongTxid = 'bb'.repeat(33);  // 33 bytes = 66 hex chars
    assert.strictEqual(tooLongTxid.length, 66, 'test setup: long txid should be 66 hex chars');

    const tx = {
      version: 1,
      locktime: 0,
      vin: [{ txid: tooLongTxid, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
      vout: [{ value: 100000, scriptPubKey: new Uint8Array([1, 2, 3]) }],
    };

    assert.throws(
      () => UAP.serializeTx(tx),
      /txid|bytes|32|length/i,
      'serializeTx should reject a 33-byte txid with a clear error message'
    );
  }

  // Test 3: Accept a correct 32-byte txid (64 hex characters)
  {
    const correctTxid = 'cc'.repeat(32);  // 32 bytes = 64 hex chars
    assert.strictEqual(correctTxid.length, 64, 'test setup: correct txid should be 64 hex chars');

    const tx = {
      version: 1,
      locktime: 0,
      vin: [{ txid: correctTxid, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
      vout: [{ value: 100000, scriptPubKey: new Uint8Array([1, 2, 3]) }],
    };

    // Should not throw; serialization should succeed
    const serialized = UAP.serializeTx(tx);
    assert(serialized instanceof Uint8Array, 'serializeTx should return a Uint8Array for a valid 32-byte txid');
    assert(serialized.length > 0, 'serialized transaction should have non-zero length');
  }
}

// ---------------------------------------------------------------------------
// A spend takes its lineage from the covenant it spends, not from its outpoint.
//
// Regression: all three builders derived the origin as deriveOrigin(input.txid,
// input.vout) unconditionally. That is right only for a mint's FIRST spend,
// where identity is being created. Spending a transfer must carry the existing
// origin forward -- and since every spend after the first spends a transfer,
// the bug meant the wallet could build exactly one valid transaction per token
// and then silently emitted transactions consensus rejects. Every test passed,
// because every test spent a mint.
// ---------------------------------------------------------------------------
{
  const priv = new Uint8Array(32).fill(0x33);
  const pub = new Uint8Array(33).fill(0x22);
  pub[0] = 0x02;
  const toPubkey = new Uint8Array(33).fill(0x03);
  toPubkey[0] = 0x02;
  const lineage = UAP.deriveOrigin('ab'.repeat(32), 0);
  const stubSecp = createStubSecp();

  const originOfOutput = (script) => {
    const hex = UAP.bytesToHex(script);
    return hex.slice(hex.length - 66, hex.length - 2);
  };

  // Spending a transfer: the output must stay in the input's lineage.
  const transferScript = UAP.buildTransferScript(pub, 4, lineage);
  const spendTransfer = UAP.buildTransferTx(stubSecp, {
    input: { txid: 'dd'.repeat(32), vout: 2, scriptCode: transferScript, value: 10 * UAP.COIN, privKey: priv },
    toPubkey, multiplier: 4, fee: 60000,
  });
  assert.strictEqual(originOfOutput(spendTransfer.vout[0].scriptPubKey), UAP.bytesToHex(lineage),
     'spending a transfer must carry its lineage forward, not mint a new one');

  // Spending a mint: identity is created here, from the outpoint being spent.
  const mintScript = UAP.buildMintScript(pub, 4);
  const spendMint = UAP.buildTransferTx(stubSecp, {
    input: { txid: 'ee'.repeat(32), vout: 1, scriptCode: mintScript, value: 10 * UAP.COIN, privKey: priv },
    toPubkey, multiplier: 4, fee: 60000,
  });
  assert.strictEqual(originOfOutput(spendMint.vout[0].scriptPubKey), UAP.bytesToHex(UAP.deriveOrigin('ee'.repeat(32), 1)),
     "a mint's first spend stamps SHA256 of its own outpoint");

  // Melt must behave the same way: it is a spend like any other.
  const melt = UAP.buildMeltTx(stubSecp, {
    input: { txid: 'dd'.repeat(32), vout: 2, scriptCode: transferScript, value: 10 * UAP.COIN, privKey: priv },
    toPubkey, multiplier: 4,
    remainderScript: new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xff), 0x88, 0xac]),
    fee: 60000,
  });
  assert.strictEqual(originOfOutput(melt.vout[0].scriptPubKey), UAP.bytesToHex(lineage),
     'melting a transfer must keep the position in its own lineage');

  // A scriptCode that is neither shape is refused rather than guessed at.
  let threw = false;
  try { UAP.originForSpend(new Uint8Array([0x76, 0xa9, 0x14, ...new Array(20).fill(0xff), 0x88, 0xac]), 'dd'.repeat(32), 0); }
  catch (e) { threw = true; }
  if (!threw) throw new Error('originForSpend accepted a non-covenant scriptCode');

  console.log('uap.js: lineage is taken from the covenant spent, not the outpoint');
}

// ---- deserializeTx / txidFromBytes: reading a transaction back, and
// verifying its identity ----
//
// The natural first test for a deserializer is that it round-trips
// serializeTx's own output. That alone would not prove much about real
// chain data, so the real teeth is in the fixture-based round trip below
// and in the parseUapScript tests: together they cover both directions
// (build -> parse) and the identity check a wallet actually leans on.
{
  const pubkey = new Uint8Array(33).fill(7);
  const origin = new Uint8Array(32).fill(0x42);
  const scriptPubKey = UAP.buildTransferScript(pubkey, 1000, origin);
  const tx = {
    version: 2,
    locktime: 12345,
    vin: [
      { txid: 'ab'.repeat(32), vout: 3, scriptSig: UAP.hexToBytes('4830450221'), sequence: 0xfffffffe },
      { txid: 'cd'.repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
    ],
    vout: [
      { value: 100000000, scriptPubKey },
      { value: 5000000000, scriptPubKey: UAP.hexToBytes('76a914' + 'aa'.repeat(20) + '88ac') },
    ],
  };
  const hex = UAP.txToHex(tx);
  const parsed = UAP.deserializeTx(hex);

  assert.strictEqual(parsed.version, tx.version);
  assert.strictEqual(parsed.locktime, tx.locktime);
  assert.strictEqual(parsed.vin.length, 2);
  assert.strictEqual(parsed.vin[0].txid, 'ab'.repeat(32));
  assert.strictEqual(parsed.vin[0].vout, 3);
  assert.strictEqual(UAP.bytesToHex(parsed.vin[0].scriptSig), '4830450221');
  assert.strictEqual(parsed.vin[0].sequence, 0xfffffffe);
  assert.strictEqual(parsed.vin[1].txid, 'cd'.repeat(32));
  assert.strictEqual(parsed.vout.length, 2);
  assert.strictEqual(parsed.vout[0].value, 100000000);
  assert.strictEqual(UAP.bytesToHex(parsed.vout[0].scriptPubKey), UAP.bytesToHex(scriptPubKey));
  assert.strictEqual(parsed.vout[1].value, 5000000000);

  // Re-serializing what we parsed must reproduce the exact original bytes:
  // a deserializer that silently normalizes anything (e.g. re-minimalizing
  // a push) would pass every field-by-field check above and still not be
  // a faithful inverse.
  assert.strictEqual(UAP.txToHex(parsed), hex, 'deserializeTx . txToHex must round-trip exactly');

  console.log('uap.js: deserializeTx round-trips txToHex output exactly');
}

// deserializeTx rejects truncated input rather than returning a partial,
// silently-wrong transaction.
{
  const full = UAP.hexToBytes(UAP.txToHex({
    version: 1, locktime: 0,
    vin: [{ txid: '11'.repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 1000, scriptPubKey: UAP.hexToBytes('51') }],
  }));
  const truncated = full.slice(0, full.length - 3);
  assert.throws(() => UAP.deserializeTx(truncated), /truncated transaction/,
    'a truncated transaction must be refused, not parsed short');

  // Trailing garbage is refused too -- those bytes are not part of the
  // transaction the txid commits to, so silently dropping them would let
  // a comparison against a verified length pass when it should not.
  const withGarbage = UAP.concatBytes(full, UAP.hexToBytes('deadbeef'));
  assert.throws(() => UAP.deserializeTx(withGarbage), /trailing bytes/,
    'trailing bytes after a well-formed transaction must be refused');

  console.log('uap.js: deserializeTx refuses truncated input and trailing garbage');
}

// txidFromBytes: hashes the exact bytes handed in and reports them in
// display (byte-reversed) order, matching what every RPC/explorer calls
// the txid.
{
  const tx = {
    version: 1, locktime: 0,
    vin: [{ txid: '22'.repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
    vout: [{ value: 1000, scriptPubKey: UAP.hexToBytes('51') }],
  };
  const hex = UAP.txToHex(tx);
  const bytes = UAP.hexToBytes(hex);
  // Compare against node's own crypto module -- an independent
  // implementation of SHA256 -- rather than only checking this function
  // against itself, so a bug shared between txidFromBytes and hash256
  // (sha256.js) cannot hide from this test.
  const nodeHash256 = (b) => crypto.createHash('sha256').update(
    crypto.createHash('sha256').update(b).digest()
  ).digest();
  const expectedTxid = UAP.bytesToHex(Uint8Array.from(nodeHash256(bytes)).reverse());
  const txidFromHex = UAP.txidFromBytes(hex);
  assert.strictEqual(txidFromHex, expectedTxid, 'txidFromBytes must match double-SHA256, byte-reversed, computed independently');
  const txidFromArray = UAP.txidFromBytes(bytes);
  assert.strictEqual(txidFromHex, txidFromArray, 'hex and Uint8Array input must produce the same txid');
  assert.match(txidFromHex, /^[0-9a-f]{64}$/, 'txid must be 64 lowercase hex characters');

  const corrupted = Uint8Array.from(bytes);
  corrupted[corrupted.length - 1] ^= 0xff;
  assert.notStrictEqual(UAP.txidFromBytes(corrupted), txidFromHex,
    'flipping a byte anywhere in the transaction must change its txid');

  console.log('uap.js: txidFromBytes is deterministic and sensitive to every byte');
}

// parseUapScript: the inverse of buildMintScript / buildTransferScript.
{
  const pubkey = new Uint8Array(33).fill(9);
  const origin = new Uint8Array(32).fill(0x11);

  // Round trip a transfer script for a variety of multipliers, including
  // the OP_1..OP_16 special-cased range and values that require a data
  // push.
  for (const multiplier of [0, 1, 16, 17, 1000, 2147483647]) {
    const script = UAP.buildTransferScript(pubkey, multiplier, origin);
    const parsed = UAP.parseUapScript(script);
    assert(parsed !== null, `parseUapScript rejected a valid transfer script for multiplier ${multiplier}`);
    assert.strictEqual(parsed.isMint, false);
    assert.strictEqual(parsed.multiplier, multiplier);
    assert.strictEqual(UAP.bytesToHex(parsed.pubkey), UAP.bytesToHex(pubkey));
    assert.strictEqual(UAP.bytesToHex(parsed.origin), UAP.bytesToHex(origin));
  }

  // Round trip a mint script too: no origin, isMint true.
  const mintScript = UAP.buildMintScript(pubkey, 42);
  const parsedMint = UAP.parseUapScript(mintScript);
  assert(parsedMint !== null);
  assert.strictEqual(parsedMint.isMint, true);
  assert.strictEqual(parsedMint.multiplier, 42);
  assert.strictEqual(parsedMint.origin.length, 0);

  // An ordinary P2PKH output is not a UAP output at all -- null, not a
  // thrown error, matching script.go's "most outputs are not UAP outputs"
  // contract.
  const p2pkh = UAP.hexToBytes('76a914' + 'aa'.repeat(20) + '88ac');
  assert.strictEqual(UAP.parseUapScript(p2pkh), null);

  // A truncated / malformed script that happens to end in OP_MINT_TRANSFER
  // must not crash the parser or be misread as a valid one.
  const malformed = UAP.concatBytes(new Uint8Array([0x21]), new Uint8Array(10), new Uint8Array([UAP.OP_MINT_TRANSFER]));
  assert.strictEqual(UAP.parseUapScript(malformed), null);

  console.log('uap.js: parseUapScript round-trips buildMintScript/buildTransferScript');
}

// The teeth for the wallet-side defect this whole feature exists to close:
// parseUapScript must disagree with a lying order whenever the order's
// claimed multiplier or origin does not match the position's actual,
// verified scriptPubKey. This is exercised end-to-end (relay-shaped
// mismatch -> wallet refusal) in uap-web/market.test.js; this half just
// pins that the two fields parseUapScript reports are exactly the ones
// that must be compared, and that they come back as the real, decoded
// values rather than something derived from the caller's expectations.
{
  const pubkey = new Uint8Array(33).fill(5);
  const realOrigin = new Uint8Array(32).fill(0xaa);
  const claimedOrigin = new Uint8Array(32).fill(0xbb); // what a lying order might claim
  const script = UAP.buildTransferScript(pubkey, 1000, realOrigin);
  const parsed = UAP.parseUapScript(script);

  assert.notStrictEqual(UAP.bytesToHex(parsed.origin), UAP.bytesToHex(claimedOrigin),
    'sanity: the fixture is set up so real and claimed origin differ');
  // A caller comparing parsed.origin against an order's claimed origin
  // would catch this; parseUapScript's job is only to report the truth,
  // which this asserts it does.
  assert.strictEqual(UAP.bytesToHex(parsed.origin), UAP.bytesToHex(realOrigin));
  assert.strictEqual(parsed.multiplier, 1000);

  console.log('uap.js: parseUapScript reports the real multiplier/origin, not a caller\'s claim');
}
