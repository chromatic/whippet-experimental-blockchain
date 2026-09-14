# UAP marketplace website — implementation plan

A minimal, mobile-friendly, self-custody web frontend for minting and
trading UAP tokens on Whippet, shipped as a **single static Go binary**
that also absorbs the existing faucet. This is the product/build plan; see
`doc/uap-marketplace-design.md` for *why* the underlying trade
construction is safe, and `doc/uap-minting-guide.md` for the protocol
details.

## Guiding constraints

- **Self-custody, always.** Keys are generated and used in the browser and
  never leave it. The backend never sees a private key and never holds
  funds. (The faucet's own hot wallet is the one exception — see the
  merge cautions below.)
- **One process, one artifact.** `contrib/faucet` and
  `contrib/uap-indexer` merge into a single binary that also embeds the
  frontend. Deployment is `scp` + `systemctl restart`.
- **No frontend build step.** `uap-js` is dependency-free ES modules; the
  site matches that. Vanilla ES modules + hand-written CSS. The only
  third-party runtime dependency is `@noble/secp256k1`, self-hosted and
  pinned, *not* loaded from a CDN.
- **Reuse what exists.** `uap-indexer` already does chain scanning, reorg
  handling, and order relaying. Extend it rather than adding a second
  service.
- **6-second blocks are a UX feature.** Confirmation is fast, but the chain is
  not yet popular enough that we can skip mempool tracking entirely and still
  feel instant.

## Architecture

```
  nginx (TLS termination, reverse proxy)
            │
            ▼
  whippet-market  ── single static binary, CGO_ENABLED=0
    │
    ├── /              embedded frontend (go:embed)
    │     ├── wallet.js   key generation, encryption, storage
    │     ├── uap.js      (existing) script + tx building, signing
    │     ├── addr.js     (NEW) base58check, hash160, P2PKH scripts
    │     └── ui/         pages: wallet, mint, market, token detail
    │
    ├── /api/…         indexer
    │     ├── /positions, /orders, /status      (existing)
    │     ├── /utxos?address=                   (NEW)
    │     ├── /broadcast                        (NEW)
    │     ├── /tokens, /token/{origin}          (NEW)
    │     └── /feerate                          (NEW)
    │
    ├── /faucet/…      faucet (existing, ported off exec.Command)
    │
    └── SQLite         index state, orders, faucet claims
            │
            │ JSON-RPC (NodeClient)
            ▼
        whippetd
```

Because the frontend is served from the same origin as the API, **there is
no CORS story at all** — nothing to configure and nothing to get wrong.

No user accounts, no sessions, no custodial component.

---

## Decisions that must be made up front

### 1. Token identity is a *lineage*, not a multiplier

`multiplier` is the only field distinguishing token "types", and it is not
scarce — anyone can mint a fresh lineage with multiplier 1000. Two
unrelated tokens can share a multiplier, and consensus keeps them from ever
merging (see `CheckUapOutputConservation`), but nothing keeps them from
*looking* alike in a UI.

**Decision: token identity = the outpoint of the originating `OP_MINT`,
hashed.** Every position carries an `origin` field: the hex encoding of
`SHA256(txid_internal_bytes || vout as 4-byte LE)`, *not* the `txid:vout`
string. Consensus stamps it into the covenant at the mint's first spend and
every transfer carries it forward unchanged, so the indexer reads it off the
script rather than reconstructing it. All market pairs, balances and listings
key off `origin`, never off `multiplier` alone.

This must be added to the indexer or the marketplace will silently
conflate unrelated tokens.

### 2. There is no bonding curve, and there cannot be one

pump.fun's core mechanic is an AMM bonding curve: the contract itself
quotes and fills, price rises deterministically with supply, and it
"graduates" to a DEX at a threshold. **Whippet cannot do this.** There are
no general smart contracts — only the fixed `OP_MINT` /
`OP_MINT_TRANSFER` covenant, whose script format
(`<pubkey> <multiplier> <origin32> OP_MINT_TRANSFER`) has no room for curve
logic.

