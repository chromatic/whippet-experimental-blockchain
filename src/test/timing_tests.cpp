// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "chain.h"
#include "chainparams.h"
#include "consensus/consensus.h"
#include "consensus/validation.h"
#include "pow.h"
#include "primitives/block.h"
#include "timedata.h"
#include "util.h"
#include "validation.h"
#include "whippet.h"
#include "arith_uint256.h"
#include "test/test_bitcoin.h"

#include <vector>

#include <boost/test/unit_test.hpp>

namespace {

/** Check a header stamped `time` against a parent at `parentTime`, at `adjusted`. */
bool CheckHeader(int64_t parentTime, int64_t time, int64_t adjusted, std::string& reason)
{
    // A short chain, so that difficulty and median-time-past have ancestors to walk.
    std::vector<CBlockIndex> chain(12);
    for (size_t i = 0; i < chain.size(); i++) {
        chain[i].nHeight = 89 + i;
        chain[i].nTime = parentTime - 6 * (chain.size() - 1 - i);
        chain[i].nBits = 0x207fffff;
        chain[i].pprev = i ? &chain[i - 1] : NULL;
    }
    CBlockIndex& parent = chain.back();

    CBlockHeader header;
    header.nVersion = 0x00620004;
    header.nTime = time;
    header.nBits = GetNextWorkRequired(&parent, &header, Params().GetConsensus(parent.nHeight + 1));

    CValidationState state;
    bool ok = ContextualCheckBlockHeader(header, state, &parent, adjusted);
    reason = state.GetRejectReason();
    return ok;
}

/** Sets the regtest timing arguments for one test and restores the defaults afterwards. */
struct TimingArgs {
    TimingArgs(const std::string& profile, const std::string& floor = "4", const std::string& mindiff = "0")
    {
        ForceSetArg("-timingprofile", profile);
        ForceSetArg("-timingfloor", floor);
        ForceSetArg("-timingmindiff", mindiff);
    }
    ~TimingArgs()
    {
        ForceSetArg("-timingprofile", "");
        ForceSetArg("-timingfloor", "4");
        ForceSetArg("-timingmindiff", "0");
        ForceSetArg("-timingdriftcapheight", "0");
        SelectParams(CBaseChainParams::REGTEST);
    }
};

} // namespace

BOOST_FIXTURE_TEST_SUITE(timing_tests, BasicTestingSetup)

BOOST_AUTO_TEST_CASE(future_drift_default_boundary)
{
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(101).nMaxFutureDrift, 7200);

    const int64_t adjusted = 1800000000;
    std::string reason;
    BOOST_CHECK(CheckHeader(adjusted - 100, adjusted + 7200, adjusted, reason));
    BOOST_CHECK(!CheckHeader(adjusted - 100, adjusted + 7201, adjusted, reason));
    BOOST_CHECK_EQUAL(reason, "time-too-new");
}

/** Header check on mainnet parameters for a block at height `parentHeight + 1` with the given nVersion. */
static std::string MainnetHeaderResult(int parentHeight, int32_t nVersion)
{
    const int64_t parentTime = 1800000000;
    std::vector<CBlockIndex> chain(12);
    for (size_t i = 0; i < chain.size(); i++) {
        chain[i].nHeight = parentHeight - 11 + i;
        chain[i].nTime = parentTime - 6 * (chain.size() - 1 - i);
        chain[i].nBits = 0x1e0ffff0;
        chain[i].pprev = i ? &chain[i - 1] : NULL;
    }
    CBlockHeader header;
    header.nVersion = nVersion;
    header.nTime = parentTime + 6;
    header.nBits = GetNextWorkRequired(&chain.back(), &header, Params().GetConsensus(parentHeight + 1));
    CValidationState state;
    return ContextualCheckBlockHeader(header, state, &chain.back(), header.nTime) ? "accepted" : state.GetRejectReason();
}

