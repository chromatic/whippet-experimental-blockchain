// Copyright (c) 2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "test/test_bitcoin.h"

#include "blockrecord.h"
#include "chainparams.h"
#include "consensus/merkle.h"
#include "random.h"
#include "script/script.h"
#include "validation.h"

#include <boost/test/unit_test.hpp>

BOOST_FIXTURE_TEST_SUITE(blockrecord_tests, TestingSetup)

namespace {

CTransactionRef MakeTx(unsigned int seed, bool coinbase)
{
    CMutableTransaction tx;
    tx.vin.resize(1);
    if (coinbase) {
        tx.vin[0].prevout.SetNull();
        tx.vin[0].scriptSig = CScript() << (int)seed << std::vector<unsigned char>(20, seed);
    } else {
        tx.vin[0].prevout = COutPoint(uint256S("0102030405060708091011121314151617181920212223242526272829303132"), seed);
        tx.vin[0].scriptSig = CScript() << std::vector<unsigned char>(71, seed);
    }
    tx.vout.resize(1);
    tx.vout[0].nValue = 1000 + seed;
    tx.vout[0].scriptPubKey = CScript() << OP_DUP << OP_HASH160 << std::vector<unsigned char>(20, seed) << OP_EQUALVERIFY << OP_CHECKSIG;
    return MakeTransactionRef(tx);
}

CBlock MakeBlock(unsigned int nTx)
{
    CBlock b;
    b.nVersion = 2;
    b.hashPrevBlock = uint256S("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899");
    b.nTime = 1700000000;
    b.nBits = 0x1e0ffff0;
    b.nNonce = 12345;
    for (unsigned int i = 0; i < nTx; i++)
        b.vtx.push_back(MakeTx(i + 1, i == 0));
    b.hashMerkleRoot = BlockMerkleRoot(b);
    return b;
}

/** Add an AuxPoW with a parent block that has parentTxs transactions; the branch is the real one. */
void AddAuxPow(CBlock& b, unsigned int parentTxs, bool fDeriveHashBlock, int nChainIndex = 0)
{
    CBlock parent;
    parent.nVersion = 0x20000000;
    parent.nTime = 1700000001;
    parent.nBits = 0x1b000000;
    parent.nNonce = 99;
    for (unsigned int i = 0; i < parentTxs; i++)
        parent.vtx.push_back(MakeTx(50 + i, i == 0));
    parent.hashPrevBlock = uint256S("1111111111111111111111111111111111111111111111111111111111111111");
    parent.hashMerkleRoot = BlockMerkleRoot(parent);

    CAuxPow* aux = new CAuxPow(parent.vtx[0]);
    aux->vMerkleBranch = BlockMerkleBranch(parent, 0);
    aux->nIndex = 0;
    aux->vChainMerkleBranch.push_back(uint256S("2222222222222222222222222222222222222222222222222222222222222222"));
    aux->nChainIndex = nChainIndex;
    aux->parentBlock = parent;
    aux->hashBlock = fDeriveHashBlock ? parent.GetHash() : uint256S("33");
    b.SetAuxpow(aux);
}

std::vector<unsigned char> Ser(const CBlock& b)
{
    CDataStream ss(SER_DISK, CLIENT_VERSION);
    ss << b;
    return std::vector<unsigned char>(ss.begin(), ss.end());
}

/** Encode (expecting a slim record), decode, compare bytes; returns the payload. */
std::vector<unsigned char> RoundTrip(const CBlock& b, bool fExpectSlim)
{
    std::vector<unsigned char> payload;
    BOOST_CHECK_EQUAL(blockrecord::EncodeSlim(b, payload), fExpectSlim);
    if (!fExpectSlim) {
        BOOST_CHECK(payload.empty());
        return payload;
    }
    BOOST_CHECK(payload.size() < Ser(b).size());
    CDataStream ss(payload, SER_DISK, CLIENT_VERSION);
    CBlock out;
    blockrecord::UnserializeSlim(ss, out);
    BOOST_CHECK(ss.empty());
    BOOST_CHECK(Ser(out) == Ser(b));
    BOOST_CHECK(out.GetHash() == b.GetHash());

    // header-only decode gives the same header, including the merkle root of a single-tx block
    CDataStream sh(payload, SER_DISK, CLIENT_VERSION);
    CBlockHeader h;
    blockrecord::UnserializeSlim(sh, h);
    BOOST_CHECK(h.GetHash() == b.GetHash());
    BOOST_CHECK(h.hashMerkleRoot == b.hashMerkleRoot);
    return payload;
}

} // namespace

BOOST_AUTO_TEST_CASE(roundtrip_legacy_stays_legacy)
{
    // Multi-tx, no AuxPoW: nothing to omit, the encoder declines.
    RoundTrip(MakeBlock(3), false);
    // Single tx whose header merkle root is not the coinbase txid: not derivable, so no SINGLE_TX; and
    // then nothing is omitted.
    CBlock b = MakeBlock(1);
    b.hashMerkleRoot = uint256S("44");
    RoundTrip(b, false);
}

BOOST_AUTO_TEST_CASE(roundtrip_single_tx)
{
    CBlock b = MakeBlock(1);
    std::vector<unsigned char> p = RoundTrip(b, true);
    // flags + header without merkle root and count: exactly 32 + 1 (count) - 1 (flags) bytes smaller.
    BOOST_CHECK_EQUAL(Ser(b).size() - p.size(), 32U + 1U - 1U);
}

