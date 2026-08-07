#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""UAP OP_MINT rule-by-rule QA test.

Each of OP_MINT's numbered guards in src/script/interpreter.cpp gets one
case here: entry fee, salt length, multiplier range, and the virtual-balance
overflow guard. A matching positive case sits next to each rejection, so a
test that rejects for the wrong reason cannot pass unnoticed.

  IMPORTANT, and the reason this file was rewritten:

  *Creating* an output whose scriptPubKey ends in OP_MINT executes nothing.
  Script runs when an output is *spent*. The previous version of this test
  only ever built and broadcast the mint-creating transaction, so every
  "invalid" case was accepted -- it printed warnings that read like
  consensus holes and asserted nothing at all. It also used the pre-1.2.0
  script format, CScript([multiplier, salt, OP_MINT]), which carries no
  recipient pubkey and is not a UAP output under the current rules.

  So every case below spends a mint position, which is what runs the
  opcode, and asserts on the node's rejection reason rather than printing it.
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

COIN = 100000000

# src/script/interpreter.cpp, OP_MINT step 2.
MINT_ENTRY_FEE = 1000 * COIN
# step 3.
MIN_SALT_LEN = 16
# step 4.
MAX_UAP_MULTIPLIER = 2147483647
MAX_VIRTUAL_BALANCE = 1 << 48

FEE = 1000000  # RECOMMENDED_MIN_TX_FEE, src/amount.h
SALT = b"uap_regtest_salt_16byte"

# A mandatory failure is a consensus rejection; a non-mandatory one is only
# policy. Every case here must be mandatory -- if one ever reports
# "non-mandatory", the rule has quietly become advisory.
MANDATORY = "mandatory-script-verify-flag-failed"


def make_key(seed):
    key = CECKey()
    key.set_secretbytes(seed)
    key.set_compressed(True)
    return key


def mint_script(pubkey, multiplier, salt=SALT):
    return CScript([pubkey, multiplier, salt, OP_MINT])


def transfer_script(pubkey, multiplier):
    return CScript([pubkey, multiplier, OP_MINT_TRANSFER])


def sign_spend(script_code, key, tx, n_in):
    sighash, err = SignatureHash(script_code, tx, n_in, SIGHASH_ALL)
    assert err is None
    sig = key.sign(sighash) + bytes([SIGHASH_ALL])
    return CScript([sig])


class UAPTransactionTest(BitcoinTestFramework):
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

        Nothing is executed here; this only creates the position. The wallet
        signs the *funding* input, which is an ordinary key and has nothing
        to do with OP_MINT's own recipient-signature rule.
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

        # Return the remainder to the wallet. Without this the whole
        # difference becomes fee and the node rejects the transaction as
        # absurdly-high-fee long before any UAP rule is reached.
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

    def spend_mint(self, mint_txid, mint_scr, key, value, out_pubkey, multiplier):
        """Build a spend of a mint position into a conforming covenant.

        This is what executes OP_MINT.
        """
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        tx.vout = [CTxOut(value - FEE, transfer_script(out_pubkey, multiplier))]
        tx.vin[0].scriptSig = sign_spend(mint_scr, key, tx, 0)
        return ToHex(tx)

    def mint_and_spend(self, node, minter, minter_pub, recipient_pub,
                       multiplier, value=MINT_ENTRY_FEE, salt=SALT):
        """Create a position with the given parameters and return the raw
        spend of it, ready to broadcast."""
        scr = mint_script(minter_pub, multiplier, salt=salt)
        txid = self.create_mint(node, scr, value)
        return self.spend_mint(txid, scr, minter, value, recipient_pub, multiplier)

    def run_test(self):
        node = self.nodes[0]
        node.generate(101)  # mature coinbases past regtest maturity

        minter = make_key(b"m" * 32)
        recipient = make_key(b"r" * 32)
        minter_pub = minter.get_pubkey()
        recipient_pub = recipient.get_pubkey()

        print("\n=== OP_MINT rule-by-rule ===\n")

        # ---- baseline ---------------------------------------------------
        # Every case below differs from this by exactly one field, so a
        # rejection can be attributed to that field and nothing else.
        print("baseline: a conforming mint spends")
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, 1000)
        spent_txid = node.sendrawtransaction(raw)
        node.generate(1)
        assert_equal(node.getrawtransaction(spent_txid, True)["confirmations"] >= 1, True)
        print("  accepted and mined: %s" % spent_txid[:16])

        # ---- rule 2: entry fee ------------------------------------------
        print("rule 2: entry fee of %d coin" % (MINT_ENTRY_FEE // COIN))
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, 1000,
                                  value=MINT_ENTRY_FEE - 1)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
        print("  one satoshi below the floor is rejected")

        # Exactly at the floor is accepted. Testing the boundary rather than
        # some value far from it is what makes the rejection above evidence
        # about the entry fee specifically.
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, 1001,
                                  value=MINT_ENTRY_FEE)
        node.sendrawtransaction(raw)
        node.generate(1)
        print("  exactly at the floor is accepted")

        # ---- rule 3: salt length ----------------------------------------
        print("rule 3: salt of at least %d bytes" % MIN_SALT_LEN)
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, 1002,
                                  salt=b"x" * (MIN_SALT_LEN - 1))
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
        print("  a %d-byte salt is rejected" % (MIN_SALT_LEN - 1))

        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, 1003,
                                  salt=b"y" * MIN_SALT_LEN)
        node.sendrawtransaction(raw)
        node.generate(1)
        print("  exactly %d bytes is accepted" % MIN_SALT_LEN)

        # ---- rule 4a: multiplier range ----------------------------------
        print("rule 4: multiplier within [0, %d]" % MAX_UAP_MULTIPLIER)
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                  MAX_UAP_MULTIPLIER + 1)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
        print("  INT32_MAX + 1 is rejected")

        # ---- rule 4b: virtual-balance overflow guard --------------------
        # (input value in whole coins) * multiplier must not exceed 2^48.
        # At the entry fee that is 1000 whole coins, so the largest legal
        # multiplier here is 2^48 / 1000.
        print("rule 4: (whole coins * multiplier) <= 2^48")
        # The guard is only reachable when 2^48 / base_coin is itself below
        # INT32_MAX -- otherwise the range check above fires first and this
        # case would prove nothing. That needs base_coin > 2^48 / 2^31 =
        # 131072 whole coins, well above the 1000-coin entry fee, so lock a
        # much larger position for these two cases.
        overflow_value = 400000 * COIN
        base_coin = overflow_value // COIN
        just_under = MAX_VIRTUAL_BALANCE // base_coin
        too_big = just_under + 1
        assert too_big <= MAX_UAP_MULTIPLIER, (
            "this case must trip the overflow guard, not the range check")

        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, too_big,
                                  value=overflow_value)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
        print("  a multiplier one above the guard is rejected")

        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub, just_under,
                                  value=overflow_value)
        node.sendrawtransaction(raw)
        node.generate(1)
        print("  a multiplier exactly at the guard is accepted")

        print("\nAll OP_MINT rule checks passed\n")


if __name__ == '__main__':
    UAPTransactionTest().main()