What Whippet has instead is a **signed-order book**: makers publish
`SIGHASH_SINGLE|ANYONECANPAY` fragments, takers fill them. This is
non-custodial and needs no coordination, but it is a limit order book, not
an AMM.

Two honest options:

- **(a) Ship an order book.** Simplest, fully non-custodial, matches the
  primitives exactly. Risk: thin books feel dead for brand-new tokens.
- **(b) Order book + optional market-maker bot.** The site (or a token's
  creator) runs a bot holding its own keys that continuously posts ladder
  orders priced along a curve. This *simulates* a bonding curve using only
  existing primitives. The bot is a normal market participant with its own
  custody — no protocol change, no user funds at risk.

Recommendation: build (a); design the order-listing API so (b) can be
added later as pure client code with no protocol or backend change.

### 3. Token metadata: on-chain anchor + hash-verified off-chain blob

**Decision: an `OP_RETURN` output in the same transaction that creates the
mint output, carrying the ticker plus a hash committing to a richer
off-chain blob.**

The format is specified normatively in
[`doc/uap-token-metadata.md`](uap-token-metadata.md) — magic `"WUAP"`, a
version byte, a `[A-Z0-9]{1,8}` ticker and an optional 32-byte
`metadata_hash`, 50 bytes at most against an 83-byte relay limit. That
document is the reference the wallet's builder and the indexer's parser are
both written against; do not restate the layout here, or a third divergent
copy is what the next reader will find.

Three points of rationale that belong to this plan rather than to the format:

- **There is deliberately no on-chain `name`.** Minter-supplied text carries
  no authority wherever it is stored — anyone can mint a token named
  `Dogecoin` — so a block only makes it more expensive and more permanent,
  never more trustworthy. The ticker is constrained enough to be a safe
  identifier; everything descriptive lives behind the hash. UI copy below
  reflects this.
- **`OP_RETURN` does not bloat the UTXO set,** so naming a token costs
  one-time block space and fee rather than permanent node state. (The
  *origin* is the thing that genuinely persists, since it sits in every
  transfer covenant's scriptPubKey, which is a real spendable output — which
  is why metadata does not go there. The v1 salt occupied that role and was
  removed; see the minting guide.)
- **Provenance needs no new identifier.** The originating mint's outpoint is
  already globally unique and unforgeable, and its hash is what consensus
  stamps into every covenant in the lineage as `origin`. A random ID would be
  redundant and strictly weaker: it is unbound, so two mints can claim the
  same one and a tiebreak rule becomes necessary.

Anchoring with a **hash rather than an ID** is the point: an ID is a
trusted pointer (whoever serves it can swap the content underneath), a
hash is a verifiable one — the client fetches the blob and checks it, so
neither the indexer nor the blob host needs to be trusted.

Note the flip side, since it is a live gap: a hash proves *which* blob is
authentic but not *where* to find it, and no resolver or blob store exists
yet. Until one does, a token's only label is its ticker.

The creating transaction is an ordinary spend, so its non-UAP outputs are
unconstrained; and when the mint position is later spent, `OP_RETURN`
outputs are not UAP-shaped and are ignored by the conservation check.

### 4. Build with `CGO_ENABLED=0`

The faucet currently imports `mattn/go-sqlite3`, which is cgo. Measured on
the current dev box, a cgo build produces:

```
ELF 64-bit LSB executable, dynamically linked,
  interpreter /lib64/ld-linux-x86-64.so.2
  libc.so.6 => /usr/lib/x86_64-linux-gnu/libc.so.6
ldd (Ubuntu GLIBC 2.43-2ubuntu2.3) 2.43
```

glibc is backward- but not forward-compatible, so a binary built here will
not start on Debian 12 (2.36), Ubuntu 22.04 (2.35), or 24.04 (2.39). Since
we are deploying a bare binary rather than an image, **the build host's
glibc silently becomes the deployment constraint** — Docker was previously
masking this by shipping the build environment alongside the binary.

**Decision: `CGO_ENABLED=0` with `modernc.org/sqlite`** (pure Go, already
present in `contrib/faucet/go.mod` as an indirect dependency). One static
binary that runs on any Linux regardless of where it was built.

