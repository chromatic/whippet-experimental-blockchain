// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "arith_uint256.h"
#include "chainparams.h"
#include "consensus/consensus.h"
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

/* Build a linked run of blocks ending at height nHeightLast, each one
   nSolvetime seconds after its parent. nSolvetime may be negative. */
static std::vector<CBlockIndex> LwmaChain(int nHeightLast, int nBlocks, int64_t nSolvetime, unsigned int nBits)
{
    std::vector<CBlockIndex> chain(nBlocks);
    const int64_t nBaseTime = 1700000000;
    for (int i = 0; i < nBlocks; i++) {
        chain[i].nHeight = nHeightLast - (nBlocks - 1) + i;
        chain[i].nTime = (unsigned int)(nBaseTime + nSolvetime * i);
        chain[i].nBits = nBits;
        chain[i].pprev = (i == 0) ? NULL : &chain[i - 1];
    }
    return chain;
}

/* Backwards timestamps must not collapse the LWMA target. The lower solvetime
   clamp used to substitute T/6, leaving every solvetime in [-6T, 0) negative,
   and sum_time reaches the target arithmetic as a uint32_t: a negative sum
   became a ~4e9 multiplier, which here loosened the target by 13 million fold
   (0x1b499dfd to 0x1e391be9), short of powLimit and so not clamped. */
BOOST_AUTO_TEST_CASE(lwma_negative_solvetime)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(251);
    BOOST_CHECK(params.fLWMADifficultyCalculation);

    const unsigned int nBits = 0x1b499dfd;
    const unsigned int nPowLimit = UintToArith256(params.powLimit).GetCompact();
    const int64_t T = params.nPowTargetSpacing;

    // Every solvetime at exactly -6T, which the old lower clamp let through.
    std::vector<CBlockIndex> atClamp = LwmaChain(251, 12, -6 * T, nBits);
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&atClamp.back(), 0, params), nBits);

    // And past it, where the old clamp substituted a positive T/6.
    std::vector<CBlockIndex> pastClamp = LwmaChain(251, 12, -100 * T, nBits);
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&pastClamp.back(), 0, params), nBits);

    // On-target solvetimes still retarget to about the same difficulty, so
    // the guard above has not swallowed the ordinary path.
    std::vector<CBlockIndex> onTarget = LwmaChain(251, 12, T, nBits);
    const unsigned int nNext = CalculateWhippetNextWorkRequired(&onTarget.back(), 0, params);
    BOOST_CHECK(nNext != nPowLimit);
    BOOST_CHECK_CLOSE((double)arith_uint256().SetCompact(nNext).getdouble(),
                      (double)arith_uint256().SetCompact(nBits).getdouble(), 1.0);
}

/* A window with no usable pace signal must still get the easing hard fork.
   Easing keys on how long this candidate has waited, not on the window, so
   suppressing the LWMA scaling must not also suppress the easing: a guard
   that returned early here would strand a stalled chain at the fork. */
BOOST_AUTO_TEST_CASE(lwma_non_positive_sum_still_eases)
{
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& params = Params().GetConsensus(MAINNET_EASING_HEIGHT);
    BOOST_CHECK(params.fLWMADifficultyCalculation);
    BOOST_CHECK(params.nEasingTau > 0);

    const unsigned int nBits = 0x1b499dfd;
    const int64_t T = params.nPowTargetSpacing;
    const int nHeight = MAINNET_EASING_HEIGHT + 11;

    // Backwards timestamps, so sum_time is non-positive and LWMA says nothing.
    std::vector<CBlockIndex> chain = LwmaChain(nHeight, 12, -6 * T, nBits);

    // No candidate time: hold the previous target.
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&chain.back(), 0, params), nBits);

    // A candidate long past the free wait must still be eased, i.e. the target
    // must come back strictly looser than the one we held.
    const int64_t nWaited = params.nEasingFreeWait + 20 * params.nEasingTau;
    const unsigned int nEased = CalculateWhippetNextWorkRequired(
        &chain.back(), 0, params, chain.back().GetBlockTime() + nWaited);
    BOOST_CHECK(arith_uint256().SetCompact(nEased) > arith_uint256().SetCompact(nBits));
}

