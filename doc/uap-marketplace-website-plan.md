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
- **6-second blocks are a UX feature.** Confirmation is fast enough that
  we can skip mempool tracking entirely and still feel instant.

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

**Decision: token identity = the outpoint of the originating `OP_MINT`.**
Every position carries an `origin` field (`txid:vout`); a fresh mint's
origin is itself, a transfer inherits it from the position it spent. All
market pairs, balances, and listings key off `origin`, never off
`multiplier` alone.

This must be added to the indexer or the marketplace will silently
conflate unrelated tokens.

### 2. There is no bonding curve, and there cannot be one

pump.fun's core mechanic is an AMM bonding curve: the contract itself
quotes and fills, price rises deterministically with supply, and it
"graduates" to a DEX at a threshold. **Whippet cannot do this.** There are
no general smart contracts — only the fixed `OP_MINT` /
`OP_MINT_TRANSFER` covenant, whose script format
(`<pubkey> <multiplier> OP_MINT_TRANSFER`) has no room for curve logic.

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

Name, ticker, and image need somewhere to live.

First, two facts that decide this:

- **`OP_RETURN` does not bloat the UTXO set.** `IsUnspendable()`
  (`src/script/script.h:642`) is true for any `OP_RETURN`-leading script,
  and `CCoins::ClearUnspendable()` (`src/coins.h:121-127`) nulls those
  outputs before the coins record is stored — pruned, in the code's own
  words, "instantly when entering the UTXO set." The cost is one-time
  block space and fee, not permanent state. (The *salt* is the thing that
  genuinely does persist, since it sits in a real spendable output's
  scriptPubKey — which is why metadata does not go there.)
- **Provenance needs no new identifier.** The originating mint's outpoint
  (`txid:vout`) is already globally unique and unforgeable, and the
  indexer tracks it as `origin` regardless. A random ID would be redundant
  and strictly weaker: it is unbound, so two mints can claim the same one
  and a tiebreak rule becomes necessary.

The real constraint is size. `MAX_OP_RETURN_RELAY` is 83
(`src/script/standard.h:30`) — 1 byte `OP_RETURN` + 2 pushdata + **80
bytes of payload**. Ticker, name, and an image URI do not comfortably fit
(an IPFS CIDv1 alone is 59 characters as text).

**Decision: an `OP_RETURN` output in the same transaction that creates the
mint output, carrying the identity fields plus a hash of a richer
off-chain blob.**

```
OP_RETURN <"WHP1"(4) || varstr(ticker) || varstr(name) || sha256(blob)(32)>
```

That leaves ~40 bytes for ticker and name. The blob holds description,
socials, and full-resolution artwork.

Anchoring with a **hash rather than an ID** is the point: an ID is a
trusted pointer (whoever serves it can swap the content underneath), a
hash is a verifiable one — the client fetches the blob and checks it, so
neither the indexer nor the blob host needs to be trusted. It also
degrades gracefully: if the blob host disappears, ticker and name still
survive on-chain.

Store the raw 32 bytes, not a text CID, and derive the fetch URL by
convention (gateway + hex hash) so no URL is baked into the chain.

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
  `{origin, multiplier, ticker, name, metadata_hash, supply, holders, mint_height}`
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

One page, one form: ticker, name, image, multiplier, amount to lock
(≥ 1000 WHIP, enforced client-side with a clear explanation of why).

Flow: build mint script → build the funding transaction (mint output +
`OP_RETURN` metadata + change) → sign with the wallet key → `POST
/api/broadcast` → poll `/api/status` until the block lands (~6 s) →
redirect to the token page.

Surface the entry fee honestly: it is locked value, recoverable by
spending the position, not a burned fee.

## Phase 6 — market

- **Token list** (`GET /api/tokens`): search/sort by newest, supply,
  activity. Card grid — image, ticker, name, supply, best ask.
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
- Mempool display (6-second blocks make it noise).
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
