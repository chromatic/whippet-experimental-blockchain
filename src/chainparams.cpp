// Copyright (c) 2010 Satoshi Nakamoto
// Copyright (c) 2009-2016 The Bitcoin Core developers
// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "chainparams.h"
#include "consensus/consensus.h"
#include "consensus/merkle.h"

#include "tinyformat.h"
#include "util.h"
#include "utilstrencodings.h"

#include <assert.h>

#include <boost/assign/list_of.hpp>

#include "chainparamsseeds.h"

static CBlock CreateGenesisBlock(const char* pszTimestamp, const CScript& genesisOutputScript, uint32_t nTime, uint32_t nNonce, uint32_t nBits, int32_t nVersion, const CAmount& genesisReward)
{
    CMutableTransaction txNew;
    txNew.nVersion = 1;
    txNew.vin.resize(1);
    txNew.vout.resize(1);
    txNew.vin[0].scriptSig = CScript() << 486604799 << CScriptNum(4) << std::vector<unsigned char>((const unsigned char*)pszTimestamp, (const unsigned char*)pszTimestamp + strlen(pszTimestamp));
    txNew.vout[0].nValue = genesisReward;
    txNew.vout[0].scriptPubKey = genesisOutputScript;

    CBlock genesis;
    genesis.nTime    = nTime;
    genesis.nBits    = nBits;
    genesis.nNonce   = nNonce;
    genesis.nVersion = nVersion;
    genesis.vtx.push_back(MakeTransactionRef(std::move(txNew)));
    genesis.hashPrevBlock.SetNull();
    genesis.hashMerkleRoot = BlockMerkleRoot(genesis);
    return genesis;
}

/**
 * Build the genesis block. Note that the output of its generation
 * transaction cannot be spent since it did not originally exist in the
 * database.
 *
 * CBlock(hash=000000000019d6, ver=1, hashPrevBlock=00000000000000, hashMerkleRoot=4a5e1e, nTime=1386325540, nBits=0x1e0ffff0, nNonce=99943, vtx=1)
 *   CTransaction(hash=4a5e1e, ver=1, vin.size=1, vout.size=1, nLockTime=0)
 *     CTxIn(COutPoint(000000, -1), coinbase 04ffff001d0104455468652054696d65732030332f4a616e2f32303039204368616e63656c6c6f72206f6e206272696e6b206f66207365636f6e64206261696c6f757420666f722062616e6b73)
 *     CTxOut(nValue=50.00000000, scriptPubKey=0x5F1DF16B2B704C8A578D0B)
 *   vMerkleTree: 4a5e1e
 */
static CBlock CreateGenesisBlock(uint32_t nTime, uint32_t nNonce, uint32_t nBits, int32_t nVersion, const CAmount& genesisReward)
{
    const char* pszTimestamp = "YouMustWhippet";
    const CScript genesisOutputScript = CScript() << ParseHex("040184710fa689ad5023690c80f3a49c8f13f8d45b8c857fbcbc8bc4a8e4d3eb4b10f4d4604fa08dce601aaf0f470216fe1b51850b4acf21b179c45070ac7b03a9") << OP_CHECKSIG;
    return CreateGenesisBlock(pszTimestamp, genesisOutputScript, nTime, nNonce, nBits, nVersion, genesisReward);
}

/**
 * Main network
 */
/**
 * What makes a good checkpoint block?
 * + Is surrounded by blocks with reasonable timestamps
 *   (no blocks before with a timestamp after, none after with
 *    timestamp before)
 * + Contains no strange transactions
 */

