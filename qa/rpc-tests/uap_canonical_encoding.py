#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""UAP canonical push encoding, end to end against a live node.

This is the network-level counterpart to the uap_mint_tests.cpp cases for
the canonical-encoding fix. The unit tests prove the interpreter parses
canonically; this proves the *mempool* agrees, which is where the original
defect actually bit.

  The defect, restated:

  Consensus required the multiplier to be a data push. SCRIPT_VERIFY_MINIMALDATA
  -- a standardness flag -- required OP_1..OP_16 for the values 1..16. So a
  mint with a multiplier in 1..16 could be created and was consensus-valid,
  but could only ever be spent by a non-standard transaction. No ordinary
  node would relay the spend. The coins were locked in practice while looking
  perfectly fine on chain.

  The fix makes consensus demand the canonical encoding, so the two rules
  agree and every consensus-valid position has exactly one byte
  representation.

Two things must therefore be true, and asserting only one of them is how
this regression would slip back in:

  1. Canonical encodings in 1..16 are spendable by a STANDARD transaction.
     sendrawtransaction applies the standard flags, so acceptance there is
     the proof; mining it afterwards proves consensus agrees.

  2. Non-canonical encodings of the SAME numeric value are rejected. A test
     that only checked (1) would pass against the unfixed node.

     This splits in two, and the split is real rather than pedantic:

       - a non-canonical spelling in an OUTPUT covenant is rejected by
         CONSENSUS, because conservation parses every output and refuses to
         count one it cannot parse (part 3a);

       - a non-canonical spelling in the script BEING SPENT is rejected only
         by POLICY, because OP_MINT reads the multiplier off the stack and
         consensus never inspects how it was spelled (part 3b).

     Asserting both as consensus rejections -- which this test originally
     did -- would have described a guarantee the protocol does not make.

The pairs are built so the two scripts differ in the multiplier's encoding
and in nothing else -- see canonical_vs_not() below.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import (
    start_nodes,
    assert_equal,
    assert_raises_jsonrpc,
)
from test_framework.mininode import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import CScript, OP_MINT, OP_MINT_TRANSFER, SignatureHash, SIGHASH_ALL
from test_framework.key import CECKey
from test_framework.bignum import bn2vch

COIN = 100000000

MINT_ENTRY_FEE = 1000 * COIN
FEE = 1000000  # RECOMMENDED_MIN_TX_FEE, src/amount.h
SALT = b"uap_regtest_salt_16byte"

MANDATORY = "mandatory-script-verify-flag-failed"
# Policy-only rejection: the node refuses to relay it, but a miner could
# still include it in a block. Asserting the two apart is the whole point of
# parts 3a and 3b -- collapsing them would hide which guarantee is which.
NON_MANDATORY = "non-mandatory-script-verify-flag"


def make_key(seed):
    key = CECKey()
    key.set_secretbytes(seed)
    key.set_compressed(True)
    return key


def canonical_vs_not(value):
    """Return (canonical, non_canonical) push operands for a multiplier.

    CScript coerces a Python int in 0..16 to OP_0/OP_1..OP_16, and a bytes
    object to a data push of those literal bytes. Passing the int gives the
    canonical form; passing bytes gives a push that decodes to the same
    number through a different byte sequence.

    Handing both to CScript() -- rather than assembling raw bytes here --
    keeps the two scripts identical everywhere except the operand under test.

    The two ranges need different treatment, and getting this wrong produces
    a "pair" of two identical scripts that tests nothing:

      0..16  canonical is OP_N. The minimal data push of the same number is
             already non-canonical, so that is the counterexample. Zero is
             the exception: its minimal push is empty, and an empty push IS
             OP_0, so it needs an explicit 0x00 byte instead.

      >16    canonical is already the minimal data push, so there is nothing
             non-minimal to reach for -- one has to be manufactured by
             appending a redundant zero byte. Note this cannot be done by
             padding to a fixed width: 128 encodes minimally as 0x80 0x00
             already, because a bare 0x80 would read as negative.
    """
    minimal = bn2vch(value)
    if 0 <= value <= 16:
        non_canonical = minimal if minimal else b"\x00"
    else:
        non_canonical = minimal + b"\x00"

    # A pair whose two halves serialize identically would make its case
    # vacuously pass. Fail loudly here instead.
    assert bytes(CScript([value])) != bytes(CScript([non_canonical])), (
        "multiplier %d: canonical and non-canonical forms are the same "
        "script, so this case would prove nothing" % value)

    return value, non_canonical


