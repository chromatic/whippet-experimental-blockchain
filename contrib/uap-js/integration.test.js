// integration.test.js -- validates uap.js against a REAL whippetd regtest
// node, using real secp256k1 signatures (not the fixed-bytes stub the rest
// of the suite uses). Everything uap.js claims to produce -- a spendable
// P2PKH scriptSig, a fee that clears relay policy, a correctly sized
// multi-output transaction, a valid OP_MINT/OP_MINT_TRANSFER spend -- is
// checked here by actually asking a node to accept or reject it.
//
// This is a plain Node script (no test framework), consistent with the
// rest of this directory. It talks to the node over JSON-RPC via fetch
// (see test-node-harness.js for why), starts exactly one node for the
// whole run, and gives each test its own UTXO(s) so tests don't interfere
// with each other.
//
// SAFETY: only ever regtest, only ever a freshly created temp datadir,
// only ever non-default ports. See test-node-harness.js.
//
// Usage: node integration.test.js
// Skips (exit 0) with a clear message if the whippetd/whippet-cli binaries
// aren't available -- it must never be required for `npm test` to pass.
//
// The EC library needs no such guard: ./secp.js wraps a vendored copy of
// @noble/secp256k1 that is checked in, so it is always importable.

import { resolveBinaries, startNode } from './test-node-harness.js';
import { satoshisFromCoins, privKeyFromWIF, assert } from './integration-helpers.js';
import * as ADDR from './addr.js';
import * as UAP from './uap.js';
import * as secp from './secp.js';

const REGTEST_PUBKEY_ADDRESS = ADDR.VERSIONS.regtest.PUBKEY_ADDRESS;

let passed = 0;
let failed = 0;
const failures = [];

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
    failures.push({ name, error: e });
  }
}

async function assertRejected(promise, description) {
  try {
    await promise;
  } catch (e) {
    return e; // expected: return the node's actual error for inspection
  }
  throw new Error(`expected rejection (${description}) but the node accepted the transaction`);
}

