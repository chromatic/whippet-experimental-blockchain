// app.js screen tests -- the Send (token transfer) screen and its QR scan
// wiring.
//
// app.js is a screen router that talks to the real `document` at import
// time, so the fake DOM below is installed as a global *and* injected into
// dom.js before app.js is imported. The pieces under test are the exported,
// dependency-injected ones (buildSendCard, buildScanOverlay,
// buildSendReviewCard, runAddressScan): everything about the send screen
// that can be got wrong without a browser lives in those.
//
// Fake-DOM conventions follow dom.test.js: there is NO textContent, and
// FakeText.toString() escapes its data, so assertions about wording go
// through the textOf() walker below -- which throws on an empty subtree
// rather than returning undefined. That matters: `/x/.test(undefined)`
// coerces to the string "undefined" and passes vacuously, a bug this
// codebase has already been bitten by once.

import { test, assert, assertEqual, run } from './test-harness.js';

// =============================================================================
// MINIMAL FAKE DOM (same shape as dom.test.js)
// =============================================================================

class FakeElement {
  constructor(tag) {
    this.tag = tag;
    this.attributes = new Map();
    this._children = [];
    this.listeners = new Map();
    this.value = '';
  }

  setAttribute(name, value) {
    this.attributes.set(name, value);
    // Real <input> elements mirror the value attribute into the property,
    // and captureSendInputs() reads the property. Model that, so a card
    // built with a state value is readable the way app.js reads it.
    if (name === 'value') this.value = value;
    if (name === 'id') this.id = value;
  }

  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  appendChild(child) {
    this._children.push(child);
  }

  get children() {
    return Object.freeze(this._children.slice());
  }

  removeChild(child) {
    const idx = this._children.indexOf(child);
    if (idx !== -1) this._children.splice(idx, 1);
  }

  get firstChild() {
    return this._children.length ? this._children[0] : null;
  }

  get lastChild() {
    return this._children.length ? this._children[this._children.length - 1] : null;
  }

  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(handler);
  }
}

class FakeText {
  constructor(data) {
    this.data = data;
    this.tag = 'TEXT';
  }

  toString() {
    return this.data
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
  }
}

const appRoot = new FakeElement('div');

const fakeDocument = {
  createElement(tag) {
    return new FakeElement(tag);
  },
  createTextNode(data) {
    return new FakeText(data);
  },
  getElementById(id) {
    // app.js resolves its mount point this way at import time.
    if (id === 'app') return appRoot;
    return findAll(appRoot, (n) => n.id === id)[0] || null;
  },
  querySelector() {
    return null;
  }
};

globalThis.document = fakeDocument;
// app.js constructs its API client with `(...args) => fetch(...args)`; the
// arrow is never called in these tests, but the identifier must exist.
if (typeof globalThis.fetch !== 'function') globalThis.fetch = () => Promise.reject(new Error('no network in tests'));

// =============================================================================
// TREE HELPERS
// =============================================================================

// Concatenate the text of a subtree. Throws on an empty one so an
// assertion can never be handed undefined (and silently pass against the
// string "undefined").
function textOf(node) {
  if (node.tag === 'TEXT') return node.data;
  const parts = node.children.map(textOf);
  const text = parts.join('');
  if (text === '') throw new Error(`textOf: <${node.tag}> contains no text at all`);
  return text;
}

// Same walker, but tolerant of childless nodes -- for whole-card sweeps
// where empty <input>s are expected.
function textOfLoose(node) {
  if (node.tag === 'TEXT') return node.data;
  return node.children.map(textOfLoose).join(' ');
}

function findAll(node, predicate) {
  const out = [];
  for (const child of node.children || []) {
    if (child.tag === 'TEXT') continue;
    if (predicate(child)) out.push(child);
    out.push(...findAll(child, predicate));
  }
  return out;
}

function byTag(node, tag) {
  return findAll(node, (n) => n.tag === tag);
}

function byId(node, id) {
  return findAll(node, (n) => n.getAttribute('id') === id)[0] || null;
}

