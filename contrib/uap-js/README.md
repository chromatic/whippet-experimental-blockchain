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
  path, and so it can supply the synchronous HMAC that `@noble/secp256k1`
  v1.x's signing needs (see below).
- `uap.js` -- script building (`buildMintScript`, `buildTransferScript`),
  legacy transaction serialization, `SignatureHash`, and signing
  (`signSpend`, `buildTransferTx`). Unit-tested in `uap.test.js`.
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

`uap.js` doesn't bundle an EC library -- you supply one, so you can choose
and audit/pin whatever you trust. The expected interface matches
[`@noble/secp256k1`](https://github.com/paulmillr/noble-secp256k1) v1.x:

```js
import * as secp from '@noble/secp256k1'; // or from a CDN, see demo.html
import * as UAP from './uap.js';

UAP.configureSecp(secp); // wires up the synchronous HMAC-SHA256 signSync() needs
```

`@noble/secp256k1` v1.x's `signSync()` requires the caller to provide an
HMAC-SHA256 implementation (it doesn't bundle one, to stay
environment-agnostic across browser/Node/RN). `configureSecp()` wires the
vendored `hmacSha256` from `sha256.js` in for you, so this "just works"
without adding another dependency.

## Usage

```js
import * as secp from '@noble/secp256k1';
import * as UAP from './uap.js';
UAP.configureSecp(secp);

// 1. Generate a keypair (or import an existing private key)
const priv = secp.utils.randomPrivateKey();
const pub = secp.getPublicKey(priv, true); // compressed

// 2. Build a mint script and fund it (the funding step is an ordinary
//    wallet spend -- see doc/uap-minting-guide.md -- only the *new*
//    output's scriptPubKey needs to be this mint script)
const salt = UAP.randomSalt(20);
const mintScript = UAP.buildMintScript(pub, /* multiplier */ 1000, salt);

// 3. Later, spend the confirmed mint output into a transfer covenant
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

It loads `@noble/secp256k1` from a CDN (`esm.sh`) for convenience. For
anything beyond local testing, self-host and pin a specific version/hash
of that dependency instead of trusting a CDN at runtime.

## Testing

```
npm test          # or: node sha256.test.js && node uap.test.js
```

These are unit tests of pure functions (hashing, script encoding, tx
serialization) and don't need a running node.

The signing path itself -- `signSpend`/`buildTransferTx` producing
signatures the actual consensus code accepts -- was verified by hand by
running a mint -> transfer -> transfer chain against a live `whippetd`
regtest node using this library plus `@noble/secp256k1`, mirroring
`qa/rpc-tests/uap_mint_transfer.py`. That script isn't checked in here
(it's Node-specific glue for a manual check), but the recipe is: fund and
broadcast a mint via the node's own wallet/RPC with `uap.js`'s
`buildMintScript` as the output script, then use `buildTransferTx` to
sign spends of it and confirm `sendrawtransaction` accepts them (and
rejects a spend signed by the wrong key).

## Caveats

- **Only `SIGHASH_ALL` is implemented.** `signatureHash`/`signSpend`
  throw for any other hash type.
- **This is a mirror of the consensus script format, not the source of
  truth**, same caveat as `uap-indexer`. If `OP_MINT`'s script format or
  signing rules change in `src/script/interpreter.cpp`, this must be
  updated to match.
- **Key handling is your responsibility.** This library signs wherever
  it's called; it does not manage key storage, backup, or recovery. Don't
  build a "paste your private key into this website" product without
  thinking hard about that.
