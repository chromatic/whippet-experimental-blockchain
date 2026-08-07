// wallet.js - Self-custody wallet for Whippet
// Key generation, BIP39 mnemonic, PBKDF2+AES-GCM encryption, storage abstraction
// Uses the vendored @noble/secp256k1 build (../uap-js/secp.js) for key derivation

import { pubkeyToAddress, VERSIONS } from '../uap-js/addr.js';
import { sha256, hash256, hmacSha256 } from '../uap-js/sha256.js';
import { WORDLIST } from './wordlist.js';
import * as secp256k1 from '../uap-js/secp.js';

const PBKDF2_ITERATIONS = 600000; // High iteration count resists brute force
const AES_KEY_LENGTH = 32; // 256-bit AES
const GCM_TAG_LENGTH = 16;
const GCM_NONCE_LENGTH = 12;

// BIP39 checksum calculation
function calculateBIP39Checksum(entropy) {
  const hash = sha256(entropy);
  const checksumBits = entropy.length * 8 / 32; // 4 bits per 32 bits of entropy
  const checksumBytes = Math.ceil(checksumBits / 8);
  return { bytes: hash.slice(0, checksumBytes), bits: checksumBits };
}

// Convert entropy to BIP39 mnemonic
function entropyToMnemonic(entropy) {
  const checksumInfo = calculateBIP39Checksum(entropy);
  const checksumBits = checksumToBits(checksumInfo.bytes).substring(0, checksumInfo.bits);
  const bits = entropyToBits(entropy) + checksumBits;

  const mnemonicIndices = [];
  for (let i = 0; i < bits.length; i += 11) {
    const chunk = bits.substring(i, i + 11);
    mnemonicIndices.push(parseInt(chunk, 2));
  }

  return mnemonicIndices.map(i => WORDLIST[i]).join(' ');
}

// Parse BIP39 mnemonic and validate checksum
function mnemonicToEntropy(mnemonic) {
  const words = mnemonic.trim().split(/\s+/);

  if (words.length !== 12 && words.length !== 24) {
    throw new Error('Mnemonic must be 12 or 24 words');
  }

  const indices = [];
  const wordSet = new Set(WORDLIST);

  for (const word of words) {
    if (!wordSet.has(word)) {
      throw new Error(`invalid word: "${word}" not in BIP39 wordlist`);
    }
    const idx = WORDLIST.indexOf(word);
    indices.push(idx);
  }

  const bits = indices.map(i => i.toString(2).padStart(11, '0')).join('');
  const entropyBits = words.length === 12 ? 128 : 256;
  const entropyBitString = bits.substring(0, entropyBits);
  const checksumBitString = bits.substring(entropyBits);

  const entropy = bitsToEntropy(entropyBitString);
  const checksumInfo = calculateBIP39Checksum(entropy);
  const expectedChecksumBits = checksumToBits(checksumInfo.bytes).substring(0, checksumInfo.bits);

  if (checksumBitString !== expectedChecksumBits) {
    throw new Error('BIP39 checksum validation failed');
  }

  return entropy;
}

function entropyToBits(entropy) {
  return Array.from(entropy).map(b => b.toString(2).padStart(8, '0')).join('');
}

function checksumToBits(checksum) {
  return Array.from(checksum).map(b => b.toString(2).padStart(8, '0')).join('');
}

function bitsToEntropy(bits) {
  const bytes = [];
  for (let i = 0; i < bits.length; i += 8) {
    bytes.push(parseInt(bits.substring(i, i + 8), 2));
  }
  return new Uint8Array(bytes);
}

// Simplified PBKDF2-SHA256 using hmacSha256
function pbkdf2Sync(password, salt, iterations, keyLength) {
  const result = new Uint8Array(keyLength);
  const blockSize = 32; // SHA256 output length
  const numBlocks = Math.ceil(keyLength / blockSize);

  for (let blockIndex = 1; blockIndex <= numBlocks; blockIndex++) {
    const counter = new Uint8Array(4);
    new DataView(counter.buffer).setUint32(0, blockIndex, false);

    let u = hmacSha256(password, salt, counter);
    let block = new Uint8Array(u);

    for (let i = 1; i < iterations; i++) {
      u = hmacSha256(password, u);
      for (let j = 0; j < block.length; j++) {
        block[j] ^= u[j];
      }
    }

    const copyLength = Math.min(blockSize, keyLength - (blockIndex - 1) * blockSize);
    result.set(block.slice(0, copyLength), (blockIndex - 1) * blockSize);
  }

  return result;
}

// BIP39 seed derivation using PBKDF2 with SHA-256 (simplified)
function mnemonicToSeed(mnemonic, passphrase = '') {
  const password = new TextEncoder().encode(mnemonic);
  const salt = new TextEncoder().encode('mnemonic' + passphrase);

  return pbkdf2Sync(password, salt, 2048, 64);
}

// Derive private key from seed (simple: take first 32 bytes)
function seedToPrivateKey(seed) {
  return seed.slice(0, 32);
}

// Get compressed public key from private key using secp256k1
function getPublicKeyFromPrivateKey(privKey) {
  const pubKey = secp256k1.getPublicKey(privKey, true); // compressed=true
  return pubKey;
}

