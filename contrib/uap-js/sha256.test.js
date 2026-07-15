import assert from 'assert';
import nodeCrypto from 'crypto';
import { sha256, hash256, hmacSha256 } from './sha256.js';

function hex(bytes) {
  return Buffer.from(bytes).toString('hex');
}
function fromUtf8(s) {
  return new Uint8Array(Buffer.from(s, 'utf8'));
}
function nodeSha256Hex(s) {
  return nodeCrypto.createHash('sha256').update(s).digest('hex');
}

// NIST FIPS 180-4 test vectors, cross-checked against Node's own
// crypto.createHash('sha256') rather than hand-copied hex strings.
const vectors = [
  '',
  'abc',
  'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq',
];
for (const v of vectors) {
  assert.strictEqual(hex(sha256(fromUtf8(v))), nodeSha256Hex(v), `mismatch for ${JSON.stringify(v)}`);
}

// A 1,000,000-byte input, to exercise the multi-block padding path.
const million_a = Buffer.alloc(1000000, 0x61);
assert.strictEqual(hex(sha256(new Uint8Array(million_a))), nodeSha256Hex(million_a));

// hash256 = sha256(sha256(x))
assert.strictEqual(hex(hash256(fromUtf8('abc'))), hex(sha256(sha256(fromUtf8('abc')))));
assert.strictEqual(hex(hash256(fromUtf8('abc'))), nodeSha256Hex(Buffer.from(nodeSha256Hex('abc'), 'hex')));

// RFC 4231 HMAC-SHA256 test vectors, cross-checked against Node's own
// crypto.createHmac('sha256', ...).
const hmacVectors = [
  { key: Buffer.from('0b'.repeat(20), 'hex'), data: Buffer.from('Hi There') },
  { key: Buffer.from('Jefe'), data: Buffer.from('what do ya want for nothing?') },
  { key: Buffer.from('aa'.repeat(20), 'hex'), data: Buffer.from('dd'.repeat(50), 'hex') },
  { key: Buffer.from('aa'.repeat(131), 'hex'), data: Buffer.from('Test Using Larger Than Block-Size Key - Hash Key First') },
];
for (const { key, data } of hmacVectors) {
  const got = hex(hmacSha256(new Uint8Array(key), new Uint8Array(data)));
  const want = nodeCrypto.createHmac('sha256', key).update(data).digest('hex');
  assert.strictEqual(got, want, `hmac mismatch for key=${key.toString('hex')}`);
}

console.log('sha256.js: all vectors passed (sha256, hash256, hmacSha256)');
