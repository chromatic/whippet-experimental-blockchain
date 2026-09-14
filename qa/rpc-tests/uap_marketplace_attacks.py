#!/usr/bin/env python3
# Copyright (c) 2026 The Whippet Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

"""Attack the whole token lifecycle -- mint, sell, buy, melt -- at the node.

The other UAP tests each prove one rule. This one walks the path a user's
money actually takes and, at every step where value changes hands, tries to
take it. A marketplace is where the covenant stops being a script question
and starts being a theft question, so the cases here are organised by what
the attacker is trying to walk away with, not by which function refuses them:

  A. the maker's money   -- a taker who fills a standing order for less
                            than it says, or pays somebody else, or moves
                            the signed input to an index where SIGHASH_SINGLE
                            degenerates into a blank cheque.
  B. tokens from nothing -- merging two lineages' backing into one, renaming
                            a lineage, upgrading its multiplier, or minting a
                            fresh lineage head out of an existing position's
                            backing.
  C. the backing         -- melting a position to recover its coins, which is
                            legitimate and must work; and melting one that
                            isn't yours, or melting a lineage out of
                            existence, which must not.
  D. identity            -- two mints by the same key with the same
                            multiplier are different tokens, and neither can
                            pass itself off as the other.

Every rejection is asserted against a specific reason string. Two rules can
refuse the same bad transaction, and a test that asserts only "rejected"
lets one of them quietly cover for the other being deleted. Where a case
would trip more than one rule, it is built so that it trips exactly the one
under test -- see the degenerate-sighash case, which carries a full set of
conservation-satisfying outputs for precisely that reason.

Each attack is paired with a positive control built by the same code path,
differing only in the one thing being attacked. A test that rejects
everything proves nothing.
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
from test_framework.script import (
    CScript, SignatureHash, SIGHASH_ALL, SIGHASH_SINGLE, SIGHASH_ANYONECANPAY,
)
from test_framework.uap import (
    COIN, FEE, MINT_ENTRY_FEE, MANDATORY, NO_INPUT,
    make_key, mint_script, transfer_script, p2pkh_script, origin_of, sign_spend,
)

MULTIPLIER = 100        # >16, so the push is unambiguously canonical
PAYMENT_VALUE = 40 * COIN
MELT_REMAINDER = 1 * COIN   # what a melt leaves behind in the covenant


def sign_p2pkh(script_code, key, tx, n_in, hashtype=SIGHASH_ALL):
    sighash, err = SignatureHash(script_code, tx, n_in, hashtype)
    assert err is None, "SignatureHash failed: %s" % (err,)
    return CScript([key.sign(sighash) + bytes([hashtype]), key.get_pubkey()])


class UAPMarketplaceAttacksTest(BitcoinTestFramework):
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

    def create_output(self, node, script, value):
        """Put one output carrying `script` and holding `value` on chain.

        Only ever used here for mint scripts and ordinary P2PKH. A transfer
        script cannot be conjured this way any more -- CheckUapOutputCreation
        refuses an output naming a lineage the transaction does not spend,
        which is the whole point of uap_output_provenance.py. So every
        position below descends from a real, fee-paying mint, exactly as a
        user's would.
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
            spk = node.validateaddress(node.getnewaddress())["scriptPubKey"]
            tx.vout.append(CTxOut(change, CScript(bytes.fromhex(spk))))

        signed = node.signrawtransaction(ToHex(tx))
        assert signed["complete"]
        txid = node.sendrawtransaction(signed["hex"])
        node.generate(1)
        return txid

    def mint_a_token(self, node, owner, multiplier=MULTIPLIER):
        """Mint, then spend the mint into its lineage.

        Returns (position_txid, lineage, value). The second transaction is
        not ceremony: a mint has no lineage of its own -- its identity is
        the outpoint it is spent at -- so a token only exists once its mint
        has been spent once.
        """
        pub = owner.get_pubkey()
        mint_txid = self.create_output(node, mint_script(pub, multiplier), MINT_ENTRY_FEE)
        lineage = origin_of(mint_txid, 0)
        value = MINT_ENTRY_FEE - FEE

        spend = CTransaction()
        spend.vin = [CTxIn(COutPoint(int(mint_txid, 16), 0))]
        spend.vout = [CTxOut(value, transfer_script(pub, multiplier, lineage))]
        spend.vin[0].scriptSig = sign_spend(
            mint_script(pub, multiplier), owner, spend, 0)
        position_txid = node.sendrawtransaction(ToHex(spend))
        node.generate(1)
        return position_txid, lineage, value

    def p2pkh_utxo(self, node, key, value):
        """An ordinary coin under a key this test controls, for paying with."""
        return self.create_output(node, p2pkh_script(key.get_pubkey()), value)

    def sign_maker_order(self, key, position_txid, position_script,
                         payment_script, payment_value):
        """The maker's standing offer: SIGHASH_SINGLE|ANYONECANPAY over a
        one-input, one-output transaction -- their position in, their asking
        price out. The Python equivalent of uap-js's signMakerOrder().

        ANYONECANPAY leaves every other input free, and SIGHASH_SINGLE binds
        only the output at the signed input's own index. That is what lets a
        stranger complete it without the maker; it is also the entire attack
        surface tested in group A.
        """
        order = CTransaction()
        order.vin = [CTxIn(COutPoint(int(position_txid, 16), 0))]
        order.vout = [CTxOut(payment_value, payment_script)]
        return sign_spend(position_script, key, order, 0,
                          SIGHASH_SINGLE | SIGHASH_ANYONECANPAY)

    def build_fill(self, maker_scriptsig, position_txid, position_value,
                   payment_script, payment_value, taker_key, taker_utxos,
                   taker_pub, lineage, maker_input_index=0,
                   token_value=None, change_value=None):
        """Assemble a taker's fill. One builder for the honest fill and for
        every tampered variant, so a negative case differs from the positive
        one only in the argument under test.

        `maker_input_index` places the maker's already-signed input. Anything
        but 0 desynchronises SIGHASH_SINGLE from the payment output, and an
        index at or past the output count degenerates the sighash entirely.
        """
        token_value = position_value if token_value is None else token_value

        taker_in = [CTxIn(COutPoint(int(t, 16), 0)) for t, _ in taker_utxos]
        maker_in = CTxIn(COutPoint(int(position_txid, 16), 0))
        maker_in.scriptSig = maker_scriptsig

        tx = CTransaction()
        tx.vin = list(taker_in)
        tx.vin.insert(maker_input_index, maker_in)

        tx.vout = [
            CTxOut(payment_value, payment_script),
            CTxOut(token_value, transfer_script(taker_pub, MULTIPLIER, lineage)),
        ]
        if change_value:
            tx.vout.append(CTxOut(change_value, p2pkh_script(taker_pub)))

        taker_code = p2pkh_script(taker_key.get_pubkey())
        for i, vin in enumerate(tx.vin):
            if i == maker_input_index:
                continue
            vin.scriptSig = sign_p2pkh(taker_code, taker_key, tx, i)
        return tx

    # ------------------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        node.generate(200)

        maker = make_key(b"uap-market-attacks-maker-seed-00")
        taker = make_key(b"uap-market-attacks-taker-seed-00")
        thief = make_key(b"uap-market-attacks-thief-seed-00")
        maker_pub, taker_pub, thief_pub = (
            maker.get_pubkey(), taker.get_pubkey(), thief.get_pubkey())

        self.group_a(node, maker, taker, thief, maker_pub, taker_pub, thief_pub)
        self.group_b(node, maker, maker_pub, thief_pub)
        self.group_c(node, maker, thief, maker_pub, thief_pub)
        self.group_d(node, maker, maker_pub)

        print("\nEvery attack on the token lifecycle was refused, "
              "and every honest path went through.\n")

    # ------------------------------------------------------------------
    # A. taking the maker's money
    # ------------------------------------------------------------------

    def group_a(self, node, maker, taker, thief, maker_pub, taker_pub, thief_pub):
        print("\n=== A. attacks on a standing sell order ===\n")

        position_txid, lineage, position_value = self.mint_a_token(node, maker)
        position_script = transfer_script(maker_pub, MULTIPLIER, lineage)
        payment_script = p2pkh_script(maker_pub)

        maker_sig = self.sign_maker_order(
            maker, position_txid, position_script, payment_script, PAYMENT_VALUE)
        print("  the maker has published an order at %d coin" % (PAYMENT_VALUE // COIN))

        # The taker's own coins. Two of them, because the degenerate-sighash
        # case needs the maker's input to sit at an index the output count
        # cannot reach, and the honest case needs change to come from
        # somewhere.
        def taker_funds(n, each):
            return [(self.p2pkh_utxo(node, taker, each), each) for _ in range(n)]

        # -- A1. pay the maker less than they asked for.
        funds = taker_funds(1, PAYMENT_VALUE + FEE)
        tx = self.build_fill(maker_sig, position_txid, position_value,
                             payment_script, PAYMENT_VALUE - COIN,
                             taker, funds, taker_pub, lineage,
                             change_value=COIN)
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  A1 a fill that skims the asking price is refused")

        # -- A2. pay somebody else entirely.
        tx = self.build_fill(maker_sig, position_txid, position_value,
                             p2pkh_script(thief_pub), PAYMENT_VALUE,
                             taker, funds, taker_pub, lineage)
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  A2 a fill that redirects the payment is refused")

        # -- A3. move the signed input, so SIGHASH_SINGLE binds a different
        #        output than the one the maker priced. The payment output is
        #        untouched and correct; only the index moved.
        tx = self.build_fill(maker_sig, position_txid, position_value,
                             payment_script, PAYMENT_VALUE,
                             taker, funds, taker_pub, lineage,
                             maker_input_index=1)
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  A3 a fill that moves the maker's input off index 0 is refused")

        # -- A4. the blank cheque. Put the maker's input at an index at or
        #        past the output count and SIGHASH_SINGLE degenerates to the
        #        constant uint256(1) -- a hash that commits to no output, no
        #        input and no script. A signature made over that value would
        #        authorise ANY spend of the position.
        #
        #        The maker never made one: signMakerOrder signs a one-in,
        #        one-output transaction at index 0, where the degenerate case
        #        is unreachable by construction. So the signature on file is
        #        over a real hash and cannot verify here.
        #
        #        Built with a full, conservation-satisfying set of outputs so
        #        that the signature is the only thing wrong with it. Two taker
        #        inputs put the maker's at index 2, against two outputs.
        funds2 = taker_funds(2, (PAYMENT_VALUE + FEE) // 2)
        tx = self.build_fill(maker_sig, position_txid, position_value,
                             payment_script, PAYMENT_VALUE,
                             taker, funds2, taker_pub, lineage,
                             maker_input_index=2)
        assert_equal(len(tx.vout), 2)   # the degeneracy this case depends on
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  A4 a fill that degenerates SIGHASH_SINGLE is refused")

        # -- A5. the control: the honest fill, same builder, nothing tampered.
        tx = self.build_fill(maker_sig, position_txid, position_value,
                             payment_script, PAYMENT_VALUE,
                             taker, funds, taker_pub, lineage)
        fill_txid = node.sendrawtransaction(ToHex(tx))
        node.generate(1)
        assert_equal(node.gettxout(fill_txid, 1) is not None, True)
        print("  A5 the honest fill is accepted, and the taker holds the token")

    # ------------------------------------------------------------------
    # B. tokens from nothing
    # ------------------------------------------------------------------

    def group_b(self, node, maker, maker_pub, thief_pub):
        print("\n=== B. attempts to create tokens from nothing ===\n")

        a_txid, a_lineage, a_value = self.mint_a_token(node, maker)
        b_txid, b_lineage, b_value = self.mint_a_token(node, maker)
        a_script = transfer_script(maker_pub, MULTIPLIER, a_lineage)
        b_script = transfer_script(maker_pub, MULTIPLIER, b_lineage)
        print("  two genuine lineages exist, each backed by %d coin"
              % (a_value // COIN))

        def spend_both(outputs):
            tx = CTransaction()
            tx.vin = [CTxIn(COutPoint(int(a_txid, 16), 0)),
                      CTxIn(COutPoint(int(b_txid, 16), 0))]
            tx.vout = outputs
            tx.vin[0].scriptSig = sign_spend(a_script, maker, tx, 0)
            tx.vin[1].scriptSig = sign_spend(b_script, maker, tx, 1)
            return tx

        # -- B1. pour B's backing into A. Both lineages are spent, so both
        #        are represented in the inputs and the provenance rule is
        #        satisfied; it is conservation that has to catch this, because
        #        the sums are only wrong per-lineage, never in total.
        tx = spend_both([
            CTxOut(a_value + b_value - FEE,
                   transfer_script(maker_pub, MULTIPLIER, a_lineage)),
            CTxOut(FEE // 2, transfer_script(maker_pub, MULTIPLIER, b_lineage)),
        ])
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  B1 merging two lineages' backing into one is refused")

        # -- B1 control: the same two inputs, each conserved separately.
        tx = spend_both([
            CTxOut(a_value - FEE, transfer_script(maker_pub, MULTIPLIER, a_lineage)),
            CTxOut(b_value, transfer_script(maker_pub, MULTIPLIER, b_lineage)),
        ])
        merged_txid = node.sendrawtransaction(ToHex(tx))
        node.generate(1)
        print("  B1 control: spending both lineages in one transaction is fine")
        a_txid, a_value, a_vout = merged_txid, a_value - FEE, 0

        def spend_a(outputs):
            tx = CTransaction()
            tx.vin = [CTxIn(COutPoint(int(a_txid, 16), a_vout))]
            tx.vout = outputs
            tx.vin[0].scriptSig = sign_spend(a_script, maker, tx, 0)
            return tx

        # -- B2. keep the lineage, raise the multiplier. Token quantity is
        #        value * multiplier, so this prints tokens out of thin air.
        tx = spend_a([
            CTxOut(a_value - FEE,
                   transfer_script(maker_pub, MULTIPLIER * 10, a_lineage)),
        ])
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, ToHex(tx))
        print("  B2 raising a lineage's multiplier is refused")

        # -- B3. rename A's backing into B, a lineage that genuinely exists
        #        but is not an input here. Without the creation rule this
        #        would let anyone move backing into a token they do not hold.
        tx = spend_a([
            CTxOut(a_value - FEE, transfer_script(thief_pub, MULTIPLIER, b_lineage)),
        ])
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, ToHex(tx))
        print("  B3 renaming a position into another live lineage is refused")

        # -- B4. skim backing off a live position into a fresh lineage head,
        #        which would mint a whole new token without paying the entry
        #        fee -- funded by a token that already exists.
        #
        #        The position survives, carrying all but the skim. That is
        #        deliberate: a spend that left NO covenant output would be
        #        refused for having destroyed the lineage (see C2) and for
        #        conserving nothing, and would prove nothing about mint-shaped
        #        outputs. Mutation testing found exactly that -- disabling the
        #        rule under test left the simpler version of this case green,
        #        caught two rules further down. Keeping the lineage alive is
        #        what makes the mint output the only thing wrong here.
        skim = 10 * COIN
        tx = spend_a([
            CTxOut(a_value - FEE - skim,
                   transfer_script(maker_pub, MULTIPLIER, a_lineage)),
            CTxOut(skim, mint_script(thief_pub, MULTIPLIER)),
        ])
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  B4 skimming backing into a new lineage head is refused")

        self.b_position = (a_txid, a_vout, a_value, a_lineage, a_script)

    # ------------------------------------------------------------------
    # C. the backing
    # ------------------------------------------------------------------

    def group_c(self, node, maker, thief, maker_pub, thief_pub):
        print("\n=== C. melting a position back into spendable coin ===\n")

        txid, vout, value, lineage, script = self.b_position

        def melt(outputs, signer=maker):
            tx = CTransaction()
            tx.vin = [CTxIn(COutPoint(int(txid, 16), vout))]
            tx.vout = outputs
            tx.vin[0].scriptSig = sign_spend(script, signer, tx, 0)
            return tx

        covenant_out = CTxOut(MELT_REMAINDER,
                              transfer_script(maker_pub, MULTIPLIER, lineage))
        cash_out = CTxOut(value - MELT_REMAINDER - FEE, p2pkh_script(maker_pub))

        # -- C1. melting somebody else's position. The covenant names the
        #        maker's pubkey, so only the maker's signature spends it --
        #        no matter how well-formed the rest of the transaction is.
        tx = melt([covenant_out, cash_out], signer=thief)
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  C1 melting a position you do not hold is refused")

        # -- C2. melting the lineage out of existence: take the backing and
        #        leave no covenant output at all. Conservation requires a
        #        surviving position, which is why a melt can only ever
        #        recover the backing minus a permanent remainder.
        tx = melt([CTxOut(value - FEE, p2pkh_script(maker_pub))])
        assert_raises_jsonrpc(-26, MANDATORY, node.sendrawtransaction, ToHex(tx))
        print("  C2 melting a lineage out of existence is refused")

        # -- C3. the control: a real melt. The position survives at the
        #        remainder, and the difference comes out as ordinary coin.
        tx = melt([covenant_out, cash_out])
        melt_txid = node.sendrawtransaction(ToHex(tx))
        node.generate(1)
        assert_equal(int(round(node.gettxout(melt_txid, 0)["value"] * COIN)),
                     MELT_REMAINDER)
        print("  C3 a melt is accepted; the lineage survives at %d coin"
              % (MELT_REMAINDER // COIN))

        # -- C4. and the recovered coin really is ordinary coin: spend it as
        #        a plain payment, with no covenant anywhere in the
        #        transaction. Until this goes through, "recovered" is a claim
        #        about an output's script, not about money.
        onward = CTransaction()
        onward.vin = [CTxIn(COutPoint(int(melt_txid, 16), 1))]
        onward.vout = [CTxOut(cash_out.nValue - FEE, p2pkh_script(thief_pub))]
        onward.vin[0].scriptSig = sign_p2pkh(
            p2pkh_script(maker_pub), maker, onward, 0)
        node.sendrawtransaction(ToHex(onward))
        node.generate(1)
        print("  C4 the recovered backing spends as ordinary coin")

    # ------------------------------------------------------------------
    # D. identity
    # ------------------------------------------------------------------

    def group_d(self, node, maker, maker_pub):
        print("\n=== D. two mints by one key are two different tokens ===\n")

        one_txid, one_lineage, _ = self.mint_a_token(node, maker)
        two_txid, two_lineage, two_value = self.mint_a_token(node, maker)
        assert one_lineage != two_lineage, (
            "same key, same multiplier, same everything except the outpoint -- "
            "and the outpoint is exactly what a lineage IS")
        print("  same key and multiplier, different lineages")

        # The second token cannot pass itself off as the first. This is what
        # stops a ticker being an identity: anyone may mint "WHIP", but no
        # mint can join a lineage it did not originate.
        two_script = transfer_script(maker_pub, MULTIPLIER, two_lineage)
        tx = CTransaction()
        tx.vin = [CTxIn(COutPoint(int(two_txid, 16), 0))]
        tx.vout = [CTxOut(two_value - FEE,
                          transfer_script(maker_pub, MULTIPLIER, one_lineage))]
        tx.vin[0].scriptSig = sign_spend(two_script, maker, tx, 0)
        assert_raises_jsonrpc(-26, NO_INPUT, node.sendrawtransaction, ToHex(tx))
        print("  D  one token cannot be spent into another's lineage")


if __name__ == '__main__':
    UAPMarketplaceAttacksTest().main()
