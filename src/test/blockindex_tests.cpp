// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "chain.h"
#include "hash.h"
#include "streams.h"
#include "txdb.h"
#include "test/test_bitcoin.h"

#include <map>
#include <memory>
#include <vector>

#include <boost/test/unit_test.hpp>

namespace {

/** A chain of `n` entries whose hashes are index-dependent, owned by `hashes` and `index`. */
struct TestChain {
    std::vector<uint256> hashes;
    std::vector<CBlockIndex> index;

    explicit TestChain(int n) : hashes(n), index(n)
    {
        for (int i = 0; i < n; i++) {
            hashes[i] = Hash(&i, &i + 1);
            index[i].nHeight = i;
            index[i].nStatus = BLOCK_VALID_TREE | BLOCK_HAVE_DATA;
            index[i].nFile = 0;
            index[i].nDataPos = 100 * i;
            index[i].nVersion = 0x00575004;
            index[i].hashMerkleRoot = Hash(&hashes[i], &hashes[i] + 1);
            index[i].nTime = 1767254400 + 6 * i;
            index[i].nBits = 0x1f0fffff;
            index[i].nNonce = i;
            index[i].nTx = 1;
            index[i].phashBlock = &hashes[i];
            index[i].pprev = i ? &index[i - 1] : NULL;
        }
    }
};

/** The hash of the header an entry describes: the key it is stored under. */
uint256 HeaderHash(const CBlockIndex& i)
{
    CBlockHeader h;
    h.nVersion = i.nVersion;
    h.hashPrevBlock = i.pprev ? i.pprev->GetBlockHash() : uint256();
    h.hashMerkleRoot = i.hashMerkleRoot;
    h.nTime = i.nTime;
    h.nBits = i.nBits;
    h.nNonce = i.nNonce;
    return h.GetHash();
}

/** The index entries are keyed by block hash, so the test chain's hashes must be what the entries hash to. */
void SetHashesFromHeaders(TestChain& c)
{
    for (size_t i = 0; i < c.index.size(); i++) {
        c.hashes[i] = HeaderHash(c.index[i]);
    }
}

std::vector<CBlockIndex*> Load(CBlockTreeDB& db, std::map<uint256, CBlockIndex*>& map)
{
    BOOST_CHECK(db.LoadBlockIndexGuts([&map](const uint256& hash) -> CBlockIndex* {
        if (hash.IsNull()) {
            return NULL; // as InsertBlockIndex() in validation.cpp
        }
        CBlockIndex*& p = map[hash];
        if (!p) {
            p = new CBlockIndex();
        }
        p->phashBlock = &map.find(hash)->first;
        return p;
    }));
    std::vector<CBlockIndex*> out;
    for (auto& kv : map) out.push_back(kv.second);
    return out;
}

void Free(std::map<uint256, CBlockIndex*>& map)
{
    for (auto& kv : map) delete kv.second;
    map.clear();
}

} // namespace

BOOST_FIXTURE_TEST_SUITE(blockindex_tests, BasicTestingSetup)

BOOST_AUTO_TEST_CASE(legacy_entry_is_unchanged_and_slim_entry_is_smaller)
{
    TestChain c(3);
    SetHashesFromHeaders(c);
    CDiskBlockIndex legacy(&c.index[2], false);
    CDiskBlockIndex slim(&c.index[2], true);
    BOOST_CHECK(!legacy.fSlim);
    BOOST_CHECK(slim.fSlim);
    // The legacy entry still carries the cached difficulty the 1.3.1 writer stored.
    BOOST_CHECK(legacy.dDifficulty > 0.0);

    CDataStream sl(SER_DISK, CLIENT_VERSION), ss(SER_DISK, CLIENT_VERSION);
    sl << legacy;
    ss << slim;
    // 1.3.1 stored a 32 byte parent hash and an 8 byte double; slim keeps 4 bytes of the hash and drops the
    // double. Its format tag takes a few more bytes than the client version it replaces.
    const size_t tagExtra = GetSizeOfVarInt(CDiskBlockIndex::SLIM_FORMAT_TAG) -
                            GetSizeOfVarInt(CLIENT_VERSION);
    BOOST_CHECK_EQUAL(sl.size() - ss.size(), (32 - CDiskBlockIndex::SLIM_PREV_BYTES) + sizeof(double) - tagExtra);

    CDiskBlockIndex readLegacy, readSlim;
    sl >> readLegacy;
    ss >> readSlim;
    BOOST_CHECK(!readLegacy.fSlim);
    BOOST_CHECK(readSlim.fSlim);
    BOOST_CHECK(readLegacy.hashPrev == c.hashes[1]);
    BOOST_CHECK(readLegacy.GetBlockHash() == c.hashes[2]);
    BOOST_CHECK_EQUAL(readLegacy.dDifficulty, legacy.dDifficulty);
    BOOST_CHECK_EQUAL(readSlim.nHeight, 2);
    BOOST_CHECK_EQUAL(readSlim.nBits, c.index[2].nBits);
    BOOST_CHECK(readSlim.hashMerkleRoot == c.index[2].hashMerkleRoot);
    BOOST_CHECK(memcmp(readSlim.hashPrev.begin(), c.hashes[1].begin(), CDiskBlockIndex::SLIM_PREV_BYTES) == 0);
}

