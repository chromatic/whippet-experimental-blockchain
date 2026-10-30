Whippet Core version 1.4.0 is now available from:

  <https://github.com/chromatic/whippet-experimental-blockchain/releases/tag/v1.4.0/>

This release hardens block timestamps and changes how difficulty reacts when
the chain stops finding blocks. It contains **two consensus changes at two
different heights**, and the second is a hard fork:

| Height | Change | Kind | Who must upgrade |
|--------|--------|------|------------------|
| 371,337 | Block timestamps may be at most 72 s ahead of adjusted time (was 2 hours); same height as AuxPoW | soft fork | most hashrate, before the height |
| 600,000 | Difficulty eases while the chain waits for a block (LWMA with easing) | **hard fork** | **every miner and node, before the height** |

The release also fixes two bugs that stopped `-reindex` and `-loadblock` on the
live chain and a node crash after `reconsiderblock`, and adds four
experimental options that make the block files, the undo files and the block
index smaller. Those options are **off by default**.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/chromatic/whippet-experimental-blockchain/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../../INSTALL.md).

**Height 371,337 (soft fork).** From this height, upgraded nodes refuse a
block whose timestamp is more than 72 s ahead of their adjusted time. Every
block that upgraded nodes accept is also accepted by older nodes, so older
nodes keep following the chain. What matters is the miners: a miner on 1.3.1
or earlier that stamps a block more than 72 s ahead produces a block the
upgraded nodes refuse, and its blocks are orphaned until it upgrades.
This is also the height at which AuxPoW blocks become acceptable, so miners and
pools have one date to prepare for. They should upgrade before height 371,337.

**Height 600,000 (hard fork).** From this height a block's required
difficulty (`nBits`) depends on how long after its parent the block is
stamped. Nodes that have not upgraded compute a different value, so they
refuse every block that follows a wait of more than 36 s, so they may stall
or fall onto a separate chain. **Every node, miner and pool must be running 1.4.0 before
height 600,000.** A node that is not upgraded may stall at the first block
that follows a long wait, because it refuses that block. If any old miner is
still producing blocks, those nodes may instead follow a separate chain. We
cannot predict which will happen, so upgrade everything.

At the block rate seen in the weeks before this release (about 6 to 6.5 s
per block) height 371,337 is about two weeks after the 1.4.0 release and
height 600,000 about a month. The rate is not constant, so treat these as
rough: watch `getblockcount`, not the calendar.

There is no wallet or chain-data migration. The on-disk format is unchanged
unless you opt in to one of the experimental storage options (`-slimblocks`,
`-slimprevprefix`, `-slimundo`, `-slimblockindex`; see Experimental, below).
Data written with those cannot be read by 1.3.x.


What changed in block timestamps
=================================

**The problem.** A block must be stamped at least 4 s after its parent (the
spacing floor), so one block stamped ahead of real time forces every
descendant to be stamped at or after it. Until now the limit on "ahead" was
a hard-coded 2 hours. A miner could walk the chain clock up to 7,200 s ahead
of real time and collect a share of blocks well above its hashrate. In the
simulator, an attacker with 0.3 of the hashrate took 0.38 of the blocks under
the old rules and about 0.25 under a 72 s cap.

**The cap (soft fork, height 371,337).** The future-time limit is now a
consensus parameter, `nMaxFutureDrift`. It is 7,200 s below height 371,337
and 72 s (12 block times) from it. 72 s was the best of the values compared
(36, 48, 72, 120, 180 s): it bounds the lead like the smaller caps but
without their stalls of honest miners.

Two effects miners should know about:

- After a block stamped about 70 s ahead, the next honest block is stamped
  past the cap, and your node refuses it until its clock catches up. In the
  simulator this costs honest miners about 250 s per hour under a 0.3
  attacker.
- A node whose clock is more than a few seconds wrong now disagrees with the
  network about which blocks are acceptable. Run an NTP client (chrony,
  systemd-timesyncd) and keep the clock within seconds of UTC. Block
  timestamps are Unix time (UTC); daylight saving time does not matter.

Related to this, `-maxtimeadjustment` now defaults to 36 s. It used to be
70 minutes, which let peers move a node's clock far outside any 72 s window.

**The miner (not a consensus change).** `UpdateTime()` ignored the 4 s
spacing floor, so after a block stamped ahead of the clock every template
was too early until the clock passed that block plus 4 s: miners saw a long
halt. Templates are now stamped at the latest of the median time past plus
one second, the parent's time plus the spacing floor, and the adjusted time.


