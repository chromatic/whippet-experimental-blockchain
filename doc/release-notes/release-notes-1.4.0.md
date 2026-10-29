Whippet Core version 1.4.0 is now available from:

  <https://github.com/chromatic/whippet-experimental-blockchain/releases/tag/v1.4.0/>

This release hardens block timestamps and changes how difficulty reacts when
the chain stops finding blocks. It contains **two consensus changes at two
different heights**, and the second is a hard fork:

| Height | Change | Kind | Who must upgrade |
|--------|--------|------|------------------|
| 360,000 | Block timestamps may be at most 72 s ahead of adjusted time (was 2 hours) | soft fork | most hashrate, before the height |
| 600,000 | Difficulty eases while the chain waits for a block (LWMA with easing) | **hard fork** | **every miner and node, before the height** |

Please report bugs using the issue tracker at GitHub:

  <https://github.com/chromatic/whippet-experimental-blockchain/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../../INSTALL.md).

**Height 360,000 (soft fork).** From this height, upgraded nodes refuse a
block whose timestamp is more than 72 s ahead of their adjusted time. Every
block that upgraded nodes accept is also accepted by older nodes, so older
nodes keep following the chain. What matters is the miners: a miner on 1.3.1
or earlier that stamps a block more than 72 s ahead produces a block the
upgraded nodes refuse, and its blocks are orphaned until it upgrades.
Miners and pools should upgrade before height 360,000.

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
per block) height 360,000 is about two weeks after the 1.4.0 release and
height 600,000 about a month. The rate is not constant, so treat these as
rough: watch `getblockcount`, not the calendar.

There is no wallet or chain-data migration; the on-disk format is unchanged.


What changed in block timestamps
=================================

**The problem.** A block must be stamped at least 4 s after its parent (the
spacing floor), so one block stamped ahead of real time forces every
descendant to be stamped at or after it. Until now the limit on "ahead" was
a hard-coded 2 hours. A miner could walk the chain clock up to 7,200 s ahead
of real time and collect a share of blocks well above its hashrate. In the
simulator, an attacker with 0.3 of the hashrate took 0.38 of the blocks under
the old rules and about 0.25 under a 72 s cap.

**The cap (soft fork, height 360,000).** The future-time limit is now a
consensus parameter, `nMaxFutureDrift`. It is 7,200 s below height 360,000
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
AuxPoW blocks become acceptable at height 371,337, between the two heights
above; a standalone block whose version carries the chain ID (0x5750) stays
valid before and after it, and only legacy-format blocks (version 1, or
version 2 with no chain ID) are refused from that height. Check that your
miner takes the version field from `getblocktemplate`.


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

Miners: do this **before height 360,000**. Everyone else: **before height
600,000**.


Testing
========

- `timing_tests` pins the mainnet rows (the cap from 360,000, the easing
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
- Full unit test suite: 350 test cases, all passing.

See `doc/timing-fixes.md` for the full write-up, the simulator results and
how to reproduce them.


Release Information
===================

**Release Date:** to be set when tagged
**Version:** 1.4.0
**GitHub Tag:** v1.4.0
**Consensus changes:**
- soft fork at height 360,000: block timestamps at most 72 s ahead of
  adjusted time (`MAINNET_DRIFT_CAP_HEIGHT`)
- **hard fork at height 600,000**: LWMA target eased while the chain waits
  (`MAINNET_EASING_HEIGHT`, tau 30 s, free wait 36 s)

For the latest information, visit:
- GitHub: <https://github.com/chromatic/whippet-experimental-blockchain/>