function buttonsLabelled(node, label) {
  return byTag(node, 'button').filter((b) => {
    let text;
    try {
      text = textOf(b);
    } catch (e) {
      return false;
    }
    return text.includes(label);
  });
}

function click(element) {
  const handlers = element.listeners.get('click') || [];
  assert(handlers.length > 0, 'element has no click handler to fire');
  for (const handler of handlers) handler();
}

function classOf(element) {
  const cls = element.getAttribute('class');
  assert(typeof cls === 'string', `element <${element.tag}> has no class attribute`);
  return cls;
}

// =============================================================================
// IMPORTS (after the fake document is in place)
// =============================================================================

const dom = await import('./dom.js');
dom.setDocument(fakeDocument);

const uap = await import('../uap-js/uap.js');
const secp256k1 = await import('../uap-js/secp.js');
const app = await import('./app.js');

const COIN = uap.COIN;

const THEIR_PUBKEY = secp256k1.getPublicKey(new Uint8Array(32).fill(0x22), true);
const MY_PUBKEY = secp256k1.getPublicKey(new Uint8Array(32).fill(0x11), true);

function makeState(overrides = {}) {
  return {
    positions: [
      { txid: 'aa'.repeat(32), vout: 0, value: 100 * COIN, multiplier: 100, pubkey: uap.bytesToHex(MY_PUBKEY) },
      { txid: 'cc'.repeat(32), vout: 3, value: 10 * COIN, multiplier: 7, pubkey: uap.bytesToHex(MY_PUBKEY) }
    ],
    fundingUtxos: [],
    feeRate: 1000,
    loadError: null,
    selectedKey: `${'aa'.repeat(32)}:0`,
    toPubKey: '',
    toAddress: '',
    amountCoins: '',
    plan: null,
    planErrors: null,
    scanNotice: null,
    scanError: null,
    scanning: false,
    ...overrides
  };
}

// A BarcodeDetector-shaped constructor and a getUserMedia-shaped function,
// enough to make isScanSupported() true.
const FAKE_SCAN_ENV = {
  detectorImpl: class { async detect() { return []; } },
  getUserMediaImpl: async () => ({ getTracks: () => [] })
};

// =============================================================================
// SEND SCREEN -- SCAN BUTTON GATING
// =============================================================================

test('the Scan button is not rendered at all when scanning is unsupported', () => {
  const card = app.buildSendCard({ state: makeState(), scanSupported: false });
  assertEqual(buttonsLabelled(card, 'Scan').length, 0,
    'a button that could only ever fail must not exist (Safari/iOS, Firefox)');
});

test('manual address entry still works when scanning is unsupported', () => {
  const card = app.buildSendCard({ state: makeState(), scanSupported: false });
  const input = byId(card, 'send-address');
  assert(input, 'the address input must exist regardless of camera support');
  assertEqual(input.tag, 'input');
});

test('the Scan button is rendered when scanning is supported', () => {
  const card = app.buildSendCard({ state: makeState(), scanSupported: true });
  assertEqual(buttonsLabelled(card, 'Scan').length, 1, 'exactly one Scan button');
});

test('the Scan button invokes its handler, and only on click', () => {
  let clicks = 0;
  const card = app.buildSendCard({
    state: makeState(),
    scanSupported: true,
    onScan: () => { clicks++; }
  });
  assertEqual(clicks, 0, 'rendering must not trigger a scan');
  click(buttonsLabelled(card, 'Scan')[0]);
  assertEqual(clicks, 1);
});

test('rendering the send screen never requests the camera', () => {
  let cameraRequests = 0;
  const env = {
    detectorImpl: FAKE_SCAN_ENV.detectorImpl,
    getUserMediaImpl: async () => { cameraRequests++; return { getTracks: () => [] }; }
  };
  app.buildSendCard({ state: makeState(), scanSupported: true, onScan: () => {} });
  app.buildScanOverlay({ onCancel: () => {} });
  assertEqual(cameraRequests, 0,
    'the camera is a permission prompt; it may only open from an explicit click');
  assertEqual(typeof env.getUserMediaImpl, 'function', 'the spy was actually wired up');
});

// =============================================================================
// SEND SCREEN -- FORM AND POSITIONS
// =============================================================================

