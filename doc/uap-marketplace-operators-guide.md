# Running your own UAP marketplace

This is an operator's guide: how to stand up your own instance of the
pieces in `contrib/uap-indexer` and `contrib/uap-js` to run a UAP token
marketplace, and how independently-run marketplaces can share liquidity
with each other. For the protocol design itself (why any of this works,
what's actually enforced by consensus vs. convention), see
`doc/uap-marketplace-design.md` — this guide assumes you've read that or
don't need to.

## What you're running

Three pieces, none of which require modifying `whippetd`:

1. **A `whippetd` node**, synced to the network you care about. This is
   your only source of truth; everything else is derived from its RPC.
2. **`uap-indexer`**, pointed at that node. It tracks UAP positions and
   hosts the order relay (`/orders`).
3. **A frontend** (your own, or `contrib/uap-js/demo.html` as a starting
   point) that calls your indexer's HTTP API and signs transactions
   client-side with `uap-js`.

Nothing here is custodial. The indexer never holds funds or private keys;
it only observes the chain and republishes signed order fragments that
are worthless without a taker completing them exactly as signed (see the
design doc's trust-model discussion).

## Quick start

The fastest path is Docker — see "Running with Docker" in
`contrib/uap-indexer/README.md` for the full walkthrough (credentials via
env vars, volume ownership gotchas, connecting to a node on the same host
vs. a separate container). Short version:

```sh
cd contrib/uap-indexer
docker build -t uap-indexer .
docker volume create uap-indexer-data
docker run -d --name uap-indexer -p 8961:8961 \
  -e UAP_RPC_HOST=your-node-host -e UAP_RPC_PORT=33665 \
  -e UAP_RPC_USER=youruser -e UAP_RPC_PASSWORD=yourpassword \
  -v uap-indexer-data:/data \
  uap-indexer
```

You have to provide your own `whippetd` — the container only runs the
indexer, not a node. Point `UAP_RPC_*` at whatever node you already have
running (this repo, or the same network's public infrastructure if you're
not operating your own node).

Or build and run the Go binary directly, without Docker:

```sh
# 1. Run a node (example: regtest, for local testing)
whippetd -regtest -server -rpcuser=you -rpcpassword=changeme

# 2. Build and run the indexer against it
cd contrib/uap-indexer
go build -o uap-indexer .
./uap-indexer \
  -rpcuser=you -rpcpassword=changeme \
  -listen=127.0.0.1:8961 \
  -statefile=/var/lib/uap-indexer/state.json

# 3. Point a frontend at http://127.0.0.1:8961
```

For mainnet, use `-rpccookiefile` (or `UAP_RPC_COOKIEFILE` in Docker)
instead of `-rpcuser`/`-rpcpassword` if your node uses cookie auth, and
set `-startheight` to whatever block UAP activates at on the network
you're indexing, so a fresh instance doesn't waste time scanning
irrelevant history.

## Operational notes

- **Back up `-statefile`.** It's the only thing that saves you from a full
  rescan on restart. It's a plain JSON file; a periodic copy is enough.
  Losing it isn't catastrophic (the indexer will happily rebuild from
  `-startheight`), just slow for a long-lived chain.
- **Reorgs are handled**, but order-pruning across a reorg is not: if the
  block that spent a position gets reorged out, a pruned order isn't
  automatically restored. In practice this only matters for the few
  blocks around a reorg; a maker whose order vanished this way can just
  republish it.
- **Tune `-ratelimit`/`-rateburst`** for your expected traffic; defaults
  (30 req/min, burst 10, per client IP) are a reasonable floor, not a
  ceiling — if you're running this behind real infrastructure (a CDN,
  a reverse proxy doing its own rate limiting), you may want to loosen
  or disable the built-in limiter (`-ratelimit=0`) and let that layer
  handle it instead. Only pass `-trustproxy` if something upstream
  genuinely overwrites `X-Forwarded-For` — otherwise any client can set
  that header themselves and bypass the limiter entirely.
- **This is a single process with in-memory state plus a JSON snapshot.**
  It's not built for high availability. If you need that, put a load
  balancer in front of multiple *read-only* replicas (each running its
  own sync against the same node, or better, run each against its own
  node) and accept that writes (`POST`/`DELETE /orders`) need to land on
  a specific instance — or lean on mirroring (below) so multiple
  instances converge on the same order set anyway.

## Collaborating with other marketplace operators

This is the part that's easy to get wrong intuitively, so it's worth
being explicit: **you do not need to trust, coordinate with, or even
contact another marketplace operator to share liquidity with them.**

An order is a self-contained, signed artifact: an outpoint, a
`SIGHASH_SINGLE|ANYONECANPAY` signature, and payment terms. Anyone who has
a copy of that artifact and their own view of the chain can independently
verify it means what it claims — a taker's completed fill is checked by
the actual consensus rules regardless of which relay they got the order
from, or whether they got it from a relay at all. This means:

- **A relay never has to trust another relay.** When `uap-indexer`
  republishes a mirrored order (see below), it runs the exact same
  `PublishOrder` validation as for a locally-submitted one: the
  referenced position must be real, unspent, and match the claimed
  multiplier, according to *this instance's own* chain view. A peer
  offering a stale, already-filled, or outright fabricated order just
  gets rejected — same as garbage from any other source.
- **Frontends can aggregate across relays with zero protocol.** The
  simplest form of collaboration needs no new code at all: point your
  frontend at several `uap-indexer` instances' `/orders` endpoints and
  merge the results client-side. Different operators, shared listings, no
  coordination.
- **Relays can mirror each other directly**, if you'd rather your own
  instance carry a merged order book itself (so every consumer of your
  API sees everyone's liquidity, not just what was published to you
  directly):

  ```sh
  ./uap-indexer ... -mirror=https://relay-a.example,https://relay-b.example -mirrorinterval=30s
  ```

  This polls each peer's `GET /orders` on an interval and republishes
  anything new through the normal validation path. It's one-way (pull)
  and best-effort — a peer being down or slow just means you miss their
  orders until it recovers, never a hard failure. Chain any number of
  operators this way (A mirrors B, B mirrors C, ...) and orders
  eventually propagate across the whole set, the same way blocks
  propagate across nodes: no central coordinator, no registration, no
  permission needed. Anyone can start mirroring anyone else's public
  `/orders` endpoint unilaterally.

### What mirroring does *not* give you

- **No dedup beyond "already have it."** If two operators independently
  discover the same order, each stores their own copy; that's harmless
  (it's the same underlying position either way, and only one taker can
  ever actually fill it — the moment it's spent, every mirroring instance
  prunes it on their next sync).
- **No guarantee an order is still fillable by the time you see it.**
  Exactly as with any single relay: `GET /orders` reflects what's known
  as of the last sync, and the real check is always the taker's own
  broadcast. Mirroring doesn't change that, it just widens where an order
  might have come from.
- **No shared identity or reputation across relays.** Nothing here tells
  you *who* is operating a peer, only that whatever orders they're
  serving pass the same validation yours would apply anyway. That's
  sufficient for the protocol to be safe, but if you want to curate
  *which* peers you mirror (e.g. to avoid a peer that's serving mostly
  noise), that's a decision you make via the `-mirror` flag's peer list,
  not something the protocol enforces for you.
