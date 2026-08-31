// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

// Tests for CheckUapOutputCreation (validation.cpp): a UAP transfer output
// may only be created by a transaction that also spends the lineage it
// names.
//
// These are deliberately NOT in uap_mint_tests.cpp, and the distinction is
// the whole point of the rule. Every fixture there drives VerifyUapInput --
// it verifies a UAP *input*, which is the only thing that makes the script
// interpreter run at all. Two of those tests
// (rejects_output_of_a_lineage_with_no_input,
// rejects_phantom_lineage_sharing_the_multiplier) aim at exactly the attack
// below and catch it, but only because their transaction also spends a
// covenant. Remove that input and the interpreter never executes, and
// nothing in the script layer has an opinion about the outputs at all.
//
// So the cases here are the ones a script-level test cannot express: a
// transaction that creates a covenant output while spending none.

#include "coins.h"
#include "consensus/validation.h"
#include "crypto/sha256.h"
#include "key.h"
#include "primitives/transaction.h"
#include "script/interpreter.h"
#include "script/script.h"
#include "script/standard.h"
#include "uint256.h"
#include "validation.h"

#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>

#include <cstring>

typedef std::vector<unsigned char> valtype;

BOOST_FIXTURE_TEST_SUITE(uap_creation_tests, BasicTestingSetup)

namespace {

const int64_t MULTIPLIER = 1000;

COutPoint Point(unsigned char tag, uint32_t n = 0)
{
    uint256 hash;
    memset(hash.begin(), tag, 32);
    return COutPoint(hash, n);
}

// SHA256(prevout.hash || prevout.n as 4-byte little-endian), spelled out
// rather than borrowed from the interpreter for the same reason
// uap_mint_tests.cpp spells it out: this formula IS the definition of a
// lineage's identity, and a shared helper would only assert that the node
// agrees with itself.
valtype OriginOf(const COutPoint& outpoint)
{
    unsigned char buf[36];
    memcpy(buf, outpoint.hash.begin(), 32);
    for (int i = 0; i < 4; i++)
        buf[32 + i] = (unsigned char)((outpoint.n >> (8 * i)) & 0xff);
    valtype out(32);
    CSHA256().Write(buf, sizeof(buf)).Finalize(&out[0]);
    return out;
}

CScript MintScript(const CPubKey& pubkey, int64_t multiplier = MULTIPLIER)
{
    return CScript() << ToByteVector(pubkey) << multiplier << OP_MINT;
}

CScript TransferScript(const CPubKey& pubkey, const valtype& origin, int64_t multiplier = MULTIPLIER)
{
    return CScript() << ToByteVector(pubkey) << multiplier << origin << OP_MINT_TRANSFER;
}

CScript P2PKHScript(const CPubKey& pubkey)
{
    return GetScriptForDestination(pubkey.GetID());
}

// An empty coins view: every prevout a test wants has to be installed
// explicitly, so no fixture can pass by accidentally reading real state.
class CCoinsViewEmpty : public CCoinsView
{
public:
    bool GetCoins(const uint256& txid, CCoins& coins) const { return false; }
    bool HaveCoins(const uint256& txid) const { return false; }
    uint256 GetBestBlock() const { return uint256(); }
    bool BatchWrite(CCoinsMap& mapCoins, const uint256& hashBlock) { return false; }
};

// A view plus the one call under test, so each fixture reads as
// "these prevouts exist; is this transaction allowed to create these
// outputs?".
struct CreationFixture
{
    CCoinsViewEmpty base;
    CCoinsViewCache view;

    CreationFixture() : view(&base) {}

    void AddPrevout(const COutPoint& outpoint, const CScript& scriptPubKey, CAmount nValue)
    {
        CCoinsModifier coins = view.ModifyCoins(outpoint.hash);
        if (coins->vout.size() <= outpoint.n)
            coins->vout.resize(outpoint.n + 1);
        coins->vout[outpoint.n].scriptPubKey = scriptPubKey;
        coins->vout[outpoint.n].nValue = nValue;
        coins->nHeight = 1;
    }

    bool Check(const CMutableTransaction& tx, std::string& reason)
    {
        CValidationState state;
        const bool ok = CheckUapOutputCreation(CTransaction(tx), view, state);
        reason = state.GetRejectReason();
        return ok;
    }
};

} // namespace

