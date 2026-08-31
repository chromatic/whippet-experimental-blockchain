#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""
Test UAP (Unspent Asset Protocol) functionality:
- OP_MINT: create a fresh UAP position, entry-fee/overflow-gated at spend time
- OP_MINT_TRANSFER: continue a UAP position under covenant conservation rules
- OP_INSPECT: introspect transaction outputs
- OP_INSPECT_SELF: introspect the current script

Script shapes (see test_framework/uap.py, and src/script/script.cpp's
ParseUapOutputScript, which is the authority):

  Mint output:     <recipient_pubkey> <multiplier> OP_MINT
  Transfer output: <recipient_pubkey> <multiplier> <origin32> OP_MINT_TRANSFER

Spending either requires scriptSig = <sig> only -- the pubkey, multiplier
and origin all come from the scriptPubKey being spent.

The covenant rules -- entry fee, origin width, one-shot, conservation -- all
run when a position is SPENT, because that is when its scriptPubKey executes.
Creating an output executes nothing, which is the single fact most of this
file's history of wrong tests came from ignoring.

The lineage itself is the thread running through every test below. A mint
carries no origin -- its identity is the outpoint it is spent at -- so the
first spend brings a lineage into existence and every hop afterwards carries
the same 32 bytes forward, unchanged, all the way down the chain.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import start_nodes, assert_equal, assert_raises_jsonrpc
from test_framework.mininode import (
    CTransaction, CTxIn, CTxOut, FromHex, ToHex, COutPoint, COIN
)
from test_framework.script import (
    CScript, OP_INSPECT, OP_INSPECT_SELF, OP_MINT, OP_EQUAL, OP_TRUE,
)
from test_framework.uap import (
    MANDATORY,
    make_key, mint_script, transfer_script, origin_of, sign_spend,
)
from decimal import Decimal


class UAPMintTransferSpendTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    def get_coinbase_utxo(self, minimum_amount=Decimal('1500')):
        """Get an unspent UTXO with sufficient balance to fund a mint."""
        node = self.nodes[0]
        for utxo in node.listunspent():
            if utxo['amount'] >= minimum_amount:
                return utxo
        raise ValueError(f"No UTXO with >= {minimum_amount} WHIP found")

    def new_key(self, seed):
        """A deterministic key. Seeds are padded to the 32 bytes make_key
        requires, so the descriptive names here stay readable."""
        return make_key(seed[:32].ljust(32, b'.'))

    def broadcast_mint(self, coinbase_utxo, mint_value, multiplier, key):
        """
        Fund a fresh OP_MINT output of `mint_value` WHIP from coinbase_utxo.
        Creation is unconditional -- the covenant only runs at spend time --
        so the wallet just signs the ordinary coinbase input.

        Returns (txid, script, lineage), where lineage is the origin this
        position will take on when it is first spent at vout 0.
        """
        node = self.nodes[0]
        script = mint_script(key.get_pubkey(), multiplier)

        inputs = [{"txid": coinbase_utxo["txid"], "vout": coinbase_utxo["vout"]}]
        input_amount = Decimal(str(coinbase_utxo["amount"]))
        change_amount = input_amount - mint_value - Decimal('0.01')
        outputs = {
            node.getnewaddress(): float(mint_value),
            node.getnewaddress(): float(change_amount),
        }

        raw_tx = node.createrawtransaction(inputs, outputs)
        tx = FromHex(CTransaction(), raw_tx)
        tx.vout[0].scriptPubKey = script
        raw_tx = ToHex(tx)

        signed = node.signrawtransaction(raw_tx)["hex"]
        txid = node.sendrawtransaction(signed)
        node.generate(1)
        return txid, script, origin_of(txid, 0)

    def build_spend(self, prev_txid, prev_vout, prev_script, spender_key,
                    origin, transfer_outputs):
        """Build (but do not send) a spend of the UAP output at
        (prev_txid, prev_vout).

        transfer_outputs: list of (value_whip, pubkey, multiplier) tuples,
        each becoming a continuing OP_MINT_TRANSFER covenant output in
        `origin`'s lineage. The origin is a parameter rather than derived
        here because that distinction IS the protocol: a mint's first spend
        derives it from the outpoint, every later spend carries forward the
        one it was given.
        """
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(prev_txid, 16), prev_vout))]
        tx.vout = [
            CTxOut(int(value * COIN), transfer_script(pubkey, multiplier, origin))
            for value, pubkey, multiplier in transfer_outputs
        ]
        tx.vin[0].scriptSig = sign_spend(prev_script, spender_key, tx, 0)
        return tx

    def spend_uap_input(self, prev_txid, prev_vout, prev_script, spender_key,
                        origin, transfer_outputs):
        """build_spend, then broadcast and mine it. Returns (txid, tx)."""
        node = self.nodes[0]
        tx = self.build_spend(prev_txid, prev_vout, prev_script, spender_key,
                              origin, transfer_outputs)
        txid = node.sendrawtransaction(ToHex(tx))
        node.generate(1)
        return txid, tx

    def run_test(self):
        print("\n=== UAP Mint/Transfer/Spend Tests ===\n")
        print("Demonstrating OP_MINT, OP_MINT_TRANSFER, OP_INSPECT, and OP_INSPECT_SELF...\n")

        self.nodes[0].generate(200)

        self.test_mint_basic()
        print("✓ Basic OP_MINT test passed\n")

        self.test_v1_salted_mint_is_not_a_covenant()
        print("✓ v1 salted mint rejection test passed\n")

        self.test_mint_entry_fee()
        print("✓ OP_MINT entry fee test passed\n")

        self.test_inspect_output()
        print("✓ OP_INSPECT output introspection test passed\n")

        self.test_inspect_self()
        print("✓ OP_INSPECT_SELF script introspection test passed\n")

        self.test_uap_transfer_template()
        print("✓ UAP transfer template covenant test passed\n")

        self.test_covenant_chain()
        print("✓ Covenant chain enforcement test passed\n")

        self.test_covenant_standardness()
        print("✓ Covenant script standardness test passed\n")

        self.test_spend_minted_tokens()
        print("✓ Token spending from OP_MINT test passed\n")

        self.test_multi_level_transactions()
        print("✓ Multi-level transaction covenant test passed\n")

    def test_mint_basic(self):
        """Test basic OP_MINT output creation."""
        node = self.nodes[0]

        key = self.new_key(b"mint_basic_test_key")
        multiplier = 1000

        coinbase = self.get_coinbase_utxo()
        txid, script, lineage = self.broadcast_mint(coinbase, Decimal('1500'), multiplier, key)

        tx_result = node.getrawtransaction(txid, True)
        assert_equal(len(tx_result["vout"]), 2)
        # A mint IS classified, and as a mint -- if Solver stopped
        # recognising it the indexer would never see the token.
        assert_equal(tx_result["vout"][0]["scriptPubKey"]["type"], "op_mint")
        print(f"  Minted UAP position (x{multiplier}): {txid}")
        print(f"  Its lineage, once spent, will be {lineage.hex()[:16]}...")

    def test_v1_salted_mint_is_not_a_covenant(self):
        """The v1 mint carried a random salt: <pubkey> <mult> <salt> OP_MINT.

        v2 removed it, and ParseUapOutputScript now requires the script to
        end immediately after OP_MINT (script.cpp). A leftover v1 script is
        therefore not a covenant at all, and the position is unspendable --
        which is the correct outcome, since the trailing push is
        attacker-choosable bytes that would otherwise sit inside the signed
        preimage.

        This case replaces the old salt-length test. That test asserted a
        salt of at least 16 bytes was required, and it is worth being
        explicit that the rule did not merely relax: the field is gone.
        """
        node = self.nodes[0]

        key = self.new_key(b"v1_salted_mint_key")
        multiplier = 100
        salt = b"a_v1_salt_of_20bytes"
        v1_script = CScript([key.get_pubkey(), multiplier, salt, OP_MINT])

        coinbase = self.get_coinbase_utxo()
        inputs = [{"txid": coinbase["txid"], "vout": coinbase["vout"]}]
        amount = Decimal(str(coinbase["amount"]))
        outputs = {
            node.getnewaddress(): float(Decimal('1500')),
            node.getnewaddress(): float(amount - Decimal('1500') - Decimal('0.01')),
        }
        tx = FromHex(CTransaction(), node.createrawtransaction(inputs, outputs))
        tx.vout[0].scriptPubKey = v1_script
        signed = node.signrawtransaction(ToHex(tx))["hex"]
        mint_txid = node.sendrawtransaction(signed)
        node.generate(1)

        # It is not classified as a covenant: Solver falls through to
        # nonstandard, so the indexer would never index it as a position.
        vout0 = node.getrawtransaction(mint_txid, True)["vout"][0]["scriptPubKey"]
        assert vout0["type"] != "op_mint", \
            "a salted v1 script must not be classified as a v2 mint"
        print(f"  A v1 salted script is not classified as a covenant (type {vout0['type']})")

        # And it cannot be spent into a lineage: the trailing push shifts
        # every element the opcode pops, so the signature is checked against
        # the wrong stack entries and fails.
        spend = self.build_spend(mint_txid, 0, v1_script, key,
                                 origin_of(mint_txid, 0),
                                 [(Decimal('1490'), key.get_pubkey(), multiplier)])
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(spend))
        print("  ...and it cannot be spent into a lineage")

    def test_mint_entry_fee(self):
        """
        OP_MINT enforces an entry fee: the mint position's own value must be
        >= 1000 COIN (WHIP) at spend time, else spending fails.
        """
        node = self.nodes[0]

        key = self.new_key(b"entry_fee_test_key")
        multiplier = 100

        # Below the floor: creation succeeds, spend must fail.
        small_coinbase = self.get_coinbase_utxo(Decimal('600'))
        small_txid, small_script, small_lineage = self.broadcast_mint(
            small_coinbase, Decimal('500'), multiplier, key)
        spend = self.build_spend(small_txid, 0, small_script, key, small_lineage,
                                 [(Decimal('490'), key.get_pubkey(), multiplier)])
        # Asserting the exact reason, not merely that it was refused: this
        # spend is well-formed in every way except the entry fee, so a bare
        # "rejected" would pass even if the fee check disappeared and
        # something else happened to reject it.
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(spend))
        print("  Sub-1000-WHIP position correctly rejected on spend")

        # At the floor: spend succeeds.
        big_coinbase = self.get_coinbase_utxo(Decimal('1500'))
        big_txid, big_script, big_lineage = self.broadcast_mint(
            big_coinbase, Decimal('1000'), multiplier, key)
        spend_txid, _ = self.spend_uap_input(
            big_txid, 0, big_script, key, big_lineage,
            [(Decimal('990'), key.get_pubkey(), multiplier)])
        print(f"  1000-WHIP position spends successfully: {spend_txid}")

    def test_inspect_output(self):
        """OP_INSPECT reads a property of the SPENDING transaction.

        The script has to be SPENT to prove anything. An earlier version of
        this test only created an output carrying an OP_INSPECT script and
        broadcast it -- which executes nothing, so it asserted nothing about
        the opcode at all. It was also written as `0 OP_INSPECT`, reading
        selector 0, which is the transaction *version*, not an output value;
        `nValue` is selector 10 and takes an output index beneath the
        selector. Both mistakes survived because nothing ever ran the script.
        """
        node = self.nodes[0]

        # <index> <selector> OP_INSPECT <expected> OP_EQUAL
        # selector 10 = vout[index].nValue, of the transaction that SPENDS this.
        PAYS = 12345
        inspect_script = CScript([0, 10, OP_INSPECT, PAYS, OP_EQUAL])

        utxo = self.get_coinbase_utxo(Decimal('1'))
        input_amount = Decimal(str(utxo["amount"]))
        inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
        outputs = {
            node.getnewaddress(): 0.01,
            node.getnewaddress(): float(input_amount - Decimal('0.01') - Decimal('0.01')),
        }
        tx = FromHex(CTransaction(), node.createrawtransaction(inputs, outputs))
        tx.vout[0].scriptPubKey = inspect_script
        tx.vout[0].nValue = int(Decimal('0.01') * COIN)
        guarded_txid = node.sendrawtransaction(node.signrawtransaction(ToHex(tx))["hex"])
        node.generate(1)
        guarded_value = tx.vout[0].nValue

        # Spending it runs the script. Output 0 pays exactly PAYS, so the
        # OP_EQUAL succeeds and the spend is accepted.
        spend = CTransaction()
        spend.vin = [CTxIn(COutPoint(int(guarded_txid, 16), 0))]
        spend.vout = [
            CTxOut(PAYS, CScript([OP_TRUE])),
            CTxOut(guarded_value - PAYS - 100000, CScript([OP_TRUE])),
        ]
        spend_txid = node.sendrawtransaction(ToHex(spend))
        node.generate(1)
        assert_equal(node.getrawtransaction(spend_txid, True)["confirmations"] >= 1, True)
        print(f"  OP_INSPECT selector 10 accepted a conforming spend: {spend_txid}")

        # And the negative half, without which the above would pass against an
        # OP_INSPECT that pushed any value at all: pay one satoshi less and
        # the same script must refuse.
        tx2 = FromHex(CTransaction(), node.createrawtransaction(
            [{"txid": utxo["txid"], "vout": utxo["vout"]}],
            {node.getnewaddress(): float(input_amount)}))
        utxo2 = self.get_coinbase_utxo(Decimal('1'))
        tx2 = FromHex(CTransaction(), node.createrawtransaction(
            [{"txid": utxo2["txid"], "vout": utxo2["vout"]}],
            {node.getnewaddress(): 0.01,
             node.getnewaddress(): float(Decimal(str(utxo2["amount"])) - Decimal('0.02'))}))
        tx2.vout[0].scriptPubKey = inspect_script
        tx2.vout[0].nValue = int(Decimal('0.01') * COIN)
        guarded2 = node.sendrawtransaction(node.signrawtransaction(ToHex(tx2))["hex"])
        node.generate(1)

        bad = CTransaction()
        bad.vin = [CTxIn(COutPoint(int(guarded2, 16), 0))]
        bad.vout = [
            CTxOut(PAYS - 1, CScript([OP_TRUE])),
            CTxOut(tx2.vout[0].nValue - (PAYS - 1) - 100000, CScript([OP_TRUE])),
        ]
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(bad))
        print("  ...and refused a spend paying one satoshi less")

    def test_inspect_self(self):
        """OP_INSPECT_SELF pushes the executing script.

        Spent, not merely created -- same reasoning as test_inspect_output.
        `OP_INSPECT_SELF OP_INSPECT_SELF OP_EQUAL` leaves true only if the
        opcode really pushes the same bytes twice.
        """
        node = self.nodes[0]

        inspect_self_script = CScript([OP_INSPECT_SELF, OP_INSPECT_SELF, OP_EQUAL])

        utxo = self.get_coinbase_utxo(Decimal('1'))
        input_amount = Decimal(str(utxo["amount"]))
        tx = FromHex(CTransaction(), node.createrawtransaction(
            [{"txid": utxo["txid"], "vout": utxo["vout"]}],
            {node.getnewaddress(): 0.01,
             node.getnewaddress(): float(input_amount - Decimal('0.02'))}))
        tx.vout[0].scriptPubKey = inspect_self_script
        tx.vout[0].nValue = int(Decimal('0.01') * COIN)
        guarded_txid = node.sendrawtransaction(node.signrawtransaction(ToHex(tx))["hex"])
        node.generate(1)

        spend = CTransaction()
        spend.vin = [CTxIn(COutPoint(int(guarded_txid, 16), 0))]
        spend.vout = [CTxOut(tx.vout[0].nValue - 100000, CScript([OP_TRUE]))]
        spend_txid = node.sendrawtransaction(ToHex(spend))
        node.generate(1)
        assert_equal(node.getrawtransaction(spend_txid, True)["confirmations"] >= 1, True)
        print(f"  OP_INSPECT_SELF script introspection succeeded on spend: {spend_txid}")

    def test_uap_transfer_template(self):
        """A mint position spends into a continuing OP_MINT_TRANSFER covenant."""
        node = self.nodes[0]

        minter_key = self.new_key(b"transfer_template_minter")
        recipient_key = self.new_key(b"transfer_template_recipient")
        multiplier = 1000

        coinbase = self.get_coinbase_utxo(Decimal('1500'))
        mint_txid, mint_scr, lineage = self.broadcast_mint(
            coinbase, Decimal('1200'), multiplier, minter_key)
        print(f"  Minted UAP position: {mint_txid}")

        spend_txid, _ = self.spend_uap_input(
            mint_txid, 0, mint_scr, minter_key, lineage,
            [(Decimal('1190'), recipient_key.get_pubkey(), multiplier)])

        result = node.getrawtransaction(spend_txid, True)
        assert_equal(len(result["vout"]), 1)
        assert_equal(Decimal(str(result["vout"][0]["value"])), Decimal('1190'))
        # The lineage really is on chain, in the output script, where the
        # indexer reads it from.
        assert_equal(result["vout"][0]["scriptPubKey"]["hex"],
                     bytes(transfer_script(recipient_key.get_pubkey(),
                                           multiplier, lineage)).hex())
        print(f"  UAP transfer template covenant accepted: {spend_txid}")

    def test_covenant_chain(self):
        """
        A full OP_MINT -> OP_MINT_TRANSFER x3 chain, each hop signed and
        broadcast for real, demonstrating the covenant can split and
        continue indefinitely while conserving value -- and carrying one
        lineage the whole way.
        """
        print("\n  === Testing Full OP_MINT Covenant Chain ===")

        multiplier = 1000
        minter_key = self.new_key(b"covenant_chain_minter")

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_scr, lineage = self.broadcast_mint(
            coinbase, Decimal('1500'), multiplier, minter_key)
        print(f"  TX1 (mint 1500 WHIP position): {mint_txid}")
        print(f"       lineage {lineage.hex()[:16]}...")

        key_2a = self.new_key(b"covenant_chain_key_2a")
        key_2b = self.new_key(b"covenant_chain_key_2b")
        tx2_txid, tx2 = self.spend_uap_input(
            mint_txid, 0, mint_scr, minter_key, lineage,
            [(Decimal('900'), key_2a.get_pubkey(), multiplier),
             (Decimal('590'), key_2b.get_pubkey(), multiplier)])
        print(f"  TX2 (split 900 + 590, fee 10): {tx2_txid}")

        key_3a = self.new_key(b"covenant_chain_key_3a")
        key_3b = self.new_key(b"covenant_chain_key_3b")
        tx3_txid, tx3 = self.spend_uap_input(
            tx2_txid, 0, tx2.vout[0].scriptPubKey, key_2a, lineage,
            [(Decimal('450'), key_3a.get_pubkey(), multiplier),
             (Decimal('440'), key_3b.get_pubkey(), multiplier)])
        print(f"  TX3 (re-split 450 + 440, fee 10): {tx3_txid}")

        key_4a = self.new_key(b"covenant_chain_key_4a")
        key_4b = self.new_key(b"covenant_chain_key_4b")
        tx4_txid, tx4 = self.spend_uap_input(
            tx3_txid, 0, tx3.vout[0].scriptPubKey, key_3a, lineage,
            [(Decimal('220'), key_4a.get_pubkey(), multiplier),
             (Decimal('220'), key_4b.get_pubkey(), multiplier)])
        print(f"  TX4 (re-split 220 + 220, fee 10): {tx4_txid}")

        # Four hops later, still the lineage the mint's outpoint named. A
        # chain that quietly re-derived the origin at some hop would still
        # split and conserve value correctly, and only this assertion would
        # notice.
        for i in (0, 1):
            assert_equal(tx4.vout[i].scriptPubKey,
                         bytes(transfer_script(
                             [key_4a, key_4b][i].get_pubkey(), multiplier, lineage)))
        print("  ✓ Covenant chain of 4 hops accepted end-to-end, one lineage throughout\n")

    def test_covenant_standardness(self):
        """Covenant outputs are recognised and classified by Solver()."""
        node = self.nodes[0]
        print("\n  === Testing Covenant Script Standardness ===")

        print("  Test 1: OP_MINT output standardness")
        mint_key = self.new_key(b"standardness_mint_key")
        coinbase = self.get_coinbase_utxo(Decimal('1500'))
        mint_txid, mint_scr, lineage = self.broadcast_mint(
            coinbase, Decimal('1200'), 1000, mint_key)
        spk = node.getrawtransaction(mint_txid, True)["vout"][0]["scriptPubKey"]
        assert_equal(spk["type"], "op_mint")
        print(f"  ✓ OP_MINT output recognized as standard: {mint_txid}")

        print("\n  Test 2: OP_MINT_TRANSFER output standardness")
        spend_txid, _ = self.spend_uap_input(
            mint_txid, 0, mint_scr, mint_key, lineage,
            [(Decimal('1190'), mint_key.get_pubkey(), 1000)])
        spk = node.getrawtransaction(spend_txid, True)["vout"][0]["scriptPubKey"]
        assert_equal(spk["type"], "op_mint_transfer")
        print(f"  ✓ OP_MINT_TRANSFER output recognized as standard: {spend_txid}")

    def test_spend_minted_tokens(self):
        """A recipient spends a minted UAP position via a signed covenant transfer."""
        node = self.nodes[0]
        print("\n  === Testing Spend of a Minted UAP Position ===")

        minter_key = self.new_key(b"spend_test_minter")
        recipient_key = self.new_key(b"spend_test_recipient")
        multiplier = 1000

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_scr, lineage = self.broadcast_mint(
            coinbase, Decimal('1500'), multiplier, minter_key)
        print(f"  TX1 (mint 1500 WHIP position): {mint_txid}")

        spend_txid, _ = self.spend_uap_input(
            mint_txid, 0, mint_scr, minter_key, lineage,
            [(Decimal('1499'), recipient_key.get_pubkey(), multiplier)])
        print(f"  TX2 (recipient spends via covenant): {spend_txid}")

        result = node.getrawtransaction(spend_txid, True)
        assert_equal(len(result["vout"]), 1)
        assert_equal(Decimal(str(result["vout"][0]["value"])), Decimal('1499'))
        print("  ✓ Position transferred with conservation (fee = 1 WHIP)")

    def test_multi_level_transactions(self):
        """
        A multi-level covenant chain across several recipients, verifying
        value conservation at each hop.

        TX1: Mint 1000 WHIP position (multiplier 100) -> A
        TX2: A spends -> B (200) + C (790), fee 10
        TX3: C spends -> B (50) + A (50) + C-change (689), fee 1
        TX4: B (from TX2) spends -> D (50) + B (40) + B-change (109), fee 1
        """
        node = self.nodes[0]
        print("\n  === Testing Multi-Level Transactions ===")

        # Any multiplier in [0, INT32_MAX] is usable. It used to be the case
        # that 1-16 was unusable -- ParseUapOutputScript demanded a data
        # push while MINIMALDATA policy demanded the OP_1..OP_16 small-int
        # opcodes, and nothing could satisfy both -- but consensus now
        # requires exactly the canonical encoding, so the two agree. See
        # uap_canonical_encoding.py, which sweeps the whole range.
        multiplier = 100
        key_a = self.new_key(b"multilevel_key_a")
        key_b = self.new_key(b"multilevel_key_b")
        key_c = self.new_key(b"multilevel_key_c")
        key_d = self.new_key(b"multilevel_key_d")

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_scr, lineage = self.broadcast_mint(
            coinbase, Decimal('1000'), multiplier, key_a)
        print(f"  TX1: mint 1000 WHIP position (x{multiplier}) -> A: {mint_txid}")

        tx2_txid, tx2 = self.spend_uap_input(
            mint_txid, 0, mint_scr, key_a, lineage,
            [(Decimal('200'), key_b.get_pubkey(), multiplier),
             (Decimal('790'), key_c.get_pubkey(), multiplier)])
        print(f"  TX2: A spends -> B(200) + C(790): {tx2_txid}")

        tx3_txid, tx3 = self.spend_uap_input(
            tx2_txid, 1, tx2.vout[1].scriptPubKey, key_c, lineage,
            [(Decimal('50'), key_b.get_pubkey(), multiplier),
             (Decimal('50'), key_a.get_pubkey(), multiplier),
             (Decimal('689'), key_c.get_pubkey(), multiplier)])
        print(f"  TX3: C spends -> B(50) + A(50) + C-change(689): {tx3_txid}")

        tx4_txid, tx4 = self.spend_uap_input(
            tx2_txid, 0, tx2.vout[0].scriptPubKey, key_b, lineage,
            [(Decimal('50'), key_d.get_pubkey(), multiplier),
             (Decimal('40'), key_b.get_pubkey(), multiplier),
             (Decimal('109'), key_b.get_pubkey(), multiplier)])
        print(f"  TX4: B spends -> D(50) + B(40) + B-change(109): {tx4_txid}")

        tx3_data = node.getrawtransaction(tx3_txid, True)
        tx4_data = node.getrawtransaction(tx4_txid, True)

        assert_equal(len(tx3_data["vout"]), 3)
        assert_equal(len(tx4_data["vout"]), 3)

        assert_equal(Decimal(str(tx3_data["vout"][0]["value"])), Decimal('50'))
        assert_equal(Decimal(str(tx3_data["vout"][1]["value"])), Decimal('50'))
        assert_equal(Decimal(str(tx3_data["vout"][2]["value"])), Decimal('689'))
        assert_equal(Decimal(str(tx4_data["vout"][0]["value"])), Decimal('50'))
        assert_equal(Decimal(str(tx4_data["vout"][1]["value"])), Decimal('40'))
        assert_equal(Decimal(str(tx4_data["vout"][2]["value"])), Decimal('109'))

        # Two independent branches of the same lineage, and both still name
        # it. Splitting a position must not fork its identity.
        for data in (tx3_data, tx4_data):
            for v in data["vout"]:
                assert lineage.hex() in v["scriptPubKey"]["hex"], \
                    "every branch of a split must carry the same lineage"

        print("  ✓ Multi-level covenant chain (mint -> 3 hops) conserves value at each step")


if __name__ == '__main__':
    UAPMintTransferSpendTest().main()
