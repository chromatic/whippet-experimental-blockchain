// Address utilities: base58check, P2PKH script building and parsing.
// Browser-compatible, dependency-free.

import { sha256, hash256, concatBytes } from './sha256.js';
import { ripemd160 } from './ripemd160.js';

const ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

/**
 * hash160 = RIPEMD160(SHA256(bytes))
 * @param {Uint8Array} bytes
 * @returns {Uint8Array} 20-byte hash
 */
function hash160(bytes) {
  return ripemd160(sha256(bytes));
}

/**
 * Encode bytes to base58 string.
 * Leading zero bytes become leading '1' characters.
 * @param {Uint8Array} bytes
 * @returns {string}
 */
function base58Encode(bytes) {
  // Count and record leading zeros
  let leadingZeros = 0;
  for (const b of bytes) {
    if (b === 0) leadingZeros++;
    else break;
  }

  // Convert bytes to BigInt
  let num = 0n;
  for (const b of bytes) {
    num = (num << 8n) | BigInt(b);
  }

  // Convert to base58
  let encoded = '';
  if (num === 0n && bytes.length > 0) {
    // All zeros — just prepend '1's
  } else if (num === 0n) {
    encoded = '';
  } else {
    while (num > 0n) {
      encoded = ALPHABET[Number(num % 58n)] + encoded;
      num = num / 58n;
    }
  }

  return '1'.repeat(leadingZeros) + encoded;
}

/**
 * Decode base58 string to bytes.
 * Leading '1' characters become leading zero bytes.
 * @param {string} str
 * @returns {Uint8Array}
 */
function base58Decode(str) {
  // Count leading 1's
  let leadingOnes = 0;
  for (const c of str) {
    if (c === '1') leadingOnes++;
    else break;
  }

  // Convert from base58 to BigInt
  let num = 0n;
  for (let i = leadingOnes; i < str.length; i++) {
    const digit = ALPHABET.indexOf(str[i]);
    if (digit === -1) throw new Error(`invalid base58 character: ${str[i]}`);
    num = num * 58n + BigInt(digit);
  }

  // Convert to bytes
  const bytes = [];
  while (num > 0n) {
    bytes.unshift(Number(num & 0xffn));
    num = num >> 8n;
  }

  const decoded = new Uint8Array(leadingOnes + bytes.length);
  for (let i = 0; i < bytes.length; i++) {
    decoded[leadingOnes + i] = bytes[i];
  }
  return decoded;
}

/**
 * Encode payload with version byte and checksum.
 * @param {Uint8Array} payload
 * @param {number} version
 * @returns {string} base58check-encoded string
 */
function base58checkEncode(payload, version) {
  const versionByte = new Uint8Array([version]);
  const toHash = concatBytes(versionByte, payload);
  const checksum = hash256(toHash);
  const withChecksum = concatBytes(toHash, checksum.slice(0, 4));
  return base58Encode(withChecksum);
}

/**
 * Decode base58check string.
 * @param {string} str
 * @returns {{version: number, payload: Uint8Array}}
 * @throws if checksum is invalid
 */
function base58checkDecode(str) {
  const decoded = base58Decode(str);
  if (decoded.length < 5) throw new Error('base58check string too short');

  const version = decoded[0];
  const payload = decoded.slice(1, -4);
  const checksum = decoded.slice(-4);

  const toHash = concatBytes(new Uint8Array([version]), payload);
  const expectedChecksum = hash256(toHash).slice(0, 4);

  let checksumOk = true;
  for (let i = 0; i < 4; i++) {
    if (checksum[i] !== expectedChecksum[i]) checksumOk = false;
  }
  if (!checksumOk) throw new Error('invalid base58check checksum');

  return { version, payload };
}

/**
 * Build a P2PKH script: OP_DUP OP_HASH160 <hash160> OP_EQUALVERIFY OP_CHECKSIG
 * @param {Uint8Array} pubkey
 * @returns {Uint8Array}
 */
function buildP2PKHScript(pubkey) {
  const h = hash160(pubkey);
  return concatBytes(
    new Uint8Array([0x76, 0xa9, 0x14]), // OP_DUP OP_HASH160 PUSH(20)
    h,
    new Uint8Array([0x88, 0xac])        // OP_EQUALVERIFY OP_CHECKSIG
  );
}

