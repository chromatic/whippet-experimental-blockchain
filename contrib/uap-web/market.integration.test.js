// market.integration.test.js -- proves an order actually FILLS on a real
// whippetd regtest node.
//
// Why this exists: market.test.js can check the *shape* of what buildFillTx
// produces, but nothing in JavaScript can tell you a signature is valid.
// There is no script interpreter here. The maker signs with
// SIGHASH_SINGLE|ANYONECANPAY, committing to their own input and to the
// output at the same index; the taker then rebuilds the transaction around
// that signature, adding inputs and outputs. Whether the maker's signature
// survives that assembly is a question only a node can answer, and getting
// it wrong produces a transaction that looks perfectly well-formed and is
// rejected by every peer on the network.
//
// So this test does the whole trade against a node:
//   mint -> transfer position owned by the maker -> maker signs an order
//   -> taker fills it -> node accepts and mines it.
//
// SAFETY: regtest only, a fresh temp datadir, non-default ports. See
// uap-js/test-node-harness.js.
//
// Usage: node market.integration.test.js
// Skips (exit 0) if whippetd is unavailable, so it can never be a hard
// requirement for `npm test`. The EC library is a checked-in vendored copy
// (../uap-js/secp.js), so it needs no such guard.

import { startNode } from '../uap-js/test-node-harness.js';
import * as secp from '../uap-js/secp.js';
import { satoshisFromCoins, privKeyFromWIF, assert } from '../uap-js/integration-helpers.js';
import * as UAP from '../uap-js/uap.js';
import * as ADDR from '../uap-js/addr.js';
import { describeOrder, planFill, buildFillTx } from './market.js';

const MULTIPLIER = 1000;
const REGTEST_PUBKEY_ADDRESS = ADDR.VERSIONS.regtest.PUBKEY_ADDRESS;

let passed = 0;
let failed = 0;

async function test(name, fn) {
  process.stdout.write(`- ${name} ... `);
  try {
    await fn();
    console.log('PASS');
    passed++;
  } catch (e) {
    console.log('FAIL');
    console.log(`  ${e && e.stack ? e.stack : e}`);
    failed++;
  }
}

const { bytesToHex } = UAP;