BOOST_AUTO_TEST_CASE(mainnet_header_rules_either_side_of_the_auxpow_height)
{
    // Only legacy-format blocks (version 1, or version 2 with no chain ID) are refused from 371337.
    // A standalone block whose version carries the chain ID is neither legacy nor AuxPoW and stays valid.
    SelectParams(CBaseChainParams::MAIN);
    const int32_t chainId = Params().GetConsensus(371337).nAuxpowChainId;
    const int32_t standalone = (chainId << 16) | 4;
    const int32_t auxpow = standalone | (1 << 8);

    BOOST_CHECK_EQUAL(MainnetHeaderResult(371335, standalone), "accepted");   // height 371336
    BOOST_CHECK_EQUAL(MainnetHeaderResult(371335, 1), "accepted");            // legacy still valid before
    BOOST_CHECK_EQUAL(MainnetHeaderResult(371335, auxpow), "early-auxpow-block");

    BOOST_CHECK_EQUAL(MainnetHeaderResult(371336, standalone), "accepted");   // height 371337
    BOOST_CHECK_EQUAL(MainnetHeaderResult(371336, auxpow), "accepted");
    BOOST_CHECK_EQUAL(MainnetHeaderResult(371336, 1), "late-legacy-block");
    SelectParams(CBaseChainParams::REGTEST);
}

/** Header check on mainnet parameters for a block whose time is `secondsAhead` of the adjusted time. */
static std::string MainnetDriftResult(int parentHeight, int64_t secondsAhead)
{
    const int64_t adjusted = 1800001000;
    const int64_t parentTime = adjusted - 100;
    std::vector<CBlockIndex> chain(12);
    for (size_t i = 0; i < chain.size(); i++) {
        chain[i].nHeight = parentHeight - 11 + i;
        chain[i].nTime = parentTime - 6 * (chain.size() - 1 - i);
        chain[i].nBits = 0x1e0ffff0;
        chain[i].pprev = i ? &chain[i - 1] : NULL;
    }
    CBlockHeader header;
    header.nVersion = (Params().GetConsensus(parentHeight + 1).nAuxpowChainId << 16) | 4;
    header.nTime = adjusted + secondsAhead;
    header.nBits = GetNextWorkRequired(&chain.back(), &header, Params().GetConsensus(parentHeight + 1));
    CValidationState state;
    return ContextualCheckBlockHeader(header, state, &chain.back(), adjusted) ? "accepted" : state.GetRejectReason();
}

BOOST_AUTO_TEST_CASE(future_drift_by_network_and_height)
{
    // Regtest and the first part of mainnet keep the 2 hour limit; testnet and mainnet from the soft
    // fork height use 12 block times. Each height branch is a copy of a base set; a field missed on the
    // way would be garbage, so check them all.
    const int heights[] = {0, 1, 10, 20, 239, 240, 79999, 80000, 99999, 100000, 145000, 157499, 157500,
                           158099, 158100, MAINNET_DRIFT_CAP_HEIGHT - 1, MAINNET_DRIFT_CAP_HEIGHT, 371336, 371337, 1000000};
    for (int h : heights) {
        SelectParams(CBaseChainParams::MAIN);
        BOOST_CHECK_MESSAGE(Params().GetConsensus(h).nMaxFutureDrift == (h < MAINNET_DRIFT_CAP_HEIGHT ? 7200 : MAX_FUTURE_DRIFT_CAPPED),
                            "main height " << h);
        SelectParams(CBaseChainParams::TESTNET);
        BOOST_CHECK_MESSAGE(Params().GetConsensus(h).nMaxFutureDrift == MAX_FUTURE_DRIFT_CAPPED, "test height " << h);
        SelectParams(CBaseChainParams::REGTEST);
        BOOST_CHECK_MESSAGE(Params().GetConsensus(h).nMaxFutureDrift == 7200, "regtest height " << h);
    }
}

BOOST_AUTO_TEST_CASE(soft_fork_boundary_on_mainnet)
{
    SelectParams(CBaseChainParams::MAIN);
    const int H = MAINNET_DRIFT_CAP_HEIGHT;
    // The last block under the old rule: 2 hours still allowed.
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 2, 7200), "accepted");
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 2, 73), "accepted");
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 2, 7201), "time-too-new");
    // The first block under the new rule.
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 1, 72), "accepted");
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 1, 73), "time-too-new");
    BOOST_CHECK_EQUAL(MainnetDriftResult(H - 1, 7200), "time-too-new");
    // It stays in force after the AuxPoW height.
    BOOST_CHECK_EQUAL(MainnetDriftResult(371336, 72), "accepted");
    BOOST_CHECK_EQUAL(MainnetDriftResult(371336, 73), "time-too-new");
    SelectParams(CBaseChainParams::REGTEST);
}

