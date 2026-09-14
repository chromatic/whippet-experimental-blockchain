Whippet Core version 1.3.0 is now available from:

  <https://github.com/whippet/whippet/releases/tag/v1.3.0/>

This release replaces the UAP covenant format. A position now carries an
explicit *lineage origin* — the identity of the mint it descends from — in
place of the mint-time salt that 1.2.x used. It also adds the rule that stops
a covenant output being created for a lineage the transaction does not spend.

**1.3.0 is not consensus-compatible with 1.2.x at block height 82,000 and
above.** Nodes still running 1.2.x when the chain reaches 82,000 will diverge.
Upgrade before then. See Compatibility below for the 80,000-82,000 window,
which is narrower than it looks.

Please report bugs using the issue tracker at GitHub:

  <https://github.com/whippet/whippet/issues>


Compatibility
==============

Whippet Core is extensively tested on Ubuntu Server LTS, macOS and Windows.
Minimum OS compatibility can be found [in the INSTALL guide](../../INSTALL.md).

Below block 80,000, 1.3.0 and 1.2.x accept exactly the same blocks.

Between 80,000 and 82,000 they differ in one respect, and only in principle:
1.2.x has the covenant opcodes active there and 1.3.0 does not, so a
transaction *spending* a UAP output would be valid to one and not the other.
No such output exists on mainnet, so no such transaction can be built. At and
above 82,000 the two disagree about the covenant format itself.

All nodes must be on 1.3.0 before the chain reaches height 82,000.

The activation height moves from 80,000 to 82,000 in the same motion as the
format change. **No mint has ever been made on mainnet**, so there are no
positions in the old format: nothing on chain is invalidated and nothing needs
migrating.

That is worth naming rather than glossing over. The provenance rule below only
ever *rejects* transactions, so it could have been introduced later as a
tightening — but a tightening is not retroactive, and consensus keeps no
provenance in the UTXO set with which to disown a counterfeit created before
it. Every position fabricated before such a rule shipped would have stayed
valid, and mergeable with genuine ones, permanently. Shipping it before the
first mint is the only version of this that costs nothing.


Notable changes
================

UAP positions carry a lineage, not a salt
--------------------------------------------

The covenant format changes shape:

    mint      <pubkey> <multiplier> OP_MINT
    transfer  <pubkey> <multiplier> <origin32> OP_MINT_TRANSFER

where `origin` is `SHA256(prevout.hash || prevout.n as 4-byte LE)` of the mint
that began the lineage. A mint carries no origin. It cannot: the origin is
derived from the outpoint, the outpoint depends on the txid, and the txid
depends on this very script. Identity is therefore assigned at the mint's
*first spend*, which is the first moment the interpreter can see the outpoint.

The `<salt>` this replaces sat on mints rather than transfers, and was
documented as the token's unique identifier. It was not one. Consensus only
ever checked its length, so uniqueness was an honour-system property that any
minter could collide deliberately — and the salt was dropped at the first
spend regardless, so nothing downstream could have used it even if it had been
unique. The origin has the property the salt only claimed: unique by
construction, because no two outputs can share an outpoint.

### Positions of one token can now be merged

This is the practical consequence, and the reason the change is worth a
consensus break rather than a note in the documentation.

Conservation now groups by `(multiplier, origin)` and sums *every* input
belonging to the lineage being spent. The old rule compared the total of the
covenant outputs against a **single** input's value, so merging two positions
worth V each capped the outputs at V and burned the remainder.

It had to. Without an origin in the script, consensus could not tell "two
positions of one token" from "two tokens that happen to share a multiplier",
and permitting the merge would have let anyone add supply to someone else's
token by posting par backing for it. With the lineage explicit the two cases
are distinguishable, and only the first is allowed.

A transfer must carry its lineage forward unchanged. Re-deriving the origin
from the transfer's own outpoint — which looks reasonable, and is exactly what
a mint does — renames the position into a lineage the transaction has no input
in, and is rejected.

A transfer output cannot be created for a lineage it does not spend
----------------------------------------------------------------------

`CheckUapOutputConservation` runs inside `OP_MINT`/`OP_MINT_TRANSFER`, and
those opcodes execute only when a covenant output is **spent**. An output's
`scriptPubKey` is not executed by the transaction that creates it. A
transaction funded entirely by ordinary P2PKH inputs therefore ran no UAP code
at all, and nothing examined its outputs.

