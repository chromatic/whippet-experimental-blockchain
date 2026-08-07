# uap-indexer

A tiny Go service that indexes UAP (`OP_MINT` /
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

```
cd contrib/uap-indexer
go build -o uap-indexer .
```

The only dependency is `modernc.org/sqlite`, a pure-Go SQLite (no cgo), so
cross-compiling and static linking stay as simple as they were. It costs
about 4 MB of binary; `make dist` UPX-compresses the result to around
4.5 MB total, which `make size` checks.

## Running

```
./uap-indexer \
  -rpchost=127.0.0.1 -rpcport=33665 \
  -rpccookiefile=/path/to/.whippet/regtest/.cookie \
  -listen=127.0.0.1:8961 \
  -statefile=/var/lib/uap-indexer/state.sqlite
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
disable).

### Client IP and `-trustproxy`

Every abuse control here keys on the client's IP, which by default means
the address that actually connected. Behind a reverse proxy that is always
the proxy — `127.0.0.1` — so every client shares one bucket and the limiter
stops distinguishing anyone from anyone.

`-trustproxy` (env `UAP_TRUSTPROXY`) makes the service derive the client IP
from `X-Forwarded-For`, falling back to `X-Real-IP`, falling back to the
connecting address. **It defaults to off and must stay off unless a proxy
is genuinely in front of this process**, because those are ordinary request
headers that any client can set to anything: honoring them without a proxy
overwriting them first lets a caller pick a fresh identity per request and
walk straight past the limiter.

`X-Forwarded-For` is a list that each hop *appends* to, so the entry to
trust is the **rightmost** one — the one your own proxy wrote. Everything
to its left was supplied by the caller. Taking the leftmost entry (the
common reading, correct only for a chain of proxies you control end to end)
would hand the attacker the key.

That parse is only correct for **exactly one trusted hop**. If a CDN or
load balancer sits in front of your nginx, the rightmost entry is that
intermediary's egress address, not the user's; fix it at the proxy with
nginx's `real_ip` module (see `deploy/nginx-uap-indexer.conf`), not here.

Malformed input — a garbage header, a trailing comma where the proxy should
have appended, no header at all — falls back to the connecting address
rather than failing the request.

The nginx side of this is in `deploy/nginx-uap-indexer.conf`, and the two
`proxy_set_header` lines in it are what make the flag safe. Enabling
`-trustproxy` without them is the exact misconfiguration to avoid: nginx
forwards the client's own `X-Forwarded-For` untouched by default, so the
"rightmost entry" would be one the client chose.

Pass `-mirror=http://peer1:8961,http://peer2:8961` to also pull open
orders from other `uap-indexer` instances (see
`doc/uap-marketplace-operators-guide.md` for why this is safe without
trusting the peer: every mirrored order is independently revalidated
against your own chain view before being adopted). `-mirrorinterval`
controls how often (default 30s).

## State

State lives in the SQLite database at `-statefile`, created on first run.
It is not a cache in front of an in-memory index — it *is* the index. The
process keeps only the tip, the prune watermark and two counters in RAM.

Each block commits its own changes in a single transaction as it is
applied, so restarts resume from where they left off rather than rescanning
from `-startheight`, and an unclean exit loses nothing — there is no save
interval to tune and no final save to miss.

This replaced a whole-state JSON snapshot written every N blocks, plus the
Go maps behind it. Measured at 200k blocks (about a fortnight of 6-second
blocks):

| | before | now |
|---|---|---|
| persisting a block | 2,399 ms (whole-state snapshot, every N blocks) | ~0.3 ms, every block |
| `GET /tokens` (50 tokens) | 474 ms | 0.2 ms |
| startup | parse the whole snapshot | 0.03 ms |
| Go heap | grew with the chain | 0.7 MB, flat from 10k to 200k blocks |

The snapshot also ran while holding the index's read lock, so it stalled
ingestion and every in-flight API request along with it. Reads now go
straight to SQLite and take no lock at all, so a slow query can no longer
hold up block processing.

`GET /tokens` reads a `lineages` table maintained as positions are created,
spent, unspent and removed — not a `GROUP BY`, which measured 660 ms on the
same fixture. It is kept in the database rather than in memory for two
reasons: a Go-side aggregate and the rows it summarises are updated
separately and can drift apart, whereas one updated inside the same
transaction cannot; and the holder set alone is one entry per unspent
position, so in RAM it would have reintroduced the growth this removed.

If a write ever fails, the indexer latches the error and stops writing
entirely rather than carrying on. That is deliberate: a failure followed by
a success would leave the stored tip ahead of the rows backing it, with a
hole nothing could later detect. The error is logged on every poll, the
in-memory index keeps serving, and the fix is to resolve the disk problem
and restart — which resumes from the last block that fully committed.

The database is a cache, not a ledger. It is entirely re-derivable by
replaying the chain, which is why it runs with `synchronous=NORMAL`: an
fsync per block would buy protection against a power cut that a resync
already provides.

## Deploying (systemd behind nginx)

The intended deployment is one binary on one host, listening on loopback,
with nginx in front of it. `deploy/` has the three files that describe it:

| file | goes to |
|---|---|
| `deploy/uap-indexer.service` | `/etc/systemd/system/uap-indexer.service` |
| `deploy/uap-indexer.env.example` | `/etc/default/uap-indexer` |
| `deploy/nginx-uap-indexer.conf` | `/etc/nginx/sites-available/uap-indexer` |

```sh
# 1. Build and install the binary. Use `make build`, not `make dist`: the
#    UPX-compressed artifact needs a writable-and-executable mapping at
#    startup, which the unit's MemoryDenyWriteExecute=yes refuses.
make build
sudo install -m 0755 uap-indexer /usr/local/bin/uap-indexer

# 2. Dedicated unprivileged account. The unit's StateDirectory= creates
#    /var/lib/uap-indexer with the right ownership on first start.
sudo useradd --system --home-dir /var/lib/uap-indexer \
             --shell /usr/sbin/nologin uap-indexer

# 3. Credentials. Mode 0640 root:uap-indexer -- this file holds the node's
#    RPC password and the service only needs to read it.
sudo install -o root -g uap-indexer -m 0640 \
     deploy/uap-indexer.env.example /etc/default/uap-indexer
sudo editor /etc/default/uap-indexer   # every value in it is a placeholder

# 4. Unit and proxy.
sudo install -m 0644 deploy/uap-indexer.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now uap-indexer
sudo systemctl status uap-indexer
```

Then install the nginx block, adjust `server_name` and the certificate
paths, and `nginx -t && systemctl reload nginx`.

**Credentials belong in the environment file, never in `ExecStart=`.**
Everything in a process's argv is world-readable through
`/proc/<pid>/cmdline`, so `-rpcpassword=…` on the command line hands the
node's RPC password to every local user with `ps`. A process's environment
is readable only by its own user and root. Better still, if the node is on
the same host, use `UAP_RPC_COOKIEFILE` and store no secret at all.

Configuration precedence is fixed:

> **explicit flag** > **environment variable** > **built-in default**

The environment is only ever read to compute a flag's *default*, so a flag
that is actually passed always wins. The practical consequence: setting
`UAP_LISTEN` in the env file does nothing while `ExecStart=` still passes
`-listen`, and nothing warns about it — pick one place per setting. A value
that does not parse (`UAP_RPC_PORT=banana`, `UAP_TRUSTPROXY=yes`) is
ignored in favour of the built-in default rather than aborting startup;
notably an unparseable `UAP_TRUSTPROXY` does **not** enable header trust.

The unit runs as a dedicated non-root user with the systemd sandbox turned
up as far as this service allows (`ProtectSystem=strict` with only its
`StateDirectory` writable, no capabilities, `SystemCallFilter=@system-service`,
`MemoryDenyWriteExecute`, and a `RestrictAddressFamilies` list covering only
what it actually opens). The unit itself comments each non-obvious choice,
including what was deliberately left out and why `DynamicUser=` is not used.

If `whippetd` uses cookie auth, the service's user needs read access to the
node's `.cookie` file — usually by adding `uap-indexer` to the group that
owns the node's data directory. That is also the reason the unit uses a
real account rather than `DynamicUser=`.

## Running with Docker

**This Dockerfile has never been successfully built and is therefore
unverified.** It is kept as a description of a reproducible build, not as
the recommended way to run the service — for that, see the systemd section
above, which is the deployment this project actually targets. Treat the
instructions below as a starting point that may need fixing.

The build context is `contrib/`, not this directory: the Dockerfile needs
to reach `whippetrpc/` (via the `go.mod` replace directive) and the
`uap-web/`/`uap-js/` frontend sources (which it runs `make web` against),
neither of which a context scoped to `uap-indexer/` could see.

```sh
cd contrib
docker build -f uap-indexer/Dockerfile -t uap-indexer .

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
`UAP_STATEFILE=/data/uap-index.sqlite`; without a volume there, state is
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
created, which were marked spent, which UTXOs were created or consumed).
On each poll, it checks whether its recorded hash at its current tip
height still matches the node's; if not, it walks backward undoing blocks
one at a time until it finds the last common ancestor, then re-applies the
new best chain forward from there.

Only the most recent `-reorgwindow` blocks (default 15000, a little over a
day at 6-second blocks) keep their undo log. Older entries are discarded:
undo is only reachable from the tip backwards, so history below the
deepest reorg worth surviving is dead weight, and retaining it grew both
memory and the state file without bound.

A reorg deeper than that window cannot be rolled back at all. The indexer
detects this rather than papering over it — it logs the fact, discards the
index entirely, and rebuilds from `-startheight`. That is a slow recovery,
but the alternative is silently serving state from an orphaned chain.
Setting `-reorgwindow=0` retains everything, restoring the old unbounded
behaviour.

## HTTP API

- `GET /status` — `{tip_height, tip_hash, position_count, order_count,
  healthy}`, plus `store_error` when there is one. `healthy` goes false
  once a write error has latched and the indexer has stopped following the
  chain; the response is then `503 Service Unavailable` and the tip fields
  are frozen at their last good values. The database stays readable
  throughout, so every other endpoint keeps answering 200 with data that
  is merely stale — this endpoint is the only place that distinguishes
  stale from current. Health checks can key on either the status code or
  the `healthy` field; both are always present.
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
(no EC library here — the node is the authority). It does do
structural validation (the scriptSig must be a single minimal push of a
DER-shaped signature ending in the `SIGHASH_SINGLE|ANYONECANPAY` byte) and
confirms the referenced position is real, unspent, and matches the
claimed multiplier. **The real, authoritative check is always the node
itself when a taker broadcasts a fill** — a malformed or dishonest order
just fails to fill; nothing here needs to be trusted for security.

One consequence worth stating plainly: because there is no signature
verification, the relay cannot tell a maker replacing their own listing
from a stranger overwriting it, so publishing is last-write-wins. Someone
can spend a rate-limited request to replace a real order with a
structurally-valid but meaningless one. The cost is noise in the listing,
not funds — a taker who builds a fill from it simply has the broadcast
rejected. The alternative, first-write-wins, is no better: it would let
anyone squat an outpoint before its owner ever listed it. Closing this
properly needs an EC implementation and a reconstruction of the maker's
sighash on the relay side.

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

  A withdrawal is remembered, so that mirroring cannot undo it: a peer that
  copied the order earlier still has a perfectly valid signed fragment and
  would otherwise push it back in on the next poll, every poll. The record
  is per-signature, not per-outpoint, so a maker who signs a fresh offer at
  a different price is not blocked; a local re-publish clears it outright.
  This survives restarts and deep-reorg rebuilds — forgetting it would let
  one reorg undo every cancellation the relay had ever accepted.

  **This is relay hygiene, not a guarantee.** Peers still hold the signed
  fragment and may keep serving it to their own users. All a cancel
  promises is that *this* relay will not resurrect what its own user
  withdrew.

An order is automatically pruned the moment the indexer observes its
underlying position get spent (filled by a taker, or moved by the maker
some other way) — no separate cleanup step needed.

Pruning is reorg-aware, which matters more here than on a slower chain:
at 6-second blocks short reorgs are ordinary, and a fill being orphaned
would otherwise silently retire an offer nobody withdrew. The pruned
order goes into that height's undo log, so rolling the block back restores
the listing exactly as it was — same price, same signature. Likewise, an
order whose position vanishes with its block stops being served while the
position is absent and is served again if the transaction is re-mined,
which is the usual outcome of a reorg.

The one case that is not recovered is an order whose position is orphaned
and never re-mined: the row stays, permanently hidden by the "is the
position still unspent" check that every read applies. That is deliberate.
Deleting it would save a few hundred bytes in the rare case, at the cost of
silently delisting the maker in the common one.

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
- **Single-process, no HA.** State lives in one SQLite file, written by
  one process. For production use beyond development/testing, consider
  running several independent indexers behind a load balancer (each keeps
  its own copy — the state is derived from the chain, so they converge on
  their own) rather than trying to share the file.
