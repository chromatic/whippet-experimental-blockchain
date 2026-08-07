Whippet Core version 1.2.1 is now available from:

  <https://github.com/whippet/whippet/releases/tag/v1.2.1/>

This is a bug-fix release for 1.2.0. It fixes a remotely triggerable halt in
block template production, and tightens the UAP output format to require
canonical push encodings.

**1.2.1 is not consensus-compatible with 1.2.0 past block height 80,000.**
Nodes still running 1.2.0 when the chain reaches 80,000 will diverge. Upgrade
before then.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/whippet/whippet/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../INSTALL.md).

Below block 80,000, 1.2.1 and 1.2.0 accept exactly the same blocks. At and
above 80,000 they do not: see "UAP outputs must use canonical push encodings"
below. All nodes must be on 1.2.1 before the chain reaches height 80,000.


Notable changes
================

Fixed: a single transaction could stop block production
---------------------------------------------------------

`SCRIPT_VERIFY_UAP_MINT` was a compile-time member of both
`STANDARD_SCRIPT_VERIFY_FLAGS` and `MANDATORY_SCRIPT_VERIFY_FLAGS`. The
activation rule it implements is not a compile-time constant — it is gated on
`UAPMintHeight` — so the mempool and `ConnectBlock` disagreed about it below
that height, in the worst possible direction.

Creating a UAP output is legal at any height: output scripts are not executed
when they are created, only when they are spent. So a covenant output can be
mined today, and *spending* it is what runs `OP_MINT_TRANSFER`. Pre-activation,
the mempool executed that opcode anyway and accepted the spend, while
`ConnectBlock`, which derives the flag from the block's own height, rejected
it. `BlockAssembler::CreateNewBlock` pulled the transaction into a template,
`TestBlockValidity` refused the template, and `CreateNewBlock` threw. One
cheap transaction from any peer stopped that node producing block templates at
all until the transaction was evicted from its mempool.

The flag is now derived from a height in exactly one place, `GetUapMintFlags()`
in `validation.cpp`, which `ConnectBlock` calls with the block's height and
`AcceptToMemoryPool` calls with `chainActive.Height() + 1`. It is applied
*after* `-promiscuousmempoolflags` is read, since an activation height is
consensus and not something that debugging knob may switch off.

A spend of a UAP output submitted before activation is now refused by the
mempool up front, with reject reason `premature-uap-spend` and a DoS score of
0. Relaying a transaction that is merely early is not misbehaviour, and the
generic script-failure path bans at 100 — which, applied here, would have
partitioned the network across the activation boundary. Nodes cannot tell
"invalid" from "not yet active" once script execution has failed: both surface
as `SCRIPT_ERR_BAD_OPCODE`.

UAP outputs must use canonical push encodings
------------------------------------------------

As of block 80,000, every element of a UAP output script — pubkey, multiplier
and salt — must use its canonical (shortest) push: `OP_0` for a zero multiplier, `OP_1`..`OP_16` for multipliers 1 through
16, and a minimal data push for anything longer.

This is what every script builder already emits (`CScript::operator<<`,
Python's `CScript([...])`, `contrib/uap-js`), so ordinary tooling is
unaffected. It does invert which encoding is accepted for multipliers 1..16:
1.2.0's parser accepted a direct data push and rejected `OP_1`..`OP_16`, and
1.2.1 does the opposite. That is why the two are not consensus-compatible past
80,000.

The rule closes a gap between consensus and relay policy. A covenant's
scriptPubKey is executed when the position is spent, and standard relay policy
applies `SCRIPT_VERIFY_MINIMALDATA` to that execution. 1.2.0 accepted, as a
UAP output, precisely the multiplier encoding `MINIMALDATA` rejects — so a
position with a multiplier of 1..16 would have been consensus-valid but
movable only by a non-standard transaction. Requiring the canonical form on
both sides makes those multipliers usable, guarantees that every UAP output
consensus honours can be spent by a standard relayable transaction, and gives
each position exactly one byte representation.

One parser, not three
------------------------

`ParseUapOutputScript` now lives in `src/script/script.cpp` and is the single
authority on UAP output shape. `Solver()`'s `TX_OP_MINT`/`TX_OP_TRANSFER`
classification previously carried its own, looser copy — it accepted any
pubkey length from 33 to 65, where consensus accepts only 33 or 65 — and now
calls the shared parser, so mempool policy cannot disagree with consensus
about what a UAP output is.

The same shape rules are pinned from all four implementations by a shared
fixture, `src/test/data/uap_script_vectors.json`, read by the C++ unit tests,
`contrib/uap-js` and `contrib/uap-indexer`.


Upgrading from Prior Versions
==============================

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
   # Should show version 1.2.1
   ```

No reindex or wallet migration is required.


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

**Release Date:** August 7, 2026
**Version:** 1.2.1
**GitHub Tag:** v1.2.1

For the latest information, visit:
- Website: <https://whippet.com/>
- GitHub: <https://github.com/whippet/whippet/>