The cost is size: `modernc.org/sqlite` is a transpiled SQLite and is
considerably larger than the C original. **This has not been measured yet
and is the first thing to check** — the current faucet is 2.6 MB after
`upx --best --lzma`, and the target is < 8 MB. If pure Go blows the
budget, the escape hatch is to build on the VPS itself and keep cgo.

---

## Phase 0 — safety net (do this first)

The plan modifies `ApplyBlock`, `UndoBlock`, `Save`, and `LoadIndex`. As
of today `contrib/uap-indexer` reports **37.5% statement coverage with no
`index_test.go`** — all four of those functions are untested, and
`UndoBlock` (reorg walk-back via the undo log) is the subtlest code in the
service. There is currently no green to refactor back to.

**0.1 Characterization tests for the index.** Drive `ApplyBlock` /
`UndoBlock` over synthetic blocks: create, spend, reorg one block, reorg
three, reorg past a spend, reorg that un-spends a position. These pin
today's behavior and must pass **unchanged** through every later phase. If
the SQLite migration breaks them, too much changed.

**0.2 Regtest acceptance harness.** Start `whippetd -regtest`, fund an
address, broadcast hex, assert accept/reject with the node's actual error
string. UAP activates at height 0 on regtest, so no activation dance is
needed.

This inverts the plan's biggest weakness. The property that actually
matters — "the node accepts this transaction" — otherwise isn't
exercisable until the market phase, and the `uap-js` README admits the
signing path was only *verified by hand* against a live node, with the
script not checked in. Every later unit gets both a fast pure unit test
and a slow real-consensus acceptance test.

## Phase 1 — shared cross-language fixtures

The top-listed risk in this project is that `ParseUapOutputScript` (C++),
`ParseUAPScript` (Go), and `uap.js` are three hand-maintained mirrors that
can drift. The repo already uses the fixture pattern
(`src/test/data/script_tests.json`), so:

- `src/test/data/uap_script_vectors.json` —
  `{script_hex, expect:{valid, pubkey, multiplier, is_mint}}`, consumed by
  `uap_mint_tests.cpp`, `script_test.go`, and `uap.test.js`.
- `src/test/data/uap_sighash_vectors.json` —
  `{tx, scriptCode, nIn, hashType, expected_hash}`, shared between C++
  `SignatureHash()` and JS `signatureHash()`. This is the riskiest pure
  function in `uap.js`: legacy sighash quirks including the degenerate
  "hash of 1" cases.

Red = add a vector one implementation gets wrong; that language's suite
goes red. This makes drift *unmergeable* rather than merely documented.

## Phase 2 — merge into a single binary

**2.1 One `NodeClient` interface.** The faucet currently shells out via
`exec.Command("whippet-cli", "validateaddress", …)` — a process fork per
request, and effectively untestable. The indexer already has a real
JSON-RPC client in `rpc.go`. Collapse to one interface with a fake for
tests; this is what makes the whole HTTP layer testable under `httptest`
with no node running.

**2.2 SQLite replaces the JSON state file.** `Save`/`LoadIndex` currently
marshal the entire index and atomically rename it — fine today, but it
does not survive adding a chain-wide P2PKH UTXO index. Swap the
persistence layer *behind the existing interface*; **do not rewrite the
reorg logic into SQL.** Keep the tested in-memory model as the hot path.
The Phase 0.1 tests are the check that this held.

**2.3 Embed the frontend.** `//go:embed web/*`, served at `/`, with the
API under `/api/` and the faucet under `/faucet/`. Same-origin, so the
CORS work that a split deployment would have needed disappears entirely.
It also means `httptest.NewServer(mux)` covers the whole application
including the frontend, so end-to-end HTTP tests need no separate static
server.

**2.4 Build target.** `CGO_ENABLED=0 go build -ldflags="-s -w"`, then
`upx --best --lzma` (the faucet Makefile already does the latter).
Measure against the < 8 MB budget here, before the frontend grows.

## Phase 3 — close the library gaps

The missing plumbing a self-custody browser wallet needs. Every item below
gets a unit test against published vectors *and* an acceptance test
through the Phase 0.2 harness.