class CMainParams : public CChainParams {
private:
    Consensus::Params digishieldConsensus;
    Consensus::Params chainIdFixConsensus;
    Consensus::Params minSpacingConsensus;
    Consensus::Params auxpowConsensus;
    Consensus::Params easingConsensus;
public:
    CMainParams() {
        strNetworkID = "main";

        // Blocks 0 - 144999 are conventional difficulty calculation
        consensus.nSubsidyHalvingInterval = 1000000; // 10x longer to match 10x faster blocks
        consensus.nMajorityEnforceBlockUpgrade = 1500;
        consensus.nMajorityRejectBlockOutdated = 1900;
        consensus.nMajorityWindow = 2000;
        // BIP34 is never enforced in Whippet v2 blocks, so we enforce from v3
        consensus.BIP34Height = 1034383;
        consensus.BIP34Hash = uint256S("0x80d1364201e5df97e696c03bdd24dc885e8617b9de51e453c10a4f629b1e797a");
        consensus.BIP65Height = 3464751; // 34cd2cbba4ba366f47e5aa0db5f02c19eba2adf679ceb6653ac003bdc9a0ef1f - first v4 block after the last v3 block
        consensus.BIP66Height = 1034383; // 80d1364201e5df97e696c03bdd24dc885e8617b9de51e453c10a4f629b1e797a - this is the last block that could be v2, 1900 blocks past the last v2 block
        consensus.UAPMintHeight = 82000; // OP_MINT/OP_MINT_TRANSFER activation (v2 origin covenant); tip was 79991 when this was set
        consensus.powLimit = uint256S("0x0000fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); // Allow easier initial mining
        consensus.nPowTargetTimespan = 4 * 60 * 60; // pre-digishield: 4 hours
        consensus.nPowTargetSpacing = 6; // 6 seconds
        consensus.nMinBlockSpacing = MIN_CONSENSUS_BLOCK_SPACING;
        consensus.nMaxFutureDrift = 2 * 60 * 60;
        consensus.fDigishieldDifficultyCalculation = false;
        consensus.fLWMADifficultyCalculation = false;
        consensus.nCoinbaseMaturity = 30;
        consensus.fPowAllowMinDifficultyBlocks = false;
        consensus.fPowAllowDigishieldMinDifficultyBlocks = false;
        consensus.fPowNoRetargeting = false;
        consensus.nRuleChangeActivationThreshold = 9576; // 95% of 10,080
        consensus.nMinerConfirmationWindow = 10080; // 60 * 24 * 7 = 10,080 blocks, or one week
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].bit = 28;
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nStartTime = 1199145601; // January 1, 2008
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nTimeout = 1230767999; // December 31, 2008

        // Deployment of BIP68, BIP112, and BIP113.
        // XXX: BIP heights and hashes all need to be updated to Whippet values
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].bit = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nStartTime = 1462060800; // May 1st, 2016
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nTimeout = 1493596800; // May 1st, 2017

