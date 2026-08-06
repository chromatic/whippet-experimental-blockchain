// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "key.h"
#include "script/interpreter.h"
#include "script/script.h"
#include "script/script_error.h"
#include "script/standard.h"
#include "uint256.h"
#include "utilstrencodings.h"

#include "data/uap_script_vectors.json.h"

#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>
#include <univalue.h>

#include <limits>

extern UniValue read_json(const std::string& jsondata);

typedef std::vector<unsigned char> valtype;

BOOST_FIXTURE_TEST_SUITE(uap_mint_tests, BasicTestingSetup)

namespace {

const unsigned int FLAGS = SCRIPT_VERIFY_P2SH | SCRIPT_VERIFY_STRICTENC | SCRIPT_VERIFY_NULLFAIL | SCRIPT_VERIFY_UAP_MINT;
const int64_t MULTIPLIER = 1000;
const valtype SALT(16, 0x42);

CScript MintScript(const CPubKey& pubkey, int64_t multiplier = MULTIPLIER, const valtype& salt = SALT)
{
    return CScript() << ToByteVector(pubkey) << multiplier << salt << OP_MINT;
}

CScript TransferScript(const CPubKey& pubkey, int64_t multiplier = MULTIPLIER)
{
    return CScript() << ToByteVector(pubkey) << multiplier << OP_MINT_TRANSFER;
}

CScript SignSpend(const CScript& scriptCode, const CKey& key, const CMutableTransaction& txTo, unsigned int nIn)
{
    uint256 hash = SignatureHash(scriptCode, txTo, nIn, SIGHASH_ALL, 0, SIGVERSION_BASE);
    valtype vchSig;
    BOOST_CHECK(key.Sign(hash, vchSig));
    vchSig.push_back((unsigned char)SIGHASH_ALL);
    return CScript() << vchSig;
}

} // namespace

// A mint output can only be spent by a signature from its declared
// recipient, and must forward its full value into a conforming
// OP_MINT_TRANSFER covenant with the same multiplier.
BOOST_AUTO_TEST_CASE(mint_then_transfer_succeeds_with_correct_signature_and_covenant)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
}

BOOST_AUTO_TEST_CASE(mint_rejects_wrong_signer)
{
    CKey minter, impostor, recipient;
    minter.MakeNewKey(true);
    impostor.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    // Signed by someone other than the pubkey embedded in the mint script.
    spendTx.vin[0].scriptSig = SignSpend(mintScript, impostor, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

BOOST_AUTO_TEST_CASE(mint_rejects_entry_fee_below_threshold)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 999 * COIN; // below the 1000 coin entry fee

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

BOOST_AUTO_TEST_CASE(mint_rejects_non_covenant_output)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    // Plain P2PK output instead of an OP_MINT_TRANSFER covenant: this would
    // silently destroy the token accounting if allowed.
    spendTx.vout[0].scriptPubKey = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG;

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

BOOST_AUTO_TEST_CASE(mint_rejects_multiplier_mismatch_in_output)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey(), MULTIPLIER);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    // Covenant output declares a different multiplier than the mint input.
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), MULTIPLIER * 2);

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

BOOST_AUTO_TEST_CASE(mint_rejects_value_created_out_of_thin_air)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    // Output value exceeds the input's value: not conservative.
    spendTx.vout[0].nValue = nValueIn + COIN;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

