# UAP marketplace merge — code review

> **Status: point-in-time snapshot, partially superseded.** This review was
> written against the tree as it stood before the covenant v2 work. It is kept
> as a record of what was found and why, not as a current defect list. Verified
> against the code as of the 1.3.0 branch:
>
> | Finding | Status |
> |---|---|
> | S2 — `script.go`'s mirror missing the multiplier upper bound | **fixed** (`script.go` now rejects `> 2147483647`) |
> | S3 — only write endpoints rate-limited | **fixed** (`api.go` applies a separate `readRL` to the read endpoints) |
> | 1 — no pagination on `/positions` or `/orders` | **fixed** (`ListOrdersPage`, `PositionsForPubKeyPage`, `Page`) |
> | 4 — `ByPubKey` sub-maps never pruned | **obsolete** (the in-memory index by pubkey is gone; positions live in the SQLite store) |
> | S1, 2, 3, 5 | **not re-verified.** Treat as open until checked against the code. |
>
> Two things this review could not have found, both since fixed, and both worth
> noting because they show the limits of a read-only review of one directory:
> UAP transfer outputs could be created for a lineage the transaction never
> spent (`CheckUapOutputCreation`, `src/validation.cpp` — the scope here
> excluded `src/`), and the covenant format itself has since changed.

Scope: `contrib/uap-indexer/*.go`, `contrib/faucet/faucet.go`, and
`contrib/uap-js/{addr,ripemd160,sha256}.js` + their tests. Read against
`doc/uap-marketplace-website-plan.md`.

**Skipped per instructions: `contrib/uap-js/uap.js` and
`contrib/uap-js/uap.test.js`** — another agent is actively editing them;
nothing below concerns those two files.

**A note on repo layout**, for whoever reads this next: this review was
requested against the `whippet-market` worktree, but two files in scope —
`contrib/faucet/faucet.go` and `doc/uap-marketplace-website-plan.md` —
exist only as untracked, worktree-local files in the sibling `whippet`
worktree (same repo, different `git worktree`), not in `whippet-market` at
all. They were read from there; everything else (the indexer, `uap-js`)
was read from the instructed `whippet-market` tree, which carries the
actively-developed `feature/uap-marketplace` branch plus some untracked
work-in-progress files (`index_test.go`, `addr.js`, `ripemd160.js`, and
their tests).

## Summary

Overall health is good for a project at this stage. `contrib/uap-indexer`
is small, readable, and mostly does what its comments claim; the new
`index_test.go` genuinely exercises `ApplyBlock`/`UndoBlock`/reorg
handling (Phase 0.1 of the plan looks substantially done — 90–100%
coverage on `index.go`, up from the plan's stated 37.5%/no-tests
baseline) and `go test -race ./...` is clean. `contrib/uap-js/addr.js` is
correct and well tested: I cross-checked its `VERSIONS` table against
`src/chainparams.cpp` for mainnet/testnet/regtest (`PUBKEY_ADDRESS`,
`SCRIPT_ADDRESS`, `SECRET_KEY`) and every byte matches; call this file
clean. `sha256.js` and `ripemd160.js` are standard, correctly-padded
implementations with passing vectors — also clean.

The real risk sits in two places nobody had flagged yet: `contrib/faucet`
has a completely defeatable anti-abuse check (not a hardening gap — the
check is inert against a trivial script today), and the Go
`ParseUAPScript` mirror is missing one of the two bounds the C++ consensus
parser enforces. Neither is exotic; both are one-line-diagnosis, small
fixes.

---

## Security findings

### S1. Faucet's "one request per day" limiter is fully bypassable with a bare `curl` loop

**File:** `contrib/faucet/faucet.go:97, 104, 107, 226-234`
**Value:** High **Effort:** S **Confidence:** CONFIRMED

`canRequest` gates a claim on `(ip=? OR ua=? OR cookie=?) AND timestamp>cutoff`
(line 228). All three keys are individually defeatable:

