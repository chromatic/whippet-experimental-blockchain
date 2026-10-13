// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "chainparams.h"

#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>

BOOST_FIXTURE_TEST_SUITE(chainparams_tests, BasicTestingSetup)

// Dogecoin's registered merge-mining chain ID. Whippet mainnet/testnet
// started out reusing this value by mistake; a height-activated fix
// switches to a distinct ID before AuxPoW itself becomes active.
static const int32_t DOGECOIN_CHAIN_ID = 0x0062;
static const int32_t WHIPPET_CHAIN_ID = 0x5750;
static const uint32_t CHAIN_ID_FIX_HEIGHT = 80000;

BOOST_AUTO_TEST_CASE(mainnet_auxpow_chain_id_fix)
{
    const CChainParams& params = Params(CBaseChainParams::MAIN);

    BOOST_CHECK_EQUAL(params.GetConsensus(0).nAuxpowChainId, DOGECOIN_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT - 1).nAuxpowChainId, DOGECOIN_CHAIN_ID);

    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT).nAuxpowChainId, WHIPPET_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT + 1).nAuxpowChainId, WHIPPET_CHAIN_ID);

    // The fix must land well before AuxPoW itself activates, otherwise a
    // window would exist where AuxPoW blocks are valid under the old,
    // colliding chain ID.
    const uint32_t auxpowActivationHeight = 371337;
    BOOST_CHECK(CHAIN_ID_FIX_HEIGHT < auxpowActivationHeight);
    BOOST_CHECK_EQUAL(params.GetConsensus(auxpowActivationHeight).nAuxpowChainId, WHIPPET_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(auxpowActivationHeight - 1).nAuxpowChainId, WHIPPET_CHAIN_ID);
}

BOOST_AUTO_TEST_CASE(testnet_auxpow_chain_id_fix)
{
    const CChainParams& params = Params(CBaseChainParams::TESTNET);

    BOOST_CHECK_EQUAL(params.GetConsensus(0).nAuxpowChainId, DOGECOIN_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT - 1).nAuxpowChainId, DOGECOIN_CHAIN_ID);

    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT).nAuxpowChainId, WHIPPET_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT + 1).nAuxpowChainId, WHIPPET_CHAIN_ID);

    const uint32_t auxpowActivationHeight = 158100;
    BOOST_CHECK(CHAIN_ID_FIX_HEIGHT < auxpowActivationHeight);
    BOOST_CHECK_EQUAL(params.GetConsensus(auxpowActivationHeight).nAuxpowChainId, WHIPPET_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(auxpowActivationHeight - 1).nAuxpowChainId, WHIPPET_CHAIN_ID);
}

BOOST_AUTO_TEST_CASE(regtest_auxpow_chain_id_fix)
{
    const CChainParams& params = Params(CBaseChainParams::REGTEST);

    BOOST_CHECK_EQUAL(params.GetConsensus(0).nAuxpowChainId, DOGECOIN_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT - 1).nAuxpowChainId, DOGECOIN_CHAIN_ID);

    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT).nAuxpowChainId, WHIPPET_CHAIN_ID);
    BOOST_CHECK_EQUAL(params.GetConsensus(CHAIN_ID_FIX_HEIGHT + 1).nAuxpowChainId, WHIPPET_CHAIN_ID);
}

BOOST_AUTO_TEST_CASE(uap_mint_activation_height)
{
    // 82000 for the v2 origin covenant; the mainnet tip was 79991 when that
    // was set. Regtest is active from genesis for testability.
    //
    // Asserted as literals on purpose. A node running a different
    // UAPMintHeight than the miner forks at that height, so this number is
    // consensus rather than configuration -- reading it back out of Params()
    // would assert nothing at all. Changing it here should feel exactly as
    // deliberate as changing it in chainparams.cpp.
    BOOST_CHECK_EQUAL(Params(CBaseChainParams::MAIN).GetConsensus(0).UAPMintHeight, 82000);
    BOOST_CHECK_EQUAL(Params(CBaseChainParams::TESTNET).GetConsensus(0).UAPMintHeight, 82000);
    BOOST_CHECK_EQUAL(Params(CBaseChainParams::REGTEST).GetConsensus(0).UAPMintHeight, 0);

    // Mainnet and testnet are equal today, and the equality is load-bearing
    // for nothing -- they are two independent chains and either may move
    // alone. Kept in step only so there is one number to remember while
    // both are unused. Do not "simplify" this into a shared constant.
    BOOST_CHECK_EQUAL(Params(CBaseChainParams::MAIN).GetConsensus(0).UAPMintHeight,
                      Params(CBaseChainParams::TESTNET).GetConsensus(0).UAPMintHeight);
}

BOOST_AUTO_TEST_CASE(min_block_spacing_floor_activation)
{
    // Before this height, nMinBlockSpacing (6s) equals nPowTargetSpacing
    // (6s), which leaves the LWMA retarget algorithm no room to ever see a
    // block land under target, so difficulty can only fall to powLimit and
    // stay there regardless of real hashrate. From this height on, the
    // floor drops to 4s, giving the algorithm headroom below the 6s target
    // to respond to hashrate changes.
    const uint32_t MIN_SPACING_FIX_HEIGHT = 100000;
    const int64_t OLD_MIN_SPACING = 6;
    const int64_t NEW_MIN_SPACING = 4;

    for (const std::string& network : {CBaseChainParams::MAIN, CBaseChainParams::TESTNET}) {
        const CChainParams& params = Params(network);

        BOOST_CHECK_EQUAL(params.GetConsensus(0).nMinBlockSpacing, OLD_MIN_SPACING);
        BOOST_CHECK_EQUAL(params.GetConsensus(MIN_SPACING_FIX_HEIGHT - 1).nMinBlockSpacing, OLD_MIN_SPACING);

        BOOST_CHECK_EQUAL(params.GetConsensus(MIN_SPACING_FIX_HEIGHT).nMinBlockSpacing, NEW_MIN_SPACING);
        BOOST_CHECK_EQUAL(params.GetConsensus(MIN_SPACING_FIX_HEIGHT + 1).nMinBlockSpacing, NEW_MIN_SPACING);

        // Must stay below nPowTargetSpacing on both sides of the activation,
        // or the retarget algorithm loses its headroom again.
        BOOST_CHECK(params.GetConsensus(MIN_SPACING_FIX_HEIGHT).nMinBlockSpacing
                    < params.GetConsensus(MIN_SPACING_FIX_HEIGHT).nPowTargetSpacing);
    }

    // Regtest disables the floor entirely for fast tests, at every height.
    const CChainParams& regtestParams = Params(CBaseChainParams::REGTEST);
    BOOST_CHECK_EQUAL(regtestParams.GetConsensus(0).nMinBlockSpacing, 0);
    BOOST_CHECK_EQUAL(regtestParams.GetConsensus(MIN_SPACING_FIX_HEIGHT).nMinBlockSpacing, 0);
}

BOOST_AUTO_TEST_SUITE_END()
