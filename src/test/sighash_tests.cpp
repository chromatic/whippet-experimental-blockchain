// Copyright (c) 2013-2016 The Bitcoin Core developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or http://www.opensource.org/licenses/mit-license.php.

#include "consensus/validation.h"
#include "data/sighash.json.h"
#include "hash.h"
#include "validation.h" // For CheckTransaction
#include "script/interpreter.h"
#include "script/script.h"
#include "serialize.h"
#include "streams.h"
#include "test/test_bitcoin.h"
#include "util.h"
#include "utilstrencodings.h"
#include "version.h"

#include <iostream>

#include <boost/test/unit_test.hpp>

#include <univalue.h>

extern UniValue read_json(const std::string& jsondata);

// Old script.cpp SignatureHash function
uint256 static SignatureHashOld(CScript scriptCode, const CTransaction& txTo, unsigned int nIn, int nHashType)
{
    static const uint256 one(uint256S("0000000000000000000000000000000000000000000000000000000000000001"));
    if (nIn >= txTo.vin.size())
    {
        printf("ERROR: SignatureHash(): nIn=%d out of range\n", nIn);
        return one;
    }
    CMutableTransaction txTmp(txTo);

    // In case concatenating two scripts ends up with two codeseparators,
    // or an extra one at the end, this prevents all those possible incompatibilities.
    scriptCode.FindAndDelete(CScript(OP_CODESEPARATOR));

    // Blank out other inputs' signatures
    for (unsigned int i = 0; i < txTmp.vin.size(); i++)
        txTmp.vin[i].scriptSig = CScript();
    txTmp.vin[nIn].scriptSig = scriptCode;

    // Blank out some of the outputs
    if ((nHashType & 0x1f) == SIGHASH_NONE)
    {
        // Wildcard payee
        txTmp.vout.clear();

        // Let the others update at will
        for (unsigned int i = 0; i < txTmp.vin.size(); i++)
            if (i != nIn)
                txTmp.vin[i].nSequence = 0;
    }
    else if ((nHashType & 0x1f) == SIGHASH_SINGLE)
    {
        // Only lock-in the txout payee at same index as txin
        unsigned int nOut = nIn;
        if (nOut >= txTmp.vout.size())
        {
            printf("ERROR: SignatureHash(): nOut=%d out of range\n", nOut);
            return one;
        }
        txTmp.vout.resize(nOut+1);
        for (unsigned int i = 0; i < nOut; i++)
            txTmp.vout[i].SetNull();

        // Let the others update at will
        for (unsigned int i = 0; i < txTmp.vin.size(); i++)
            if (i != nIn)
                txTmp.vin[i].nSequence = 0;
    }

    // Blank out other inputs completely, not recommended for open transactions
    if (nHashType & SIGHASH_ANYONECANPAY)
    {
        txTmp.vin[0] = txTmp.vin[nIn];
        txTmp.vin.resize(1);
    }

    // Serialize and hash
    CHashWriter ss(SER_GETHASH, SERIALIZE_TRANSACTION_NO_WITNESS);
    ss << txTmp << nHashType;
    return ss.GetHash();
}

void static RandomScript(CScript &script) {
    static const opcodetype oplist[] = {OP_FALSE, OP_1, OP_2, OP_3, OP_CHECKSIG, OP_IF, OP_VERIF, OP_RETURN, OP_CODESEPARATOR};
    script = CScript();
    int ops = (InsecureRandRange(10));
    for (int i=0; i<ops; i++)
        script << oplist[InsecureRandRange(sizeof(oplist)/sizeof(oplist[0]))];
}

void static RandomTransaction(CMutableTransaction &tx, bool fSingle) {
    tx.nVersion = InsecureRand32();
    tx.vin.clear();
    tx.vout.clear();
    tx.nLockTime = (InsecureRandBool()) ? InsecureRand32() : 0;
    int ins = (InsecureRandBits(2)) + 1;
    int outs = fSingle ? ins : (InsecureRandBits(2)) + 1;
    for (int in = 0; in < ins; in++) {
        tx.vin.push_back(CTxIn());
        CTxIn &txin = tx.vin.back();
        txin.prevout.hash = InsecureRand256();
        txin.prevout.n = InsecureRandBits(2);
        RandomScript(txin.scriptSig);
        txin.nSequence = (InsecureRandBool()) ? InsecureRand32() : (unsigned int)-1;
    }
    for (int out = 0; out < outs; out++) {
        tx.vout.push_back(CTxOut());
        CTxOut &txout = tx.vout.back();
        txout.nValue = InsecureRandRange(100000000);
        RandomScript(txout.scriptPubKey);
    }
}

