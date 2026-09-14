// End-to-end: mint, sell, buy, transfer and cancel, driven through a real
// browser against a real indexer and a real regtest node.
//
// # What this covers that nothing else does
//
// The unit suites call exported functions against a fake DOM and never reach
// the network. mint.integration.test.js and market.integration.test.js reach
// a real node but call the uap-js library directly -- they never load app.js,
// never click a button, never cross a screen boundary. The seam between the
// two is where confirmMint once built a transaction, signed it, showed its
// length in an alert() and dropped it: every mint test passed.
//
// So every phase here asserts twice. Once in the browser, because that is
// where the user is. Then again against the node, by mining a block and
// asking what it actually got -- because a wallet that builds a plausible
// covenant the network rejects is a wallet that looks perfect in a DOM
// assertion.
//
// Usage: npm run test:e2e
// Exits 0 on success, 0 with a SKIP line if whippetd or Chrome is missing,
// 1 on any failure.
import * as stack from './stack.js';
import * as cdp from './cdp.js';
import * as keys from './keys.js';
import * as uap from '../../uap-js/uap.js';

const COIN = uap.COIN;

// The maker's asking price, in satoshis of WHIP. Comfortably inside the
// taker's funding and well above the dust limit.
const ASKING_PRICE = 50 * COIN;
const MINT_AMOUNT_COINS = 1000;
const MINT_MULTIPLIER = 100;
// Typed in lowercase on purpose. The wallet folds a ticker to uppercase
// before it builds the script, and the indexer rejects a lowercase one
// outright -- so if the fold is ever dropped, the record stops parsing and
// the token loses its name. Typing 'e2e' and expecting 'E2E' back out of
// the node is the cheapest place to catch that.
const MINT_TICKER_TYPED = 'e2e';
const MINT_TICKER = 'E2E';
const FUNDING_COINS = 2000;

const failures = [];
const lines = [];
function say(s) { console.log(s); lines.push(s); }
function check(ok, msg) {
  say((ok ? '  ✓ ' : '  ✗ ') + msg);
  if (!ok) failures.push(msg);
  return ok;
}
function phase(n) { say(`\n── ${n}`); }

/** Assert, but keep going: one broken phase should not hide the rest. */
function checkEqual(actual, expected, msg) {
  return check(actual === expected, `${msg}${actual === expected ? '' : ` (got ${actual}, expected ${expected})`}`);
}

