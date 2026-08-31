#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""A UAP transfer output may only be created by a transaction that spends
the lineage it names -- proven against a live node.

  The defect, restated:

  CheckUapOutputConservation runs inside OP_MINT/OP_MINT_TRANSFER, which
  execute only when a covenant output is SPENT. An output's scriptPubKey is
  not executed by the transaction that creates it. So a transaction funded
  entirely by ordinary P2PKH inputs ran no UAP code at all, and could create
  a covenant output naming any lineage it liked.

  The result was a counterfeit position: byte-identical on chain to a
  genuine one, spendable, indistinguishable to the indexer (which reads the
  lineage straight out of the script), and inflating the token's reported
  supply for the price of one transaction. The same trick with an invented
  32-byte origin conjured an entire token without paying the OP_MINT entry
  fee.

  The fix is CheckUapOutputCreation (validation.cpp): a transaction may only
  create a transfer output of group (multiplier, origin) if it also spends
  an input of that group.

uap_creation_tests.cpp drives that function directly. This file exists to
prove the function is actually WIRED IN -- a correct rule that nothing calls
would pass every unit test. Both enforcement points are asserted separately,
because they are separate code paths and only one of them is the consensus
floor:

  - the MEMPOOL, via AcceptToMemoryPool. A node will not relay it.

  - a BLOCK, via ConnectBlock and submitblock. This is the half that
    matters: mempool policy binds only well-behaved peers, and a miner
    writes blocks directly. Note also that ConnectBlock does not run
    CheckInputs over the coinbase, so a rule that had ridden along inside
    CheckInputs would have left miners as the one party still able to
    fabricate positions -- which is why the coinbase gets its own case here.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import (
    start_nodes,
    assert_equal,
    assert_raises_jsonrpc,
)
from test_framework.mininode import (
    CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex,
)
from test_framework.blocktools import create_block, create_coinbase
from test_framework.script import CScript
from test_framework.uap import (
    COIN, FEE, MINT_ENTRY_FEE, NO_INPUT,
    make_key, mint_script, transfer_script, origin_of, sign_spend,
)

MULTIPLIER = 1000


class UAPOutputProvenanceTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    # ------------------------------------------------------------------
    # helpers
    # ------------------------------------------------------------------

    def fund(self, node, min_value):
        for utxo in node.listunspent():
            if int(round(utxo["amount"] * COIN)) >= min_value:
                return utxo
        raise AssertionError("no matured UTXO worth at least %d satoshi" % min_value)

    def build_output_creating_tx(self, node, script, value):
        """A wallet-signed transaction whose vout[0] carries `script`.

        Every input is an ordinary P2PKH the wallet controls, so no UAP code
        runs when this is validated -- which is precisely the hole. Returned
        signed but NOT sent, so callers choose whether to expect acceptance.
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
            addr = node.getnewaddress()
            spk = node.validateaddress(addr)["scriptPubKey"]
            tx.vout.append(CTxOut(change, CScript(bytes.fromhex(spk))))

        signed = node.signrawtransaction(ToHex(tx))
        assert signed["complete"]
        return signed["hex"]

    def submit_in_a_block(self, node, raw_hexes):
        """Mine `raw_hexes` into a block by hand and submit it.

        The mempool never sees these, so this reaches ConnectBlock directly
        -- the path a hostile miner would use. Returns submitblock's result
        (None on acceptance, a reason string on rejection).
        """
        tip = node.getbestblockhash()
        height = node.getblockcount() + 1
        block = create_block(int(tip, 16), create_coinbase(height),
                             node.getblock(tip)["time"] + 1)
        for raw in raw_hexes:
            block.vtx.append(FromHex(CTransaction(), raw))
        block.hashMerkleRoot = block.calc_merkle_root()
        block.solve()
        return node.submitblock(ToHex(block))

    # ------------------------------------------------------------------

    def run_test(self):
        print("\n=== UAP transfer-output provenance ===\n")
        node = self.nodes[0]
        node.generate(200)

        minter = make_key(b"uap-provenance-minter-seed-00000")
        attacker = make_key(b"uap-provenance-attacker-seed-000")
        minter_pub = minter.get_pubkey()
        attacker_pub = attacker.get_pubkey()

        # ==============================================================
        # A genuine token, so the attack below has a real lineage to
        # counterfeit rather than only an invented one.
        # ==============================================================
        mint_txid = node.sendrawtransaction(self.build_output_creating_tx(
            node, mint_script(minter_pub, MULTIPLIER), MINT_ENTRY_FEE))
        node.generate(1)
        assert_equal(node.gettxout(mint_txid, 0) is not None, True)

        # A mint is spent into its lineage, which is derived from the
        # outpoint being spent. That first spend is what brings the lineage
        # into existence.
        lineage = origin_of(mint_txid, 0)
        position_value = MINT_ENTRY_FEE - FEE
        spend = CTransaction()
        spend.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        spend.vout = [CTxOut(position_value, transfer_script(minter_pub, MULTIPLIER, lineage))]
        spend.vin[0].scriptSig = sign_spend(mint_script(minter_pub, MULTIPLIER), minter, spend, 0)
        position_txid = node.sendrawtransaction(ToHex(spend))
        node.generate(1)
        print("  genuine position established: %s" % position_txid)

        # ==============================================================
        # 1. THE ATTACK, at the mempool.
        # ==============================================================
        counterfeit = self.build_output_creating_tx(
            node, transfer_script(attacker_pub, MULTIPLIER, lineage), 99 * COIN)
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, counterfeit)
        print("  mempool refuses a counterfeit position")

        # 2. The same with a lineage no mint ever created: no victim needed,
        #    and the OP_MINT entry fee is skipped entirely.
        invented = self.build_output_creating_tx(
            node, transfer_script(attacker_pub, MULTIPLIER, b"\xab" * 32), 1 * COIN)
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, invented)
        print("  mempool refuses an invented lineage")

        # ==============================================================
        # 3. THE ATTACK, in a block. This is the consensus floor: a miner
        #    does not have to ask the mempool's permission.
        # ==============================================================
        before = node.getbestblockhash()
        reason = self.submit_in_a_block(node, [counterfeit])
        assert reason is not None, \
            "a block containing a counterfeit position was ACCEPTED -- " \
            "mempool policy alone does not bind a miner"
        assert_equal(node.getbestblockhash(), before)
        print("  ConnectBlock refuses a counterfeit position: %s" % reason)

        # 4. And in the coinbase, which ConnectBlock does not route through
        #    CheckInputs at all.
        tip = node.getbestblockhash()
        height = node.getblockcount() + 1
        block = create_block(int(tip, 16), create_coinbase(height),
                             node.getblock(tip)["time"] + 1)
        block.vtx[0].vout[0].scriptPubKey = transfer_script(attacker_pub, MULTIPLIER, lineage)
        block.vtx[0].rehash()
        block.hashMerkleRoot = block.calc_merkle_root()
        block.solve()
        reason = node.submitblock(ToHex(block))
        assert reason is not None, \
            "a coinbase minting a counterfeit position was ACCEPTED -- " \
            "the rule is not reached for coinbase transactions"
        assert_equal(node.getbestblockhash(), tip)
        print("  ConnectBlock refuses a counterfeit coinbase: %s" % reason)

        # ==============================================================
        # 5. The rule must not break the protocol it protects. Without
        #    these, every case above would pass against a node that
        #    rejected all covenant outputs unconditionally.
        # ==============================================================

        # An ordinary transfer of a real position: spends lineage, creates
        # lineage.
        recipient = make_key(b"uap-provenance-recipient-seed-00")
        onward = CTransaction()
        onward.vin = [CTxIn(COutPoint(int(position_txid, 16), 0))]
        onward.vout = [CTxOut(position_value - FEE,
                              transfer_script(recipient.get_pubkey(), MULTIPLIER, lineage))]
        onward.vin[0].scriptSig = sign_spend(
            transfer_script(minter_pub, MULTIPLIER, lineage), minter, onward, 0)
        onward_txid = node.sendrawtransaction(ToHex(onward))
        node.generate(1)
        assert_equal(node.gettxout(onward_txid, 0) is not None, True)
        print("  an ordinary transfer is still accepted")

        # Minting still works: a mint output has no lineage to prove, and
        # creating one is how a lineage begins.
        second_mint = node.sendrawtransaction(self.build_output_creating_tx(
            node, mint_script(minter_pub, MULTIPLIER), MINT_ENTRY_FEE))
        node.generate(1)
        assert_equal(node.gettxout(second_mint, 0) is not None, True)
        print("  minting is still possible")

        # A phantom lineage smuggled alongside a genuine spend: the
        # transaction really does spend `lineage`, so a rule that only asked
        # "does this transaction spend any covenant?" would wave it through.
        phantom = CTransaction()
        phantom.vin = [CTxIn(COutPoint(int(onward_txid, 16), 0))]
        half = (position_value - 2 * FEE) // 2
        phantom.vout = [
            CTxOut(half, transfer_script(recipient.get_pubkey(), MULTIPLIER, lineage)),
            CTxOut(half, transfer_script(attacker_pub, MULTIPLIER, origin_of(onward_txid, 0))),
        ]
        phantom.vin[0].scriptSig = sign_spend(
            transfer_script(recipient.get_pubkey(), MULTIPLIER, lineage), recipient, phantom, 0)
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, ToHex(phantom))
        print("  a phantom lineage beside a genuine spend is refused")


if __name__ == '__main__':
    UAPOutputProvenanceTest().main()