// Derive address from private key
function deriveAddressFromPrivKey(privKey, network) {
  const pubKey = getPublicKeyFromPrivateKey(privKey);
  const versionByte = VERSIONS[network]?.PUBKEY_ADDRESS;
  if (!versionByte) {
    throw new Error(`Unknown network: ${network}`);
  }
  return pubkeyToAddress(pubKey, versionByte);
}

// Format: { algo: 'pbkdf2-aes-gcm', iterations: 600000, salt: hex, nonce: hex, ciphertext: hex, tag: hex }
async function encryptPrivateKey(privKey, passphrase, cryptoSubtle) {
  if (!cryptoSubtle) throw new Error('crypto.subtle unavailable');

  const salt = globalThis.crypto.getRandomValues(new Uint8Array(16));
  const nonce = globalThis.crypto.getRandomValues(new Uint8Array(GCM_NONCE_LENGTH));

  const password = new TextEncoder().encode(passphrase);
  const keyMaterial = await cryptoSubtle.importKey(
    'raw',
    password,
    { name: 'PBKDF2' },
    false,
    ['deriveBits']
  );

  const derivedKeyBits = await cryptoSubtle.deriveBits(
    {
      name: 'PBKDF2',
      hash: 'SHA-256',
      salt: salt,
      iterations: PBKDF2_ITERATIONS
    },
    keyMaterial,
    AES_KEY_LENGTH * 8
  );

  const encryptionKey = await cryptoSubtle.importKey(
    'raw',
    derivedKeyBits,
    { name: 'AES-GCM' },
    false,
    ['encrypt']
  );

  const encrypted = await cryptoSubtle.encrypt(
    { name: 'AES-GCM', iv: nonce, tagLength: GCM_TAG_LENGTH * 8 },
    encryptionKey,
    privKey
  );

  // encrypted includes the tag appended at the end
  const encryptedArray = new Uint8Array(encrypted);
  const ciphertext = encryptedArray.slice(0, privKey.length);
  const tag = encryptedArray.slice(privKey.length);

  return {
    algo: 'pbkdf2-aes-gcm',
    iterations: PBKDF2_ITERATIONS,
    salt: bytesToHex(salt),
    nonce: bytesToHex(nonce),
    ciphertext: bytesToHex(ciphertext),
    tag: bytesToHex(tag)
  };
}

async function decryptPrivateKey(encryptedData, passphrase, cryptoSubtle) {
  if (!cryptoSubtle) throw new Error('crypto.subtle unavailable');
  if (!encryptedData || encryptedData.algo !== 'pbkdf2-aes-gcm') {
    throw new Error('invalid encrypted data format');
  }

  const salt = hexToBytes(encryptedData.salt);
  const nonce = hexToBytes(encryptedData.nonce);
  const ciphertext = hexToBytes(encryptedData.ciphertext);
  const tag = hexToBytes(encryptedData.tag);

  const password = new TextEncoder().encode(passphrase);
  const keyMaterial = await cryptoSubtle.importKey(
    'raw',
    password,
    { name: 'PBKDF2' },
    false,
    ['deriveBits']
  );

  const derivedKeyBits = await cryptoSubtle.deriveBits(
    {
      name: 'PBKDF2',
      hash: 'SHA-256',
      salt: salt,
      iterations: encryptedData.iterations || PBKDF2_ITERATIONS
    },
    keyMaterial,
    AES_KEY_LENGTH * 8
  );

  const decryptionKey = await cryptoSubtle.importKey(
    'raw',
    derivedKeyBits,
    { name: 'AES-GCM' },
    false,
    ['decrypt']
  );

  try {
    const decrypted = await cryptoSubtle.decrypt(
      { name: 'AES-GCM', iv: nonce, tagLength: GCM_TAG_LENGTH * 8 },
      decryptionKey,
      new Uint8Array([...ciphertext, ...tag])
    );
    return new Uint8Array(decrypted);
  } catch (e) {
    return null; // Wrong passphrase - authentication failed
  }
}

function bytesToHex(bytes) {
  return Array.from(bytes).map(b => b.toString(16).padStart(2, '0')).join('');
}

function hexToBytes(hex) {
  const bytes = [];
  for (let i = 0; i < hex.length; i += 2) {
    bytes.push(parseInt(hex.substr(i, 2), 16));
  }
  return new Uint8Array(bytes);
}

/**
 * A self-custody wallet.
 *
 * Persistence rule: storage holds the AES-GCM ciphertext of the mnemonic and
 * nothing else that is secret. In particular the mnemonic is NEVER written in
 * the clear -- it derives the private key, so persisting it unencrypted would
 * make the encryption decorative. That means:
 *
 *   - Creating a wallet writes nothing at all. The mnemonic exists only in
 *     memory, for the user to write down. If they close the tab without
 *     locking, the wallet is gone -- which is the correct outcome, because the
 *     alternative is a key sitting in localStorage for any script to read.
 *   - lock(passphrase) is the only thing that persists, and it clears both the
 *     private key and the mnemonic from memory afterwards.
 *   - The address is stored in the clear on purpose: it is public, and it lets
 *     a locked wallet show the user which account they are looking at.
 */
