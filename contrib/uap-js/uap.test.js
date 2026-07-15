import assert from 'assert';
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

// small-int multiplier encoding: 0, 1..16, and -1 use single-opcode pushes
// (OP_0 / OP_1.. OP_16 / OP_1NEGATE), matching CScript::operator<<(int64_t).
{
  const pubkey = new Uint8Array(33).fill(0x02);
  const t0 = UAP.buildTransferScript(pubkey, 0);
  assert.strictEqual(t0[34], 0x00); // OP_0

  const t10 = UAP.buildTransferScript(pubkey, 10);
  assert.strictEqual(t10[34], 0x50 + 10); // OP_10
  assert.strictEqual(t10.length, 1 + 33 + 1 + 1); // pubkey push + single opcode + OP_MINT_TRANSFER
}

// transaction serialization: a 1-in-1-out tx has a predictable byte length,
// and re-decoding the varints/fields back out should match what went in.
{
  const scriptPubKey = UAP.buildTransferScript(new Uint8Array(33).fill(3), 5);
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

console.log('uap.js: all tests passed');
