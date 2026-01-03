// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "arith_uint256.h"
#include "chainparams.h"
#include "whippet.h"
#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>

BOOST_FIXTURE_TEST_SUITE(whippet_tests, TestingSetup)

static CAmount ExpectedSimplifiedSubsidy(int height, const Consensus::Params& params)
{
    // Keep 500k rewards for blocks before the fix at 4500
    if (height < 4500) {
        const int halvings = height / params.nSubsidyHalvingInterval;
        if (halvings < 6) {
            return (500000 >> halvings) * COIN;
        }
        return 10000 * COIN;
    }

    // After block 4500, use 1/10th Dogecoin rewards (50k base)
    const int halvings = height / params.nSubsidyHalvingInterval;
    if (halvings < 6) {
        return (50000 >> halvings) * COIN;
    }
    return 1000 * COIN;
}

BOOST_AUTO_TEST_CASE(subsidy_first_100k_test)
{
    const CChainParams& mainParams = Params(CBaseChainParams::MAIN);
    arith_uint256 prevHash = UintToArith256(uint256S("0"));

    for (int nHeight = 0; nHeight <= 100000; nHeight++) {
        const Consensus::Params& params = mainParams.GetConsensus(nHeight);
        CAmount nSubsidy = GetWhippetBlockSubsidy(nHeight, params, ArithToUint256(prevHash));
        // Before block 4500: max 1M, after: max 100k (1/10th)
        const CAmount maxReward = nHeight < 4500
            ? (1000000 >> (nHeight / params.nSubsidyHalvingInterval)) * COIN
            : (100000 >> (nHeight / params.nSubsidyHalvingInterval)) * COIN;
        BOOST_CHECK(MoneyRange(nSubsidy));
        BOOST_CHECK(nSubsidy <= maxReward);
        // Use nSubsidy to give us some variation in previous block hash, without requiring full block templates
        prevHash += nSubsidy;
    }
}

BOOST_AUTO_TEST_CASE(subsidy_100k_145k_test)
{
    const CChainParams& mainParams = Params(CBaseChainParams::MAIN);
    arith_uint256 prevHash = UintToArith256(uint256S("0"));

    for (int nHeight = 100000; nHeight <= 145000; nHeight++) {
        const Consensus::Params& params = mainParams.GetConsensus(nHeight);
        CAmount nSubsidy = GetWhippetBlockSubsidy(nHeight, params, ArithToUint256(prevHash));
        // Before block 4500: max 1M, after: max 100k (1/10th)
        const CAmount maxReward = nHeight < 4500
            ? (1000000 >> (nHeight / params.nSubsidyHalvingInterval)) * COIN
            : (100000 >> (nHeight / params.nSubsidyHalvingInterval)) * COIN;
        BOOST_CHECK(MoneyRange(nSubsidy));
        BOOST_CHECK(nSubsidy <= maxReward);
        // Use nSubsidy to give us some variation in previous block hash, without requiring full block templates
        prevHash += nSubsidy;
    }
}

// Check the simplified rewards after block 145,000
BOOST_AUTO_TEST_CASE(subsidy_post_145k_test)
{
    const CChainParams& mainParams = Params(CBaseChainParams::MAIN);
    const uint256 prevHash = uint256S("0");

    const std::vector<int> heights = {
        145000,
        static_cast<int>(mainParams.GetConsensus(0).nSubsidyHalvingInterval - 1),
        static_cast<int>(mainParams.GetConsensus(0).nSubsidyHalvingInterval),
        static_cast<int>(mainParams.GetConsensus(0).nSubsidyHalvingInterval * 2),
        static_cast<int>(mainParams.GetConsensus(0).nSubsidyHalvingInterval * 6),
        static_cast<int>(mainParams.GetConsensus(0).nSubsidyHalvingInterval * 7)
    };

    for (const int height : heights) {
        const Consensus::Params& params = mainParams.GetConsensus(height);
        const CAmount nSubsidy = GetWhippetBlockSubsidy(height, params, prevHash);
        const CAmount nExpectedSubsidy = ExpectedSimplifiedSubsidy(height, params);
        BOOST_CHECK(MoneyRange(nSubsidy));
        BOOST_CHECK_EQUAL(nSubsidy, nExpectedSubsidy);
    }
}

BOOST_AUTO_TEST_CASE(get_next_work_difficulty_limit)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(0);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1386474927; // Block # 1

    pindexLast.nHeight = 239;
    pindexLast.nTime = 1386475638; // Block #239
    pindexLast.nBits = 0x1e0ffff0;
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), 0x1e00ffff);
}

BOOST_AUTO_TEST_CASE(get_next_work_pre_digishield)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(0);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1386942008; // Block 9359

    pindexLast.nHeight = 9599;
    pindexLast.nTime = 1386954113;
    pindexLast.nBits = 0x1c1a1206;
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), 0x1c15ea59);
}

