# Browser end-to-end tests

Drives the wallet through mint → sell → buy → transfer → cancel in headless
Chrome, against a real `uap-indexer` and a real regtest `whippetd`.

```
npm run test:e2e
```

Exits `0` on success, `0` with a `SKIP:` line if `whippetd` or Chrome is
missing, and `1` on failure — printing the failing screen's text, the console
buffer, and a screenshot path. Artifacts (screenshots, the indexer log) land
in a temp directory named at the end of the run.

Takes two to three minutes, most of it the two 600 000-iteration PBKDF2
unlocks and the block mining.

## Why it exists

The unit suites call exported functions against a fake DOM and never reach the
network. `mint.integration.test.js` and `market.integration.test.js` reach a
real node but call the `uap-js` library directly — they never load `app.js`,
never click a button, never cross a screen boundary.

The seam between those two is where `confirmMint` once built a transaction,
signed it, showed its length in an `alert()` and dropped it on the floor.
Every mint test passed, because they all stop at `buildMintTx`.

Its first complete run found two more of the same kind:

- `buildSellOrder` signed every position as though it were a transfer
  covenant. For a freshly minted position that is the wrong scriptCode, so the
  order published, listed and looked perfect — and the *buyer* got NULLFAIL
  from the node.
- The indexer never published a position's own script, so a minted position
  could not be transferred or sold at all unless the wallet still had the salt
  it minted with. That stranded every token at the moment of creation.

Neither is visible from the DOM. Both are obvious the moment you ask the node.

## Shape

| File | What it is |
|---|---|
| `run.js` | the scenarios, and the only thing you run |
| `stack.js` | whippetd + uap-indexer lifecycle, and the guard |
| `cdp.js` | the Chrome DevTools Protocol driver |
| `keys.js` | the two personas, from fixed BIP39 vectors |

A subdirectory is deliberate: `contrib/uap-indexer/Makefile`'s `web` target
globs `../uap-web/*.js` without recursing, so nothing here can be embedded
into the shipped binary.

## Every assertion is made twice

Once in the browser, because that is where the user is. Then again against the
node — mine a block, and ask what it actually got.

That second half is not belt-and-braces. Break `confirmMint` so it reports a
plausible txid without broadcasting, and *every DOM assertion still passes*:
the review screen renders, the pending badge appears, the txid looks right.
Only `getrawtransaction` notices. A suite asserting on screens alone would
have gone green on the exact bug it was written for.

## Safety

The indexer's `-rpcport` defaults to `33665`, which is **mainnet RPC**.
Nothing here defaults. `startStack` passes every flag explicitly and refuses
to spawn the indexer unless the node it is about to be aimed at both reports
`chain == "regtest"` and is the one this process started (`assertRegtest`).
`-mirror` is never set — it is the only flag that would reach off this
machine.

`whippetd` comes from `contrib/uap-js/test-node-harness.js`, which is always
`-regtest` with an explicit `-datadir` in a fresh `mkdtemp`, on ports 39701 /
39702, and which kills and removes it afterwards.

## Keeping it honest

A green suite is worth nothing until you have watched it go red. These
mutations must each fail it, and each is worth re-running after a substantial
change:

| Break this | Expect |
|---|---|
| `publishMint` returns a txid without calling `broadcast` | `the node has no transaction …` |
| `fillOrder` pays the maker one satoshi less | the node rejects the fill; the taker never reaches the wallet screen |
| `planTransfer`'s address/pubkey cross-check always passes | negative A fails on both assertions |
| `confirmFill` reports a failure with `alert()` again | the run stalls at the fill-failure wait — which is the freeze the modal causes, made visible |
| `uap.normalizeTicker` stops folding case | phase 2 never reaches the review screen: the form rejects the typed `e2e` with `invalid character "e" at position 0` |
| the JS builder emits version `0x01`, or a fifth push | the byte-exact record assertion fails, and the token disappears from `/api/tokens` |

Phase 2 types its ticker in lowercase on purpose. The fold is the wallet's job
and the fold alone — the Go parser rejects a lowercase ticker outright rather
than folding on read, so that one displayed ticker has exactly one on-chain
byte string. Typing `e2e` and demanding `E2E` back out of the node is the
cheapest place to notice if that ever stops being true.

Those last two are also the only mutations here that no unit suite can catch.
The metadata format has two implementations in different languages, and each
one's tests are written against its own idea of the format — so they can drift
into disagreement and both stay green. Phase 2 is the one place the wallet's
actual bytes are handed to the real parser.
