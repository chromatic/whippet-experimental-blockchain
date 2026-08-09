// End-to-end check that mint.js produces a transaction a real node accepts.
//
// The unit tests in mint.test.js assert on the returned object's fields. That
// is not enough on its own: an earlier version of buildMintTx returned
// `rawHex: 'placeholder'` and signed nothing, and every one of those tests
// still passed. Only a node can tell you the transaction is real.
//
// Starts its own whippetd on regtest, in a temporary datadir on a
// non-default port, and stops it again. Never touches a real datadir.
//
//   WHIPPETD_BIN=/path/to/whippetd npm run test:integration

import { startNode } from '../uap-js/test-node-harness.js';
import * as uap from '../uap-js/uap.js';
import * as addr from '../uap-js/addr.js';
import * as secp from '../uap-js/secp.js';
import { planMint, buildMintTx } from './mint.js';
import { assert } from '../uap-js/integration-helpers.js';

uap.configureSecp(secp);

async function main() {
  const priv = new Uint8Array(32).fill(0x11);
  const pub = secp.getPublicKey(priv, true);
  const address = addr.pubkeyToAddress(pub, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
  const spkHex = uap.bytesToHex(addr.buildP2PKHScript(pub));

  const node = await startNode();
  let failed = 0;
  try {
    await node.rpc('generate', 120);

    // Fund the browser wallet's own address, the way a user would.
    const fundTxid = await node.rpc('sendtoaddress', address, 1010);
    await node.rpc('generate', 1);
    const funding = await node.rpc('getrawtransaction', fundTxid, 1);
    const out = funding.vout.find((v) => v.scriptPubKey.hex === spkHex);
    assert(out, 'funding output paying the wallet address was not found');

    const utxo = {
      txid: fundTxid,
      vout: out.n,
      value: Math.round(out.value * uap.COIN),
      scriptPubKey: spkHex,
    };

    const amountSats = 1000 * uap.COIN; // exactly the OP_MINT entry fee
    const planned = planMint({
      ticker: 'E2E',
      multiplier: 100,
      amountSats,
      utxos: [utxo],
      feeRate: 1000,
      address,
    });
    assert(planned.ok, `planMint refused a valid mint: ${JSON.stringify(planned.errors)}`);

    const built = await buildMintTx({
      secp,
      plan: planned.plan,
      privKey: priv,
      pubKey: pub,
      utxos: [utxo],
      changeAddress: address,
    });
    assert(/^[0-9a-f]+$/.test(built.rawHex) && built.rawHex.length > 200,
      `buildMintTx did not produce a real transaction: ${String(built.rawHex).slice(0, 40)}`);

    const mintTxid = await node.rpc('sendrawtransaction', built.rawHex);
    await node.rpc('generate', 1);
    const mintTx = await node.rpc('getrawtransaction', mintTxid, 1);

    const mintOut = mintTx.vout.find(
      (v) => v.scriptPubKey.hex === uap.bytesToHex(built.mintScript));
    assert(mintOut, 'the mint output is not in the mined transaction');
    assert(Math.round(mintOut.value * uap.COIN) === amountSats,
      `mint output value ${Math.round(mintOut.value * uap.COIN)} != planned ${amountSats}`);
    // The node's own classifier agreeing is what proves the script is a UAP
    // mint under current consensus rules, not merely well-formed bytes.
    assert(mintOut.scriptPubKey.type === 'op_mint',
      `node classified the mint output as ${mintOut.scriptPubKey.type}, not op_mint`);
    assert(mintTx.confirmations >= 1, 'the mint transaction was not mined');

    console.log('✓ mint.js transaction accepted and mined');
    console.log(`  txid ${mintTxid}`);
    console.log(`  ${amountSats} sat locked, classified ${mintOut.scriptPubKey.type}`);
  } catch (e) {
    failed = 1;
    console.error('✗ ' + (e && e.stack ? e.stack : e));
  } finally {
    await node.stop();
  }
  process.exitCode = failed;
}

main().catch((e) => {
  console.error('FATAL:', e && e.stack ? e.stack : e);
  process.exitCode = 1;
});
