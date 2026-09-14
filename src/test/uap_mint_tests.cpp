// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "crypto/sha256.h"
#include "key.h"
#include "primitives/transaction.h"
#include "script/interpreter.h"
#include "script/script.h"
#include "script/script_error.h"
#include "script/standard.h"
#include "uint256.h"
#include "utilstrencodings.h"

#include "data/uap_origin_vectors.json.h"
#include "data/uap_script_vectors.json.h"

#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>
#include <univalue.h>

#include <cstring>
#include <set>
#include <limits>

extern UniValue read_json(const std::string& jsondata);

typedef std::vector<unsigned char> valtype;

BOOST_FIXTURE_TEST_SUITE(uap_mint_tests, BasicTestingSetup)

namespace {

const unsigned int FLAGS = SCRIPT_VERIFY_P2SH | SCRIPT_VERIFY_STRICTENC | SCRIPT_VERIFY_NULLFAIL | SCRIPT_VERIFY_UAP_MINT;
const int64_t MULTIPLIER = 1000;

// A distinct, deterministic outpoint per fixture. Which outpoint a mint is
// spent at is no longer cosmetic: it *is* the lineage's name. Fixtures name
// theirs explicitly rather than leaning on the default null prevout, so that
// two fixtures cannot silently share a lineage.
COutPoint Point(unsigned char tag, uint32_t n = 0)
{
    uint256 hash;
    memset(hash.begin(), tag, 32);
    return COutPoint(hash, n);
}

// SHA256(prevout.hash || prevout.n as 4-byte little-endian).
//
// Deliberately spelled out here instead of calling the interpreter's copy:
// this formula *is* the definition of a lineage's identity, so the test has to
// state it independently. A shared helper would assert only that the
// interpreter agrees with itself.
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

CScript SignSpend(const CScript& scriptCode, const CKey& key, const CMutableTransaction& txTo, unsigned int nIn)
{
    uint256 hash = SignatureHash(scriptCode, txTo, nIn, SIGHASH_ALL, 0, SIGVERSION_BASE);
    valtype vchSig;
    BOOST_CHECK(key.Sign(hash, vchSig));
    vchSig.push_back((unsigned char)SIGHASH_ALL);
    return CScript() << vchSig;
}

// Verify one input with the full prevout context the covenant now requires.
//
// Conservation sums every input belonging to the spent lineage, not just the
// input under verification, so a checker that cannot report sibling inputs'
// scripts and values cannot verify *any* UAP spend -- it fails closed rather
// than guessing. Every fixture below therefore goes through here, and
// `no_prevout_context_fails_closed` pins that behaviour down directly.
bool VerifyUapInput(CMutableTransaction& tx, unsigned int nIn,
                    const std::vector<CScript>& prevScripts,
                    const std::vector<CAmount>& prevAmounts,
                    ScriptError& err)
{
    BOOST_REQUIRE_EQUAL(prevScripts.size(), tx.vin.size());
    BOOST_REQUIRE_EQUAL(prevAmounts.size(), tx.vin.size());
    return VerifyScript(tx.vin[nIn].scriptSig, prevScripts[nIn], NULL, FLAGS,
        MutableTransactionSignatureChecker(&tx, nIn, prevAmounts[nIn], &prevScripts, &prevAmounts), &err);
}

} // namespace

// A mint output can only be spent by a signature from its declared
// recipient, and must forward its full value into a conforming
// OP_MINT_TRANSFER covenant with the same multiplier -- now also carrying
// the lineage identity derived from the outpoint being spent.
BOOST_AUTO_TEST_CASE(mint_then_transfer_succeeds_with_correct_signature_and_covenant)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x11);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

BOOST_AUTO_TEST_CASE(mint_rejects_wrong_signer)
{
    CKey minter, impostor, recipient;
    minter.MakeNewKey(true);
    impostor.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x12);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    // Signed by someone other than the pubkey embedded in the mint script.
    spendTx.vin[0].scriptSig = SignSpend(mintScript, impostor, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
}

BOOST_AUTO_TEST_CASE(mint_rejects_entry_fee_below_threshold)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x13);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 999 * COIN; // below the 1000 coin entry fee

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_MINT_ENTRY_FEE);
}

BOOST_AUTO_TEST_CASE(mint_rejects_non_covenant_output)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x14);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    // Plain P2PK: the position's value would escape the covenant entirely.
    spendTx.vout[0].scriptPubKey = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG;

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_LINEAGE_NOT_CONTINUED);
}

BOOST_AUTO_TEST_CASE(mint_rejects_multiplier_mismatch_in_output)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x15);
    CScript mintScript = MintScript(minter.GetPubKey(), MULTIPLIER);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    // Correct lineage, wrong multiplier: minting supply out of nothing.
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), MULTIPLIER * 2);

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_UNBACKED_LINEAGE_OUTPUT);
}

BOOST_AUTO_TEST_CASE(mint_rejects_value_created_out_of_thin_air)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x16);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn + 1; // more than the input holds
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_CONSERVATION_VIOLATION);
}

// A transfer output spends the same way a mint output does, minus the
// entry-fee requirement, and continues to require conservation.
BOOST_AUTO_TEST_CASE(transfer_chain_continues_correctly)
{
    CKey holder, nextHolder;
    holder.MakeNewKey(true);
    nextHolder.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0x21));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 10 * COIN; // below mint's 1000-coin floor; fine for transfers

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = Point(0x22); // NOT the mint outpoint -- irrelevant for a transfer
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn - 1000; // small fee
    spendTx.vout[0].scriptPubKey = TransferScript(nextHolder.GetPubKey(), origin);

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 0, {transferScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// One-shot: a mint transaction may not also spend another UAP position as a
// sibling input, to keep asset accounting isolated to one position per tx.
BOOST_AUTO_TEST_CASE(mint_rejects_sibling_uap_input)
{
    CKey minter, otherHolder, recipient;
    minter.MakeNewKey(true);
    otherHolder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x31);
    CScript mintScript = MintScript(minter.GetPubKey());
    CScript otherUapScript = TransferScript(otherHolder.GetPubKey(), OriginOf(Point(0x32)));
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vin[1].prevout = Point(0x33);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);
    // vin[1] purports to spend an unrelated UAP transfer output alongside the mint.

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript, otherUapScript}, {nValueIn, 5 * COIN}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_ONE_SHOT_VIOLATION);
}