BOOST_AUTO_TEST_CASE(soft_fork_row_only_tightens_the_drift)
{
    // A soft fork may only make validation stricter. Every other field must be what it was the block before.
    SelectParams(CBaseChainParams::MAIN);
    const Consensus::Params& before = Params().GetConsensus(MAINNET_DRIFT_CAP_HEIGHT - 1);
    const Consensus::Params& after = Params().GetConsensus(MAINNET_DRIFT_CAP_HEIGHT);
    BOOST_CHECK(after.nMaxFutureDrift < before.nMaxFutureDrift);
    BOOST_CHECK_EQUAL(after.nMinBlockSpacing, before.nMinBlockSpacing);
    BOOST_CHECK_EQUAL(after.nPowTargetSpacing, before.nPowTargetSpacing);
    BOOST_CHECK_EQUAL(after.nPowTargetTimespan, before.nPowTargetTimespan);
    BOOST_CHECK_EQUAL(after.fLWMADifficultyCalculation, before.fLWMADifficultyCalculation);
    BOOST_CHECK_EQUAL(after.fDigishieldDifficultyCalculation, before.fDigishieldDifficultyCalculation);
    BOOST_CHECK_EQUAL(after.fPowAllowMinDifficultyBlocks, before.fPowAllowMinDifficultyBlocks);
    BOOST_CHECK_EQUAL(after.fPowNoRetargeting, before.fPowNoRetargeting);
    BOOST_CHECK(after.powLimit == before.powLimit);
    BOOST_CHECK_EQUAL(after.fAllowLegacyBlocks, before.fAllowLegacyBlocks);
    BOOST_CHECK_EQUAL(after.fStrictChainId, before.fStrictChainId);
    BOOST_CHECK_EQUAL(after.nAuxpowChainId, before.nAuxpowChainId);
    BOOST_CHECK_EQUAL(after.nCoinbaseMaturity, before.nCoinbaseMaturity);
    BOOST_CHECK_EQUAL(after.nSubsidyHalvingInterval, before.nSubsidyHalvingInterval);
    BOOST_CHECK_EQUAL(after.fSimplifiedRewards, before.fSimplifiedRewards);
    BOOST_CHECK_EQUAL(after.UAPMintHeight, before.UAPMintHeight);
    BOOST_CHECK(after.hashGenesisBlock == before.hashGenesisBlock);
    // And the AuxPoW row after it keeps the cap while changing only what it always changed.
    const Consensus::Params& auxpow = Params().GetConsensus(371337);
    BOOST_CHECK_EQUAL(auxpow.nMaxFutureDrift, after.nMaxFutureDrift);
    BOOST_CHECK_EQUAL(auxpow.nMinBlockSpacing, after.nMinBlockSpacing);
    BOOST_CHECK(auxpow.hashGenesisBlock == after.hashGenesisBlock);
    SelectParams(CBaseChainParams::REGTEST);
}

BOOST_AUTO_TEST_CASE(testnet_floor_is_below_the_target_spacing_from_genesis)
{
    // With the floor equal to the target spacing, LWMA never sees a fast block and cannot raise difficulty.
    SelectParams(CBaseChainParams::TESTNET);
    const int heights[] = {0, 1, 239, 240, 79999, 80000, 99999, 100000, 157500, 158100, 1000000};
    for (int h : heights) {
        const Consensus::Params& p = Params().GetConsensus(h);
        BOOST_CHECK_MESSAGE(p.nMinBlockSpacing == 4 && p.nMinBlockSpacing < p.nPowTargetSpacing, "height " << h);
    }
    SelectParams(CBaseChainParams::REGTEST);
}