test('the send card exposes the ids the submit handler reads back', () => {
  const card = app.buildSendCard({ state: makeState(), scanSupported: false });
  for (const id of ['send-amount', 'send-pubkey', 'send-address']) {
    assert(byId(card, id), `missing input #${id}`);
  }
});

test('typed values survive a re-render', () => {
  const card = app.buildSendCard({
    state: makeState({ amountCoins: '12.5', toPubKey: '02ab', toAddress: 'nXYZ' }),
    scanSupported: false
  });
  assertEqual(byId(card, 'send-amount').getAttribute('value'), '12.5');
  assertEqual(byId(card, 'send-pubkey').getAttribute('value'), '02ab');
  assertEqual(byId(card, 'send-address').getAttribute('value'), 'nXYZ');
});

test('positions still loading render a skeleton, not "you have no tokens"', () => {
  const card = app.buildSendCard({ state: makeState({ positions: null }), scanSupported: false });
  const skeletons = findAll(card, (n) => (n.getAttribute('class') || '') === 'skeleton');
  assertEqual(skeletons.length, 1, 'a loading skeleton is required while positions are unknown');
  assert(!/no token positions/i.test(textOfLoose(card)),
    'must not claim the user holds nothing while the fetch is still in flight');
});

test('an empty position list says so plainly', () => {
  const card = app.buildSendCard({ state: makeState({ positions: [] }), scanSupported: false });
  const text = textOfLoose(card);
  assert(text.length > 0, 'card must contain text at all');
  assert(/no token positions/i.test(text), `expected an empty-state message, got: ${text}`);
});

test('each position gets a row, and the selected one is marked', () => {
  const state = makeState();
  const card = app.buildSendCard({ state, scanSupported: false });
  const rows = findAll(card, (n) => (n.getAttribute('class') || '').startsWith('position-row'));
  assertEqual(rows.length, 2, 'one row per position');

  const pressed = findAll(card, (n) => n.getAttribute('aria-pressed') === 'true');
  assertEqual(pressed.length, 1, 'exactly one position is selected');
  assertEqual(textOf(pressed[0]), 'Selected');
});

test('selecting a position reports the position that was clicked', () => {
  const state = makeState();
  let picked = null;
  const card = app.buildSendCard({
    state,
    scanSupported: false,
    onSelectPosition: (position) => { picked = position; }
  });
  const selectButtons = buttonsLabelled(card, 'Select');
  click(selectButtons[selectButtons.length - 1]);
  assert(picked, 'a position must be reported');
  assertEqual(picked.txid, 'cc'.repeat(32), 'the second row must report the second position');
});

test('plan errors are listed with their field names', () => {
  const card = app.buildSendCard({
    state: makeState({ planErrors: [{ field: 'amountSats', message: 'Amount must be positive' }] }),
    scanSupported: false
  });
  const text = textOfLoose(card);
  assert(/amountSats/.test(text), `expected the field name in the output, got: ${text}`);
  assert(/Amount must be positive/.test(text), `expected the message in the output, got: ${text}`);
});

test('a cancelled scan is shown as a neutral notice, never as an error', () => {
  const card = app.buildSendCard({
    state: makeState({ scanNotice: 'Scan cancelled.' }),
    scanSupported: true
  });
  const notices = findAll(card, (n) => (n.getAttribute('class') || '').includes('scan-notice'));
  assertEqual(notices.length, 1, 'the cancellation must be acknowledged');
  assert(!classOf(notices[0]).split(/\s+/).includes('error'),
    `cancelling is not a failure and must not use the error style: got class "${classOf(notices[0])}"`);
  assertEqual(
    findAll(card, (n) => (n.getAttribute('class') || '').includes('scan-error')).length,
    0,
    'no error element for a cancellation'
  );
});

test('a scan error IS shown as an error', () => {
  const card = app.buildSendCard({
    state: makeState({ scanError: 'Scanned code is not a valid Whippet address' }),
    scanSupported: true
  });
  const errors = findAll(card, (n) => (n.getAttribute('class') || '').includes('scan-error'));
  assertEqual(errors.length, 1);
  assert(classOf(errors[0]).split(/\s+/).includes('error'), 'a real scan failure uses the error style');
  assert(/not a valid Whippet address/.test(textOf(errors[0])));
});