**3.1 `contrib/uap-js/addr.js` (new).** `uap.js` has no P2PKH support at
all and `sha256.js` has no RIPEMD160, so the browser currently cannot fund
a mint, pay for a buy, or make change.

- `ripemd160()` — vendored, tested against standard vectors (mirror how
  `sha256.js` is structured and tested).
- `hash160()`, `base58checkEncode/Decode()` — known vectors, round-trip
  property, and an explicit red for a corrupted checksum.
- `addressToScript()` / `scriptToAddress()` for P2PKH, using Whippet's
  version bytes.
- `buildP2PKHScript(pubkey)`.

**3.2 `uap.js`: P2PKH input signing.** `signSpend()` returns `<sig>` only,
correct for UAP positions but wrong for P2PKH, which needs
`<sig> <pubkey>`:

```js
function signP2PKHInput(secp, scriptCode, privKey, pubKey, tx, nIn, hashType)
  // → pushData(sig||hashtype) ++ pushData(pubKey)
```

Plus `buildPaymentTx()` (select inputs, add change, sign all inputs) —
needed by the mint funding flow, the taker fill flow, and plain sends.
Coin selection is property-tested: always covers the fee, never emits dust
change, never over-selects.

**3.3 Index ordinary P2PKH UTXOs.** The wallet needs plain WHIP UTXOs and
the node has no address index. `ApplyBlock` already walks every output —
extend it to record P2PKH outputs keyed by hash160.

- `GET /api/utxos?address=<addr>[&unspent=true]` →
  `[{txid, vout, value, height}]`

**3.4 Lineage tracking.** Add `origin` to each position (self for mints,
inherited from the spent position for transfers), and parse the
`OP_RETURN` metadata during indexing.

- `GET /api/tokens` → one row per lineage:
  `{origin, multiplier, ticker, metadata_hash, supply, holders, mint_height}`
- `GET /api/token/{origin}` → detail + position list

The client fetches and verifies the metadata blob against
`metadata_hash` itself; the server never needs to be trusted for it.

> **The subtle part.** 3.3 and 3.4 both extend the undo log, which
> currently records only "positions created, positions spent". Undoing a
> block must now also restore `origin` on un-spent positions and roll back
> P2PKH UTXOs. Write those tests red first — this is exactly the seam that
> was untested before Phase 0.

**3.5 Broadcast + fee rate.** The `uap-js` README correctly warns never to
point a browser at node RPC. The merged binary already holds RPC
credentials, so a narrow passthrough is the minimal safe answer:

- `POST /api/broadcast` `{hex}` → `sendrawtransaction`, returning txid or
  the node's rejection message. Rate-limited via the existing limiter.
- `GET /api/feerate` → a recommended sat/kB figure.

**3.6 Fix the stale `uap-js` README.** It claims "Only `SIGHASH_ALL` is
implemented" and that `signatureHash`/`signSpend` throw for anything else.
The code implements `ALL`/`NONE`/`SINGLE` with `ANYONECANPAY`, plus
`signMakerOrder()` and `fillOrder()`. Correct it before anyone builds
against it.

## Phase 4 — wallet

The trust foundation. Get this right before anything else user-facing.

- **Key generation** in-browser via `secp.utils.randomPrivateKey()`.
- **BIP39 mnemonic** for backup. Requires vendoring the 2048-word list
  (~13 KB). Force the user to record it at creation, and verify a few
  words before letting them fund anything.
- **Encrypted storage**: derive a key from a passphrase via WebCrypto
  PBKDF2 (high iteration count), AES-GCM the private key, persist to
  IndexedDB. Never store the plaintext key or the passphrase.
- **Single account** to start — one key, one address. Multi-account/HD
  derivation is a later luxury.
- **Screens**: create/restore, unlock, balance + receive (address + QR),
  send.

Test the *logic* headlessly in Node — keygen, encrypt/decrypt round-trip,
tx assembly, coin selection. Do not build a browser-automation rig for
rendering; visual checking is the right call for a project whose stated
goal is simplicity.

Explicitly out of scope: any "paste your private key" path, and any
server-side key material whatsoever.