        // Deployment of SegWit (BIP141, BIP143, and BIP147)
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].bit = 1;
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nStartTime = 1479168000; // November 15th, 2016.
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nTimeout = 0; // Disabled

        // The best chain should have at least this much work.
        // Use zero for a new chain so nodes exit IBD and relay blocks immediately.
        consensus.nMinimumChainWork = uint256S("0x0");

        // By default assume that the signatures in ancestors of this block are valid.
        // Block 79000, one block above the last stretch of contested history (a
        // run of one-block stale tips around 78920-78925) and just under the tip
        // at the time this was set.
        consensus.defaultAssumeValid = uint256S("0x947d56182dde4d07af5e0ddeb880ae77d8ab770a2693ee62a610390cea5f5e1a");

        // AuxPoW parameters
        consensus.nAuxpowChainId = 0x0062; // 98 - Josh Wise!
        consensus.fStrictChainId = true;
        consensus.fAllowLegacyBlocks = true;
        consensus.nHeightEffective = 0;

        // Blocks 240+ are LWMA (after accumulating enough history)
        digishieldConsensus = consensus;
        digishieldConsensus.nHeightEffective = 240;
        digishieldConsensus.fSimplifiedRewards = true;
        digishieldConsensus.fDigishieldDifficultyCalculation = false;
        digishieldConsensus.fLWMADifficultyCalculation = true;
        digishieldConsensus.nPowTargetTimespan = 60; // LWMA: 1 minute (10 blocks at 6 sec)
        digishieldConsensus.nCoinbaseMaturity = 240;

        // Blocks 80000+ use a distinct AuxPoW chain ID; 0x0062 collides with
        // Dogecoin's registered merge-mining chain ID, which would let
        // Dogecoin-merge-mining pools produce blocks that appear valid on Whippet.
        chainIdFixConsensus = digishieldConsensus;
        chainIdFixConsensus.nHeightEffective = 80000;
        chainIdFixConsensus.nAuxpowChainId = 0x5750; // "WP" - distinct from Dogecoin (0x0062)

        // Blocks 100000+: lower the minimum block spacing floor from 6s
        // (equal to nPowTargetSpacing) to 4s. With the floor equal to the
        // target, generateBlocks()/submitblock() timestamps could never land
        // under target, so the LWMA retarget algorithm never saw a "too fast"
        // block and difficulty could only fall to powLimit and stay there,
        // regardless of real hashrate. A 4s floor leaves the algorithm real
        // headroom below the 6s target to respond to hashrate changes.
        minSpacingConsensus = chainIdFixConsensus;
        minSpacingConsensus.nHeightEffective = 100000;
        minSpacingConsensus.nMinBlockSpacing = 4;

        // Blocks 371337+ are AuxPoW with LWMA. The same height carries the soft fork: from
        // MAINNET_DRIFT_CAP_HEIGHT a block may be at most 72s (12 block times) ahead of the node's
        // adjusted time instead of 2 hours.
        static_assert(MAINNET_DRIFT_CAP_HEIGHT == 371337, "the drift cap shares the 371337 row");
        auxpowConsensus = minSpacingConsensus;
        auxpowConsensus.nMaxFutureDrift = MAX_FUTURE_DRIFT_CAPPED;
        auxpowConsensus.nHeightEffective = 371337;
        auxpowConsensus.fAllowLegacyBlocks = false;
        auxpowConsensus.fLWMADifficultyCalculation = true;

        // Hard fork: from MAINNET_EASING_HEIGHT LWMA eases the target while the chain waits for a block
        // (see nEasingTau in consensus/params.h). Nothing else changes in this row.
        static_assert(MAINNET_EASING_HEIGHT > 371337, "the easing row must sit above the 371337 row");
        easingConsensus = auxpowConsensus;
        easingConsensus.nHeightEffective = MAINNET_EASING_HEIGHT;
        easingConsensus.nEasingFreeWait = 6 * easingConsensus.nPowTargetSpacing;
        easingConsensus.nEasingTau = MAINNET_EASING_TAU;

        // Assemble the binary search tree of consensus parameters
        pConsensusRoot = &digishieldConsensus;
        digishieldConsensus.pLeft = &consensus;
        digishieldConsensus.pRight = &chainIdFixConsensus;
        chainIdFixConsensus.pRight = &minSpacingConsensus;
        minSpacingConsensus.pRight = &auxpowConsensus;
        auxpowConsensus.pRight = &easingConsensus;

        /**
         * The message start string is designed to be unlikely to occur in normal data.
         * The characters are rarely used upper ASCII, not valid as UTF-8, and produce
         * a large 32-bit integer with any alignment.
         */
        pchMessageStart[0] = 0xcc;
        pchMessageStart[1] = 0xbb;
        pchMessageStart[2] = 0xaa;
        pchMessageStart[3] = 0xdd;
        nDefaultPort = 33666;
        nPruneAfterHeight = 100000;

        genesis = CreateGenesisBlock(1767254400, 2684381204, 0x1f00ffff, 1, 76 * COIN);

        consensus.hashGenesisBlock = genesis.GetHash();
        digishieldConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        chainIdFixConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        minSpacingConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        auxpowConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        easingConsensus.hashGenesisBlock = consensus.hashGenesisBlock;

        assert(consensus.hashGenesisBlock == uint256S("0x863714e1c1a15526a0081a35097c56e28dbd43ef97f76fec0ccd5a7510326292"));
        assert(genesis.hashMerkleRoot == uint256S("0x7bef007a121450dc80c102774fe3d97803761b75820cb7af56344868897c916e"));

        // Note that of those with the service bits flag, most only support a subset of possible options
        vSeeds.push_back(CDNSSeedData("ifdogethenwow.com", "whippet-seed.ifdogethenwow.com", true));

        base58Prefixes[PUBKEY_ADDRESS] = std::vector<unsigned char>(1, 73);
        base58Prefixes[SCRIPT_ADDRESS] = std::vector<unsigned char>(1,22);
        base58Prefixes[SECRET_KEY] =     std::vector<unsigned char>(1,158);
        base58Prefixes[EXT_PUBLIC_KEY] = boost::assign::list_of(0x02)(0xfa)(0xca)(0xfd).convert_to_container<std::vector<unsigned char> >();
        base58Prefixes[EXT_SECRET_KEY] = boost::assign::list_of(0x02)(0xfa)(0xc3)(0x98).convert_to_container<std::vector<unsigned char> >();

        vFixedSeeds = std::vector<SeedSpec6>(pnSeed6_main, pnSeed6_main + ARRAYLEN(pnSeed6_main));

        fMiningRequiresPeers = true;
        fDefaultConsistencyChecks = false;
        fRequireStandard = true;
        fMineBlocksOnDemand = false;

        // 78000 sits below the run of one-block stale tips around 78920-78925,
        // the only contested history the chain has, and below the UAPMintHeight
        // activation at 82000 -- so it fixes the undisputed part and nothing
        // else. On a chain this lightly mined a checkpoint is standing in for
        // accumulated work that does not exist yet.
        checkpointData = (CCheckpointData) {
            boost::assign::map_list_of
            (     0, uint256S("e7c8ca400e600680341f25c6a26814101366201a598132ba1d32116f5c5a0d35"))
            ( 78000, uint256S("0x82230c60ae78e053bee0345b31c73a8e68f1129de06d133d79f144be9e0dca37"))
        };

        chainTxData = ChainTxData{
            // Data as of block c000c2bc5c9cc8842bb1eb6a2589b0cfb2b39b87319a302eb1c12f9b12dd2238 (height 79991).
            //
            // The transaction count is derived rather than read directly: tx= is
            // only logged by UpdateTip(), and with nothing mining, the chain has
            // connected no block since this one. Inverting the progress figure
            // that LoadBlockIndexDB reported against the previous (Dogecoin)
            // constants gives 167615, good to about +/- 300 given that progress
            // prints to six decimals. That is 2.10 transactions per block, which
            // is a coinbase plus roughly one spend.
            1786301508, // * UNIX timestamp of last checkpoint block
            167615,     // * total number of transactions between genesis and last checkpoint
                        //   (the tx=... number in the SetBestChain debug.log lines)
            0.0088      // * estimated number of transactions per second after checkpoint
                        // 167615 / 19047108 seconds since genesis = 0.0088
        };
    }
};
static CMainParams mainParams;