// A transfer output spends the same way a mint output does, minus the
// entry-fee/salt requirements, and continues to require conservation.
BOOST_AUTO_TEST_CASE(transfer_chain_continues_correctly)
{
    CKey holder, nextHolder;
    holder.MakeNewKey(true);
    nextHolder.MakeNewKey(true);

    CScript transferScript = TransferScript(holder.GetPubKey());
    const CAmount nValueIn = 10 * COIN; // below mint's 1000-coin floor; fine for transfers

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn - 1000; // small fee
    spendTx.vout[0].scriptPubKey = TransferScript(nextHolder.GetPubKey());

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, transferScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
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

    CScript mintScript = MintScript(minter.GetPubKey());
    CScript otherUapScript = TransferScript(otherHolder.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);
    // vin[1] purports to spend an unrelated UAP transfer output alongside the mint.

    std::vector<CScript> prevScripts;
    prevScripts.push_back(mintScript);
    prevScripts.push_back(otherUapScript);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn, &prevScripts), &err);
    BOOST_CHECK(!ok);
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

    CScript transferScript = TransferScript(holder.GetPubKey());
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2); // token input (0) + an unrelated plain payment input (1)
    spendTx.vout.resize(2);
    spendTx.vout[0].nValue = nValueIn; // continuing covenant, full value
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());
    spendTx.vout[1].nValue = 5 * COIN; // ordinary payment output, not a covenant
    spendTx.vout[1].scriptPubKey = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG;

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, transferScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
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

    CScript transferScript = TransferScript(holder.GetPubKey());
    const CAmount nValueIn = 500 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn - 1000;
    spendTx.vout[0].scriptPubKey = CScript() << ToByteVector(recipient.GetPubKey()) << OP_CHECKSIG; // not a covenant

    spendTx.vin[0].scriptSig = SignSpend(transferScript, holder, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, transferScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

// Two independent lineages (different holders, already past their initial
// mint -- i.e. spending OP_MINT_TRANSFER inputs, so the mint-only one-shot
// rule doesn't apply and this isolates the conservation math specifically)
// cannot be merged into one combined output, even at the same multiplier.
// CheckUapOutputConservation runs once per UAP input and each run
// independently demands the tx's total same-multiplier covenant output
// value fit within *that* input's own value -- so a combined output large
// enough to satisfy one input's ceiling necessarily violates the other's.
// This is what gives same-multiplier positions scarce, per-lineage
// identity without any separate token-ID bookkeeping: distinct lineages
// can never be diluted together.
BOOST_AUTO_TEST_CASE(transfer_rejects_merging_independent_lineages)
{
    CKey holderA, holderB, attacker;
    holderA.MakeNewKey(true);
    holderB.MakeNewKey(true);
    attacker.MakeNewKey(true);

    CScript transferScriptA = TransferScript(holderA.GetPubKey());
    CScript transferScriptB = TransferScript(holderB.GetPubKey());
    const CAmount valueA = 5000 * COIN;
    const CAmount valueB = 5000 * COIN;

    CMutableTransaction mergeTx;
    mergeTx.vin.resize(2);
    mergeTx.vout.resize(1);
    mergeTx.vout[0].nValue = valueA + valueB - 1000; // combined, minus fee
    mergeTx.vout[0].scriptPubKey = TransferScript(attacker.GetPubKey());

    mergeTx.vin[0].scriptSig = SignSpend(transferScriptA, holderA, mergeTx, 0);
    mergeTx.vin[1].scriptSig = SignSpend(transferScriptB, holderB, mergeTx, 1);

    ScriptError err;
    bool okA = VerifyScript(mergeTx.vin[0].scriptSig, transferScriptA, NULL, FLAGS,
        MutableTransactionSignatureChecker(&mergeTx, 0, valueA), &err);
    BOOST_CHECK(!okA);

    bool okB = VerifyScript(mergeTx.vin[1].scriptSig, transferScriptB, NULL, FLAGS,
        MutableTransactionSignatureChecker(&mergeTx, 1, valueB), &err);
    BOOST_CHECK(!okB);
}

// Sanity check that the merge rejection above is really about combining
// two lineages, not some incidental multi-input breakage: each position
// can still be spent independently (as its own single-input transaction).
BOOST_AUTO_TEST_CASE(transfer_solo_spend_of_either_lineage_still_works)
{
    CKey holderA, recipient;
    holderA.MakeNewKey(true);
    recipient.MakeNewKey(true);

    CScript transferScriptA = TransferScript(holderA.GetPubKey());
    const CAmount valueA = 5000 * COIN;

    CMutableTransaction soloTx;
    soloTx.vin.resize(1);
    soloTx.vout.resize(1);
    soloTx.vout[0].nValue = valueA - 1000;
    soloTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());

    soloTx.vin[0].scriptSig = SignSpend(transferScriptA, holderA, soloTx, 0);

    ScriptError err;
    bool ok = VerifyScript(soloTx.vin[0].scriptSig, transferScriptA, NULL, FLAGS,
        MutableTransactionSignatureChecker(&soloTx, 0, valueA), &err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
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

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey());
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    const unsigned int flagsWithoutUapMint = FLAGS & ~SCRIPT_VERIFY_UAP_MINT;

    ScriptError errDisabled;
    bool okDisabled = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, flagsWithoutUapMint,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &errDisabled);
    BOOST_CHECK(!okDisabled);
    BOOST_CHECK_EQUAL(errDisabled, SCRIPT_ERR_BAD_OPCODE);

    // The exact same transaction succeeds once the flag is set (FLAGS
    // already includes it) -- confirming the rejection above was
    // specifically about the flag, not some other defect in the fixture.
    ScriptError errEnabled;
    bool okEnabled = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &errEnabled);
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

    const int64_t hugeMultiplier = int64_t(std::numeric_limits<int32_t>::max()) + 1;
    CScript mintScript = MintScript(minter.GetPubKey(), hugeMultiplier);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), hugeMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK(!ok);
}