async function main() {
  UAP.configureSecp(secp);

  let node = null;
  try {
    node = await startNode({ port: 39601, rpcport: 39602 });
  } catch (e) {
    console.log(`SKIP: ${e.message}`);
    process.exit(0);
  }

  try {
    console.log(`node up: datadir=${node.datadir} rpcport=${node.rpcport}`);

    // ---- wallet setup: the taker pays from this address ----------------
    const walletAddr = await node.rpc('getnewaddress');
    await node.rpc('generatetoaddress', 101, walletAddr);
    const takerPrivKey = privKeyFromWIF(await node.rpc('dumpprivkey', walletAddr));
    const takerPubKey = secp.getPublicKey(takerPrivKey, true);
    assert(
      ADDR.pubkeyToAddress(takerPubKey, REGTEST_PUBKEY_ADDRESS) === walletAddr,
      'derived taker pubkey does not match wallet address'
    );
    const walletScript = ADDR.addressToScript(walletAddr);

    async function confirmMined(txid) {
      await node.rpc('generatetoaddress', 1, walletAddr);
      const tx = await node.rpc('getrawtransaction', txid, 1);
      assert(tx.confirmations >= 1, `expected ${txid} mined, got ${tx.confirmations}`);
      return tx;
    }

    async function freshUtxo(minSats) {
      const utxos = await node.rpc('listunspent', 1, 9999999, [walletAddr]);
      for (const u of utxos) {
        if (satoshisFromCoins(u.amount) >= minSats) return u;
      }
      throw new Error(`no wallet UTXO worth at least ${minSats} sats`);
    }

    // ---- the maker: a separate identity that owns the token ------------
    const makerPrivKey = secp.utils.randomPrivateKey();
    const makerPubKey = secp.getPublicKey(makerPrivKey, true);
    const makerPaymentScript = ADDR.buildP2PKHScript(makerPubKey);

    // ---- build a position the maker will sell --------------------------
    // A mint, then a transfer into a covenant the maker controls. Selling a
    // transfer position (not the mint itself) is the ordinary case: it is
    // what every trade after the first one looks like.
    //
    // This is a function, not a one-off, because every test that broadcasts
    // a fill CONSUMES the position. Sharing one position across tests makes
    // the second broadcast fail with "Missing inputs" -- a double-spend
    // rejection that looks exactly like success in a test that only checks
    // "was it rejected?". The negative case below needs its own.
    async function createMakerPosition() {
      const mintPrivKey = secp.utils.randomPrivateKey();
      const mintPubKey = secp.getPublicKey(mintPrivKey, true);
      const salt = new Uint8Array(20);
      crypto.getRandomValues(salt);
      const mintScript = UAP.buildMintScript(mintPubKey, MULTIPLIER);

      const mintValue = 2000 * UAP.COIN; // above the 1000-coin entry fee
      const fee = UAP.RECOMMENDED_MIN_TX_FEE;
      const utxo = await freshUtxo(mintValue + fee + UAP.DEFAULT_DUST_LIMIT);
      const utxoSats = satoshisFromCoins(utxo.amount);

      const mintTx = {
        version: 1,
        locktime: 0,
        vin: [{ txid: utxo.txid, vout: utxo.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
        vout: [
          { value: mintValue, scriptPubKey: mintScript },
          { value: utxoSats - mintValue - fee, scriptPubKey: walletScript },
        ],
      };
      mintTx.vin[0].scriptSig = UAP.signP2PKHInput(
        secp, UAP.hexToBytes(utxo.scriptPubKey), takerPrivKey, takerPubKey, mintTx, 0);
      const mintTxid = await node.rpc('sendrawtransaction', UAP.txToHex(mintTx));
      await confirmMined(mintTxid);

      // Spend the mint into a covenant addressed to the maker.
      const transferTx = UAP.buildTransferTx(secp, {
        input: {
          txid: mintTxid,
          vout: 0,
          scriptCode: mintScript,
          value: mintValue,
          privKey: mintPrivKey,
        },
        toPubkey: makerPubKey,
        multiplier: MULTIPLIER,
        fee: UAP.RECOMMENDED_MIN_TX_FEE,
      });
      const transferTxid = await node.rpc('sendrawtransaction', UAP.txToHex(transferTx));
      const mined = await confirmMined(transferTxid);

      // The node must agree this is a UAP position, not merely that the
      // transaction was accepted -- if Solver() failed to classify it, the
      // order relay and indexer would never see it either.
      assert(
        mined.vout[0].scriptPubKey.type === 'op_transfer' ||
        mined.vout[0].scriptPubKey.type === 'op_mint_transfer',
        `expected a transfer covenant, node classified it as ${mined.vout[0].scriptPubKey.type}`
      );

      return {
        txid: transferTxid,
        vout: 0,
        multiplier: MULTIPLIER,
        pubkey: bytesToHex(makerPubKey),
        value: transferTx.vout[0].value,
        height: 0,
        scriptPubKey: bytesToHex(UAP.buildTransferScript(makerPubKey, MULTIPLIER, UAP.deriveOrigin(makerOrder.txid, makerOrder.vout))),
      };
    }

    // ---- the maker signs a standing order ------------------------------
    const paymentValue = 5 * UAP.COIN;

    function makeOrder(pos) {
      const signed = UAP.signMakerOrder(secp, {
        input: {
          txid: pos.txid,
          vout: pos.vout,
          scriptCode: UAP.buildTransferScript(makerPubKey, MULTIPLIER, UAP.deriveOrigin(makerOrder.txid, makerOrder.vout)),
        },
        privKey: makerPrivKey,
        paymentScript: makerPaymentScript,
        paymentValue,
      });
      // Shaped exactly as the relay's JSON is, so this exercises the same
      // field names market.js reads in production.
      return {
        txid: signed.input.txid,
        vout: signed.input.vout,
        multiplier: MULTIPLIER,
        pubkey: bytesToHex(makerPubKey),
        script_sig: bytesToHex(signed.scriptSig),
        payment_script: bytesToHex(signed.paymentScript),
        payment_value: signed.paymentValue,
        created_at: Math.floor(Date.now() / 1000),
      };
    }

    let position = null;
    let order = null;

    await test('a transfer position owned by the maker is created on chain', async () => {
      position = await createMakerPosition();
      assert(position.value > 0, 'position carries no value');
    });

    await test('maker signs an order with SIGHASH_SINGLE|ANYONECANPAY', async () => {
      order = makeOrder(position);
      assert(order.script_sig.length > 0, 'order carries no signature');
      const description = describeOrder(order);
      assert(typeof description === 'string' && description.length > 0,
        'describeOrder produced nothing for a real order');
    });

    // ---- the taker fills it --------------------------------------------
    await test('taker fills the order and the node accepts and mines it', async () => {
      const takerUtxo = await freshUtxo(paymentValue + UAP.RECOMMENDED_MIN_TX_FEE + UAP.DEFAULT_DUST_LIMIT);
      const takerUtxos = [{
        txid: takerUtxo.txid,
        vout: takerUtxo.vout,
        value: satoshisFromCoins(takerUtxo.amount),
        height: 0,
      }];

      const planResult = planFill({
        order,
        position,
        takerUtxos,
        takerPubkey: takerPubKey,
        feeRate: 1000,
      });
      assert(planResult.ok, `planFill refused a fillable order: ${JSON.stringify(planResult.errors)}`);

      const built = await buildFillTx({
        secp,
        plan: planResult.plan,
        privKey: takerPrivKey,
        pubKey: takerPubKey,
        order,
        position,
        takerUtxos,
      });

      // The moment of truth: if the maker's SIGHASH_SINGLE signature did not
      // survive the taker's assembly, this throws.
      const txid = await node.rpc('sendrawtransaction', built.rawHex);
      const mined = await confirmMined(txid);

      // Accepted is not enough -- check the trade actually happened.
      // 1. the maker got paid, at the exact index their signature committed to
      assert(mined.vout[0].value !== undefined, 'no payment output');
      assert(satoshisFromCoins(mined.vout[0].value) === paymentValue,
        `maker payment is ${satoshisFromCoins(mined.vout[0].value)}, expected ${paymentValue}`);
      const paidTo = ADDR.pubkeyToAddress(makerPubKey, REGTEST_PUBKEY_ADDRESS);
      assert(
        (mined.vout[0].scriptPubKey.addresses || []).includes(paidTo),
        `payment went to ${JSON.stringify(mined.vout[0].scriptPubKey.addresses)}, not the maker (${paidTo})`
      );

      // 2. the token moved to the taker, and is still a UAP covenant
      const tokenOut = mined.vout[1];
      assert(satoshisFromCoins(tokenOut.value) === position.value,
        `covenant carries ${satoshisFromCoins(tokenOut.value)}, expected the full position ${position.value}`);
      assert(
        tokenOut.scriptPubKey.hex === bytesToHex(UAP.buildTransferScript(takerPubKey, MULTIPLIER, UAP.deriveOrigin(makerOrder.txid, makerOrder.vout))),
        'token covenant is not addressed to the taker with the same multiplier'
      );
    });

    // ---- a negative case, so acceptance above means something -----------
    await test('a fill that shortchanges the maker is rejected', async () => {
      // A FRESH position: the fill above already spent the first one, and
      // reusing it here would get "Missing inputs" (a double-spend) instead
      // of the signature failure this test is about.
      const position2 = await createMakerPosition();
      const order2 = makeOrder(position2);

      const takerUtxo = await freshUtxo(paymentValue + UAP.RECOMMENDED_MIN_TX_FEE + UAP.DEFAULT_DUST_LIMIT);
      const takerUtxos = [{
        txid: takerUtxo.txid,
        vout: takerUtxo.vout,
        value: satoshisFromCoins(takerUtxo.amount),
        height: 0,
      }];
      const planResult = planFill({
        order: order2, position: position2, takerUtxos, takerPubkey: takerPubKey, feeRate: 1000,
      });
      assert(planResult.ok, 'planFill should accept before we tamper');

      const built = await buildFillTx({
        secp, plan: planResult.plan, privKey: takerPrivKey, pubKey: takerPubKey,
        order: order2, position: position2, takerUtxos,
      });

      // Underpay the maker by one satoshi and re-sign only the taker's own
      // inputs. The maker's signature covers this output, so it must break.
      built.tx.vout[0].value -= 1;
      for (let i = 1; i < built.tx.vin.length; i++) {
        built.tx.vin[i].scriptSig = UAP.signP2PKHInput(
          secp, ADDR.buildP2PKHScript(takerPubKey), takerPrivKey, takerPubKey, built.tx, i);
      }

      // Assert on WHY it was rejected, not merely that it was. A bare
      // try/catch here would pass if the transaction were rejected for any
      // unrelated reason -- bad fee, missing input, malformed anything --
      // and would keep passing even if the maker's signature stopped being
      // checked at all. The claim under test is specifically that script
      // verification fails.
      let err = null;
      try {
        await node.rpc('sendrawtransaction', UAP.txToHex(built.tx));
      } catch (e) {
        err = e;
      }
      assert(err, 'node accepted a fill that paid the maker one satoshi less than they signed for');
      assert(/script-verify-flag/.test(err.message),
        `expected a script verification failure, got: ${err.message}`);
    });
  } finally {
    await node.stop();
  }

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