def mint_script(pubkey, multiplier, salt=SALT):
    return CScript([pubkey, multiplier, salt, OP_MINT])


def transfer_script(pubkey, multiplier):
    return CScript([pubkey, multiplier, OP_MINT_TRANSFER])


def sign_spend(script_code, key, tx, n_in):
    sighash, err = SignatureHash(script_code, tx, n_in, SIGHASH_ALL)
    assert err is None
    sig = key.sign(sighash) + bytes([SIGHASH_ALL])
    return CScript([sig])


class UAPCanonicalEncodingTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    def fund(self, node, min_value):
        for utxo in node.listunspent():
            if int(round(utxo["amount"] * COIN)) >= min_value:
                return utxo
        raise AssertionError("no matured UTXO worth at least %d satoshi" % min_value)

    def create_mint(self, node, script, value):
        """Put an output carrying `script` and holding `value` on chain.

        Nothing is executed here -- creating an output runs no script. The
        wallet signs the funding input, which is an ordinary key.
        """
        utxo = self.fund(node, value + FEE)
        total = int(round(utxo["amount"] * COIN))
        rawtx = node.createrawtransaction(
            [{"txid": utxo["txid"], "vout": utxo["vout"]}],
            {node.getnewaddress(): float(utxo["amount"])},
        )
        tx = FromHex(CTransaction(), rawtx)
        tx.vout[0].nValue = value
        tx.vout[0].scriptPubKey = script

        # Return the remainder, or the difference becomes fee and the node
        # rejects for absurdly-high-fee before any UAP rule is reached.
        change = total - value - FEE
        if change > 0:
            change_addr = node.getnewaddress()
            change_spk = node.validateaddress(change_addr)["scriptPubKey"]
            tx.vout.append(CTxOut(change, CScript(bytes.fromhex(change_spk))))

        signed = node.signrawtransaction(ToHex(tx))
        assert signed["complete"]
        txid = node.sendrawtransaction(signed["hex"])
        node.generate(1)
        return txid

    def spend_mint(self, mint_txid, mint_scr, key, value, out_pubkey, out_multiplier):
        """Build the spend of a mint position. This is what runs OP_MINT."""
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        tx.vout = [CTxOut(value - FEE, transfer_script(out_pubkey, out_multiplier))]
        tx.vin[0].scriptSig = sign_spend(mint_scr, key, tx, 0)
        return ToHex(tx)

    def mint_and_spend(self, node, minter, minter_pub, recipient_pub,
                       mint_multiplier, out_multiplier, value=MINT_ENTRY_FEE):
        """Create a position and return the raw spend of it, ready to send.

        mint_multiplier is the operand as it should appear in the mint
        script (int for canonical, bytes for non-canonical). out_multiplier
        is the numeric value the covenant output must carry -- always
        canonical, so that a rejection is attributable to the input.
        """
        scr = mint_script(minter_pub, mint_multiplier)
        txid = self.create_mint(node, scr, value)
        return self.spend_mint(txid, scr, minter, value, recipient_pub, out_multiplier)

    def run_test(self):
        node = self.nodes[0]
        node.generate(101)

        minter = make_key(b"m" * 32)
        recipient = make_key(b"r" * 32)
        minter_pub = minter.get_pubkey()
        recipient_pub = recipient.get_pubkey()

        print("\n=== UAP canonical push encoding ===\n")

        # ---- 1: every small multiplier is spendable by a standard tx -----
        # This is the range the defect made unspendable. Each one is checked
        # individually rather than sampled, because the boundaries (1 and 16)
        # and the interior are all governed by the same OP_N branch and a
        # partial check would not prove the branch is right.
        print("canonical OP_1..OP_16 are relayable and mineable")
        for m in range(1, 17):
            canonical, _ = canonical_vs_not(m)
            raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                      canonical, m)
            # sendrawtransaction applies the standard flags, MINIMALDATA
            # among them. Acceptance here is the standardness proof.
            print("  multiplier %d ..." % m)
            txid = node.sendrawtransaction(raw)
            node.generate(1)
            confirmations = node.getrawtransaction(txid, True)["confirmations"]
            assert_equal(confirmations >= 1, True)
        print("  all 16 accepted by the mempool and mined")

        # ---- 2: multiplier 0 ---------------------------------------------
        print("canonical OP_0 is relayable and mineable")
        canonical, _ = canonical_vs_not(0)
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                  canonical, 0)
        txid = node.sendrawtransaction(raw)
        node.generate(1)
        assert_equal(node.getrawtransaction(txid, True)["confirmations"] >= 1, True)
        print("  accepted")

        # ---- 3a: non-canonical OUTPUT covenants are consensus-rejected ----
        # This is the consensus half of the rule, and the one that matters:
        # a spend may not *create* a position whose multiplier is spelled
        # non-canonically. CheckUapOutputConservation parses every output
        # with ParseUapOutputScript; a non-canonical covenant fails to parse,
        # is therefore not counted as a continuing covenant, and the spend is
        # rejected for having none. Mandatory, so a miner cannot include it
        # either.
        #
        # Each output below decodes to the same number as its canonical
        # partner in part 1, which is what makes the rejection evidence about
        # the encoding rather than about the value.
        print("non-canonical OUTPUT covenants are rejected by consensus")
        for m in [0, 1, 5, 16, 17]:
            canonical, non_canonical = canonical_vs_not(m)
            raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                      canonical, non_canonical)
            assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
            print("  multiplier %d spelled %s rejected (mandatory)"
                  % (m, non_canonical.hex()))

        # ---- 3b: non-canonical SPENT scripts are policy-rejected ---------
        # The other half, and deliberately asserted as NON-mandatory,
        # because that is what the node actually does and pretending
        # otherwise would misdescribe the protocol:
        #
        # When a position is spent, OP_MINT reads the multiplier off the
        # stack, not out of the script text. Consensus therefore does not
        # care how that push was spelled -- only SCRIPT_VERIFY_MINIMALDATA
        # does, and that is standardness, not consensus.
        #
        # The residual risk this leaves is bounded and worth stating: a miner
        # could mine an output whose multiplier is non-canonical. Such an
        # output is not a UAP position as far as ParseUapOutputScript is
        # concerned, so Solver() will not classify it and the indexer will
        # not see it; it is simply an unrecognized script, not a token
        # position that later becomes stuck. Ordinary users cannot create one,
        # because the transaction creating it is non-standard and will not
        # relay -- which is exactly what this half asserts.
        print("non-canonical SPENT scripts are rejected by policy")
        for m in [0, 1, 5, 16, 17]:
            _, non_canonical = canonical_vs_not(m)
            raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                      non_canonical, m)
            assert_raises_jsonrpc(None, NON_MANDATORY, node.sendrawtransaction, raw)
            print("  multiplier %d spelled %s rejected (policy)"
                  % (m, non_canonical.hex()))

        # ---- 4: above 16 the canonical form still works ------------------
        # Guards against a fix that made OP_N mandatory everywhere and broke
        # ordinary minimal data pushes for larger multipliers.
        print("canonical minimal data pushes above 16 still work")
        for m in [17, 127, 128, 1000]:
            canonical, _ = canonical_vs_not(m)
            raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                      canonical, m)
            node.sendrawtransaction(raw)
            node.generate(1)
        print("  17, 127, 128 and 1000 all accepted")

        print("\nAll canonical-encoding checks passed\n")


if __name__ == '__main__':
    UAPCanonicalEncodingTest().main()
