// Wallet tests - strict red/green TDD
// All logic tested headlessly in Node.js
import { Wallet } from './wallet.js';
import { mnemonicToPrivateKey } from './bip32.js';
import { WORDLIST } from './wordlist.js';
import { test, assert, assertEqual, assertArrayEquals, assertThrows, run } from './test-harness.js';

// =============================================================================
// WORDLIST INVARIANTS
// =============================================================================

test('wordlist has exactly 2048 words', async () => {
  assertEqual(WORDLIST.length, 2048, 'wordlist length');
});

test('all wordlist entries are lowercase', async () => {
  const invalid = WORDLIST.filter(w => w !== w.toLowerCase());
  assert(invalid.length === 0, `found non-lowercase words: ${invalid.join(', ')}`);
});

test('all wordlist entries are unique', async () => {
  const set = new Set(WORDLIST);
  assertEqual(set.size, 2048, 'wordlist uniqueness');
});

test('wordlist is alphabetically sorted', async () => {
  const sorted = [...WORDLIST].sort();
  for (let i = 0; i < WORDLIST.length; i++) {
    assertEqual(WORDLIST[i], sorted[i], `word ${i} ordering`);
  }
});

test('first 4 characters of each word are unique', async () => {
  const first4 = WORDLIST.map(w => w.slice(0, 4));
  const set = new Set(first4);
  assertEqual(set.size, 2048, 'first 4 char uniqueness');
});

// =============================================================================
// WALLET CREATION & KEY GENERATION
// =============================================================================

test('wallet creation fails if crypto.subtle is unavailable', async () => {
  const storage = new Map();
  await assertThrows(
    async () => await Wallet.create({ storage, cryptoSubtle: undefined }),
    'crypto.subtle'
  );
});

test('wallet can be created with 12-word mnemonic', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  assert(wallet.mnemonic, 'wallet has mnemonic');
  const words = wallet.mnemonic.split(' ');
  assertEqual(words.length, 12, 'mnemonic word count');
});

test('wallet can be created with 24-word mnemonic', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage, mnemonicLength: 24 });
  assert(wallet.mnemonic, 'wallet has mnemonic');
  const words = wallet.mnemonic.split(' ');
  assertEqual(words.length, 24, 'mnemonic word count');
});

test('each created wallet has a unique mnemonic', async () => {
  const w1 = await Wallet.create({ storage: new Map() });
  const w2 = await Wallet.create({ storage: new Map() });
  assert(w1.mnemonic !== w2.mnemonic, 'mnemonics are different');
});

test('mnemonic consists of valid wordlist entries', async () => {
  const wallet = await Wallet.create({ storage: new Map() });
  const words = wallet.mnemonic.split(' ');
  const wordSet = new Set(WORDLIST);
  for (const word of words) {
    assert(wordSet.has(word), `"${word}" not in wordlist`);
  }
});

test('wallet has an address after creation', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  assert(wallet.address, 'wallet has address');
  assert(wallet.address.startsWith('W'), 'mainnet address starts with W');
});

// =============================================================================
// ENCRYPTION & DECRYPTION
// =============================================================================

test('encrypt then decrypt recovers the exact key bytes', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  const passphrase = 'test-passphrase-12345';

  const privKeyBefore = wallet.privKey;
  await wallet.lock(passphrase);

  const walletRestored = new Wallet({ storage, restore: true });
  const unlockedOk = await walletRestored.unlock(passphrase);
  assert(unlockedOk, 'unlock succeeded');

  assertArrayEquals(walletRestored.privKey, privKeyBefore, 'private key matches');
});

test('wrong passphrase fails cleanly', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  await wallet.lock('correct-passphrase');

  const walletRestored = new Wallet({ storage, restore: true });
  const unlockedOk = await walletRestored.unlock('wrong-passphrase');
  assert(!unlockedOk, 'unlock failed as expected');
  assert(!walletRestored.privKey, 'no key material available after failed unlock');
});

test('storage contains no plaintext private key after lock', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  const privKeyHex = Array.from(wallet.privKey).map(b => b.toString(16).padStart(2, '0')).join('');

  await wallet.lock('passphrase');

  // Inspect raw storage
  let foundPlaintextKey = false;
  for (const [key, value] of storage.entries()) {
    const stringified = JSON.stringify(value);
    if (stringified.includes(privKeyHex)) {
      foundPlaintextKey = true;
      console.log(`Found plaintext key in storage key "${key}"`);
    }
  }
  assert(!foundPlaintextKey, 'plaintext key found in storage');
});