// A transfer may coexist with ordinary, non-UAP outputs in the same
// transaction (e.g. a marketplace payment leg or plain change) as long as
// the continuing covenant output's value doesn't exceed the input's value.
// This is what makes an atomic token-for-WHIP swap possible in a single
// transaction: the buyer's payment output doesn't need to be a covenant.
BOOST_AUTO_TEST_CASE(transfer_allows_non_covenant_sibling_outputs)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0x41));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    CScript plainScript = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG;
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2); // token input (0) + an unrelated plain payment input (1)
    spendTx.vin[0].prevout = Point(0x42);
    spendTx.vin[1].prevout = Point(0x43);
    spendTx.vout.resize(2);
    spendTx.vout[0].nValue = nValueIn; // continuing covenant, full value
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin);
    spendTx.vout[1].nValue = 5 * COIN; // ordinary payment output, not a covenant
    spendTx.vout[1].scriptPubKey = plainScript;

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 0, {transferScript, plainScript}, {nValueIn, 6 * COIN}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// A transfer must still produce at least one continuing covenant output --
// spending a position entirely into ordinary outputs (no covenant at all)
// is rejected, not treated as an implicit burn/redemption.
BOOST_AUTO_TEST_CASE(transfer_rejects_no_continuing_covenant)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript transferScript = TransferScript(holder.GetPubKey(), OriginOf(Point(0x51)));
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = Point(0x52);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn - 1000;
    spendTx.vout[0].scriptPubKey = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG; // not a covenant

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {transferScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_LINEAGE_NOT_CONTINUED);
}

// ---------------------------------------------------------------------------
// Lineage identity and merging.
//
// These are the rules the v2 covenant exists for, and none of them can be
// expressed in src/test/data/uap_script_vectors.json: a vector there is one
// script parsed in isolation, whereas every rule below is a property of the
// whole spending transaction (which inputs it spends, at which outpoints, and
// what they are worth). The vector file covers script *shape*; this section
// covers lineage *behaviour*. Both are needed to describe the format.
// ---------------------------------------------------------------------------

// Spending a mint stamps the lineage with SHA256(outpoint) -- the one moment
// identity is created. A mint output cannot carry its own outpoint (the
// outpoint depends on the txid, which depends on the script), so first spend
// is the earliest point the interpreter can see it.
BOOST_AUTO_TEST_CASE(mint_first_spend_stamps_origin_from_its_outpoint)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x61, 7);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// ...and any other origin is refused, including one derived from a *different
// index of the same transaction*. Two mint outputs in one transaction share a
// txid and differ only in n, so an origin derived from the txid alone would
// collide them into a single lineage -- and merging would then let one mint's
// holder dilute the other's.
BOOST_AUTO_TEST_CASE(mint_first_spend_rejects_a_foreign_origin)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x62, 0);
    const CAmount nValueIn = 1000 * COIN;

    // Same txid, different index; and an unrelated outpoint entirely.
    const valtype wrongOrigins[] = {
        OriginOf(Point(0x62, 1)),
        OriginOf(Point(0x63, 0)),
        valtype(32, 0x00),
    };

    for (const valtype& wrong : wrongOrigins) {
        BOOST_REQUIRE(wrong != OriginOf(mintPoint));

        CScript mintScript = MintScript(minter.GetPubKey());
        CMutableTransaction spendTx;
        spendTx.vin.resize(1);
        spendTx.vin[0].prevout = mintPoint;
        spendTx.vout.resize(1);
        spendTx.vout[0].nValue = nValueIn;
        spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), wrong);
        spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

        ScriptError err;
        BOOST_CHECK_MESSAGE(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err),
            "a mint spend was allowed to stamp origin " << HexStr(wrong));
    }
}

// A mint need not be input 0. Its lineage is the outpoint of the input
// *being verified*, which coincides with the transaction's first input in
// every single-input fixture above -- so a rule that read vin[0] instead of
// vin[nIn] would pass all of them and misname every lineage minted in a
// transaction that also has a funding input. Written separately for exactly
// that reason.
BOOST_AUTO_TEST_CASE(mint_at_a_nonzero_input_index_uses_its_own_outpoint)
{
    CKey funder, minter, recipient;
    funder.MakeNewKey(true);
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint fundingPoint = Point(0x64);
    const COutPoint mintPoint = Point(0x65);
    CScript fundingScript = CScript() << ToByteVector(funder.GetPubKey()) << OP_CHECKSIG;
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount fundingValue = 3 * COIN;
    const CAmount nValueIn = 1000 * COIN;

    const std::vector<CScript> prevScripts = {fundingScript, mintScript};
    const std::vector<CAmount> prevAmounts = {fundingValue, nValueIn};

    // The mint is input 1; input 0 is an ordinary (non-UAP, so one-shot-safe)
    // funding input.
    CMutableTransaction spendTx;
    spendTx.vin.resize(2);
    spendTx.vin[0].prevout = fundingPoint;
    spendTx.vin[1].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));
    spendTx.vin[1].scriptSig = SignSpend(mintScript, minter, spendTx, 1);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 1, prevScripts, prevAmounts, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));

    // ...and the funding input's outpoint is not an acceptable lineage name.
    CMutableTransaction wrongTx = spendTx;
    wrongTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(fundingPoint));
    wrongTx.vin[1].scriptSig = SignSpend(mintScript, minter, wrongTx, 1);

    ScriptError errWrong;
    BOOST_CHECK_MESSAGE(!VerifyUapInput(wrongTx, 1, prevScripts, prevAmounts, errWrong),
        "a mint took its lineage from another input's outpoint");
}

// The cross-implementation contract for how an origin is derived
// (src/test/data/uap_origin_vectors.json). The Go indexer and the JS wallet
// read the same file.
//
// Every one of those implementations can be self-consistent and still
// disagree with consensus, because the derivation hashes the txid's INTERNAL
// byte order -- the reverse of how a txid is displayed everywhere a human or
// an RPC would see one. A port that hashes the display bytes passes all of
// its own tests and produces lineages this chain will never accept. These
// vectors are the only thing standing between that mistake and a position
// nobody can spend.
BOOST_AUTO_TEST_CASE(origin_derivation_matches_shared_fixture)
{
    const std::string raw(json_tests::uap_origin_vectors,
                          json_tests::uap_origin_vectors + sizeof(json_tests::uap_origin_vectors));
    UniValue tests = read_json(raw);
    BOOST_REQUIRE(tests.isArray());
    BOOST_REQUIRE_GT(tests.size(), 0);

    std::set<std::string> seen;
    for (size_t idx = 0; idx < tests.size(); idx++) {
        const UniValue& test = tests[idx];
        BOOST_REQUIRE_MESSAGE(test.exists("txid") && test.exists("n") && test.exists("origin"),
            "origin vector " << idx << ": missing required fields");
        const std::string where = "origin vector " + std::to_string(idx)
            + " (" + test["comment"].get_str() + ")";

        // uint256S parses the displayed (big-endian) form and stores the
        // internal reversed bytes -- exactly the conversion a port must make.
        const COutPoint outpoint(uint256S(test["txid"].get_str()),
                                 (uint32_t)test["n"].get_int64());
        const valtype expect = ParseHex(test["origin"].get_str());
        BOOST_REQUIRE_MESSAGE(expect.size() == 32, where << ": declared origin is not 32 bytes");
        BOOST_CHECK_MESSAGE(OriginOf(outpoint) == expect,
            where << ": derived 0x" << HexStr(OriginOf(outpoint))
                  << " but the fixture says 0x" << HexStr(expect));

        // Distinct outpoints must give distinct origins, or two lineages
        // collide and merging would let one dilute the other.
        BOOST_CHECK_MESSAGE(seen.insert(test["origin"].get_str()).second,
            where << ": duplicate origin in the fixture");
    }

    // The fixture must actually contain the byte-order trap it claims to:
    // a txid and its own reversal, which a port that skips the reversal
    // would map to the same origin as each other's.
    const COutPoint a(uint256S("00000000000000000000000000000000000000000000000000000000000000ff"), 0);
    const COutPoint b(uint256S("ff00000000000000000000000000000000000000000000000000000000000000"), 0);
    BOOST_CHECK(OriginOf(a) != OriginOf(b));

    // ...and that n is part of the preimage, not decoration.
    const COutPoint c(uint256S("00000000000000000000000000000000000000000000000000000000000000ff"), 1);
    BOOST_CHECK(OriginOf(a) != OriginOf(c));
}