/**
 * Testnet (v3)
 */
class CTestNetParams : public CChainParams {
private:
    Consensus::Params digishieldConsensus;
    Consensus::Params chainIdFixConsensus;
    Consensus::Params minSpacingConsensus;
    Consensus::Params auxpowConsensus;
    Consensus::Params minDifficultyConsensus;
public:
    CTestNetParams() {
        strNetworkID = "test";

        // Blocks 0 - 144999 are pre-Digishield
        consensus.nHeightEffective = 0;
        consensus.nPowTargetTimespan = 4 * 60 * 60; // pre-digishield: 4 hours
        consensus.fDigishieldDifficultyCalculation = false;
        consensus.fLWMADifficultyCalculation = false;
        consensus.nCoinbaseMaturity = 30;
        consensus.fPowAllowMinDifficultyBlocks = true;
        consensus.fPowAllowDigishieldMinDifficultyBlocks = false;
        consensus.nSubsidyHalvingInterval = 1000000; // 10x longer to match 10x faster blocks
        consensus.nMajorityEnforceBlockUpgrade = 501;
        consensus.nMajorityRejectBlockOutdated = 750;
        consensus.nMajorityWindow = 1000;
        // BIP34 is never enforced in Whippet v2 blocks, so we enforce from v3
        // Testnet is a fresh chain whose miners only produce current-version
        // blocks, so BIP34/65/66 are enforced from the first block after genesis.
        consensus.BIP34Height = 1;
        consensus.BIP34Hash = uint256();
        consensus.BIP65Height = 1;
        consensus.BIP66Height = 1;
        consensus.UAPMintHeight = 82000; // OP_MINT/OP_MINT_TRANSFER activation (v2 origin covenant); tracks mainnet deliberately -- see chainparams_tests
        consensus.powLimit = uint256S("0x0000fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); // Allow easier initial mining; must admit the genesis nBits
        consensus.nPowTargetTimespan = 4 * 60 * 60; // pre-digishield: 4 hours
        consensus.nPowTargetSpacing = 6; // 6 seconds
        // Testnet is a fresh chain, so it starts with the rules mainnet reaches at height 100000 and at
        // the soft fork: a floor below the 6s target (else LWMA never sees a fast block and cannot
        // raise difficulty) and blocks at most 72s ahead of the adjusted time.
        consensus.nMinBlockSpacing = 4;
        consensus.nMaxFutureDrift = MAX_FUTURE_DRIFT_CAPPED;
        consensus.fPowNoRetargeting = false;
        consensus.nRuleChangeActivationThreshold = 2880; // 2 days (note this is significantly lower than Bitcoin standard)
        consensus.nMinerConfirmationWindow = 10080; // 60 * 24 * 7 = 10,080 blocks, or one week
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].bit = 28;
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nStartTime = 1199145601; // January 1, 2008
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nTimeout = 1230767999; // December 31, 2008

        // Deployment of BIP68, BIP112, and BIP113.
        // XXX: BIP heights and hashes all need to be updated to Whippet values
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].bit = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nStartTime = 1456790400; // March 1st, 2016
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nTimeout = 1493596800; // May 1st, 2017