BOOST_FIXTURE_TEST_SUITE(sighash_tests, BasicTestingSetup)

BOOST_AUTO_TEST_CASE(sighash_test)
{
    SeedInsecureRand(false);

    #if defined(PRINT_SIGHASH_JSON)
    std::cout << "[\n";
    std::cout << "\t[\"raw_transaction, script, input_index, hashType, signature_hash (result)\"],\n";
    #endif
    int nRandomTests = 50000;

    #if defined(PRINT_SIGHASH_JSON)
    nRandomTests = 500;
    #endif
    for (int i=0; i<nRandomTests; i++) {
        int nHashType = InsecureRand32();
        CMutableTransaction txTo;
        RandomTransaction(txTo, (nHashType & 0x1f) == SIGHASH_SINGLE);
        CScript scriptCode;
        RandomScript(scriptCode);
        int nIn = InsecureRandRange(txTo.vin.size());

        uint256 sh, sho;
        sho = SignatureHashOld(scriptCode, txTo, nIn, nHashType);
        sh = SignatureHash(scriptCode, txTo, nIn, nHashType, 0, SIGVERSION_BASE);
        #if defined(PRINT_SIGHASH_JSON)
        CDataStream ss(SER_NETWORK, PROTOCOL_VERSION);
        ss << txTo;

        std::cout << "\t[\"" ;
        std::cout << HexStr(ss.begin(), ss.end()) << "\", \"";
        std::cout << HexStr(scriptCode) << "\", ";
        std::cout << nIn << ", ";
        std::cout << nHashType << ", \"";
        std::cout << sho.GetHex() << "\"]";
        if (i+1 != nRandomTests) {
          std::cout << ",";
        }
        std::cout << "\n";
        #endif
        BOOST_CHECK(sh == sho);
    }
    #if defined(PRINT_SIGHASH_JSON)
    std::cout << "]\n";
    #endif
}

// Goal: check that SignatureHash generates correct hash
BOOST_AUTO_TEST_CASE(sighash_from_data)
{
    UniValue tests = read_json(std::string(json_tests::sighash, json_tests::sighash + sizeof(json_tests::sighash)));

    for (unsigned int idx = 0; idx < tests.size(); idx++) {
        UniValue test = tests[idx];
        std::string strTest = test.write();
        if (test.size() < 1) // Allow for extra stuff (useful for comments)
        {
            BOOST_ERROR("Bad test: " << strTest);
            continue;
        }
        if (test.size() == 1) continue; // comment

        std::string raw_tx, raw_script, sigHashHex;
        int nIn, nHashType;
        uint256 sh;
        CTransactionRef tx;
        CScript scriptCode = CScript();

        try {
          // deserialize test data
          raw_tx = test[0].get_str();
          raw_script = test[1].get_str();
          nIn = test[2].get_int();
          nHashType = test[3].get_int();
          sigHashHex = test[4].get_str();

          CDataStream stream(ParseHex(raw_tx), SER_NETWORK, PROTOCOL_VERSION);
          stream >> tx;

          CValidationState state;
          BOOST_CHECK_MESSAGE(CheckTransaction(*tx, state), strTest);
          BOOST_CHECK(state.IsValid());

          std::vector<unsigned char> raw = ParseHex(raw_script);
          scriptCode.insert(scriptCode.end(), raw.begin(), raw.end());
        } catch (...) {
          BOOST_ERROR("Bad test, couldn't deserialize data: " << strTest);
          continue;
        }
        sh = SignatureHash(scriptCode, *tx, nIn, nHashType, 0, SIGVERSION_BASE);
        const std::string hash_hex = sh.GetHex();
        if (hash_hex != sigHashHex) {
            std::cerr << "sighash mismatch expected=" << sigHashHex << " actual=" << hash_hex << std::endl;
        }
        BOOST_CHECK_MESSAGE(hash_hex == sigHashHex, strTest << " got " << hash_hex);
    }
}