BOOST_AUTO_TEST_CASE(roundtrip_auxpow)
{
    // Multi-tx block with AuxPoW; single parent tx (empty branch) and a deep branch.
    for (unsigned int parentTxs : {1U, 2U, 7U, 13U}) {
        for (bool derive : {false, true}) {
            for (int idx : {0, 5}) {
                CBlock b = MakeBlock(3);
                AddAuxPow(b, parentTxs, derive, idx);
                RoundTrip(b, true);
                CBlock one = MakeBlock(1);
                AddAuxPow(one, parentTxs, derive, idx);
                RoundTrip(one, true);
            }
        }
    }
    // The derived hashBlock case saves 32 bytes more than the non-derived one.
    CBlock a = MakeBlock(1), c = MakeBlock(1);
    AddAuxPow(a, 3, true);
    AddAuxPow(c, 3, false);
    BOOST_CHECK_EQUAL(RoundTrip(c, true).size() - RoundTrip(a, true).size(), 32U);
}

BOOST_AUTO_TEST_CASE(auxpow_not_derivable_falls_back)
{
    CBlock b = MakeBlock(1);
    AddAuxPow(b, 4, true);
    b.auxpow->nIndex = 1; // consensus rejects it, but the encoder must not lose it
    RoundTrip(b, false);

    CBlock m = MakeBlock(1);
    AddAuxPow(m, 4, true);
    m.auxpow->parentBlock.hashMerkleRoot = uint256S("55"); // does not match the branch
    RoundTrip(m, false);
}

BOOST_AUTO_TEST_CASE(reserved_flags_and_truncation)
{
    CBlock b = MakeBlock(1);
    AddAuxPow(b, 5, true);
    std::vector<unsigned char> p = RoundTrip(b, true);

    for (int bit = 3; bit < 8; bit++) {
        std::vector<unsigned char> q = p;
        q[0] |= (1 << bit);
        CDataStream ss(q, SER_DISK, CLIENT_VERSION);
        CBlock out;
        BOOST_CHECK_THROW(blockrecord::UnserializeSlim(ss, out), std::ios_base::failure);
    }
    // every truncation fails cleanly
    for (size_t n = 0; n < p.size(); n++) {
        CDataStream ss(std::vector<unsigned char>(p.begin(), p.begin() + n), SER_DISK, CLIENT_VERSION);
        CBlock out;
        BOOST_CHECK_THROW(blockrecord::UnserializeSlim(ss, out), std::exception);
    }
    // random corruption never crashes or hangs: it either decodes to something or throws
    for (int i = 0; i < 300; i++) {
        std::vector<unsigned char> q = p;
        q[InsecureRandRange(q.size())] ^= (unsigned char)(1 + InsecureRandRange(255));
        CDataStream ss(q, SER_DISK, CLIENT_VERSION);
        CBlock out;
        try {
            blockrecord::UnserializeSlim(ss, out);
        } catch (const std::exception&) {
        }
    }
}

BOOST_AUTO_TEST_CASE(random_blocks)
{
    for (int i = 0; i < 100; i++) {
        CBlock b = MakeBlock(1 + InsecureRandRange(4));
        if (InsecureRandBool())
            AddAuxPow(b, 1 + InsecureRandRange(9), InsecureRandBool(), InsecureRandRange(3));
        b.nNonce = InsecureRand32();
        b.nTime = InsecureRand32();
        std::vector<unsigned char> payload;
        bool fSlim = blockrecord::EncodeSlim(b, payload);
        BOOST_CHECK_EQUAL(fSlim, b.vtx.size() == 1 || b.IsAuxpow());
        if (fSlim) {
            CDataStream ss(payload, SER_DISK, CLIENT_VERSION);
            CBlock out;
            blockrecord::UnserializeSlim(ss, out);
            BOOST_CHECK(Ser(out) == Ser(b));
        }
    }
}

/** Write and read through the real file functions, both encodings in one file. */
BOOST_AUTO_TEST_CASE(disk_roundtrip_mixed_file)
{
    const CChainParams& params = Params();
    std::vector<CBlock> blocks;
    blocks.push_back(MakeBlock(1));
    blocks.push_back(MakeBlock(4));
    blocks.push_back(MakeBlock(1));
    AddAuxPow(blocks.back(), 6, true);
    blocks.push_back(MakeBlock(5));
    AddAuxPow(blocks.back(), 3, false);

    std::vector<CDiskBlockPos> pos;
    std::vector<bool> slim;
    for (size_t i = 0; i < blocks.size(); i++) {
        fSlimBlocks = (i % 3 != 1); // blocks 0, 2 and 3 slim, block 1 legacy
        CDiskBlockPos p(14, 0);
        // append at the end of the file like FindBlockPos would
        FILE* f = OpenBlockFile(CDiskBlockPos(14, 0));
        BOOST_REQUIRE(f);
        fseek(f, 0, SEEK_END);
        p.nPos = ftell(f);
        fclose(f);
        BOOST_REQUIRE(WriteBlockToDisk(blocks[i], p, params.MessageStart()));
        pos.push_back(p);
        uint32_t word = 0;
        BOOST_REQUIRE(ReadBlockRecordSizeWord(p, word));
        slim.push_back((word & blockrecord::SLIM_FLAG) != 0);
    }
    fSlimBlocks = false;
    BOOST_CHECK(slim[0] && !slim[1] && slim[2] && slim[3]);

    // Reading needs no option; every record decodes to the same bytes.
    for (size_t i = 0; i < blocks.size(); i++) {
        CBlock out;
        BOOST_REQUIRE(ReadBlockFromDisk(out, pos[i], params.GetConsensus(0), false));
        BOOST_CHECK(Ser(out) == Ser(blocks[i]));
    }
}

BOOST_AUTO_TEST_SUITE_END()
