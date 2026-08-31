// Copyright (c) 2011-2016 The Bitcoin Core developers
// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "consensus/validation.h"
#include "key.h"
#include "validation.h"
#include "miner.h"
#include "pubkey.h"
#include "txmempool.h"
#include "random.h"
#include "chainparams.h"
#include "script/standard.h"
#include "test/test_bitcoin.h"
#include "utiltime.h"

#include <boost/test/unit_test.hpp>

BOOST_AUTO_TEST_SUITE(tx_validationcache_tests)

static bool
ToMemPool(CMutableTransaction& tx)
{
    LOCK(cs_main);

    CValidationState state;
    return AcceptToMemoryPool(mempool, state, MakeTransactionRef(tx), false, NULL, NULL, true, 0);
}

BOOST_FIXTURE_TEST_CASE(tx_mempool_block_doublespend, TestChain240Setup)
{
    // Make sure skipping validation of transactions that were
    // validated going into the memory pool does not allow
    // double-spends in blocks to pass validation when they should not.

    CScript scriptPubKey = CScript() <<  ToByteVector(coinbaseKey.GetPubKey()) << OP_CHECKSIG;

    // Create a double-spend of mature coinbase txn:
    std::vector<CMutableTransaction> spends;
    spends.resize(2);
    for (int i = 0; i < 2; i++)
    {
        spends[i].nVersion = 1;
        spends[i].vin.resize(1);
        spends[i].vin[0].prevout.hash = coinbaseTxns[0].GetHash();
        spends[i].vin[0].prevout.n = 0;
        spends[i].vout.resize(1);
        spends[i].vout[0].nValue = COIN;
        spends[i].vout[0].scriptPubKey = scriptPubKey;

        // Sign:
        std::vector<unsigned char> vchSig;
        uint256 hash = SignatureHash(scriptPubKey, spends[i], 0, SIGHASH_ALL, 0, SIGVERSION_BASE);
        BOOST_CHECK(coinbaseKey.Sign(hash, vchSig));
        vchSig.push_back((unsigned char)SIGHASH_ALL);
        spends[i].vin[0].scriptSig << vchSig;
    }

    CBlock block;

    // Test 1: block with both of those transactions should be rejected.
    block = CreateAndProcessBlock(spends, scriptPubKey);
    BOOST_CHECK(chainActive.Tip()->GetBlockHash() != block.GetHash());

    // Test 2: ... and should be rejected if spend1 is in the memory pool
    BOOST_CHECK(ToMemPool(spends[0]));
    block = CreateAndProcessBlock(spends, scriptPubKey);
    BOOST_CHECK(chainActive.Tip()->GetBlockHash() != block.GetHash());
    mempool.clear();

    // Test 3: ... and should be rejected if spend2 is in the memory pool
    BOOST_CHECK(ToMemPool(spends[1]));
    block = CreateAndProcessBlock(spends, scriptPubKey);
    BOOST_CHECK(chainActive.Tip()->GetBlockHash() != block.GetHash());
    mempool.clear();

    // Final sanity test: first spend in mempool, second in block, that's OK:
    std::vector<CMutableTransaction> oneSpend;
    oneSpend.push_back(spends[0]);
    BOOST_CHECK(ToMemPool(spends[1]));
    block = CreateAndProcessBlock(oneSpend, scriptPubKey);
    BOOST_CHECK(chainActive.Tip()->GetBlockHash() == block.GetHash());
    // spends[1] should have been removed from the mempool when the
    // block with spends[0] is accepted:
    BOOST_CHECK_EQUAL(mempool.size(), 0);
}

namespace {

//! Restores the regtest UAP activation height however the test exits.
struct UapMintHeightGuard {
    const int nSaved;
    UapMintHeightGuard() : nSaved(Params().GetConsensus(0).UAPMintHeight) {}
    ~UapMintHeightGuard() { UpdateRegtestUAPMintHeight(nSaved); }
};

} // namespace

