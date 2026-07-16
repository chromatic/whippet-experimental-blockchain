# uap-indexer

A tiny, dependency-free Go service that indexes UAP (`OP_MINT` /
`OP_MINT_TRANSFER`) token positions from a `whippetd` node's RPC interface,
and serves them over a small HTTP JSON API. It requires no changes to
`whippetd` — point it at any node's RPC port, including a remote one you
don't operate.

This exists because `whippetd` itself has no concept of "which UTXOs are
UAP positions" or "what does address X hold" — that bookkeeping is left
entirely to consumers, since `OP_MINT`/`OP_MINT_TRANSFER` are just script
templates recognized at spend time, not first-class chain state. This
service does that bookkeeping so a frontend doesn't have to scan and parse
every UTXO itself.

## Building

Requires only the Go standard library (no `go get` needed):

```
cd contrib/uap-indexer
go build -o uap-indexer .
```

## Running

```
./uap-indexer \
  -rpchost=127.0.0.1 -rpcport=33665 \
  -rpccookiefile=/path/to/.whippet/regtest/.cookie \
  -listen=127.0.0.1:8961 \
  -statefile=/var/lib/uap-indexer/state.json
```

Or with explicit credentials instead of a cookie file:

```
./uap-indexer -rpcuser=myuser -rpcpassword=mypass ...
```

By default it starts indexing from genesis (`-startheight=0`). If you know
the height UAP activated on the network you're indexing, pass
`-startheight` to skip scanning irrelevant history on a fresh state file.

`POST`/`DELETE /orders` are rate-limited per client IP by default
(`-ratelimit=30` requests/minute, `-rateburst=10`; set `-ratelimit=0` to
disable). If the relay is genuinely deployed behind a reverse proxy, pass
`-trustproxy` so the limiter keys on the proxy's `X-Forwarded-For` header
instead of the proxy's own connecting IP — only do this if something
upstream actually sets that header, otherwise any client can bypass the
limiter by setting it themselves.

Pass `-mirror=http://peer1:8961,http://peer2:8961` to also pull open
orders from other `uap-indexer` instances (see
`doc/uap-marketplace-operators-guide.md` for why this is safe without
trusting the peer: every mirrored order is independently revalidated
against your own chain view before being adopted). `-mirrorinterval`
controls how often (default 30s).

State is persisted to `-statefile` periodically (`-saveevery`, default
every 20 blocks) and on clean shutdown (SIGINT/SIGTERM), so restarts resume
from where they left off rather than rescanning from `-startheight`.

## Running with Docker

```sh
cd contrib/uap-indexer
docker build -t uap-indexer .

docker volume create uap-indexer-data
docker run -d --name uap-indexer \
  -p 8961:8961 \
  -e UAP_RPC_HOST=your-node-host \
  -e UAP_RPC_PORT=33665 \
  -e UAP_RPC_USER=youruser \
  -e UAP_RPC_PASSWORD=yourpassword \
  -v uap-indexer-data:/data \
  uap-indexer
```

The image is built `FROM scratch` — a statically-linked Go binary and
nothing else (no shell, no package manager), running as a fixed non-root
UID. Every RPC/behavior flag has an `env UAP_*` equivalent (see `-help`
for the full list, or the flag descriptions in `main.go`); credentials
passed via `-e` don't end up baked into the container's command line the
way flags would.

**Provide your own RPC credentials** — there's no default. If you're
running `whippetd` on the same host, either use `--network host` (Linux
only) so the container can reach `127.0.0.1:<rpcport>` directly, or put
both containers on the same Docker network and use the node container's
name as `UAP_RPC_HOST`. If `whippetd` uses cookie auth, mount the cookie
file's directory read-only and set `UAP_RPC_COOKIEFILE` to its path
inside the container instead of `UAP_RPC_USER`/`UAP_RPC_PASSWORD`.

**Always mount `/data` as a volume.** The image bakes in
`UAP_STATEFILE=/data/uap-index.json`; without a volume there, state is
lost (and has to fully resync) every time the container is recreated.
`docker volume create` (as above) or a bind mount both work — either way,
the volume needs to end up owned by UID/GID `65532` (what the image runs
as) for the process to actually be able to write to it, which the image
already arranges for a *fresh* named volume (Docker initializes a new
named volume's ownership from whatever already exists at that path in
the image). A bind mount to an existing host directory won't get that
treatment automatically — `chown -R 65532:65532` it yourself first, or
writes will fail silently until you notice state isn't surviving
restarts.

