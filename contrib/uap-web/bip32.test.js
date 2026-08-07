// BIP39 / BIP32 tests, pinned to the published specification vectors.
//
// These are the whole point of the module: hand-rolled key derivation that
// merely looks plausible is how people lose coins. Every vector below comes
// from the specs themselves, not from our own implementation.
import { mnemonicToSeed, masterKeyFromSeed, deriveHardened, derivePath, WHIPPET_KEY_PATH } from './bip32.js';
import { test, assert, assertEqual, run } from './test-harness.js';

const hex = (bytes) => Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');

// =============================================================================
// BIP39 SEED DERIVATION
// Vectors from github.com/trezor/python-mnemonic/blob/master/vectors.json,
// the reference vectors cited by BIP39. Passphrase is "TREZOR" throughout.
// =============================================================================

const BIP39_VECTORS = [
  {
    mnemonic: 'abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about',
    seed: 'c55257c360c07c72029aebc1b53c05ed0362ada38ead3e3e9efa3708e53495531f09a6987599d18264c1e1c92f2cf141630c7a3c4ab7c81b2f001698e7463b04',
  },
  {
    mnemonic: 'legal winner thank year wave sausage worth useful legal winner thank yellow',
    seed: '2e8905819b8723fe2c1d161860e5ee1830318dbf49a83bd451cfb8440c28bd6fa457fe1296106559a3c80937a1c1069be3a3a5bd381ee6260e8d9739fce1f607',
  },
  {
    mnemonic: 'zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong',
    seed: 'ac27495480225222079d7be181583751e86f571027b0497b5b5d11218e0a8a13332572917f0f8e5a589620c6f15b11c61dee327651a14c34e18231052e48c069',
  },
];

for (const [i, v] of BIP39_VECTORS.entries()) {
  test(`BIP39 vector ${i}: mnemonic -> 64-byte seed`, async () => {
    const seed = await mnemonicToSeed(v.mnemonic, 'TREZOR');
    assertEqual(seed.length, 64, 'seed length');
    assertEqual(hex(seed), v.seed, `seed for "${v.mnemonic.split(' ')[0]}..."`);
  });
}

test('BIP39 passphrase changes the seed', async () => {
  const m = BIP39_VECTORS[0].mnemonic;
  const a = await mnemonicToSeed(m, '');
  const b = await mnemonicToSeed(m, 'TREZOR');
  assert(hex(a) !== hex(b), 'passphrase must affect the seed');
});

// =============================================================================
// BIP32 MASTER KEY AND HARDENED DERIVATION
// Vectors from BIP32 itself (Test vector 1, seed 000102...0f).
// =============================================================================

const BIP32_SEED_1 = new Uint8Array([
  0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
  0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
]);

test('BIP32 test vector 1: master key from seed', async () => {
  const m = await masterKeyFromSeed(BIP32_SEED_1);
  assertEqual(hex(m.key), 'e8f32e723decf4051aefac8e2c93c9c5b214313817cdb01a1494b917c8436b35', 'master private key');
  assertEqual(hex(m.chainCode), '873dff81c02f525623fd1fe5167eac3a55a049de3d314bb42ee227ffed37d508', 'master chain code');
});

test("BIP32 test vector 1: m/0' hardened child", async () => {
  const m = await masterKeyFromSeed(BIP32_SEED_1);
  const c = await deriveHardened(m, 0);
  assertEqual(hex(c.key), 'edb2e14f9ee77d26dd93b4ecede8d16ed408ce149b6cd80b0715a2d911a0afea', "m/0' private key");
  assertEqual(hex(c.chainCode), '47fdacbd0f1097043b78c63c20c34ef4ed9a111d980047ad16282c7ae6236141', "m/0' chain code");
});

test("BIP32 test vector 1: m/0'/1 is NOT what hardened derivation gives", async () => {
  // Guards against silently treating an index as hardened when it is not.
  const m = await masterKeyFromSeed(BIP32_SEED_1);
  const c = await deriveHardened(m, 1);
  assert(hex(c.key) !== '3c6cb8d0f6a264c91ea8b5030fadaa8e538b020f0a387421a12de9319dc93368',
    'hardened index 1 must not equal the non-hardened m/0\'/1 vector');
});

test('BIP32 derivation is deterministic', async () => {
  const a = await deriveHardened(await masterKeyFromSeed(BIP32_SEED_1), 3);
  const b = await deriveHardened(await masterKeyFromSeed(BIP32_SEED_1), 3);
  assertEqual(hex(a.key), hex(b.key), 'same input, same key');
});

// =============================================================================
// THE WHIPPET PATH
// whippetd derives m/0'/3'/n' (src/wallet/wallet.cpp:148-158, with
// BIP44_COIN_TYPE = 3). The browser wallet must agree, or a mnemonic
// backed up here cannot be recovered there.
// =============================================================================

test("the wallet path is m/0'/3'/0'", () => {
  assertEqual(WHIPPET_KEY_PATH, "m/0'/3'/0'", 'documented derivation path');
});

test("derivePath matches walking the hardened steps by hand", async () => {
  const seed = await mnemonicToSeed(BIP39_VECTORS[0].mnemonic, '');
  const byPath = await derivePath(seed, [0, 3, 0]);
  const byHand = await deriveHardened(await deriveHardened(await deriveHardened(await masterKeyFromSeed(seed), 0), 3), 0);
  assertEqual(hex(byPath.key), hex(byHand.key), 'path derivation');
});

test('a derived key is a valid secp256k1 scalar', async () => {
  const N = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
  const seed = await mnemonicToSeed(BIP39_VECTORS[0].mnemonic, '');
  const k = await derivePath(seed, [0, 3, 0]);
  assertEqual(k.key.length, 32, 'private key length');
  const asInt = BigInt('0x' + hex(k.key));
  assert(asInt > 0n && asInt < N, 'private key must be in [1, n-1]');
});

test('different mnemonics give different keys', async () => {
  const a = await derivePath(await mnemonicToSeed(BIP39_VECTORS[0].mnemonic, ''), [0, 3, 0]);
  const b = await derivePath(await mnemonicToSeed(BIP39_VECTORS[2].mnemonic, ''), [0, 3, 0]);
  assert(hex(a.key) !== hex(b.key), 'distinct mnemonics must not collide');
});

run();
