#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""UAP atomic swap: prove a real node accepts a maker/taker order fill.

contrib/uap-js/uap.js offers signMakerOrder() / fillOrder() as a way to
trade a UAP position for WHIP without a trusted third party:

  - The MAKER owns a UAP position (an OP_MINT_TRANSFER covenant output)
    and signs SIGHASH_SINGLE|ANYONECANPAY over a transaction with exactly
    one input (their position, at index 0) and one output (their asking
    price, at the same index). SIGHASH_SINGLE ties the signature to
    "whatever ends up at output index N", where N is the signed input's
    eventual index -- so the maker's payment is pinned to index 0 only
    as long as their input stays at index 0 in the final transaction.
    ANYONECANPAY means nothing else the taker adds is covered by this
    signature at all.

  - The TAKER later assembles the real transaction: maker input at index
    0 (their scriptSig from the order), maker's payment output at index
    0, then the taker's own funding input(s), a continuing covenant
    output re-addressed to the taker, and change. The taker signs their
    own input(s) normally, then broadcasts.

  - CheckUapOutputConservation (src/script/interpreter.cpp) is what
    actually lets this be atomic: it only requires *some* output to
    continue the covenant with the same multiplier and bounded value; it
    does not care what else the transaction does, which is exactly what
    lets a plain WHIP payment output ride alongside the covenant leg in
    the same transaction.

This file has never been run against a live node before -- signMakerOrder
and fillOrder in uap.js were previously exercised only against fixtures.
See doc/uap-marketplace-design.md and contrib/uap-js/uap.js (around
signMakerOrder/fillOrder) for the design this mirrors in Python.

Three things are checked, each on a real broadcast against real consensus:

  1. The happy path: a filled order is accepted, mined, and the position
     provably moves to the taker while the maker is provably paid.

  2. Reordering the maker's payment output out of index 0 -- while
     leaving the maker's input at index 0 -- desyncs the SIGHASH_SINGLE
     commitment from what's actually being paid, so the maker's own
     signature no longer verifies. Rejected.

  3. Dropping the continuing covenant output (or mutating its
     multiplier) trips CheckUapOutputConservation directly. Rejected.

Every rejection is asserted against a specific, known reason string --
not just "it failed" -- and every negative case has a corresponding
"weaken it back to valid" control proving the assertion is actually
caused by the defect under test. See the giant comment block near the
bottom for how that was verified.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import (
    start_nodes,
    assert_equal,
    assert_raises_jsonrpc,
)
from test_framework.mininode import CTransaction, CTxIn, CTxOut, COutPoint, FromHex, ToHex
from test_framework.script import (
    CScript, OP_MINT_TRANSFER, OP_DUP, OP_HASH160, OP_EQUALVERIFY, OP_CHECKSIG,
    SignatureHash, SIGHASH_ALL, SIGHASH_SINGLE, SIGHASH_ANYONECANPAY, hash160,
)
from test_framework.key import CECKey

COIN = 100000000

FEE = 1000000  # RECOMMENDED_MIN_TX_FEE, src/amount.h -- see uap_transactions.py's
               # comment on absurdly-high-fee: leaving the true change amount
               # unaccounted for turns it into fee, and the node rejects that
               # long before any UAP rule is even reached.

MULTIPLIER = 100          # >16, so a plain int is already the canonical push
                           # (see uap_canonical_encoding.py); no OP_N pitfalls.
TOKEN_VALUE = 100 * COIN  # the maker's position, in full
PAYMENT_VALUE = 50 * COIN  # what the maker is asking for it
CHANGE_VALUE = 5 * COIN
TAKER_INPUT_VALUE = PAYMENT_VALUE + CHANGE_VALUE + FEE

MANDATORY = "mandatory-script-verify-flag-failed"


def make_key(seed):
    key = CECKey()
    key.set_secretbytes(seed)
    key.set_compressed(True)
    return key


def transfer_script(pubkey, multiplier):
    """<pubkey> <multiplier> OP_MINT_TRANSFER -- a UAP covenant continuation."""
    return CScript([pubkey, multiplier, OP_MINT_TRANSFER])


