// Test the deriveOrigin function against the shared fixture
// (src/test/data/uap_origin_vectors.json)

import { readFileSync } from 'fs';
import * as UAP from './uap.js';

const vectors = JSON.parse(
  readFileSync(new URL('../../src/test/data/uap_origin_vectors.json', import.meta.url), 'utf8')
);

// Skip the _doc entry if present
const testVectors = vectors.filter((v) => v.n !== undefined);

let passed = 0;
let failed = 0;

for (const v of testVectors) {
  try {
    const result = UAP.deriveOrigin(v.txid, v.n);
    const resultHex = UAP.bytesToHex(result);
    if (resultHex === v.origin) {
      passed++;
    } else {
      console.error(`FAIL: ${v.comment}`);
      console.error(`  txid: ${v.txid}, n: ${v.n}`);
      console.error(`  expected: ${v.origin}`);
      console.error(`  got:      ${resultHex}`);
      failed++;
    }
  } catch (e) {
    console.error(`ERROR: ${v.comment}: ${e.message}`);
    failed++;
  }
}

console.log(`origin-vectors: ${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