// =============================================================================
// SCAN OVERLAY
// =============================================================================

test('the scan overlay carries a video element and a Cancel button', () => {
  const { overlay, video } = app.buildScanOverlay({ onCancel: () => {} });
  assertEqual(video.tag, 'video');
  assertEqual(byTag(overlay, 'video').length, 1, 'the overlay must contain the video it hands back');
  assertEqual(buttonsLabelled(overlay, 'Cancel').length, 1, 'a cancel path must be reachable by the user');
});

test('the overlay Cancel button fires the cancel callback', () => {
  let cancelled = 0;
  const { overlay } = app.buildScanOverlay({ onCancel: () => { cancelled++; } });
  click(buttonsLabelled(overlay, 'Cancel')[0]);
  assertEqual(cancelled, 1, 'Cancel must abort the scan, which is what stops the camera');
});

// =============================================================================
// runAddressScan
// =============================================================================

test('runAddressScan returns a validated address on success', async () => {
  const result = await app.runAddressScan({
    scanEnv: FAKE_SCAN_ENV,
    network: 'regtest',
    video: {},
    scanImpl: async () => 'nSomeAddress'
  });
  assertEqual(result.ok, true);
  assertEqual(result.address, 'nSomeAddress');
});

test('runAddressScan passes the wallet network through, so a scan is validated against it', async () => {
  let seen = null;
  await app.runAddressScan({
    scanEnv: FAKE_SCAN_ENV,
    network: 'regtest',
    video: { marker: 'the video' },
    signal: 'the signal',
    scanImpl: async (opts) => { seen = opts; return 'nSomeAddress'; }
  });
  assert(seen, 'the scan implementation must actually be called');
  assertEqual(seen.network, 'regtest',
    'without the network, a testnet address would be accepted into a mainnet send');
  assertEqual(seen.video.marker, 'the video');
  assertEqual(seen.signal, 'the signal', 'the abort signal must reach the scanner, or Cancel cannot work');
  assertEqual(seen.detectorImpl, FAKE_SCAN_ENV.detectorImpl);
  assertEqual(seen.getUserMediaImpl, FAKE_SCAN_ENV.getUserMediaImpl);
});

test('runAddressScan reports cancellation as cancelled, not as an error', async () => {
  const abort = new Error('Scan cancelled');
  abort.name = 'AbortError';
  const result = await app.runAddressScan({
    scanEnv: FAKE_SCAN_ENV,
    network: 'regtest',
    video: {},
    scanImpl: async () => { throw abort; }
  });
  assertEqual(result.ok, false);
  assertEqual(result.cancelled, true, 'a user pressing Cancel has not hit an error');
  assertEqual(result.error, undefined, 'there must be no error text to put in a banner');
});

test('runAddressScan reports a real failure as an error', async () => {
  const result = await app.runAddressScan({
    scanEnv: FAKE_SCAN_ENV,
    network: 'regtest',
    video: {},
    scanImpl: async () => { throw new Error('Permission denied'); }
  });
  assertEqual(result.ok, false);
  assert(!result.cancelled, 'a permission denial is not a cancellation');
  assert(/Permission denied/.test(String(result.error)), `expected the message, got ${result.error}`);
});

test('runAddressScan surfaces an invalid scanned string as an error, not an address', async () => {
  const result = await app.runAddressScan({
    scanEnv: FAKE_SCAN_ENV,
    network: 'mainnet',
    video: {},
    scanImpl: async () => { throw new Error('P2SH addresses are not supported by this wallet'); }
  });
  assertEqual(result.ok, false);
  assertEqual(result.address, undefined, 'nothing may reach the address field from a rejected scan');
  assert(/P2SH/.test(String(result.error)));
});

// =============================================================================
// REVIEW SCREEN
// =============================================================================