// The exact boundary value (INT32_MAX itself) must still be accepted --
// confirming the previous test rejects specifically for exceeding the
// bound, not multipliers in general once they get large.
BOOST_AUTO_TEST_CASE(mint_accepts_multiplier_at_int32_max)
{
    CKey minter, recipient;
    minter.MakeNewKey(true);
    recipient.MakeNewKey(true);

    const int64_t maxMultiplier = std::numeric_limits<int32_t>::max();
    CScript mintScript = MintScript(minter.GetPubKey(), maxMultiplier);
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(1);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), maxMultiplier);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    ScriptError err;
    bool ok = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err);
    BOOST_CHECK_MESSAGE(ok, ScriptErrorString(err));
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
// ParseUapOutputScript() is file-static in interpreter.cpp and cannot be
// called directly. Rather than restate its rules (which would test the test),
// each vector is pushed through the real interpreter via two probes:
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
// confirms the multiplier for the vectors where it is observable.
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

    CScript mintScript = MintScript(minter.GetPubKey());
    const CAmount nValueIn = 1000 * COIN;

    CMutableTransaction spendTx;
    spendTx.vin.resize(2);
    spendTx.vout.resize(1);
    spendTx.vout[0].nValue = nValueIn;
    spendTx.vout[0].scriptPubKey = TransferScript(recipient.GetPubKey(), MULTIPLIER);
    spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);

    std::vector<CScript> prevScripts;
    prevScripts.push_back(mintScript);
    prevScripts.push_back(candidate);

    ScriptError err;
    bool spendOk = VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
        MutableTransactionSignatureChecker(&spendTx, 0, nValueIn, &prevScripts), &err);
    return !spendOk;
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
    BOOST_CHECK_MESSAGE(ProbeParsesAsUapOutput(TransferScript(k.GetPubKey(), MULTIPLIER)),
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
        BOOST_REQUIRE_MESSAGE(test.exists("multiplier") && test.exists("pubkey") && test.exists("is_mint"),
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

        // A fresh mint has a salt push between the multiplier and the opcode;
        // a transfer does not. Distinguish them by the terminating opcode.
        BOOST_REQUIRE(!scriptBytes.empty());
        const bool endsWithMint = scriptBytes.back() == OP_MINT;
        BOOST_CHECK_MESSAGE(endsWithMint == test["is_mint"].get_bool(),
            where << ": is_mint disagrees with the terminating opcode");
    }

    BOOST_CHECK_EQUAL(validCount, 17U);
    BOOST_CHECK_EQUAL(invalidCount, 25U);
    // Five valid vectors declare multiplier 0; probe A cannot observe them.
    BOOST_CHECK_EQUAL(multiplierAmbiguous, 5U);
    BOOST_CHECK_EQUAL(multiplierChecked, 12U);
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
    BOOST_CHECK_EQUAL(checked, 17U);

    // Guard: the probe must be capable of failing. A multiplier of 1 written
    // as an explicit one-byte push is exactly the encoding consensus used to
    // accept and MINIMALDATA has always rejected.
    const std::vector<unsigned char> nonMinimalBytes = ParseHex(
        "21020102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f200101ba");
    const CScript nonMinimal(nonMinimalBytes.begin(), nonMinimalBytes.end());
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

        // OP_1..OP_16 is what MINIMALDATA demands...
        CScript covenant = TransferScript(recipient.GetPubKey(), (int64_t)m);
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
        spendTx.vout.resize(1);
        spendTx.vout[0].nValue = nValueIn;
        spendTx.vout[0].scriptPubKey = covenant;
        spendTx.vin[0].scriptSig = SignSpend(mintScript, minter, spendTx, 0);
        BOOST_CHECK_MESSAGE(VerifyScript(spendTx.vin[0].scriptSig, mintScript, NULL, FLAGS,
                                MutableTransactionSignatureChecker(&spendTx, 0, nValueIn), &err),
            where << ": mint into an OP_" << m << " covenant failed (" << ScriptErrorString(err) << ")");

        // The explicit data push, which used to be the *only* accepted form,
        // is now rejected -- there is exactly one encoding per multiplier.
        CScript nonCanonical;
        nonCanonical << ToByteVector(recipient.GetPubKey());
        nonCanonical.push_back(0x01);
        nonCanonical.push_back((unsigned char)m);
        nonCanonical.push_back(OP_MINT_TRANSFER);
        BOOST_CHECK_MESSAGE(!ProbeParsesAsUapOutput(nonCanonical),
            where << ": consensus still accepts the non-canonical data-push encoding");
    }
}

BOOST_AUTO_TEST_SUITE_END()