// A transfer carries its origin forward untouched. Rewriting it would let a
// holder rename their own position into someone else's lineage, which is the
// whole attack the origin exists to prevent.
BOOST_AUTO_TEST_CASE(transfer_rejects_rewriting_the_origin)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0x71));
    const valtype foreign = OriginOf(Point(0x72));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = Point(0x73);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn - 1000;
    // Same pubkey shape, same multiplier -- only the lineage differs.
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), foreign);
    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {transferScript}, {nValueIn}, err));
}

// The feature: two positions of the *same* lineage combine into one, with the
// full value of both. Under the v1 rule this transaction was invalid --
// conservation compared the total covenant output against a single input's
// value, so merging two positions worth V each capped the output at V and
// burned the rest. Fragmentation was a one-way ratchet.
BOOST_AUTO_TEST_CASE(transfer_allows_merging_positions_of_one_lineage)
{
    CKey holderA, holderB, recipient;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0x81));
    CScript scriptA = TransferScript(holderA.GetPubKey(), origin);
    CScript scriptB = TransferScript(holderB.GetPubKey(), origin);
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 3000 * COIN;

    CMutableTransaction mergeTx;
    mergeTx.vin.resize(2);
    mergeTx.vin[0].prevout = Point(0x82);
    mergeTx.vin[1].prevout = Point(0x83);
    mergeTx.vout.resize(1);
    mergeTx.vout[0].nValue = valueA + valueB - 1000; // combined, minus fee
    mergeTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin);

    mergeTx.vin[0].scriptSig = SignSpend(scriptA, holderA, mergeTx, 0);
    mergeTx.vin[1].scriptSig = SignSpend(scriptB, holderB, mergeTx, 1);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, valueB};

    ScriptError errA;
    BOOST_CHECK_MESSAGE(VerifyUapInput(mergeTx, 0, prevScripts, prevAmounts, errA), ScriptErrorString(errA));
    ScriptError errB;
    BOOST_CHECK_MESSAGE(VerifyUapInput(mergeTx, 1, prevScripts, prevAmounts, errB), ScriptErrorString(errB));
}

// ...but a merge may still not create value: the combined output is capped by
// the combined input, not waved through because the inputs agree on lineage.
BOOST_AUTO_TEST_CASE(transfer_merge_still_cannot_create_value)
{
    CKey holderA, holderB, recipient;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0x84));
    CScript scriptA = TransferScript(holderA.GetPubKey(), origin);
    CScript scriptB = TransferScript(holderB.GetPubKey(), origin);
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 3000 * COIN;

    CMutableTransaction mergeTx;
    mergeTx.vin.resize(2);
    mergeTx.vin[0].prevout = Point(0x85);
    mergeTx.vin[1].prevout = Point(0x86);
    mergeTx.vout.resize(1);
    mergeTx.vout[0].nValue = valueA + valueB + 1; // one satoshi more than exists
    mergeTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin);

    mergeTx.vin[0].scriptSig = SignSpend(scriptA, holderA, mergeTx, 0);
    mergeTx.vin[1].scriptSig = SignSpend(scriptB, holderB, mergeTx, 1);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, valueB};

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(mergeTx, 0, prevScripts, prevAmounts, err));
    BOOST_CHECK(!VerifyUapInput(mergeTx, 1, prevScripts, prevAmounts, err));
}

// Two independent lineages cannot be merged, *even at an identical
// multiplier*. The matching multiplier is the point: under v1 the multiplier
// was the only identity-ish field in the covenant, and this case was caught
// only as an arithmetic accident of the per-input rule. Now it is refused on
// identity. Paired with the merge test above, this is the assertion that
// distinguishes real grouping from a global sum over all UAP inputs -- which
// would let anyone add supply to someone else's token by posting par backing.
BOOST_AUTO_TEST_CASE(transfer_rejects_merging_independent_lineages)
{
    CKey holderA, holderB, attacker;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    attacker.MakeNewKey(true);

    const valtype originA = OriginOf(Point(0x91));
    const valtype originB = OriginOf(Point(0x92));
    CScript scriptA = TransferScript(holderA.GetPubKey(), originA);
    CScript scriptB = TransferScript(holderB.GetPubKey(), originB);
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 5000 * COIN;

    CMutableTransaction mergeTx;
    mergeTx.vin.resize(2);
    mergeTx.vin[0].prevout = Point(0x93);
    mergeTx.vin[1].prevout = Point(0x94);
    mergeTx.vout.resize(1);
    mergeTx.vout[0].nValue = valueA + valueB - 1000;
    mergeTx.vout[0].scriptPubKey = TransferScript(attacker.GetPubKey(), originA); // A's lineage swallows B's

    mergeTx.vin[0].scriptSig = SignSpend(scriptA, holderA, mergeTx, 0);
    mergeTx.vin[1].scriptSig = SignSpend(scriptB, holderB, mergeTx, 1);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, valueB};

    ScriptError err;
    // Input 0 sees only valueA in its group, so the doubled output overflows it.
    BOOST_CHECK(!VerifyUapInput(mergeTx, 0, prevScripts, prevAmounts, err));
    // Input 1's own lineage has no continuing output at all.
    BOOST_CHECK(!VerifyUapInput(mergeTx, 1, prevScripts, prevAmounts, err));
}

// Two lineages may be spent in one transaction as long as each is conserved
// on its own. This is what stops the rule above from being "reject any
// transaction touching two lineages", which would be a much blunter rule and
// would forbid, say, a single transaction settling two independent trades.
BOOST_AUTO_TEST_CASE(two_lineages_in_one_transaction_each_conserved_is_fine)
{
    CKey holderA, holderB, recipient;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype originA = OriginOf(Point(0xa1));
    const valtype originB = OriginOf(Point(0xa2));
    CScript scriptA = TransferScript(holderA.GetPubKey(), originA);
    CScript scriptB = TransferScript(holderB.GetPubKey(), originB);
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 3000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(2);
    tx.vin[0].prevout = Point(0xa3);
    tx.vin[1].prevout = Point(0xa4);
    tx.vout.resize(2);
    tx.vout[0].nValue = valueA - 1000;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), originA);
    tx.vout[1].nValue = valueB - 1000;
    tx.vout[1].scriptPubKey = TransferScript(recipient.GetPubKey(), originB);

    tx.vin[0].scriptSig = SignSpend(scriptA, holderA, tx, 0);
    tx.vin[1].scriptSig = SignSpend(scriptB, holderB, tx, 1);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, valueB};

    ScriptError errA;
    BOOST_CHECK_MESSAGE(VerifyUapInput(tx, 0, prevScripts, prevAmounts, errA), ScriptErrorString(errA));
    ScriptError errB;
    BOOST_CHECK_MESSAGE(VerifyUapInput(tx, 1, prevScripts, prevAmounts, errB), ScriptErrorString(errB));
}

