import assert from 'assert';
import fs from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import {
  hash160,
  base58Encode,
  base58Decode,
  base58checkEncode,
  base58checkDecode,
  buildP2PKHScript,
  pubkeyToAddress,
  addressToScript,
  scriptToAddress,
  VERSIONS
} from './addr.js';
import { hex, fromHex, fromUtf8 } from './test-helpers.js';

let assertCount = 0;
function track(condition, message) {
  assertCount++;
  assert(condition, message);
}

// Test 1: base58 encode/decode round-trip (original simple cases)
{
  const testCases = [
    fromHex(''),
    fromHex('00'),
    fromHex('0000'),
    fromHex('00ff'),
    fromHex('deadbeef'),
    fromHex('0102030405060708090a0b0c0d0e0f'),
  ];
  for (const bytes of testCases) {
    const encoded = base58Encode(bytes);
    const decoded = base58Decode(encoded);
    track(
      Buffer.from(decoded).equals(Buffer.from(bytes)),
      `round-trip failed for ${hex(bytes)}`
    );
  }
}

// Test 2: Leading zeros become leading 1's in base58
{
  const single_zero = fromHex('00');
  track(base58Encode(single_zero) === '1', 'single 00 should encode to "1"');

  const two_zeros = fromHex('0000');
  track(base58Encode(two_zeros) === '11', 'two 00s should encode to "11"');

  const zeros_with_data = fromHex('0001ff');
  const encoded = base58Encode(zeros_with_data);
  track(encoded.startsWith('1'), `leading zero not converted to 1: ${encoded}`);
}

// Test 3: base58check round-trip
{
  const payload = fromHex('deadbeefcafebabe');
  const version = 73; // Mainnet PUBKEY_ADDRESS
  const encoded = base58checkEncode(payload, version);
  const decoded = base58checkDecode(encoded);
  track(decoded.version === version, `version mismatch`);
  track(
    Buffer.from(decoded.payload).equals(Buffer.from(payload)),
    'payload mismatch in base58check round-trip'
  );
}

// Test 4: base58check with corrupted checksum throws (single position)
{
  const payload = fromHex('deadbeefcafebabe');
  const version = 73;
  let encoded = base58checkEncode(payload, version);
  // Flip one character (avoiding '0' and 'O' and 'I' and 'l' which aren't in base58)
  const chars = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
  const idx = encoded.length - 1;
  const oldChar = encoded[idx];
  let newChar = chars[0];
  while (newChar === oldChar) newChar = chars[1];
  encoded = encoded.slice(0, idx) + newChar + encoded.slice(idx + 1);

  let threw = false;
  try {
    base58checkDecode(encoded);
  } catch (e) {
    threw = true;
  }
  track(threw, 'corrupted checksum should throw');
}

// Test 5: hash160 of a known compressed pubkey
{
  // Compressed pubkey: 02...
  const pubkey = fromHex('0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798');
  const result = hash160(pubkey);
  track(result.length === 20, 'hash160 should produce 20 bytes');
  // Cross-check against reference value
  const expectedHash = fromHex('751e76e8199196d454941c45d1b3a323f1433bd6');
  track(
    Buffer.from(result).equals(Buffer.from(expectedHash)),
    `hash160 mismatch: got ${hex(result)}, expected ${hex(expectedHash)}`
  );
}

// Test 6: buildP2PKHScript format
{
  const pubkey = Buffer.alloc(33, 0xab);
  const script = buildP2PKHScript(pubkey);
  // Expected: OP_DUP (0x76) OP_HASH160 (0xa9) <0x14> <20 bytes> OP_EQUALVERIFY (0x88) OP_CHECKSIG (0xac)
  track(script[0] === 0x76, 'first byte should be OP_DUP');
  track(script[1] === 0xa9, 'second byte should be OP_HASH160');
  track(script[2] === 0x14, 'third byte should be push(20)');
  track(script[23] === 0x88, 'byte 23 should be OP_EQUALVERIFY');
  track(script[24] === 0xac, 'byte 24 should be OP_CHECKSIG');
  track(script.length === 25, 'total length should be 25');
}

// Test 7: addressToScript -> scriptToAddress round-trip
{
  const pubkey = Buffer.alloc(33, 0xab);
  const pubkey_hash = hash160(pubkey);
  const address = base58checkEncode(pubkey_hash, VERSIONS.mainnet.PUBKEY_ADDRESS);
  const script = addressToScript(address);
  const recoveredAddress = scriptToAddress(script, VERSIONS.mainnet.PUBKEY_ADDRESS);
  track(recoveredAddress === address, 'address round-trip failed');
}