## Reorg handling

The indexer keeps a small per-height undo log (which positions were
created, which were marked spent) for every block it has indexed. On each
poll, it checks whether its recorded hash at its current tip height still
matches the node's; if not, it walks backward undoing blocks one at a time
until it finds the last common ancestor, then re-applies the new best
chain forward from there.

## HTTP API

- `GET /status` — `{tip_height, tip_hash, position_count, order_count}`
- `GET /positions?pubkey=<hex>[&unspent=true]` — all known positions
  (mint or transfer outputs) for a recipient pubkey, optionally filtered
  to unspent only
- `GET /position/{txid}/{vout}` — a single position by outpoint

Each position:

```json
{
  "txid": "...",
  "vout": 0,
  "pubkey": "02...",
  "multiplier": 1000,
  "value": 49999999800000,
  "is_mint": false,
  "height": 104,
  "spent": false
}
```

`value` is in satoshis. `is_mint` is true for a fresh `OP_MINT` output,
false for an `OP_MINT_TRANSFER` covenant output. A `spent` position also
carries `spent_txid` and `spent_height`.

### Order relay (marketplace)

See `doc/uap-marketplace-design.md` for the full design. In short: a maker
sells a position by signing a transaction fragment with
`SIGHASH_SINGLE|ANYONECANPAY` (via `uap-js`'s `signMakerOrder`), which
commits only to their own input and their own payment output — not the
token's eventual destination, since that doesn't need to exist yet. The
relay stores and serves these signed fragments; it never holds funds or
private keys, and does no cryptographic signature verification itself
(deliberately staying dependency-free — no EC library). It does do
structural validation (the scriptSig must be a single minimal push of a
DER-shaped signature ending in the `SIGHASH_SINGLE|ANYONECANPAY` byte) and
confirms the referenced position is real, unspent, and matches the
claimed multiplier. **The real, authoritative check is always the node
itself when a taker broadcasts a fill** — a malformed or dishonest order
just fails to fill; nothing here needs to be trusted for security.

- `POST /orders` — publish a signed order. Body:
  ```json
  {
    "txid": "...", "vout": 0, "multiplier": 1000,
    "script_sig": "<hex>", "payment_script": "<hex>", "payment_value": 700000000
  }
  ```
  Returns the stored order (with `pubkey` filled in from the position) on
  success, `400` with an error message otherwise.
- `GET /orders[?multiplier=1000]` — list open orders (i.e. whose
  underlying position is still unspent), optionally filtered.
- `GET /orders/{txid}/{vout}` — a single open order, `404` if filled,
  cancelled, or never existed.
- `DELETE /orders/{txid}/{vout}?script_sig=<hex>` — withdraw an order.
  Reproducing the exact `script_sig` originally published is required —
  not real authentication (there isn't any private key material here to
  authenticate with), just enough friction that only someone who already
  had the signed order can remove it from the relay. The maker can always
  unilaterally invalidate their own order for real by spending the
  position elsewhere.

An order is automatically pruned the moment the indexer observes its
underlying position get spent (filled by a taker, or moved by the maker
some other way) — no separate cleanup step needed. Note this pruning
isn't reorg-aware: if the block that spent a position gets reorged out,
the order is not restored (an acceptable gap for ephemeral, non-consensus
data — the maker can just republish).

## Caveats

- **Script recognition is a hand-maintained mirror, not the source of
  truth.** `script.go`'s `ParseUAPScript` re-implements the same template
  matching as `ParseUapOutputScript` in `src/script/interpreter.cpp`
  purely for observation. If the consensus opcode logic changes, this
  must be updated to match, or the index will silently drift from what
  the chain actually enforces. It does not re-verify signatures,
  conservation, or any other consensus rule — it only reports what
  outputs exist, not whether the transactions that created them were
  actually valid (the node already guarantees that for anything in a
  confirmed block).
- **No mempool awareness.** Only confirmed blocks are indexed; unconfirmed
  mint/transfer transactions won't appear until mined.
- **Single-process, no HA.** State lives in one JSON file. For production
  use beyond development/testing, consider adding replication or backing
  the state with a real datastore.
