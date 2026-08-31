# uap-js

A small, dependency-free JavaScript library for building and signing
Whippet UAP (`OP_MINT` / `OP_MINT_TRANSFER`) transactions client-side, in
a browser or Node, with no build step. It's the client-side counterpart to
[`contrib/uap-indexer`](../uap-indexer): the indexer answers "what
positions does this pubkey hold," and `uap.js` lets you actually construct
and sign a spend of one.

## What's here

- `sha256.js` -- a vendored, synchronous SHA-256 + HMAC-SHA256, verified
  against the standard NIST/RFC test vectors (`sha256.test.js`). Exists so
  the library doesn't need WebCrypto's async-only API or a Node-specific
  path, and so it can supply the synchronous HMAC that
  `@noble/secp256k1`'s signing needs (see below).
- `ripemd160.js` -- a vendored RIPEMD-160, verified against the standard
  vectors (`ripemd160.test.js`). Needed for `hash160`, which WebCrypto
  does not provide.
- `addr.js` -- P2PKH plumbing: `hash160`, base58/base58check
  encode+decode, `pubkeyToAddress`, `addressToScript`, `scriptToAddress`,
  `buildP2PKHScript`, and Whippet's `VERSIONS` (mainnet/testnet/regtest
  address bytes). Unit-tested in `addr.test.js`.
- `uap.js` -- script building (`buildMintScript`, `buildTransferScript`),
  legacy transaction serialization, `signatureHash`, and signing
  (`signSpend` for UAP positions, `signP2PKHInput` for ordinary inputs,
  `buildPaymentTx`, `buildTransferTx`, `signMakerOrder`, `fillOrder`).
  Unit-tested in `uap.test.js`.