// THE ATTACK. A transaction funded entirely by ordinary P2PKH inputs creates
// a covenant output naming somebody else's lineage. No UAP input means the
// interpreter never runs, so conservation never sees this transaction; the
// output that lands on chain is byte-identical to a genuine position of that
// token and inflates its supply for the price of one transaction.
BOOST_AUTO_TEST_CASE(rejects_a_counterfeit_position_created_from_plain_inputs)
{
    CKey victim, attacker;
    victim.MakeNewKey(true);
    attacker.MakeNewKey(true);

    // A real lineage, minted by someone else. Its origin is public: it is
    // written in the scriptPubKey of every position of that token.
    const valtype victimLineage = OriginOf(Point(0x11));

    CreationFixture f;
    const COutPoint funding = Point(0x22);
    f.AddPrevout(funding, P2PKHScript(attacker.GetPubKey()), 1 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = funding;
    tx.vout.resize(1);
    tx.vout[0].nValue = 99 * COIN / 100;
    tx.vout[0].scriptPubKey = TransferScript(attacker.GetPubKey(), victimLineage);

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// The same shape without a victim: an origin is 32 bytes, and nothing
// requires it to correspond to any outpoint that ever existed. Left
// unchecked this conjures a whole token -- at any multiplier, backed by
// dust -- without paying the OP_MINT entry fee at all.
BOOST_AUTO_TEST_CASE(rejects_a_lineage_that_no_mint_ever_created)
{
    CKey attacker;
    attacker.MakeNewKey(true);

    valtype invented(32, 0xab); // not SHA256 of anything

    CreationFixture f;
    const COutPoint funding = Point(0x33);
    f.AddPrevout(funding, P2PKHScript(attacker.GetPubKey()), 1 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = funding;
    tx.vout.resize(1);
    tx.vout[0].nValue = 1000;
    tx.vout[0].scriptPubKey = TransferScript(attacker.GetPubKey(), invented,
                                             std::numeric_limits<int32_t>::max());

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// A coinbase spends nothing, so it can satisfy no lineage. This needs its
// own fixture because ConnectBlock does not run CheckInputs over the
// coinbase (validation.cpp) -- a rule that rode along inside CheckInputs
// would leave miners as the one party still able to fabricate positions.
BOOST_AUTO_TEST_CASE(rejects_a_transfer_output_in_a_coinbase)
{
    CKey miner;
    miner.MakeNewKey(true);

    CreationFixture f;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout.SetNull(); // coinbase
    tx.vin[0].scriptSig = CScript() << 1 << OP_0;
    tx.vout.resize(1);
    tx.vout[0].nValue = 50 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(miner.GetPubKey(), OriginOf(Point(0x44)));

    BOOST_REQUIRE(CTransaction(tx).IsCoinBase());

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// The ordinary case must still work: spending a position of lineage X and
// creating a position of lineage X.
BOOST_AUTO_TEST_CASE(accepts_a_transfer_backed_by_an_input_of_the_same_lineage)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype lineage = OriginOf(Point(0x55));

    CreationFixture f;
    const COutPoint position = Point(0x56);
    f.AddPrevout(position, TransferScript(holder.GetPubKey(), lineage), 10 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = position;
    tx.vout.resize(1);
    tx.vout[0].nValue = 10 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), lineage);

    std::string reason;
    BOOST_CHECK_MESSAGE(f.Check(tx, reason), "an ordinary transfer must be allowed: " + reason);
}

// A mint's first spend is where its lineage is born, and the mint output
// itself cannot name it (the origin depends on the outpoint, which depends
// on the txid, which depends on the script). So a mint input entitles the
// transaction to the lineage its own outpoint mints into -- derived here
// exactly as the interpreter derives it when stamping the covenant.
BOOST_AUTO_TEST_CASE(accepts_a_first_spend_stamping_the_lineage_of_the_mint_it_spends)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CreationFixture f;
    const COutPoint mintPoint = Point(0x66, 3);
    f.AddPrevout(mintPoint, MintScript(minter.GetPubKey()), 1000 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = mintPoint;
    tx.vout.resize(1);
    tx.vout[0].nValue = 1000 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    std::string reason;
    BOOST_CHECK_MESSAGE(f.Check(tx, reason), "a mint's first spend must be allowed: " + reason);

    // ...and only into its OWN lineage. Naming a different one here is the
    // rename the interpreter also refuses, caught at creation as well.
    CMutableTransaction renamed = tx;
    renamed.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(Point(0x67)));
    BOOST_CHECK(!f.Check(renamed, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// Grouping is by (multiplier, origin), exactly as conservation groups. A
// position of the same lineage at a different multiplier is a different
// token, so spending one does not entitle you to create the other -- which
// would otherwise be a supply increase dressed up as a transfer.
BOOST_AUTO_TEST_CASE(rejects_an_output_sharing_the_lineage_but_not_the_multiplier)
{
    CKey holder;
    holder.MakeNewKey(true);

    const valtype lineage = OriginOf(Point(0x77));

    CreationFixture f;
    const COutPoint position = Point(0x78);
    f.AddPrevout(position, TransferScript(holder.GetPubKey(), lineage, MULTIPLIER), 10 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = position;
    tx.vout.resize(1);
    tx.vout[0].nValue = 10 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(holder.GetPubKey(), lineage, MULTIPLIER + 1);

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// A genuine spend is not a licence to mint a second, unrelated lineage
// alongside it. The interpreter refuses this too (conservation rejects an
// output group with no input), so this fixture pins that the creation rule
// draws the line in the same place rather than a looser one.
BOOST_AUTO_TEST_CASE(rejects_a_phantom_lineage_smuggled_beside_a_genuine_one)
{
    CKey holder, attacker;
    holder.MakeNewKey(true);
    attacker.MakeNewKey(true);

    const valtype lineage = OriginOf(Point(0x88));
    const valtype phantom = OriginOf(Point(0x89));

    CreationFixture f;
    const COutPoint position = Point(0x8a);
    f.AddPrevout(position, TransferScript(holder.GetPubKey(), lineage), 10 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = position;
    tx.vout.resize(2);
    tx.vout[0].nValue = 9 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(holder.GetPubKey(), lineage);
    tx.vout[1].nValue = 1 * COIN;
    tx.vout[1].scriptPubKey = TransferScript(attacker.GetPubKey(), phantom);

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-output-without-input");
}

// Mint outputs stay unrestricted, and that is deliberate: creating one is
// how a lineage comes into existence at all, and a mint is inert until
// spent -- at which point the entry fee and the outpoint-derived lineage
// are both enforced by the interpreter. A rule that caught mints here would
// make minting impossible.
BOOST_AUTO_TEST_CASE(allows_a_mint_output_to_be_created_from_plain_inputs)
{
    CKey minter;
    minter.MakeNewKey(true);

    CreationFixture f;
    const COutPoint funding = Point(0x99);
    f.AddPrevout(funding, P2PKHScript(minter.GetPubKey()), 2000 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = funding;
    tx.vout.resize(1);
    tx.vout[0].nValue = 1000 * COIN;
    tx.vout[0].scriptPubKey = MintScript(minter.GetPubKey());

    std::string reason;
    BOOST_CHECK_MESSAGE(f.Check(tx, reason), "minting must remain possible: " + reason);
}

// The division of responsibility, pinned. A transaction spending a position
// may not also create a mint output -- but that is CONSERVATION's rule
// (interpreter.cpp rejects any mint-shaped output when a position is spent),
// not this one's. This rule is about lineage provenance and must wave the
// mint output through, leaving the interpreter to refuse the transaction.
//
// Without this fixture the `fIsMint` skip in CheckUapOutputCreation is
// untestable: every other mint case returns early from the cheap pre-scan,
// so deleting the skip changes no outcome and no test notices. Here the
// transfer output forces the pre-scan open, and the mint output is then
// judged on its own.
BOOST_AUTO_TEST_CASE(leaves_mint_shaped_outputs_to_conservation)
{
    CKey holder;
    holder.MakeNewKey(true);

    const valtype lineage = OriginOf(Point(0xcc));

    CreationFixture f;
    const COutPoint position = Point(0xcd);
    f.AddPrevout(position, TransferScript(holder.GetPubKey(), lineage), 10 * COIN);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = position;
    tx.vout.resize(2);
    tx.vout[0].nValue = 9 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(holder.GetPubKey(), lineage); // properly backed
    tx.vout[1].nValue = 1 * COIN;
    tx.vout[1].scriptPubKey = MintScript(holder.GetPubKey());              // conservation's problem

    std::string reason;
    BOOST_CHECK_MESSAGE(f.Check(tx, reason),
        "a mint output must not be rejected by the creation rule -- conservation "
        "is what forbids it here, and reporting it as a provenance failure would "
        "misattribute the reason: " + reason);
}

// A transaction with no covenant output at all is not this rule's business,
// and must not pay for the check. This is also the fast path every ordinary
// transaction on the chain takes.
BOOST_AUTO_TEST_CASE(ignores_a_transaction_with_no_covenant_outputs)
{
    CKey payer;
    payer.MakeNewKey(true);

    CreationFixture f;
    // Deliberately no prevout installed: the pre-scan must return before
    // anything tries to read the inputs.
    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xaa);
    tx.vout.resize(1);
    tx.vout[0].nValue = 1 * COIN;
    tx.vout[0].scriptPubKey = P2PKHScript(payer.GetPubKey());

    std::string reason;
    BOOST_CHECK_MESSAGE(f.Check(tx, reason), "an ordinary payment must not be touched: " + reason);
}

// Fail closed. If an input's prevout cannot be read, its script is unknown,
// so whether it entitles this transaction to the lineage being created is
// unknown too. Treating an unreadable input as "not a UAP input" would be a
// way to widen the permitted set rather than narrow it.
BOOST_AUTO_TEST_CASE(refuses_to_decide_when_a_prevout_is_unavailable)
{
    CKey attacker;
    attacker.MakeNewKey(true);

    CreationFixture f;
    // No AddPrevout call: the input names a coin the view has never heard of.
    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xbb);
    tx.vout.resize(1);
    tx.vout[0].nValue = 1 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(attacker.GetPubKey(), OriginOf(Point(0xbc)));

    std::string reason;
    BOOST_CHECK(!f.Check(tx, reason));
    BOOST_CHECK_EQUAL(reason, "bad-txns-uap-creation-missing-prevout");
}

BOOST_AUTO_TEST_SUITE_END()