// ...and conservation binds each group separately: over-issuing one lineage
// fails even when the transaction's overall covenant total balances against
// its overall covenant input total. Without per-group comparison, one lineage
// could be inflated by draining another.
BOOST_AUTO_TEST_CASE(conservation_binds_per_lineage_not_transaction_wide)
{
    CKey holderA, holderB, recipient;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype originA = OriginOf(Point(0xb1));
    const valtype originB = OriginOf(Point(0xb2));
    CScript scriptA = TransferScript(holderA.GetPubKey(), originA);
    CScript scriptB = TransferScript(holderB.GetPubKey(), originB);
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 3000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(2);
    tx.vin[0].prevout = Point(0xb3);
    tx.vin[1].prevout = Point(0xb4);
    tx.vout.resize(2);
    // Transaction-wide the covenant total is exactly valueA + valueB, but A is
    // over-issued by 1000 coin at B's expense.
    tx.vout[0].nValue = valueA + 1000 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), originA);
    tx.vout[1].nValue = valueB - 1000 * COIN;
    tx.vout[1].scriptPubKey = TransferScript(recipient.GetPubKey(), originB);

    tx.vin[0].scriptSig = SignSpend(scriptA, holderA, tx, 0);
    tx.vin[1].scriptSig = SignSpend(scriptB, holderB, tx, 1);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, valueB};

    ScriptError errA;
    BOOST_CHECK_MESSAGE(!VerifyUapInput(tx, 0, prevScripts, prevAmounts, errA),
        "lineage A was allowed to over-issue at lineage B's expense");
    // B's own spend is under-issuing, which conservation permits (value may
    // leave a position); it is A's side that must fail.
    ScriptError errB;
    BOOST_CHECK_MESSAGE(VerifyUapInput(tx, 1, prevScripts, prevAmounts, errB), ScriptErrorString(errB));
}

// ...and the permission above is exactly as wide as it needs to be: a foreign
// lineage may appear in the outputs only because it also has an input here,
// which guarantees its own conservation check runs. An output of a lineage
// with NO input in the transaction is tokens from nothing -- nothing else
// would ever examine it -- so it must be refused.
BOOST_AUTO_TEST_CASE(rejects_output_of_a_lineage_with_no_input)
{
    CKey holder, attacker;
    holder.MakeNewKey(true);
    attacker.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xb7));
    const valtype phantom = OriginOf(Point(0xb8)); // never spent here
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 5000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xb9);
    tx.vout.resize(2);
    tx.vout[0].nValue = 2000 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(holder.GetPubKey(), origin); // conserved
    tx.vout[1].nValue = 2000 * COIN;
    // A position in a lineage this transaction never spends from: its backing
    // is real, but its supply would be conjured, since the only thing that
    // ever binds a lineage is a spend of one of its own positions.
    tx.vout[1].scriptPubKey = TransferScript(attacker.GetPubKey(), phantom);
    tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_UNBACKED_LINEAGE_OUTPUT);
}

// The same, at a matching multiplier: the phantom lineage differs from the
// spent one *only* in its origin, so this is the case a multiplier-only
// grouping would wave through.
BOOST_AUTO_TEST_CASE(rejects_phantom_lineage_sharing_the_multiplier)
{
    CKey holder, attacker;
    holder.MakeNewKey(true);
    attacker.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xba));
    const valtype phantom = OriginOf(Point(0xbb));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin, MULTIPLIER);
    const CAmount nValueIn = 5000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xbc);
    tx.vout.resize(2);
    tx.vout[0].nValue = 4999 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(holder.GetPubKey(), origin, MULTIPLIER);
    tx.vout[1].nValue = 1 * COIN;
    tx.vout[1].scriptPubKey = TransferScript(attacker.GetPubKey(), phantom, MULTIPLIER);
    tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_UNBACKED_LINEAGE_OUTPUT);
}

// A position may be split into several outputs of one lineage.
BOOST_AUTO_TEST_CASE(transfer_allows_splitting_one_position_into_many)
{
    CKey holder, recipientA, recipientB;
    holder.MakeNewKey(true);
    recipientA.MakeNewKey(true);
    recipientB.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xc1));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 5000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xc2);
    tx.vout.resize(2);
    tx.vout[0].nValue = 2000 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(recipientA.GetPubKey(), origin);
    tx.vout[1].nValue = 2999 * COIN;
    tx.vout[1].scriptPubKey = TransferScript(recipientB.GetPubKey(), origin);
    tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// No output may be mint-shaped. A mint's origin comes from the outpoint being
// spent, so a mint output produced *by* a spend would be a second lineage head
// funded from an existing position's backing -- supply from nothing. This is
// also what makes melt (redeeming backing to plain WHIP) emit
// OP_MINT_TRANSFER rather than OP_MINT.
//
// Like the wrong-width origin above, this is caught twice: a mint output
// carries no origin at all, so it can never match the spent lineage nor
// appear among the transaction's input lineages. The explicit check states
// the rule where a reader looks for it instead of leaving it as an emergent
// consequence of two other comparisons.
BOOST_AUTO_TEST_CASE(spend_rejects_a_mint_shaped_output)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xd1));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 5000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xd2);
    tx.vout.resize(2);
    tx.vout[0].nValue = 2000 * COIN;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin); // a valid continuation...
    tx.vout[1].nValue = 2000 * COIN;
    tx.vout[1].scriptPubKey = MintScript(recipient.GetPubKey()); // ...alongside a fresh mint head
    tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_MINT_SHAPED_OUTPUT);
}

// A covenant whose origin is not 32 bytes is unspendable. Two independent
// rules reject it -- the interpreter's explicit width check, and the fact
// that ParseUapOutputScript accepts only 32-byte origins, so no conforming
// output could ever match a wrong-width lineage anyway. The redundancy is
// deliberate: the failure mode being guarded against is a position the chain
// accepts but nobody can ever spend, which is stuck value rather than a
// clean rejection. (Mutation testing confirms either rule alone suffices; do
// not delete one on the grounds that this test still passes.)
BOOST_AUTO_TEST_CASE(covenant_with_wrong_width_origin_is_unspendable)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const CAmount nValueIn = 500 * COIN;
    const valtype good = OriginOf(Point(0xe3));

    const valtype widths[] = {valtype(31, 0x5a), valtype(33, 0x5a), valtype()};
    for (const valtype& bad : widths) {
        CScript transferScript = TransferScript(holder.GetPubKey(), bad);

        CMutableTransaction tx;
        tx.vin.resize(1);
        tx.vin[0].prevout = Point(0xe4);
        tx.vout.resize(1);
        tx.vout[0].nValue = nValueIn - 1000;
        // Best-effort continuation: a well-formed covenant. Nothing the
        // spender can put here helps, which is the point.
        tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), good);
        tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

        ScriptError err;
        BOOST_CHECK_MESSAGE(!VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err),
            "a covenant with a " << bad.size() << "-byte origin was spendable");
        BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_ORIGIN_SIZE);
    }
}