What changed in difficulty
===========================

**The problem.** LWMA reacts slowly when a very large miner leaves. In the
simulator, after a 100x burst of hashrate for 300 s followed by its
departure, the first block took a median of 1,079 s (worst 2,700 s). The
72 s cap does not change this.

**The fix (hard fork, height 600,000).** A candidate block stamped more than
36 s (six block times) after its parent gets its target multiplied by
2^((gap - 36) / 30), up to the proof-of-work limit. The longer the chain
waits, the easier the next block becomes: the same burst now recovers in a
median of about 200 s. The 4 s spacing floor, the 72 s cap and the LWMA
window are unchanged, and the term is exactly 1 for blocks that follow a
wait of 36 s or less, which is almost every block.

The rule uses only block timestamps from the headers. It reads no wall
clock, so NTP adjustments, time zones and daylight saving time cannot change
which `nBits` a block must carry. The easing is keyed to the candidate's own
timestamp, which the miner chooses within the 72 s cap; stamping late to
ease a block cannot be repeated block after block, and in the simulator it
did not change an attacker's share of blocks.

**Merge-mining pools.** An AuxPoW block's timestamp is fixed when the pool
creates the job, not when the block is found. A pool that refreshes its jobs
at least every 30 s loses nothing; a 60 s refresh costs about 40 s of extra
waiting after a burst. (Simulator result; no real pool has been tested.)
AuxPoW blocks become acceptable at height 371,337, the soft-fork height above;
a standalone block whose version carries the chain ID (0x5750) stays
valid before and after it, and only legacy-format blocks (version 1, or
version 2 with no chain ID) are refused from that height. Check that your
miner takes the version field from `getblocktemplate`.


Fixes
======

**`-reindex` stopped at height 166,639.** A block that arrived before its
parent was read back with the consensus rules of height 0, so its AuxPoW
chain ID was compared with the value from before the chain-ID fix (98). Every
such block past the fix was refused ("block does not have our chain ID (got
22352, expected 98)") and the reindex ended there on a copy of the live chain.
The child's height is now taken from its parent.

**`-loadblock` and `bootstrap.dat` dropped blocks that preceded their
parent.** Only `-reindex`, which has a position on disk, kept such a block
until its parent arrived. An import from a file dropped it together with
every descendant, so a file in arrival order stopped at the first block that
came before its parent (height 166,639 of 180,504 on a copy of the live
`blk00000.dat`). Such blocks are now kept in memory, keyed by parent hash, up
to 128 MiB; past that a block is dropped with a log line, as before. Checked
on the chain to height 180,633 with the file as it was, with windows of 100
and 5,000 blocks shuffled, fully random and fully reversed: every case
reached the tip with `verifychain 3 1000` true and nothing dropped. That
chain has no AuxPoW blocks and is 75 MB, so it never reached the cap; at
about 950 bytes an AuxPoW block, the cap holds roughly 140,000 blocks
waiting for a parent.

A fresh node still wants the genesis block first in the file: it activates
its chain when it meets genesis, and with `-checkblockindex` (on in regtest)
accepting a block before that trips an assertion.

**A crash after `reconsiderblock`.** `reconsiderblock` cleared the failure
flags of a block but left it in the node's set of failed blocks. A header
from a peer that built on a descendant of that block, before the descendant
was fully validated, then hit `assert(failedit->nStatus & BLOCK_FAILED_VALID)`
in `AcceptBlockHeader` and the node aborted. A peer could trigger it, but only
on a node whose operator had run `invalidateblock` and then `reconsiderblock`.
`reconsiderblock` now removes the entry.


Testnet
========

The testnet is now a separate chain with its own genesis block
(`WhippetTestnet`), no Dogecoin checkpoints, chain work, seeds or
assume-valid hash, BIP34/65/66 active from height 1, and its own network
magic (`fc c1 b7 dc`). Ports are 43666 (P2P) and 43665 (RPC); addresses
start with `n`. **Testnet data from earlier builds is incompatible and must
be deleted.** There are no DNS or fixed seeds yet: connect with `-addnode`
or `-connect`.

The 72 s cap is active on testnet from genesis. The easing rule is not
active on testnet in this release.

The `generate` RPC stops early because `maxtries` (1,000,000 hashes) is
shared by the whole call; pass `maxtries` as the second argument to mine
more blocks at one go.


Experimental: `-slimblockindex`
=================================

