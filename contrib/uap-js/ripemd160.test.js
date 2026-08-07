import assert from 'assert';
import { ripemd160 } from './ripemd160.js';
import { hex, fromHex, fromUtf8 } from './test-helpers.js';

// RIPEMD-160 specification test vectors.
// These are canonical — if implementation disagrees, implementation is wrong.
const vectors = [
  { input: '', expected: '9c1185a5c5e9fc54612808977ee8f548b2258d31' },
  { input: 'a', expected: '0bdc9d2d256b3ee9daae347be6f4dc835a467ffe' },
  { input: 'abc', expected: '8eb208f7e05d987a9b044a8e98c6b087f15a0bfc' },
  { input: 'message digest', expected: '5d0689ef49d2fae572b881b123a85ffa21595f36' },
  { input: 'abcdefghijklmnopqrstuvwxyz', expected: 'f71c27109c692c1b56bbdceb5b9d2865b3708dbc' },
  { input: 'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq', expected: '12a053384a9c0c88e405a06c27dcf49ada62eb2b' },
  { input: 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789', expected: 'b0e20b6e3116640286ed3a87a5713079b21f5189' },
];

for (const { input, expected } of vectors) {
  const result = hex(ripemd160(fromUtf8(input)));
  assert.strictEqual(result, expected, `mismatch for ${JSON.stringify(input)}`);
}

// Test 1,000,000 'a's — exercises multi-block padding path.
const million_a = Buffer.alloc(1000000, 0x61);
assert.strictEqual(hex(ripemd160(new Uint8Array(million_a))), '52783243c1697bdbe16d37f97f68f08325dc1528');

// Test 8 repetitions of "1234567890"
const repeated_digits = Buffer.from('1234567890'.repeat(8), 'utf8');
assert.strictEqual(hex(ripemd160(new Uint8Array(repeated_digits))), '9b752e45573d4b39f4dbd3323cab82bf63326bfb');

console.log('ripemd160.test.js: all vectors passed');