// Conservation reads sibling inputs, so a checker with no prevout context
// cannot verify a UAP spend and must refuse rather than assume. Treating
// "unknown" as zero would reject valid merges; treating it as "no sibling
// belongs to this lineage" would admit invalid ones.
BOOST_AUTO_TEST_CASE(no_prevout_context_fails_closed)
{
    CKey holder, recipient;
    holder.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xe1));
    CScript transferScript = TransferScript(holder.GetPubKey(), origin);
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vin[0].prevout = Point(0xe2);
    tx.vout.resize(1);
    tx.vout[0].nValue = nValueIn - 1000;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin);
    tx.vin[0].scriptSig = SignSpend(transferScript, holder, tx, 0);

    // Identical transaction, verified without vPrevScriptPubKeys/vPrevAmounts.
    ScriptError errBlind;
    bool okBlind = VerifyScript(tx.vin[0].scriptSig, transferScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&tx, 0, nValueIn), &errBlind);
    BOOST_CHECK_MESSAGE(!okBlind, "a UAP spend verified without prevout context");
    BOOST_CHECK_EQUAL(errBlind, SCRIPT_ERR_UAP_INPUT_UNREADABLE);

    // ...and it is genuinely valid once the context is supplied, so the
    // rejection above is about the missing context and nothing else.
    ScriptError err;
    bool ok = VerifyUapInput(tx, 0, {transferScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// A prevout whose value is unavailable is reported as -1 by CScriptCheck
// (see validation.h). Conservation must reject rather than fold a negative
// into the lineage's input total.
BOOST_AUTO_TEST_CASE(unavailable_prevout_value_is_rejected)
{
    CKey holderA, holderB, recipient;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const valtype origin = OriginOf(Point(0xe5));
    CScript scriptA = TransferScript(holderA.GetPubKey(), origin);
    CScript scriptB = TransferScript(holderB.GetPubKey(), origin);
    const CAmount valueA = 5000 * COIN;

    CMutableTransaction tx;
    tx.vin.resize(2);
    tx.vin[0].prevout = Point(0xe6);
    tx.vin[1].prevout = Point(0xe7);
    tx.vout.resize(1);
    tx.vout[0].nValue = valueA - 1000;
    tx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), origin);
    tx.vin[0].scriptSig = SignSpend(scriptA, holderA, tx, 0);

    const std::vector<CScript> prevScripts = {scriptA, scriptB};
    const std::vector<CAmount> prevAmounts = {valueA, -1}; // sibling's coin unknown

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(tx, 0, prevScripts, prevAmounts, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_VALUE_OVERFLOW);
}

// Height-gated activation: an otherwise fully valid mint spend must be
// rejected as an unrecognized opcode when SCRIPT_VERIFY_UAP_MINT isn't
// set (i.e. before Consensus::Params::UAPMintHeight), and accepted once
// it is -- proving the flag genuinely gates the opcode rather than being
// vestigial.
BOOST_AUTO_TEST_CASE(mint_disabled_without_uap_mint_flag)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0xf1);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint));
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    const std::vector<CScript> prevScripts = {mintScript};
    const std::vector<CAmount> prevAmounts = {nValueIn};
    const unsigned int flagsWithoutUapMint = FLAGS & ~SCRIPT_VERIFY_UAP_MINT;

    ScriptError errDisabled;
    bool okDisabled = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, flagsWithoutUapMint,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn, &prevScripts, &prevAmounts), &errDisabled);
    BOOST_CHECK(!okDisabled);
    BOOST_CHECK_EQUAL(errDisabled, SCRIPT_ERR_BAD_OPCODE);

    // The exact same transaction succeeds once the flag is set (FLAGS
    // already includes it) -- confirming the rejection above was
    // specifically about the flag, not some other defect in the fixture.
    ScriptError errEnabled;
    bool okEnabled = VerifyUapInput(spendTx, 0, prevScripts, prevAmounts, errEnabled);
    BOOST_CHECK_MESSAGE(okEnabled, ScriptErrorString(errEnabled));
}

// A multiplier above INT32_MAX must be rejected outright rather than
// silently truncated by CScriptNum::getint() (which clamps to
// [INT32_MIN, INT32_MAX] instead of erroring). Both the input's own
// declared multiplier and a covenant output's multiplier go through this
// same bound.
BOOST_AUTO_TEST_CASE(mint_rejects_multiplier_above_int32_max)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0xf2);
    const int64_t hugeMultiplier = int64_t(std::numeric_limits<int32_t>::max()) + 1;
    CScript mintScript = MintScript(minter.GetPubKey(), hugeMultiplier);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), hugeMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
}

// The exact boundary value (INT32_MAX itself) must still be accepted --
// confirming the previous test rejects specifically for exceeding the
// bound, not multipliers in general once they get large.
BOOST_AUTO_TEST_CASE(mint_accepts_multiplier_at_int32_max)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0xf3);
    const int64_t maxMultiplier = std::numeric_limits<int32_t>::max();
    CScript mintScript = MintScript(minter.GetPubKey(), maxMultiplier);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), maxMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

// A negative multiplier is the one way to breach the [0, MAX_UAP_MULTIPLIER]
// bound while still fitting CScriptNum's default 4-byte encoding -- anything
// *above* MAX_UAP_MULTIPLIER (== INT32_MAX) cannot even be pushed onto the
// stack without tripping CScriptNum's own overflow check first (see
// mint_rejects_multiplier_above_int32_max, which never reaches this rule at
// all). This is the only case that pins the "multiplier < 0" half of rule 4
// down to its own, specific error rather than the interpreter's generic
// script-number exception.
BOOST_AUTO_TEST_CASE(mint_rejects_negative_multiplier)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0xf4);
    const int64_t negativeMultiplier = -1;
    CScript mintScript = MintScript(minter.GetPubKey(), negativeMultiplier);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), negativeMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_MULTIPLIER_RANGE);
}

// The multiplier-times-base-coin bound (rule 4's other half): a multiplier
// within [0, MAX_UAP_MULTIPLIER] can still push the position's virtual
// balance (base coin * multiplier) past 2^48, which is the range
// OP_INSPECT's virtual-balance selector and this guard both rely on staying
// inside. 200,000 coin at the maximum multiplier clears 2^48 comfortably
// while the multiplier itself stays in range, isolating this half of the
// rule from mint_rejects_negative_multiplier above.
BOOST_AUTO_TEST_CASE(mint_rejects_virtual_balance_above_bound)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0xf5);
    const int64_t hugeMultiplier = MAX_UAP_MULTIPLIER;
    CScript mintScript = MintScript(minter.GetPubKey(), hugeMultiplier);
    const CAmount nValueIn = 200000 * COIN; // base_coin = 200,000

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), hugeMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    BOOST_CHECK(!VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_VIRTUAL_BALANCE_BOUND);
}

