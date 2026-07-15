// Copyright (c) 2013-2026 The Dogecoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "key.h"
#include "script/interpreter.h"
#include "script/script.h"
#include "script/script_error.h"
#include "uint256.h"

#include "test/test_bitcoin.h"

#include <boost/test/unit_test.hpp>

typedef std::vector<unsigned char> valtype;

BOOST_FIXTURE_TEST_SUITE(uap_mint_tests, BasicTestingSetup)

namespace {

const unsigned int FLAGS = SCRIPT_VERIFY_P2SH | SCRIPT_VERIFY_STRICTENC | SCRIPT_VERIFY_NULLFAIL;
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

BOOST_AUTO_TEST_SUITE_END()