/**
 * Derive a P2PKH address from a public key.
 * @param {Uint8Array} pubkey
 * @param {number} version
 * @returns {string}
 */
function pubkeyToAddress(pubkey, version) {
  const h = hash160(pubkey);
  return base58checkEncode(h, version);
}

/**
 * Convert an address string to its scriptPubKey.
 *
 * ONLY pay-to-pubkey-hash addresses are supported, and the version byte is
 * checked rather than discarded. This used to decode the address, throw the
 * version byte away, and emit a P2PKH script unconditionally -- which meant
 * a perfectly valid P2SH address (mainnet 0x16, testnet/regtest 0xc4) was
 * silently re-encoded as a payment to a *pubkey* hash that equals someone's
 * *script* hash. Nobody holds a key for that hash, so those funds would be
 * permanently unspendable. Nothing in this repo builds or spends P2SH, so
 * the honest answer is to refuse: a throw here is loud and recoverable,
 * whereas the previous silent mis-encoding was neither.
 *
 * If P2SH support is ever wanted, add an explicit `OP_HASH160 <hash>
 * OP_EQUAL` branch here -- do not relax the check.
 *
 * @param {string} address
 * @returns {Uint8Array} a 25-byte P2PKH scriptPubKey
 * @throws if the address is not a base58check P2PKH address for a known network
 */
function addressToScript(address) {
  const { version, payload } = base58checkDecode(address);

  const p2pkhVersions = Object.values(VERSIONS).map((v) => v.PUBKEY_ADDRESS);
  if (!p2pkhVersions.includes(version)) {
    throw new Error(
      `addressToScript: refusing to encode address version byte 0x${version.toString(16).padStart(2, '0')} ` +
      'as a P2PKH output. Only pay-to-pubkey-hash addresses are supported; ' +
      'a P2SH (or otherwise unknown) address encoded this way would be unspendable.'
    );
  }
  if (payload.length !== 20) {
    throw new Error(`addressToScript: expected a 20-byte hash160 payload, got ${payload.length} bytes`);
  }

  // payload is the hash160, build script directly (don't hash again)
  return concatBytes(
    new Uint8Array([0x76, 0xa9, 0x14]), // OP_DUP OP_HASH160 PUSH(20)
    payload,
    new Uint8Array([0x88, 0xac])        // OP_EQUALVERIFY OP_CHECKSIG
  );
}

/**
 * Extract the address from a P2PKH script.
 * @param {Uint8Array} script
 * @param {number} version
 * @returns {string}
 * @throws if script is not P2PKH-shaped
 */
function scriptToAddress(script, version) {
  // P2PKH script: OP_DUP OP_HASH160 <0x14> <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
  if (script.length !== 25) throw new Error('script length is not 25');
  if (script[0] !== 0x76) throw new Error('script does not start with OP_DUP');
  if (script[1] !== 0xa9) throw new Error('script does not have OP_HASH160');
  if (script[2] !== 0x14) throw new Error('script does not have PUSH(20)');
  if (script[23] !== 0x88) throw new Error('script does not have OP_EQUALVERIFY');
  if (script[24] !== 0xac) throw new Error('script does not end with OP_CHECKSIG');

  const hash = script.slice(3, 23);
  return base58checkEncode(hash, version);
}

const VERSIONS = {
  mainnet: {
    PUBKEY_ADDRESS: 0x49,  // 73
    SCRIPT_ADDRESS: 0x16,  // 22
    SECRET_KEY: 0x9e,      // 158
  },
  testnet: {
    PUBKEY_ADDRESS: 0x71,  // 113
    SCRIPT_ADDRESS: 0xc4,  // 196
    SECRET_KEY: 0xf1,      // 241
  },
  regtest: {
    PUBKEY_ADDRESS: 0x49,  // 73
    SCRIPT_ADDRESS: 0xc4,  // 196
    SECRET_KEY: 0xef,      // 239
  },
};

export {
  hash160,
  base58Encode,
  base58Decode,
  base58checkEncode,
  base58checkDecode,
  buildP2PKHScript,
  pubkeyToAddress,
  addressToScript,
  scriptToAddress,
  VERSIONS,
};