// Solver()'s UAP recognition in standard.cpp triggers on a script's LAST
// byte matching OP_MINT/OP_MINT_TRANSFER, which is not on its own good
// evidence of a real UAP script -- an unrelated script (e.g. a witness
// program, whose final byte is just the tail of a hash) can coincidentally
// end that way. This must fall through to normal classification rather
// than misclassifying (or outright failing to classify) an unrelated,
// perfectly valid script. Found via a ~1/256-odds collision in
// transaction_tests' test_witness (which uses randomly generated keys
// each run) silently mismatching a P2WSH witness program.
BOOST_AUTO_TEST_CASE(solver_falls_through_on_coincidental_last_byte_match)
{
    // A witness-v0 program is just <OP_0> <push of a 20 or 32-byte hash>.
    // Construct one whose hash happens to end in the OP_MINT byte.
    valtype hash(32, 0x00);
    hash[hash.size() - 1] = OP_MINT; // 0xb5 -- coincidental collision
    CScript witnessProgram = CScript() << OP_0 << hash;
    BOOST_CHECK_EQUAL(witnessProgram[witnessProgram.size() - 1], (unsigned char)OP_MINT);

    txnouttype whichType;
    std::vector<valtype> solutions;
    bool ok = Solver(witnessProgram, whichType, solutions);
    BOOST_CHECK(ok);
    BOOST_CHECK_EQUAL(whichType, TX_WITNESS_V0_SCRIPTHASH);

    // Same check for the OP_MINT_TRANSFER byte.
    valtype hash2(20, 0x00);
    hash2[hash2.size() - 1] = OP_MINT_TRANSFER; // 0xba
    CScript witnessProgram2 = CScript() << OP_0 << hash2;
    txnouttype whichType2;
    std::vector<valtype> solutions2;
    bool ok2 = Solver(witnessProgram2, whichType2, solutions2);
    BOOST_CHECK(ok2);
    BOOST_CHECK_EQUAL(whichType2, TX_WITNESS_V0_KEYHASH);
}

// ---------------------------------------------------------------------------
// Shared UAP script-format fixture (src/test/data/uap_script_vectors.json).
//
// The same vector file is consumed by contrib/uap-indexer (Go) and
// contrib/uap-js (JavaScript), so all three implementations agree on exactly
// which byte sequences are UAP outputs. This is the C++ side.
//
// The fixture covers script *shape* only -- one script, parsed with no
// transaction around it. Everything that depends on the spending transaction
// (conservation, merging, per-lineage grouping, deriving a mint's origin from
// its outpoint) lives in the "lineage identity and merging" section above.
// Neither half describes the format on its own.
//
// ParseUapOutputScript() is not directly reachable from here, so rather than
// restate its rules (which would test the test), each vector is pushed through
// the real interpreter via two probes:
//
//   Probe A -- OP_INSPECT selector 11 ("output virtual balance"). The
//     interpreter calls ParseUapOutputScript on the named output and pushes
//     either 0 (not a UAP output) or nValue * multiplier. This recovers the
//     extracted multiplier, but cannot distinguish "not UAP" from "UAP with
//     multiplier 0" -- both push 0.
//
//   Probe B -- OP_MINT's one-shot rule (CheckUapOneShot). A mint spend fails
//     if any *sibling* input's prevout scriptPubKey parses as a UAP output.
//     Putting the vector there gives a clean accept/reject bit for every
//     vector, multiplier 0 included, so it is what pins down parse acceptance.
//
// Together: B decides parse/no-parse for every vector; A independently
// confirms the multiplier for the vectors where it is observable. Neither
// probe observes the origin, so the origin is checked structurally below,
// against the script's own third push.
// ---------------------------------------------------------------------------
namespace {

// The fixture is compiled in via the usual JSON_TEST_FILES route (see
// src/Makefile.test.include), so the test works in any build layout. The same
// file stays on disk because the Go indexer and JS builder suites read it
// directly -- it is the cross-implementation contract, not just a C++ input.
std::string ReadUapVectorFile()
{
    return std::string(json_tests::uap_script_vectors,
                       json_tests::uap_script_vectors + sizeof(json_tests::uap_script_vectors));
}

// A transaction whose vout[0] carries the candidate script, used as the
// subject of both probes.
CMutableTransaction TxWithOutput(const CScript& candidate, CAmount nValue)
{
    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vout.resize(1);
    tx.vout[0].nValue = nValue;
    tx.vout[0].scriptPubKey = candidate;
    return tx;
}

// Probe A: run "<0> <11> OP_INSPECT" and return the bytes it pushed.
// The interpreter pushes CScriptNum(balance).getvch(), so the caller compares
// against CScriptNum(expected).getvch() rather than re-deriving the encoding.
// Returns false only if the interpreter errored (e.g. the balance multiply
// overflowed), which no vector in this file is expected to trigger.
bool ProbeVirtualBalance(const CScript& candidate, CAmount nValue, valtype& balanceOut)
{
    CMutableTransaction tx = TxWithOutput(candidate, nValue);
    CScript probe = CScript() << (int64_t)0 << (int64_t)11 << OP_INSPECT;

    std::vector<valtype> stack;
    ScriptError err;
    if (!EvalScript(stack, probe, FLAGS, MutableTransactionSignatureChecker(&tx, 0, nValue), SIGVERSION_BASE, &err))
        return false;
    BOOST_REQUIRE_EQUAL(stack.size(), 1U);
    balanceOut = stack.back();
    return true;
}

// Probe B: does the interpreter consider `candidate` a UAP output?
// A valid mint is spent at input 0; `candidate` is input 1's prevout script.
// CheckUapOneShot rejects the spend iff `candidate` parses as UAP, so
// "spend succeeded" == "candidate is not a UAP output".
bool ProbeParsesAsUapOutput(const CScript& candidate)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const COutPoint mintPoint = Point(0x77);
    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2);
    spendTx.vin[0].prevout = mintPoint;
    spendTx.vin[1].prevout = Point(0x78);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), OriginOf(mintPoint), MULTIPLIER);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    const std::vector<CScript> prevScripts = {mintScript, candidate};
    const std::vector<CAmount> prevAmounts = {nValueIn, 5 * COIN};

    ScriptError err;
    return !VerifyUapInput(spendTx, 0, prevScripts, prevAmounts, err);
}

} // namespace

