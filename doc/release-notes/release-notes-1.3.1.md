Whippet Core version 1.3.1 is now available from:

  <https://github.com/chromatic/whippet-experimental-blockchain/releases/tag/v1.3.1/>

This is an urgent bugfix release. It fixes a chain-halting validation bug:
every node running 1.3.0 or earlier stops accepting *any* block, including
its own, at block height 80,000. Mainnet reached that height on 2026-09-21
and has been unable to progress since. There is no consensus-format change
and no new feature in this release — it corrects a single validation
lookup that used the wrong block height.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/chromatic/whippet-experimental-blockchain/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../../INSTALL.md).

This release changes no consensus rule and requires no new activation height.
It is a straightforward safety fix: a node running the old, buggy code simply
cannot build or accept any block at height 80,000 or above, so there is no
way for it to diverge onto a different chain. Upgrading is what lets a node
make progress again; not upgrading leaves it stalled, never wrong.


The bug
========

At block height 80,000, `chainIdFixConsensus` (added in 1.2.0 to resolve an
AuxPoW chain ID collision with Dogecoin) changes the network's expected
AuxPoW chain ID from `0x0062` to `0x5750`. `ConnectBlock()` already looked
this up per-height correctly. But an earlier, more permissive check —
`CheckBlockHeader()`, which runs before a block's real height is known (for
example, before its previous block has even been looked up) — always
validated against `Params().GetConsensus(0)`: the chain ID as of height 0,
which never changes. That was harmless for as long as the chain ID itself
never changed. Once it did, at height 80,000, `CheckBlockHeader()` started
rejecting every block, mined locally or received from a peer, with:

    CheckAuxPowProofOfWork : block does not have our chain ID
    (got 22352, expected 98, full nVersion ...)

Because this check runs ahead of the real per-height validation in
`ContextualCheckBlockHeader()`, no block ever got far enough to reach the
correct check. The chain simply stopped.


The fix
========

`CheckBlockHeader()` now estimates the height as `chainActive.Height() + 1`
instead of a fixed `0`. A block being validated almost always extends the
current tip, so this estimate is correct in every normal case. It does not
need to be exact: the authoritative, correctly-scoped check still happens
afterward in `ContextualCheckBlockHeader()`, which has the block's real
height via `pindexPrev` and is unchanged by this release.

A new regression test (`src/test/auxpow_tests.cpp`,
`checkblockheader_uses_tip_height_for_chain_id`) proves the fix directly: it
points `chainActive`'s tip at a synthetic height-79999 index and checks that
a block carrying the height-80000 chain ID is now accepted, while one
carrying the pre-80000 chain ID is rejected — the reverse of what the
original, fixed `GetConsensus(0)` lookup did.


Upgrading from Prior Versions
==============================

Every node currently on 1.3.0 or earlier is stalled at or below height
79,999 and needs this upgrade to make further progress. There is no wallet
or chain-data migration; the on-disk format is unchanged.

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
   # subversion should read /Devotoshi:1.3.1/
   ```

5. **Confirm the chain is moving again**:
   ```bash
   whippet-cli getblockcount
   # run again a few minutes later and confirm it has increased
   ```


Testing
========

- `src/test/auxpow_tests.cpp` gained
  `checkblockheader_uses_tip_height_for_chain_id`, which fails against the
  pre-fix code (reintroducing `Params().GetConsensus(0)` makes both of its
  assertions fail in the direction the original bug did) and passes against
  the fix, without mining through height 80,000.
- Full unit test suite: 314 test cases, all passing.


Release Information
===================

**Release Date:** 2026-09-21
**Version:** 1.3.1
**GitHub Tag:** v1.3.1
**Fixes:** chain-halting `CheckBlockHeader` chain-ID validation bug at block
height 80,000 (no consensus-format change)

For the latest information, visit:
- GitHub: <https://github.com/chromatic/whippet-experimental-blockchain/>
