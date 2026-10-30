// Copyright (c) 2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "test/test_bitcoin.h"

#include "chainparams.h"
#include "undo.h"
#include "util.h"
#include "validation.h"

#include <fstream>
#include <iterator>
#include <vector>

#include <boost/test/unit_test.hpp>

BOOST_FIXTURE_TEST_SUITE(undo_tests, TestingSetup)

namespace {

CBlockUndo MakeUndo(unsigned int nTx)
{
    CBlockUndo undo;
    for (unsigned int i = 0; i < nTx; i++) {
        CTxUndo txundo;
        CScript script;
        script << OP_DUP << OP_HASH160 << std::vector<unsigned char>(20, i + 1) << OP_EQUALVERIFY << OP_CHECKSIG;
        txundo.vprevout.push_back(CTxInUndo(CTxOut(1000 + i, script), i == 0, 100 + i, 1));
        undo.vtxundo.push_back(txundo);
    }
    return undo;
}

std::vector<char> ReadFileBytes(const CDiskBlockPos& pos)
{
    std::ifstream f(GetBlockPosFilename(pos, "rev").string(), std::ios::binary);
    return std::vector<char>((std::istreambuf_iterator<char>(f)), std::istreambuf_iterator<char>());
}

void WriteFileBytes(const CDiskBlockPos& pos, const std::vector<char>& data)
{
    std::ofstream f(GetBlockPosFilename(pos, "rev").string(), std::ios::binary | std::ios::trunc);
    f.write(data.data(), data.size());
}

/** Write one record with the given option state; returns the file size afterwards. */
size_t WriteRecord(const CBlockUndo& undo, CDiskBlockPos& pos, const uint256& hash, bool fSlim)
{
    fSlimUndo = fSlim;
    BOOST_REQUIRE(UndoWriteToDisk(undo, pos, hash, Params().MessageStart()));
    fSlimUndo = false;
    return ReadFileBytes(pos).size();
}

} // namespace

BOOST_AUTO_TEST_CASE(undo_record_both_formats)
{
    const uint256 hash1 = uint256S("0102030405060708091011121314151617181920212223242526272829303132");
    const uint256 hash2 = uint256S("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899");
    CBlockUndo undo1 = MakeUndo(3), undo2 = MakeUndo(5);
    const unsigned int size1 = ::GetSerializeSize(undo1, SER_DISK, CLIENT_VERSION);
    const unsigned int size2 = ::GetSerializeSize(undo2, SER_DISK, CLIENT_VERSION);

    // Legacy record first, then a short one after it in the same file.
    CDiskBlockPos pos1(11, 0);
    size_t end1 = WriteRecord(undo1, pos1, hash1, false);
    BOOST_CHECK_EQUAL(pos1.nPos, 8U);
    BOOST_CHECK_EQUAL(end1, 8U + size1 + 32U);

    CDiskBlockPos pos2(11, end1);
    size_t end2 = WriteRecord(undo2, pos2, hash2, true);
    BOOST_CHECK_EQUAL(pos2.nPos, end1 + 8U);
    BOOST_CHECK_EQUAL(end2, end1 + 8U + size2 + 8U);

    // The overhead helper agrees with what was written.
    fSlimUndo = true;
    BOOST_CHECK_EQUAL(UndoRecordOverhead(), 16U);
    fSlimUndo = false;
    BOOST_CHECK_EQUAL(UndoRecordOverhead(), 40U);

    // Both read back, whatever the option says at read time.
    for (int slim = 0; slim < 2; slim++) {
        fSlimUndo = slim;
        CBlockUndo r1, r2;
        BOOST_CHECK(UndoReadFromDisk(r1, pos1, hash1));
        BOOST_CHECK(UndoReadFromDisk(r2, pos2, hash2));
        BOOST_CHECK(r1.vtxundo.size() == 3 && r2.vtxundo.size() == 5);
        BOOST_CHECK(r2.vtxundo[4].vprevout[0].txout.nValue == 1004);
        // A record is keyed by the previous block hash: the wrong one is rejected.
        CBlockUndo bad;
        BOOST_CHECK(!UndoReadFromDisk(bad, pos1, hash2));
        BOOST_CHECK(!UndoReadFromDisk(bad, pos2, hash1));
    }
    fSlimUndo = false;

    // Corrupting any checksum byte, in either format, is rejected; corrupting the neighbour is not touched.
    std::vector<char> data = ReadFileBytes(pos1);
    for (size_t off : {pos1.nPos + size1, pos1.nPos + size1 + 31, pos2.nPos + size2, pos2.nPos + size2 + 7}) {
        std::vector<char> corrupt = data;
        corrupt[off] ^= 0x01;
        WriteFileBytes(pos1, corrupt);
        CBlockUndo r1, r2;
        bool ok1 = UndoReadFromDisk(r1, pos1, hash1);
        bool ok2 = UndoReadFromDisk(r2, pos2, hash2);
        BOOST_CHECK_MESSAGE(ok1 == (off >= pos2.nPos), "legacy record, corrupt byte at " << off);
        BOOST_CHECK_MESSAGE(ok2 == (off < pos2.nPos), "short record, corrupt byte at " << off);
    }

    // Clearing the short-record bit makes the reader expect a 32 byte checksum that is not there.
    {
        std::vector<char> corrupt = data;
        corrupt[pos2.nPos - 1] ^= (char)0x80; // top byte of the size word of record 2 (little endian)
        WriteFileBytes(pos1, corrupt);
        CBlockUndo r2;
        BOOST_CHECK(!UndoReadFromDisk(r2, pos2, hash2));
    }

    // A truncated short record fails cleanly.
    {
        std::vector<char> trunc(data.begin(), data.end() - 3);
        WriteFileBytes(pos1, trunc);
        CBlockUndo r2;
        BOOST_CHECK(!UndoReadFromDisk(r2, pos2, hash2));
    }
}

BOOST_AUTO_TEST_SUITE_END()