// A spend of a UAP output submitted BEFORE the activation height must be
// refused by the mempool, and refused without banning the sender.
//
// What this pins is not a wrong answer but a stopped miner. Creating a
// covenant output is legal at any height -- output scripts are not executed
// -- so the output below is minable immediately. Only the SPEND runs
// OP_MINT_TRANSFER. While SCRIPT_VERIFY_UAP_MINT was a compile-time member
// of STANDARD/MANDATORY_SCRIPT_VERIFY_FLAGS, the mempool executed that
// opcode regardless of height and accepted the spend, while ConnectBlock --
// which derives the flag from the block's own height -- refused it.
// CreateNewBlock calls TestBlockValidity and throws when it fails, so one
// cheap transaction from any peer stopped block templates being produced at
// all until it was evicted.
BOOST_FIXTURE_TEST_CASE(uap_premature_spend_rejected_without_stalling_the_miner, TestChain240Setup)
{
    UapMintHeightGuard heightGuard;

    const CScript scriptPubKey = CScript() << ToByteVector(coinbaseKey.GetPubKey()) << OP_CHECKSIG;

    CKey recipient;
    recipient.MakeNewKey(true);
    const int64_t multiplier = 1000;
    // A transfer covenant carries its lineage origin. This one is spent into
    // an output of the same lineage, so any 32 bytes serve -- the origin's
    // derivation from a mint's outpoint is what uap_mint_tests covers, and is
    // beside the point here, which is purely the height gate.
    const std::vector<unsigned char> origin(32, 0x5c);
    const CScript covenant = CScript() << ToByteVector(recipient.GetPubKey()) << multiplier << origin << OP_MINT_TRANSFER;

    // Put activation comfortably beyond the tip, so the next block stays
    // pre-activation however many blocks this test mines.
    UpdateRegtestUAPMintHeight(chainActive.Height() + 100);

    // Creating the covenant output is legal pre-activation and gets mined.
    CMutableTransaction create;
    create.nVersion = 1;
    create.vin.resize(1);
    create.vin[0].prevout.hash = coinbaseTxns[0].GetHash();
    create.vin[0].prevout.n = 0;
    create.vout.resize(1);
    create.vout[0].nValue = coinbaseTxns[0].vout[0].nValue - 100000;
    create.vout[0].scriptPubKey = covenant;
    {
        std::vector<unsigned char> vchSig;
        uint256 hash = SignatureHash(scriptPubKey, create, 0, SIGHASH_ALL, 0, SIGVERSION_BASE);
        BOOST_CHECK(coinbaseKey.Sign(hash, vchSig));
        vchSig.push_back((unsigned char)SIGHASH_ALL);
        create.vin[0].scriptSig << vchSig;
    }
    BOOST_CHECK(ToMemPool(create));
    CreateAndProcessBlock(std::vector<CMutableTransaction>(1, create), scriptPubKey);
    BOOST_CHECK_EQUAL(mempool.size(), 0);

    // The spend. Every output of a covenant spend must itself be a covenant
    // with the same multiplier, so the fee is the drop in value rather than
    // a separate change output. This transaction is otherwise entirely
    // valid -- correct recipient signature, conserving covenant -- and is
    // refused only because the opcodes are not active yet.
    CMutableTransaction spend;
    spend.nVersion = 1;
    spend.vin.resize(1);
    spend.vin[0].prevout.hash = create.GetHash();
    spend.vin[0].prevout.n = 0;
    spend.vout.resize(1);
    spend.vout[0].nValue = create.vout[0].nValue - 100000;
    spend.vout[0].scriptPubKey = covenant;
    {
        uint256 hash = SignatureHash(covenant, spend, 0, SIGHASH_ALL, 0, SIGVERSION_BASE);
        std::vector<unsigned char> vchSig;
        BOOST_CHECK(recipient.Sign(hash, vchSig));
        vchSig.push_back((unsigned char)SIGHASH_ALL);
        spend.vin[0].scriptSig = CScript() << vchSig;
    }

    {
        LOCK(cs_main);
        CValidationState state;
        BOOST_CHECK(!AcceptToMemoryPool(mempool, state, MakeTransactionRef(spend), false, NULL, NULL, true, 0));
        // DoS score 0: relaying a spend that is merely early is not
        // misbehaviour, and banning for it would partition the network
        // across the activation boundary.
        int nDoS = -1;
        BOOST_CHECK(state.IsInvalid(nDoS));
        BOOST_CHECK_EQUAL(nDoS, 0);
        BOOST_CHECK_EQUAL(state.GetRejectReason(), "premature-uap-spend");
    }
    BOOST_CHECK_EQUAL(mempool.size(), 0);

    // The point of all of the above: block production still works.
    BOOST_CHECK_NO_THROW(BlockAssembler(Params()).CreateNewBlock(scriptPubKey, true));

    // And the refusal is about activation, not a permanently broken path --
    // the very same transaction is accepted once the opcodes are live.
    UpdateRegtestUAPMintHeight(0);
    BOOST_CHECK(ToMemPool(spend));
    BOOST_CHECK_EQUAL(mempool.size(), 1);
    BOOST_CHECK_NO_THROW(BlockAssembler(Params()).CreateNewBlock(scriptPubKey, true));
    mempool.clear();
}

BOOST_AUTO_TEST_SUITE_END()