BOOST_AUTO_TEST_CASE(default_time_adjustment_stays_well_inside_the_drift_cap)
{
    // Peers may move a node's clock by at most this much. It has to be a small part of the 72s window,
    // or a node can be talked into accepting or refusing blocks it should not.
    BOOST_CHECK_EQUAL(DEFAULT_MAX_TIME_ADJUSTMENT, 36);
    BOOST_CHECK(2 * DEFAULT_MAX_TIME_ADJUSTMENT <= MAX_FUTURE_DRIFT_CAPPED);
}

BOOST_AUTO_TEST_CASE(profile_whippet_values)
{
    TimingArgs args("whippet");
    SelectParams(CBaseChainParams::REGTEST);
    const int heights[] = {0, 15, 25, 90000};
    for (int h : heights) {
        const Consensus::Params& p = Params().GetConsensus(h);
        BOOST_CHECK_EQUAL(p.nPowTargetSpacing, 6);
        BOOST_CHECK_EQUAL(p.nMinBlockSpacing, 4);
        BOOST_CHECK(p.fLWMADifficultyCalculation);
        BOOST_CHECK(!p.fPowNoRetargeting);
        BOOST_CHECK(!p.fPowAllowMinDifficultyBlocks);
        BOOST_CHECK_EQUAL(p.DifficultyAdjustmentInterval(), 10);
        BOOST_CHECK_EQUAL(p.nMaxFutureDrift, 7200);
    }
}

BOOST_AUTO_TEST_CASE(profile_scaled_drift_boundary)
{
    TimingArgs args("scaled-drift");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(101).nMaxFutureDrift, 72);

    const int64_t adjusted = 1800000000;
    std::string reason;
    BOOST_CHECK(CheckHeader(adjusted - 100, adjusted + 72, adjusted, reason));
    BOOST_CHECK(!CheckHeader(adjusted - 100, adjusted + 73, adjusted, reason));
    BOOST_CHECK_EQUAL(reason, "time-too-new");
}

BOOST_AUTO_TEST_CASE(regtest_drift_cap_height_tightens_only_from_that_height)
{
    TimingArgs args("");
    ForceSetArg("-timingdriftcapheight", "500");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(499).nMaxFutureDrift, 7200);
    BOOST_CHECK_EQUAL(Params().GetConsensus(500).nMaxFutureDrift, 72);
    BOOST_CHECK_EQUAL(Params().GetConsensus(79999).nMaxFutureDrift, 72);
    BOOST_CHECK_EQUAL(Params().GetConsensus(80000).nMaxFutureDrift, 72); // the chain id fix row inherits it
    // Only the drift differs from the row before.
    const Consensus::Params& before = Params().GetConsensus(499);
    const Consensus::Params& after = Params().GetConsensus(500);
    BOOST_CHECK_EQUAL(before.nAuxpowChainId, after.nAuxpowChainId);
    BOOST_CHECK_EQUAL(before.nMinBlockSpacing, after.nMinBlockSpacing);
    BOOST_CHECK_EQUAL(before.nPowTargetSpacing, after.nPowTargetSpacing);
    BOOST_CHECK_EQUAL(before.fAllowLegacyBlocks, after.fAllowLegacyBlocks);
    BOOST_CHECK_EQUAL(Params().GetConsensus(80000).nAuxpowChainId, 0x5750);

    // Selecting again without the option restores the plain rows.
    ForceSetArg("-timingdriftcapheight", "0");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(500).nMaxFutureDrift, 7200);
    BOOST_CHECK_EQUAL(Params().GetConsensus(80000).nMaxFutureDrift, 7200);

    // The cap height has to lie between the AuxPoW row and the chain id fix row.
    ForceSetArg("-timingdriftcapheight", "20");
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::REGTEST), std::runtime_error);
    ForceSetArg("-timingdriftcapheight", "80000");
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::REGTEST), std::runtime_error);
    ForceSetArg("-timingdriftcapheight", "500");
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::MAIN), std::runtime_error);
}

BOOST_AUTO_TEST_CASE(profile_floor_override)
{
    TimingArgs args("whippet", "0");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(101).nMinBlockSpacing, 0);
}

