// Copyright (c) 2009-2010 Satoshi Nakamoto
// Copyright (c) 2009-2016 The Bitcoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "txdb.h"

#include "chainparams.h"
#include "hash.h"
#include "pow.h"
#include "uint256.h"

#include <algorithm>
#include <stdint.h>
#include <string.h>

#include <boost/thread.hpp>

static const char DB_COINS = 'c';
static const char DB_BLOCK_FILES = 'f';
static const char DB_TXINDEX = 't';
static const char DB_BLOCK_INDEX = 'b';

static const char DB_BEST_BLOCK = 'B';
static const char DB_FLAG = 'F';
static const char DB_REINDEX_FLAG = 'R';
static const char DB_LAST_BLOCK = 'l';
static const char DB_BLOCK_INDEX_VERSION = 'V';


CCoinsViewDB::CCoinsViewDB(size_t nCacheSize, bool fMemory, bool fWipe) : db(GetDataDir() / "chainstate", nCacheSize, fMemory, fWipe, true) 
{
}

bool CCoinsViewDB::GetCoins(const uint256 &txid, CCoins &coins) const {
    return db.Read(std::make_pair(DB_COINS, txid), coins);
}

bool CCoinsViewDB::HaveCoins(const uint256 &txid) const {
    return db.Exists(std::make_pair(DB_COINS, txid));
}

uint256 CCoinsViewDB::GetBestBlock() const {
    uint256 hashBestChain;
    if (!db.Read(DB_BEST_BLOCK, hashBestChain))
        return uint256();
    return hashBestChain;
}

bool CCoinsViewDB::BatchWrite(CCoinsMap &mapCoins, const uint256 &hashBlock) {
    CDBBatch batch(db);
    size_t count = 0;
    size_t changed = 0;
    for (CCoinsMap::iterator it = mapCoins.begin(); it != mapCoins.end();) {
        if (it->second.flags & CCoinsCacheEntry::DIRTY) {
            if (it->second.coins.IsPruned())
                batch.Erase(std::make_pair(DB_COINS, it->first));
            else
                batch.Write(std::make_pair(DB_COINS, it->first), it->second.coins);
            changed++;
        }
        count++;
        CCoinsMap::iterator itOld = it++;
        mapCoins.erase(itOld);
    }
    if (!hashBlock.IsNull())
        batch.Write(DB_BEST_BLOCK, hashBlock);

    LogPrint("coindb", "Committing %u changed transactions (out of %u) to coin database...\n", (unsigned int)changed, (unsigned int)count);
    return db.WriteBatch(batch);
}

CBlockTreeDB::CBlockTreeDB(size_t nCacheSize, bool fMemory, bool fWipe) : CDBWrapper(GetDataDir() / "blocks" / "index", nCacheSize, fMemory, fWipe) {
}

bool CBlockTreeDB::ReadBlockFileInfo(int nFile, CBlockFileInfo &info) {
    return Read(std::make_pair(DB_BLOCK_FILES, nFile), info);
}

bool CBlockTreeDB::WriteReindexing(bool fReindexing) {
    if (fReindexing)
        return Write(DB_REINDEX_FLAG, '1');
    else
        return Erase(DB_REINDEX_FLAG);
}

bool CBlockTreeDB::ReadReindexing(bool &fReindexing) {
    fReindexing = Exists(DB_REINDEX_FLAG);
    return true;
}

bool CBlockTreeDB::ReadLastBlockFile(int &nFile) {
    return Read(DB_LAST_BLOCK, nFile);
}

bool CBlockTreeDB::WriteBlockIndexVersion(int nVersion) {
    return Write(DB_BLOCK_INDEX_VERSION, nVersion);
}

bool CBlockTreeDB::ReadBlockIndexVersion(int &nVersion) {
    if (!Read(DB_BLOCK_INDEX_VERSION, nVersion))
        return false;
    return true;
}

CCoinsViewCursor *CCoinsViewDB::Cursor() const
{
    CCoinsViewDBCursor *i = new CCoinsViewDBCursor(const_cast<CDBWrapper*>(&db)->NewIterator(), GetBestBlock());
    /* It seems that there are no "const iterators" for LevelDB.  Since we
       only need read operations on it, use a const-cast to get around
       that restriction.  */
    i->pcursor->Seek(DB_COINS);
    // Cache key of first record
    if (i->pcursor->Valid()) {
        i->pcursor->GetKey(i->keyTmp);
    } else {
        i->keyTmp.first = 0; // Make sure Valid() and GetKey() return false
    }
    return i;
}

bool CCoinsViewDBCursor::GetKey(uint256 &key) const
{
    // Return cached key
    if (keyTmp.first == DB_COINS) {
        key = keyTmp.second;
        return true;
    }
    return false;
}

bool CCoinsViewDBCursor::GetValue(CCoins &coins) const
{
    return pcursor->GetValue(coins);
}

unsigned int CCoinsViewDBCursor::GetValueSize() const
{
    return pcursor->GetValueSize();
}

bool CCoinsViewDBCursor::Valid() const
{
    return keyTmp.first == DB_COINS;
}

void CCoinsViewDBCursor::Next()
{
    pcursor->Next();
    if (!pcursor->Valid() || !pcursor->GetKey(keyTmp))
        keyTmp.first = 0; // Invalidate cached key after last record so that Valid() and GetKey() return false
}