BOOST_AUTO_TEST_CASE(block_tree_db_round_trips_in_either_format_and_mixed)
{
    const int N = 40;
    for (int mode = 0; mode < 3; mode++) {
        // 0: legacy, 1: slim, 2: first half legacy then second half slim (a node that turned the flag on late)
        TestChain c(N);
        SetHashesFromHeaders(c);
        CBlockTreeDB db(1 << 20, true);
        std::vector<std::pair<int, const CBlockFileInfo*> > files;
        for (int i = 0; i < N; i++) {
            std::vector<const CBlockIndex*> one(1, &c.index[i]);
            db.SetSlimBlockIndex(mode == 1 || (mode == 2 && i >= N / 2));
            BOOST_CHECK(db.WriteBatchSync(files, 0, one));
        }

        std::map<uint256, CBlockIndex*> map;
        Load(db, map);
        BOOST_CHECK_EQUAL(map.size(), (size_t)N);
        BOOST_CHECK_EQUAL(db.nLegacyEntries, (size_t)(mode == 0 ? N : mode == 1 ? 0 : N / 2));
        for (int i = 0; i < N; i++) {
            auto it = map.find(c.hashes[i]);
            BOOST_REQUIRE(it != map.end());
            const CBlockIndex* p = it->second;
            BOOST_CHECK_EQUAL(p->nHeight, i);
            BOOST_CHECK_EQUAL(p->nBits, c.index[i].nBits);
            BOOST_CHECK_EQUAL(p->nDataPos, c.index[i].nDataPos);
            BOOST_CHECK(p->hashMerkleRoot == c.index[i].hashMerkleRoot);
            if (i == 0) {
                BOOST_CHECK(p->pprev == NULL);
            } else {
                BOOST_REQUIRE(p->pprev != NULL);
                BOOST_CHECK(p->pprev->GetBlockHash() == c.hashes[i - 1]);
            }
            // Slim or not, the cached difficulty is filled in.
            BOOST_CHECK_EQUAL(p->dDifficulty, GetDifficultyFromBits(p->nBits));
        }
        Free(map);
    }
}

BOOST_AUTO_TEST_CASE(slim_entries_pick_the_right_parent_among_siblings)
{
    // Two blocks at height 2 (a fork) with different parents at height 1: the prefix must tell them apart.
    TestChain a(3);
    TestChain b(3);
    SetHashesFromHeaders(a);
    b.index[0] = a.index[0];
    b.hashes[0] = a.hashes[0];
    b.index[0].phashBlock = &b.hashes[0];
    b.index[1].hashMerkleRoot = uint256S("0x01"); // a sibling of a.index[1]
    b.index[1].pprev = &b.index[0];
    b.index[2].pprev = &b.index[1];
    for (int i = 1; i < 3; i++) {
        b.hashes[i] = HeaderHash(b.index[i]);
    }

    CBlockTreeDB db(1 << 20, true);
    db.SetSlimBlockIndex(true);
    std::vector<std::pair<int, const CBlockFileInfo*> > files;
    std::vector<const CBlockIndex*> all = {&a.index[0], &a.index[1], &a.index[2], &b.index[1], &b.index[2]};
    BOOST_CHECK(db.WriteBatchSync(files, 0, all));

    std::map<uint256, CBlockIndex*> map;
    Load(db, map);
    BOOST_CHECK_EQUAL(map.size(), (size_t)5);
    BOOST_CHECK(map[a.hashes[2]]->pprev == map[a.hashes[1]]);
    BOOST_CHECK(map[b.hashes[2]]->pprev == map[b.hashes[1]]);
    BOOST_CHECK(map[b.hashes[1]]->pprev == map[a.hashes[0]]);
    BOOST_CHECK(map[a.hashes[2]]->pprev != map[b.hashes[1]]);
    Free(map);
}

BOOST_AUTO_TEST_SUITE_END()