// Test 8: scriptToAddress throws on non-P2PKH script
{
  const nonP2PKH = Buffer.alloc(10, 0xff);
  let threw = false;
  try {
    scriptToAddress(nonP2PKH, VERSIONS.mainnet.PUBKEY_ADDRESS);
  } catch (e) {
    threw = true;
  }
  track(threw, 'should throw on non-P2PKH script');
}

// Test 9: Load and test all fixture pairs from base58_encode_decode.json
{
  const __filename = fileURLToPath(import.meta.url);
  const __dirname = path.dirname(__filename);
  const fixturePath = path.join(__dirname, '../../src/test/data/base58_encode_decode.json');
  const fixtureData = JSON.parse(fs.readFileSync(fixturePath, 'utf8'));

  track(Array.isArray(fixtureData), 'fixture is an array');
  track(fixtureData.length >= 12, `fixture should have at least 12 pairs, got ${fixtureData.length}`);

  for (const [hexInput, expectedBase58] of fixtureData) {
    // Test encoding
    const bytes = fromHex(hexInput);
    const encoded = base58Encode(bytes);
    track(
      encoded === expectedBase58,
      `encode mismatch for ${hexInput}: got ${encoded}, expected ${expectedBase58}`
    );

    // Test decoding
    const decoded = base58Decode(expectedBase58);
    track(
      Buffer.from(decoded).equals(Buffer.from(bytes)),
      `decode mismatch for ${expectedBase58}: got ${hex(decoded)}, expected ${hexInput}`
    );
  }
}

// Test 10: Property test - base58 round-trip with random data (500+ random arrays, lengths 0-64)
{
  const rng = (seed) => {
    seed = (seed * 9301 + 49297) % 233280;
    return seed / 233280;
  };
  let seed = 42;
  const randomBytes = () => {
    const len = Math.floor(rng(++seed) * 65);
    const arr = new Uint8Array(len);
    for (let i = 0; i < len; i++) {
      arr[i] = Math.floor(rng(++seed) * 256);
    }
    return arr;
  };

  for (let i = 0; i < 500; i++) {
    const original = randomBytes();
    const encoded = base58Encode(original);
    const decoded = base58Decode(encoded);
    track(
      Buffer.from(decoded).equals(Buffer.from(original)),
      `round-trip failed on random iteration ${i}`
    );
  }
}

// Test 11: Property test - leading-zero behavior (0-8 leading zeros)
{
  for (let n = 0; n <= 8; n++) {
    const bytes = new Uint8Array(n + 1);
    bytes[n] = 0xFF; // one non-zero byte after the leading zeros
    const encoded = base58Encode(bytes);
    const leadingOnes = encoded.length - encoded.replace(/^1+/, '').length;
    track(
      leadingOnes === n,
      `${n} leading zeros should produce ${n} leading 1's, got ${leadingOnes} in "${encoded}"`
    );
  }
}

// Test 12: Property test - base58check round-trip with random payloads and version bytes
{
  const versions = [0x49, 0x71, 0x16, 0xc4];
  const rng = (seed) => {
    seed = (seed * 9301 + 49297) % 233280;
    return seed / 233280;
  };
  let seed = 100;

  for (let i = 0; i < 100; i++) {
    const len = Math.floor(rng(++seed) * 32) + 1;
    const payload = new Uint8Array(len);
    for (let j = 0; j < len; j++) {
      payload[j] = Math.floor(rng(++seed) * 256);
    }

    const version = versions[Math.floor(rng(++seed) * versions.length)];
    const encoded = base58checkEncode(payload, version);
    const decoded = base58checkDecode(encoded);

    track(
      decoded.version === version,
      `version mismatch in base58check property test iteration ${i}`
    );
    track(
      Buffer.from(decoded.payload).equals(Buffer.from(payload)),
      `payload mismatch in base58check property test iteration ${i}`
    );
  }
}

