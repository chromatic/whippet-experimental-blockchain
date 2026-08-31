#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""UAP covenant rule-by-rule QA test.

Each numbered guard in src/script/interpreter.cpp's OP_MINT/OP_MINT_TRANSFER
case gets one case here: entry fee (2), origin width (3), multiplier range
and the virtual-balance overflow guard (4). A matching positive case sits
next to each rejection, so a test that rejects for the wrong reason cannot
pass unnoticed.

  TRAP 1, and the reason this file was first rewritten:

  *Creating* an output whose scriptPubKey ends in OP_MINT executes nothing.
  Script runs when an output is *spent*. An early version only ever built
  and broadcast the mint-creating transaction, so every "invalid" case was
  accepted -- it printed warnings that read like consensus holes and
  asserted nothing at all.

  So every case below spends a position, which is what runs the opcode, and
  asserts on the node's rejection reason rather than printing it.

  TRAP 2, and the reason for the v2 rewrite:

  A test that asserts only "this was rejected" cannot tell "rejected by the
  rule I am testing" from "rejected because this is no longer a covenant at
  all". When the covenant format dropped the salt and gained a 32-byte
  origin, the old salted scripts stopped parsing as UAP outputs entirely --
  and every negative case here went on passing while testing nothing.

  Hence the positive half of every pair, which is what actually pins the
  rule; and hence test_framework/uap.py, so the format lives in one place
  instead of a private copy in each of five test files.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import (
    start_nodes,
    assert_equal,
    assert_raises_jsonrpc,
)
from test_framework.mininode import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import CScript, OP_MINT_TRANSFER
from test_framework.uap import (
    COIN, FEE, MINT_ENTRY_FEE, ORIGIN_SIZE, MAX_UAP_MULTIPLIER,
    MAX_VIRTUAL_BALANCE, MANDATORY,
    make_key, mint_script, transfer_script, origin_of, sign_spend,
)


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

    def create_output(self, node, script, value):
        """Put an output carrying `script` and holding `value` on chain.

        Nothing is executed here; this only creates the position. The wallet
        signs the *funding* input, which is an ordinary key and has nothing
        to do with the covenant's own recipient-signature rule.
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

        This is what executes OP_MINT. The lineage the covenant output must
        carry is derived from the outpoint being spent -- a mint cannot name
        its own lineage, since the origin depends on the txid, which depends
        on the script.
        """
        lineage = origin_of(mint_txid, 0)
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        tx.vout = [CTxOut(value - FEE, transfer_script(out_pubkey, multiplier, lineage))]
        tx.vin[0].scriptSig = sign_spend(mint_scr, key, tx, 0)
        return ToHex(tx)

    def mint_and_spend(self, node, minter, minter_pub, recipient_pub,
                       multiplier, value=MINT_ENTRY_FEE):
        """Create a position with the given parameters and return the raw
        spend of it, ready to broadcast."""
        scr = mint_script(minter_pub, multiplier)
        txid = self.create_output(node, scr, value)
        return self.spend_mint(txid, scr, minter, value, recipient_pub, multiplier)

    def run_test(self):
        node = self.nodes[0]
        node.generate(101)  # mature coinbases past regtest maturity

        minter = make_key(b"m" * 32)
        recipient = make_key(b"r" * 32)
        minter_pub = minter.get_pubkey()
        recipient_pub = recipient.get_pubkey()

        print("\n=== UAP covenant rules, one at a time ===\n")

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

        # ---- rule 3: origin width ---------------------------------------
        # This is the slot the salt-length rule used to occupy, and the two
        # are opposites: v1 required a salt of AT LEAST 16 bytes on a mint,
        # v2 requires an origin of EXACTLY 32 bytes on a transfer, and a
        # mint carries no third field at all.
        #
        # A wrong-width origin is checked twice over, deliberately.
        # ParseUapOutputScript refuses to classify it as a covenant, so such
        # an output has no conforming continuation and is unspendable for
        # that reason alone; rule 3 states the same thing at execution time,
        # which turns "stuck forever, for reasons two functions away" into
        # an immediate, locatable rejection. The C++ side calls this out at
        # interpreter.cpp rule 3.
        print("rule 3: origin of exactly %d bytes" % ORIGIN_SIZE)
        for width in (ORIGIN_SIZE - 1, ORIGIN_SIZE + 1):
            scr = transfer_script(recipient_pub, 1000, b"o" * width)
            txid = self.create_output(node, scr, 10 * COIN)
            tx = CTransaction()
            tx.vin = [CTxIn(COutPoint(int(txid, 16), 0))]
            tx.vout = [CTxOut(10 * COIN - FEE,
                              transfer_script(recipient_pub, 1000, b"o" * width))]
            tx.vin[0].scriptSig = sign_spend(scr, recipient, tx, 0)
            assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(tx))
            print("  a %d-byte origin is unspendable" % width)

        # The positive half: the same script at exactly 32 bytes spends.
        # Without it, the two cases above would pass just as well against a
        # node that refused every transfer covenant.
        mint_txid = self.create_output(node, mint_script(minter_pub, 1002), MINT_ENTRY_FEE)
        lineage = origin_of(mint_txid, 0)
        position = self.spend_mint(mint_txid, mint_script(minter_pub, 1002),
                                   minter, MINT_ENTRY_FEE, recipient_pub, 1002)
        position_txid = node.sendrawtransaction(position)
        node.generate(1)
        onward = CTransaction()
        onward.vin = [CTxIn(COutPoint(int(position_txid, 16), 0))]
        onward.vout = [CTxOut(MINT_ENTRY_FEE - 2 * FEE,
                              transfer_script(recipient_pub, 1002, lineage))]
        onward.vin[0].scriptSig = sign_spend(
            transfer_script(recipient_pub, 1002, lineage), recipient, onward, 0)
        node.sendrawtransaction(ToHex(onward))
        node.generate(1)
        print("  exactly %d bytes is accepted, and carries the lineage forward" % ORIGIN_SIZE)

        # ---- rule 4a: multiplier range ----------------------------------
        print("rule 4: multiplier within [0, %d]" % MAX_UAP_MULTIPLIER)
        raw = self.mint_and_spend(node, minter, minter_pub, recipient_pub,
                                  MAX_UAP_MULTIPLIER + 1)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, raw)
        print("  INT32_MAX + 1 is rejected")

        # ---- rule 4b: virtual-balance overflow guard --------------------
        # (input value in whole coins) * multiplier must not exceed 2^48.
        # At the entry fee that is 1000 whole coins, so the largest legal
        # multiplier there is 2^48 / 1000.
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

        print("\nAll UAP covenant rule checks passed\n")


if __name__ == '__main__':
    UAPTransactionTest().main()
