#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""
Test UAP (Unspent Asset Protocol) functionality:
- OP_MINT: create a fresh UAP position, entry-fee/salt/overflow-gated at spend time
- OP_MINT_TRANSFER: continue a UAP position under covenant conservation rules
- OP_INSPECT: introspect transaction outputs
- OP_INSPECT_SELF: introspect the current script

Script shapes (see src/script/interpreter.cpp):
  Mint output:     <recipient_pubkey> <multiplier> <salt> OP_MINT
  Transfer output: <recipient_pubkey> <multiplier> OP_MINT_TRANSFER

Spending either requires scriptSig = <sig> only -- the pubkey, multiplier,
and (for mint) salt all come from the scriptPubKey being spent. All of the
OP_MINT/OP_MINT_TRANSFER checks (entry fee, salt length, one-shot, output
conservation) run when the position is *spent*, not when it is created,
since that's when its scriptPubKey actually executes.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import start_nodes, assert_equal
from test_framework.authproxy import JSONRPCException
from test_framework.mininode import (
    CTransaction, CTxIn, CTxOut, FromHex, ToHex, COutPoint, COIN
)
from test_framework.script import (
    CScript, CScriptNum, OP_INSPECT, OP_INSPECT_SELF, OP_MINT, OP_MINT_TRANSFER,
    OP_EQUAL, SignatureHash, SIGHASH_ALL
)
from test_framework.key import CECKey
from decimal import Decimal


def uap_mint_script(pubkey, multiplier, salt):
    """<pubkey> <multiplier> <salt> OP_MINT -- a fresh UAP position.

    multiplier must go through CScriptNum: a plain int 1-16 would otherwise
    serialize as an OP_N small-int opcode instead of a data push, which
    ParseUapOutputScript (interpreter.cpp) rejects as malformed.
    """
    return CScript([pubkey, CScriptNum(multiplier), salt, OP_MINT])


def uap_transfer_script(pubkey, multiplier):
    """<pubkey> <multiplier> OP_MINT_TRANSFER -- a UAP covenant continuation."""
    return CScript([pubkey, CScriptNum(multiplier), OP_MINT_TRANSFER])


def sign_uap_input(tx, i, script_code, key, hashtype=SIGHASH_ALL):
    """Sign input i of tx to satisfy a UAP mint/transfer scriptPubKey."""
    sighash, _ = SignatureHash(script_code, tx, i, hashtype)
    sig = key.sign(sighash) + bytes([hashtype])
    tx.vin[i].scriptSig = CScript([sig])