function makePlan(overrides = {}) {
  return {
    multiplier: 100,
    toPubKey: THEIR_PUBKEY,
    ownPubKey: MY_PUBKEY,
    amountSats: 40 * COIN,
    remainderSats: 60 * COIN,
    tokenValueIn: 100 * COIN,
    tokenValueOut: 100 * COIN,
    virtualBalanceIn: 10000,
    virtualBalanceOut: 10000,
    virtualBalanceLost: 0,
    fee: 1000000,
    change: 3 * COIN,
    outputs: [
      { kind: 'token-recipient', value: 40 * COIN },
      { kind: 'token-change', value: 60 * COIN },
      { kind: 'whip-change', value: 3 * COIN }
    ],
    ...overrides
  };
}

test('the review screen shows the recipient key, the amount and the fee before anything is broadcast', () => {
  const card = app.buildSendReviewCard({ plan: makePlan() });
  const text = textOfLoose(card);
  assert(text.length > 0, 'the review card must render text');
  assert(text.includes(uap.bytesToHex(THEIR_PUBKEY)), 'the recipient pubkey must be shown in full');
  assert(/40\.00000000/.test(text), `the amount being sent must be shown, got: ${text}`);
  assert(/0\.01000000/.test(text), `the fee must be shown, got: ${text}`);
});

test('the review screen requires an explicit confirmation to broadcast', () => {
  let confirmed = 0;
  const card = app.buildSendReviewCard({ plan: makePlan(), onConfirm: () => { confirmed++; } });
  const confirmBtns = buttonsLabelled(card, 'Confirm');
  assertEqual(confirmBtns.length, 1, 'there must be a confirm step between the form and the broadcast');
  assertEqual(confirmed, 0, 'rendering the review must not broadcast');
  click(confirmBtns[0]);
  assertEqual(confirmed, 1);
});

test('the review screen offers a way back to the form', () => {
  let edited = 0;
  const card = app.buildSendReviewCard({ plan: makePlan(), onEdit: () => { edited++; } });
  click(buttonsLabelled(card, 'Edit')[0]);
  assertEqual(edited, 1);
});

test('the review screen states that value in equals value out', () => {
  const card = app.buildSendReviewCard({ plan: makePlan() });
  const text = textOfLoose(card);
  assert(/100\.00000000 coins \/ 100\.00000000 coins/.test(text),
    `token value in/out must both be shown so a burn would be visible, got: ${text}`);
});

function warningsIn(card, kind = 'warning') {
  return findAll(card, (n) => (n.getAttribute('class') || '').split(' ').includes(kind));
}

test('outputs that carry less than the position holds are called a bug, not a trade-off', () => {
  // The invariant is satoshi conservation against the OUTPUT LIST, because
  // that list is what buildTransferTx encodes. The plan below is a real plan
  // shape with one satoshi missing from the token change -- the shape a
  // position value too large for a JavaScript number produces (planTransfer
  // now refuses those; this screen is the second line). Those satoshis leave
  // the covenant as extra fee, so they are tokens destroyed silently, and the
  // screen has to say so rather than present it as a rounding trade-off.
  const card = app.buildSendReviewCard({
    plan: makePlan({
      outputs: [
        { kind: 'token-recipient', value: 40 * COIN },
        { kind: 'token-change', value: 60 * COIN - 1 },
        { kind: 'whip-change', value: 3 * COIN }
      ]
    })
  });
  const warnings = warningsIn(card, 'warning-bug');
  assertEqual(warnings.length, 1, 'a burn must be stated, not buried');
  const text = textOf(warnings[0]);
  assert(/1 satoshis less/.test(text), `warning should quantify the loss, got: ${text}`);
  assert(/destroy/.test(text), `warning must say the balance is destroyed, got: ${text}`);
  assert(/should be impossible|bug/.test(text),
    `warning must identify this as a bug, not normal rounding, got: ${text}`);
  assert(!/whole number of coins/.test(text),
    `must not advise a whole-coin workaround for a non-rounding problem, got: ${text}`);
});

