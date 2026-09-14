# UAP Minting and Token Spending Guide

How to mint a UAP token on Whippet, move it, and redeem it.

The normative reference for the covenant format is
`ParseUapOutputScript` in `src/script/script.cpp`; for the spend rules it is
the `OP_MINT`/`OP_MINT_TRANSFER` case in `src/script/interpreter.cpp`. Where
this guide and that code disagree, the code is right — and please fix the
guide.

## Table of Contents

1. [Overview](#overview)
2. [Lineage: what identifies a token](#lineage-what-identifies-a-token)
3. [Creating a mint](#creating-a-mint)
4. [The first spend](#the-first-spend)
5. [Transfers](#transfers)
6. [Redeeming backing (melt)](#redeeming-backing-melt)
7. [Complete example](#complete-example)
8. [Introspection opcodes](#introspection-opcodes)
9. [Testing](#testing)

## Overview

A UAP position is an ordinary transaction output whose `scriptPubKey` is a
covenant. Like any Bitcoin-style script it is evaluated when the output is
*spent*, not when it is created, so almost every rule below is enforced at
spend time against the position's own declared fields.

There are two shapes, and only two:

```
mint      <recipient_pubkey> <multiplier> OP_MINT
transfer  <recipient_pubkey> <multiplier> <origin32> OP_MINT_TRANSFER
```

### Rules enforced when a position is spent

Numbered as in `src/script/interpreter.cpp`:

0. **Activation.** Before `UAPMintHeight` both opcodes are disabled entirely,
   identical to an undefined opcode.
1. **Signature.** Only the `recipient_pubkey` embedded in the script may
   authorize the spend. Strict DER, low-S and a defined hashtype are required
   unconditionally, not gated on policy flags.
2. **Entry fee** *(mints only)*. A fresh mint must hold at least
   `1000 * COIN` = **100,000,000,000 satoshis (1,000 WHIP)**.
3. **Origin width** *(transfers only)*. The origin must be exactly 32 bytes.
4. **Multiplier range and overflow.** `0 <= multiplier <= 2147483647`, and
   `(input value in whole coins) * multiplier <= 2^48`.
5. **One-shot** *(mints only)*. A transaction spending a mint may not also
   spend another UAP position as a sibling input.
6. **Conservation.** See [Transfers](#transfers).

### The rule enforced when a position is *created*

One rule does not wait for the spend, because it cannot:
`CheckUapOutputCreation` in `src/validation.cpp` requires that **a transaction
creating a transfer output of lineage `(multiplier, origin)` also spends an
input of that lineage.**

It has to live outside the script interpreter. Creating an output executes no
script, so a transaction funded entirely by ordinary inputs runs no covenant
code at all — and without this rule it could mint a position into any lineage
it named, counterfeiting supply for the price of one transaction. Mint outputs
are exempt: creating one is how a lineage begins, and a mint is inert until
spent.

### Multiplier and token quantity

The multiplier declares how many token units each satoshi of backing
represents. A position's quantity is:

```
units = value_in_satoshis * multiplier
```

**Example.** 1,000 WHIP is 100,000,000,000 satoshis. At multiplier 1,000 that
position carries 100,000,000,000,000 units.

The satoshis locked in a position are not a fee — they *are* the token, at a
fixed ratio. Value can leave a position, but token quantity leaves in exact
proportion, which is what makes a position's backing verifiable by anyone
holding the outpoint.

### Why a position feels permanent to whoever buys one

The rules above have a consequence worth stating on its own, because it is
the whole reason backing is a meaningful promise rather than a marketing
claim: **buying a position is not exposure to what anyone else does with
theirs.**

- **Melting is self-service only.** Rule 1 above requires the signature of
  the `recipient_pubkey` embedded in *that* position's own script — there is
  no other way to authorize a spend. Nobody can melt, transfer, or otherwise
  touch a position they do not hold the key to. Your backing cannot be
  withdrawn by anyone but you.
- **Backing is per position, never pooled.** There is no shared reserve that
  positions draw against. One satoshi at multiplier `M` is `M` token units,
  in every position carrying that multiplier, always — see "Multiplier and
  token quantity" above. Another holder's melt does not change your
  position's declared value, its multiplier, or the ratio between them: it
  cannot, because their spend is a different transaction touching a
  different UTXO. Nothing about your position's script or backing is
  read, written, or referenced by theirs.
- **A melt shrinks supply and backing together, in the same proportion, and
  touches nothing else.** `CheckUapOutputConservation` (`src/script/
  interpreter.cpp`) enforces `nValueOut <= nValueIn` on the lineage being
  spent, and only on the inputs actually included in that transaction.
  Melting position A removes exactly A's value and exactly the units that
  value represented — from total supply and from nothing else. Position B,
  held by someone else, is not an input to A's spend, so it is not
  evaluated, not touched, and not diminished. If anything, B's *share* of
  the lineage's remaining supply goes up, not down, because the denominator
  just got smaller while B's own numerator did not move.
- **The honest caveat.** A minter who still holds most of a token's supply
  can melt their own position down to dust and walk away with most of its
  backing. Watched from outside, that looks exactly like a rug pull — total
  supply and total backing both drop sharply, all at once. It is one,
  though, in an important sense: it is the minter withdrawing money that was
  always theirs, at the ratio they locked it in at, from a position only
  they could ever spend. Anyone who *bought* a position from that minter
  still holds their own coins, at their own multiplier, completely
  undiminished — the melt is not a claim against their backing, because
  there was never a claim to make; it was never pooled with the minter's in
  the first place. The caution this leaves standing is about concentration,
  not about the mechanism: a token where one address still holds most of the
  supply is one where that address can walk away with most of the backing,
  and that is a fact worth checking about a token before buying into it, not
  a flaw in what buying a position guarantees you once you hold it.
- **The entry fee is a floor, not a fixed amount.** Rule 2 rejects a mint
  locking *less* than `1000 * COIN` (`if (fIsMint && nValueIn < 1000 * COIN)`
  in `src/script/interpreter.cpp`); it does not reject locking more. A mint
  may lock any amount at or above the floor, and — like every other
  position — that value is never burned. It is ordinary locked backing,
  recoverable in full (minus dust and fee) by melting the position, the same
  as backing added at any later spend.

> Note on the overflow guard: rule 4 computes whole coins as
> `nValueIn / COIN`, integer division, so a position of 1.99999999 WHIP counts
> as 1 and a position under 1 WHIP counts as 0 (which short-circuits the guard
> entirely). The bound is therefore looser than `2^48` suggests. Nothing is
> exploitable — the multiplication is separately overflow-checked — but do not
> write a contract that relies on `2^48` being exact.

## Lineage: what identifies a token

A token is a **lineage**, identified by 32 bytes:

```
origin = SHA256(mint_outpoint.txid_internal_bytes || mint_outpoint.n as 4-byte LE)
```

`txid_internal_bytes` is the txid in internal byte order — reversed relative
to the display form you get from RPC. Getting that backwards produces a
plausible-looking 32 bytes that no node will agree with.

Three consequences worth internalising:

- **A mint carries no origin.** It cannot: the origin depends on the outpoint,
  the outpoint depends on the txid, and the txid depends on the script. The
  lineage is assigned at the mint's *first spend*, which is the first moment
  the interpreter can see the outpoint.
- **A transfer carries its lineage forward, unchanged.** Re-deriving the
  origin from a transfer's own outpoint looks reasonable — it is exactly what
  a mint does — and is rejected, because it renames the position into a
  lineage the transaction has no input in.
- **Conservation groups by `(multiplier, origin)`**, so two positions of one
  token can be merged, while two tokens that merely share a multiplier cannot.

This replaced a mint-time `<salt>`, which was documented as the token's unique
identifier but never was one: consensus only checked its length, so uniqueness
was an honour-system property any minter could collide deliberately, and the
salt was discarded at the first spend regardless. An outpoint is unique by
construction.

## Creating a mint

Creating the output runs no script. You need an ordinary wallet spend whose
new output carries the mint script and at least 1,000 WHIP.

**Encoding:** every element must use its *canonical* (shortest) push — `OP_0`
for a zero multiplier, `OP_1`..`OP_16` for 1..16, and a minimal data push for
anything longer. Any script builder gives you this for free (Python's
`CScript([...])`, C++'s `CScript::operator<<`, `uap.js`'s `buildMintScript`);
it only matters if you assemble bytes by hand. The rule exists because the
script is *executed* when the position is spent, and `SCRIPT_VERIFY_MINIMALDATA`
demands that same canonical encoding then. Consensus therefore accepts only
encodings that are also relayable: there is exactly one way to write a given
position, and no way to create one that cannot later be moved.

```python
from test_framework.uap import mint_script, make_key, COIN, MINT_ENTRY_FEE

minter = make_key(b"m" * 32)
script = mint_script(minter.get_pubkey(), 1000)   # <pubkey> <1000> OP_MINT
```

Fund it like any other output — see `create_output()` in
`qa/rpc-tests/uap_transactions.py`, which does exactly this: build a raw
transaction, overwrite `vout[0].scriptPubKey`, keep a change output so the
remainder does not become an absurd fee, sign, broadcast.

## The first spend

This is where the lineage is born. The covenant output must carry
`origin_of(mint_txid, mint_vout)` — the outpoint being spent — and nothing
else is accepted.

```python
from test_framework.uap import transfer_script, origin_of, sign_spend, FEE
from test_framework.mininode import CTransaction, CTxIn, CTxOut, COutPoint, ToHex

lineage = origin_of(mint_txid, 0)

tx = CTransaction()
tx.vin  = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
tx.vout = [CTxOut(mint_value - FEE,
                  transfer_script(recipient.get_pubkey(), 1000, lineage))]
tx.vin[0].scriptSig = sign_spend(script, minter, tx, 0)
node.sendrawtransaction(ToHex(tx))
```

The scriptSig is just `<sig>`. The pubkey, multiplier and origin all come from
the `scriptPubKey` being spent, never from the scriptSig — and the signature
is made over that exact script as the scriptCode, so use the bytes the chain
has rather than reassembling them.

## Transfers

Spending a transfer works the same way, except the lineage is read from the
input's script instead of derived.

Conservation (rule 6) requires, for the lineage being spent:

1. **at least one** output that is a covenant of the same `(multiplier,
   origin)`;
2. the total value of that lineage's outputs must not exceed the total of its
   inputs — summed across *every* input of that lineage in the transaction,
   which is what allows two positions of one token to be merged;
3. no output may be mint-shaped;
4. any covenant output of a *different* lineage is permitted only if that
   lineage also has an input in the same transaction.

**Outputs that are not covenants at all are unrestricted.** This is the point
that makes trading possible: a plain P2PKH payment leg can ride alongside the
covenant leg in one atomic transaction, which is exactly how a maker/taker
swap settles. (An earlier version of this guide said every output had to be a
covenant. That was true once and has not been since non-covenant siblings were
allowed.)

Value may leave a position — the difference becomes miner fee, or a payment,
or change — and token quantity leaves with it in proportion. Nothing creates
tokens.

## Redeeming backing (melt)

Because a position needs only *one* surviving same-lineage covenant output,
you can recover most of its backing as ordinary spendable WHIP: send a
dust-valued covenant onward and take the remainder to a plain output.
`uap-js`'s `buildMeltTx` does this.

A position can never be melted to zero — a covenant output always survives —
so "fully redeemable" means "redeemable minus dust and fee".

## Complete example

The end-to-end flow, in runnable form, is
`qa/rpc-tests/uap_mint_transfer.py`: mint, reject the wrong signer, reject a
non-covenant destination, reject value creation, reject a foreign origin on
the first spend, transfer, reject an origin rewrite on the onward hop, and
assert the lineage survived both hops unchanged.

Rather than duplicating it here — where it would rot, as the previous version
of this section did — read that file. Its helpers live in
`qa/rpc-tests/test_framework/uap.py`, which is the single Python definition of
the format:

```python
mint_script(pubkey, multiplier)
transfer_script(pubkey, multiplier, origin)
origin_of(txid_hex, n)
sign_spend(script_code, key, tx, n_in, hashtype=SIGHASH_ALL)
```

Two notes for anyone adapting older code: the transaction primitives are in
`test_framework.mininode` (not `test_framework.messages`), and this tree's
`SignatureHash(script, txTo, inIdx, hashtype)` returns a `(hash, err)` tuple,
so the error must be checked rather than the result used directly.

## Introspection opcodes

### `OP_INSPECT` — transaction introspection

```
[index] selector OP_INSPECT -> value
```

The selector is on top of the stack and is popped first; selectors 10-12 then
pop an output index from beneath it. So the script order is
`<index> <selector> OP_INSPECT`. Any selector not listed below is an error.

| Selector | Index? | Pushes |
|---|---|---|
| 0 | no | transaction version |
| 1 | no | index of the input being verified |
| 2 | no | input count |
| 3 | no | output count |
| 10 | yes | `vout[index].nValue` |
| 11 | yes | virtual balance of `vout[index]` (see below) |
| 12 | yes | `vout[index].scriptPubKey` |

Example — require that output 0 of the *spending* transaction pays exactly
1,000 satoshis:

```
0 10 OP_INSPECT 1000 OP_EQUAL
```

Note the ordering, and note that this constrains the transaction that
*spends* the output carrying the script, not the one that creates it. An
earlier version of this guide documented `0 OP_INSPECT` as reading `nValue`;
selector 0 is the transaction version, and a script written that way compares
the version against your expected value instead.

**Selector 11, virtual balance**, is `nValue * multiplier` for a covenant
output, and 0 for an output that is not a covenant at all. **It does not
consult the output's origin**, so two positions of different lineages that
share a multiplier report identical, interchangeable balances. That ambiguity
is currently unreachable — `ParseUapOutputScript` accepts only the two exact
covenant shapes, so no covenant can contain `OP_INSPECT` — but do not build on
selector 11 expecting it to identify a token.

### `OP_INSPECT_SELF` — script introspection

```
OP_INSPECT_SELF -> scriptPubKey
```

Pushes the currently-executing script, enabling self-referential covenants:

```
OP_INSPECT_SELF OP_INSPECT_SELF OP_EQUAL
```

Neither introspection opcode is height-gated the way
`OP_MINT`/`OP_MINT_TRANSFER` are: `SCRIPT_VERIFY_UAP_MINT` gates only the
latter pair.

## Testing

```bash
# C++ unit tests for the covenant and creation rules
./src/test/test_whippet --run_test=uap_mint_tests
./src/test/test_whippet --run_test=uap_creation_tests

# Live regtest coverage
qa/rpc-tests/uap_transactions.py          # rules 2, 3, 4, one at a time
qa/rpc-tests/uap_mint_transfer.py         # a position's whole life
qa/rpc-tests/uap_mint_transfer_spend.py   # chains, splits, introspection
qa/rpc-tests/uap_canonical_encoding.py    # canonical push encodings
qa/rpc-tests/uap_swap.py                  # maker/taker atomic swap
qa/rpc-tests/uap_output_provenance.py     # the creation rule, mempool + block
```

## References

- [Token metadata format](uap-token-metadata.md) — the `OP_RETURN` ticker record
- [Marketplace design](uap-marketplace-design.md) — the signed-order model
- [`contrib/uap-js/README.md`](../contrib/uap-js/README.md) — JavaScript builder
- [`contrib/uap-indexer/README.md`](../contrib/uap-indexer/README.md) — indexer and order relay