- `ip := r.RemoteAddr` (line 97) is stored **with the ephemeral source
  port still attached** (`"1.2.3.4:54321"`), unlike `clientIP()` in
  `contrib/uap-indexer/ratelimit.go:109`, which correctly strips the port
  with `net.SplitHostPort`. Since the port differs on essentially every
  TCP connection, `ip=?` almost never matches a prior row from the same
  real address — the IP check is a no-op in practice.
- `ua` (User-Agent) is attacker-supplied and trivially varied per
  request.
- `cookie` is only checked if the request *sends* one (line 99-106): if
  the client omits the `Cookie` header entirely (the default for any
  scripted client, e.g. plain `curl`), the `else` branch mints a brand
  new `faucetID` from `time.Now().UnixNano()` (line 104) — unique on
  every single request — and that fresh value is what gets compared and
  logged.

Net effect: a script that simply never sends a cookie defeats all three
checks simultaneously, every time. The only surviving limit is the
*global* daily cap (`daily_claim_limit`, default 100 — `faucet.go:188,
193`), which isn't per-user, so one such script can exhaust the entire
day's faucet budget (100 × 10 WHIP by current defaults) by itself, and
repeat the next day indefinitely. This matters more once this process
also holds the hot wallet the plan describes.

**Suggested fix:** use `net.SplitHostPort(r.RemoteAddr)` for the IP
component (mirror `clientIP()` from `ratelimit.go`, which the merge is
already bringing into the same binary), and require a valid,
previously-issued cookie before falling back to any weaker signal —
don't let "no cookie" generate a fresh always-passing identity. Realistically
this needs a real rate limiter (the indexer's `RateLimiter` token-bucket
type is a natural fit post-merge) keyed on IP alone, not an
OR-of-three-weak-signals heuristic.

### S2. `script.go`'s `ParseUAPScript` mirror is missing the multiplier upper bound consensus enforces

**File:** `contrib/uap-indexer/script.go:152-159` vs.
`src/script/interpreter.cpp:256, 290`
**Value:** Medium **Effort:** S **Confidence:** CONFIRMED

Consensus defines `MAX_UAP_MULTIPLIER = INT32_MAX` and
`ParseUapOutputScript` in `interpreter.cpp:290` rejects
`multiplierOut < 0 || multiplierOut > MAX_UAP_MULTIPLIER`. The Go mirror's
`ParseUAPScript` only rejects the negative case (`script.go:157-158`:
`if err != nil || multiplier < 0 { return nil, nil }`) — there is no
upper-bound check at all, so it would happily index a script number up to
`int64` range (`scriptNumToInt64` caps push length at 8 bytes,
`script.go:99-101`).

This isn't exploitable *today*: the indexer only ever parses scripts from
blocks it pulled via `getblock` from a node that already enforced the
consensus bound before accepting the transaction, so no real chain data
can carry a multiplier `> MAX_UAP_MULTIPLIER`. But it is a genuine,
specific instance of exactly the drift-risk the plan's Phase 1
(`doc/uap-marketplace-website-plan.md` "shared cross-language fixtures")
is designed to catch — and `script_test.go` has
`TestParseRejectsNegativeMultiplier` (line 148) but no equivalent test
for the upper bound, so a shared fixture vector for
`multiplier == MAX_UAP_MULTIPLIER + 1` would currently pass in Go and
fail in C++, which is exactly the kind of red Phase 1 wants.

**Suggested fix:** add `const maxUAPMultiplier = math.MaxInt32` and check
`multiplier > maxUAPMultiplier` alongside the existing negative check.

### S3. Only the two write endpoints are rate-limited; every read endpoint is wide open

**File:** `contrib/uap-indexer/api.go:35-160`, `main.go:53`
**Value:** Medium **Effort:** S **Confidence:** CONFIRMED