Off by default. With `-slimblockindex=1` the block index (`blocks/index`) is
stored in a smaller format: the parent hash is cut to a 4-byte prefix and a
cached difficulty that nothing reads is dropped. On a 170,000-block mainnet
index the entry data shrank by about a third and the directory by about a
quarter (25 MB to 19.6 MB). Existing entries are converted once at startup
and the database is compacted afterwards. A reindex is not needed.

- It is **one way**: once a node has written slim entries, versions before
  1.4.0 cannot read its block index without `-reindex`. Turning the option
  off again keeps the slim entries and still works with 1.4.0.
- Two blocks at the same height whose hashes share the first 4 bytes would
  stop the node starting ("ambiguous parent prefix"); the chance is about 1
  in 4 billion per pair of forked blocks.
- Block and undo files (`blk*.dat`, `rev*.dat`) and the chainstate are not
  touched. With the option off the block index is written exactly as in 1.3.1.


Experimental: `-slimblocks`
============================

Off by default. With `-slimblocks=1` a new block is written to `blk*.dat`
without the header fields that can be recomputed: the merkle root of a block
with one transaction (it is the coinbase txid), and for merge-mined blocks the
parent merkle root, `nIndex`, and `hashBlock` and `nChainIndex` when they hold
their usual values. `hashPrevBlock` is kept in full (see `-slimprevprefix`
for the option that shortens it). A block is only stored this way after it has been decoded again and
compared byte for byte with the original; otherwise it is stored as before.
`getblock` and block relay return exactly the same bytes. A single-transaction
block takes 32 bytes less.

- It is **one way**: versions before 1.4.0 cannot read such a record and need
  `-reindex` from the block files of another node (or a re-download). Take a
  backup before turning it on.
- Records already on disk are not rewritten. Old and new records can share a
  file, and a node reads both whether or not the option is on.
- `-txindex`, `-reindex`, `verifychain`, pruning and `-loadblock` handle both
  kinds of record.


Experimental: `-slimundo`
============================

Off by default. With `-slimundo=1` a block that spent nothing (a coinbase
only block) gets no record in `rev*.dat`, and the undo checksum of the other
blocks is cut from 32 to 8 bytes. Disconnecting a block with no record needs
no undo data, so reorgs work as before.

- It is **one way**: versions before 1.4.0 cannot disconnect such a block
  without `-reindex`.
- Records already on disk are not rewritten.


Experimental: `-slimprevprefix`
=================================

Off by default, and only with `-slimblocks`. A `blk*.dat` record then keeps
the first 8 bytes of the previous block hash instead of all 32.

- Reads that come through the block index (RPC, relay, `verifychain`, wallet
  rescan, reorgs) take the parent hash from the index.
- `-reindex` and `-loadblock` resolve the prefix against the hashes of the
  blocks they have accepted so far. A block whose parent has not arrived yet
  waits with the other out-of-order blocks. A wrong match changes the block
  hash and fails the proof-of-work check.
- The genesis block, and a block whose parent is not in the block index when
  it is written, keep the full hash.
- The `txindex` stores a file position, not a block hash, so
  `getrawtransaction` finds the block hash of such a record from a position
  map the node keeps in memory.
- Two known blocks whose hashes share their first 8 bytes are not detected
  when the record is written; at this chain size the chance is about 1 in
  1e9, and a wrong match is rejected by the proof-of-work check.


Storage savings on the live chain
===================================

A node importing `bootstrap.dat` in height order to block 180,633. Sizes are
bytes in use; the files themselves are preallocated in chunks (16 MiB for
`blk`, 1 MiB for `rev`), so the directory looks bigger.

| | Default | `-slimblocks -slimundo -slimblockindex` | plus `-slimprevprefix` |
|---|---|---|---|
| `blk00000.dat` | 75,086,721 | 69,905,633 (-6.9%) | 65,589,166 (-12.6%) |
| `rev00000.dat` | 14,050,876 | 6,963,248 (-50.4%) | 6,963,248 |
| block index | 25,440,245 | 18,652,234 (-26.7%) | 18,652,122 |
| total | 114,577,842 | 95,521,115 (-16.6%) | 91,204,536 (-20.4%) |

The chainstate is 10.5 MB in every case. 161,909 of the 180,634 `blk` records
are in the shorter form. Only 18,725 of the 180,633 blocks have an `rev`
record. `verifychain 3 1000` returned true in every run, and a `-reindex`
from the prefix-only `blk` file reached the same tip with the same
chainstate and block index sizes.

The saving follows the chain: it comes from blocks with one transaction and
blocks that spent nothing, which are most of the early chain. Expect less on
a chain of large blocks.

