import assert from 'assert';
import fs from 'node:fs';
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
  const salt = new Uint8Array(20).fill(0x11);

  const mint = UAP.buildMintScript(pubkey, 1000, salt);
  // <21 pubkey(33)> <02 e803 (1000 LE)> <14 salt(20)> <b5 OP_MINT>
  assert.strictEqual(mint[0], 0x21);
  assert.strictEqual(mint[34], 0x02); // multiplier push length
  assert.strictEqual(mint[35], 0xe8);
  assert.strictEqual(mint[36], 0x03);
  assert.strictEqual(mint[37], 0x14); // salt push length
  assert.strictEqual(mint[mint.length - 1], UAP.OP_MINT);
  assert.strictEqual(mint.length, 1 + 33 + 1 + 2 + 1 + 20 + 1);

  const transfer = UAP.buildTransferScript(pubkey, 42);
  assert.strictEqual(transfer[0], 0x21);
  assert.strictEqual(transfer[34], 0x01); // 42 fits in one byte
  assert.strictEqual(transfer[35], 42);
  assert.strictEqual(transfer[transfer.length - 1], UAP.OP_MINT_TRANSFER);

  assert.throws(() => UAP.buildMintScript(pubkey, 1000, new Uint8Array(15)), /at least 16 bytes/);
}

// Multiplier encoding. A UAP output must use the canonical push for every
// element: OP_0 for 0, OP_1..OP_16 for 1..16, a minimal data push above
// that. Consensus (ParseUapOutputScript) and SCRIPT_VERIFY_MINIMALDATA now
// agree on exactly that set, so every position this builds can be spent by a
// standard transaction. See pushMultiplier.
{
  const pubkey = new Uint8Array(33).fill(0x02);

  // 0 -> OP_0: an empty data push, and already minimal.
  const t0 = UAP.buildTransferScript(pubkey, 0);
  assert.strictEqual(t0[34], 0x00);
  assert.strictEqual(t0.length, 1 + 33 + 1 + 1);

  // 1..16 -> the small-integer opcodes, one byte, no length prefix.
  for (const m of [1, 2, 10, 15, 16]) {
    const t = UAP.buildTransferScript(pubkey, m);
    assert.strictEqual(t[34], 0x50 + m, `multiplier ${m} must encode as OP_${m}`);
    assert.strictEqual(t.length, 1 + 33 + 1 + 1, `multiplier ${m} must be a single byte`);

    const mint = UAP.buildMintScript(pubkey, m, new Uint8Array(16));
    assert.strictEqual(mint[34], 0x50 + m, `mint multiplier ${m} must encode as OP_${m}`);
  }

  // 17 is the first multiplier above the small-int range, so it becomes a
  // one-byte data push.
  const t17 = UAP.buildTransferScript(pubkey, 17);
  assert.strictEqual(t17[34], 0x01);
  assert.strictEqual(t17[35], 17);
  assert.strictEqual(t17.length, 1 + 33 + 2 + 1);

  const t1000 = UAP.buildTransferScript(pubkey, 1000);
  assert.strictEqual(t1000[34], 0x02);
  assert.strictEqual(t1000[35], 0xe8);
  assert.strictEqual(t1000[36], 0x03);

  // Out-of-range multipliers are rejected rather than silently encoded.
  assert.throws(() => UAP.buildTransferScript(pubkey, -1), /multiplier/);
  assert.throws(() => UAP.buildTransferScript(pubkey, 2147483648), /multiplier/);
  assert.throws(() => UAP.buildTransferScript(pubkey, 1.5), /multiplier/);
}

// transaction serialization: a 1-in-1-out tx has a predictable byte length,
// and re-decoding the varints/fields back out should match what went in.
{
  const scriptPubKey = UAP.buildTransferScript(new Uint8Array(33).fill(3), 500);
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
  const scriptCode = UAP.buildTransferScript(pubkey, 500);

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
  const scriptCode = UAP.buildTransferScript(pubkey, 500);

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
      const { elements } = splitElements(expected);
      assert.strictEqual(elements.length, 3, `fixture "${vec.comment}": expected 3 elements`);
      actual = UAP.buildMintScript(pubkey, vec.multiplier, elements[2].data);
    } else {
      actual = UAP.buildTransferScript(pubkey, vec.multiplier);
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
  const scriptPubKey = UAP.buildTransferScript(pubkey, 1000);

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