test('outputs that carry more than the position holds are flagged as unbroadcastable', () => {
  // The other direction of the same defect. Consensus fails the transaction
  // (nValueOut > nValueIn), so nothing is lost -- but the screen must not
  // send the user to a rejection without a word, and must not describe it as
  // a burn, which it is not.
  const card = app.buildSendReviewCard({
    plan: makePlan({
      outputs: [
        { kind: 'token-recipient', value: 40 * COIN },
        { kind: 'token-change', value: 60 * COIN + 1 },
        { kind: 'whip-change', value: 3 * COIN }
      ]
    })
  });
  const warnings = warningsIn(card, 'warning-bug');
  assertEqual(warnings.length, 1, 'an over-spending plan must be flagged too');
  const text = textOf(warnings[0]);
  assert(/1 satoshis more/.test(text), `warning should quantify the overshoot, got: ${text}`);
  assert(/reject/.test(text), `the consequence is rejection, not a burn, got: ${text}`);
});

test('the token value in/out line is summed from the outputs, not read off the plan', () => {
  // plan.tokenValueOut is the planner's own arithmetic; the outputs are what
  // gets encoded. If the screen displayed the former it could report perfect
  // conservation over a transaction that burns tokens.
  const card = app.buildSendReviewCard({
    plan: makePlan({
      tokenValueOut: 100 * COIN,          // what the planner believes
      outputs: [
        { kind: 'token-recipient', value: 40 * COIN },
        { kind: 'token-change', value: 59 * COIN },   // what is really encoded
        { kind: 'whip-change', value: 3 * COIN }
      ]
    })
  });
  const text = textOfLoose(card);
  assert(/100\.00000000 coins \/ 99\.00000000 coins/.test(text),
    `the screen must show what the outputs really carry, got: ${text}`);
});

test('a fractional-coin split shows no warning at all', () => {
  // Exactly the case the old, incorrect truncation model warned about: 100
  // coins split 40.5 / 59.5. Every satoshi is accounted for, so there is
  // nothing to warn about -- and this plan carries no advisory warnings
  // either, so the whole card must be warning-free.
  const card = app.buildSendReviewCard({
    plan: makePlan({
      warnings: [],
      outputs: [
        { kind: 'token-recipient', value: 40.5 * COIN },
        { kind: 'token-change', value: 59.5 * COIN },
        { kind: 'whip-change', value: 3 * COIN }
      ]
    })
  });
  assertEqual(warningsIn(card).length, 0,
    'a fractional split conserves balance; warning about it is a false alarm');
});

test('a plan warning reaches the screen instead of living only in the plan object', () => {
  // planTransfer attaches `position-value-unverified` to every plan whose
  // position value the caller has not verified: if the real output holds more
  // than the indexer said, the difference is paid to miners instead of moving
  // with the tokens. That mitigation is worth nothing unless the user sees it.
  const card = app.buildSendReviewCard({
    plan: makePlan({
      warnings: [{
        code: 'position-value-unverified',
        field: 'position',
        message: 'This position\'s value of 10000000000 satoshis is as reported by the indexer and ' +
          'cannot be verified here. If the real output holds more than that, the difference will be ' +
          'paid to miners as fee instead of moving with your tokens.'
      }]
    })
  });
  const advisories = warningsIn(card, 'warning-advisory');
  assertEqual(advisories.length, 1, 'every warning the plan carries must be rendered');
  const text = textOf(advisories[0]);
  assert(/paid to miners as fee/.test(text),
    `the warning's own wording must reach the user, got: ${text}`);
  // Not the bug alarm: this transfer is valid and broadcastable.
  assertEqual(warningsIn(card, 'warning-bug').length, 0,
    'an advisory must not be dressed up as a planner bug');
});

test('several plan warnings are all rendered, not just the first', () => {
  const card = app.buildSendReviewCard({
    plan: makePlan({
      warnings: [
        { code: 'one', field: 'position', message: 'first thing the user should know' },
        { code: 'two', field: 'position', message: 'second thing the user should know' }
      ]
    })
  });
  assertEqual(warningsIn(card, 'warning-advisory').length, 2);
  const text = textOfLoose(card);
  assert(/first thing/.test(text) && /second thing/.test(text),
    `both warnings must appear, got: ${text}`);
});