Not tested on real data: AuxPoW blocks (none exist on the chain before height
371,337, so the AuxPoW part of `-slimblocks` is covered only by unit tests
and regtest), a downgrade, and pruning with slim files on a live chain
(regtest covers it, see Testing).

Regtest options
================

Regtest-only test hooks (rejected on mainnet and testnet):

- `-timingprofile=whippet|scaled-drift|scaled-drift-easing` makes regtest
  run Whippet's timing rules with the spacing floor, drift limit and
  optionally the easing rule. `-timingfloor`, `-timingmindiff` and
  `-timingeasingtau` adjust them.
- `-timingdriftcapheight=H` adds the mainnet 72 s cap at height `H`.
- `-timingeasingheight=H` adds the mainnet easing row (tau 30, free wait
  36 s) at height `H`.
- `-fastprune` divides the block file size (128 MiB), the preallocation chunks
  (16 MiB and 1 MiB) and the minimum prune target (2200 MiB) by 50, so
  pruning can be tested with blocks 1/50 the normal size. `pruning.py` uses
  it: it needs about 1 GB and a few minutes where it needed 21 GB and up to
  two hours, so it now runs in the default test set.


Upgrading from Prior Versions
==============================

1. **Stop your Whippet daemon**:
   ```bash
   whippet-cli stop
   ```

2. **Upgrade Whippet Core** using your package manager or by building from
   source:
   ```bash
   ./autogen.sh
   ./configure
   make
   make check
   sudo make install
   ```

3. **Start the daemon**:
   ```bash
   whippetd
   ```

4. **Verify the upgrade**:
   ```bash
   whippet-cli getnetworkinfo
   # subversion should read /Devotoshi:1.4.0/
   ```

5. **Confirm the clock**: make sure an NTP client is running and that your
   system time is within a few seconds of UTC.

Miners: do this **before height 371,337**. Everyone else: **before height
600,000**.


Testing
========

- `timing_tests` pins the mainnet rows (the cap from 371,337, the easing
  row from 600,000), the header rules either side of the AuxPoW height, the
  easing arithmetic (doubling per tau, clamping at the proof-of-work limit)
  and the regtest hooks.
- `contrib/timing-sim` is a block-timestamp rule simulator whose difficulty
  and acceptance rules are checked against the real C++ (`timing_ref`
  calls `GetNextWorkRequired()` and `ContextualCheckBlockHeader()`). The
  easing rule agrees with the C++ bit for bit over random chains with gaps
  from 4 s to 1,500 s. Its tests (91) all pass.
- Real regtest nodes replay the far-future, walk-ahead and easing scenarios
  (`contrib/timing-sim/confirm_regtest.py`).
- **Activation rehearsals** with an old and a new binary on regtest:
  - the 72 s cap (`--scenario activation`): old nodes follow every block
    a new miner produces; the new node refuses a block 73 s ahead from the
    activation height and the old node then reorganises onto its chain;
  - the easing fork (`--scenario easing_activation`): from the activation
    height a block mined after a 66 s wait is refused by the other side in
    both directions, and the nodes end on different chains. The "old" node
    in this test is the build before the easing row, not a 1.3.1 binary.
- Full unit test suite: 362 test cases, all passing.
- `slimblocks.py`, `slimundo.py`, `slimprevprefix.py` and `slimprune.py` run
  nodes with the storage options and compare every block, header and
  transaction with a node on the default format, through restarts with and
  without the options, `-reindex`, the `txindex`, deep reorgs and automatic
  pruning. `slimprevprefix.py` also feeds a file with every block before its
  parent to `-reindex` and to `-loadblock`.
- `reconsider_headers.py` reproduces the `reconsiderblock` crash; it aborts a
  node that lacks the fix.

See `doc/timing-fixes.md` for the full write-up, the simulator results and
how to reproduce them.


Release Information
===================

**Release Date:** to be set when tagged
**Version:** 1.4.0
**GitHub Tag:** v1.4.0
**Consensus changes:**
- soft fork at height 371,337 (with AuxPoW): block timestamps at most 72 s ahead of
  adjusted time (`MAINNET_DRIFT_CAP_HEIGHT`)
- **hard fork at height 600,000**: LWMA target eased while the chain waits
  (`MAINNET_EASING_HEIGHT`, tau 30 s, free wait 36 s)

For the latest information, visit:
- GitHub: <https://github.com/chromatic/whippet-experimental-blockchain/>
