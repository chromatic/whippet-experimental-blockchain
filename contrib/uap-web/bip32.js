// BIP39 seed derivation and BIP32 hardened child derivation.
//
// This replaces an earlier scheme that called itself BIP39 and was not:
// it ran PBKDF2 over HMAC-SHA256 instead of the mandated HMAC-SHA512, and
// took the private key as the first 32 bytes of the seed rather than
// deriving through BIP32 at all. Because the mnemonic itself was a valid
// BIP39 mnemonic, another wallet would accept those words and silently
// produce different keys -- the user would see an empty wallet and no
// error. Anything claiming to be a standard has to actually be one.
//
// All hashing goes through WebCrypto. SHA-512 is not hand-rolled here,
// and neither is PBKDF2: the wallet already requires crypto.subtle for
// its AES-GCM at-rest encryption, so depending on it costs nothing new.
//
// Only HARDENED derivation is implemented, because that is all the
// Whippet path needs. Hardened children are a pure hash of the parent
// PRIVATE key, so no elliptic-curve point arithmetic appears below.

// secp256k1 group order.
const N = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;

const HARDENED = 0x80000000;

// whippetd derives m/0'/3'/n' -- see src/wallet/wallet.cpp, which walks
// m/0', then m/0'/3' using BIP44_COIN_TYPE = 3, then a hardened child.
// The browser wallet uses the first such key so that a mnemonic backed up
// here means something to the core wallet too.
const WHIPPET_KEY_PATH = "m/0'/3'/0'";
const WHIPPET_KEY_INDICES = [0, 3, 0];

function subtle() {
  const s = globalThis.crypto && globalThis.crypto.subtle;
  if (!s) {
    throw new Error('crypto.subtle is unavailable. This wallet requires a secure context (HTTPS or localhost).');
  }
  return s;
}

/**
 * BIP39: mnemonic + passphrase -> 64-byte seed.
 *
 * PBKDF2-HMAC-SHA512, 2048 iterations, salt "mnemonic" + passphrase, as
 * the specification requires. Both inputs are NFKD-normalised, which
 * matters for any passphrase outside ASCII: the same characters typed on
 * a different platform must produce the same seed.
 */
async function mnemonicToSeed(mnemonic, passphrase = '') {
  const enc = new TextEncoder();
  const key = await subtle().importKey(
    'raw',
    enc.encode(mnemonic.normalize('NFKD')),
    { name: 'PBKDF2' },
    false,
    ['deriveBits'],
  );
  const bits = await subtle().deriveBits(
    {
      name: 'PBKDF2',
      salt: enc.encode(('mnemonic' + passphrase).normalize('NFKD')),
      iterations: 2048,
      hash: 'SHA-512',
    },
    key,
    512,
  );
  return new Uint8Array(bits);
}

// HMAC-SHA512 via WebCrypto. There is deliberately no synchronous
// fallback and no vendored SHA-512: one implementation, already audited
// by the platform, is worth more than the convenience of a sync API.
// Derivation is therefore async all the way down, which costs nothing --
// every caller was already async to reach crypto.subtle.
async function hmacSha512(keyBytes, data) {
  const key = await subtle().importKey(
    'raw',
    keyBytes,
    { name: 'HMAC', hash: 'SHA-512' },
    false,
    ['sign'],
  );
  return new Uint8Array(await subtle().sign('HMAC', key, data));
}

function bytesToBigInt(b) {
  let n = 0n;
  for (const byte of b) n = (n << 8n) | BigInt(byte);
  return n;
}

function bigIntTo32Bytes(n) {
  const out = new Uint8Array(32);
  for (let i = 31; i >= 0; i--) {
    out[i] = Number(n & 0xffn);
    n >>= 8n;
  }
  return out;
}

/**
 * BIP32 master key: HMAC-SHA512("Bitcoin seed", seed).
 * Left half is the private key, right half the chain code.
 */
async function masterKeyFromSeed(seed) {
  const I = await hmacSha512(new TextEncoder().encode('Bitcoin seed'), seed);
  const key = I.slice(0, 32);
  const k = bytesToBigInt(key);
  if (k === 0n || k >= N) {
    // Probability ~2^-127. The spec says such a seed is invalid rather
    // than something to work around.
    throw new Error('invalid seed: master key out of range');
  }
  return { key, chainCode: I.slice(32, 64) };
}

/**
 * BIP32 hardened child derivation (CKDpriv with i >= 2^31).
 *
 * I = HMAC-SHA512(cpar, 0x00 || ser256(kpar) || ser32(i + 2^31))
 * child key = (IL + kpar) mod n
 */
async function deriveHardened(parent, index) {
  if (!Number.isInteger(index) || index < 0 || index >= HARDENED) {
    throw new Error(`hardened index out of range: ${index}`);
  }
  const i = (index + HARDENED) >>> 0;

  const data = new Uint8Array(1 + 32 + 4);
  data[0] = 0x00;
  data.set(parent.key, 1);
  data[33] = (i >>> 24) & 0xff;
  data[34] = (i >>> 16) & 0xff;
  data[35] = (i >>> 8) & 0xff;
  data[36] = i & 0xff;

  const I = await hmacSha512(parent.chainCode, data);
  const IL = bytesToBigInt(I.slice(0, 32));
  if (IL >= N) {
    throw new Error('derived scalar out of range; use the next index');
  }
  const childKey = (IL + bytesToBigInt(parent.key)) % N;
  if (childKey === 0n) {
    throw new Error('derived key is zero; use the next index');
  }
  return { key: bigIntTo32Bytes(childKey), chainCode: I.slice(32, 64) };
}

/**
 * Walk a list of hardened indices from a seed. Every step is hardened;
 * there is no non-hardened form here because the Whippet path has none.
 */
async function derivePath(seed, indices) {
  let node = await masterKeyFromSeed(seed);
  for (const i of indices) node = await deriveHardened(node, i);
  return node;
}

/** The wallet's private key for a mnemonic: BIP39 seed, then m/0'/3'/0'. */
async function mnemonicToPrivateKey(mnemonic, passphrase = '') {
  const seed = await mnemonicToSeed(mnemonic, passphrase);
  return (await derivePath(seed, WHIPPET_KEY_INDICES)).key;
}

export {
  mnemonicToSeed,
  masterKeyFromSeed,
  deriveHardened,
  derivePath,
  mnemonicToPrivateKey,
  WHIPPET_KEY_PATH,
  WHIPPET_KEY_INDICES,
};