// Test 13: Corrupted checksum at multiple positions
{
  const payload = fromHex('deadbeefcafebabe');
  const version = 73;
  const chars = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

  // Test corruption at beginning, middle, and end
  for (const position of ['first', 'middle', 'last']) {
    let encoded = base58checkEncode(payload, version);
    let idx;
    if (position === 'first') {
      idx = 0;
    } else if (position === 'middle') {
      idx = Math.floor(encoded.length / 2);
    } else {
      idx = encoded.length - 1;
    }

    const oldChar = encoded[idx];
    let newChar = chars[0];
    while (newChar === oldChar) newChar = chars[1];
    encoded = encoded.slice(0, idx) + newChar + encoded.slice(idx + 1);

    let threw = false;
    try {
      base58checkDecode(encoded);
    } catch (e) {
      threw = true;
    }
    track(threw, `corrupted checksum at ${position} position should throw`);
  }
}

// Test 14: Invalid base58 characters throw on decode
{
  for (const invalidChar of ['0', 'O', 'I', 'l']) {
    let threw = false;
    try {
      base58Decode('1' + invalidChar + 'abc');
    } catch (e) {
      threw = true;
    }
    track(threw, `base58Decode should throw on invalid character "${invalidChar}"`);
  }
}

// Test 15: scriptToAddress throws on wrong script lengths
{
  const testLengths = [0, 1, 10, 24, 26, 100];
  for (const len of testLengths) {
    const script = Buffer.alloc(len);
    let threw = false;
    try {
      scriptToAddress(script, VERSIONS.mainnet.PUBKEY_ADDRESS);
    } catch (e) {
      threw = true;
    }
    track(threw, `scriptToAddress should throw on length ${len}`);
  }
}

// Test 16: scriptToAddress throws on wrong opcodes (length 25 but wrong opcodes)
{
  const wrongOpcodes = [
    // All zeros
    Buffer.alloc(25, 0x00),
    // Wrong first byte
    Buffer.alloc(25, 0x00).fill(0x75, 0, 1),
    // Wrong second byte
    Buffer.concat([
      Buffer.from([0x76, 0xa8]), // wrong second byte
      Buffer.alloc(23),
    ]),
    // Wrong third byte
    Buffer.concat([
      Buffer.from([0x76, 0xa9, 0x13]), // wrong push length
      Buffer.alloc(22),
    ]),
  ];

  for (const script of wrongOpcodes) {
    let threw = false;
    try {
      scriptToAddress(script, VERSIONS.mainnet.PUBKEY_ADDRESS);
    } catch (e) {
      threw = true;
    }
    track(threw, 'scriptToAddress should throw on wrong opcodes');
  }
}

// Test 17: Empty-input behavior
{
  // Empty bytes to base58
  const emptyEncoded = base58Encode(new Uint8Array());
  track(emptyEncoded === '', 'base58Encode of empty should be empty string');

  // Empty string from base58
  const emptyDecoded = base58Decode('');
  track(emptyDecoded.length === 0, 'base58Decode of empty should be empty');

  // Empty payload to base58check
  const emptyCheckEncoded = base58checkEncode(new Uint8Array(), 0);
  track(emptyCheckEncoded.length > 0, 'base58checkEncode of empty payload should produce a string');

  // Verify we can decode it
  const emptyCheckDecoded = base58checkDecode(emptyCheckEncoded);
  track(emptyCheckDecoded.version === 0, 'version should be 0 for empty payload');
  track(emptyCheckDecoded.payload.length === 0, 'payload should be empty');
}

// Test 18: Whippet-specific assertions - version byte constants
{
  track(VERSIONS.mainnet.PUBKEY_ADDRESS === 0x49, 'mainnet PUBKEY_ADDRESS should be 0x49 (73)');
  track(VERSIONS.mainnet.PUBKEY_ADDRESS === 73, 'mainnet PUBKEY_ADDRESS should be 73');

  track(VERSIONS.testnet.PUBKEY_ADDRESS === 0x71, 'testnet PUBKEY_ADDRESS should be 0x71 (113)');
  track(VERSIONS.testnet.PUBKEY_ADDRESS === 113, 'testnet PUBKEY_ADDRESS should be 113');

  track(VERSIONS.regtest.PUBKEY_ADDRESS === 0x49, 'regtest PUBKEY_ADDRESS should be 0x49 (73)');
  track(VERSIONS.mainnet.SCRIPT_ADDRESS === 0x16, 'mainnet SCRIPT_ADDRESS should be 0x16 (22)');
  track(VERSIONS.testnet.SCRIPT_ADDRESS === 0xc4, 'testnet SCRIPT_ADDRESS should be 0xc4 (196)');
}