// Guard for probe B: with an ordinary non-UAP sibling the mint spend must
// succeed, otherwise "rejected" below would mean nothing.
BOOST_AUTO_TEST_CASE(uap_vectors_probe_baseline_is_sound)
{
    CKey k;
    k.MakeNewKey(true);
    CScript p2pkh = CScript() << OP_DUP << OP_HASH160 << ToByteVector(k.GetPubKey().GetID()) << OP_EQUALVERIFY << OP_CHECKSIG;
    BOOST_CHECK_MESSAGE(!ProbeParsesAsUapOutput(p2pkh), "P2PKH sibling must not trip the one-shot rule");

    // ...and a known-good covenant script must trip it, so the probe is not
    // simply always answering "no".
    BOOST_CHECK_MESSAGE(ProbeParsesAsUapOutput(TransferScript(k.GetPubKey(), OriginOf(Point(0x79)), MULTIPLIER)),
        "a real OP_MINT_TRANSFER sibling must trip the one-shot rule");
}

BOOST_AUTO_TEST_CASE(uap_script_vectors)
{
    UniValue tests = read_json(ReadUapVectorFile());
    BOOST_REQUIRE(tests.isArray());
    BOOST_REQUIRE_GT(tests.size(), 0);

    // Chosen so that nValue * INT32_MAX still fits an int64 comfortably, and
    // so that a wrong multiplier cannot coincidentally give the right product.
    const CAmount kProbeValue = 1234567;

    size_t validCount = 0, invalidCount = 0, multiplierChecked = 0, multiplierAmbiguous = 0;
    size_t mintCount = 0, transferCount = 0;

    for (size_t idx = 0; idx < tests.size(); idx++) {
        const UniValue& test = tests[idx];
        BOOST_REQUIRE_MESSAGE(test.exists("script") && test.exists("valid") && test.exists("comment"),
            "vector " << idx << ": missing required fields");

        const std::string comment = test["comment"].get_str();
        const bool expectValid = test["valid"].get_bool();
        const std::vector<unsigned char> scriptBytes = ParseHex(test["script"].get_str());
        const CScript candidate(scriptBytes.begin(), scriptBytes.end());
        const std::string where = "vector " + std::to_string(idx) + " (" + comment + ")";

        // --- Probe B: parse acceptance, authoritative for every vector. ---
        BOOST_CHECK_MESSAGE(ProbeParsesAsUapOutput(candidate) == expectValid,
            where << ": interpreter " << (expectValid ? "rejected" : "accepted")
                  << " a script the fixture calls " << (expectValid ? "valid" : "invalid"));

        if (!expectValid) {
            invalidCount++;
            // An unparseable script carries no tokens, so probe A must say 0.
            valtype balance;
            BOOST_CHECK_MESSAGE(ProbeVirtualBalance(candidate, kProbeValue, balance),
                where << ": OP_INSPECT errored on an invalid script");
            BOOST_CHECK_MESSAGE(balance == CScriptNum(0).getvch(),
                where << ": invalid script reported balance 0x" << HexStr(balance));
            continue;
        }

        validCount++;
        BOOST_REQUIRE_MESSAGE(test.exists("multiplier") && test.exists("pubkey")
                              && test.exists("is_mint") && test.exists("origin"),
            where << ": valid vector missing expected parse output");

        const int64_t expectMultiplier = test["multiplier"].get_int64();

        // --- Probe A: the multiplier the interpreter actually extracted. ---
        valtype balance;
        BOOST_REQUIRE_MESSAGE(ProbeVirtualBalance(candidate, kProbeValue, balance),
            where << ": OP_INSPECT errored on a valid script");
        const valtype expectBalance = CScriptNum(kProbeValue * expectMultiplier).getvch();
        BOOST_CHECK_MESSAGE(balance == expectBalance,
            where << ": virtual balance 0x" << HexStr(balance) << " != 0x" << HexStr(expectBalance)
                  << " (" << kProbeValue << " * " << expectMultiplier << ")");

        if (expectMultiplier == 0) {
            // Honest limitation: probe A pushes 0 for "not a UAP output" too,
            // so for these vectors the assertion above is only consistent
            // with, not evidence of, a successful parse. Probe B above is
            // what actually established that.
            multiplierAmbiguous++;
        } else {
            multiplierChecked++;
        }

        // The fixture's declared pubkey must be the literal leading push, so
        // the Go and JS consumers (which do expose the pubkey) are pinned to
        // the same bytes this script really contains.
        const std::vector<unsigned char> expectPubKey = ParseHex(test["pubkey"].get_str());
        BOOST_CHECK_MESSAGE(expectPubKey.size() == 33 || expectPubKey.size() == 65,
            where << ": declared pubkey is " << expectPubKey.size() << " bytes");
        CScript::const_iterator pc = candidate.begin();
        opcodetype op;
        valtype firstPush;
        BOOST_CHECK(candidate.GetOp(pc, op, firstPush));
        BOOST_CHECK_MESSAGE(firstPush == expectPubKey, where << ": leading push is not the declared pubkey");

        // A transfer carries a 32-byte origin between the multiplier and the
        // opcode; a mint carries nothing there. Distinguish them by the
        // terminating opcode, then check the declared origin really is the
        // script's third push -- neither probe can observe it, and the Go and
        // JS consumers read it straight out of the script.
        BOOST_REQUIRE(!scriptBytes.empty());
        const bool endsWithMint = scriptBytes.back() == OP_MINT;
        const bool declaredMint = test["is_mint"].get_bool();
        BOOST_CHECK_MESSAGE(endsWithMint == declaredMint,
            where << ": is_mint disagrees with the terminating opcode");

        const std::vector<unsigned char> expectOrigin = ParseHex(test["origin"].get_str());
        valtype multPush, originPush;
        BOOST_CHECK(candidate.GetOp(pc, op, multPush)); // skip the multiplier
        if (declaredMint) {
            mintCount++;
            BOOST_CHECK_MESSAGE(expectOrigin.empty(), where << ": a mint vector declares an origin");
            // ...and the script really does end right after the multiplier.
            BOOST_CHECK_MESSAGE(pc + 1 == candidate.end(),
                where << ": a mint vector has a push between the multiplier and OP_MINT");
        } else {
            transferCount++;
            BOOST_CHECK_MESSAGE(expectOrigin.size() == 32,
                where << ": a transfer vector declares a " << expectOrigin.size() << "-byte origin");
            BOOST_CHECK(candidate.GetOp(pc, op, originPush));
            BOOST_CHECK_MESSAGE(originPush == expectOrigin,
                where << ": third push 0x" << HexStr(originPush) << " is not the declared origin");
        }
    }

    BOOST_CHECK_EQUAL(validCount, 19U);
    BOOST_CHECK_EQUAL(invalidCount, 31U);
    BOOST_CHECK_EQUAL(mintCount, 7U);
    BOOST_CHECK_EQUAL(transferCount, 12U);
    // Five valid vectors declare multiplier 0; probe A cannot observe them.
    BOOST_CHECK_EQUAL(multiplierAmbiguous, 5U);
    BOOST_CHECK_EQUAL(multiplierChecked, 14U);
}