BOOST_AUTO_TEST_CASE(profile_unset_is_unchanged)
{
    {
        TimingArgs args("whippet");
        SelectParams(CBaseChainParams::REGTEST);
    }
    // The destructor re-selects regtest with the profile cleared.
    const Consensus::Params& p = Params().GetConsensus(101);
    BOOST_CHECK_EQUAL(p.nPowTargetSpacing, 1);
    BOOST_CHECK_EQUAL(p.nMinBlockSpacing, 0);
    BOOST_CHECK(p.fPowNoRetargeting);
    BOOST_CHECK(!p.fLWMADifficultyCalculation);
}

BOOST_AUTO_TEST_CASE(profile_mindiff_flag)
{
    {
        TimingArgs args("whippet", "4", "1");
        SelectParams(CBaseChainParams::REGTEST);
        BOOST_CHECK(Params().GetConsensus(101).fPowAllowMinDifficultyBlocks);
    }
    TimingArgs args("whippet");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK(!Params().GetConsensus(101).fPowAllowMinDifficultyBlocks);
}

BOOST_AUTO_TEST_CASE(profile_unknown_throws)
{
    TimingArgs args("bogus");
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::REGTEST), std::runtime_error);
}

BOOST_AUTO_TEST_CASE(profile_rejected_off_regtest)
{
    TimingArgs args("whippet");
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::TESTNET), std::runtime_error);
    BOOST_CHECK_THROW(SelectParams(CBaseChainParams::MAIN), std::runtime_error);
}

BOOST_AUTO_TEST_CASE(ease_target_doubles_every_tau)
{
    const arith_uint256 limit = UintToArith256(Params().GetConsensus(101).powLimit);
    const arith_uint256 base = arith_uint256(1) << 100;
    BOOST_CHECK(EaseTarget(base, 0, 30, limit) == base);
    BOOST_CHECK(EaseTarget(base, -5, 30, limit) == base);
    BOOST_CHECK(EaseTarget(base, 30, 30, limit) == base << 1);
    BOOST_CHECK(EaseTarget(base, 90, 30, limit) == base << 3);
    // The fractional part is a monotone cubic between the powers of two.
    arith_uint256 last = base;
    for (int extra = 1; extra <= 30; extra++) {
        arith_uint256 t = EaseTarget(base, extra, 30, limit);
        BOOST_CHECK(t >= last);
        BOOST_CHECK(t <= (base << 1));
        last = t;
    }
    // Half a tau is 2^0.5 = 1.41421..., to within the polynomial's 0.013%.
    const arith_uint256 half = EaseTarget(base, 15, 30, limit) >> 90; // 2^10 * 1.41421 = 1448.15
    BOOST_CHECK(half >= 1447 && half <= 1449);
}

BOOST_AUTO_TEST_CASE(ease_target_clamps_to_the_pow_limit)
{
    const arith_uint256 limit = UintToArith256(Params().GetConsensus(101).powLimit);
    const arith_uint256 base = arith_uint256(1) << 200;
    BOOST_CHECK(EaseTarget(base, 30 * 60, 30, limit) == limit);
    BOOST_CHECK(EaseTarget(base, 30 * 300, 30, limit) == limit);
    BOOST_CHECK(EaseTarget(limit, 1, 30, limit) == limit);
    // The LWMA product can overshoot powLimit before easing; that must not wrap.
    BOOST_CHECK(EaseTarget(limit + (limit >> 1), 25, 30, limit) == limit);
    BOOST_CHECK(EaseTarget(~arith_uint256(0), 25, 30, limit) == limit);
}