> **HTTPS is a hard requirement, not a nicety.** `crypto.subtle` is only
> available in a secure context. Over plain HTTP it is `undefined`, so the
> wallet can generate keys (`crypto.getRandomValues` works everywhere) but
> cannot encrypt them — the worst possible failure mode. nginx terminates
> TLS, so this is satisfied in production; make sure development uses
> `localhost` (also a secure context) rather than a LAN IP.

## Phase 5 — mint

One page, one form: ticker, multiplier, amount to lock (≥ 1000 WHIP,
enforced client-side with a clear explanation of why). There is no name or
image field — those live in the off-chain blob, and gain one when a
resolver for `metadata_hash` exists.

Flow: build mint script → build the funding transaction (mint output +
`OP_RETURN` metadata + change) → sign with the wallet key → `POST
/api/broadcast` → poll `/api/status` until the block lands (~6 s) →
redirect to the token page.

Surface the entry fee honestly: it is locked value, recoverable by
spending the position, not a burned fee.

## Phase 6 — market

- **Token list** (`GET /api/tokens`): search/sort by newest, supply,
  activity. Card grid — ticker, supply, best ask (plus image and
  description once the off-chain blob behind `metadata_hash` resolves).
- **Token detail** (`GET /api/token/{origin}` + `GET /api/orders`):
  metadata, your balance, the order book, buy/sell.
- **Sell**: pick a position, enter an asking price, `signMakerOrder()`,
  `POST /api/orders`. Show it as cancellable (`DELETE /api/orders/…`).
- **Buy**: `fillOrder()` + sign your own payment inputs with
  `signP2PKHInput()` + `POST /api/broadcast`.

Present amounts in tokens (`value × multiplier`) with the WHIP figure
secondary — the design doc flags the missing price/fee unit as a real gap,
and it is pure presentation logic.

Note that `fillOrder()` moves the position's *full* value; partial fills
are not supported. Makers wanting to sell part of a holding must first
split it into a smaller position with an ordinary transfer. Either expose
a "split" action, or do it transparently as a pre-step in the sell flow.

## Trust model: what the wallet verifies, and what it still trusts

The wallet's only source of chain data is the relay (`uap-indexer`). It has
no other view of the chain: no light client, no independent header chain,
nothing. That makes the relay's honesty worth being precise about, field by
field, rather than asserting "self-custody" and leaving the reader to guess
how far that actually reaches.

Most of what a hostile relay could lie about **fails closed already**,
before any special handling: a fabricated `script_sig`, `payment_script`, or
`payment_value` on an order just produces a transaction the maker never
signed, which the wallet can still build and broadcast, but the node refuses
it (the signature does not verify against the tampered fields). The taker
loses nothing but the broadcast attempt.

One field did not fail closed, until now: `position.value`. A UAP covenant's
conservation rule only requires outputs to sum to no *more* than the input
(`nValueOut <= nValueIn` in `CheckUapOutputConservation`,
`src/script/interpreter.cpp`) — less is a melt, and melts are legal. A relay
that under-reports a position's value gets the taker to build a covenant
output smaller than the position actually spent; the shortfall is silently
burned to miner fees, the transaction is completely valid, and the node has
no reason to refuse it. `origin` and `multiplier`, also read from the relay
on the fill path, are exposed to the same problem: nothing about a wrong
`origin` or `multiplier` alone stops the resulting transaction from
confirming.

### What the wallet verifies for itself

On the fill path, before signing or broadcasting anything, the wallet
(`verifyPosition` in `contrib/uap-web/market.js`) does the following:

1. Fetches the raw, hex-encoded bytes of the transaction that created the
   position, via `GET /api/rawtx/{txid}` (`contrib/uap-indexer`, proxying
   the node's own `getrawtransaction`).
2. Double-SHA256s those bytes and confirms the byte-reversed result equals
   the txid the order names. This is the load-bearing step: a txid is a
   commitment to the bytes that hash to it, and the taker already has that
   txid independently — it is part of the outpoint the maker's own
   `script_sig` commits to, not something the relay could swap out without
   the maker's signature failing to verify. A relay cannot produce different
   bytes with the same hash, so once this check passes, everything read out
   of those bytes below is as trustworthy as the chain itself.
3. Parses the verified bytes and reads the claimed output's real value and
   `scriptPubKey` directly from them — never from the relay's `/position`
   JSON.
4. Decodes the `scriptPubKey`'s multiplier and origin (deriving the origin
   from the outpoint instead, for a position sold straight off its mint,
   which carries none in the script) and compares both against what the
   order claims, and compares the parsed value against what
   `apiClient.getPosition` reported.

