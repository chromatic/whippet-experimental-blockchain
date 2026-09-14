// Copyright (c) 2009-2010 Satoshi Nakamoto
// Copyright (c) 2009-2016 The Bitcoin Core developers
// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#ifndef BITCOIN_NET_PROCESSING_H
#define BITCOIN_NET_PROCESSING_H

#include "net.h"
#include "validationinterface.h"

/** Default for -maxorphantx, maximum number of orphan transactions kept in memory */
static const unsigned int DEFAULT_MAX_ORPHAN_TRANSACTIONS = 100;
/** Expiration time for orphan transactions in seconds */
static const int64_t ORPHAN_TX_EXPIRE_TIME = 20 * 60;
/** Minimum time between orphan transactions expire time checks in seconds */
static const int64_t ORPHAN_TX_EXPIRE_INTERVAL = 5 * 60;
/** Default number of orphan+recently-replaced txn to keep around for block reconstruction */
static const unsigned int DEFAULT_BLOCK_RECONSTRUCTION_EXTRA_TXN = 100;
/** Headers download timeout expressed in microseconds
 *  Timeout = base + per_header * (expected number of headers) */
static constexpr int64_t HEADERS_DOWNLOAD_TIMEOUT_BASE = 15 * 60 * 1000000; // 15 minutes
static constexpr int64_t HEADERS_DOWNLOAD_TIMEOUT_PER_HEADER = 1000; // 1ms/header
/** Block download timeout base, expressed in millionths of the block interval. */
static constexpr int64_t BLOCK_DOWNLOAD_TIMEOUT_BASE = 5000000;
/** Additional block download timeout per parallel downloading peer. */
static constexpr int64_t BLOCK_DOWNLOAD_TIMEOUT_PER_PEER = 2500000;

/** Sets a hard floor under the multiplier used for block download timeouts.
 *
 * Upstream scales the window by the block interval, which on Bitcoin's 600
 * second spacing yields a ten minute window. That scaling does not survive a
 * six second block: the interval shrank by a hundred, the work of moving and
 * connecting a block did not, and neither did the time a peer's link can
 * legitimately take. The floor is what actually decides the window here --
 * mainnet's spacing of 6 is below it, so mainnet and regtest both land on the
 * floor -- and it is set to restore upstream's absolute ten minutes rather
 * than a hundredth of it.
 *
 * It wants to stay generous. The timer runs from when the download started,
 * not from the last byte received, so it measures wall clock rather than
 * progress: a node that is itself busy -- applying a long rollback, connecting
 * large blocks -- can exhaust the window while a peer is serving it perfectly
 * well, and disconnect the one peer that had what it needed. Detecting a peer
 * that genuinely is not delivering is the stalling logic's job (see
 * BLOCK_STALLING_TIMEOUT), and that fires in seconds. This is only a backstop.
 */
static constexpr int64_t MIN_BLOCK_DOWNLOAD_MULTIPLIER = 120; // a ten minute window, as upstream has

/** The block download timeout, in microseconds, for a peer we are downloading
 * from while nOtherPeersWithValidatedDownloads others are also serving us.
 * Factored out of SendMessages() so it can be reasoned about on its own.
 */
static constexpr int64_t GetBlockDownloadTimeout(int64_t nPowTargetSpacing, int nOtherPeersWithValidatedDownloads)
{
    return (nPowTargetSpacing > MIN_BLOCK_DOWNLOAD_MULTIPLIER ? nPowTargetSpacing : MIN_BLOCK_DOWNLOAD_MULTIPLIER) *
        (BLOCK_DOWNLOAD_TIMEOUT_BASE + BLOCK_DOWNLOAD_TIMEOUT_PER_PEER * nOtherPeersWithValidatedDownloads);
}

/** The maximum rate of address records we're willing to process on average.
 * Is bypassed for whitelisted connections. */
static constexpr double MAX_ADDR_RATE_PER_SECOND{0.1};

/** The soft limit of the address processing token bucket (the regular MAX_ADDR_RATE_PER_SECOND
 *  based increments won't go above this, but the MAX_ADDR_TO_SEND increment following GETADDR
 *  is exempt from this limit. */
static constexpr size_t MAX_ADDR_PROCESSING_TOKEN_BUCKET{MAX_ADDR_TO_SEND};

/** Register with a network node to receive its signals */
void RegisterNodeSignals(CNodeSignals& nodeSignals);
/** Unregister a network node */
void UnregisterNodeSignals(CNodeSignals& nodeSignals);

class PeerLogicValidation : public CValidationInterface {
private:
    CConnman* connman;

public:
    PeerLogicValidation(CConnman* connmanIn);

    virtual void SyncTransaction(const CTransaction& tx, const CBlockIndex* pindex, int nPosInBlock);
    virtual void UpdatedBlockTip(const CBlockIndex *pindexNew, const CBlockIndex *pindexFork, bool fInitialDownload);
    virtual void BlockChecked(const CBlock& block, const CValidationState& state);
    virtual void NewPoWValidBlock(const CBlockIndex *pindex, const std::shared_ptr<const CBlock>& pblock);
};

struct CNodeStateStats {
    int nMisbehavior;
    int nSyncHeight;
    int nCommonHeight;
    std::vector<int> vHeightInFlight;
};

/** Get statistics from node state */
bool GetNodeStateStats(NodeId nodeid, CNodeStateStats &stats);
/** Increase a node's misbehavior score. */
void Misbehaving(NodeId nodeid, int howmuch);

/** Process protocol messages received from a given node */
bool ProcessMessages(CNode* pfrom, CConnman& connman, const std::atomic<bool>& interrupt);
/**
 * Send queued protocol messages to be sent to a give node.
 *
 * @param[in]   pto             The node which we are sending messages to.
 * @param[in]   connman         The connection manager for that node.
 * @param[in]   interrupt       Interrupt condition for processing threads
 * @return                      True if there is more work to be done
 */
bool SendMessages(CNode* pto, CConnman& connman, const std::atomic<bool>& interrupt);

#endif // BITCOIN_NET_PROCESSING_H
