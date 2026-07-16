# UAP marketplace / atomic swap design

This documents how token-for-WHIP trading works for UAP positions
(`OP_MINT` / `OP_MINT_TRANSFER`), what's already implemented and verified,
and what's still needed to build a real maker/taker marketplace on top of
it. If you want to actually run one, see
`doc/uap-marketplace-operators-guide.md` instead — this document is about
why it works, that one is about standing it up.

## No new opcode needed

An atomic swap is just an ordinary multi-input, multi-output transaction:

```
inputs:  [ seller's OP_MINT_TRANSFER position, buyer's WHIP UTXO(s) ]
outputs: [ token covenant -> buyer, payment -> seller, change -> buyer ]
```

Each party signs only their own input. Atomicity is free: an
under-signed transaction can't broadcast, and once broadcast it either
fully confirms or not at all. There's no escrow, no HTLC, no trusted
third party.

This required one consensus change (`5c70b6938`): `OP_MINT_TRANSFER`
previously required *every* output of the spending transaction to be a
same-multiplier covenant, which made the payment/change legs above
impossible. It's now relaxed to: any non-UAP-shaped output is left alone,
while the position must still produce at least one continuing covenant
output whose value (summed with any other same-multiplier covenant
outputs) doesn't exceed the input's value. See
`CheckUapOutputConservation` in `src/script/interpreter.cpp` for the exact
rule and its reasoning.

## Scarce identity is a free side effect, not a separate feature

A natural question: since `multiplier` is the only thing distinguishing
"types" of tokens and isn't itself scarce (anyone can `OP_MINT` more of
any multiplier), what stops two unrelated mints from being merged/diluted
together?