/* One slow solvetime among on-target ones. Before the easing fork the newest solvetime had the smallest LWMA
   weight (1 of 10), so a slow newest block moved the target only 1.09x. From the fork it has the largest
   (10 of 10) and the same block moves it 1.91x: (9 * 6 * 5 + 36 * 10) / 330 against (6 * 54 + 36) / 330. */
static std::vector<CBlockIndex> LwmaChainSlowNewest(int nHeightLast, unsigned int nBits)
{
    std::vector<CBlockIndex> chain(11);
    const int64_t T = 6;
    int64_t t = 1700000000;
    for (int i = 0; i < 11; i++) {
        if (i > 0)
            t += (i == 10) ? 6 * T : T;
        chain[i].nHeight = nHeightLast - 10 + i;
        chain[i].nTime = (unsigned int)t;
        chain[i].nBits = nBits;
        chain[i].pprev = (i == 0) ? NULL : &chain[i - 1];
    }
    return chain;
}

BOOST_AUTO_TEST_CASE(lwma_weights_flip_at_the_easing_fork)
{
    SelectParams(CBaseChainParams::MAIN);
    BOOST_CHECK(!Params().GetConsensus(MAINNET_EASING_HEIGHT - 1).fLWMANewestHeaviest);
    BOOST_CHECK(Params().GetConsensus(MAINNET_EASING_HEIGHT).fLWMANewestHeaviest);
    SelectParams(CBaseChainParams::TESTNET);
    BOOST_CHECK(!Params().GetConsensus(TESTNET_EASING_HEIGHT - 1).fLWMANewestHeaviest);
    BOOST_CHECK(Params().GetConsensus(TESTNET_EASING_HEIGHT).fLWMANewestHeaviest);

    SelectParams(CBaseChainParams::MAIN);
    const unsigned int nBits = 0x1b499dfd;
    const double base = arith_uint256().SetCompact(nBits).getdouble();

    const int nOld = 1000;
    std::vector<CBlockIndex> oldChain = LwmaChainSlowNewest(nOld, nBits);
    const unsigned int nOldNext = CalculateWhippetNextWorkRequired(&oldChain.back(), 0, Params().GetConsensus(nOld));
    BOOST_CHECK_CLOSE(arith_uint256().SetCompact(nOldNext).getdouble() / base, 360.0 / 330.0, 1.0);

    const int nNew = MAINNET_EASING_HEIGHT + 10;
    std::vector<CBlockIndex> newChain = LwmaChainSlowNewest(nNew, nBits);
    const unsigned int nNewNext = CalculateWhippetNextWorkRequired(&newChain.back(), 0, Params().GetConsensus(nNew));
    BOOST_CHECK_CLOSE(arith_uint256().SetCompact(nNewNext).getdouble() / base, 630.0 / 330.0, 1.0);
}

/* With a powLimit of 2^255 (regtest) a window of maximally slow blocks asks for 6x the old target. The
   product used to wrap 2^256 and came out far too small, i.e. much harder. It now saturates at powLimit. */
BOOST_AUTO_TEST_CASE(lwma_product_saturates_at_pow_limit)
{
    SelectParams(CBaseChainParams::MAIN);
    Consensus::Params params = Params().GetConsensus(1000);
    params.powLimit = uint256S("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff");
    const arith_uint256 limit = UintToArith256(params.powLimit);
    const unsigned int nLimitBits = limit.GetCompact();
    const arith_uint256 start = arith_uint256().SetCompact(nLimitBits) >> 1; // 2^254: 6x is 1.5 * 2^256, so it wraps

    std::vector<CBlockIndex> chain(11);
    int64_t t = 1700000000;
    for (int i = 0; i < 11; i++) {
        if (i > 0)
            t += 6 * params.nPowTargetSpacing; // clamp ceiling on every solvetime
        chain[i].nHeight = 990 + i;
        chain[i].nTime = (unsigned int)t;
        chain[i].nBits = start.GetCompact();
        chain[i].pprev = (i == 0) ? NULL : &chain[i - 1];
    }
    BOOST_CHECK_EQUAL(CalculateWhippetNextWorkRequired(&chain.back(), 0, params), nLimitBits);
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
