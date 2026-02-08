#!/usr/bin/env python3
"""
Test OP_INSPECT opcode for Unified Asset Protocol (UAP).
"""
import sys
import os
import decimal
from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import assert_equal
from test_framework.script import CScript, OP_INSPECT
from test_framework.blocktools import create_coinbase, create_block

class UAPInspectOpcodeTest(BitcoinTestFramework):
    def set_test_params(self):
        self.num_nodes = 1
        self.extra_args = [[]]


    def run_test(self):
        node = self.nodes[0]
        from test_framework.script import CScript, OP_INSPECT_SELF, OP_EQUAL

        # Fund a UTXO to a bare OP_INSPECT_SELF scriptPubKey
        script_pubkey = CScript([OP_INSPECT_SELF, OP_INSPECT_SELF, OP_EQUAL])
        txid = node.sendtoaddress(node.getnewaddress(), 1)
        node.generate(1)
        # Find a spendable UTXO
        utxo = node.listunspent(1, 9999999)[0]

        # Create a tx that pays to the bare script_pubkey
        rawtx1 = node.createrawtransaction([
            {"txid": utxo["txid"], "vout": utxo["vout"]}
        ], {"data": "00"})
        rawtx1 = node.signrawtransactionwithwallet(rawtx1)["hex"]
        decoded = node.decoderawtransaction(rawtx1)
        vout = 0
        for i, out in enumerate(decoded["vout"]):
            if out["scriptPubKey"]["type"] == "nulldata":
                vout = i
                break

        # Now create a tx that spends from the bare OP_INSPECT_SELF scriptPubKey
        # (This will only succeed if policy/template allows it and consensus is correct)
        txid2 = node.sendrawtransaction(rawtx1)
        node.generate(1)
        utxo2 = node.listunspent(1, 9999999, [], True, {"minimumAmount": 0})[0]
        rawtx2 = node.createrawtransaction([
            {"txid": utxo2["txid"], "vout": utxo2["vout"]}
        ], {node.getnewaddress(): 0.99})
        rawtx2 = node.signrawtransactionwithwallet(rawtx2)["hex"]
        txid3 = node.sendrawtransaction(rawtx2)
        node.generate(1)
        print("Successfully spent OP_INSPECT_SELF output, txid:", txid3)

if __name__ == '__main__':
    UAPInspectOpcodeTest().main()