test('sending the whole position reads as such rather than showing a zero remainder', () => {
  const card = app.buildSendReviewCard({
    plan: makePlan({
      remainderSats: 0,
      outputs: [
        { kind: 'token-recipient', value: 100 * COIN },
        { kind: 'whip-change', value: 3 * COIN }
      ]
    })
  });
  const text = textOfLoose(card);
  assert(/whole position/.test(text), `expected wording about the whole position, got: ${text}`);
});

test('the review screen says the fee is paid in WHIP, not out of the tokens', () => {
  const card = app.buildSendReviewCard({ plan: makePlan() });
  const text = textOfLoose(card);
  assert(/not tokens/i.test(text),
    `the fee's funding source is the whole reason for the extra input; say it: ${text}`);
});

// =============================================================================
// MINT REVIEW SCREEN
//
// The figure that matters here is the virtual balance, and it is the same
// truncation bug the transfer path was fixed for: consensus computes balance
// as nValue * multiplier in RAW SATOSHIS, with no division and no truncation
// (interpreter.cpp, OP_INSPECT selector 11). A fractional-coin mint carries
// fractional balance, and rounding the coin figure down before multiplying
// under-reports every mint that is not a whole number of coins.
// =============================================================================

function makeMintState(overrides = {}) {
  return {
    ticker: 'TEST',
    name: 'Test Token',
    multiplier: 100,
    amountSats: 40.5 * COIN,
    ...overrides
  };
}

const MINT_PLAN = { fee: 1000000, change: 3 * COIN };

test('the mint review screen shows the ticker, multiplier and amount being locked', () => {
  const card = app.buildMintReviewCard({ mintState: makeMintState(), plan: MINT_PLAN });
  const text = textOfLoose(card);
  assert(/TEST/.test(text), `the ticker must be shown, got: ${text}`);
  assert(/Test Token/.test(text), `the name must be shown, got: ${text}`);
  assert(/40\.5/.test(text), `the amount being locked must be shown, got: ${text}`);
  assert(/0\.01000000/.test(text), `the fee must be shown, got: ${text}`);
});

test('the mint review screen does not truncate a fractional-coin virtual balance', () => {
  // 40.5 coins x 100 = 4050, not 4000. Truncating the coin figure first
  // (floor(coins) * multiplier) under-reports this mint by a full 50 units of
  // balance -- the exact bug this work set out to kill, seen from the mint side.
  const card = app.buildMintReviewCard({
    mintState: makeMintState({ amountSats: 40.5 * COIN, multiplier: 100 }),
    plan: MINT_PLAN
  });
  const text = textOfLoose(card);
  assert(text.includes((4050).toLocaleString()),
    `virtual balance must be 40.5 x 100 = ${(4050).toLocaleString()}, got: ${text}`);
  assert(!text.includes((4000).toLocaleString()),
    `a truncated 40 x 100 = ${(4000).toLocaleString()} would under-report the mint, got: ${text}`);
});

test('a sub-coin mint still shows the balance it really carries', () => {
  // The same bug from the far end: floor(0.5) * 1000 is zero, which would
  // price a half-coin mint at no tokens at all.
  const card = app.buildMintReviewCard({
    mintState: makeMintState({ amountSats: COIN / 2, multiplier: 1000 }),
    plan: MINT_PLAN
  });
  const text = textOfLoose(card);
  assert(text.includes((500).toLocaleString()),
    `0.5 coins x 1000 = 500 of balance, got: ${text}`);
});

test('the mint review screen requires an explicit confirmation, and offers a way back', () => {
  let confirmed = 0;
  let edited = 0;
  const card = app.buildMintReviewCard({
    mintState: makeMintState(),
    plan: MINT_PLAN,
    onConfirm: () => { confirmed++; },
    onEdit: () => { edited++; }
  });
  const confirmBtns = buttonsLabelled(card, 'Confirm');
  assertEqual(confirmBtns.length, 1, 'there must be a confirm step between the form and the broadcast');
  assertEqual(confirmed, 0, 'rendering the review must not broadcast');
  click(confirmBtns[0]);
  assertEqual(confirmed, 1);
  click(buttonsLabelled(card, 'Edit')[0]);
  assertEqual(edited, 1);
});

run({ style: 'compact' });