That was exploitable, and cheaply. Anyone could create a covenant output
naming any lineage they liked:

    vin[0]  = an ordinary P2PKH UTXO
    vout[0] = <attacker_pubkey> <multiplier> <someone_else's_origin> OP_MINT_TRANSFER

The result is a counterfeit position: byte-identical on chain to a genuine one,
spendable, and inflating that token's reported supply for the price of one
transaction. An `origin` is 32 bytes and nothing required it to correspond to
an outpoint that ever existed, so the same trick with arbitrary bytes conjured
an entire token — at any multiplier, backed by dust — without paying the
`OP_MINT` entry fee at all.

`CheckUapOutputCreation` (`validation.cpp`) closes it: a transaction may create
a transfer output of group `(multiplier, origin)` only if it also spends an
input of that group, where a mint input contributes the lineage its own
outpoint mints into. Reject reason `bad-txns-uap-output-without-input`.

Three notes on where the rule lives, because each was a way to get it wrong:

* It is a **transaction-level** rule, not a script rule. Script rules only run
  when a UAP input is spent, and the entire attack is that there is no such
  input.
* It runs over **every** transaction in a block, including the coinbase.
  `ConnectBlock` does not route the coinbase through `CheckInputs`, so a rule
  placed there would have left miners as the one party still able to fabricate
  positions.
* It runs **outside** `fScriptChecks`, so `-assumevalid` does not skip it.

Mint outputs stay unrestricted. Creating one is how a lineage comes into
existence, and a mint is inert until spent — at which point the entry fee and
the outpoint-derived lineage are both enforced.

One knock-on effect belongs to the lineage change above rather than to this
rule, and is easy to attribute to the wrong one. A position whose multiplier
is spelled non-canonically does not parse as a covenant, so conservation --
which identifies a lineage's inputs with `ParseUapOutputScript` — does not
count it, leaving the covenant output such a spend must produce exceeding a
zero input total. In 1.2.1 such a position was merely non-standard to relay
and a miner could still mine one; from this release it is unspendable
outright. `CheckUapOutputCreation` refuses the same transaction, and refuses
it first, so that is the reason a node reports — but it is not what made the
position unspendable.

Token metadata: `name` removed, tickers restricted to `[A-Z0-9]{1,8}`
------------------------------------------------------------------------

Token metadata rides in an `OP_RETURN` on the mint transaction. The record is
now four pushes rather than five:

    OP_RETURN "WUAP" <0x02> <ticker> <metadata_hash>

`metadata_hash` is 0 or 32 bytes; the whole `scriptPubKey` is 11 to 50 bytes,
against the 83-byte relay limit. Version `0x01` is rejected outright, not
parsed.

The old bounds were not simultaneously satisfiable — a 16-byte ticker plus a
64-byte name plus a 32-byte hash is 112 bytes of payload against a 72-byte
budget — so the format's declared maxima were unreachable in practice.

`name` is gone rather than shrunk. It was free-text and unauthenticated:
anyone could mint a token named `Dogecoin`, so minter-supplied text in a block
carried no more authority than minter-supplied text anywhere else. It only
*looked* authoritative. Richer metadata belongs behind `metadata_hash`, which
commits to it immutably.

With `name` gone the ticker is the only human-readable identifier, which makes
its character set a security question rather than a cosmetic one: arbitrary
UTF-8 lets `WHIP` be minted with a Cyrillic `Р` that renders identically in a
marketplace listing. Restricting to `[A-Z0-9]` kills homoglyphs outright, makes
8 bytes exactly 8 characters, and lets the UTF-8 and control-character checks
be deleted rather than merely tightened. Lowercase is folded to uppercase in
the wallet before the script is built, and never on read, so one displayed
ticker has exactly one canonical byte string.

This is **not** a consensus change. `Solver()` classifies any push-only
`OP_RETURN` as `TX_NULL_DATA` and never decomposes the payload, so push count,
charset and field layout are invisible to consensus and policy alike. The
format is documented in [doc/uap-token-metadata.md](../uap-token-metadata.md),
which both implementations are written against.

Marketplace stack
--------------------

`contrib/` gains the pieces needed to actually use a token, all off-chain:

* **`uap-indexer`** — indexes positions and lineages, and relays non-custodial
  signed orders. Order listings now carry the position's backing value and
  multiplier, so a buyer can see what an ask is backed by without a second
  fetch. Orders whose position is already being spent by an unconfirmed
  transaction are hidden, which closes a double-fill race that previously
  surfaced as `bad-txns-inputs-missingorspent` *after* the loser had committed.
  That includes the fill the relay has just broadcast itself: the book is
  updated before the broadcast is acknowledged, rather than on the next
  mempool poll, which is what leaves the tightest version of the race open.
* **`uap-js` / `uap-web`** — a self-custody browser wallet: mint, transfer,
  sell, fill and cancel. `buildMeltTx` redeems a position's backing to
  spendable WHIP, which was consensus-legal from the start but had no
  implementation, so a minter's coins were locked until somebody bought them.
* **`uaptx`** — the Go port of the signing and taker-fill paths, checked
  against byte-exact fixtures generated by the JavaScript implementation.

The indexer's `CurrentSchemaVersion` is bumped, which makes an existing state
file fail to open until it is reindexed. That is deliberate: metadata is
persisted rather than re-derived, so an old database holds records under the
retired format, and `json.Unmarshal` would silently ignore the dead fields and
go on serving tickers no new mint could produce.


Known limitations
==================

**`OP_INSPECT`'s virtual-balance selector is lineage-blind.** Selector 11
returns `nValue * multiplier` for an output without consulting its origin, so
two positions of different lineages that share a multiplier report identical,
interchangeable balances. This is not currently reachable —
`ParseUapOutputScript` accepts only the exact 3- and 4-element covenant shapes,
so no UAP output can contain `OP_INSPECT`, and nothing in the marketplace uses
it — but it is a trap for any future P2SH contract written against it. Adding a
lineage-aware selector later is a soft fork; changing selector 11's meaning
would not be.

**The `2^48` virtual-balance bound is stated more tightly than it is
enforced.** The guard computes `base_coin` as `nValueIn / COIN`, integer
division, so a position of 1.99999999 WHIP counts as 1 and the true virtual
balance can be nearly double the documented ceiling. A position under 1 WHIP
has `base_coin == 0`, which short-circuits the guard entirely. Neither is
exploitable — the multiplication is separately overflow-checked — but a
contract written against a strict 2^48 ceiling would be wrong.

**`OP_INSPECT` and `OP_INSPECT_SELF` are not height-gated** the way
`OP_MINT`/`OP_MINT_TRANSFER` are: `SCRIPT_VERIFY_UAP_MINT` gates only the
latter pair. This is inconsistent rather than harmful here, since there are no
pre-existing nodes to stay compatible with, but enabling opcodes is the
hard-fork direction and the activation design should say so in one voice.


Testing
========

The consensus rules above are covered at two levels, deliberately, because
they fail differently:

* `src/test/uap_mint_tests.cpp` and `src/test/uap_creation_tests.cpp` drive the
  checks directly. The creation tests are a separate file for a reason: every
  fixture in `uap_mint_tests.cpp` verifies a UAP *input*, which is what makes
  the interpreter run at all, so a transaction that spends no covenant cannot
  be expressed there.
* `qa/rpc-tests/uap_output_provenance.py` proves the rule is *wired in*, which
  a correct rule that nothing calls would still pass every unit test. It
  asserts the mempool path and the `ConnectBlock` path separately — via
  `submitblock`, since a miner does not ask the mempool's permission — and
  covers the coinbase case on its own.

The UAP functional tests share one definition of the covenant format, in
`qa/rpc-tests/test_framework/uap.py`. Five separate copies of it is how all
five files came to be testing the retired format simultaneously while staying
green: a script the interpreter no longer recognises is simply a script, and a
test asserting only "this was rejected" cannot tell "rejected by the rule I am
testing" from "rejected because this is no longer a covenant at all". Every
negative case now has a positive counterpart, and asserts the specific reject
reason rather than the fact of rejection.


Release Information
===================

**Release Date:** August 31, 2026
**Version:** 1.3.0
**GitHub Tag:** v1.3.0
**Consensus activation:** block 82,000 (`UAPMintHeight`)

For the latest information, visit:
- Website: <https://whippet.com/>
- GitHub: <https://github.com/whippet/whippet/>