bool CBlockTreeDB::WriteBatchSync(const std::vector<std::pair<int, const CBlockFileInfo*> >& fileInfo, int nLastFile, const std::vector<const CBlockIndex*>& blockinfo) {
    CDBBatch batch(*this);
    for (std::vector<std::pair<int, const CBlockFileInfo*> >::const_iterator it=fileInfo.begin(); it != fileInfo.end(); it++) {
        batch.Write(std::make_pair(DB_BLOCK_FILES, it->first), *it->second);
    }
    batch.Write(DB_LAST_BLOCK, nLastFile);
    for (std::vector<const CBlockIndex*>::const_iterator it=blockinfo.begin(); it != blockinfo.end(); it++) {
        batch.Write(std::make_pair(DB_BLOCK_INDEX, (*it)->GetBlockHash()), CDiskBlockIndex(*it, fWriteSlimIndex));
    }
    if (!WriteBatch(batch, true))
        return false;
    if (fCompactAfterWrite) {
        fCompactAfterWrite = false;
        LogPrintf("Compacting the block index after converting it to the slim format\n");
        CompactAll();
    }
    return true;
}

bool CBlockTreeDB::ReadTxIndex(const uint256 &txid, CDiskTxPos &pos) {
    return Read(std::make_pair(DB_TXINDEX, txid), pos);
}

bool CBlockTreeDB::WriteTxIndex(const std::vector<std::pair<uint256, CDiskTxPos> >&vect) {
    CDBBatch batch(*this);
    for (std::vector<std::pair<uint256,CDiskTxPos> >::const_iterator it=vect.begin(); it!=vect.end(); it++)
        batch.Write(std::make_pair(DB_TXINDEX, it->first), it->second);
    return WriteBatch(batch);
}

bool CBlockTreeDB::WriteFlag(const std::string &name, bool fValue) {
    return Write(std::make_pair(DB_FLAG, name), fValue ? '1' : '0');
}

bool CBlockTreeDB::ReadFlag(const std::string &name, bool &fValue) {
    char ch;
    if (!Read(std::make_pair(DB_FLAG, name), ch))
        return false;
    fValue = ch == '1';
    return true;
}

bool CBlockTreeDB::LoadBlockIndexGuts(std::function<CBlockIndex*(const uint256&)> insertBlockIndex)
{
    std::unique_ptr<CDBIterator> pcursor(NewIterator());

    pcursor->Seek(std::make_pair(DB_BLOCK_INDEX, uint256()));

    nLegacyEntries = 0;
    nLegacyBytes = 0;
    nSlimBytes = 0;

    // Slim entries only carry a prefix of hashPrev, so their parents are
    // resolved after every entry is loaded.
    std::vector<std::pair<CBlockIndex*, uint32_t> > vSlim;
    std::vector<std::pair<int, CBlockIndex*> > vByHeight;

    // Load mapBlockIndex
    while (pcursor->Valid()) {
        boost::this_thread::interruption_point();
        std::pair<char, uint256> key;
        if (pcursor->GetKey(key) && key.first == DB_BLOCK_INDEX) {
            CDiskBlockIndex diskindex;
            if (pcursor->GetValue(diskindex)) {
                // Construct block index object
                CBlockIndex* pindexNew = insertBlockIndex(key.second);
                pindexNew->nHeight        = diskindex.nHeight;
                pindexNew->nFile          = diskindex.nFile;
                pindexNew->nDataPos       = diskindex.nDataPos;
                pindexNew->nUndoPos       = diskindex.nUndoPos;
                pindexNew->nVersion       = diskindex.nVersion;
                pindexNew->hashMerkleRoot = diskindex.hashMerkleRoot;
                pindexNew->nTime          = diskindex.nTime;
                pindexNew->nBits          = diskindex.nBits;
                pindexNew->nNonce         = diskindex.nNonce;
                pindexNew->nStatus        = diskindex.nStatus;
                pindexNew->nTx            = diskindex.nTx;
                pindexNew->dDifficulty    = diskindex.fSlim ? GetDifficultyFromBits(pindexNew->nBits) : diskindex.dDifficulty;

                if (diskindex.fSlim) {
                    uint32_t prefix;
                    memcpy(&prefix, diskindex.hashPrev.begin(), sizeof(prefix));
                    vSlim.push_back(std::make_pair(pindexNew, prefix));
                    nSlimBytes += pcursor->GetValueSize();
                } else {
                    pindexNew->pprev = insertBlockIndex(diskindex.hashPrev);
                    nLegacyEntries++;
                    nLegacyBytes += pcursor->GetValueSize();
                }
                vByHeight.push_back(std::make_pair(pindexNew->nHeight, pindexNew));

                /* Bitcoin checks the PoW here.  We don't do this because
                   the CDiskBlockIndex does not contain the auxpow.
                   This check isn't important, since the data on disk should
                   already be valid and can be trusted.  */

                pcursor->Next();
            } else {
                return error("LoadBlockIndex() : failed to read value");
            }
        } else {
            break;
        }
    }

    if (!vSlim.empty()) {
        std::sort(vByHeight.begin(), vByHeight.end());
        for (const auto& item : vSlim) {
            CBlockIndex* pindex = item.first;
            if (pindex->nHeight == 0)
                continue;
            auto range = std::equal_range(vByHeight.begin(), vByHeight.end(),
                std::make_pair(pindex->nHeight - 1, (CBlockIndex*)NULL),
                [](const std::pair<int, CBlockIndex*>& a, const std::pair<int, CBlockIndex*>& b) { return a.first < b.first; });
            CBlockIndex* pprev = NULL;
            for (auto it = range.first; it != range.second; ++it) {
                uint32_t candidate;
                memcpy(&candidate, it->second->phashBlock->begin(), sizeof(candidate));
                if (candidate == item.second) {
                    if (pprev)
                        return error("LoadBlockIndex() : ambiguous parent prefix at height %d", pindex->nHeight);
                    pprev = it->second;
                }
            }
            if (!pprev)
                return error("LoadBlockIndex() : no parent found at height %d", pindex->nHeight);
            pindex->pprev = pprev;
        }
    }

    return true;
}
