// The two identities the e2e suite trades between.
//
// # Why fixed mnemonics
//
// The wallet keeps nothing on disk -- app.js's storage is an in-memory Map --
// so a page reload wipes it and there is no "unlock existing wallet" path
// from the init screen. Every persona therefore lives inside one page load,
// entered through the app's own Restore screen. Fixing the phrase means the
// harness can fund the addresses before the browser has even started, and can
// assert that the address the app derives is the one it expects.
//
// Both phrases are canonical BIP39 test vectors, so their checksums are
// valid: wallet.js validates them and would reject anything invented.
//
// # Why deriving through Wallet rather than bip32 directly
//
// This is the same code path the browser runs. If it were reimplemented here
// the two could disagree about what address a phrase produces, and the suite
// would fund an address the wallet never watches -- failing with an empty
// balance and no hint as to why. Phase 1 asserts the agreement explicitly.
//
// # Network
//
// Wallet defaults to mainnet and the app never overrides it (there is no
// network picker). That is fine on regtest: both share PUBKEY_ADDRESS 0x49
// (addr.js), so the addresses are byte-identical and start with 'W'. Only
// testnet differs. Nothing here or in the app needs patching for this to work.
import { Wallet } from '../wallet.js';
import * as uap from '../../uap-js/uap.js';
import * as secp256k1 from '../../uap-js/secp.js';

export const MAKER_MNEMONIC =
  'abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about';
export const TAKER_MNEMONIC =
  'legal winner thank year wave sausage worth useful legal winner thank yellow';

// The local encryption passphrase, not a BIP39 25th word -- it does not
// affect the key. It must be non-empty: Wallet.lock rejects an empty one,
// despite what the field's placeholder says.
export const PASSPHRASE = 'e2e-test-passphrase';

/** Derive { mnemonic, address, pubKeyHex, privKey } for a phrase. */
export async function identity(mnemonic) {
  const wallet = await Wallet.create({ storage: new Map(), mnemonic });
  const pubKey = secp256k1.getPublicKey(wallet.privKey, true);
  return {
    mnemonic,
    address: wallet.address,
    pubKey,
    pubKeyHex: uap.bytesToHex(pubKey),
    privKey: wallet.privKey
  };
}

export async function identities() {
  const maker = await identity(MAKER_MNEMONIC);
  const taker = await identity(TAKER_MNEMONIC);
  if (maker.address === taker.address) {
    throw new Error('the two personas derived the same address; the fixtures are wrong');
  }
  return { maker, taker };
}