`checkRateLimit` is called only from `POST /orders` (api.go:79) and
`DELETE /orders/{txid}/{vout}` (api.go:143). `GET /status`, `GET
/positions`, `GET /position/{txid}/{vout}`, `GET /orders`, and `GET
/orders/{txid}/{vout}` have no rate limiting whatsoever — this matches
the `-ratelimit` flag's own description ("max requests per minute per
client IP on write endpoints"), so it's a documented choice, not an
oversight, and today's per-request cost is genuinely cheap (map lookups
under an `RWMutex`). Flagging it because the plan adds `GET /utxos`,
`GET /tokens`, and `GET /token/{origin}` to this same surface, at least
one of which (`/utxos?address=`) is explicitly earmarked to scan a
chain-wide P2PKH index — a case where "cheap today" won't hold, and
where an unauthenticated client can currently issue unlimited concurrent
requests against a process that, post-merge, also holds signing
credentials for the faucet.

**Suggested fix:** put a coarse, generous rate limit (or at least a
global concurrent-request cap) on all endpoints, not just the two
current write paths, before the new read endpoints land.

---

## Other findings

| # | Finding | Category | Value | Effort | File:line |
|---|---|---|---|---|---|
| 1 | No pagination on `/positions` or `/orders` | Optimization | Medium | S | `contrib/uap-indexer/index.go:157`, `orders.go:103` |
| 2 | Cancelled orders can be silently resurrected by a mirror peer | Correctness | Medium | M | `contrib/uap-indexer/orders.go:144-158`, `mirror.go:41-79` |
| 3 | `idx.TipHeight` read outside the mutex in the sync loop | Simplification | Low | S | `contrib/uap-indexer/main.go:134,148-149` |
| 4 | `ByPubKey` never prunes emptied sub-maps | Simplification | Low | S | `contrib/uap-indexer/index.go:140,142` |
| 5 | `idx.TipHash` goes briefly stale relative to `TipHeight` mid-reorg | Correctness | Low | S | `contrib/uap-indexer/main.go:144-146` |

### 1. No pagination on `/positions` or `/orders`

`PositionsForPubKey` (`index.go:157-173`) and `ListOrders`
(`orders.go:103-119`) both return their full, unbounded result set on
every call, and `api.go` serializes whatever comes back with no limit or
cursor. At today's scale this is fine — the plan itself calls out only
the *all-positions* scan case as a real algorithmic problem, and this
isn't that (`ByPubKey` already indexes by key, so a single busy pubkey
is the actual bound, not total index size). It's worth doing before
Phase 3.3 lands `GET /utxos?address=`, since a chain-wide P2PKH index is
exactly the kind of thing where "the one busy address" (an exchange
hot wallet, say) can return an unbounded response today with no query
parameter to cap it. **Fix:** add `limit`/`offset` (or a cursor) to both
handlers now, before the surface area doubles. Confidence: CONFIRMED
(read both functions and their callers in `api.go`).

### 2. Cancelled orders can be silently resurrected by a mirror peer

`CancelOrder` (`orders.go:144-158`) does nothing more than
`delete(idx.Orders, k)` — there is no record that this position was
*deliberately* withdrawn, only that it's currently absent. `mirrorOnce`
(`mirror.go:41-79`) runs on a timer (`-mirrorinterval`, default 30s) and
calls `PublishOrder` for every order a peer currently serves;
`PublishOrder`'s only checks are that the referenced position is known,
unspent, and multiplier-matched (`orders.go:66-99`) — it has no way to
know the maker asked *this* relay to forget the order a few seconds ago.
If a peer (malicious, stale, or simply slower to converge) still serves
the cancelled order, the very next mirror cycle re-adds it, and it will
keep coming back every interval for as long as that peer keeps serving
it and the position stays unspent — the maker's only durable way to kill
it is to actually spend the position on-chain. `mirror_test.go` covers
"peer order for a known position gets adopted" and "peer order for an
unknown position gets rejected" but has no case for "peer order for a
position we ourselves just cancelled." Confidence: CONFIRMED (traced
`CancelOrder` → no tombstone written; `PublishOrder` → no check against
one; `mirrorOnce` → unconditionally calls `PublishOrder` per pulled
order).

**Suggested fix:** keep a small tombstone set (`map[string]time.Time`,
positionKey → cancel time) checked by `PublishOrder`, expired after
some bound (e.g. the position being spent, or a TTL) so it doesn't grow
forever.