async function main() {
  const bins = resolveBinaries();
  if (!bins) {
    console.log(
      'SKIP: whippetd not found. Build it (make -C src whippetd) or set ' +
      'WHIPPETD_BIN, to run the integration suite.'
    );
    process.exit(0);
  }

  UAP.configureSecp(secp);

  let node = null;
  try {
    node = await startNode({});
    console.log(`node up: datadir=${node.datadir} rpcport=${node.rpcport}`);

    // ---- shared setup: one funded wallet address, matured coinbase ----
    const addr1 = await node.rpc('getnewaddress');
    await node.rpc('generatetoaddress', 101, addr1);
    const wif = await node.rpc('dumpprivkey', addr1);
    const privKey1 = privKeyFromWIF(wif);
    const pubKey1 = secp.getPublicKey(privKey1, true);
    assert(
      ADDR.pubkeyToAddress(pubKey1, REGTEST_PUBKEY_ADDRESS) === addr1,
      'derived pubkey does not match wallet address -- WIF decode or key derivation is wrong'
    );
    const addr1Script = ADDR.addressToScript(addr1);

    async function freshUtxo() {
      const utxos = await node.rpc('listunspent', 1, 9999999, [addr1]);
      if (utxos.length === 0) throw new Error('wallet has no spendable UTXOs at address ' + addr1);
      return utxos[0];
    }

    async function confirmMined(txid) {
      await node.rpc('generatetoaddress', 1, addr1);
      // getrawtransaction (not gettransaction) so this works for
      // transactions the wallet has no key involvement in at all, e.g.
      // OP_MINT/OP_MINT_TRANSFER spends -- requires txindex=1, set in
      // test-node-harness.js.
      const tx = await node.rpc('getrawtransaction', txid, 1);
      assert(tx.confirmations >= 1, `expected txid ${txid} to be mined, got confirmations=${tx.confirmations}`);
    }

    let derSigLengths = [];

    // ---- 1: real P2PKH spend, built+signed entirely by uap.js, accepted ----
    await test('P2PKH spend signed by uap.js is accepted by sendrawtransaction', async () => {
      const utxo = await freshUtxo();
      const scriptCode = UAP.hexToBytes(utxo.scriptPubKey);
      const valueSats = satoshisFromCoins(utxo.amount);
      const destAddr = await node.rpc('getnewaddress');
      const destScript = ADDR.addressToScript(destAddr);
      const fee = UAP.RECOMMENDED_MIN_TX_FEE;
      const sendValue = valueSats - fee;
      assert(sendValue > 0, 'utxo too small to cover RECOMMENDED_MIN_TX_FEE');

      const tx = {
        version: 1,
        locktime: 0,
        vin: [{ txid: utxo.txid, vout: utxo.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
        vout: [{ value: sendValue, scriptPubKey: destScript }],
      };
      const scriptSig = UAP.signP2PKHInput(secp, scriptCode, privKey1, pubKey1, tx, 0);
      tx.vin[0].scriptSig = scriptSig;

      // Record the real DER signature length for the report -- the whole
      // point of this suite is that this varies (70-72 bytes), unlike the
      // stub's fixed 70. scriptSig = push(sig+hashtype) push(pubkey); both
      // pushes use a 1-byte length prefix here (well under the 76-byte
      // OP_PUSHDATA1 threshold), so: total - 2 length bytes - pubkey - 1
      // hashtype byte = DER signature length.
      derSigLengths.push(scriptSig.length - 2 - pubKey1.length - 1);

      const hex = UAP.txToHex(tx);
      const txid = await node.rpc('sendrawtransaction', hex);
      await confirmMined(txid);
    });

    // ---- 2: same shape of spend, signed with the WRONG key, rejected ----
    let wrongKeyRejection = null;
    await test('P2PKH spend signed with the wrong key is rejected', async () => {
      const utxo = await freshUtxo();
      const scriptCode = UAP.hexToBytes(utxo.scriptPubKey);
      const valueSats = satoshisFromCoins(utxo.amount);
      const destAddr = await node.rpc('getnewaddress');
      const destScript = ADDR.addressToScript(destAddr);
      const fee = UAP.RECOMMENDED_MIN_TX_FEE;
      const sendValue = valueSats - fee;

      const tx = {
        version: 1,
        locktime: 0,
        vin: [{ txid: utxo.txid, vout: utxo.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
        vout: [{ value: sendValue, scriptPubKey: destScript }],
      };
      // Sign with an unrelated random key, but still claim to be spending
      // addr1's output by pushing addr1's real pubkey (so this exercises
      // signature verification, not merely a pubkey/hash mismatch).
      const wrongPrivKey = secp.utils.randomPrivateKey();
      tx.vin[0].scriptSig = UAP.signP2PKHInput(secp, scriptCode, wrongPrivKey, pubKey1, tx, 0);

      const hex = UAP.txToHex(tx);
      wrongKeyRejection = await assertRejected(
        node.rpc('sendrawtransaction', hex),
        'spend signed with wrong key'
      );
    });

    // ---- 3: buildPaymentTx output accepted -- validates the min-fee floor ----
    let minFeeEvidence = null;
    await test('buildPaymentTx output is accepted (min-fee floor clears relay policy)', async () => {
      const utxo = await freshUtxo();
      const valueSats = satoshisFromCoins(utxo.amount);
      const destAddr = await node.rpc('getnewaddress');
      const destScript = ADDR.addressToScript(destAddr);
      const sendValue = Math.floor(valueSats / 10);

      const opts = {
        inputs: [
          {
            txid: utxo.txid,
            vout: utxo.vout,
            value: valueSats,
            scriptCode: UAP.hexToBytes(utxo.scriptPubKey),
            privKey: privKey1,
            pubKey: pubKey1,
          },
        ],
        outputs: [{ value: sendValue, scriptPubKey: destScript }],
        changeScript: addr1Script,
        feeRate: 1000, // deliberately low -- a ~192-byte tx at 1000 sat/kB is ~192 sats, far below the floor
      };
      const tx = UAP.buildPaymentTx(secp, opts);

      const totalOut = tx.vout.reduce((s, o) => s + o.value, 0);
      const actualFee = valueSats - totalOut;
      minFeeEvidence = { actualFee, floor: UAP.RECOMMENDED_MIN_TX_FEE };
      assert(
        actualFee >= UAP.RECOMMENDED_MIN_TX_FEE,
        `expected fee >= RECOMMENDED_MIN_TX_FEE (${UAP.RECOMMENDED_MIN_TX_FEE}), got ${actualFee}`
      );

      const hex = UAP.txToHex(tx);
      const txid = await node.rpc('sendrawtransaction', hex);
      await confirmMined(txid);
    });

    // ---- 4: multi-output buildPaymentTx (3 outputs + change) accepted ----
    await test('multi-output buildPaymentTx (3 outputs + change) is accepted', async () => {
      const utxos = await node.rpc('listunspent', 1, 9999999, [addr1]);
      assert(utxos.length >= 1, 'need at least one UTXO for multi-output test');
      const candidateInputs = utxos.slice(0, 5).map((u) => ({
        txid: u.txid,
        vout: u.vout,
        value: satoshisFromCoins(u.amount),
        scriptCode: UAP.hexToBytes(u.scriptPubKey),
        privKey: privKey1,
        pubKey: pubKey1,
      }));
      const totalCandidateValue = candidateInputs.reduce((s, i) => s + i.value, 0);
      const perOutput = Math.floor(totalCandidateValue / 20); // small relative to funds, leaves room for change+fee

      const destAddrs = [
        await node.rpc('getnewaddress'),
        await node.rpc('getnewaddress'),
        await node.rpc('getnewaddress'),
      ];
      const outputs = destAddrs.map((a) => ({ value: perOutput, scriptPubKey: ADDR.addressToScript(a) }));

      const tx = UAP.buildPaymentTx(secp, {
        inputs: candidateInputs,
        outputs,
        changeScript: addr1Script,
        feeRate: 1000,
      });
      assert(tx.vout.length === 4, `expected 3 payment outputs + 1 change output, got ${tx.vout.length}`);

      const hex = UAP.txToHex(tx);
      const txid = await node.rpc('sendrawtransaction', hex);
      await confirmMined(txid);
    });

    // ---- 5 (stretch): OP_MINT output created and spent via signSpend ----
    // Shared by tests 5 and 7. Creates a fresh, confirmed OP_MINT position.
    async function createMintPosition(multiplier) {
      const mintPrivKey = secp.utils.randomPrivateKey();
      const mintPubKey = secp.getPublicKey(mintPrivKey, true);
      const salt = new Uint8Array(20);
      crypto.getRandomValues(salt);
      const mintScript = UAP.buildMintScript(mintPubKey, multiplier);

      const utxo = await freshUtxo();
      const valueSats = satoshisFromCoins(utxo.amount);
      const mintValue = 2000 * UAP.COIN; // well above the 1000*COIN entry-fee floor
      const fee = UAP.RECOMMENDED_MIN_TX_FEE;
      const change = valueSats - mintValue - fee;
      assert(change > 0, 'funding utxo too small to cover mint value + fee');

      const tx = {
        version: 1,
        locktime: 0,
        vin: [{ txid: utxo.txid, vout: utxo.vout, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
        vout: [
          { value: mintValue, scriptPubKey: mintScript },
          { value: change, scriptPubKey: addr1Script },
        ],
      };
      const scriptCode = UAP.hexToBytes(utxo.scriptPubKey);
      tx.vin[0].scriptSig = UAP.signP2PKHInput(secp, scriptCode, privKey1, pubKey1, tx, 0);
      const txid = await node.rpc('sendrawtransaction', UAP.txToHex(tx));
      await confirmMined(txid);
      return { mintScript, mintTxid: txid, mintValue, mintPrivKey, mintPubKey };
    }

    let mintDone = false;
    let secondMint = null; // { mintScript, mintTxid, mintValue, mintPrivKey } for test 6
    await test('OP_MINT output created and spent via signSpend is accepted', async () => {
      const mint = await createMintPosition(1000);
      // Second mint position, held in reserve for the stretch rejection test (6).
      secondMint = await createMintPosition(1000);

      const transferKey = secp.utils.randomPrivateKey();
      const transferPub = secp.getPublicKey(transferKey, true);
      const transferTx = UAP.buildTransferTx(secp, {
        input: {
          txid: mint.mintTxid,
          vout: 0,
          scriptCode: mint.mintScript,
          value: mint.mintValue,
          privKey: mint.mintPrivKey,
        },
        toPubkey: transferPub,
        multiplier: 1000,
        fee: UAP.RECOMMENDED_MIN_TX_FEE,
      });
      const txid = await node.rpc('sendrawtransaction', UAP.txToHex(transferTx));
      await confirmMined(txid);
      mintDone = true;
    });

    // ---- 6 (stretch): mint spend signed with the wrong key is rejected ----
    let mintWrongKeyRejection = null;
    if (mintDone && secondMint) {
      await test('OP_MINT spend signed with the wrong key is rejected', async () => {
        const wrongKey = secp.utils.randomPrivateKey();
        const bogusToPub = secp.getPublicKey(secp.utils.randomPrivateKey(), true);
        const transferTx = UAP.buildTransferTx(secp, {
          input: {
            txid: secondMint.mintTxid,
            vout: 0,
            scriptCode: secondMint.mintScript,
            value: secondMint.mintValue,
            privKey: wrongKey, // NOT secondMint.mintPrivKey
          },
          toPubkey: bogusToPub,
          multiplier: 1000,
          fee: UAP.RECOMMENDED_MIN_TX_FEE,
        });
        mintWrongKeyRejection = await assertRejected(
          node.rpc('sendrawtransaction', UAP.txToHex(transferTx)),
          'mint spend signed with wrong key'
        );
      });
    }

    // ---- 7 (stretch): small multipliers (1..16) round-trip end to end ----
    //
    // These sixteen values used to be unusable. Two node rules disagreed
    // about how such a multiplier had to be encoded:
    //
    //   consensus (ParseUapOutputScript): every element of a UAP script had
    //     to be a real data push, opcode <= OP_PUSHDATA4. OP_1..OP_16 are not.
    //   standardness (SCRIPT_VERIFY_MINIMALDATA): a script being executed
    //     must use the minimal encoding, and for 1..16 that IS OP_1..OP_16.
    //
    // A covenant *output* is only parsed, so it needed the data push; the
    // same script being *spent* is executed, so it needed OP_N. A position in
    // that range was therefore consensus-valid and unspendable-in-practice.
    // Consensus now requires the canonical encoding everywhere, so the two
    // rules ask for the same bytes. This test mints at multiplier 10, moves
    // the position twice, and checks that the encoding the old node demanded
    // is now rejected -- so there is exactly one way to write a position.
    let smallMultEvidence = null;
    if (mintDone) {
      await test('small multipliers (1..16) round-trip end to end', async () => {
        const M = 10;
        const dataPush = UAP.concatBytes(Uint8Array.of(0x01), Uint8Array.of(M));
        const withMult = (pub, multEnc, salt) => (salt
          ? UAP.concatBytes(Uint8Array.of(pub.length), pub, multEnc, Uint8Array.of(salt.length), salt, Uint8Array.of(0xb5))
          : UAP.concatBytes(Uint8Array.of(pub.length), pub, multEnc, Uint8Array.of(0xba)));

        // uap.js builds the canonical form: OP_10, a single byte.
        const built = UAP.buildTransferScript(pubKey1, M);
        assert(built[34] === 0x50 + M, `uap.js must encode multiplier ${M} as OP_${M}, got 0x${built[34].toString(16)}`);

        // Mint a real position at multiplier 10, entirely through uap.js.
        const pos = await createMintPosition(M);
        const posValue = pos.mintValue;

        // Move it once: mint -> covenant.
        const kA = secp.utils.randomPrivateKey();
        const pubA = secp.getPublicKey(kA, true);
        const covA = UAP.buildTransferScript(pubA, M);
        const tx1 = {
          version: 1, locktime: 0,
          vin: [{ txid: pos.mintTxid, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
          vout: [{ value: posValue - UAP.RECOMMENDED_MIN_TX_FEE, scriptPubKey: covA }],
        };
        tx1.vin[0].scriptSig = UAP.signSpend(secp, pos.mintScript, pos.mintPrivKey, tx1, 0);
        const txid1 = await node.rpc('sendrawtransaction', UAP.txToHex(tx1));
        await confirmMined(txid1);

        // And again: covenant -> covenant. This is the step that was
        // impossible before -- spending a small-multiplier covenant means
        // executing it, which is where MINIMALDATA applies.
        const kB = secp.utils.randomPrivateKey();
        const covB = UAP.buildTransferScript(secp.getPublicKey(kB, true), M);
        const tx2 = {
          version: 1, locktime: 0,
          vin: [{ txid: txid1, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
          vout: [{ value: tx1.vout[0].value - UAP.RECOMMENDED_MIN_TX_FEE, scriptPubKey: covB }],
        };
        tx2.vin[0].scriptSig = UAP.signSpend(secp, covA, kA, tx2, 0);
        const txid2 = await node.rpc('sendrawtransaction', UAP.txToHex(tx2));
        await confirmMined(txid2);

        // The old encoding -- an explicit one-byte data push -- is no longer a
        // UAP output, so OP_MINT_TRANSFER's conservation check sees no
        // continuing covenant. Mandatory failure, not a policy one.
        const kC = secp.utils.randomPrivateKey();
        const covNonCanonical = withMult(secp.getPublicKey(kC, true), dataPush);
        const tx3 = {
          version: 1, locktime: 0,
          vin: [{ txid: txid2, vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff }],
          vout: [{ value: tx2.vout[0].value - UAP.RECOMMENDED_MIN_TX_FEE, scriptPubKey: covNonCanonical }],
        };
        tx3.vin[0].scriptSig = UAP.signSpend(secp, covB, kB, tx3, 0);
        const rejection = await assertRejected(
          node.rpc('sendrawtransaction', UAP.txToHex(tx3)),
          'non-canonical data-push multiplier in a covenant output'
        );
        assert(/mandatory/.test(rejection.message), `expected a mandatory failure, got: ${rejection.message}`);

        smallMultEvidence = { txid1, txid2, rejection };
      });
    }

    // ---- report ----
    console.log('\n=== summary ===');
    console.log(`${passed} passed, ${failed} failed`);
    if (wrongKeyRejection) {
      console.log(`P2PKH wrong-key rejection message: [${wrongKeyRejection.code}] ${wrongKeyRejection.message}`);
    }
    if (mintWrongKeyRejection) {
      console.log(`OP_MINT wrong-key rejection message: [${mintWrongKeyRejection.code}] ${mintWrongKeyRejection.message}`);
    }
    if (minFeeEvidence) {
      console.log(
        `min-fee floor evidence: actual fee used = ${minFeeEvidence.actualFee} sats, ` +
        `RECOMMENDED_MIN_TX_FEE = ${minFeeEvidence.floor} sats -- node accepted it via sendrawtransaction`
      );
    }
    if (smallMultEvidence) {
      console.log('small-multiplier (1..16) evidence:');
      console.log(`  OP_10 mint -> covenant           -> mined as ${smallMultEvidence.txid1}`);
      console.log(`  OP_10 covenant -> covenant       -> mined as ${smallMultEvidence.txid2}`);
      console.log(`  non-canonical data-push covenant -> [${smallMultEvidence.rejection.code}] ${smallMultEvidence.rejection.message}`);
    }
    if (derSigLengths.length) {
      console.log(`observed real DER signature length(s): ${derSigLengths.join(', ')} bytes (stub always returned 70)`);
    }
  } finally {
    if (node) {
      await node.stop();
      console.log(`node stopped, datadir removed: ${node.datadir}`);
    }
  }

  if (failed > 0) {
    process.exitCode = 1;
  }
}

main().catch((e) => {
  console.error('FATAL:', e && e.stack ? e.stack : e);
  process.exitCode = 1;
});