        // Deployment of SegWit (BIP141, BIP143, and BIP147)
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].bit = 1;
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nStartTime = 1462060800; // May 1st 2016
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nTimeout = 0; // Disabled

        // The best chain should have at least this much work.
        // Use zero for a new chain so nodes exit IBD and relay blocks immediately.
        consensus.nMinimumChainWork = uint256S("0x0");

        // Verify every signature on a new chain.
        consensus.defaultAssumeValid = uint256S("0x00");

        // AuxPoW parameters
        consensus.nAuxpowChainId = 0x0062; // 98 - Josh Wise!
        consensus.fStrictChainId = false;
        consensus.nHeightEffective = 0;
        consensus.fAllowLegacyBlocks = true;

        // Blocks 240+ are LWMA (after accumulating enough history)
        digishieldConsensus = consensus;
        digishieldConsensus.nHeightEffective = 240;
        digishieldConsensus.nPowTargetTimespan = 60; // LWMA: 1 minute (10 blocks at 6 sec)
        digishieldConsensus.fDigishieldDifficultyCalculation = false;
        digishieldConsensus.fLWMADifficultyCalculation = true;
        digishieldConsensus.fSimplifiedRewards = true;
        digishieldConsensus.fPowAllowMinDifficultyBlocks = false;
        digishieldConsensus.nCoinbaseMaturity = 240;

        // Blocks 80000+ use a distinct AuxPoW chain ID; 0x0062 collides with
        // Dogecoin's registered merge-mining chain ID (see mainnet for details).
        chainIdFixConsensus = digishieldConsensus;
        chainIdFixConsensus.nHeightEffective = 80000;
        chainIdFixConsensus.nAuxpowChainId = 0x5750; // "WP" - distinct from Dogecoin (0x0062)

        // Blocks 100000+: lower the minimum block spacing floor from 6s to
        // 4s, tracking mainnet deliberately (see mainnet for details).
        minSpacingConsensus = chainIdFixConsensus;
        minSpacingConsensus.nHeightEffective = 100000;
        minSpacingConsensus.nMinBlockSpacing = 4;

        // Blocks 157500 - 158099 are Digishield with minimum difficulty on all blocks
        minDifficultyConsensus = minSpacingConsensus;
        minDifficultyConsensus.nHeightEffective = 157500;
        minDifficultyConsensus.fPowAllowDigishieldMinDifficultyBlocks = true;
        minDifficultyConsensus.fPowAllowMinDifficultyBlocks = true;

        // Enable AuxPoW at 158100
        auxpowConsensus = minDifficultyConsensus;
        auxpowConsensus.nHeightEffective = 158100;
        auxpowConsensus.fPowAllowDigishieldMinDifficultyBlocks = true;
        auxpowConsensus.fAllowLegacyBlocks = false;

        // Assemble the binary search tree of parameters
        pConsensusRoot = &digishieldConsensus;
        digishieldConsensus.pLeft = &consensus;
        digishieldConsensus.pRight = &chainIdFixConsensus;
        chainIdFixConsensus.pRight = &minSpacingConsensus;
        minSpacingConsensus.pRight = &minDifficultyConsensus;
        minDifficultyConsensus.pRight = &auxpowConsensus;

        pchMessageStart[0] = 0xfc;
        pchMessageStart[1] = 0xc1;
        pchMessageStart[2] = 0xb7;
        pchMessageStart[3] = 0xdc;
        nDefaultPort = 43666;
        nPruneAfterHeight = 1000;

        genesis = CreateGenesisBlock("WhippetTestnet", CScript() << ParseHex("040184710fa689ad5023690c80f3a49c8f13f8d45b8c857fbcbc8bc4a8e4d3eb4b10f4d4604fa08dce601aaf0f470216fe1b51850b4acf21b179c45070ac7b03a9") << OP_CHECKSIG,
                                     1792886400, 48974, 0x1f00ffff, 1, 76 * COIN);
        consensus.hashGenesisBlock = genesis.GetHash();
        digishieldConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        chainIdFixConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        minSpacingConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        minDifficultyConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        auxpowConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        assert(consensus.hashGenesisBlock == uint256S("0xcf82096be3651f0f0381729e8a49b7643185b1fbabcacf45c3e1f5256fc394a2"));
        assert(genesis.hashMerkleRoot == uint256S("0xbdad58d4b6e454c5dc28f8df0840afe3a50dc8d922b6f3bfd485c9520ed0dfc9"));

        vSeeds.clear();
        // No testnet DNS seed yet; connect with -addnode/-connect.

        base58Prefixes[PUBKEY_ADDRESS] = std::vector<unsigned char>(1, 113); // Testnet addresses start with 'n'
        base58Prefixes[SCRIPT_ADDRESS] = std::vector<unsigned char>(1,196); // 0xc4
        base58Prefixes[SECRET_KEY] =     std::vector<unsigned char>(1,241); // 0xf1
        base58Prefixes[EXT_PUBLIC_KEY] = boost::assign::list_of(0x04)(0x35)(0x87)(0xcf).convert_to_container<std::vector<unsigned char> >();
        base58Prefixes[EXT_SECRET_KEY] = boost::assign::list_of(0x04)(0x35)(0x83)(0x94).convert_to_container<std::vector<unsigned char> >();

        vFixedSeeds.clear(); // no testnet fixed seeds yet

        fMiningRequiresPeers = true;
        fDefaultConsistencyChecks = false;
        fRequireStandard = false;
        fMineBlocksOnDemand = false;

        checkpointData = (CCheckpointData) {
            boost::assign::map_list_of
            ( 0, uint256S("0xcf82096be3651f0f0381729e8a49b7643185b1fbabcacf45c3e1f5256fc394a2"))
        };

        chainTxData = ChainTxData{
            // Fresh chain: no history beyond genesis.
            1792886400, // * UNIX timestamp of genesis
            0,          // * total number of transactions between genesis and last checkpoint
            0.0         // * estimated number of transactions per second after that timestamp
        };

    }
};
static CTestNetParams testNetParams;

/**
 * Regression test
 */
