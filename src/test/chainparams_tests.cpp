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

BOOST_AUTO_TEST_SUITE_END()
