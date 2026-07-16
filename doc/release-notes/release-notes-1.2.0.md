Whippet Core version 1.2.0 is now available from:

  <https://github.com/whippet/whippet/releases/tag/v1.2.0/>

This is a major version release fixing a critical bug in OP_MINT/OP_MINT_TRANSFER
(the tokens introduced in 1.1.0 could be spent by anyone, not just their intended
recipient), and fixing a merge-mining AuxPoW chain ID collision with Dogecoin.
Both changes are consensus changes activated at block height 80,000 on mainnet
and testnet. All nodes must upgrade to 1.2.0 to remain on the main chain past
that height.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/whippet/whippet/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../INSTALL.md).

Note: This release contains consensus changes activating at block 80,000 and is
not compatible with prior versions past that height. All nodes must upgrade to
1.2.0 before the chain reaches height 80,000.


Notable changes
================

Consensus Changes
==================

Both of the following changes are height-gated to activate at block 80,000 on
mainnet and testnet (0, i.e. genesis, on regtest). Before that height, behavior
is unchanged from 1.1.0.

OP_MINT / OP_MINT_TRANSFER: signature requirement and covenant enforcement
----------------------------------------------------------------------------

1.1.0 shipped `OP_MINT` as a scriptPubKey opcode that validated its minting
conditions (entry fee, salt length, overflow) and then unconditionally
succeeded, with no scriptSig requirement at all. In practice, any real WHIP
value locked in a mint output could be spent by anyone who saw it, not just
its intended owner. Two of the five safety properties from the original UAP
design ("one-shot" and "strict script" covenant enforcement) were also
unimplemented.

As of 80,000:

- `OP_MINT` requires a valid signature from a recipient pubkey now included
  in the script format: `<pubkey> <multiplier> <salt> OP_MINT`. Signature and
  pubkey encoding are checked unconditionally (not gated on mempool-policy
  flags), so a malformed-encoding signature or pubkey can no longer reach a
  mined block.
- A new `OP_MINT_TRANSFER` opcode enforces covenant continuation at the
  consensus level: spending a mint/transfer position requires the transaction
  to produce at least one continuing `OP_MINT_TRANSFER` output carrying the
  same multiplier, with total output value bounded by the input's value.
  Token conservation is enforced by consensus, not by convention.
- One-shot is enforced by checking that no sibling input in a mint
  transaction is itself an unspent UAP position.
- Non-covenant outputs are now allowed alongside a covenant continuation, to
  support atomic token-for-WHIP swaps in a single transaction.

Before 80,000, `OP_MINT`/`OP_MINT_TRANSFER` behave exactly as they did in
1.1.0. There is no change to already-mined blocks below that height.

AuxPoW chain ID collision fix
-------------------------------

Whippet's AuxPoW chain ID (`0x0062`) collided with Dogecoin's own registered
merge-mining chain ID. As of block 80,000, Whippet uses a distinct chain ID
(`0x5750`, "WP") to prevent ambiguity for merge-mining pools.

Upgrading from Prior Versions
==============================

**Important:** This release contains consensus changes activating at block
80,000. Follow these steps:

1. **Backup your wallet** before upgrading:
   ```bash
   whippet-cli dumpwallet backup.txt
   ```

2. **Stop your Whippet daemon**:
   ```bash
   whippet-cli stop
   ```

3. **Upgrade Whippet Core** using your package manager or by building from source

4. **Start the daemon**:
   ```bash
   whippetd
   ```

5. **Verify the upgrade**:
   ```bash
   whippet-cli getinfo
   # Should show version 1.2.0
   ```


New Features
============

Client-side and indexing tooling for UAP
-------------------------------------------

Since none of this was previously usable end-to-end (OP_MINT was unsafe to
use), this release also adds the surrounding tooling needed to actually use
UAP tokens:

- `contrib/uap-js`: a dependency-free JavaScript library for building and
  signing UAP transactions client-side, in a browser or Node.
- `contrib/uap-indexer`: a small Go service that indexes UAP positions from a
  node's RPC and hosts a non-custodial maker/taker order relay
  (`SIGHASH_SINGLE|ANYONECANPAY`), including a Docker image and an operator's
  guide for running your own instance, and order mirroring across
  independently-run instances.

See `doc/uap-minting-guide.md`, `doc/uap-marketplace-design.md`, and
`doc/uap-marketplace-operators-guide.md`.


Building and Installation
==========================

Standard build process applies:

```bash
./autogen.sh
./configure
make
make check
sudo make install
```

For detailed build instructions, see [INSTALL.md](../INSTALL.md).


Known Issues
============

None known at release time. Please report any issues at:
<https://github.com/whippet/whippet/issues>


Release Information
===================

**Release Date:** July 16, 2026
**Version:** 1.2.0
**GitHub Tag:** v1.2.0

For the latest information, visit:
- Website: <https://whippet.com/>
- GitHub: <https://github.com/whippet/whippet/>