**Any mismatch — wrong hash, wrong value, wrong multiplier, wrong origin —
refuses the fill outright**, before anything is signed, with the specific
reason shown inline on the page. Once this passes, the covenant output is
built from the verified value, not the relay's.

### What the wallet still takes on trust from the relay

- **Order-book liveness and completeness.** Whether an order is listed at
  all, and whether a listed order has already been filled or cancelled, is
  taken entirely from `GET /orders` / `GET /orders/{txid}/{vout}`. The
  wallet does not independently scan the chain for competing spends of the
  same position before building a fill.
  *Consequence of a hostile or merely broken relay:* it can waste a taker's
  time — e.g. serving a stale order for an already-spent position — but the
  worst outcome is a broadcast the node rejects (`bad-txns-inputs-spent`),
  reported inline, same as any other failed broadcast. No funds move on a
  rejected broadcast.
- **The maker's terms** (`payment_value`, `payment_script`, `script_sig`).
  These are not independently re-verified by the wallet, because they do
  not need to be: they are exactly what the maker's own
  `SIGHASH_SINGLE|ANYONECANPAY` signature commits to, so a relay that alters
  any of them produces a signature that fails to verify, and either the
  wallet's own build step or the node's broadcast check catches it.
  *Consequence of a hostile relay:* a failed fill, never a wrong one — this
  is the "fails closed already" case above, not something this feature
  changed.
- **Fee-rate estimates** (`GET /feerate`). A relay reporting a bad rate
  costs the taker in overpaid fees or a slower confirmation.
  *Consequence of a hostile relay:* wasted fee, at worst, never a wrong
  covenant value — `market.js` clamps to the policy floor regardless (see
  `RecommendedMinTxFee`).
- **Token listing metadata** (`GET /tokens`, `GET /token/{origin}`: ticker,
  and any off-chain blob resolved from `metadata_hash`). None of this is on
  the fill path this document describes, and none of it changed here — see
  `doc/uap-token-metadata.md` for what is and isn't anchored on-chain there.
  *Consequence of a hostile relay:* it can show a wrong name, ticker, or
  image for a token; nothing about a position's value or lineage depends on
  it.
- **Availability.** A relay can simply refuse to serve `/api/rawtx/{txid}`,
  or go offline entirely. Verification then cannot proceed and the fill is
  refused with a "relay unreachable"-shaped error — the same failure mode
  as any other endpoint being down. This is not a new gap this feature
  opens; it is the same fail-closed behavior every other network call in
  the wallet already has.

  A live position always has an unspent output, so `/api/rawtx` in fact
  works against a completely default node in the case that matters here —
  confirmed by hand against a real regtest node; see
  `contrib/uap-indexer/README.md`, "Node requirements", for exactly what
  was tested and why. `-txindex=1` remains the recommended, unconditional
  fix for the edge this relies on incidentally (a stale, already-spent
  query), and `uap-indexer` checks for itself at startup rather than
  assuming either way — a loud log banner plus `GET /status` reporting
  `"rawtx_lookup": "broken"` if confirmed-transaction lookup genuinely
  isn't available — specifically so an operator finds out before a buyer
  does, rather than every fill on an otherwise-healthy
  relay failing with no obvious cause.

**What this explicitly does not claim:** verification proves the relay's
description of *this specific position* matches the chain. It says nothing
about whether the order itself is a good deal, whether the token has real
liquidity, or whether the party on the other end of a trade is trustworthy
in any sense beyond "the covenant they're selling is what they say it is."

## Phase 7 — polish

- Mobile-first CSS: single column, thumb-reachable actions, 44 px targets,
  system font stack. No framework.