// The property that actually matters, stated once and checked directly:
// whatever is in storage must be useless without the passphrase. Checking for
// the private key alone is not enough -- the mnemonic derives the private key,
// so persisting it in the clear defeats the encryption entirely.
test('storage never contains the mnemonic, before or after lock', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  const mnemonic = wallet.mnemonic;
  const words = mnemonic.split(' ');

  function dump() {
    return JSON.stringify(Array.from(storage.entries()));
  }
  function leaks(where) {
    const blob = dump();
    assert(!blob.includes(mnemonic), `full mnemonic present in storage ${where}`);
    // A run of consecutive words is just as good as the whole phrase.
    for (let i = 0; i + 3 <= words.length; i++) {
      const run = words.slice(i, i + 3).join(' ');
      assert(!blob.includes(run), `mnemonic words "${run}" present in storage ${where}`);
    }
  }

  leaks('before lock');
  await wallet.lock('a-passphrase');
  leaks('after lock');
});

test('a newly created wallet writes nothing to storage until it is locked', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  assertEqual(storage.size, 0, 'creation must not persist anything');
  assert(wallet.mnemonic, 'mnemonic is available in memory for the user to record');

  await wallet.lock('a-passphrase');
  assert(storage.size > 0, 'lock is what persists the wallet');
});

test('storage contents alone yield no key material without the passphrase', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  await wallet.lock('the-real-passphrase');

  // Simulate an attacker with a full copy of localStorage and nothing else.
  const stolen = new Map(JSON.parse(JSON.stringify(Array.from(storage.entries()))));
  const attacker = new Wallet({ storage: stolen, restore: true });

  assert(!attacker.privKey, 'no private key from storage alone');
  assert(!attacker.mnemonic, 'no mnemonic from storage alone');
  assert(attacker.locked, 'a restored wallet starts locked');

  const guessed = await attacker.unlock('not-the-passphrase');
  assert(!guessed, 'wrong passphrase must not unlock');
  assert(!attacker.privKey, 'still no private key after a failed unlock');
  assert(!attacker.mnemonic, 'still no mnemonic after a failed unlock');
});

test('unlock restores the mnemonic and address, lock clears them from memory', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  const mnemonic = wallet.mnemonic;
  const address = wallet.address;

  await wallet.lock('pass');
  assert(!wallet.privKey, 'lock clears the private key from memory');
  assert(!wallet.mnemonic, 'lock clears the mnemonic from memory');
  assertEqual(wallet.address, address, 'address is not secret and survives lock');

  const restored = new Wallet({ storage, restore: true });
  assertEqual(restored.address, address, 'address is recovered from storage while locked');
  assert(await restored.unlock('pass'), 'unlock succeeds');
  assertEqual(restored.mnemonic, mnemonic, 'mnemonic recovered');
  assertEqual(restored.address, address, 'address unchanged after unlock');
});

test('storage contains no plaintext passphrase or password hint', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  const passphrase = 'my-secret-passphrase-xyz789';

  await wallet.lock(passphrase);

  let foundPassphrase = false;
  for (const [key, value] of storage.entries()) {
    const stringified = JSON.stringify(value).toLowerCase();
    if (stringified.includes(passphrase.toLowerCase())) {
      foundPassphrase = true;
    }
  }
  assert(!foundPassphrase, 'passphrase found in storage');
});

// =============================================================================
// MNEMONIC RESTORATION
// =============================================================================

test('restoring from mnemonic yields the same address', async function() {
  const storage1 = new Map();
  const wallet1 = await Wallet.create({ storage: storage1 });
  const mnemonic = wallet1.mnemonic;
  const address1 = wallet1.address;

  const storage2 = new Map();
  const wallet2 = await Wallet.create({ storage: storage2, mnemonic });
  const address2 = wallet2.address;

  assertEqual(address1, address2, 'addresses match');
});

test('restoring from mnemonic yields deterministic keys', async function() {
  const mnemonic = 'abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about';

  const storage1 = new Map();
  const w1 = await Wallet.create({ storage: storage1, mnemonic });
  const privKey1 = new Uint8Array(w1.privKey);

  const storage2 = new Map();
  const w2 = await Wallet.create({ storage: storage2, mnemonic });
  const privKey2 = new Uint8Array(w2.privKey);

  assertArrayEquals(privKey1, privKey2, 'derived keys are identical');
});