class CRegTestParams : public CChainParams {
private:
    Consensus::Params digishieldConsensus;
    Consensus::Params auxpowConsensus;
    Consensus::Params chainIdFixConsensus;
    // Parameter sets as constructed, saved when a timing profile is first applied.
    Consensus::Params pristine[4];
    bool fTimingProfileApplied = false;
    Consensus::Params driftCapConsensus; //!< extra row for -timingdriftcapheight
    Consensus::Params easingConsensus; //!< extra row for -timingeasingheight
public:
    CRegTestParams() {
        strNetworkID = "regtest";
        consensus.nSubsidyHalvingInterval = 150;
        consensus.nMajorityEnforceBlockUpgrade = 750;
        consensus.nMajorityRejectBlockOutdated = 950;
        consensus.nMajorityWindow = 1000;
        consensus.BIP34Height = 100000000; // BIP34 has not activated on regtest (far in the future so block v1 are not rejected in tests)
        consensus.BIP34Hash = uint256();
        consensus.BIP65Height = 1351; // BIP65 activated on regtest (Used in rpc activation tests)
        consensus.BIP66Height = 1251; // BIP66 activated on regtest (Used in rpc activation tests)
        consensus.UAPMintHeight = 0; // active from genesis on regtest, for testability
        consensus.powLimit = uint256S("0x7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); // ~uint256(0) >> 1;
        consensus.nPowTargetTimespan = 4 * 60 * 60; // pre-digishield: 4 hours
        consensus.nPowTargetSpacing = 1; // regtest: 1 second blocks
        consensus.nMinBlockSpacing = 0; // Disable timestamp spacing rule for automated QA/regtest
        consensus.nMaxFutureDrift = 2 * 60 * 60;
        consensus.fDigishieldDifficultyCalculation = false;
        consensus.fLWMADifficultyCalculation = false;
        consensus.fPowAllowMinDifficultyBlocks = true;
        consensus.fPowNoRetargeting = true;
        consensus.nRuleChangeActivationThreshold = 540; // 75% for testchains
        consensus.nMinerConfirmationWindow = 720; // Faster than normal for regtest (2,520 instead of 10,080)
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].bit = 28;
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nStartTime = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_TESTDUMMY].nTimeout = 999999999999ULL;
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].bit = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nStartTime = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_CSV].nTimeout = 999999999999ULL;
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].bit = 1;
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nStartTime = 0;
        consensus.vDeployments[Consensus::DEPLOYMENT_SEGWIT].nTimeout = 999999999999ULL;

        // The best chain should have at least this much work.
        consensus.nMinimumChainWork = uint256S("0x00");

        // By default assume that the signatures in ancestors of this block are valid.
        consensus.defaultAssumeValid = uint256S("0x00");

        // AuxPow parameters
        consensus.nAuxpowChainId = 0x0062; // 98 - Josh Wise!
        consensus.fStrictChainId = true;
        consensus.fAllowLegacyBlocks = true;

        // Whippet parameters
        consensus.fSimplifiedRewards = true;
        consensus.nCoinbaseMaturity = 60; // For easier testability in RPC tests

        digishieldConsensus = consensus;
        digishieldConsensus.nHeightEffective = 10;
        digishieldConsensus.nPowTargetTimespan = 1; // regtest: also retarget every second in digishield mode, for conformity
        digishieldConsensus.fDigishieldDifficultyCalculation = true;

        auxpowConsensus = digishieldConsensus;
        auxpowConsensus.fAllowLegacyBlocks = false;
        auxpowConsensus.nHeightEffective = 20;

        // Blocks 80000+ use a distinct AuxPoW chain ID; 0x0062 collides with
        // Dogecoin's registered merge-mining chain ID (see mainnet for details).
        chainIdFixConsensus = auxpowConsensus;
        chainIdFixConsensus.nHeightEffective = 80000;
        chainIdFixConsensus.nAuxpowChainId = 0x5750; // "WP" - distinct from Dogecoin (0x0062)

        // Assemble the binary search tree of parameters
        digishieldConsensus.pLeft = &consensus;
        digishieldConsensus.pRight = &auxpowConsensus;
        auxpowConsensus.pRight = &chainIdFixConsensus;
        pConsensusRoot = &digishieldConsensus;

        pchMessageStart[0] = 0xfa;
        pchMessageStart[1] = 0xbf;
        pchMessageStart[2] = 0xb5;
        pchMessageStart[3] = 0xda;
        nDefaultPort = 53666;
        nPruneAfterHeight = 1000;

        genesis = CreateGenesisBlock(1767254400, 1, 0x207fffff, 1, 76 * COIN);
        consensus.hashGenesisBlock = genesis.GetHash();
        digishieldConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        auxpowConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        chainIdFixConsensus.hashGenesisBlock = consensus.hashGenesisBlock;
        // Regtest genesis hash: 2fd0127392635992184580433b3fc43eeaa5e8a40220d751ba39b742e9488ce0

        vFixedSeeds.clear(); //!< Regtest mode doesn't have any fixed seeds.
        vSeeds.clear();      //!< Regtest mode doesn't have any DNS seeds.

        fMiningRequiresPeers = false;
        fDefaultConsistencyChecks = true;
        fRequireStandard = false;
        fMineBlocksOnDemand = true;

        checkpointData = (CCheckpointData){
            boost::assign::map_list_of
            ( 0, genesis.GetHash())  // Use computed genesis hash
        };

        chainTxData = ChainTxData{
            0,
            0,
            0
        };

        base58Prefixes[PUBKEY_ADDRESS] = std::vector<unsigned char>(1, 73);
        base58Prefixes[SCRIPT_ADDRESS] = std::vector<unsigned char>(1,196);  // 0xc4
        base58Prefixes[SECRET_KEY] =     std::vector<unsigned char>(1,239);  // 0xef
        base58Prefixes[EXT_PUBLIC_KEY] = boost::assign::list_of(0x04)(0x35)(0x87)(0xCF).convert_to_container<std::vector<unsigned char> >();
        base58Prefixes[EXT_SECRET_KEY] = boost::assign::list_of(0x04)(0x35)(0x83)(0x94).convert_to_container<std::vector<unsigned char> >();
    }

    void UpdateBIP9Parameters(Consensus::DeploymentPos d, int64_t nStartTime, int64_t nTimeout)
    {
        consensus.vDeployments[d].nStartTime = nStartTime;
        consensus.vDeployments[d].nTimeout = nTimeout;
    }

    void UpdateUAPMintHeight(int nHeight)
    {
        consensus.UAPMintHeight = nHeight;
    }

    void UpdateChainIdFixHeight(int nHeight)
    {
        chainIdFixConsensus.nHeightEffective = nHeight;
    }

    /**
     * Rehearse the mainnet soft fork on regtest: from nHeight on, blocks may be at most
     * MAX_FUTURE_DRIFT_CAPPED ahead of adjusted time, as in the 371337 row of mainnet.
     * Nothing else changes. 0 removes the row. Call it before ApplyTimingProfile and again after.
     * @throws std::runtime_error unless 20 < nHeight < the chain id fix height.
     */
    void ApplyDriftCapHeight(int nHeight)
    {
        // Undo an earlier call: the chain id fix row inherits the drift of the row before it.
        auxpowConsensus.pRight = &chainIdFixConsensus;
        chainIdFixConsensus.nMaxFutureDrift = auxpowConsensus.nMaxFutureDrift;
        if (nHeight == 0)
            return;
        if (nHeight <= auxpowConsensus.nHeightEffective || nHeight >= chainIdFixConsensus.nHeightEffective)
            throw std::runtime_error(strprintf("%s: -timingdriftcapheight must lie between %d and %d.", __func__,
                                               auxpowConsensus.nHeightEffective, chainIdFixConsensus.nHeightEffective));
        driftCapConsensus = auxpowConsensus;
        driftCapConsensus.nHeightEffective = nHeight;
        driftCapConsensus.nMaxFutureDrift = MAX_FUTURE_DRIFT_CAPPED;
        driftCapConsensus.pLeft = NULL;
        driftCapConsensus.pRight = &chainIdFixConsensus;
        auxpowConsensus.pRight = &driftCapConsensus;
        chainIdFixConsensus.nMaxFutureDrift = MAX_FUTURE_DRIFT_CAPPED;
    }

    /**
     * Rehearse the mainnet easing hard fork on regtest: from nHeight on, LWMA eases the target of a block
     * stamped more than 6 block times after its parent, with the mainnet tau (MAINNET_EASING_TAU).
     * Nothing else changes, so run it with -timingprofile=scaled-drift-easing -timingeasingtau=0 (every-block
     * LWMA, easing off) to compare with a node that does not know the row. 0 removes the row.
     * Call it after ApplyDriftCapHeight and ApplyTimingProfile.
     * @throws std::runtime_error unless the row lies between the last earlier row and the chain id fix row.
     */
    void ApplyEasingHeight(int nHeight)
    {
        // Undo an earlier call: the row before the easing row points at the chain id fix row again.
        Consensus::Params* prev = auxpowConsensus.pRight == &driftCapConsensus ? &driftCapConsensus : &auxpowConsensus;
        prev->pRight = &chainIdFixConsensus;
        if (nHeight == 0)
            return;
        if (nHeight <= prev->nHeightEffective || nHeight >= chainIdFixConsensus.nHeightEffective)
            throw std::runtime_error(strprintf("%s: -timingeasingheight must lie between %d and %d.", __func__,
                                               prev->nHeightEffective, chainIdFixConsensus.nHeightEffective));
        easingConsensus = *prev;
        easingConsensus.nHeightEffective = nHeight;
        easingConsensus.nEasingFreeWait = 6 * easingConsensus.nPowTargetSpacing;
        easingConsensus.nEasingTau = MAINNET_EASING_TAU;
        easingConsensus.pLeft = NULL;
        easingConsensus.pRight = &chainIdFixConsensus;
        prev->pRight = &easingConsensus;
    }

    /**
     * Make regtest run Whippet's block timing rules (see -timingprofile).
     * An empty profile restores the parameters as constructed.
     * @throws std::runtime_error for an unknown profile.
     */
    void ApplyTimingProfile(const std::string& profile, int64_t nFloor, bool fMinDifficulty, int64_t nEasingTau)
    {
        Consensus::Params* sets[4] = {&consensus, &digishieldConsensus, &auxpowConsensus, &chainIdFixConsensus};
        if (profile != "" && profile != "whippet" && profile != "scaled-drift" && profile != "scaled-drift-easing")
            throw std::runtime_error(strprintf("%s: Unknown -timingprofile %s.", __func__, profile));
        if (fTimingProfileApplied) {
            for (int i = 0; i < 4; i++)
                *sets[i] = pristine[i];
            fTimingProfileApplied = false;
        }
        if (profile.empty())
            return;
        for (int i = 0; i < 4; i++)
            pristine[i] = *sets[i];
        fTimingProfileApplied = true;
        for (Consensus::Params* p : sets) {
            p->nPowTargetSpacing = 6;
            p->nPowTargetTimespan = 60; // LWMA window of 10 blocks, as testnet
            p->nMinBlockSpacing = nFloor;
            p->fLWMADifficultyCalculation = true;
            p->fDigishieldDifficultyCalculation = false;
            p->fPowNoRetargeting = false;
            p->fPowAllowMinDifficultyBlocks = fMinDifficulty;
            if (profile == "scaled-drift" || profile == "scaled-drift-easing")
                p->nMaxFutureDrift = 12 * p->nPowTargetSpacing;
            if (profile == "scaled-drift-easing") {
                p->nEasingFreeWait = 6 * p->nPowTargetSpacing;
                p->nEasingTau = nEasingTau;
                p->nEveryBlockRetargetHeight = 0; // regtest chains are far below 145000 and easing needs a retarget on every block
            }
        }
    }
};
static CRegTestParams regTestParams;