- Dark/light via `prefers-color-scheme`.
- PWA manifest + service worker so the static shell loads instantly and
  degrades to "here's your balance, you're offline".
- QR scanning for addresses (`BarcodeDetector` where available).
- Skeleton loaders instead of spinners; optimistic UI on broadcast, since
  a block is only ~6 seconds away.

---

## Deployment

nginx terminates TLS and reverse-proxies to the binary on localhost.

- **Set `-trustproxy`.** With nginx in front, the rate limiter must key on
  `X-Forwarded-For` rather than the proxy's own connecting IP. Confirm
  nginx actually sets that header, and never expose the binary's port
  directly while this flag is on — otherwise clients can spoof past the
  limiter.
- **systemd unit** with `Restart=always`, replacing `docker run`.
- **Credentials via `EnvironmentFile=`, not `ExecStart` flags.** Flags are
  world-readable in `/proc/<pid>/cmdline`. This is the same reasoning the
  indexer README already gives for preferring env vars over container
  command lines.
- `contrib/uap-indexer/Dockerfile` and the Docker section of its README
  become stale for this deployment path; keep or drop them deliberately
  rather than leaving them to rot.

## Deliberately not building

- Custody, accounts, email, or KYC.
- A bonding curve (not expressible in the covenant).
- Partial order fills (needs a split step; call it out in the UI instead).
- Reorg-aware order restoration — the relay already documents this gap;
  makers republish.
- Price charts and history, until there is enough trade volume for them to
  mean anything.
- Browser-automation UI tests.

## Risks worth stating plainly

- **The merge colocates a hot wallet with a public API.** The faucet can
  spend; the indexer is public and read-only. In one process, a bug in the
  marketplace surface is a bug adjacent to a spending key. Low stakes on
  an experimental chain, but keep the faucet's signing path off the shared
  HTTP surface, give it its own rate limit, and keep `/api/broadcast`
  strictly hex-in/txid-out — it must never construct or modify a
  transaction.
- **Binary size is unverified.** `modernc.org/sqlite` is the main unknown
  against the < 8 MB target. Measure in Phase 2.4, before the frontend
  grows.
- **Empty-book problem.** A brand-new token with no orders looks broken.
  This is the strongest argument for the market-maker bot in option (b).
- **Single instance, no HA.** Mitigate with the existing `-mirror`
  peering, and let the frontend accept a configurable API base URL so
  users can point at their own instance.
- **Multipliers 1..16 were unusable; fixed at the node (1.2.0).** Consensus's
  `ParseUapOutputScript` used to require every element of a UAP script to be a
  real data push (`opcode <= OP_PUSHDATA4`), so a covenant output encoding its
  multiplier as `OP_1..OP_16` was not a UAP output at all and failed
  `CheckUapOutputConservation` — a mandatory failure. But
  `SCRIPT_VERIFY_MINIMALDATA` requires the *minimal* encoding when a script is
  executed, and for 1..16 the minimal encoding is exactly `OP_1..OP_16`, so the
  data-push form was rejected as "Data push larger than necessary" when the
  position was spent. No encoding satisfied both, making such a position
  consensus-valid but movable only by a non-standard transaction. The fix
  makes `ParseUapOutputScript` require the *canonical* push for every element
  — the same encoding `MINIMALDATA` demands — so the two rules now ask for the
  same bytes and every position has exactly one representation. A full
  mint → covenant → covenant round trip at multiplier 10 runs against a real
  regtest node in `contrib/uap-js/integration.test.js` ("small multipliers
  (1..16) round-trip end to end"). Client-side consequence: `uap.js` must emit
  the canonical form (`pushMultiplier` does), and the indexer must not report
  non-canonical scripts as positions (`script.go` does not).
- **The script parsers are hand-maintained mirrors.** `script.go` and
  `uap.js` both re-implement the consensus template match. Any change to
  `ParseUapOutputScript` in `src/script/script.cpp` must be
  propagated to both — which is what Phase 1's shared fixtures are for.
  (Within the node itself there is now only one copy: `Solver()`'s
  `TX_OP_MINT`/`TX_OP_TRANSFER` classification calls the consensus parser
  rather than keeping its own, so policy cannot drift from consensus.)