class Wallet {
  constructor(options = {}) {
    const {
      storage = new Map(),
      restore = false,
      mnemonic = null,
      network = 'mainnet',
      mnemonicLength = 12
    } = options;

    // Deliberately resolved by presence rather than as a default parameter.
    // A default fires whenever the value is `undefined`, which makes
    // `{ cryptoSubtle: undefined }` -- "I am telling you there is no
    // SubtleCrypto" -- indistinguishable from omitting the option entirely.
    // That silently fell back to the real global, so the guard below could
    // never be reached by a caller trying to signal an insecure context.
    const cryptoSubtle = 'cryptoSubtle' in options
      ? options.cryptoSubtle
      : globalThis.crypto?.subtle;

    this.storage = storage;
    this.network = network;
    this.cryptoSubtle = cryptoSubtle;
    this._privKey = null;
    this._address = null;
    this._mnemonic = null;
    this._locked = false;

    if (!this.cryptoSubtle) {
      throw new Error(
        'crypto.subtle is unavailable. This wallet requires a secure context (HTTPS or localhost).'
      );
    }

    if (restore) {
      const stored = this.storage.get('wallet');
      if (!stored) {
        throw new Error('No wallet found in storage');
      }
      if (!stored.encrypted) {
        // Nothing here can produce a key. Refuse rather than hand back a
        // half-initialised wallet.
        throw new Error('Stored wallet has no encrypted key material');
      }
      // Only public metadata is readable while locked.
      this._address = stored.address || null;
      this.network = stored.network || network;
      this._locked = true;
    } else if (mnemonic) {
      this._initializeFromMnemonic(mnemonic);
    } else {
      this._generateNewWallet(mnemonicLength);
    }
  }

  _generateNewWallet(mnemonicLength) {
    const entropyLength = mnemonicLength === 24 ? 32 : 16;
    const entropy = globalThis.crypto.getRandomValues(new Uint8Array(entropyLength));
    this._mnemonic = entropyToMnemonic(entropy);
    this._deriveFromMnemonic();
  }

  _initializeFromMnemonic(mnemonic) {
    mnemonicToEntropy(mnemonic); // throws if the phrase or its checksum is bad
    this._mnemonic = mnemonic;
    this._deriveFromMnemonic();
  }

  // Derivation only. Deliberately does not touch storage: see the class note.
  _deriveFromMnemonic() {
    const seed = mnemonicToSeed(this._mnemonic);
    this._privKey = seedToPrivateKey(seed);
    this._address = deriveAddressFromPrivKey(this._privKey, this.network);
  }

  get mnemonic() {
    return this._mnemonic;
  }

  get privKey() {
    return this._privKey;
  }

  get address() {
    return this._address;
  }

  get locked() {
    return this._locked;
  }

  /** True once this wallet exists in storage in encrypted form. */
  get persisted() {
    const stored = this.storage.get('wallet');
    return Boolean(stored && stored.encrypted);
  }

  /**
   * Encrypt the mnemonic under `passphrase` and persist it, then drop all key
   * material from memory. The mnemonic (not the private key) is what gets
   * encrypted, so unlocking can re-derive everything and the user's written-
   * down backup and the stored copy can never disagree.
   */
  async lock(passphrase) {
    if (!this._mnemonic) throw new Error('No mnemonic available to lock');
    if (!passphrase) throw new Error('A passphrase is required to lock the wallet');

    const encrypted = await encryptPrivateKey(
      new TextEncoder().encode(this._mnemonic), passphrase, this.cryptoSubtle
    );

    this.storage.set('wallet', {
      version: 2,
      encrypted,
      iterations: PBKDF2_ITERATIONS,
      address: this._address,   // public; lets a locked wallet name itself
      network: this.network
    });

    this._privKey = null;
    this._mnemonic = null;
    this._locked = true;
  }

  /**
   * Decrypt the stored mnemonic and re-derive the key and address.
   * Returns false (without changing any state) if the passphrase is wrong.
   */
  async unlock(passphrase) {
    const stored = this.storage.get('wallet');
    if (!stored || !stored.encrypted) {
      throw new Error('No encrypted wallet in storage');
    }

    const plaintext = await decryptPrivateKey(stored.encrypted, passphrase, this.cryptoSubtle);
    if (!plaintext) {
      return false; // wrong passphrase; GCM tag did not verify
    }

    const mnemonic = new TextDecoder().decode(plaintext);
    mnemonicToEntropy(mnemonic); // stored ciphertext must decrypt to a real phrase
    this.network = stored.network || this.network;
    this._mnemonic = mnemonic;
    this._deriveFromMnemonic();

    // The stored address is unauthenticated (it sits outside the GCM tag), so
    // treat a mismatch as tampering rather than silently adopting either one.
    if (stored.address && stored.address !== this._address) {
      this._privKey = null;
      this._mnemonic = null;
      throw new Error('Stored address does not match the decrypted key; storage may be corrupt');
    }

    this._locked = false;
    return true;
  }
}

export { Wallet };