// Evaluate the push-only prefix of a UAP output script (everything before the
// terminating OP_MINT / OP_MINT_TRANSFER) under SCRIPT_VERIFY_MINIMALDATA.
// Returns false, with serror set, if any element uses a non-canonical push.
static bool PushPrefixIsMinimal(const CScript& script, ScriptError* serror)
{
    BOOST_REQUIRE(!script.empty());
    CScript prefix(script.begin(), script.end() - 1);
    std::vector<valtype> stack;
    return EvalScript(stack, prefix, SCRIPT_VERIFY_MINIMALDATA, BaseSignatureChecker(),
                      SIGVERSION_BASE, serror);
}

// The reason ParseUapOutputScript insists on canonical pushes: a covenant's
// scriptPubKey is *executed* when the position is spent, and standard relay
// policy applies SCRIPT_VERIFY_MINIMALDATA to that execution. If consensus
// accepted an encoding MINIMALDATA rejects, a position could be created that
// the chain honours but the network will not relay a spend of -- stuck value.
// So: every consensus-valid UAP output must be MINIMALDATA-clean.
BOOST_AUTO_TEST_CASE(uap_valid_vectors_are_minimaldata_clean)
{
    UniValue tests = read_json(ReadUapVectorFile());
    BOOST_REQUIRE(tests.isArray());

    size_t checked = 0;
    for (size_t idx = 0; idx < tests.size(); idx++) {
        const UniValue& test = tests[idx];
        if (!test["valid"].get_bool())
            continue;
        const std::vector<unsigned char> scriptBytes = ParseHex(test["script"].get_str());
        const CScript candidate(scriptBytes.begin(), scriptBytes.end());
        const std::string where = "vector " + std::to_string(idx) + " (" + test["comment"].get_str() + ")";

        ScriptError err = SCRIPT_ERR_OK;
        BOOST_CHECK_MESSAGE(PushPrefixIsMinimal(candidate, &err),
            where << ": consensus accepts this output, but executing it trips MINIMALDATA ("
                  << ScriptErrorString(err) << "), so no standard transaction could spend it");
        checked++;
    }
    BOOST_CHECK_EQUAL(checked, 19U);

    // Guard: the probe must be capable of failing. A multiplier of 1 written
    // as an explicit one-byte push is exactly the encoding consensus used to
    // accept and MINIMALDATA has always rejected.
    CKey k;
    k.MakeNewKey(true);
    CScript nonMinimal;
    nonMinimal << ToByteVector(k.GetPubKey());
    nonMinimal.push_back(0x01);
    nonMinimal.push_back(0x01); // multiplier 1 as a data push, not OP_1
    nonMinimal << OriginOf(Point(0x7a));
    nonMinimal.push_back(OP_MINT_TRANSFER);
    ScriptError err = SCRIPT_ERR_OK;
    BOOST_CHECK(!PushPrefixIsMinimal(nonMinimal, &err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_MINIMALDATA);
    // ...and consensus must now reject that same script, which is the fix.
    BOOST_CHECK(!ProbeParsesAsUapOutput(nonMinimal));
}

// A multiplier in 1..16 is the case the canonical-encoding rule exists for.
// Before the fix, the only encoding consensus accepted (a data push) was the
// one MINIMALDATA rejected, and vice versa, so such a position was
// untradeable. Both halves must now agree on OP_1..OP_16.
BOOST_AUTO_TEST_CASE(small_multipliers_are_usable_end_to_end)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    for (int m = 1; m <= 16; m++) {
        const std::string where = "multiplier " + std::to_string(m);
        const COutPoint mintPoint = Point(0xa0 + (unsigned char)m);
        const valtype origin = OriginOf(mintPoint);

        // OP_1..OP_16 is what MINIMALDATA demands...
        CScript covenant = TransferScript(recipient.GetPubKey(), origin, (int64_t)m);
        ScriptError err = SCRIPT_ERR_OK;
        BOOST_CHECK_MESSAGE(PushPrefixIsMinimal(covenant, &err),
            where << ": OP_" << m << " covenant trips MINIMALDATA (" << ScriptErrorString(err) << ")");

        // ...and consensus now recognizes it as a UAP output.
        BOOST_CHECK_MESSAGE(ProbeParsesAsUapOutput(covenant),
            where << ": consensus does not recognize an OP_" << m << " covenant");

        // A mint at this multiplier really does spend into that covenant.
        CScript mintScript = MintScript(minter.GetPubKey(), (int64_t)m);
        const CAmount nValueIn = 1000 * COIN;
        CMutableTransaction spendTx;
        spendTx.vin.resize(1);
        spendTx.vin[0].prevout = mintPoint;
        spendTx.vout.resize(1);
        spendTx.vout[0].nValue = nValueIn;
        spendTx.vout[0].scriptPubKey = covenant;
        spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);
        BOOST_CHECK_MESSAGE(VerifyUapInput(spendTx, 0, {mintScript}, {nValueIn}, err),
            where << ": mint into an OP_" << m << " covenant failed (" << ScriptErrorString(err) << ")");

        // The explicit data push, which used to be the *only* accepted form,
        // is now rejected -- there is exactly one encoding per multiplier.
        CScript nonCanonical;
        nonCanonical << ToByteVector(recipient.GetPubKey());
        nonCanonical.push_back(0x01);
        nonCanonical.push_back((unsigned char)m);
        nonCanonical << origin;
        nonCanonical.push_back(OP_MINT_TRANSFER);
        BOOST_CHECK_MESSAGE(!ProbeParsesAsUapOutput(nonCanonical),
            where << ": consensus still accepts the non-canonical data-push encoding");
    }
}

// OP_INSPECT selector 11 ("output virtual balance") computes
// nValue * multiplier as an int64_t (see the __builtin_mul_overflow guard
// fixed for 32-bit portability in a2b7cc9). A UAP output whose declared
// nValue and multiplier multiply past INT64_MAX must report the overflow
// rather than let it wrap into a bogus, silently-wrong balance -- this is
// the same overflow-guard family as CheckUapOutputConservation's summation
// guards, just for the one caller outside that function.
BOOST_AUTO_TEST_CASE(inspect_virtual_balance_reports_overflow)
{
    CKey holder;
    holder.MakeNewKey(true);

    // nValue at MAX_MONEY times a multiplier near MAX_UAP_MULTIPLIER
    // overflows int64_t (their product is far past INT64_MAX).
    const CAmount hugeValue = MAX_MONEY;
    CScript candidate = TransferScript(holder.GetPubKey(), OriginOf(Point(0xf6)), MAX_UAP_MULTIPLIER);

    CMutableTransaction tx;
    tx.vin.resize(1);
    tx.vout.resize(1);
    tx.vout[0].nValue = hugeValue;
    tx.vout[0].scriptPubKey = candidate;

    CScript probe = CScript() << (int64_t)0 << (int64_t)11 << OP_INSPECT;
    std::vector<valtype> stack;
    ScriptError err;
    BOOST_CHECK(!EvalScript(stack, probe, FLAGS, MutableTransactionSignatureChecker(&tx, 0, hugeValue), SIGVERSION_BASE, &err));
    BOOST_CHECK_EQUAL(err, SCRIPT_ERR_UAP_VALUE_OVERFLOW);
}

BOOST_AUTO_TEST_SUITE_END()
