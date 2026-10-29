// Copyright (c) 2009-2010 Satoshi Nakamoto
// Copyright (c) 2009-2016 The Bitcoin Core developers
// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#ifndef BITCOIN_CONSENSUS_CONSENSUS_H
#define BITCOIN_CONSENSUS_CONSENSUS_H

#include <stdint.h>

/** The maximum allowed size for a serialized block, in bytes (only for buffer size limits) */
static const unsigned int MAX_BLOCK_SERIALIZED_SIZE = 4000000;
/** The maximum allowed weight for a block, see BIP 141 (network rule) */
static const unsigned int MAX_BLOCK_WEIGHT = 4000000;
/** The maximum allowed size for a block excluding witness data, in bytes (network rule) */
static const unsigned int MAX_BLOCK_BASE_SIZE = 1000000;
/** The maximum allowed number of signature check operations in a block (network rule) */
static const int64_t MAX_BLOCK_SIGOPS_COST = 80000;

/**
 * Hard minimum gap (in seconds) between consecutive block timestamps on
 * networks that opt in. Set the per-network value to zero to disable.
 */
static const int64_t MIN_CONSENSUS_BLOCK_SPACING = 6;

/**
 * Soft fork: a block timestamp may be at most this many seconds ahead of the node's adjusted
 * time (12 block times; it was 2 hours). Applies on mainnet from MAINNET_DRIFT_CAP_HEIGHT and on
 * testnet from genesis. Stricter than before, so nodes that have not upgraded still accept
 * everything upgraded nodes accept.
 */
static const int64_t MAX_FUTURE_DRIFT_CAPPED = 72;

/**
 * The mainnet height from which MAX_FUTURE_DRIFT_CAPPED applies: the AuxPoW height, so miners have
 * one upgrade date for both. Upgraded nodes refuse blocks stamped further ahead from here on.
 * chainparams.cpp asserts that it equals the 371337 row.
 */
static const int MAINNET_DRIFT_CAP_HEIGHT = 371337;

/**
 * Hard fork: from MAINNET_EASING_HEIGHT the LWMA target is eased for a candidate block stamped more
 * than 6 block times after its parent: target * 2^((gap - 36s) / MAINNET_EASING_TAU). Old nodes
 * require the old nBits and will not follow the new chain, so every miner and node must upgrade
 * before this height. It must lie above the 371337 row in chainparams.cpp.
 */
static const int MAINNET_EASING_HEIGHT = 600000;
static const int64_t MAINNET_EASING_TAU = 30;

/** Flags for nSequence and nLockTime locks */
enum {
    /* Interpret sequence numbers as relative lock-time constraints. */
    LOCKTIME_VERIFY_SEQUENCE = (1 << 0),

    /* Use GetMedianTimePast() instead of nTime for end point timestamp. */
    LOCKTIME_MEDIAN_TIME_PAST = (1 << 1),
};

#endif // BITCOIN_CONSENSUS_CONSENSUS_H