BOOST_AUTO_TEST_CASE(profile_easing_values_and_header_check)
{
    TimingArgs args("scaled-drift-easing");
    SelectParams(CBaseChainParams::REGTEST);
    const Consensus::Params& p = Params().GetConsensus(145001);
    BOOST_CHECK_EQUAL(p.nEasingFreeWait, 36);
    BOOST_CHECK_EQUAL(p.nEasingTau, 30);
    BOOST_CHECK_EQUAL(p.nMaxFutureDrift, 72);

    // Build a parent chain at the every-block retarget regime, then compare bits for a quick and a late child.
    std::vector<CBlockIndex> chain(12);
    for (size_t i = 0; i < chain.size(); i++) {
        chain[i].nHeight = 145000 + i;
        chain[i].nTime = 1800000000 + 6 * i;
        chain[i].nBits = 0x1e0fffff;
        chain[i].pprev = i ? &chain[i - 1] : NULL;
    }
    CBlockIndex& parent = chain.back();
    CBlockHeader quick, inside, late, later;
    quick.nTime = parent.nTime + 6;
    inside.nTime = parent.nTime + 36;
    late.nTime = parent.nTime + 66;
    later.nTime = parent.nTime + 96;
    const unsigned int bq = GetNextWorkRequired(&parent, &quick, p);
    const unsigned int bi = GetNextWorkRequired(&parent, &inside, p);
    const unsigned int bl = GetNextWorkRequired(&parent, &late, p);
    const unsigned int bw = GetNextWorkRequired(&parent, &later, p);
    BOOST_CHECK_EQUAL(bq, bi);
    arith_uint256 tq, tl, tw;
    tq.SetCompact(bq); tl.SetCompact(bl); tw.SetCompact(bw);
    // 30s and 60s past the free window: twice and four times the target, within compact rounding.
    BOOST_CHECK(tl > tq * 19 / 10 && tl < tq * 21 / 10);
    BOOST_CHECK(tw > tq * 39 / 10 && tw < tq * 41 / 10);

    // A block stamped late must carry the eased bits, and the quick bits are refused on it.
    CValidationState state;
    late.nBits = bl;
    late.nVersion = 0x00620004;
    BOOST_CHECK(ContextualCheckBlockHeader(late, state, &parent, late.nTime));
    late.nBits = bq;
    BOOST_CHECK(!ContextualCheckBlockHeader(late, state, &parent, late.nTime));
    BOOST_CHECK_EQUAL(state.GetRejectReason(), "bad-diffbits");
}

BOOST_AUTO_TEST_CASE(easing_is_off_for_the_plain_profiles)
{
    TimingArgs args("scaled-drift");
    SelectParams(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(Params().GetConsensus(145001).nEasingTau, 0);
}

BOOST_AUTO_TEST_CASE(easing_is_off_on_testnet)
{
    SelectParams(CBaseChainParams::TESTNET);
    BOOST_CHECK_EQUAL(Params().GetConsensus(400000).nEasingTau, 0);
    SelectParams(CBaseChainParams::REGTEST);
}

BOOST_AUTO_TEST_CASE(mainnet_easing_starts_at_its_height_and_changes_nothing_else)
{
    SelectParams(CBaseChainParams::MAIN);
    BOOST_CHECK_EQUAL(MAINNET_EASING_HEIGHT, 600000);
    BOOST_CHECK_EQUAL(MAINNET_EASING_TAU, 30);

    const Consensus::Params& before = Params().GetConsensus(MAINNET_EASING_HEIGHT - 1);
    const Consensus::Params& after = Params().GetConsensus(MAINNET_EASING_HEIGHT);
    BOOST_CHECK_EQUAL(before.nEasingTau, 0);
    BOOST_CHECK_EQUAL(before.nEasingFreeWait, 0);
    BOOST_CHECK_EQUAL(after.nEasingTau, 30);
    BOOST_CHECK_EQUAL(after.nEasingFreeWait, 36);
    BOOST_CHECK_EQUAL(Params().GetConsensus(MAINNET_EASING_HEIGHT + 1000000).nEasingTau, 30);

    // The row inherits the 371337 row: LWMA, floor, drift cap and AuxPoW rules are unchanged.
    BOOST_CHECK(after.fLWMADifficultyCalculation);
    BOOST_CHECK_EQUAL(after.nMinBlockSpacing, before.nMinBlockSpacing);
    BOOST_CHECK_EQUAL(after.nMaxFutureDrift, before.nMaxFutureDrift);
    BOOST_CHECK_EQUAL(after.nAuxpowChainId, before.nAuxpowChainId);
    BOOST_CHECK_EQUAL(after.fAllowLegacyBlocks, before.fAllowLegacyBlocks);
    BOOST_CHECK_EQUAL(after.nEveryBlockRetargetHeight, before.nEveryBlockRetargetHeight);
    BOOST_CHECK(after.powLimit == before.powLimit);
    SelectParams(CBaseChainParams::REGTEST);
}

BOOST_AUTO_TEST_SUITE_END()
