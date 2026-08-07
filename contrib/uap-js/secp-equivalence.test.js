// secp-equivalence.test.js -- pins the exact bytes the elliptic-curve library
// is allowed to produce.
//
// WHY THIS EXISTS
//
// Almost nothing in the offline test suite touches real EC code. uap.test.js
// signs with createStubSecp(), which returns a fixed 0xaa.../0xbb... blob;
// sighash-vectors.test.js and addr.test.js are pure hashing/base58. The only
// tests that ever exercised a real secp256k1 were the integration tests, and
// those skip unless a whippetd binary is present. So a change of EC library --
// exactly what the v1.7.1 -> v2.3.0 upgrade was -- could previously have landed
// with the whole suite green and every signature subtly wrong.
//
// ECDSA per RFC6979 is fully deterministic: for a given (private key, message
// hash) there is exactly one correct signature, and it does not depend on which
// library computed it. That makes the bytes themselves testable. The fixture in
// secp-equivalence.fixture.json was generated with @noble/secp256k1 v1.7.1,
// before the upgrade, using this repo's own signing path.
//
// IF THIS TEST FAILS after a library bump, the new library disagrees with
// RFC6979 as previously implemented, or with the DER encoding, or with low-S
// normalisation. That is a finding to investigate, not a fixture to regenerate:
// re-recording the expectations would turn a real behavioural change into a
// silent one. (The fixture was produced by driving getPublicKey/signSync and
// UAP.signP2PKHInput/signSpend over fixed inputs; recreating it needs the old
// library installed.)

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as secp from './secp.js';
import * as UAP from './uap.js';

const fixture = JSON.parse(
  readFileSync(new URL('./secp-equivalence.fixture.json', import.meta.url), 'utf8')
);

UAP.configureSecp(secp);

const hex = (b) => UAP.bytesToHex(b);
const unhex = (s) => UAP.hexToBytes(s);

let checks = 0;
function eq(actual, expected, what) {
  assert.equal(actual, expected, `${what}\n  expected (v${fixture.nobleVersion}): ${expected}\n  actual:   ${actual}`);
  checks++;
}

// The same fixed transaction the fixture generator signed. Rebuilt per call
// because signing helpers are given a fresh copy each time.
function fixedTx() {
  return {
    version: 1,
    locktime: 0,
    vin: [
      { txid: 'ab'.repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
      { txid: 'cd'.repeat(32), vout: 3, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
    ],
    vout: [
      { value: 12345678, scriptPubKey: unhex('76a914' + '11'.repeat(20) + '88ac') },
      { value: 87654321, scriptPubKey: unhex('76a914' + '22'.repeat(20) + '88ac') },
    ],
  };
}

// ---- public keys ----
for (const k of fixture.keys) {
  const priv = unhex(k.priv);
  eq(hex(secp.getPublicKey(priv, true)), k.pubCompressed, `getPublicKey(${k.priv.slice(0, 8)}.., true)`);
  eq(hex(secp.getPublicKey(priv, false)), k.pubUncompressed, `getPublicKey(${k.priv.slice(0, 8)}.., false)`);
}

// ---- raw DER signatures via the shim uap.js calls ----
for (const s of fixture.rawSignatures) {
  const der = secp.signSync(unhex(s.msgHash), unhex(s.priv), { canonical: true, der: true });
  eq(hex(der), s.der, `signSync(${s.msgHash.slice(0, 8)}.., ${s.priv.slice(0, 8)}..)`);

  // Structural invariants the consensus rules care about, independent of the
  // fixture: strict DER shape, and low-S (BIP62) so the signature is canonical.
  assert.equal(der[0], 0x30, 'DER signature must start with SEQUENCE');
  assert.equal(der[1], der.length - 2, 'DER length byte must cover the rest');
  assert.equal(der[2], 0x02, 'first DER element must be INTEGER r');
  assert.equal(der[4 + der[3]], 0x02, 'second DER element must be INTEGER s');
  checks += 4;
}

// A high-S signature must be normalised down; asking for the non-canonical
// form must actually produce something different at least once across the set,
// otherwise "low-S" would be untested regardless of what the default does.
{
  let sawDifference = false;
  for (const s of fixture.rawSignatures) {
    const lo = secp.signSync(unhex(s.msgHash), unhex(s.priv), { canonical: true, der: false });
    const hi = secp.signSync(unhex(s.msgHash), unhex(s.priv), { canonical: false, der: false });
    if (hex(lo) !== hex(hi)) sawDifference = true;
    // Whatever else, the canonical form must be low-S.
    const sVal = BigInt('0x' + hex(lo.slice(32)));
    const halfN = BigInt('0x7fffffffffffffffffffffffffffffff5d576e7357a4501ddfe92f46681b20a0');
    assert.ok(sVal <= halfN, 'canonical signature must have low S');
    checks++;
  }
  assert.ok(sawDifference, 'expected at least one fixture vector where low-S normalisation bites');
  checks++;
}

// ---- the codebase's own signing path ----
for (const c of fixture.scriptSigs) {
  const priv = unhex(c.priv);
  const key = fixture.keys.find((k) => k.priv === c.priv);
  const pub = unhex(key.pubCompressed);
  const scriptCode = UAP.buildTransferScript(pub, 500);
  const label = `${c.priv.slice(0, 8)}.. hashType=${c.hashType} nIn=${c.nIn}`;

  eq(hex(UAP.signatureHash(scriptCode, fixedTx(), c.nIn, c.hashType)), c.sigHash, `signatureHash ${label}`);
  eq(
    hex(UAP.signP2PKHInput(secp, scriptCode, priv, pub, fixedTx(), c.nIn, c.hashType)),
    c.signP2PKHInput,
    `signP2PKHInput ${label}`
  );
  eq(
    hex(UAP.signSpend(secp, scriptCode, priv, fixedTx(), c.nIn, c.hashType)),
    c.signSpend,
    `signSpend ${label}`
  );
}

// ---- the signatures must actually verify ----
for (const s of fixture.rawSignatures) {
  const compact = secp.signSync(unhex(s.msgHash), unhex(s.priv), { der: false });
  const pub = secp.getPublicKey(unhex(s.priv), true);
  assert.ok(secp.verify(compact, unhex(s.msgHash), pub), 'signature must verify against its own pubkey');
  checks++;
}

// ---- v2 API guards ----
// The v1 option names must never be forwarded to noble, which rejects them.
assert.doesNotThrow(
  () => secp.signSync(new Uint8Array(32).fill(7), new Uint8Array(32).fill(3), { canonical: true, der: true }),
  'v1-style options must be translated, not forwarded'
);
checks++;

// signSync must fail loudly, not silently, if configureSecp was never called.
{
  const saved = secp.etc.hmacSha256Sync;
  secp.etc.hmacSha256Sync = undefined;
  assert.throws(
    () => secp.signSync(new Uint8Array(32).fill(7), new Uint8Array(32).fill(3)),
    /hmacSha256Sync/,
    'unconfigured signSync must throw'
  );
  secp.etc.hmacSha256Sync = saved;
  checks++;
}

console.log(
  `secp-equivalence: ${checks} assertions passed ` +
    `(v${fixture.nobleVersion} fixture reproduced by the installed library)`
);