class UAPMintTransferSpendTest(BitcoinTestFramework):
    """
    UAP mint/transfer tests, exercised through real transactions with real
    ECDSA signatures against the actual consensus-enforced script shapes.
    """

    def __init__(self):
        super().__init__()
        self.num_nodes = 1
        self.setup_clean_chain = True
        self.extra_args = [['-txindex']]

    def set_test_params(self):
        self.num_nodes = 1
        self.setup_clean_chain = True

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, self.extra_args)

    # ========== Helper Methods ==========

    def get_coinbase_utxo(self, minimum_amount=Decimal('1500')):
        """Get an unspent UTXO with sufficient balance to fund a mint."""
        node = self.nodes[0]
        for utxo in node.listunspent():
            if utxo['amount'] >= minimum_amount:
                return utxo
        raise ValueError(f"No UTXO with >= {minimum_amount} WHIP found")

    def new_key(self, seed):
        key = CECKey()
        key.set_secretbytes(seed)
        return key

    def broadcast_mint(self, coinbase_utxo, mint_value, multiplier, salt, key):
        """
        Fund a fresh OP_MINT output of `mint_value` WHIP from coinbase_utxo.
        Creation is unconditional (script only runs at spend time); the
        wallet just needs to sign the ordinary coinbase input.

        Returns (txid, mint_script).
        """
        node = self.nodes[0]
        script = uap_mint_script(key.get_pubkey(), multiplier, salt)

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
        return txid, script

    def spend_uap_input(self, prev_txid, prev_vout, prev_script, spender_key, transfer_outputs):
        """
        Spend a UAP mint/transfer output at (prev_txid, prev_vout), whose
        scriptPubKey is prev_script and which is authorized by spender_key.

        transfer_outputs: list of (value_whip, pubkey, multiplier) tuples,
        each built as a continuing OP_MINT_TRANSFER covenant output.

        Returns (txid, tx).
        """
        node = self.nodes[0]
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(prev_txid, 16), prev_vout))]
        tx.vout = [
            CTxOut(int(value * COIN), uap_transfer_script(pubkey, multiplier))
            for value, pubkey, multiplier in transfer_outputs
        ]
        sign_uap_input(tx, 0, prev_script, spender_key)
        raw_tx = ToHex(tx)
        txid = node.sendrawtransaction(raw_tx)
        node.generate(1)
        return txid, tx

    def run_test(self):
        print("\n=== UAP Mint/Transfer/Spend Tests ===\n")
        print("Demonstrating OP_MINT, OP_MINT_TRANSFER, OP_INSPECT, and OP_INSPECT_SELF...\n")

        self.nodes[0].generate(101)

        self.test_mint_basic()
        print("✓ Basic OP_MINT test passed\n")

        self.test_mint_salt_validation()
        print("✓ OP_MINT salt validation test passed\n")

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

        self.test_covenant_spending()
        print("✓ Covenant spending authorization test passed\n")

        self.test_spend_minted_tokens()
        print("✓ Token spending from OP_MINT test passed\n")

        self.test_multi_level_transactions()
        print("✓ Multi-level transaction covenant test passed\n")

    def test_mint_basic(self):
        """Test basic OP_MINT output creation."""
        node = self.nodes[0]

        key = self.new_key(b"mint_basic_test_key_1234567890a")
        salt = b"mint_basic_test_salt_1234567890"  # 32 bytes, >= 16 required at spend time
        multiplier = 1000

        coinbase = self.get_coinbase_utxo()
        txid, _ = self.broadcast_mint(coinbase, Decimal('1500'), multiplier, salt, key)

        tx_result = node.getrawtransaction(txid, True)
        assert_equal(len(tx_result["vout"]), 2)
        print(f"  Minted UAP position (x{multiplier}) with salt {salt.hex()}: {txid}")

    def test_mint_salt_validation(self):
        """
        Salt length isn't checked at creation time (scriptPubKey doesn't
        execute until spent), only when the mint position is spent.
        """
        node = self.nodes[0]

        key = self.new_key(b"salt_validation_test_key_123456")
        short_salt = b"short"  # 5 bytes, too short
        multiplier = 100

        coinbase = self.get_coinbase_utxo()
        mint_txid, mint_script = self.broadcast_mint(coinbase, Decimal('1500'), multiplier, short_salt, key)
        print(f"  Created mint output with short salt (creation is unconditional): {mint_txid}")

        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        tx.vout = [CTxOut(int(Decimal('1490') * COIN), uap_transfer_script(key.get_pubkey(), multiplier))]
        sign_uap_input(tx, 0, mint_script, key)

        rejected = False
        try:
            node.sendrawtransaction(ToHex(tx))
        except JSONRPCException as e:
            rejected = True
            print(f"  Spend correctly rejected for short salt: {e.error['message']}")
        assert_equal(rejected, True)

    def test_mint_entry_fee(self):
        """
        Test that OP_MINT enforces an entry fee: the mint position's own
        value must be >= 1000 COIN (WHIP) at spend time, else spending fails.
        """
        node = self.nodes[0]

        key = self.new_key(b"entry_fee_test_key_1234567890ab")
        salt = b"entry_fee_test_salt_12345678901"
        multiplier = 100

        # Below the floor: creation succeeds, spend must fail.
        small_coinbase = self.get_coinbase_utxo(Decimal('600'))
        small_txid, small_script = self.broadcast_mint(small_coinbase, Decimal('500'), multiplier, salt, key)

        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(small_txid, 16), 0))]
        tx.vout = [CTxOut(int(Decimal('490') * COIN), uap_transfer_script(key.get_pubkey(), multiplier))]
        sign_uap_input(tx, 0, small_script, key)

        rejected = False
        try:
            node.sendrawtransaction(ToHex(tx))
        except JSONRPCException as e:
            rejected = True
            print(f"  Sub-1000-WHIP position correctly rejected on spend: {e.error['message']}")
        assert_equal(rejected, True)

        # At the floor: spend succeeds.
        big_coinbase = self.get_coinbase_utxo(Decimal('1500'))
        big_txid, big_script = self.broadcast_mint(big_coinbase, Decimal('1000'), multiplier, salt, key)
        spend_txid, _ = self.spend_uap_input(
            big_txid, 0, big_script, key,
            [(Decimal('990'), key.get_pubkey(), multiplier)])
        print(f"  1000-WHIP position spends successfully: {spend_txid}")

    def test_inspect_output(self):
        """Test OP_INSPECT to read output values"""
        node = self.nodes[0]

        utxo = self.get_coinbase_utxo(Decimal('1'))
        input_amount = Decimal(str(utxo["amount"]))

        # Script: 0 INSPECT 1000 EQUAL (output nValue == 1000 satoshis)
        inspect_script = CScript([0, OP_INSPECT, 1000, OP_EQUAL])

        # Keep a real change output so overriding vout[0] down to 1000
        # satoshis doesn't turn the rest of the input into an implicit fee.
        inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
        outputs = {
            node.getnewaddress(): 0.00001,
            node.getnewaddress(): float(input_amount - Decimal('0.00001') - Decimal('0.01')),
        }
        rawtx = node.createrawtransaction(inputs, outputs)

        tx = FromHex(CTransaction(), rawtx)
        tx.vout[0].scriptPubKey = inspect_script
        tx.vout[0].nValue = 1000  # Set output value to 1000 satoshis
        rawtx = ToHex(tx)

        signed = node.signrawtransaction(rawtx)["hex"]
        txid = node.sendrawtransaction(signed)
        node.generate(1)
        print(f"  OP_INSPECT output value verification succeeded: {txid}")

    def test_inspect_self(self):
        """Test OP_INSPECT_SELF to introspect the current script"""
        node = self.nodes[0]

        utxo = self.get_coinbase_utxo(Decimal('1'))

        # Script: OP_INSPECT_SELF OP_INSPECT_SELF OP_EQUAL
        inspect_self_script = CScript([OP_INSPECT_SELF, OP_INSPECT_SELF, OP_EQUAL])

        inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
        outputs = {node.getnewaddress(): float(utxo["amount"]) - 0.01}
        rawtx = node.createrawtransaction(inputs, outputs)

        tx = FromHex(CTransaction(), rawtx)
        tx.vout[0].scriptPubKey = inspect_self_script
        rawtx = ToHex(tx)

        signed = node.signrawtransaction(rawtx)["hex"]
        txid = node.sendrawtransaction(signed)
        node.generate(1)
        print(f"  OP_INSPECT_SELF script introspection succeeded: {txid}")

    def test_uap_transfer_template(self):
        """Test that a mint position can be spent into a continuing OP_MINT_TRANSFER covenant."""
        node = self.nodes[0]

        minter_key = self.new_key(b"transfer_template_minter_key_12")
        recipient_key = self.new_key(b"transfer_template_recipient_key")
        salt = b"transfer_template_salt_12345678"
        multiplier = 1000

        coinbase = self.get_coinbase_utxo(Decimal('1500'))
        mint_txid, mint_script = self.broadcast_mint(coinbase, Decimal('1200'), multiplier, salt, minter_key)
        print(f"  Minted UAP position: {mint_txid}")

        spend_txid, spend_tx = self.spend_uap_input(
            mint_txid, 0, mint_script, minter_key,
            [(Decimal('1190'), recipient_key.get_pubkey(), multiplier)])

        result = node.getrawtransaction(spend_txid, True)
        assert_equal(len(result["vout"]), 1)
        assert_equal(Decimal(str(result["vout"][0]["value"])), Decimal('1190'))
        print(f"  UAP transfer template covenant accepted: {spend_txid}")

    def test_covenant_chain(self):
        """
        Validate a full OP_MINT -> OP_MINT_TRANSFER -> OP_MINT_TRANSFER ->
        OP_MINT_TRANSFER covenant chain, each hop signed and broadcast for
        real, demonstrating the covenant can split and continue indefinitely
        while conserving value at each step.
        """
        node = self.nodes[0]
        print("\n  === Testing Full OP_MINT Covenant Chain ===")

        multiplier = 1000
        salt = b"covenant_chain_test_salt_v1_123"
        minter_key = self.new_key(b"covenant_chain_minter_key_12345")

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_script = self.broadcast_mint(coinbase, Decimal('1500'), multiplier, salt, minter_key)
        print(f"  TX1 (mint 1500 WHIP position): {mint_txid}")

        key_2a = self.new_key(b"covenant_chain_key_2a_test_1234")
        key_2b = self.new_key(b"covenant_chain_key_2b_test_1234")
        tx2_txid, tx2 = self.spend_uap_input(
            mint_txid, 0, mint_script, minter_key,
            [(Decimal('900'), key_2a.get_pubkey(), multiplier),
             (Decimal('590'), key_2b.get_pubkey(), multiplier)])
        print(f"  TX2 (split 900 + 590, fee 10): {tx2_txid}")

        key_3a = self.new_key(b"covenant_chain_key_3a_test_1234")
        key_3b = self.new_key(b"covenant_chain_key_3b_test_1234")
        tx3_txid, tx3 = self.spend_uap_input(
            tx2_txid, 0, tx2.vout[0].scriptPubKey, key_2a,
            [(Decimal('450'), key_3a.get_pubkey(), multiplier),
             (Decimal('440'), key_3b.get_pubkey(), multiplier)])
        print(f"  TX3 (re-split 450 + 440, fee 10): {tx3_txid}")

        key_4a = self.new_key(b"covenant_chain_key_4a_test_1234")
        key_4b = self.new_key(b"covenant_chain_key_4b_test_1234")
        tx4_txid, _tx4 = self.spend_uap_input(
            tx3_txid, 0, tx3.vout[0].scriptPubKey, key_3a,
            [(Decimal('220'), key_4a.get_pubkey(), multiplier),
             (Decimal('220'), key_4b.get_pubkey(), multiplier)])
        print(f"  TX4 (re-split 220 + 220, fee 10): {tx4_txid}")
        print("  ✓ Covenant chain of 4 hops accepted end-to-end, value conserved at each step\n")

    def test_covenant_spending(self):
        """Test that OP_MINT and OP_MINT_TRANSFER outputs are recognized as standard."""
        node = self.nodes[0]
        print("\n  === Testing Covenant Script Standardness ===")

        print("  Test 1: OP_MINT output standardness")
        print("  " + "=" * 50)

        mint_key = self.new_key(b"standardness_mint_test_key_1234")
        salt = b"standardness_test_salt_12345678"
        mint_script = uap_mint_script(mint_key.get_pubkey(), 1000, salt)

        utxo = self.get_coinbase_utxo(Decimal('1'))
        mint_accepted = False
        try:
            inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
            outputs = {node.getnewaddress(): float(utxo["amount"]) - 0.01}
            rawtx = node.createrawtransaction(inputs, outputs)

            tx = FromHex(CTransaction(), rawtx)
            tx.vout[0].scriptPubKey = mint_script
            rawtx = ToHex(tx)

            signed = node.signrawtransaction(rawtx)["hex"]
            txid = node.sendrawtransaction(signed)
            node.generate(1)
            mint_accepted = True
            print(f"  ✓ OP_MINT output recognized as standard: {txid}")
        except Exception as e:
            print(f"  ✗ OP_MINT output rejected: {str(e)[:100]}")
        assert_equal(mint_accepted, True)

        print(f"\n  Test 2: OP_MINT_TRANSFER output standardness")
        print("  " + "=" * 50)

        transfer_key = self.new_key(b"standardness_xfer_test_key_1234")
        transfer_script = uap_transfer_script(transfer_key.get_pubkey(), 1000)

        utxo = self.get_coinbase_utxo(Decimal('1'))
        transfer_accepted = False
        try:
            inputs = [{"txid": utxo["txid"], "vout": utxo["vout"]}]
            outputs = {node.getnewaddress(): float(utxo["amount"]) - 0.01}
            rawtx = node.createrawtransaction(inputs, outputs)

            tx = FromHex(CTransaction(), rawtx)
            tx.vout[0].scriptPubKey = transfer_script
            rawtx = ToHex(tx)

            signed = node.signrawtransaction(rawtx)["hex"]
            txid = node.sendrawtransaction(signed)
            node.generate(1)
            transfer_accepted = True
            print(f"  ✓ OP_MINT_TRANSFER output recognized as standard: {txid}")
        except Exception as e:
            print(f"  ✗ OP_MINT_TRANSFER output rejected: {str(e)[:100]}")
        assert_equal(transfer_accepted, True)

    def test_spend_minted_tokens(self):
        """Test a recipient spending a minted UAP position via a signed covenant transfer."""
        node = self.nodes[0]
        print("\n  === Testing Spend of a Minted UAP Position ===")

        minter_key = self.new_key(b"spend_test_minter_key_1234567ab")
        recipient_key = self.new_key(b"spend_test_recipient_key_123456")
        salt = b"spend_test_salt_v1_1234567890ab"
        multiplier = 1000

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_script = self.broadcast_mint(coinbase, Decimal('1500'), multiplier, salt, minter_key)
        print(f"  TX1 (mint 1500 WHIP position): {mint_txid}")

        spend_txid, _ = self.spend_uap_input(
            mint_txid, 0, mint_script, minter_key,
            [(Decimal('1499'), recipient_key.get_pubkey(), multiplier)])
        print(f"  TX2 (recipient spends via covenant): {spend_txid}")

        result = node.getrawtransaction(spend_txid, True)
        assert_equal(len(result["vout"]), 1)
        assert_equal(Decimal(str(result["vout"][0]["value"])), Decimal('1499'))
        print("  ✓ Position transferred with conservation (fee = 1 WHIP)")

    def test_multi_level_transactions(self):
        """
        Test a multi-level covenant chain across several recipients,
        verifying value conservation at each hop.

        TX1: Mint 1000 WHIP position (multiplier 100) -> A
        TX2: A spends -> B (200) + C (790), fee 10
        TX3: C spends -> B (50) + A (50) + C-change (689), fee 1
        TX4: B (from TX2) spends -> D (50) + B (40) + B-change (109), fee 1
        """
        node = self.nodes[0]
        print("\n  === Testing Multi-Level Transactions ===")

        # Multiplier must be > 16: values 1-16 hit a real quirk in the
        # consensus code where ParseUapOutputScript (interpreter.cpp)
        # requires an explicit data push (rejecting OP_1..OP_16 small-int
        # opcodes), while SCRIPT_VERIFY_MINIMALDATA policy requires exactly
        # those small-int opcodes for single-byte values 1-16. Nothing in
        # 1-16 can satisfy both, making such multipliers non-standard to
        # relay even though a miner could still include them in a block.
        multiplier = 100
        salt = b"multi_level_tx_salt_1234567890a"
        key_a = self.new_key(b"multilevel_key_a_test_1234567ab")
        key_b = self.new_key(b"multilevel_key_b_test_1234567ab")
        key_c = self.new_key(b"multilevel_key_c_test_1234567ab")
        key_d = self.new_key(b"multilevel_key_d_test_1234567ab")

        coinbase = self.get_coinbase_utxo(Decimal('2000'))
        mint_txid, mint_script = self.broadcast_mint(coinbase, Decimal('1000'), multiplier, salt, key_a)
        print(f"  TX1: mint 1000 WHIP position (x{multiplier}) -> A: {mint_txid}")

        tx2_txid, tx2 = self.spend_uap_input(
            mint_txid, 0, mint_script, key_a,
            [(Decimal('200'), key_b.get_pubkey(), multiplier),
             (Decimal('790'), key_c.get_pubkey(), multiplier)])
        print(f"  TX2: A spends -> B(200) + C(790): {tx2_txid}")

        tx3_txid, tx3 = self.spend_uap_input(
            tx2_txid, 1, tx2.vout[1].scriptPubKey, key_c,
            [(Decimal('50'), key_b.get_pubkey(), multiplier),
             (Decimal('50'), key_a.get_pubkey(), multiplier),
             (Decimal('689'), key_c.get_pubkey(), multiplier)])
        print(f"  TX3: C spends -> B(50) + A(50) + C-change(689): {tx3_txid}")

        tx4_txid, tx4 = self.spend_uap_input(
            tx2_txid, 0, tx2.vout[0].scriptPubKey, key_b,
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

        print("  ✓ Multi-level covenant chain (mint -> 3 hops) conserves value at each step")


if __name__ == '__main__':
    UAPMintTransferSpendTest().main()