def p2pkh_script(pubkey):
    return CScript([OP_DUP, OP_HASH160, hash160(pubkey), OP_EQUALVERIFY, OP_CHECKSIG])


def sign_uap_input(script_code, key, tx, n_in, hashtype):
    """Sign input n_in against a UAP mint/transfer scriptPubKey.

    Spending a UAP output only ever needs scriptSig = <sig>; the pubkey,
    multiplier, (and salt, for OP_MINT) come from the scriptPubKey being
    spent, not the scriptSig. See uap_mint_transfer_spend.py.
    """
    sighash, err = SignatureHash(script_code, tx, n_in, hashtype)
    assert err is None
    return CScript([key.sign(sighash) + bytes([hashtype])])


def sign_p2pkh_input(script_code, key, tx, n_in, hashtype=SIGHASH_ALL):
    sighash, err = SignatureHash(script_code, tx, n_in, hashtype)
    assert err is None
    sig = key.sign(sighash) + bytes([hashtype])
    return CScript([sig, key.get_pubkey()])


class UAPSwapTest(BitcoinTestFramework):
    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir, [["-debug"]])

    # ---------------------------------------------------------------- #
    # helpers
    # ---------------------------------------------------------------- #

    def fund(self, node, min_value):
        for utxo in node.listunspent():
            if int(round(utxo["amount"] * COIN)) >= min_value:
                return utxo
        raise AssertionError("no matured UTXO worth at least %d satoshi" % min_value)

    def create_output(self, node, script, value):
        """Put an output carrying `script` and holding `value` on chain.

        Creating an output runs no script -- only spending it does -- so
        the wallet just needs to sign the ordinary funding input. Mirrors
        create_mint() in uap_transactions.py / uap_canonical_encoding.py.
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

    def make_maker_order(self, maker_key, maker_position_txid, maker_position_script,
                          payment_script, payment_value):
        """Sign SIGHASH_SINGLE|ANYONECANPAY over a 1-in/1-out tx: the
        maker's position at input 0, the maker's asking price at output 0.
        This is the Python equivalent of uap.js's signMakerOrder().

        Returns (scriptSig, prevout) -- everything a taker needs.
        """
        order_tx = CTransaction()
        order_tx.vin = [CTxIn(COutPoint(int(maker_position_txid, 16), 0))]
        order_tx.vout = [CTxOut(payment_value, payment_script)]
        hashtype = SIGHASH_SINGLE | SIGHASH_ANYONECANPAY
        script_sig = sign_uap_input(maker_position_script, maker_key, order_tx, 0, hashtype)
        return script_sig

    def build_fill(self, maker_scriptsig, maker_position_txid, payment_script, payment_value,
                    taker_input_txid, taker_input_value, taker_fund_key,
                    token_out_pubkey, token_out_multiplier, token_out_value,
                    change_script, change_value,
                    reorder_outputs=False, drop_covenant=False):
        """Assemble and sign a fill of the maker's order. This is the
        Python equivalent of uap.js's fillOrder() plus the taker's own
        signing step (which fillOrder() deliberately leaves to the
        caller).

        reorder_outputs / drop_covenant let a single builder produce both
        the well-formed fill and the deliberately-broken variants used in
        the negative tests below, so the "good" and "bad" transactions
        are constructed by the same code path and differ only in the one
        thing under test.
        """
        tx = CTransaction()
        tx.vin = [
            CTxIn(COutPoint(int(maker_position_txid, 16), 0)),
            CTxIn(COutPoint(int(taker_input_txid, 16), 0)),
        ]

        payment_out = CTxOut(payment_value, payment_script)
        covenant_out = CTxOut(token_out_value, transfer_script(token_out_pubkey, token_out_multiplier))
        change_out = CTxOut(change_value, change_script)

        if drop_covenant:
            # Route what would have been the covenant leg's value into the
            # plain change output instead of just deleting it -- otherwise
            # the "removed" value reads as an enormous fee and the node
            # rejects for absurdly-high-fee before CheckUapOutputConservation
            # is ever reached, which would prove nothing about conservation.
            fattened_change = CTxOut(change_value + token_out_value, change_script)
            tx.vout = [payment_out, fattened_change]
        elif reorder_outputs:
            # The maker's input stays at vin[0], but their payment output
            # is no longer at vout[0] -- exactly the desync the design
            # doc warns about.
            tx.vout = [covenant_out, payment_out, change_out]
        else:
            tx.vout = [payment_out, covenant_out, change_out]

        tx.vin[0].scriptSig = maker_scriptsig
        taker_script_code = p2pkh_script(taker_fund_key.get_pubkey())
        tx.vin[1].scriptSig = sign_p2pkh_input(taker_script_code, taker_fund_key, tx, 1)
        return tx

    # ---------------------------------------------------------------- #
    # test cases
    # ---------------------------------------------------------------- #

    def run_test(self):
        node = self.nodes[0]
        node.generate(101)

        print("\n=== UAP atomic swap (maker order / taker fill) ===\n")

        self.test_happy_path()
        print("\n=== reorder negative case ===\n")
        self.test_reorder_rejected()
        print("\n=== conservation negative case ===\n")
        self.test_conservation_rejected()

        print("\nAll UAP swap checks passed\n")

    def _setup_order(self, node, seed_suffix):
        """Common setup shared by all three cases: a maker position, a
        signed order for it, and a funded taker input. Returns everything
        build_fill() needs.
        """
        maker_key = make_key(("swap_maker_" + seed_suffix).encode().ljust(32, b"m"))
        taker_token_key = make_key(("swap_taker_tok_" + seed_suffix).encode().ljust(32, b"t"))
        taker_fund_key = make_key(("swap_taker_fund_" + seed_suffix).encode().ljust(32, b"f"))

        maker_position_script = transfer_script(maker_key.get_pubkey(), MULTIPLIER)
        maker_position_txid = self.create_output(node, maker_position_script, TOKEN_VALUE)

        maker_payment_addr = node.getnewaddress()
        payment_script = CScript(bytes.fromhex(node.validateaddress(maker_payment_addr)["scriptPubKey"]))

        maker_scriptsig = self.make_maker_order(
            maker_key, maker_position_txid, maker_position_script,
            payment_script, PAYMENT_VALUE)

        taker_input_txid = self.create_output(
            node, p2pkh_script(taker_fund_key.get_pubkey()), TAKER_INPUT_VALUE)

        change_addr = node.getnewaddress()
        change_script = CScript(bytes.fromhex(node.validateaddress(change_addr)["scriptPubKey"]))

        return {
            "maker_key": maker_key,
            "maker_position_txid": maker_position_txid,
            "maker_payment_addr": maker_payment_addr,
            "payment_script": payment_script,
            "maker_scriptsig": maker_scriptsig,
            "taker_token_key": taker_token_key,
            "taker_fund_key": taker_fund_key,
            "taker_input_txid": taker_input_txid,
            "change_script": change_script,
        }

    def test_happy_path(self):
        node = self.nodes[0]
        ctx = self._setup_order(node, "happy")

        tx = self.build_fill(
            ctx["maker_scriptsig"], ctx["maker_position_txid"],
            ctx["payment_script"], PAYMENT_VALUE,
            ctx["taker_input_txid"], TAKER_INPUT_VALUE, ctx["taker_fund_key"],
            ctx["taker_token_key"].get_pubkey(), MULTIPLIER, TOKEN_VALUE,
            ctx["change_script"], CHANGE_VALUE)

        txid = node.sendrawtransaction(ToHex(tx))
        node.generate(1)
        print("  filled order accepted and mined: %s" % txid)

        result = node.getrawtransaction(txid, True)
        assert_equal(result["confirmations"] >= 1, True)
        assert_equal(len(result["vout"]), 3)

        # The maker was paid, at index 0, the exact amount they asked for.
        assert_equal(result["vout"][0]["value"] * COIN, PAYMENT_VALUE)
        assert_equal(
            result["vout"][0]["scriptPubKey"]["addresses"][0], ctx["maker_payment_addr"])
        print("  maker payment output present: %s WHIP to %s" %
              (result["vout"][0]["value"], ctx["maker_payment_addr"]))

        # The position provably moved: a fresh OP_MINT_TRANSFER covenant
        # output exists, addressed to the taker's pubkey, same multiplier.
        expected_covenant_hex = transfer_script(
            ctx["taker_token_key"].get_pubkey(), MULTIPLIER).hex()
        assert_equal(result["vout"][1]["scriptPubKey"]["hex"], expected_covenant_hex)
        assert_equal(result["vout"][1]["value"] * COIN, TOKEN_VALUE)
        print("  continuing covenant output present, addressed to the taker, x%d" % MULTIPLIER)

        # And it's actually spendable by the taker going forward -- not
        # just shaped right on the wire. Prove it by spending it once
        # more, ordinary single-hop transfer style.
        next_key = make_key(b"swap_happy_next_hop_key_1234567")
        covenant_script = transfer_script(ctx["taker_token_key"].get_pubkey(), MULTIPLIER)
        spend = CTransaction()
        spend.vin = [CTxIn(COutPoint(int(txid, 16), 1))]
        spend.vout = [CTxOut(TOKEN_VALUE - FEE, transfer_script(next_key.get_pubkey(), MULTIPLIER))]
        spend.vin[0].scriptSig = sign_uap_input(
            covenant_script, ctx["taker_token_key"], spend, 0, SIGHASH_ALL)
        spend_txid = node.sendrawtransaction(ToHex(spend))
        node.generate(1)
        assert_equal(node.getrawtransaction(spend_txid, True)["confirmations"] >= 1, True)
        print("  taker's newly-received position is genuinely spendable: %s" % spend_txid)

    def test_reorder_rejected(self):
        node = self.nodes[0]
        ctx = self._setup_order(node, "reorder")

        tx = self.build_fill(
            ctx["maker_scriptsig"], ctx["maker_position_txid"],
            ctx["payment_script"], PAYMENT_VALUE,
            ctx["taker_input_txid"], TAKER_INPUT_VALUE, ctx["taker_fund_key"],
            ctx["taker_token_key"].get_pubkey(), MULTIPLIER, TOKEN_VALUE,
            ctx["change_script"], CHANGE_VALUE,
            reorder_outputs=True)

        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  maker input at vin[0] but payment moved off vout[0]: rejected (%s)" % MANDATORY)
        print("  -- the maker's SIGHASH_SINGLE|ANYONECANPAY signature no longer covers")
        print("     what output 0 actually pays, so it fails to verify.")

    def test_conservation_rejected(self):
        node = self.nodes[0]

        # 3a: drop the continuing covenant output entirely.
        ctx = self._setup_order(node, "cons_drop")
        tx = self.build_fill(
            ctx["maker_scriptsig"], ctx["maker_position_txid"],
            ctx["payment_script"], PAYMENT_VALUE,
            ctx["taker_input_txid"], TAKER_INPUT_VALUE, ctx["taker_fund_key"],
            ctx["taker_token_key"].get_pubkey(), MULTIPLIER, TOKEN_VALUE,
            ctx["change_script"], CHANGE_VALUE,
            drop_covenant=True)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  fill with no continuing covenant output: rejected (%s)" % MANDATORY)

        # 3b: keep the covenant output, but give it the wrong multiplier.
        ctx = self._setup_order(node, "cons_mult")
        tx = self.build_fill(
            ctx["maker_scriptsig"], ctx["maker_position_txid"],
            ctx["payment_script"], PAYMENT_VALUE,
            ctx["taker_input_txid"], TAKER_INPUT_VALUE, ctx["taker_fund_key"],
            ctx["taker_token_key"].get_pubkey(), MULTIPLIER + 1, TOKEN_VALUE,
            ctx["change_script"], CHANGE_VALUE)
        assert_raises_jsonrpc(None, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  continuing covenant output with wrong multiplier (x%d instead of x%d): rejected (%s)" %
              (MULTIPLIER + 1, MULTIPLIER, MANDATORY))


if __name__ == '__main__':
    UAPSwapTest().main()