- `noble-secp256k1.js` -- a vendored, verbatim copy of
  [`@noble/secp256k1`](https://github.com/paulmillr/noble-secp256k1)'s
  single-file ESM build. Regenerate with `npm run vendor:secp` after
  changing the pinned devDependency; the file's own header records the
  exact version and where it was copied from.
- `secp.js` -- the EC entry point everything imports. It re-exports the
  vendored library and adds back the DER signature encoding the 2.x line
  dropped, under v1's `signSync(hash, priv, opts)` name and semantics.
  Import this, never the bare `@noble/secp256k1` and never
  `noble-secp256k1.js` directly.
- `integration.test.js` -- the same library driven against a real
  `whippetd` regtest node (see Testing below).
- `demo.html` -- a self-contained browser page: generate a keypair, build
  a mint script, and sign a transfer, entirely client-side.

## What this does *not* do

It never talks to the network. You bring your own UTXO/position details
(e.g. from `uap-indexer`'s `/positions` endpoint, or your node's
`listunspent`) and are responsible for broadcasting the resulting hex
yourself. **Never point a browser at raw node RPC** -- it's a
trusted-operator interface with no CORS story, not a public API. Broadcast
through a backend you control, or hand the hex to `whippet-cli
sendrawtransaction` yourself.

## Elliptic-curve signing

`uap.js` doesn't bundle an EC library -- you pass one in, so you can
choose and audit/pin whatever you trust. It needs
`getPublicKey(privKey, compressed)` and `signSync(msgHash, privKey, opts)`
returning a DER-encoded, low-S signature. `secp.js` in this directory is
that module, and is what the rest of the repo uses:

```js
import * as secp from './secp.js';
import * as UAP from './uap.js';

UAP.configureSecp(secp); // wires up the synchronous HMAC-SHA256 signSync() needs
```

Two things are worth knowing about `secp.js`.

**It's a vendored file, not a package import.** The browser frontend is
served by `uap-indexer` as plain ES modules under a `script-src 'self'`
CSP. A browser can't resolve the bare specifier `@noble/secp256k1` without
an import map, and the CSP forbids an inline one -- so the library is
checked in as `noble-secp256k1.js` and imported by relative path.
`@noble/secp256k1` stays a devDependency purely as the source for
`npm run vendor:secp`; nothing imports it at runtime.

**It re-adds DER.** `@noble/secp256k1` v2 dropped DER encoding entirely:
`sign()` returns a `Signature` that only emits the 64-byte compact form,
and it throws outright if v1's `der`/`canonical` options are even present.
Whippet scriptSigs carry DER, so `secp.js` encodes it, and translates
`canonical` to v2's `lowS`. `secp-equivalence.test.js` pins the resulting
bytes against a fixture recorded from v1.7.1.

Synchronous RFC6979 signing also requires the caller to provide an
HMAC-SHA256 (noble bundles none, to stay environment-agnostic across
browser/Node/RN). `configureSecp()` wires the vendored `hmacSha256` from
`sha256.js` in for you, so this "just works" without another dependency.

## Usage

```js
import * as secp from './secp.js';
import * as UAP from './uap.js';
UAP.configureSecp(secp);

// 1. Generate a keypair (or import an existing private key)
const priv = secp.utils.randomPrivateKey();
const pub = secp.getPublicKey(priv, true); // compressed

// 2. Build a mint script and fund it (the funding step is an ordinary
//    wallet spend -- see doc/uap-minting-guide.md -- only the *new*
//    output's scriptPubKey needs to be this mint script)
const mintScript = UAP.buildMintScript(pub, /* multiplier */ 1000);

// 3. Later, spend the confirmed mint output into a transfer covenant.
//    The first spend is where the lineage is born: buildTransferTx derives
//    the origin from the outpoint being spent (see originForSpend), so it
//    is not passed in here. A later transfer carries that origin forward.
const recipientPub = /* ... */;
const tx = UAP.buildTransferTx(secp, {
  input: {
    txid: mintTxid,
    vout: 0,
    scriptCode: mintScript, // the exact script of the output being spent
    value: mintOutputValueInSatoshis,
    privKey: priv,
  },
  toPubkey: recipientPub,
  multiplier: 1000, // must match the input's own multiplier
  fee: 50000,
});

const rawHex = UAP.txToHex(tx); // hand this to your broadcast backend
```

## Running the demo

`demo.html` needs no build step or server beyond something to serve static
files (browsers block ES module imports from `file://`):

```
cd contrib/uap-js
python3 -m http.server 8000
# open http://localhost:8000/demo.html
```

It imports the vendored `./secp.js` like the rest of the frontend, so it
needs no network access and no import map.

## Testing

```
npm test                # unit tests; no node required
npm run test:integration # end-to-end against a real regtest node
```

`npm test` covers the pure functions -- hashing, address encoding, script
encoding, transaction serialization, signature hashing -- and needs
nothing running.

It also covers the EC library, which for a long time it did not.
`uap.test.js` signs with a stub that returns fixed bytes, and the sighash
and address suites never touch a curve at all, so until
`secp-equivalence.test.js` existed the entire offline suite could pass
with the signing library swapped for a wrong one. That test replays a
fixture of public keys, DER signatures and finished scriptSigs recorded
from `@noble/secp256k1` v1.7.1 and requires the installed library to
reproduce them byte for byte. RFC6979 ECDSA is deterministic, so there is
exactly one right answer per (key, message hash) and it does not depend on
the implementation. **If that test fails after a version bump, the new
library disagrees with the old one -- investigate it, don't re-record the
fixture.**

`integration.test.js` starts its own `whippetd` on **regtest** in a
temporary datadir on a non-default port, and drives the real thing: fund a
P2PKH spend, mint a position, spend it into a covenant, spend that
covenant onward, and confirm the node rejects a spend signed by the wrong
key. It is the check that `signSpend` produces signatures consensus
actually accepts, rather than ones that merely look well-formed. Point it
at your binaries if they are not on `PATH`:

```
WHIPPETD_BIN=/path/to/whippetd WHIPPET_CLI_BIN=/path/to/whippet-cli \
  npm run test:integration
```

`uap.test.js` and the Go indexer's `script_test.go` both consume
`src/test/data/uap_script_vectors.json`, the same fixture the C++
consensus test reads. That file is the cross-implementation contract: if
`uap.js` and the node ever disagree about what a UAP script is, one of
the three suites fails.

## Caveats

- **Multipliers use a canonical encoding, and it is not optional.** A UAP
  output must push every element in its shortest form -- `OP_0` for a zero
  multiplier, `OP_1`..`OP_16` for 1..16, a minimal data push above that.
  `buildMintScript`/`buildTransferScript` do this for you; if you assemble
  script bytes yourself, get it right. Consensus rejects anything else,
  because the same script is executed under `SCRIPT_VERIFY_MINIMALDATA`
  when the position is spent. A non-canonically spelled position is worse
  than unrelayable: it parses as no covenant at all, so spending it confers
  no lineage, so the covenant output such a spend must produce is refused by
  `CheckUapOutputCreation`. It is unspendable outright, not merely awkward.
- **This is a mirror of the consensus script format, not the source of
  truth**, same caveat as `uap-indexer`. If `OP_MINT`'s script format or
  signing rules change in `src/script/script.cpp`, this must be updated to
  match -- and the shared fixture above is what catches it if you forget.
- **Fees are capped, and the cap throws.** `buildPaymentTx` and
  `buildTransferTx` refuse to build a transaction paying more than
  `DEFAULT_TRANSACTION_MAXFEE` (100 coins), mirroring the node constant in
  `src/validation.h`. A fee rate is easy to get wrong by orders of magnitude
  and the rate itself gives no sign of it -- only the absolute fee does, and
  by then a signed transaction has already handed the money to a miner. Pass
  `maxFee` to authorise a larger one. Note the limit of this guard: at a
  100-coin cap, a rate wrong by 1000x still slips under it for a small
  transaction. It catches the coins-for-satoshis class of error, not every
  unit mistake.
- **Key handling is your responsibility.** This library signs wherever
  it's called; it does not manage key storage, backup, or recovery. Don't
  build a "paste your private key into this website" product without
  thinking hard about that.