`CheckUapOutputConservation` runs once per UAP input in a transaction, and
each run independently requires the transaction's *total* same-multiplier
covenant output value to fit within *that one input's* own value. For two
inputs from different lineages, both of those constraints must hold
simultaneously against a shared output total -- which is only possible if
one side contributes ~0. In practice this means a transaction can contain
at most one UAP input if its covenant outputs are to be non-trivial:
distinct mint lineages can never be merged, and every `OP_MINT_TRANSFER`
output's value traces back through exactly one unbroken chain of spends to
a single originating `OP_MINT`. This is verified live and covered by
`transfer_rejects_merging_independent_lineages` in
`src/test/uap_mint_tests.cpp` (and `transfer_solo_spend_of_either_lineage_still_works`
confirming this isn't just multi-input transactions breaking generally).

Practical implication for a marketplace/indexer: authenticity of a
position is established by walking its spend history back to a genuine
`OP_MINT` -- which `uap-indexer` already does implicitly, since it only
records positions it observed as real chain outputs and tracks spend
status through that same lineage. There is no way to fabricate a
same-multiplier position's value without it actually tracing back to a
real mint that funded it.

## Signed-order model: maker/taker, no live coordination

Because the consensus-level `CheckSig` call for `OP_MINT`/`OP_MINT_TRANSFER`
is generic (it doesn't restrict which SIGHASH type is used), a seller can
sign their side of a trade with `SIGHASH_SINGLE | SIGHASH_ANYONECANPAY`:
this commits only to their own input and the single output at the same
index, and explicitly does *not* commit to anything else in the final
transaction. That's exactly what a standing, fillable order needs -- with
one important subtlety about *which* output gets pinned.

The maker doesn't know who the taker will be yet, so they can't pin "the
covenant output, addressed to the eventual buyer" -- that destination
doesn't exist at signing time. But they don't need to: `CheckUapOutputConservation`
doesn't care which output continues the covenant or who it's addressed to,
only that *some* valid same-multiplier covenant exists somewhere in the
transaction with value within bounds. That check runs unconditionally,
regardless of what the signature's SIGHASH flags commit to. So the thing
the maker's signature actually needs to pin down is the thing they can't
otherwise enforce: getting paid.

1. **Maker (seller) creates an order**: build a transaction with their
   token input at index *i*, and at that *same* index *i* in `vout`,
   their own payment-receiving output for the asking price (not the
   token's destination). Sign that input with
   `SIGHASH_SINGLE|ANYONECANPAY`, which pins "my input is being spent" and
   "output *i* must be exactly this payment to me," and commits to nothing
   else. Publish the signed fragment + asking price to an order relay. No
   live buyer needed yet.
2. **Taker (buyer) fills it**: fetch an open order, build the full
   transaction preserving the maker's input and payment output at their
   original index, then add their own payment input(s), a token-covenant
   output addressed to themselves (anywhere in `vout` -- its position
   relative to the maker's signed index doesn't matter), and their own
   change. Sign their own input(s) with a normal `SIGHASH_ALL`. Broadcast.
3. The maker's signature remains valid regardless of what the taker adds,
   since `ANYONECANPAY` ignores other inputs and `SINGLE` ignores other
   outputs entirely -- as long as output *i* is still exactly the payment
   they signed. The taker cannot reduce the price or redirect it elsewhere;
   they can only choose to fill the order as specified, or not fill it at
   all. Meanwhile the token itself is unconstrained by the maker's
   signature and free to go to whatever covenant output the taker adds,
   because the consensus-level conservation check -- not the signature --
   is what guarantees a valid covenant exists at all.

**This construction is now implemented and verified live**, the same way
as everything else in this document: `uap.js` implements the full legacy
sighash transform (`signatureHash` supports `ALL`/`NONE`/`SINGLE` combined
with `ANYONECANPAY`, including the classic Satoshi-client degenerate
"hash of 1" edge cases, matching `SignatureHash()` in
`src/script/interpreter.cpp` byte for byte), plus two convenience
functions: `signMakerOrder()` (produces the maker's signed fragment) and
`fillOrder()` (assembles the taker's completing transaction, keeping the
maker's input/payment output paired at the same index as signed). Run
against a live regtest node: a maker signs an order with no taker
present, a taker later fills it with their own payment input and a
covenant destination the maker never specified, and the trade confirms
with no live coordination between the two parties. A second test
confirmed a taker cannot alter the maker's payment output (e.g. reduce
the agreed price) without invalidating the maker's signature.

This is the standard non-custodial "maker signs once, any taker can fill
later" pattern used by UTXO-chain orderbooks (e.g. Bitcoin's classic
`SIGHASH_SINGLE|ANYONECANPAY` atomic swap trick). No escrow, no relay
custody of funds -- the relay only ever holds *signed order fragments*,
which are worthless without a taker completing them exactly as signed.

### Order relay: built and verified

`uap-indexer` now hosts the relay (see its README for the full API):
`POST /orders`, `GET /orders[?multiplier=]`, `GET /orders/{txid}/{vout}`,
`DELETE /orders/{txid}/{vout}?script_sig=`. It never holds funds or keys,
does no cryptographic signature verification (deliberately staying
dependency-free -- a taker's own node broadcast is the real check), and
only does structural sanity checks (correct hashtype byte, DER-shaped
signature, referenced position real/unspent/matching multiplier) plus
automatic pruning the moment a position is observed spent.

Verified live end to end through the *actual HTTP API* (not just the Go
functions directly): a maker mints a position, signs an order, and
publishes it over real HTTP; a taker -- a separate process with no
back-channel to the maker -- browses `GET /orders`, fetches the specific
order, fills it, and broadcasts; the indexer notices the fill and the
order disappears from listings and 404s on direct lookup. A separate
cancellation test confirmed withdrawing with the wrong `script_sig` is
rejected and the right one succeeds.

### What's still not built

- **No fee/price-index UX.** A real marketplace UI would want to show
  "N tokens for P WHIP" in a comparable unit; that's presentation logic on
  top of the primitives here, not a protocol concern.
- **Relay pruning isn't reorg-aware.** If the block that spent a position
  gets reorged out, the pruned order isn't restored. Acceptable for
  ephemeral, non-consensus data -- the maker can republish -- but worth
  knowing.
- **No authentication on the relay's write endpoints, by design.** Anyone
  can `POST` a (structurally valid) order or attempt cancellations; this
  is fine for the trust model itself (garbage orders just fail to fill,
  cancellation requires reproducing the signed contents) -- there's no
  private key material a relay could authenticate against even if it
  wanted to. Basic per-IP rate limiting on the write endpoints is in place
  (`-ratelimit`/`-rateburst`/`-trustproxy` in `uap-indexer`) as a floor
  against casual abuse; a public deployment fronted by real infrastructure
  (CDN, reverse proxy) would likely still want more.

## Summary of what's verified live (regtest)

- A single-lineage transfer with a non-covenant sibling output succeeds
  (the relaxation itself).
- A transfer with *no* continuing covenant output at all is rejected (no
  implicit burn/redemption path exists today).
- Two independent mint lineages cannot be merged into one output, with or
  without the relaxation.
- A real token-for-WHIP swap transaction (token input + payment input ->
  covenant + payment + change) succeeds end to end.
- Tampering with the agreed payment amount after the seller signs
  invalidates their signature -- the atomicity/no-cheating property holds.
- A maker's `SIGHASH_SINGLE|ANYONECANPAY` sell order, signed with no taker
  present, is later filled by a taker adding their own payment input and
  a covenant destination the maker never specified -- with no live
  coordination between the two parties -- and confirms successfully.
- A taker attempting to alter the maker's payment output (e.g. pay less
  than the signed asking price) is rejected: the maker's signature no
  longer validates against the modified transaction.
- A full maker/taker cycle through `uap-indexer`'s actual HTTP order relay
  (publish, browse, fetch, fill, on-chain confirmation, auto-pruning, and
  cancellation) all work end to end between two independent processes
  with no direct communication.
