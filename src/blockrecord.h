// Copyright (c) 2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#ifndef BITCOIN_BLOCKRECORD_H
#define BITCOIN_BLOCKRECORD_H

#include "auxpow.h"
#include "primitives/block.h"
#include "serialize.h"
#include "streams.h"

#include <stdint.h>
#include <ios>
#include <vector>

/**
 * Slim block records for blk*.dat (-slimblocks).
 *
 * A legacy record is magic(4) | size(4) | CBlock. In a slim record bit 31 of the size word is set and
 * the low 31 bits are the payload size. The size word sits at nPos - 4 for every block position, so
 * a reader learns the encoding before it reads the payload. The payload leaves out fields that can be
 * recomputed from other fields of the same record:
 *
 *   byte  flags
 *   i32   nVersion
 *   u256  hashPrevBlock                   (kept: -reindex reads it straight from the record)
 *   u256  hashMerkleRoot                  (omitted when SINGLE_TX: it is the coinbase txid)
 *   u32   nTime, nBits, nNonce
 *   [AuxPoW, when nVersion has VERSION_AUXPOW]
 *         parent coinbase tx
 *         u256 hashBlock                  (omitted when AUX_HASHBLOCK_DERIVED: sha256d of the parent header)
 *         vMerkleBranch
 *                                         (nIndex omitted: must be 0, as CAuxPow::check requires)
 *         vChainMerkleBranch
 *         i32 nChainIndex                 (omitted when AUX_CHAININDEX_ZERO)
 *         parent header without hashMerkleRoot (recomputed from the coinbase txid and vMerkleBranch)
 *   compact size tx count                 (omitted when SINGLE_TX)
 *   transactions
 *
 * The encoder only emits a slim record after decoding it again and comparing the network
 * serialization byte for byte with the input block; otherwise the caller writes a legacy record.
 * Offsets stored by the txindex (CDiskTxPos::nTxOffset) count from the end of the header part of
 * the record and include the tx count, as for a legacy record, so they are valid for both
 * encodings; a SINGLE_TX record has no count byte, so the reader skips one byte less.
 */
namespace blockrecord {

static const uint32_t SLIM_FLAG = 0x80000000;
static const uint32_t SIZE_MASK = 0x7fffffff;

static const unsigned char FLAG_SINGLE_TX = 1 << 0;
static const unsigned char FLAG_AUX_HASHBLOCK_DERIVED = 1 << 1;
static const unsigned char FLAG_AUX_CHAININDEX_ZERO = 1 << 2;
static const unsigned char FLAGS_KNOWN = FLAG_SINGLE_TX | FLAG_AUX_HASHBLOCK_DERIVED | FLAG_AUX_CHAININDEX_ZERO;

/** Smallest payload a slim record can have (flags, header fields without merkle root, nothing else). */
static const unsigned int MIN_SLIM_PAYLOAD = 1 + 4 + 32 + 12;

/** Flags a block would be encoded with, or false if it cannot be (no verification here). */
bool ChooseFlags(const CBlock& block, unsigned char& flags);

template <typename Stream>
void SerializeSlim(Stream& s, const CBlock& block, unsigned char flags)
{
    ser_writedata8(s, flags);
    s << block.nVersion << block.hashPrevBlock;
    if (!(flags & FLAG_SINGLE_TX))
        s << block.hashMerkleRoot;
    s << block.nTime << block.nBits << block.nNonce;
    if (block.IsAuxpow()) {
        const CAuxPow& aux = *block.auxpow;
        s << aux.tx;
        if (!(flags & FLAG_AUX_HASHBLOCK_DERIVED))
            s << aux.hashBlock;
        s << aux.vMerkleBranch << aux.vChainMerkleBranch;
        if (!(flags & FLAG_AUX_CHAININDEX_ZERO))
            s << aux.nChainIndex;
        const CPureBlockHeader& p = aux.parentBlock;
        s << p.nVersion << p.hashPrevBlock << p.nTime << p.nBits << p.nNonce;
    }
    if (!(flags & FLAG_SINGLE_TX))
        s << block.vtx;
    else
        s << *block.vtx[0];
}

/**
 * Read the header part of a slim record (up to the transactions). hashMerkleRoot is left null when
 * the record is SINGLE_TX: the caller sets it once it has the coinbase.
 */
template <typename Stream>
void UnserializeSlimHeader(Stream& s, CBlockHeader& header, unsigned char& flags)
{
    header.SetNull();
    flags = ser_readdata8(s);
    if (flags & ~FLAGS_KNOWN)
        throw std::ios_base::failure("slim block record: reserved flag bits set");
    s >> header.nVersion >> header.hashPrevBlock;
    if (!(flags & FLAG_SINGLE_TX))
        s >> header.hashMerkleRoot;
    s >> header.nTime >> header.nBits >> header.nNonce;
    if (header.IsAuxpow()) {
        std::shared_ptr<CAuxPow> aux(new CAuxPow());
        s >> aux->tx;
        if (!aux->tx)
            throw std::ios_base::failure("slim block record: null parent coinbase");
        if (!(flags & FLAG_AUX_HASHBLOCK_DERIVED))
            s >> aux->hashBlock;
        s >> aux->vMerkleBranch >> aux->vChainMerkleBranch;
        aux->nIndex = 0;
        aux->nChainIndex = 0;
        if (!(flags & FLAG_AUX_CHAININDEX_ZERO))
            s >> aux->nChainIndex;
        CPureBlockHeader& p = aux->parentBlock;
        s >> p.nVersion >> p.hashPrevBlock >> p.nTime >> p.nBits >> p.nNonce;
        p.hashMerkleRoot = CAuxPow::CheckMerkleBranch(aux->tx->GetHash(), aux->vMerkleBranch, 0);
        if (flags & FLAG_AUX_HASHBLOCK_DERIVED)
            aux->hashBlock = p.GetHash();
        header.auxpow = aux;
    }
}

/** Read a whole slim block record payload. */
template <typename Stream>
void UnserializeSlim(Stream& s, CBlock& block)
{
    block.SetNull();
    unsigned char flags;
    UnserializeSlimHeader(s, block, flags);
    if (flags & FLAG_SINGLE_TX) {
        CTransactionRef tx;
        s >> tx;
        if (!tx)
            throw std::ios_base::failure("slim block record: null transaction");
        block.vtx.assign(1, tx);
        block.hashMerkleRoot = tx->GetHash();
    } else {
        s >> block.vtx;
    }
}

/** Read only the header of a slim record; SINGLE_TX needs the coinbase to recover the merkle root. */
template <typename Stream>
void UnserializeSlim(Stream& s, CBlockHeader& header)
{
    unsigned char flags;
    UnserializeSlimHeader(s, header, flags);
    if (flags & FLAG_SINGLE_TX) {
        CTransactionRef tx;
        s >> tx;
        if (!tx)
            throw std::ios_base::failure("slim block record: null transaction");
        header.hashMerkleRoot = tx->GetHash();
    }
}

/**
 * Encode a block as a slim payload (without magic and size word) into vchPayload. Returns false, and
 * leaves vchPayload empty, if the block must be stored as a legacy record: it has fields that are not
 * derivable, or the decoded payload does not serialize to the same bytes as the block.
 */
bool EncodeSlim(const CBlock& block, std::vector<unsigned char>& vchPayload);

} // namespace blockrecord

#endif // BITCOIN_BLOCKRECORD_H