test('invalid mnemonic (bad word) is rejected', async () => {
  const storage = new Map();
  const badMnemonic = 'notaword abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about';
  await assertThrows(
    async () => await Wallet.create({ storage, mnemonic: badMnemonic }),
    'invalid word'
  );
});

test('invalid mnemonic (wrong length) is rejected', async () => {
  const storage = new Map();
  const badMnemonic = 'abandon abandon abandon about';
  await assertThrows(
    async () => await Wallet.create({ storage, mnemonic: badMnemonic }),
    '12 or 24 words'
  );
});

test('invalid mnemonic (bad checksum) is rejected', async () => {
  const storage = new Map();
  // Valid words but invalid checksum
  const badMnemonic = 'abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon';
  await assertThrows(
    async () => await Wallet.create({ storage, mnemonic: badMnemonic }),
    'checksum'
  );
});

// =============================================================================
// ADDRESS FORMAT
// =============================================================================

test('mainnet addresses start with W', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage, network: 'mainnet' });
  assert(wallet.address.startsWith('W'), `address "${wallet.address}" should start with W`);
});

test('testnet addresses start with n', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage, network: 'testnet' });
  assert(wallet.address.startsWith('n'), `address "${wallet.address}" should start with n`);
});

test('regtest addresses start with W', async () => {
  const storage = new Map();
  const wallet = await Wallet.create({ storage, network: 'regtest' });
  assert(wallet.address.startsWith('W'), `address "${wallet.address}" should start with W`);
});

// =============================================================================
// PBKDF2 ITERATION COUNT
// =============================================================================

test('encryption uses high PBKDF2 iteration count', async function() {
  const storage = new Map();
  const wallet = await Wallet.create({ storage });
  await wallet.lock('passphrase');

  // Check that stored metadata includes iteration count info
  const stored = storage.get('wallet');
  assert(stored && stored.iterations, 'stored wallet has iterations metadata');
  assert(stored.iterations >= 100000, 'iteration count is >= 100000');
});

// =============================================================================
// SUMMARY
// =============================================================================

const toHex = (b) => Array.from(b).map((x) => x.toString(16).padStart(2, '0')).join('');

// =============================================================================
// DERIVATION CHANGE AND THE WALLETS THAT PREDATE IT
// =============================================================================

test('a new wallet derives its key the BIP39/BIP32 way, not the old way', async () => {
  const storage = new Map();
  const w = await Wallet.create({ storage });
  const viaSpec = await mnemonicToPrivateKey(w.mnemonic);
  assertEqual(toHex(w.privKey), toHex(viaSpec),
    'wallet key must equal the specification derivation');
});

test('the two derivations genuinely disagree', async () => {
  // If this ever fails, the "legacy" path is not legacy and the migration
  // handling below is testing nothing.
  const w = await Wallet.create({ storage: new Map() });
  const specKey = toHex(w.privKey);
  w._deriveLegacyFromMnemonic();
  assert(toHex(w.privKey) !== specKey,
    'old and new derivation must differ, or there was no bug to fix');
});

test('a wallet created before the fix still unlocks, and says so', async () => {
  const storage = new Map();
  // Put storage into exactly the state the pre-fix code would have left.
  const w = await Wallet.create({ storage });
  w._deriveLegacyFromMnemonic();
  const legacyAddress = w.address;
  await w.lock('correct horse');

  const reopened = new Wallet({ storage, restore: true });
  assertEqual(await reopened.unlock('correct horse'), true, 'unlock should succeed');
  assertEqual(reopened.address, legacyAddress, 'legacy funds must remain reachable');
  assertEqual(reopened.usesLegacyDerivation, true, 'wallet must report it is on the old scheme');
});

test('genuinely corrupt storage is still rejected', async () => {
  const storage = new Map();
  const w = await Wallet.create({ storage });
  await w.lock('correct horse');
  const stored = storage.get('wallet');
  stored.address = 'WrongAddressThatMatchesNeitherDerivation';
  storage.set('wallet', stored);

  const reopened = new Wallet({ storage, restore: true });
  await assertThrows(() => reopened.unlock('correct horse'), 'storage may be corrupt');
});

run();