// Test 19: Whippet-specific assertions - address prefix (mainnet version 73 -> 'W')
{
  const pubkey = fromHex('0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798');
  const h = hash160(pubkey);
  const address = base58checkEncode(h, VERSIONS.mainnet.PUBKEY_ADDRESS);
  track(address.startsWith('W'), `mainnet address should start with W, got ${address}`);

  const expectedAddress = 'WZMJS5eeCEgiqxNK1QRbE1HR4wFQwmCjJV';
  track(
    address === expectedAddress,
    `known pubkey should produce expected mainnet address: got ${address}, expected ${expectedAddress}`
  );
}

// Test 20: Whippet-specific assertions - address prefix (testnet version 113 -> 'n')
{
  const pubkey = fromHex('0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798');
  const h = hash160(pubkey);
  const address = base58checkEncode(h, VERSIONS.testnet.PUBKEY_ADDRESS);
  track(address.startsWith('n'), `testnet address should start with n, got ${address}`);

  const expectedAddress = 'nesRpRaAbTDmZHwmzBkLd2AtF7Z9L9z5S2';
  track(
    address === expectedAddress,
    `known pubkey should produce expected testnet address: got ${address}, expected ${expectedAddress}`
  );
}

// Test 21: Whippet-specific - P2PKH script round-trip with reference values
{
  const pubkey = fromHex('0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798');
  const script = buildP2PKHScript(pubkey);
  const expectedScript = fromHex('76a914751e76e8199196d454941c45d1b3a323f1433bd688ac');

  track(
    Buffer.from(script).equals(Buffer.from(expectedScript)),
    `P2PKH script mismatch: got ${hex(script)}, expected ${hex(expectedScript)}`
  );
}

// Test 22: base58checkEncode with specific payload and version matches reference
{
  const payload = fromHex('000001');
  const version = 0;
  const encoded = base58checkEncode(payload, version);
  const expectedEncoded = '111E1CgqW';

  track(
    encoded === expectedEncoded,
    `base58checkEncode([0x00,0x00,0x01], 0) should produce "${expectedEncoded}", got "${encoded}"`
  );
}

// Test 23: addressToScript refuses a P2SH address instead of mis-encoding it
//
// A P2SH address decodes to a 20-byte *script* hash. Emitting the P2PKH
// template for it would produce an output payable only by whoever holds the
// private key for a hash160 that is, by construction, nobody's pubkey hash
// -- i.e. permanently unspendable funds. The failure must be a throw, and
// no script may come back.
{
  const scriptHash = hash160(fromUtf8('some redeem script'));

  for (const [network, versions] of Object.entries(VERSIONS)) {
    const p2shAddress = base58checkEncode(scriptHash, versions.SCRIPT_ADDRESS);

    let threw = false;
    let produced = null;
    try {
      produced = addressToScript(p2shAddress);
    } catch (e) {
      threw = true;
      track(
        /P2PKH|pay-to-pubkey-hash/.test(e.message),
        `${network}: throw should explain that only P2PKH is supported, got "${e.message}"`
      );
    }

    track(threw, `${network}: addressToScript must throw on a P2SH address, not return a script`);
    track(
      produced === null,
      `${network}: addressToScript must not produce any script for a P2SH address (got ${produced ? hex(produced) : 'null'})`
    );
  }
}

// Test 24: addressToScript still accepts every network's P2PKH version, and
// the resulting script really does commit to the address's own hash160.
{
  const pubkey = Buffer.alloc(33, 0xcd);
  const pkh = hash160(pubkey);

  for (const [network, versions] of Object.entries(VERSIONS)) {
    const address = base58checkEncode(pkh, versions.PUBKEY_ADDRESS);
    const script = addressToScript(address);
    track(script.length === 25, `${network}: P2PKH script should be 25 bytes, got ${script.length}`);
    track(
      Buffer.from(script.slice(3, 23)).equals(Buffer.from(pkh)),
      `${network}: script must embed the address's own hash160`
    );
  }
}

// Test 25: addressToScript rejects a wrong-length payload under a valid
// version byte, rather than emitting a malformed "P2PKH" script whose
// PUSH(20) does not match the bytes that follow it.
{
  const shortPayload = fromHex('0102030405');
  const address = base58checkEncode(shortPayload, VERSIONS.mainnet.PUBKEY_ADDRESS);
  let threw = false;
  try {
    addressToScript(address);
  } catch (e) {
    threw = true;
    track(/20-byte/.test(e.message), `throw should name the expected payload size, got "${e.message}"`);
  }
  track(threw, 'addressToScript must throw on a non-20-byte payload');
}

console.log(`addr.test.js: ${assertCount} assertions passed`);