BOOST_AUTO_TEST_CASE(get_next_work_digishield)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(145000);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1395094427;

    // First hard-fork at 145,000, which applies to block 145,001 onwards
    pindexLast.nHeight = 145000;
    pindexLast.nTime = 1395094679;
    pindexLast.nBits = 0x1b499dfd;
    // Under LWMA without full history, difficulty stays the same
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), pindexLast.nBits);
}

BOOST_AUTO_TEST_CASE(get_next_work_digishield_modulated_upper)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(145000);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1395100835;

    // Test the upper bound on modulated time using mainnet block #145,107
    pindexLast.nHeight = 145107;
    pindexLast.nTime = 1395101360;
    pindexLast.nBits = 0x1b3439cd;
    // With LWMA active and no historical chain context, difficulty remains unchanged
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), pindexLast.nBits);
}

BOOST_AUTO_TEST_CASE(get_next_work_digishield_modulated_lower)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(145000);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1395380517;

    // Test the lower bound on modulated time using mainnet block #149,423
    pindexLast.nHeight = 149423;
    pindexLast.nTime = 1395380447;
    pindexLast.nBits = 0x1b446f21;
    // With LWMA active here (N=240 starts at height 240), the function
    // returns the previous difficulty when there isn't enough context.
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), pindexLast.nBits);
}

BOOST_AUTO_TEST_CASE(get_next_work_digishield_rounding)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(145000);

    CBlockIndex pindexLast;
    int64_t nLastRetargetTime = 1395094679;

    // Test case for correct rounding of modulated time - this depends on
    // handling of integer division, and is not obvious from the code
    pindexLast.nHeight = 145001;
    pindexLast.nTime = 1395094727;
    pindexLast.nBits = 0x1b671062;
    // LWMA returns prior difficulty in this synthetic context (no chain history)
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pindexLast, nLastRetargetTime, params), pindexLast.nBits);
}

BOOST_AUTO_TEST_CASE(hardfork_parameters)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& initialParams = Params().GetConsensus(0);

    BOOST_CHECK_EQUAL(initialParams.nPowTargetTimespan, 14400);
    BOOST_CHECK_EQUAL(initialParams.fAllowLegacyBlocks, true);
    BOOST_CHECK_EQUAL(initialParams.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(initialParams.fLWMADifficultyCalculation, false);

    const Consensus::Params& initialParamsEnd = Params().GetConsensus(239);
    BOOST_CHECK_EQUAL(initialParamsEnd.nPowTargetTimespan, 14400);
    BOOST_CHECK_EQUAL(initialParamsEnd.fAllowLegacyBlocks, true);
    BOOST_CHECK_EQUAL(initialParamsEnd.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(initialParamsEnd.fLWMADifficultyCalculation, false);

    const Consensus::Params& lwmaParams = Params().GetConsensus(240);
    BOOST_CHECK_EQUAL(lwmaParams.nPowTargetTimespan, 60);
    BOOST_CHECK_EQUAL(lwmaParams.fAllowLegacyBlocks, true);
    BOOST_CHECK_EQUAL(lwmaParams.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(lwmaParams.fLWMADifficultyCalculation, true);

    const Consensus::Params& lwmaParamsEnd = Params().GetConsensus(371336);
    BOOST_CHECK_EQUAL(lwmaParamsEnd.nPowTargetTimespan, 60);
    BOOST_CHECK_EQUAL(lwmaParamsEnd.fAllowLegacyBlocks, true);
    BOOST_CHECK_EQUAL(lwmaParamsEnd.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(lwmaParamsEnd.fLWMADifficultyCalculation, true);

    const Consensus::Params& auxpowParams = Params().GetConsensus(371337);
    BOOST_CHECK_EQUAL(auxpowParams.nHeightEffective, 371337);
    BOOST_CHECK_EQUAL(auxpowParams.nPowTargetTimespan, 60);
    BOOST_CHECK_EQUAL(auxpowParams.fAllowLegacyBlocks, false);
    BOOST_CHECK_EQUAL(auxpowParams.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(auxpowParams.fLWMADifficultyCalculation, true);

    const Consensus::Params& auxpowHighParams = Params().GetConsensus(700000); // Arbitrary point after last hard-fork
    BOOST_CHECK_EQUAL(auxpowHighParams.nPowTargetTimespan, 60);
    BOOST_CHECK_EQUAL(auxpowHighParams.fAllowLegacyBlocks, false);
    BOOST_CHECK_EQUAL(auxpowHighParams.fDigishieldDifficultyCalculation, false);
    BOOST_CHECK_EQUAL(auxpowHighParams.fLWMADifficultyCalculation, true);
}

BOOST_AUTO_TEST_SUITE_END()
