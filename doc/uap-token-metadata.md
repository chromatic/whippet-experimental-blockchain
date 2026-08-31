# WUAP token metadata

Normative specification for the `OP_RETURN` record that names a UAP token.

This format has a version byte and two independent implementations in different
languages — the wallet's builder (`contrib/uap-js/uap.js`) and the indexer's parser
(`contrib/uap-indexer/metadata.go`) — which must agree byte for byte. This file is
the reference they are both written against; if they disagree with each other, this
document decides which one is wrong.

## Wire format (version 2)

```
scriptPubKey = OP_RETURN                      0x6a
               PUSH(4)  "WUAP"                57 55 41 50
               PUSH(1)  version               0x02
               PUSH(n)  ticker                n in 1..8
               PUSH(m)  metadata_hash         m == 0 or m == 32

size = 10 + n + m        minimum 11 bytes, maximum 50
```

**Exactly four pushes.** A fifth is a parse failure, not surplus to be ignored:
trailing data would let two different scripts parse to the same metadata, and give a
minter somewhere to hide bytes that consumers never see.

The record appears **on the mint transaction only**. A token's identity is fixed at
creation; a `WUAP` record on any later transaction in the lineage is ignored, so a
subsequent owner cannot rename a token out from under its holders.

The token's identity is derived from the mint output's outpoint:
`origin = SHA256(txid_internal_bytes || vout as 4-byte LE)`. Note that this is the
32-byte hash, not the `txid:vout` string — the indexer serves it hex-encoded as
`origin`, and every transfer covenant in the lineage carries those same 32 bytes in
its scriptPubKey. The metadata record does not carry an identifier of its own: the
outpoint is already globally unique and unforgeable, whereas a self-declared ID is
unbound, so two mints could claim the same one and a tiebreak rule would become
necessary.

### `ticker`

One to eight bytes, each of which must be ASCII `A-Z` (`0x41-0x5A`) or `0-9`
(`0x30-0x39`). Nothing else: no lowercase, no UTF-8, no punctuation, no whitespace,
no control characters.

The restriction is a security property, not a formatting preference. With no `name`
field the ticker is the only human-readable identifier a user sees, so arbitrary
UTF-8 would let `WHIP` be minted with a Cyrillic `Р` (`U+0420`) that renders
identically in a marketplace listing. An ASCII-only alphabet makes homoglyphs
impossible rather than merely discouraged, and has two useful consequences: eight
bytes is exactly eight characters, and UTF-8 validity and control-character checks
are unnecessary rather than merely tightened.

**Case is folded in the wallet, never on read.** A builder uppercases what the user
typed before constructing the script; a parser rejects a lowercase ticker outright.
This keeps one canonical byte string per displayed ticker — if parsers folded on
read, `whip` and `WHIP` would be two distinct on-chain records that display
identically, reintroducing the collision the charset rule exists to remove.

### `metadata_hash`

Either absent (a zero-length push) or exactly 32 bytes. It is a commitment to a
richer off-chain metadata blob — description, links, full-resolution artwork.

A hash rather than a URL or an ID is the point. An ID is a *trusted* pointer:
whoever serves it can swap the content underneath. A hash is a *verifiable* one —
the client fetches the blob and checks it itself, so neither the indexer nor the
blob host has to be trusted. No URL is baked into the chain; the fetch location is
derived by convention (gateway + hex hash).

> **Reserved, not implemented.** Nothing currently produces or consumes this field,
> and no off-chain store or resolution convention exists yet. A hash proves *which*
> metadata is authentic but not *where* to fetch it, so a resolver is a separate
> design question. Until one lands, a token's only label is its ticker. The slot is
> specified now because the record is written once at mint and never again: a token
> minted without it could never acquire metadata later.

### `version`

`0x02`. Version `0x01` was the five-push form `"WUAP" 0x01 <ticker> <name> <hash>`
and is **explicitly rejected**, not parsed — see [History](#history).

A parser must check the version before the push count, so that a future version's
different shape produces "unknown version" rather than a misleading structural
error.

## What the node enforces

Nothing about this format. Consensus and policy see an opaque data output.

`Solver()` (`src/script/standard.cpp:149-156`) classifies any push-only
`OP_RETURN` script as `TX_NULL_DATA` without populating `vSolutionsRet` — the
payload is never decomposed into pushes, so the magic, version, push count and
charset are invisible to the node. There is no soft fork here and no activation
height; `WUAP` is a convention between the wallet and the indexer that happens to
be durably stored.

The node imposes exactly two constraints on the output, both satisfied with room to
spare:

| Constraint | Where | Effect here |
|---|---|---|
| `scriptPubKey.size() <= 83` | `MAX_OP_RETURN_RELAY`, `src/script/standard.h:30`; checked in `src/policy/policy.cpp:58-60` | 50-byte maximum leaves 33 bytes of headroom |
| at most one `OP_RETURN` output per transaction | `src/policy/policy.cpp:123-127`, `"multi-op-return"` | a mint carries this record and no other data output |

The 83-byte limit is measured against the **entire serialized script**, including the
leading `OP_RETURN` byte and every push-length prefix — not against the payload
alone. It is overridable by a node operator via `-datacarriersize`
(`src/init.cpp`), so a record above 83 bytes is not invalid, merely unrelayable by
default; a miner could include one directly. Builders must therefore treat 83 as a
hard ceiling, and parsers must not assume it.

## It does not bloat the UTXO set

An `OP_RETURN`-leading script is *provably* unspendable, and the node discards those
outputs rather than storing them: `IsUnspendable()` (`src/script/script.h:640-643`)
is true for any script beginning with `OP_RETURN`, and `ClearUnspendable()`
(`src/coins.h:121-128`) nulls such outputs before the coins record is written —
pruned, in the code's own words, "instantly when entering the UTXO set."

The cost of naming a token is therefore one-time block space and fee, not permanent
node state. This is precisely why the metadata is not stuffed into the mint
covenant's own scriptPubKey, which *is* resident: a mint position
(`<pubkey> <multiplier> OP_MINT`, ~37 bytes) sits in the UTXO set until it is
spent, and a transfer position (`<pubkey> <multiplier> <origin32>
OP_MINT_TRANSFER`, ~70 bytes) sits there for as long as it is held.

The output must carry **zero value**. Paying an unspendable script burns the coins
outright.

## History

**Version 1** — `"WUAP" 0x01 <ticker> <name> <metadata_hash>`, five pushes, with
declared bounds of ticker ≤ 16 bytes, name ≤ 64, hash 32.

Those bounds were never simultaneously satisfiable: 16 + 64 + 32 = 112 against a
72-byte payload budget, and even 16 + 64 with no hash overflowed. Version 2 resolves
it by removing `name` rather than by shrinking it.

Dropping `name` loses less than it appears to. Minter-supplied text carries no
authority wherever it is stored — anyone can mint a token named `Dogecoin` — so
putting it in a block never made it more trustworthy, only more expensive and
more permanent. Identity that needs to be *verified* belongs behind
`metadata_hash`; identity that needs to be *unique* is the outpoint.

Version 1 was never deployed to a live chain. It is rejected rather than
supported so that neither implementation carries a second parse path, and so
`TokenMetadata` need not retain a vestigial `Name` field.