static CChainParams *pCurrentParams = 0;

const CChainParams &Params() {
    assert(pCurrentParams);
    return *pCurrentParams;
}

const Consensus::Params *Consensus::Params::GetConsensus(uint32_t nTargetHeight) const {
    if (nTargetHeight < this -> nHeightEffective && this -> pLeft != NULL) {
        return this -> pLeft -> GetConsensus(nTargetHeight);
    } else if (nTargetHeight > this -> nHeightEffective && this -> pRight != NULL) {
        const Consensus::Params *pCandidate = this -> pRight -> GetConsensus(nTargetHeight);
        if (pCandidate->nHeightEffective <= nTargetHeight) {
            return pCandidate;
        }
    }

    // No better match below the target height
    return this;
}

CChainParams& Params(const std::string& chain)
{
    if (chain == CBaseChainParams::MAIN)
            return mainParams;
    else if (chain == CBaseChainParams::TESTNET)
            return testNetParams;
    else if (chain == CBaseChainParams::REGTEST)
            return regTestParams;
    else
        throw std::runtime_error(strprintf("%s: Unknown chain %s.", __func__, chain));
}

void SelectParams(const std::string& network)
{
    SelectBaseParams(network);
    // -timingprofile is a regtest-only test hook.
    const std::string profile = GetArg("-timingprofile", "");
    // -timingdriftcapheight likewise: it rehearses the mainnet drift cap on regtest.
    const int driftCapHeight = GetArg("-timingdriftcapheight", 0);
    // -timingeasingheight rehearses the mainnet easing hard fork on regtest.
    const int easingHeight = GetArg("-timingeasingheight", 0);
    if (network == CBaseChainParams::REGTEST) {
        regTestParams.ApplyDriftCapHeight(0);
        regTestParams.ApplyEasingHeight(0);
        regTestParams.ApplyTimingProfile(profile, GetArg("-timingfloor", 4), GetBoolArg("-timingmindiff", false), GetArg("-timingeasingtau", 30));
        regTestParams.ApplyDriftCapHeight(driftCapHeight);
        regTestParams.ApplyEasingHeight(easingHeight);
    } else if (!profile.empty() || driftCapHeight != 0 || easingHeight != 0) {
        throw std::runtime_error("-timingprofile, -timingdriftcapheight and -timingeasingheight are only valid with -regtest.");
    }
    pCurrentParams = &Params(network);
}

void UpdateRegtestBIP9Parameters(Consensus::DeploymentPos d, int64_t nStartTime, int64_t nTimeout)
{
    regTestParams.UpdateBIP9Parameters(d, nStartTime, nTimeout);
}

void UpdateRegtestUAPMintHeight(int nHeight)
{
    regTestParams.UpdateUAPMintHeight(nHeight);
}

void UpdateRegtestChainIdFixHeight(int nHeight)
{
    regTestParams.UpdateChainIdFixHeight(nHeight);
}