### 3. `idx.TipHeight` is read directly (no lock) in the sync loop

`syncOnce` (`main.go:126-174`) reads `idx.TipHeight` as a bare field
access at lines 134 and 148-149, bypassing `idx.mu` entirely, while
`ApplyBlock`/`UndoBlock` mutate the same field under `idx.mu.Lock()` and
`StatusSnapshot` reads it under `idx.mu.RLock()` from HTTP handler
goroutines. This is not a live data race today — `-race` is clean, and
tracing it through confirms why: the polling goroutine is currently the
*only* writer, and it writes exclusively via the locked methods, so its
own unlocked reads never race against a concurrent write (there isn't
one). But it's a latent trap: it only stays safe as long as nothing else
ever writes `TipHeight`, and nothing in the code enforces that
invariant. Confidence: CONFIRMED that the lock is bypassed at those
lines; CONFIRMED via `go test -race ./...` (clean) that it's not
presently a live race.

**Suggested fix:** route those reads through `idx.HashAtHeight`-style
accessors (or add a `TipHeightSnapshot()`), cheap insurance for a
one-line change.

### 4. `ByPubKey` sub-maps are never pruned once empty

`UndoBlock` (`index.go:138-143`) does
`delete(idx.ByPubKey[pos.PubKey], k)` when unwinding a created position,
but never removes the outer `idx.ByPubKey[pubkey]` entry itself once its
inner map is empty — same gap exists on the spend path, which never
touches `ByPubKey` at all (correctly, since spending doesn't remove the
position). Over the life of a long-running indexer this accumulates one
empty `map[string]bool{}` per pubkey that ever *had* a position and then
had all of them undone (reorg) — a slow, low-severity leak, not
something reachable by an attacker at any interesting rate. Confidence:
CONFIRMED by reading `UndoBlock` and `ApplyBlock`; no code path deletes
an emptied `ByPubKey[pubkey]` entry.

**Suggested fix:** in `UndoBlock`, after the inner `delete`, drop the
outer entry too if `len(idx.ByPubKey[pos.PubKey]) == 0`.

### 5. `idx.TipHash` can be briefly stale relative to `TipHeight` mid-reorg

In `syncOnce`'s reorg loop (`main.go:134-146`), each iteration calls
`idx.UndoBlock(idx.TipHeight)` then decrements `idx.TipHeight` directly
— but `UndoBlock` never touches `idx.TipHash`, and nothing in the loop
updates it either. Between the loop's end (if the node height is behind
where undoing stopped, e.g. mid-reorg on a slow node) and the next
`ApplyBlock` call that would overwrite it, `StatusSnapshot`'s `TipHash`
field describes a block that's no longer the believed tip — `TipHeight`
is correct but `TipHash` lags by one rolled-back block. This is a
narrow, self-correcting window (resolved by the very next successful
`ApplyBlock`), and I did not find a way to make it persist, so severity
is low. Confidence: CONFIRMED that `UndoBlock` doesn't touch `TipHash`
and the loop doesn't either; PLAUSIBLE (not CONFIRMED) that this is
ever externally observable, since it requires a poll cycle where undoing
fully catches up but re-applying doesn't happen in the same call (e.g.
node itself is behind, or an RPC error aborts `syncOnce` right after the
undo loop).

**Suggested fix:** have `UndoBlock` also set `idx.TipHash` from
`idx.Heights[height-1]` (or have the caller do it) so the two fields are
never out of sync even transiently.

---

## What I did not find

`readPushes`/`scriptNumToInt64` (`script.go:36-111`) handle truncated
pushes, all four push-length opcode forms, and the CScriptNum sign-bit
convention correctly as far as I could verify by hand; `ratelimit.go`'s
token bucket and eviction loop are straightforward and race-clean; `rpc.go`
has no injection surface (it JSON-marshals params, doesn't
string-concatenate). `addr.js`'s base58/base58check round-trips are
exercised by 792 assertions including corrupted-checksum-at-every-position
and a 500-iteration property test, and both `sha256.js` and `ripemd160.js`
pass their standard test vectors — I have no findings in either.