// sighash.json (500 vectors above) is entirely the product of RandomScript
// and RandomTransaction, and both generators have blind spots that no amount
// of re-running closes: RandomScript only ever emits the nine single-byte
// opcodes in its oplist, so it can never produce a data push, let alone a
// push whose length prefix outruns the bytes that follow it; and
// RandomTransaction always hands SIGHASH_SINGLE a same-sized vout array, so
// nIn is never simultaneously "a valid input" and "not a valid output".
// Those two blind spots are exactly the two divergent branches in uap.js
// (contrib/uap-js/uap.js): SIGHASH_DEGENERATE_HASH and the scriptSigLength
// override in serializeScriptCode. This case hand-builds the handful of
// transactions needed to reach both, plus a few neighbouring cases that are
// cheap to add and were not obviously implied by the other two. It is
// deliberately not random: the value of this fixture is that a human chose
// each byte, not that it has statistical coverage.
//
// Which hash function is recorded: the 500-vector generator above records
// SignatureHashOld's result, because historically that was the function
// being cross-checked. SignatureHashOld prints "ERROR: SignatureHash():
// nIn=... out of range" (or the SIGHASH_SINGLE equivalent) to stdout via a
// bare printf whenever nIn or nOut is out of range -- precisely the two
// conditions vectors (a) and (b) below exist to hit -- which would land
// inside the JSON this test prints and corrupt it. So this case records
// SignatureHash() (the production function actually shipped in
// src/script/interpreter.cpp) directly, and separately asserts it agrees
// with SignatureHashOld() for every vector where that comparison is
// meaningful. That is not simply "everywhere but (a) and (b)": vectors (c)
// and (h), the truncated-push cases, turned out under this cross-check to
// be a second class of case where the two functions are expected to
// disagree, not just unsafe to compare (see the per-vector comments below).
// So each vector records for itself whether the comparison applies.
//
// Regeneration recipe: uncomment the #define below, rebuild
// (`make -C src test/test_whippet -j8`), run
// `./src/test/test_whippet --run_test=sighash_tests/sighash_uap_edge_cases`,
// take the stdout between (and including) the enclosing `[` and `]`, and
// write it verbatim to src/test/data/sighash_uap.json. Then remove the
// #define and rebuild again so the checked-in source has the generator
// dormant, matching PRINT_SIGHASH_JSON above. This file is deliberately not
// listed in JSON_TEST_FILES in src/Makefile.test.include -- adding it there
// would require regenerating automake's build files, which is out of scope
// here -- so nothing in this test suite re-verifies the committed JSON
// against the generator that produced it. src/test/data/sighash_uap.json is
// only ever as current as the last time someone ran this recipe by hand.
// #define PRINT_UAP_SIGHASH_JSON
BOOST_AUTO_TEST_CASE(sighash_uap_edge_cases)
{
    struct Vector {
        std::string label;
        CMutableTransaction tx;
        CScript scriptCode;
        unsigned int nIn;
        int hashType;
        // False for the two vectors that would make SignatureHashOld's own
        // printf fire (see the comment above); true everywhere else, where
        // SignatureHashOld is well-defined and used as a second oracle.
        bool checkAgainstOld;
    };

    // A single fixed input/output pair, reused as the base for every vector
    // below so the only thing that differs between them is the thing each
    // vector exists to test.
    auto baseTx = [](unsigned int nInputs, unsigned int nOutputs) {
        CMutableTransaction tx;
        tx.nVersion = 1;
        tx.nLockTime = 0;
        for (unsigned int i = 0; i < nInputs; i++) {
            CTxIn in;
            in.prevout.hash = uint256S(strprintf("%064x", 0x11 * (i + 1)));
            in.prevout.n = i;
            in.scriptSig = CScript();
            in.nSequence = 0xffffffff;
            tx.vin.push_back(in);
        }
        for (unsigned int i = 0; i < nOutputs; i++) {
            CTxOut out;
            out.nValue = 1000 * (i + 1);
            out.scriptPubKey = CScript() << OP_DUP << OP_HASH160 << OP_EQUALVERIFY << OP_CHECKSIG;
            tx.vout.push_back(out);
        }
        return tx;
    };

    std::vector<Vector> vectors;

    // (a) nIn == tx.vin.size(): the classic out-of-range index. Production
    // SignatureHash short-circuits to the degenerate hash before it ever
    // looks at vin[nIn], so a single input is enough.
    vectors.push_back({"a: nIn out of range", baseTx(1, 1), CScript() << OP_1, /*nIn=*/1, SIGHASH_ALL, false});

    // (b) SIGHASH_SINGLE with nIn >= tx.vout.size(): nIn is a valid input
    // index (there are two inputs) but there is only one output, so the
    // "lock in the matching output" rule has nothing to lock onto.
    vectors.push_back({"b: SIGHASH_SINGLE with no matching output", baseTx(2, 1), CScript() << OP_1, /*nIn=*/1, SIGHASH_SINGLE, false});

    // (c) scriptCode ending in a truncated OP_PUSHDATA1: the length byte
    // (0x05) claims five bytes of payload but only two (0xaa 0xbb) follow.
    // CScript::GetOp2 stops parsing right after the length byte without
    // consuming the short payload, so SerializeScriptCode's two walks
    // disagree: the byte-count walk (scriptCode.size() - nCodeSeparators)
    // sees the whole 5-byte script and writes a length prefix of 5, while
    // the emission walk stops at the same place GetOp2 gave up and writes
    // only the 3 bytes it actually consumed (51 4c 05). The two trailing
    // bytes are silently dropped from the preimage despite being counted in
    // its declared length.
    //
    // checkAgainstOld is false here, and this is a real, expected divergence
    // discovered while writing this vector, not an oversight: CScript::
    // FindAndDelete (what SignatureHashOld uses to strip OP_CODESEPARATOR)
    // finds zero literal 0xab bytes in this script, leaves it completely
    // untouched, and SignatureHashOld then serializes it normally -- a
    // length prefix of 5 followed by all 5 bytes, 51 4c 05 aa bb. Production
    // SignatureHash serializes the same bytes as a length prefix of 5
    // followed by only 3 bytes, 51 4c 05. Those are two different byte
    // strings, so the hashes are two different hashes: this is precisely the
    // quirk in CTransactionSignatureSerializer::SerializeScriptCode that
    // uap.js's scriptSigLength override exists to reproduce, and asserting
    // sh == sho here would be asserting the bug this vector is meant to
    // catch does not exist.
    {
        CScript truncated;
        truncated << OP_1;
        std::vector<unsigned char> tail = {0x4c, 0x05, 0xaa, 0xbb};
        truncated.insert(truncated.end(), tail.begin(), tail.end());
        vectors.push_back({"c: scriptCode ends in a truncated OP_PUSHDATA1", baseTx(1, 1), truncated, /*nIn=*/0, SIGHASH_ALL, false});
    }

    // (d) scriptCode consisting of nothing but OP_CODESEPARATORs: every byte
    // is stripped, so both the declared length and the emitted bytes are
    // zero. This is the degenerate end of the ordinary (non-truncated)
    // codeseparator-stripping path exercised by sighash.json, included here
    // because sighash.json's random scripts only ever hit a handful of
    // codeseparators, never a script that is only codeseparators.
    vectors.push_back({"d: scriptCode is only OP_CODESEPARATORs", baseTx(1, 1), CScript() << OP_CODESEPARATOR << OP_CODESEPARATOR << OP_CODESEPARATOR, /*nIn=*/0, SIGHASH_ALL, true});

    // (e) hashType == 0: not SIGHASH_ALL/NONE/SINGLE, not ANYONECANPAY. The
    // node's (nHashType & 0x1f) switch has no case for 0, so it falls
    // through and is treated exactly like SIGHASH_ALL for output/sequence
    // handling, but the raw value 0 (not 1) is still what gets serialized
    // into the last four bytes of the preimage. A caller that normalizes
    // hashType to SIGHASH_ALL before hashing, instead of passing it through
    // verbatim, would pass every other vector here and fail only this one.
    vectors.push_back({"e: hashType == 0", baseTx(1, 1), CScript() << OP_1, /*nIn=*/0, 0, true});

    // (f) SIGHASH_ANYONECANPAY with nIn > 0: the signed input is not vin[0],
    // so this is the case that catches an implementation that always moves
    // vin[0] into the truncated one-input list instead of moving vin[nIn].
    vectors.push_back({"f: SIGHASH_ANYONECANPAY with nIn > 0", baseTx(3, 1), CScript() << OP_1, /*nIn=*/1, SIGHASH_ALL | SIGHASH_ANYONECANPAY, true});

    // (g) SIGHASH_SINGLE | SIGHASH_ANYONECANPAY together, with nIn in range
    // for both: neither (b) nor (f) alone exercises the interaction between
    // "only serialize the signed input" and "only serialize the matching
    // output", and RandomTransaction/RandomScript's blind spots mean
    // sighash.json cannot be trusted to have hit this combination either
    // (it never gives SIGHASH_SINGLE a chance to be out of range, and it
    // pairs hashType bits with vin/vout shape purely by chance).
    vectors.push_back({"g: SIGHASH_SINGLE | SIGHASH_ANYONECANPAY", baseTx(3, 3), CScript() << OP_1, /*nIn=*/1, SIGHASH_SINGLE | SIGHASH_ANYONECANPAY, true});

    // (h) a truncated push that also has a real OP_CODESEPARATOR before it:
    // (c) alone has zero codeseparators, so it never exercises the
    // subtraction of nCodeSeparators from scriptCode.size() at the same time
    // as the emission walk stopping short. Here the declared length is
    // scriptCode.size() - 1 (one separator), while the emitted bytes stop at
    // the truncated push, so declared length, emitted-byte count, and
    // scriptCode.size() are all three different numbers. checkAgainstOld is
    // false for the same reason as (c): FindAndDelete matches the single
    // literal 0xab byte here (the OP_CODESEPARATOR), which is a real match,
    // so SignatureHashOld's script is one byte shorter than the original --
    // but it still writes every remaining byte, including the two bytes
    // production drops after the truncated push. The two implementations
    // are working from a different notion of "the script with separators
    // removed" and are not expected to agree.
    {
        CScript truncated;
        truncated << OP_1 << OP_CODESEPARATOR << OP_2;
        std::vector<unsigned char> tail = {0x4d, 0x03, 0x00, 0xaa};
        truncated.insert(truncated.end(), tail.begin(), tail.end());
        vectors.push_back({"h: OP_CODESEPARATOR followed by a truncated OP_PUSHDATA2", baseTx(1, 1), truncated, /*nIn=*/0, SIGHASH_ALL, false});
    }

    #if defined(PRINT_UAP_SIGHASH_JSON)
    std::cout << "[\n";
    std::cout << "\t[\"raw_transaction, script, input_index, hashType, signature_hash (result)\"],\n";
    #endif

    for (size_t i = 0; i < vectors.size(); i++) {
        const Vector& v = vectors[i];
        const CTransaction txTo(v.tx);
        uint256 sh = SignatureHash(v.scriptCode, txTo, v.nIn, v.hashType, 0, SIGVERSION_BASE);

        if (v.checkAgainstOld) {
            uint256 sho = SignatureHashOld(v.scriptCode, txTo, v.nIn, v.hashType);
            BOOST_CHECK_MESSAGE(sh == sho, v.label << ": SignatureHash and SignatureHashOld disagree");
        }

        #if defined(PRINT_UAP_SIGHASH_JSON)
        CDataStream ss(SER_NETWORK, PROTOCOL_VERSION);
        ss << txTo;

        std::cout << "\t[\"";
        std::cout << HexStr(ss.begin(), ss.end()) << "\", \"";
        std::cout << HexStr(v.scriptCode) << "\", ";
        std::cout << v.nIn << ", ";
        std::cout << v.hashType << ", \"";
        std::cout << sh.GetHex() << "\"]";
        if (i + 1 != vectors.size()) {
          std::cout << ",";
        }
        std::cout << "\n";
        #endif
    }

    #if defined(PRINT_UAP_SIGHASH_JSON)
    std::cout << "]\n";
    #endif
}
BOOST_AUTO_TEST_SUITE_END()
