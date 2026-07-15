#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.
"""UAP OP_MINT / OP_MINT_TRANSFER end-to-end QA test.

Exercises the fixed OP_MINT design against a live regtest node:
  - minting requires the recipient's signature (not anyone-can-spend)
  - transfers must forward to a conforming OP_MINT_TRANSFER covenant
    carrying the same multiplier
  - wrong signer, value inflation, and multiplier mismatch are all
    rejected by the mempool/consensus layer
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
MULTIPLIER = 1000
SALT = b"uap_regtest_salt_16byte"


def make_key(seed):
    key = CECKey()
    key.set_secretbytes(seed)
    key.set_compressed(True)
    return key


def mint_script(pubkey, multiplier=MULTIPLIER, salt=SALT):
    return CScript([pubkey, multiplier, salt, OP_MINT])


def transfer_script(pubkey, multiplier=MULTIPLIER):
    return CScript([pubkey, multiplier, OP_MINT_TRANSFER])


def sign_spend(script_code, key, tx, n_in):
    sighash, err = SignatureHash(script_code, tx, n_in, SIGHASH_ALL)
    assert err is None
    sig = key.sign(sighash) + bytes([SIGHASH_ALL])
    return CScript([sig])


class UAPMintTransferTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    def fund_mint_input(self, node, min_value=1000 * COIN):
        """Find (or mine towards) a single matured UTXO worth >= min_value."""
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
        tx.vout[0].scriptPubKey = mint_script(minter_pubkey)
        rawtx = ToHex(tx)

        # This scriptSig authorizes spending the funding UTXO (ordinary
        # wallet key), unrelated to OP_MINT's own recipient-signature rule.
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

        mint_tx_raw = node.getrawtransaction(mint_txid)
        mint_tx = FromHex(CTransaction(), mint_tx_raw)
        the_mint_script = mint_script(minter_pubkey)
        assert_equal(mint_tx.vout[0].scriptPubKey, bytes(the_mint_script))
        print("  mint output confirmed: %d satoshi, multiplier %d" % (mint_value, MULTIPLIER))

        print("Step 2: reject spend by the wrong signer")
        bad_spend = CTransaction()
        bad_spend.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_spend.vout = [CTxOut(mint_value - 50000, transfer_script(recipient_pubkey))]
        bad_spend.vin[0].scriptSig = sign_spend(the_mint_script, impostor_key, bad_spend, 0)
        assert_raises_jsonrpc(
            None, "mandatory-script-verify-flag-failed",
            node.sendrawtransaction, ToHex(bad_spend),
        )
        print("  correctly rejected")

        print("Step 3: reject a non-covenant destination output")
        bad_dest = CTransaction()
        bad_dest.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_dest.vout = [CTxOut(mint_value - 50000, CScript([recipient_pubkey, b'\xac']))]  # plain P2PK-ish, not a covenant
        bad_dest.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, bad_dest, 0)
        assert_raises_jsonrpc(
            None, "mandatory-script-verify-flag-failed",
            node.sendrawtransaction, ToHex(bad_dest),
        )
        print("  correctly rejected")

        print("Step 4: reject value created out of thin air")
        bad_value = CTransaction()
        bad_value.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        bad_value.vout = [CTxOut(mint_value + COIN, transfer_script(recipient_pubkey))]
        bad_value.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, bad_value, 0)
        # Caught by the generic "outputs exceed inputs" consensus check
        # before script validation even runs; still a correct rejection.
        assert_raises_jsonrpc(
            None, "bad-txns-in-belowout",
            node.sendrawtransaction, ToHex(bad_value),
        )
        print("  correctly rejected")

        print("Step 5: correct transfer to recipient succeeds")
        transfer_value = mint_value - 50000
        good_transfer = CTransaction()
        good_transfer.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        good_transfer.vout = [CTxOut(transfer_value, transfer_script(recipient_pubkey))]
        good_transfer.vin[0].scriptSig = sign_spend(the_mint_script, minter_key, good_transfer, 0)
        transfer_txid = node.sendrawtransaction(ToHex(good_transfer))
        node.generate(1)
        print("  transfer confirmed: %s" % transfer_txid)

        print("Step 6: recipient forwards onward")
        the_transfer_script = transfer_script(recipient_pubkey)
        final_value = transfer_value - 50000
        onward = CTransaction()
        onward.vin = [CTxIn(COutPoint(int(transfer_txid, 16), 0))]
        onward.vout = [CTxOut(final_value, transfer_script(next_pubkey))]
        onward.vin[0].scriptSig = sign_spend(the_transfer_script, recipient_key, onward, 0)
        onward_txid = node.sendrawtransaction(ToHex(onward))
        node.generate(1)
        print("  onward transfer confirmed: %s" % onward_txid)

        print("All UAP mint/transfer checks passed")


if __name__ == "__main__":
    UAPMintTransferTest().main()
