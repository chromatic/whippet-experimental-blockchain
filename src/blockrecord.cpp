// Copyright (c) 2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "blockrecord.h"

#include "clientversion.h"

namespace blockrecord {

bool ChooseFlags(const CBlock& block, unsigned char& flags)
{
    flags = 0;
    if (block.vtx.empty() || !block.vtx[0])
        return false;
    for (const auto& tx : block.vtx)
        if (!tx)
            return false;
    if (block.vtx.size() == 1 && block.hashMerkleRoot == block.vtx[0]->GetHash())
        flags |= FLAG_SINGLE_TX;
    if (block.IsAuxpow()) {
        if (!block.auxpow || !block.auxpow->tx)
            return false;
        const CAuxPow& aux = *block.auxpow;
        // Not derivable (consensus rejects such a block, but the encoder does not rely on that).
        if (aux.nIndex != 0)
            return false;
        if (aux.parentBlock.hashMerkleRoot != CAuxPow::CheckMerkleBranch(aux.tx->GetHash(), aux.vMerkleBranch, 0))
            return false;
        if (aux.hashBlock == aux.parentBlock.GetHash())
            flags |= FLAG_AUX_HASHBLOCK_DERIVED;
        if (aux.nChainIndex == 0)
            flags |= FLAG_AUX_CHAININDEX_ZERO;
    }
    return true;
}

bool EncodeSlim(const CBlock& block, std::vector<unsigned char>& vchPayload)
{
    vchPayload.clear();
    unsigned char flags;
    if (!ChooseFlags(block, flags))
        return false;

    CDataStream ss(SER_DISK, CLIENT_VERSION);
    try {
        SerializeSlim(ss, block, flags);

        // Verify-on-write: decode what we just wrote and compare the full serialization.
        CDataStream check(ss.begin(), ss.end(), SER_DISK, CLIENT_VERSION);
        CBlock decoded;
        UnserializeSlim(check, decoded);
        if (!check.empty())
            return false;
        CDataStream a(SER_DISK, CLIENT_VERSION), b(SER_DISK, CLIENT_VERSION);
        a << block;
        b << decoded;
        if (a.size() != b.size() || memcmp(a.data(), b.data(), a.size()) != 0)
            return false;
        // Nothing omitted (multi-tx, no AuxPoW): the flags byte would make it larger.
        if (ss.size() >= a.size())
            return false;
    } catch (const std::exception&) {
        return false;
    }
    vchPayload.assign(ss.begin(), ss.end());
    return true;
}

} // namespace blockrecord