async function main() {
  const chromeBin = stack.resolveChrome();
  if (!stack.resolveBinaries()) {
    console.log('SKIP: whippetd not found -- build it (make -C src whippetd) or set WHIPPETD_BIN');
    return 0;
  }
  if (!chromeBin) {
    console.log('SKIP: no Chrome/Chromium found -- set CHROME_BIN');
    return 0;
  }

  const st = await stack.startStack({ log: (m) => say('  ' + m) });
  const chrome = await cdp.launchChrome({ binary: chromeBin, workdir: st.workdir });
  const { maker, taker } = await keys.identities();

  // Mine a block and wait for the indexer to see it. Every /api assertion
  // after a broadcast depends on this; the indexer polls on a 1s tick, so
  // reading immediately after `generate` returns reads the old state.
  let coinbaseAddr = null;
  const mine = async (n = 1) => {
    await st.node.rpc('generatetoaddress', n, coinbaseAddr);
    const h = await st.node.rpc('getblockcount');
    await st.api.syncTo(h);
    return h;
  };

  /**
   * The node's view of a transaction the browser claims to have sent.
   *
   * "The node has never heard of it" is the single most important failure
   * this suite can report -- it is what a wallet that builds and signs a
   * transaction and then drops it looks like from outside, and the badge on
   * screen looks identical either way. So it gets a message that says so
   * rather than surfacing a raw RPC error.
   */
  const tx = async (txid) => {
    try {
      return await st.node.rpc('getrawtransaction', txid, 1);
    } catch (e) {
      throw new Error(
        `the node has no transaction ${txid} -- the wallet reported a broadcast ` +
        `that never reached the network (${e.message})`
      );
    }
  };

  /** Restore a persona through the app's own screens and unlock it. */
  const openWallet = async (name, who) => {
    const p = await cdp.newPersona(chrome, name);
    await p.navigate(st.base + '/');
    await p.click('Restore from Mnemonic');
    await p.waitForScreen('restore');
    await p.fill('mnemonic-input', who.mnemonic);
    await p.fill('restore-passphrase', keys.PASSPHRASE);
    await p.click('Restore Wallet');
    // PBKDF2 runs 600k iterations here and again on unlock, and headless
    // Chrome is not fast at it.
    await p.waitForScreen('unlock', { timeoutMs: 60000 });
    await p.fill('unlock-passphrase', keys.PASSPHRASE);
    await p.click('Unlock');
    await p.waitForScreen('wallet', { timeoutMs: 60000 });
    return p;
  };

  /** Click Select on the position row for `txid`, if not already selected. */
  const selectPosition = async (p, txid) => {
    const clicked = await p.evaluate(`
      (() => {
        const rows = Array.from(document.querySelectorAll('.position-row'));
        const row = rows.find(r => r.innerText.includes(${JSON.stringify(txid.slice(0, 12))}));
        if (!row) return 'no-row';
        const btn = row.querySelector('button');
        if (!btn) return 'no-button';
        btn.click();
        return 'ok';
      })()
    `);
    if (clicked !== 'ok') {
      throw new Error(`[${p.name}] could not select position ${txid.slice(0, 12)}: ${clicked}\n` +
        `--- screen ---\n${await p.text()}`);
    }
  };

  /** After a Confirm & Broadcast, read the txid off the wallet screen. */
  const broadcastTxid = async (p) => {
    await p.waitForScreen('wallet', { timeoutMs: 30000 });
    await p.waitFor('document.querySelector("code.tx-status-txid")',
      { what: 'a broadcast txid badge' });
    return (await p.textOf('code.tx-status-txid')).trim();
  };

  try {
    // ==================================================================
    phase('0. fund both personas');
    coinbaseAddr = await st.node.rpc('getnewaddress');
    await st.node.rpc('generatetoaddress', 101, coinbaseAddr);
    await st.node.rpc('sendtoaddress', maker.address, FUNDING_COINS);
    await st.node.rpc('sendtoaddress', taker.address, FUNDING_COINS);
    await mine(1);
    const makerUtxos = await st.api.get('/api/utxos?address=' + maker.address);
    const takerUtxos = await st.api.get('/api/utxos?address=' + taker.address);
    check(makerUtxos.length > 0 && takerUtxos.length > 0,
      `both personas funded (maker ${makerUtxos.length}, taker ${takerUtxos.length} utxo)`);

    // ==================================================================
    phase('1. the maker restores a wallet');
    const m = await openWallet('maker', maker);
    checkEqual(await m.textOf('code.address'), maker.address,
      'the address the app derives is the one the harness funded');
    await m.waitFor(`document.body.innerText.includes('Total:')`, { what: 'the balance' });
    const balText = await m.text();
    check(balText.includes(String(FUNDING_COINS * COIN)),
      `the funded balance is shown (${(balText.match(/Total:.*/) || [])[0]})`);
    check(!/undefined|NaN|\[object /.test(balText), 'no undefined/NaN leaked onto the wallet screen');
    await m.screenshot('01-wallet');

    // ==================================================================
    phase('2. the maker mints a token');
    await m.click('Mint Token');
    await m.waitForScreen('mint');
    await m.fill('mint-ticker', MINT_TICKER_TYPED);
    await m.fill('mint-multiplier', MINT_MULTIPLIER);
    await m.fill('mint-amount', MINT_AMOUNT_COINS);
    await m.click('Review');
    await m.waitForScreen('mint-review', { timeoutMs: 30000 });
    const reviewText = await m.text();
    check(!/undefined|NaN/.test(reviewText), 'the mint review screen has no NaN or undefined');
    await m.screenshot('02-mint-review');
    await m.click('Confirm & Broadcast');
    const mintTxid = await broadcastTxid(m);
    check(/^[0-9a-f]{64}$/.test(mintTxid), `the mint was broadcast (${mintTxid.slice(0, 16)}…)`);

    // The node is the authority, not the badge.
    await mine(1);
    const mintTx = await tx(mintTxid);
    check(mintTx.confirmations >= 1, 'the node confirmed the mint');
    const mintVout = mintTx.vout.find((v) => v.scriptPubKey.type === 'op_mint');
    check(!!mintVout, `the mint output is a covenant, not a plain payment ` +
      `(types: ${mintTx.vout.map((v) => v.scriptPubKey.type).join(', ')})`);
    if (mintVout) {
      checkEqual(Math.round(mintVout.value * COIN), MINT_AMOUNT_COINS * COIN,
        'the minted output locks the amount that was asked for');
    }

    const positions = await st.api.get('/api/positions?pubkey=' + maker.pubKeyHex);
    check(positions.length === 1 && positions[0].txid === mintTxid,
      `the indexer sees the maker's new position (${positions.length} position)`);
    const P1 = positions[0];

    // The ticker the user typed has to survive the round trip to the chain
    // and back. It travels in an OP_RETURN on the mint transaction -- a
    // separate output the wallet has to remember to build, and a form that
    // validates a ticker and then drops it looks identical on screen to one
    // that publishes it. See doc/uap-token-metadata.md for the format.
    const wuapOut = mintTx.vout.find((v) => v.scriptPubKey.type === 'nulldata');
    check(!!wuapOut, 'the mint transaction carries an OP_RETURN metadata output');

    // Byte-exact, because this is the one place where the JS builder and the
    // Go parser can silently drift apart: OP_RETURN, push "WUAP", push the
    // version, push the (folded) ticker.
    //   6a 04 57554150 01 02 03 453245
    if (wuapOut) {
      const want = '6a' + '04' + '57554150' + '01' + '02' +
        '03' + Buffer.from(MINT_TICKER, 'ascii').toString('hex');
      check(wuapOut.scriptPubKey.hex.startsWith(want),
        `the metadata record is v2 and carries the folded ticker ` +
        `(want ${want}…, got ${wuapOut.scriptPubKey.hex})`);
    }

    // The lineage a mint creates is SHA256(txid_reversed || vout_le) -- NOT
    // the "txid:vout" string v1 used, which is what this assertion looked for
    // until the covenant change. Deriving it here with uap.js and matching it
    // against what the Go indexer published is the cross-implementation check
    // that the two agree about lineage identity on real chain data.
    const expectedOrigin = uap.bytesToHex(uap.deriveOrigin(mintTxid, mintVout ? mintVout.n : 0));
    const tokens = await st.api.get('/api/tokens');
    const token = tokens.find((t) => t.origin === expectedOrigin);
    check(!!token, `the indexer lists the new token under the lineage uap-js derives ` +
      `(want ${expectedOrigin}, got ${tokens.map((t) => t.origin).join(', ') || 'none'})`);
    if (token) {
      checkEqual(token.ticker, MINT_TICKER,
        `the indexer reports the ticker uppercased from the typed '${MINT_TICKER_TYPED}'`);
      check(!('name' in token), 'the token carries no name field');
    }

    // ==================================================================
    phase('3. the maker offers it for sale');
    await m.click('Sell a Position');
    await m.waitForScreen('sell', { timeoutMs: 30000 });
    await m.waitFor(`document.querySelector('.position-row')`, { what: 'the position list' });
    await selectPosition(m, P1.txid);
    await m.fill('sell-price', ASKING_PRICE);
    await m.click('Review');
    await m.waitForScreen('sell-review', { timeoutMs: 20000 });
    const sellReview = await m.text();
    check(sellReview.includes(String(ASKING_PRICE)),
      'the sale review names the exact asking price');
    check(/ANYONE may take/.test(sellReview),
      'the maker is told the offer is open to anyone');
    await m.screenshot('03-sell-review');
    await m.click('Publish offer');
    await m.waitForScreen('market', { timeoutMs: 30000 });

    const orders = await st.api.get('/api/orders');
    check(orders.length === 1 && orders[0].txid === P1.txid,
      `the relay is offering the position (${orders.length} order)`);
    checkEqual(orders[0].payment_value, ASKING_PRICE,
      'the published order asks the price the maker typed');

    // ==================================================================
    phase('4. the taker buys it');
    const t = await openWallet('taker', taker);
    await t.click('Order Book');
    await t.waitForScreen('market', { timeoutMs: 30000 });
    await t.waitFor(`document.querySelector('.order-row')`, { what: 'the maker\'s order' });
    check((await t.text()).includes('SELL multiplier=' + MINT_MULTIPLIER),
      'the taker sees the offer in the book');
    await t.click('Fill');
    await t.waitFor(`/Review Fill Transaction/.test(document.body.innerText)`,
      { what: 'the fill review', timeoutMs: 30000 });
    const fillReview = await t.text();
    check(!/undefined|NaN/.test(fillReview), 'the fill review has no NaN or undefined');
    await t.screenshot('04-fill-review');
    await t.click('Confirm & Broadcast');
    const fillTxid = await broadcastTxid(t);
    check(/^[0-9a-f]{64}$/.test(fillTxid), `the fill was broadcast (${fillTxid.slice(0, 16)}…)`);

    // The double-fill window. The fill is in the mempool and nowhere else:
    // no block has been mined, and the relay's mempool poller runs on a
    // timer that has almost certainly not ticked since the broadcast
    // returned a moment ago. A second taker reading the book right now must
    // not still be offered this position -- they would sign a fill, pay a
    // fee, and have the node refuse it.
    //
    // What closes it is the relay noting the spend on the broadcast it
    // performed itself, rather than waiting to rediscover it (see
    // NotePendingSpends in contrib/uap-indexer/mempool.go). Asserting it
    // here, before mine(1), is the only place the timing is real.
    const bookDuringFill = await st.api.get('/api/orders');
    check(!bookDuringFill.some((o) => o.txid === P1.txid && o.vout === P1.vout),
      'the order left the book the instant its fill was broadcast');
    const duringFill = await st.api.get(`/api/orders/${P1.txid}/${P1.vout}`);
    checkEqual(duringFill.status, 'pending_fill',
      'a taker who already had the order open is told a fill is in flight');
    checkEqual(duringFill.pending_txid, fillTxid,
      'and is told which transaction to watch');

    await mine(1);
    const fillTx = await tx(fillTxid);
    check(fillTx.confirmations >= 1, 'the node confirmed the fill');

    // Output ORDER is the whole point: the maker signed SIGHASH_SINGLE |
    // ANYONECANPAY, which commits to output 0 and nothing else. If the
    // payment is not at index 0 the maker's signature does not cover it, and
    // a taker could pay themselves.
    const payOut = fillTx.vout[0];
    checkEqual(Math.round(payOut.value * COIN), ASKING_PRICE,
      'output 0 pays exactly the asking price');
    check((payOut.scriptPubKey.addresses || []).includes(maker.address),
      `output 0 pays the maker (${(payOut.scriptPubKey.addresses || []).join(',')})`);
    const tokenOut = fillTx.vout[1];
    check(/^op_(mint_)?transfer$/.test(tokenOut.scriptPubKey.type),
      `output 1 is the taker's token covenant (type ${tokenOut.scriptPubKey.type})`);
    checkEqual(Math.round(tokenOut.value * COIN), P1.value,
      'the token keeps its full value across the sale');

    const takerPositions = await st.api.get('/api/positions?pubkey=' + taker.pubKeyHex + '&unspent=true');
    check(takerPositions.length === 1, `the taker now holds the position (${takerPositions.length})`);
    const P2 = takerPositions[0];

    // ==================================================================
    phase('5. the taker sends half back to the maker');
    const halfCoins = (P2.value / COIN) / 2;
    await t.click('Send Tokens');
    await t.waitForScreen('send', { timeoutMs: 30000 });
    await t.waitFor(`document.querySelector('.position-row')`, { what: 'the token list' });
    await selectPosition(t, P2.txid);
    await t.fill('send-amount', halfCoins);
    await t.fill('send-pubkey', maker.pubKeyHex);
    await t.fill('send-address', maker.address);
    await t.click('Review');
    await t.waitForScreen('send-review', { timeoutMs: 30000 });
    check(!(await t.has('.warning-bug')),
      'the transfer conserves token value (no conservation warning)');
    await t.screenshot('05-send-review');
    await t.click('Confirm & Broadcast');
    const sendTxid = await broadcastTxid(t);
    check(/^[0-9a-f]{64}$/.test(sendTxid), `the transfer was broadcast (${sendTxid.slice(0, 16)}…)`);

    await mine(1);
    const sendTx = await tx(sendTxid);
    check(sendTx.confirmations >= 1, 'the node confirmed the transfer');
    const tokenOuts = sendTx.vout.filter((v) => /^op_(mint_)?transfer$/.test(v.scriptPubKey.type));
    checkEqual(tokenOuts.length, 2, 'the transfer produced a recipient output and a change output');
    const tokenSatsOut = tokenOuts.reduce((sum, v) => sum + Math.round(v.value * COIN), 0);
    checkEqual(tokenSatsOut, P2.value,
      'token value out equals token value in -- nothing minted or burned in transit');

    // ==================================================================
    phase('6. the maker sees what arrived');
    const makerPositions = await st.api.get('/api/positions?pubkey=' + maker.pubKeyHex + '&unspent=true');
    check(makerPositions.length === 1, `the maker holds the received position (${makerPositions.length})`);
    const P3 = makerPositions[0];
    checkEqual(P3.value, Math.round(halfCoins * COIN), 'the maker received the half that was sent');
    // The maker's page is still the same page load; go back to the wallet so
    // it re-fetches, then into Sell, which is where positions are listed.
    await m.click('Back');
    await m.waitForScreen('wallet', { timeoutMs: 30000 });
    await m.click('Sell a Position');
    await m.waitForScreen('sell', { timeoutMs: 30000 });
    await m.waitFor(`document.querySelector('.position-row')`, { what: 'the received position' });
    check((await m.text()).includes(P3.txid.slice(0, 12)),
      'the received position is visible in the maker\'s wallet');

    // ==================================================================
    phase('7. the maker publishes and then cancels an order');
    await selectPosition(m, P3.txid);
    await m.fill('sell-price', ASKING_PRICE);
    await m.click('Review');
    await m.waitForScreen('sell-review', { timeoutMs: 20000 });
    await m.click('Publish offer');
    await m.waitForScreen('market', { timeoutMs: 30000 });
    check((await st.api.get('/api/orders')).length === 1, 'the second offer is live on the relay');

    await m.click('Back');
    await m.waitForScreen('wallet', { timeoutMs: 30000 });
    await m.click('My Orders');
    await m.waitForScreen('my-orders', { timeoutMs: 30000 });
    await m.waitFor(`document.querySelector('.order-row')`, { what: 'the maker\'s own order' });
    await m.click('Cancel');
    await m.waitForScreen('my-orders-cancel', { timeoutMs: 20000 });
    await m.click('Confirm Cancel');
    await m.waitFor(`/You have no open orders/.test(document.body.innerText)`,
      { what: 'an empty order list', timeoutMs: 30000 });
    check(true, 'the cancelled order is gone from the maker\'s list');
    check((await st.api.get('/api/orders')).length === 0,
      'the relay dropped the cancelled order');
    await m.screenshot('07-cancelled');

    // ==================================================================
    phase('negative A. a transfer to a mismatched address is refused');
    // The recipient of a covenant is the PUBKEY; the address field is an
    // optional cross-check. Pairing one persona's pubkey with the other's
    // address must stop the send, because the likeliest real cause is a
    // pasted address that does not belong to the intended recipient.
    const mempoolBefore = await st.node.rpc('getrawmempool');
    await t.click('Send Tokens');
    await t.waitForScreen('send', { timeoutMs: 30000 });
    await t.waitFor(`document.querySelector('.position-row')`, { what: 'the token list' });
    await t.fill('send-amount', 1);
    await t.fill('send-pubkey', maker.pubKeyHex);
    await t.fill('send-address', taker.address);   // deliberately not the maker's
    await t.click('Review');
    await t.waitFor(
      `document.querySelector('.error-field') || document.querySelector('.screen-send-review')`,
      { what: 'either a rejection or (wrongly) a review screen' });
    check(!(await t.has('.screen-send-review')),
      'a mismatched address does not reach the review screen');
    check(await t.has('.error-field'), 'the mismatch is reported as a field error');
    const mempoolAfter = await st.node.rpc('getrawmempool');
    checkEqual(mempoolAfter.length, mempoolBefore.length,
      'nothing was broadcast for the refused transfer');
    await t.screenshot('08-mismatch-refused');

    // ==================================================================
    phase('negative B. an order whose position is spent fails visibly');
    // The taker still holds the change from phase 5. They offer it, the
    // maker gets as far as the review screen, and then the taker spends it
    // out from under the offer -- the classic race a relay cannot prevent.
    // What matters is that the maker is TOLD, rather than being shown a
    // pending badge for a transaction the node refused.
    const takerRemaining = await st.api.get('/api/positions?pubkey=' + taker.pubKeyHex + '&unspent=true');
    check(takerRemaining.length === 1, `the taker still holds their change (${takerRemaining.length})`);
    const P4 = takerRemaining[0];

    await t.click('Back');
    await t.waitForScreen('wallet', { timeoutMs: 30000 });
    await t.click('Sell a Position');
    await t.waitForScreen('sell', { timeoutMs: 30000 });
    await t.waitFor(`document.querySelector('.position-row')`, { what: 'the taker\'s position' });
    await selectPosition(t, P4.txid);
    await t.fill('sell-price', ASKING_PRICE);
    await t.click('Review');
    await t.waitForScreen('sell-review', { timeoutMs: 20000 });
    await t.click('Publish offer');
    await t.waitForScreen('market', { timeoutMs: 30000 });

    // The maker walks up to the point of no return, but does not confirm.
    await m.click('Back');
    await m.waitForScreen('wallet', { timeoutMs: 30000 });
    await m.click('Order Book');
    await m.waitForScreen('market', { timeoutMs: 30000 });
    await m.waitFor(`document.querySelector('.order-row')`, { what: 'the taker\'s order' });
    await m.click('Fill');
    await m.waitFor(`/Review Fill Transaction/.test(document.body.innerText)`,
      { what: 'the fill review', timeoutMs: 30000 });

    // Now the taker spends it. Straight through the node: this is about what
    // the maker's browser does next, not about the taker's UI.
    await t.click('Back');
    await t.waitForScreen('wallet', { timeoutMs: 30000 });
    await t.click('Send Tokens');
    await t.waitForScreen('send', { timeoutMs: 30000 });
    await t.waitFor(`document.querySelector('.position-row')`, { what: 'the token list' });
    await selectPosition(t, P4.txid);
    await t.fill('send-amount', (P4.value / COIN) / 2);
    await t.fill('send-pubkey', maker.pubKeyHex);
    await t.fill('send-address', maker.address);
    await t.click('Review');
    await t.waitForScreen('send-review', { timeoutMs: 30000 });
    await t.click('Confirm & Broadcast');
    const spendTxid = await broadcastTxid(t);
    await mine(1);
    check((await tx(spendTxid)).confirmations >= 1, 'the taker spent the offered position');

    // The maker confirms an offer that can no longer be filled. The node
    // will refuse it, which the browser logs as a failed request -- that is
    // this phase working, so it is declared here rather than being allowed
    // to fail the console-error check below.
    m.expectedConsoleErrors.push(/40[0-9] \(Bad Request\)/);
    await m.click('Confirm & Broadcast');
    await m.waitFor(
      `document.querySelector('.screen-market-fill .error') || document.querySelector('.tx-status-pending')`,
      { what: 'either a reported failure or (wrongly) a pending badge', timeoutMs: 30000 });
    const reportedFailure = await m.has('.screen-market-fill .error');
    check(reportedFailure, 'the doomed fill is reported on the page, not shown as pending');
    if (reportedFailure) {
      say('      relay said: ' + (await m.textOf('.screen-market-fill .error')));
    }
    check(!(await m.has('.tx-status-pending')),
      'no pending badge is shown for a transaction the node refused');
    await m.screenshot('09-stale-order');

    // ==================================================================
    phase('cross-cutting');
    for (const p of [m, t]) {
      check(p.dialogs.length === 0,
        `[${p.name}] no modal interrupted the flow` +
        (p.dialogs.length ? ': ' + JSON.stringify(p.dialogs) : ''));
      // Anything not explicitly declared by the phase that caused it. The
      // allowance is a list of patterns rather than a count so that an
      // unexpected error arriving alongside an expected one still fails.
      const unexpected = p.consoleErrors.filter(
        (e) => !p.expectedConsoleErrors.some((re) => re.test(e)));
      check(unexpected.length === 0,
        `[${p.name}] no unexpected console errors through the whole run` +
        (unexpected.length ? ': ' + unexpected.join(' | ') : ''));
      check(!(await p.has('.stale-notice')),
        `[${p.name}] no request fell back to the offline cache`);
    }
  } finally {
    say(`\nartifacts: ${st.workdir}`);
    await chrome.close();
    await st.stop();
  }

  say(`\n${failures.length === 0 ? 'ALL PASS' : failures.length + ' FAILED'}`);
  for (const f of failures) say('  ✗ ' + f);
  return failures.length === 0 ? 0 : 1;
}

main().then(
  (code) => process.exit(code),
  (e) => { console.error('\nE2E ABORTED: ' + (e && e.stack || e)); process.exit(1); }
);
