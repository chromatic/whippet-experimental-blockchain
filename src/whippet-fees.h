// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#ifndef BITCOIN_WHTCOIN_FEES_H
#define BITCOIN_WHTCOIN_FEES_H

#include "amount.h"
#include "chain.h"
#include "chainparams.h"

#ifdef ENABLE_WALLET

enum FeeRatePreset
{
    MINIMUM,
    MORE,
    WOW,
    AMAZE,
    MANY_GENEROUS,
    SUCH_EXPENSIVE
};

/** Estimate fee rate needed to get into the next nBlocks */
CFeeRate GetWhippetFeeRate(int priority);
const std::string GetWhippetPriorityLabel(int priority);
#endif // ENABLE_WALLET
CAmount GetWhippetMinRelayFee(const CTransaction& tx, unsigned int nBytes, bool fAllowFree);
CAmount GetWhippetDustFee(const std::vector<CTxOut> &vout, const CAmount dustLimit);

#endif // BITCOIN_WHTCOIN_FEES_H
