// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "chain.h"
#include "chainparams.h"
#include "consensus/validation.h"
#include "pow.h"
#include "primitives/block.h"
#include "util.h"
#include "validation.h"
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

BOOST_AUTO_TEST_CASE(future_drift_is_set_at_every_height_on_every_network)
{
    // Each height branch is a copy of a base parameter set; a field missed on the way would be garbage.
    const std::string networks[] = {CBaseChainParams::MAIN, CBaseChainParams::TESTNET, CBaseChainParams::REGTEST};
    const int heights[] = {0, 1, 10, 20, 239, 240, 79999, 80000, 99999, 100000, 145000, 157499, 157500, 158099, 158100, 1000000};
    for (const std::string& network : networks) {
        SelectParams(network);
        for (int h : heights)
            BOOST_CHECK_MESSAGE(Params().GetConsensus(h).nMaxFutureDrift == 7200, network << " height " << h);
    }
    SelectParams(CBaseChainParams::REGTEST);
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

BOOST_AUTO_TEST_SUITE_END()
