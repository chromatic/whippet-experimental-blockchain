#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.
"""UAP OP_MINT / OP_MINT_TRANSFER end-to-end QA test.

Walks one position through its whole life against a live regtest node --
minted, spent into a lineage, forwarded onward -- with the ways that can go
wrong asserted at each step:

  - only the declared recipient may spend a position
  - the destination must be a conforming covenant, not a plain script
  - value cannot be created
  - a mint's first spend names its lineage, and may only name its own
  - an onward transfer must carry that lineage forward unchanged

The last two are what v2 added. A mint carries no origin -- its identity is
the outpoint it is spent at, which it cannot contain without circularity --
so the first spend is where a lineage comes into existence, and every spend
after that is bound to it.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import (
    start_nodes,
    assert_equal,
    assert_raises_jsonrpc,
)
from test_framework.mininode import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import CScript
from test_framework.uap import (
    COIN, MANDATORY, NO_INPUT,
    make_key, mint_script, transfer_script, origin_of, sign_spend,
)

MULTIPLIER = 1000


class UAPMintTransferTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    def fund_mint_input(self, node, min_value=1000 * COIN):
        """Find a single matured UTXO worth >= min_value."""
        for utxo in node.listunspent():
            if int(round(utxo["amount"] * COIN)) >= min_value:
                return utxo
        raise AssertionError("no matured UTXO with sufficient value for the OP_MINT entry fee")

    def build_and_send_mint(self, node, minter_pubkey, utxo, out_value):
        inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
        # Overwrite the output below; this destination address is discarded.
        outputs = {node.getnewaddress(): float(utxo["amount"])}
        rawtx = node.createrawtransaction(inputs, outputs)
        tx = FromHex(CTransaction(), rawtx)
        tx.vout[0].nValue = out_value
        tx.vout[0].scriptPubKey = mint_script(minter_pubkey, MULTIPLIER)
        rawtx = ToHex(tx)

        # This scriptSig authorizes spending the funding UTXO (ordinary
        # wallet key), unrelated to the covenant's own recipient-signature
        # rule.
        signed = node.signrawtransaction(rawtx)
        assert signed["complete"]
        return node.sendrawtransaction(signed["hex"])

    def run_test(self):
        node = self.nodes[0]
        node.generate(101)  # mature a coinbase past the 60-block regtest maturity

        minter_key = make_key(b"m" * 32)
        recipient_key = make_key(b"r" * 32)
        impostor_key = make_key(b"x" * 32)
        next_key = make_key(b"n" * 32)

        minter_pubkey = minter_key.get_pubkey()
        recipient_pubkey = recipient_key.get_pubkey()
        impostor_pubkey = impostor_key.get_pubkey()
        next_pubkey = next_key.get_pubkey()

        print("Step 1: mint")
        utxo = self.fund_mint_input(node)
        mint_value = int(round(utxo["amount"] * COIN)) - 100000  # leave a small fee
        mint_txid = self.build_and_send_mint(node, minter_pubkey, utxo, mint_value)
        node.generate(1)

        mint_tx = FromHex(CTransaction(), node.getrawtransaction(mint_txid))
        the_mint_script = mint_script(minter_pubkey, MULTIPLIER)
        assert_equal(mint_tx.vout[0].scriptPubKey, bytes(the_mint_script))

        # The lineage this mint will become, fixed by the outpoint it is
        # spent at. Nothing on chain carries it yet -- it comes into
        # existence with the first spend, in step 5.
        lineage = origin_of(mint_txid, 0)
        print("  mint output confirmed: %d satoshi, multiplier %d" % (mint_value, MULTIPLIER))
        print("  its lineage will be %s" % lineage.hex()[:16])

        print("Step 2: reject spend by the wrong signer")
        bad_spend = CTransaction()
        bad_spend.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_spend.vout = [CTxOut(mint_value - 50000,
                                 transfer_script(recipient_pubkey, MULTIPLIER, lineage))]
        bad_spend.vin[0].scriptSig = sign_spend(the_mint_script, impostor_key, bad_spend, 0)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(bad_spend))
        print("  correctly rejected")

        print("Step 3: reject a non-covenant destination output")
        bad_dest = CTransaction()
        bad_dest.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_dest.vout = [CTxOut(mint_value - 50000, CScript([recipient_pubkey, b'\xac']))]  # plain P2PK-ish, not a covenant
        bad_dest.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, bad_dest, 0)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(bad_dest))
        print("  correctly rejected")

        print("Step 4: reject value created out of thin air")
        bad_value = CTransaction()
        bad_value.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_value.vout = [CTxOut(mint_value + COIN,
                                 transfer_script(recipient_pubkey, MULTIPLIER, lineage))]
        bad_value.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, bad_value, 0)
        # Caught by the generic "outputs exceed inputs" consensus check
        # before script validation even runs; still a correct rejection.
        assert_raises_jsonrpc(None, "bad-txns-in-belowout",
                              node.sendrawtransaction, ToHex(bad_value))
        print("  correctly rejected")

        print("Step 5: a mint's first spend may only name its own lineage")
        # A fresh mint cannot be renamed into somebody else's token. The
        # origin the covenant output carries is not the spender's choice:
        # consensus derives it from the outpoint being spent and requires a
        # matching output.
        #
        # Two independent rules refuse this, and the reported one is
        # CheckUapOutputCreation rather than the covenant check, because a
        # renamed output is also an output of a lineage the transaction does
        # not spend -- and creation is checked before scripts are run. That
        # ordering is the reason to assert the exact reason string here: an
        # assertion of "rejected somehow" would not notice if the covenant
        # rule stopped working, since the creation rule would keep the test
        # green on its own.
        renamed = CTransaction()
        renamed.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        renamed.vout = [CTxOut(mint_value - 50000,
                               transfer_script(recipient_pubkey, MULTIPLIER,
                                               origin_of(mint_txid, 1)))]
        renamed.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, renamed, 0)
        assert_raises_jsonrpc(None, NO_INPUT, node.sendrawtransaction, ToHex(renamed))
        print("  a foreign origin is rejected")

        transfer_value = mint_value - 50000
        good_transfer = CTransaction()
        good_transfer.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        good_transfer.vout = [CTxOut(transfer_value,
                                     transfer_script(recipient_pubkey, MULTIPLIER, lineage))]
        good_transfer.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, good_transfer, 0)
        transfer_txid = node.sendrawtransaction(ToHex(good_transfer))
        node.generate(1)
        print("  its own origin is accepted: %s" % transfer_txid)

        print("Step 6: an onward transfer carries the lineage forward, unchanged")
        the_transfer_script = transfer_script(recipient_pubkey, MULTIPLIER, lineage)
        final_value = transfer_value - 50000

        # Deriving a fresh origin from the transfer's OWN outpoint is the
        # mistake this rule exists to catch: it looks reasonable, it is what
        # a mint does, and it would rename the position into a lineage that
        # has no input in the transaction -- minting supply out of nothing.
        # Reported by CheckUapOutputCreation, for the reason given in step 5.
        rewritten = CTransaction()
        rewritten.vin = [CTxIn(COutPoint(int(transfer_txid, 16), 0))]
        rewritten.vout = [CTxOut(final_value,
                                 transfer_script(next_pubkey, MULTIPLIER,
                                                 origin_of(transfer_txid, 0)))]
        rewritten.vin[0].scriptSig = sign_spend(the_transfer_script, recipient_key, rewritten, 0)
        assert_raises_jsonrpc(None, NO_INPUT, node.sendrawtransaction, ToHex(rewritten))
        print("  re-deriving the origin from its own outpoint is rejected")

        onward = CTransaction()
        onward.vin = [CTxIn(COutPoint(int(transfer_txid, 16), 0))]
        onward.vout = [CTxOut(final_value, transfer_script(next_pubkey, MULTIPLIER, lineage))]
        onward.vin[0].scriptSig = sign_spend(the_transfer_script, recipient_key, onward, 0)
        onward_txid = node.sendrawtransaction(ToHex(onward))
        node.generate(1)
        print("  onward transfer confirmed: %s" % onward_txid)

        # The lineage survived two hops and is still the one the mint's
        # outpoint named. This is the assertion the whole file builds to.
        final_tx = FromHex(CTransaction(), node.getrawtransaction(onward_txid))
        assert_equal(final_tx.vout[0].scriptPubKey,
                     bytes(transfer_script(next_pubkey, MULTIPLIER, lineage)))
        print("  and it still carries the lineage the mint's outpoint named")

        print("All UAP mint/transfer checks passed")


if __name__ == "__main__":
    UAPMintTransferTest().main()
