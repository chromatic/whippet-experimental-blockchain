// Minimal UI shell for wallet
// DOM-safe using dom.js helpers, exercises wallet.js and api.js

import { Wallet } from './wallet.js';
import { WHIPPET_KEY_PATH } from './bip32.js';
import { planSell, buildSellOrder } from './sell.js';
import { myOrders, planCancel, describeCancelFailure } from './cancel.js';
import * as dom from './dom.js';
import { API, pollForConfirmation, staleInfo } from './api.js';
import { planMint, buildMintTx } from './mint.js';
import { describeOrder, planFill, buildFillTx } from './market.js';
import {
  planTransfer,
  buildTransferTx,
  OUTPUT_TOKEN_RECIPIENT,
  OUTPUT_TOKEN_CHANGE,
  OUTPUT_WHIP_CHANGE
} from './transfer.js';
import * as qrscan from './qrscan.js';
import * as secp256k1 from '../uap-js/secp.js';
import * as uap from '../uap-js/uap.js';

const app = document.getElementById('app');
const storage = new Map();
let wallet = null;
let currentScreen = 'init';
// Sell (maker order) flow state. `positions` is null until the first fetch
// resolves, same convention as sendState.positions -- see buildSellCard.
let sellState = freshSellState();

function freshSellState() {
  return { positions: null, loadError: null, selectedKey: null, priceSats: '', plan: null, error: null };
}

// My-orders flow state: viewing and cancelling the wallet's own standing
// sell orders. `orders` is null until the first fetch resolves (renders a
// skeleton, not "you have no orders" -- see renderMyOrders), same
// convention as sendState.positions.
let myOrdersState = {
  orders: null,
  loadError: null,
  selectedOrder: null,
  cancelError: null,
  cancelling: false
};

function freshMyOrdersState() {
  return { orders: null, loadError: null, selectedOrder: null, cancelError: null, cancelling: false };
}

// The most recent broadcast's status, or null if nothing has been
// broadcast this session. { txid, state: 'pending'|'confirmed'|'timeout' }.
//
// Set optimistically the instant broadcast() resolves (see confirmFill):
// the node accepting a transaction into its mempool is not confirmation,
// but Whippet's ~6s blocks make it reasonable to show a pending state
// immediately rather than block the UI on a wait. pollForConfirmation
// (api.js) always resolves this to either 'confirmed' or 'timeout' -- see
// the comment in confirmFill for what happens if confirmation never comes.
let broadcastStatus = null;

// Mint flow state
let mintState = {
  ticker: '',
  name: '',
  multiplier: 100,
  amountSats: 1000 * uap.COIN,
  plan: null,
  planErrors: null
};

// Send (token transfer) flow state.
//
// `positions` is null until the first fetch resolves, which is what tells
// renderSend to paint a skeleton rather than "you have no tokens" -- those
// are very different statements to make to someone holding tokens.
let sendState = {
  positions: null,
  fundingUtxos: [],
  feeRate: null,
  loadError: null,
  selectedKey: null,
  toPubKey: '',
  toAddress: '',
  amountCoins: '',
  plan: null,
  planErrors: null,
  // Scan feedback lives next to the address field. `scanNotice` is neutral
  // wording (used for cancellation, which is not an error), `scanError` is
  // shown as an error. Cancelling must never look like a failure.
  scanNotice: null,
  scanError: null,
  scanning: false
};

function positionKey(position) {
  return `${position.txid}:${position.vout}`;
}

function freshSendState() {
  return {
    positions: null,
    fundingUtxos: [],
    feeRate: null,
    loadError: null,
    selectedKey: null,
    toPubKey: '',
    toAddress: '',
    amountCoins: '',
    plan: null,
    planErrors: null,
    scanNotice: null,
    scanError: null,
    scanning: false
  };
}

// Market/order book flow state
let marketState = {
  orders: [],
  selectedOrder: null,
  fillPlan: null,
  fillPlanErrors: null
};

// The marketplace binary serves this page and its API from the same origin
// (Phase 2.3 of doc/uap-marketplace-website-plan.md), so the base URL is
// relative: no CORS, and no hostname baked into the bundle.
//
// This must NEVER point at the node's RPC port (33665 mainnet). Node RPC is
// a trusted-operator interface with full wallet authority and no CORS story
// -- a browser reaches the chain only through the indexer's narrow,
// rate-limited API. For a split deployment, override with
// <meta name="uap-api-base" content="https://api.example.com">.
const apiBaseMeta = document.querySelector('meta[name="uap-api-base"]');
const apiClient = new API({
  baseUrl: (apiBaseMeta && apiBaseMeta.content) || '/api',
  fetchImpl: (...args) => fetch(...args)
});

function render() {
  dom.clear(app);

  if (currentScreen === 'init') {
    renderInit();
  } else if (currentScreen === 'create') {
    renderCreate();
  } else if (currentScreen === 'restore') {
    renderRestore();
  } else if (currentScreen === 'unlock') {
    renderUnlock();
  } else if (currentScreen === 'wallet') {
    renderWallet();
  } else if (currentScreen === 'mint') {
    renderMint();
  } else if (currentScreen === 'mint-review') {
    renderMintReview();
  } else if (currentScreen === 'market') {
    renderMarket();
  } else if (currentScreen === 'market-fill') {
    renderMarketFill();
  } else if (currentScreen === 'send') {
    renderSend();
  } else if (currentScreen === 'sell') {
    renderSell();
  } else if (currentScreen === 'sell-review') {
    renderSellReview();
  } else if (currentScreen === 'my-orders') {
    renderMyOrders();
  } else if (currentScreen === 'my-orders-cancel') {
    renderMyOrdersCancel();
  } else if (currentScreen === 'migrate') {
    renderMigrate();
  } else if (currentScreen === 'send-review') {
    renderSendReview();
  }
}

// Returns a click handler that switches to `screen` and re-renders. The
// same two-statement closure (`() => { currentScreen = X; render(); }`) was
// being hand-written at every navigation call site; this just names it once.
function goTo(screen) {
  return () => { currentScreen = screen; render(); };
}

// The "Back" button that ends nearly every screen: same class, same label,
// same shape, differing only in which screen it returns to.
function backButton(screen) {
  return dom.el('button', { class: 'btn btn-secondary', onClick: goTo(screen) }, 'Back');
}

function renderInit() {
  const createBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: createNewWallet },
    'Create Wallet'
  );
  const restoreBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: restoreWallet },
    'Restore from Mnemonic'
  );

  const buttonGroup = dom.el('div', { class: 'button-group' },
    createBtn,
    restoreBtn
  );

  const card = dom.el('div', { class: 'card' },
    dom.el('h1', {}, 'Whippet Wallet'),
    dom.el('p', {}, 'Self-custody wallet for the Whippet marketplace'),
    buttonGroup
  );

  const screenDiv = dom.el('div', { class: 'screen screen-init' }, card);
  app.appendChild(screenDiv);
}

async function renderCreate() {
  if (!wallet) {
    wallet = await Wallet.create({ storage });
  }

  const passphraseInput = dom.el('input', {
    type: 'password',
    id: 'passphrase',
    placeholder: 'Leave blank for no passphrase'
  });

  const saveBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: finishWalletSetup },
    'I\'ve Saved the Phrase'
  );
  const cancelBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: goToInit },
    'Cancel'
  );

  const buttonGroup = dom.el('div', { class: 'button-group' },
    saveBtn,
    cancelBtn
  );

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Your Backup Phrase'),
    dom.el('p', { class: 'warning' },
      '⚠️ Save this phrase in a safe place. Anyone with it can access your funds.'
    ),
    dom.el('div', { class: 'mnemonic-display' },
      dom.el('code', {}, wallet.mnemonic)
    ),
    dom.el('p', { class: 'hint' },
      `This is a 12-word BIP39 mnemonic. Together with your passphrase it derives your private key at ${WHIPPET_KEY_PATH}. Write the words down: any BIP39 wallet that can derive that path will recover this key.`
    ),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Set a passphrase (optional but recommended):'),
      passphraseInput
    ),
    buttonGroup
  );

  const screenDiv = dom.el('div', { class: 'screen screen-create' }, card);
  app.appendChild(screenDiv);
}

async function renderRestore() {
  const mnemonicInput = dom.el('textarea', {
    id: 'mnemonic-input',
    placeholder: 'word1 word2 word3 ...',
    rows: '4'
  });

  const passphraseInput = dom.el('input', {
    type: 'password',
    id: 'restore-passphrase',
    placeholder: 'Leave blank if none'
  });

  const restoreBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: doRestore },
    'Restore Wallet'
  );
  const cancelBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: goToInit },
    'Cancel'
  );

  const errorDiv = dom.el('div', { id: 'restore-error', style: 'display:none; color: red; margin-top: 1rem;' });

  const buttonGroup = dom.el('div', { class: 'button-group' },
    restoreBtn,
    cancelBtn
  );

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Restore from Mnemonic'),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Enter your 12 or 24-word mnemonic:'),
      mnemonicInput
    ),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Passphrase (if you used one):'),
      passphraseInput
    ),
    buttonGroup,
    errorDiv
  );

  const screenDiv = dom.el('div', { class: 'screen screen-restore' }, card);
  app.appendChild(screenDiv);
}

async function renderUnlock() {
  const passphraseInput = dom.el('input', {
    type: 'password',
    id: 'unlock-passphrase',
    placeholder: 'Enter passphrase'
  });

  const unlockBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: doUnlock },
    'Unlock'
  );
  const cancelBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: goToInit },
    'Cancel'
  );

  const errorDiv = dom.el('div', { id: 'unlock-error', style: 'display:none; color: red; margin-top: 1rem;' });

  const buttonGroup = dom.el('div', { class: 'button-group' },
    unlockBtn,
    cancelBtn
  );

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Unlock Wallet'),
    dom.el('p', {}, 'Your wallet is encrypted. Enter your passphrase to unlock it.'),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Passphrase:'),
      passphraseInput
    ),
    buttonGroup,
    errorDiv
  );

  const screenDiv = dom.el('div', { class: 'screen screen-unlock' }, card);
  app.appendChild(screenDiv);
}

async function renderWallet() {
  const copyBtn = dom.el('button',
    { class: 'btn-copy', onClick: copyAddress },
    'Copy'
  );

  const infoRow = dom.el('div', { class: 'info-row' },
    dom.el('label', {}, 'Address:'),
    dom.el('code', { class: 'address' }, wallet.address),
    copyBtn
  );

  const mintBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: goToMint },
    'Mint Token'
  );
  const sendBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: goToSend },
    'Send Tokens'
  );
  const marketBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: goToMarket },
    'Order Book'
  );
  const sellBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: startSell },
    'Sell a Position'
  );
  const myOrdersBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: goToMyOrders },
    'My Orders'
  );
  const lockBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: lockWallet },
    'Lock Wallet'
  );
  const signOutBtn = dom.el('button',
    { class: 'btn btn-danger', onClick: goToInit },
    'Sign Out'
  );

  const actionsButtonGroup = dom.el('div', { class: 'button-group' },
    mintBtn,
    sendBtn,
    marketBtn,
    sellBtn,
    myOrdersBtn,
    lockBtn,
    signOutBtn
  );

  // A wallet on the pre-fix derivation still works, but its recovery words
  // do not mean what the app told the user they meant. Say so where it
  // cannot be missed, rather than leaving the flag for nobody to read.
  const legacyNotice = wallet.usesLegacyDerivation
    ? dom.el('div', { class: 'warning banner' },
        dom.el('span', {}, 'Your recovery words cannot restore this wallet in other software. '),
        dom.el('button',
          { class: 'btn btn-sm', onClick: goTo('migrate') },
          'What this means'))
    : null;

  // Balance section starts as a skeleton and is filled in once the UTXO
  // fetch resolves (see loadBalanceSection below). Building it as a
  // skeleton *and appending the screen before awaiting anything* is the
  // fix for there being no loading state at all: previously this function
  // awaited the fetch before the section (or anything else on the screen)
  // was ever appended to `app`, so the screen just stayed blank/stale for
  // the whole fetch instead of showing a placeholder.
  const balanceSection = dom.el('div', { class: 'section' },
    dom.el('h3', {}, 'Balance'),
    dom.skeleton({ lines: 3 })
  );

  const statusBadge = broadcastStatus
    ? dom.txStatus(broadcastStatus.state, broadcastStatus.txid)
    : null;

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Wallet'),
    statusBadge,
    ...(legacyNotice ? [legacyNotice] : []),
    dom.el('div', { class: 'wallet-info' }, infoRow),
    balanceSection,
    dom.el('div', { class: 'section' },
      dom.el('h3', {}, 'Actions'),
      actionsButtonGroup
    )
  );

  const screenDiv = dom.el('div', { class: 'screen screen-wallet' }, card);
  app.appendChild(screenDiv);

  // Kick the fetch off only after the skeleton is already on screen. Not
  // awaited here on purpose -- renderWallet's job is to paint the screen
  // immediately; loadBalanceSection paints over its own skeleton in place
  // once the fetch settles.
  loadBalanceSection(balanceSection);
}

async function loadBalanceSection(section) {
  try {
    const utxos = await apiClient.getUtxos(wallet.address);
    dom.clear(section);
    section.appendChild(dom.el('h3', {}, 'Balance'));

    // If the service worker answered this from cache because the network
    // was unreachable, say so above the figure. A cached balance is not a
    // balance; it is a balance as of some earlier moment, and only the user
    // can judge whether that is good enough to act on.
    const freshness = staleInfo(utxos);
    if (freshness.stale) {
      section.appendChild(dom.staleNotice(freshness.cachedAt));
    }

    let totalValue = 0;
    const rows = [];
    for (const utxo of utxos) {
      totalValue += utxo.value;
      rows.push(
        dom.el('div', { class: 'utxo-row' },
          dom.el('span', { class: 'txid' }, utxo.txid.slice(0, 16) + '...'),
          dom.el('span', { class: 'amount' }, utxo.value.toString())
        )
      );
    }

    section.appendChild(dom.el('p', {}, `Total: ${totalValue}`));
    for (const row of rows) {
      section.appendChild(row);
    }
  } catch (e) {
    // API not available, replace the skeleton with the error -- it must
    // not be left showing a skeleton forever just because the fetch failed.
    dom.clear(section);
    section.appendChild(dom.el('h3', {}, 'Balance'));
    section.appendChild(dom.el('p', { class: 'placeholder' },
      '❌ Failed to load balance: ',
      e.message
    ));
  }
}

// Event handlers
async function createNewWallet() {
  wallet = await Wallet.create({ storage });
  currentScreen = 'create';
  render();
}

function restoreWallet() {
  currentScreen = 'restore';
  render();
}

function goToInit() {
  wallet = null;
  // A signed-out session has no business still displaying the previous
  // wallet's broadcast status once someone signs back in (possibly as a
  // different wallet).
  broadcastStatus = null;
  currentScreen = 'init';
  render();
}

async function finishWalletSetup() {
  const passphrase = document.getElementById('passphrase').value;

  try {
    await wallet.lock(passphrase);
    currentScreen = 'unlock';
    render();
  } catch (e) {
    alert(`Error locking wallet: ${e.message}`);
  }
}

async function doRestore() {
  const mnemonic = document.getElementById('mnemonic-input').value.trim();
  const passphrase = document.getElementById('restore-passphrase').value;
  const errorEl = document.getElementById('restore-error');

  try {
    wallet = await Wallet.create({ storage, mnemonic });
    await wallet.lock(passphrase);
    currentScreen = 'unlock';
    render();
  } catch (e) {
    errorEl.textContent = e.message;
    errorEl.style.display = 'block';
  }
}

async function doUnlock() {
  const passphrase = document.getElementById('unlock-passphrase').value;
  const errorEl = document.getElementById('unlock-error');

  try {
    if (!wallet) {
      const stored = storage.get('wallet');
      if (!stored) throw new Error('No wallet in storage');
      wallet = new Wallet({ storage, restore: true });
    }

    const ok = await wallet.unlock(passphrase);
    if (!ok) {
      errorEl.textContent = 'Wrong passphrase';
      errorEl.style.display = 'block';
    } else {
      currentScreen = 'wallet';
      render();
    }
  } catch (e) {
    errorEl.textContent = e.message;
    errorEl.style.display = 'block';
  }
}

async function lockWallet() {
  wallet._privKey = null;
  wallet._locked = true;
  currentScreen = 'unlock';
  render();
}

function copyAddress() {
  navigator.clipboard.writeText(wallet.address).then(() => {
    alert('Address copied to clipboard');
  }).catch(() => {
    alert('Failed to copy address');
  });
}

// Mint flow handlers
function goToMint() {
  mintState = {
    ticker: '',
    name: '',
    multiplier: 100,
    amountSats: 1000 * uap.COIN,
    plan: null,
    planErrors: null
  };
  currentScreen = 'mint';
  render();
}

async function renderMint() {
  const tickerInput = dom.el('input', {
    id: 'mint-ticker',
    type: 'text',
    placeholder: 'e.g., DOGE',
    value: mintState.ticker
  });

  const nameInput = dom.el('input', {
    id: 'mint-name',
    type: 'text',
    placeholder: 'e.g., Dogecoin Token',
    value: mintState.name
  });

  const multiplierInput = dom.el('input', {
    id: 'mint-multiplier',
    type: 'number',
    placeholder: '100',
    value: mintState.multiplier.toString()
  });

  const amountInput = dom.el('input', {
    id: 'mint-amount',
    type: 'number',
    placeholder: '1000',
    value: (mintState.amountSats / uap.COIN).toString()
  });

  const reviewBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: planMintTx },
    'Review'
  );
  const backBtn = backButton('wallet');

  const errorsList = [];
  if (mintState.planErrors && mintState.planErrors.length > 0) {
    for (const error of mintState.planErrors) {
      errorsList.push(
        dom.el('div', { class: 'error-field' },
          dom.el('strong', {}, error.field + ':'),
          ' ' + error.message
        )
      );
    }
  }

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Mint New Token'),
    errorsList.length > 0 ? dom.el('div', { class: 'error-list' }, ...errorsList) : null,
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Ticker (1-16 characters):'),
      tickerInput
    ),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Name (0-64 characters, optional):'),
      nameInput
    ),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Multiplier (0-2147483647):'),
      multiplierInput
    ),
    dom.el('div', { class: 'form-group' },
      dom.el('label', {}, 'Amount to Lock (coins):'),
      amountInput
    ),
    dom.el('div', { class: 'button-group' },
      reviewBtn,
      backBtn
    )
  );

  const screenDiv = dom.el('div', { class: 'screen screen-mint' }, card);
  app.appendChild(screenDiv);
}

async function planMintTx() {
  mintState.ticker = document.getElementById('mint-ticker').value;
  mintState.name = document.getElementById('mint-name').value;
  mintState.multiplier = parseInt(document.getElementById('mint-multiplier').value) || 0;
  mintState.amountSats = parseFloat(document.getElementById('mint-amount').value) * uap.COIN;

  // Fetch UTXOs for the wallet
  let utxos = [];
  try {
    utxos = await apiClient.getUtxos(wallet.address);
  } catch (e) {
    alert(`Failed to fetch UTXOs: ${e.message}`);
    return;
  }

  // Plan the transaction
  const feeRate = await apiClient.getFeeRate();
  const planResult = planMint({
    ticker: mintState.ticker,
    name: mintState.name,
    multiplier: mintState.multiplier,
    amountSats: mintState.amountSats,
    utxos,
    feeRate,
    address: wallet.address
  });

  if (!planResult.ok) {
    mintState.planErrors = planResult.errors;
    render();  // Re-render with errors
    return;
  }

  mintState.plan = planResult.plan;
  mintState.planErrors = null;
  currentScreen = 'mint-review';
  render();
}

/**
 * The mint review step, as a pure function of the state it displays.
 *
 * Exported and dependency-injected for the same reason buildSendReviewCard
 * is: the figures on a review screen are the last thing a user sees before
 * committing, and the one that is easy to get wrong is the virtual balance.
 * It is `coins * multiplier` and NOT `floor(coins) * multiplier`: consensus
 * computes balance as nValue * multiplier in raw satoshis, with no division
 * and no truncation (interpreter.cpp, OP_INSPECT selector 11), so a
 * fractional-coin mint carries fractional balance. Truncating here would
 * under-report every non-whole-coin mint.
 */
export function buildMintReviewCard({
  mintState,
  plan,
  onConfirm = () => {},
  onEdit = () => {}
} = {}) {
  const coins = mintState.amountSats / uap.COIN;
  const feeCoins = plan.fee / uap.COIN;
  const changeCoins = plan.change / uap.COIN;
  const virtualBalance = coins * mintState.multiplier;

  const confirmBtn = dom.el('button',
    { class: 'btn btn-primary', onClick: onConfirm },
    'Confirm & Broadcast'
  );
  const editBtn = dom.el('button',
    { class: 'btn btn-secondary', onClick: onEdit },
    'Edit'
  );

  return dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Review Mint Transaction'),
    dom.el('div', { class: 'review-section' },
      dom.el('h3', {}, 'Token Details'),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Ticker:'),
        dom.el('code', {}, mintState.ticker)
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Name:'),
        dom.el('code', {}, mintState.name || '(empty)')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Multiplier:'),
        dom.el('code', {}, mintState.multiplier.toString())
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Virtual Balance:'),
        dom.el('code', {}, virtualBalance.toLocaleString())
      )
    ),
    dom.el('div', { class: 'review-section' },
      dom.el('h3', {}, 'Transaction Details'),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Amount Locked:'),
        dom.el('code', {}, coins.toLocaleString() + ' coins')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Estimated Fee:'),
        dom.el('code', {}, feeCoins.toFixed(8) + ' coins')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Change:'),
        dom.el('code', {}, changeCoins > 0 ? changeCoins.toFixed(8) + ' coins' : '(none)')
      )
    ),
    dom.el('div', { class: 'button-group' },
      confirmBtn,
      editBtn
    )
  );
}

async function renderMintReview() {
  if (!mintState.plan) {
    currentScreen = 'mint';
    render();
    return;
  }

  const card = buildMintReviewCard({
    mintState,
    plan: mintState.plan,
    onConfirm: confirmMint,
    onEdit: goTo('mint')
  });

  const screenDiv = dom.el('div', { class: 'screen screen-mint-review' }, card);
  app.appendChild(screenDiv);
}

async function confirmMint() {
  if (!mintState.plan) {
    alert('No plan available. Please review again.');
    return;
  }

  try {
    // Get UTXOs again for building the transaction
    const utxos = await apiClient.getUtxos(wallet.address);

    // Configure secp
    uap.configureSecp(secp256k1);

    // Build the mint transaction
    const pubKey = secp256k1.getPublicKey(wallet.privKey, true);
    const buildResult = await buildMintTx({
      secp: secp256k1,
      plan: mintState.plan,
      privKey: wallet.privKey,
      pubKey,
      utxos,
      changeAddress: wallet.address
    });

    // For now, show the result
    alert(`Mint prepared. Script: ${buildResult.mintScript.length} bytes\nSalt: ${Array.from(buildResult.salt).map(b => b.toString(16).padStart(2, '0')).join('')}`);
    currentScreen = 'wallet';
    render();
  } catch (e) {
    alert(`Error building transaction: ${e.message}`);
  }
}

// =============================================================================
// SEND (UAP token transfer) FLOW
// =============================================================================
//
// The transaction shape and the reason the recipient is a PUBLIC KEY rather
// than an address are documented at the top of transfer.js. In short: the
// covenant script embeds a real pubkey, an address is only a hash of one,
// and a hash cannot be inverted -- so the address field here is a
// cross-check on the pasted pubkey (and what the QR scanner fills in), not
// the destination itself.

/**
 * The browser capabilities qrscan.js needs, or undefined for each one that
 * is missing. Both are absent on Safari/iOS and Firefox; isScanSupported()
 * turns that into the single boolean the UI branches on, and the "Scan"
 * button is then never rendered at all rather than rendered-and-broken.
 *
 * Read lazily, per call: this must never touch `navigator.mediaDevices` at
 * module load, and looking it up here (rather than caching at import time)
 * also keeps it injectable in tests.
 * @private
 */
function scanEnvironment() {
  const g = typeof globalThis !== 'undefined' ? globalThis : {};
  const nav = g.navigator;
  const hasGetUserMedia = nav && nav.mediaDevices && typeof nav.mediaDevices.getUserMedia === 'function';
  return {
    detectorImpl: typeof g.BarcodeDetector === 'function' ? g.BarcodeDetector : undefined,
    getUserMediaImpl: hasGetUserMedia ? nav.mediaDevices.getUserMedia.bind(nav.mediaDevices) : undefined
  };
}

/**
 * Build the camera preview overlay: a <video> for the stream and a Cancel
 * button. The overlay is only ever created from a click handler, never from
 * a render function -- creating it is one step away from opening the camera,
 * and the camera must only ever open on an explicit user action.
 *
 * Exported for tests.
 */
export function buildScanOverlay({ onCancel = () => {} } = {}) {
  // playsinline/muted keep iOS from taking the video fullscreen; harmless
  // elsewhere. autoplay is NOT set: scanAddressFromCamera calls play()
  // itself, after it has a stream.
  const video = dom.el('video', {
    class: 'scan-video',
    playsinline: 'true',
    muted: 'true'
  });

  const cancelBtn = dom.el('button',
    { class: 'btn btn-secondary scan-cancel', onClick: onCancel },
    'Cancel'
  );

  const overlay = dom.el('div',
    { class: 'scan-overlay', role: 'dialog', 'aria-label': 'Scan a recipient address' },
    dom.el('div', { class: 'scan-frame' },
      video,
      dom.el('p', { class: 'scan-hint' }, 'Point the camera at the recipient\'s address QR code.'),
      cancelBtn
    )
  );

  return { overlay, video, cancelBtn };
}

/**
 * Run one scan attempt and classify the outcome.
 *
 * Three outcomes, deliberately distinct:
 *   { ok: true, address }       - a validated address for this network
 *   { ok: false, cancelled }    - the user pressed Cancel (AbortError). NOT
 *                                 an error: it must not surface as a banner.
 *   { ok: false, error }        - anything else, including a scanned string
 *                                 that is not a valid address for this
 *                                 network.
 *
 * The address is validated by qrscan.parseScannedAddress (inside
 * scanAddressFromCamera) against `network` before it is ever returned, so a
 * camera pointed at an arbitrary QR code cannot put arbitrary text into the
 * form. Camera teardown on every path is qrscan.js's job (its `finally`);
 * nothing here may swallow a rejection in a way that skips it.
 *
 * Exported for tests.
 */
export async function runAddressScan({
  scanEnv,
  network,
  video,
  signal,
  scanImpl = qrscan.scanAddressFromCamera
} = {}) {
  try {
    const address = await scanImpl({
      video,
      getUserMediaImpl: scanEnv.getUserMediaImpl,
      detectorImpl: scanEnv.detectorImpl,
      network,
      signal
    });
    return { ok: true, address };
  } catch (e) {
    if (e && e.name === 'AbortError') {
      return { ok: false, cancelled: true };
    }
    return { ok: false, error: (e && e.message) || String(e) };
  }
}

/**
 * Build the Send screen's card.
 *
 * Pure: it reads `state` and calls the handlers it is given, and touches no
 * module state, no network and no camera. Exported so the screen's wiring
 * (especially "no Scan button where scanning cannot work") is testable
 * without a browser.
 */
export function buildSendCard({
  state,
  network = 'mainnet',
  scanSupported = false,
  onReview = () => {},
  onBack = () => {},
  onScan = () => {},
  onSelectPosition = () => {}
} = {}) {
  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Send Tokens')
  );

  if (state.loadError) {
    card.appendChild(dom.el('p', { class: 'error' }, '❌ Failed to load your tokens: ' + state.loadError));
  }

  if (state.planErrors && state.planErrors.length > 0) {
    const errorsList = state.planErrors.map((error) =>
      dom.el('div', { class: 'error-field' },
        dom.el('strong', {}, error.field + ':'),
        ' ' + error.message
      )
    );
    card.appendChild(dom.el('div', { class: 'error-list' }, ...errorsList));
  }

  // ---- position picker ----
  const positionsSection = dom.el('div', { class: 'section' },
    dom.el('h3', {}, 'Token to send')
  );

  if (state.positions === null) {
    positionsSection.appendChild(dom.skeleton({ lines: 3 }));
  } else if (state.positions.length === 0) {
    positionsSection.appendChild(dom.el('p', { class: 'placeholder' },
      'You hold no token positions to send.'
    ));
  } else {
    const list = dom.el('div', { class: 'positions-list' });
    for (const position of state.positions) {
      const key = positionKey(position);
      const selected = key === state.selectedKey;
      const row = dom.el('div', { class: selected ? 'position-row position-row--selected' : 'position-row' },
        dom.el('span', { class: 'txid' }, position.txid.slice(0, 16) + '...'),
        dom.el('span', { class: 'amount' },
          (position.value / uap.COIN).toFixed(8) + ' coins × ' + position.multiplier
        ),
        dom.el('button',
          {
            class: selected ? 'btn btn-sm btn-primary' : 'btn btn-sm',
            'aria-pressed': selected ? 'true' : 'false',
            onClick: () => onSelectPosition(position)
          },
          selected ? 'Selected' : 'Select'
        )
      );
      list.appendChild(row);
    }
    positionsSection.appendChild(list);
  }
  card.appendChild(positionsSection);

  // ---- amount ----
  card.appendChild(dom.el('div', { class: 'form-group' },
    dom.el('label', { for: 'send-amount' }, 'Amount to send (coins):'),
    dom.el('input', {
      id: 'send-amount',
      type: 'number',
      step: 'any',
      placeholder: '0.00000000',
      value: state.amountCoins
    })
  ));

  // ---- recipient pubkey ----
  card.appendChild(dom.el('div', { class: 'form-group' },
    dom.el('label', { for: 'send-pubkey' }, 'Recipient public key (hex):'),
    dom.el('input', {
      id: 'send-pubkey',
      type: 'text',
      placeholder: '02...',
      value: state.toPubKey
    }),
    dom.el('p', { class: 'hint' },
      'A token covenant is addressed to a public key, not to an address, so the recipient must give you theirs.'
    )
  ));

  // ---- recipient address + scan ----
  const addressInput = dom.el('input', {
    id: 'send-address',
    type: 'text',
    placeholder: 'Recipient address (optional check)',
    value: state.toAddress
  });

  const addressRow = dom.el('div', { class: 'input-with-action' }, addressInput);

  // No BarcodeDetector/getUserMedia (Safari/iOS, Firefox) means no button at
  // all. A "Scan" button that can only ever fail is worse than no button,
  // and typing into the field above stays fully functional either way.
  if (scanSupported) {
    addressRow.appendChild(dom.el('button',
      { class: 'btn btn-secondary btn-scan', type: 'button', onClick: onScan },
      state.scanning ? 'Scanning…' : 'Scan'
    ));
  }

  const addressGroup = dom.el('div', { class: 'form-group' },
    dom.el('label', { for: 'send-address' }, 'Recipient address (optional):'),
    addressRow,
    dom.el('p', { class: 'hint' },
      `If given, it is checked against the public key above (${network} addresses only). A mismatch blocks the send.`
    )
  );

  // Cancelling a scan is a normal thing to do, so it gets neutral wording
  // and a neutral class -- never the error style.
  if (state.scanNotice) {
    addressGroup.appendChild(dom.el('p', { class: 'hint scan-notice', role: 'status' }, state.scanNotice));
  }
  if (state.scanError) {
    addressGroup.appendChild(dom.el('p', { class: 'error scan-error', role: 'alert' }, state.scanError));
  }

  card.appendChild(addressGroup);

  card.appendChild(dom.el('div', { class: 'button-group' },
    dom.el('button', { class: 'btn btn-primary', onClick: onReview }, 'Review'),
    dom.el('button', { class: 'btn btn-secondary', onClick: onBack }, 'Back')
  ));

  return card;
}

function goToSend() {
  sendState = freshSendState();
  currentScreen = 'send';
  render();
  loadSendData();
}

async function loadSendData() {
  try {
    const pubKeyHex = uap.bytesToHex(secp256k1.getPublicKey(wallet.privKey, true));
    const positions = await apiClient.getPositions(pubKeyHex, { unspentOnly: true });
    const utxos = await apiClient.getUtxos(wallet.address);
    const feeRateResp = await apiClient.getFeeRate();

    sendState.positions = positions;
    sendState.fundingUtxos = utxos;
    sendState.feeRate = feeRateResp.sat_per_kb;
    sendState.loadError = null;
    if (!sendState.selectedKey && positions.length > 0) {
      sendState.selectedKey = positionKey(positions[0]);
    }
  } catch (e) {
    // An empty list, not null: otherwise the screen would sit on a skeleton
    // forever just because the fetch failed.
    sendState.positions = [];
    sendState.loadError = e.message;
  }
  if (currentScreen === 'send') render();
}

function selectedPosition() {
  if (!sendState.positions) return null;
  return sendState.positions.find((p) => positionKey(p) === sendState.selectedKey) || null;
}

async function renderSend() {
  const card = buildSendCard({
    state: sendState,
    network: wallet ? wallet.network : 'mainnet',
    scanSupported: qrscan.isScanSupported(scanEnvironment()),
    onReview: planSendTx,
    onBack: goTo('wallet'),
    onScan: startAddressScan,
    onSelectPosition: (position) => {
      sendState.selectedKey = positionKey(position);
      captureSendInputs();
      render();
    }
  });

  app.appendChild(dom.el('div', { class: 'screen screen-send' }, card));
}

// Read the form back into state so a re-render (selecting a position,
// finishing a scan) doesn't throw away what has been typed.
function captureSendInputs() {
  const amountEl = document.getElementById('send-amount');
  const pubKeyEl = document.getElementById('send-pubkey');
  const addressEl = document.getElementById('send-address');
  if (amountEl) sendState.amountCoins = amountEl.value;
  if (pubKeyEl) sendState.toPubKey = pubKeyEl.value;
  if (addressEl) sendState.toAddress = addressEl.value;
}

async function startAddressScan() {
  // Keep whatever is typed; a scan should not wipe the form.
  captureSendInputs();

  const scanEnv = scanEnvironment();
  if (!qrscan.isScanSupported(scanEnv)) {
    // Defensive: the button is not rendered in this case.
    sendState.scanError = 'QR scanning is not available in this browser. Type or paste the address instead.';
    render();
    return;
  }

  const controller = new AbortController();
  const { overlay, video } = buildScanOverlay({ onCancel: () => controller.abort() });

  sendState.scanning = true;
  sendState.scanError = null;
  sendState.scanNotice = null;
  render();
  app.appendChild(overlay);

  const result = await runAddressScan({
    scanEnv,
    network: wallet ? wallet.network : 'mainnet',
    video,
    signal: controller.signal
  });

  sendState.scanning = false;
  if (result.ok) {
    sendState.toAddress = result.address;
    sendState.scanNotice = 'Address scanned.';
  } else if (result.cancelled) {
    // Cancelling is not a failure and must not be reported as one.
    sendState.scanNotice = 'Scan cancelled.';
  } else {
    sendState.scanError = result.error;
  }
  render();
}

async function planSendTx() {
  captureSendInputs();

  const position = selectedPosition();
  const coins = parseFloat(sendState.amountCoins);
  const amountSats = Number.isFinite(coins) ? Math.round(coins * uap.COIN) : NaN;

  const planResult = planTransfer({
    position,
    ownPubKey: secp256k1.getPublicKey(wallet.privKey, true),
    toPubKey: sendState.toPubKey,
    toAddress: sendState.toAddress,
    network: wallet.network,
    amountSats,
    fundingUtxos: sendState.fundingUtxos,
    feeRate: sendState.feeRate
  });

  if (!planResult.ok) {
    sendState.plan = null;
    sendState.planErrors = planResult.errors;
    render();
    return;
  }

  sendState.plan = planResult.plan;
  sendState.planErrors = null;
  currentScreen = 'send-review';
  render();
}

/**
 * The review step. A wallet must never broadcast straight off the form:
 * every number that is about to be committed to gets shown first, and the
 * only way past this screen is an explicit confirmation.
 */
export function buildSendReviewCard({ plan, onConfirm = () => {}, onEdit = () => {} } = {}) {
  const coins = (sats) => (sats / uap.COIN).toFixed(8) + ' coins';
  const recipient = plan.outputs.find((o) => o.kind === OUTPUT_TOKEN_RECIPIENT);
  const tokenChange = plan.outputs.find((o) => o.kind === OUTPUT_TOKEN_CHANGE);
  const whipChange = plan.outputs.find((o) => o.kind === OUTPUT_WHIP_CHANGE);

  // Summed from the OUTPUT LIST -- the same list buildTransferTx encodes --
  // rather than read from plan.tokenValueOut. A figure the plan computed for
  // itself can agree with the plan while disagreeing with the transaction;
  // this one cannot. It is both what the screen displays and what the
  // conservation guard below is tested against.
  const tokenSatsOut = plan.outputs
    .filter((o) => o.kind !== OUTPUT_WHIP_CHANGE)
    .reduce((sum, o) => sum + o.value, 0);

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Review Transfer'),
    dom.el('div', { class: 'review-section' },
      dom.el('h3', {}, 'Recipient'),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Public key:'),
        dom.el('code', {}, uap.bytesToHex(plan.toPubKey))
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Address checked:'),
        dom.el('code', {}, plan.toAddressChecked || '(not provided)')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Sending:'),
        dom.el('code', {}, coins(recipient.value) + ' × ' + plan.multiplier)
      )
    ),
    dom.el('div', { class: 'review-section' },
      dom.el('h3', {}, 'Transaction'),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Kept in your position:'),
        dom.el('code', {}, tokenChange ? coins(tokenChange.value) : '(none — sending the whole position)')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Fee (paid in WHIP, not tokens):'),
        dom.el('code', {}, coins(plan.fee))
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'WHIP change:'),
        dom.el('code', {}, whipChange ? coins(whipChange.value) : '(none)')
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Token value in / out:'),
        dom.el('code', {}, coins(plan.tokenValueIn) + ' / ' + coins(tokenSatsOut))
      )
    )
  );

  // THE conservation check, in satoshis, against the outputs that will be
  // encoded. Consensus counts balance as value x multiplier in raw satoshis
  // with no division and no truncation, so conserving satoshis conserves
  // balance -- fractional-coin amounts included. Satoshis are integers, so
  // this comparison is exact and cannot false-alarm on rounding.
  //
  // It deliberately does NOT look at plan.virtualBalanceLost, which is
  // `V - (a + (V - a))` scaled by 1/COIN: structurally zero for every plan
  // planTransfer can emit, and so a guard that can never fire. What CAN break
  // is this: outputs whose values do not add back up to the position's, which
  // is what a satoshi figure too large for a JavaScript number produces
  // (planTransfer refuses those outright; this is the second line).
  if (tokenSatsOut !== plan.tokenValueIn) {
    const delta = plan.tokenValueIn - tokenSatsOut;
    card.appendChild(dom.el('p', { class: 'warning warning-bug' },
      delta > 0
        ? `⚠️ This transfer's outputs carry ${delta} satoshis less than the position holds, ` +
          'so that much token balance would be destroyed. ' +
          'That should be impossible; it indicates a bug. Do not broadcast it — report it instead.'
        : `⚠️ This transfer's outputs carry ${-delta} satoshis more than the position holds, ` +
          'so the network will reject it. ' +
          'That should be impossible; it indicates a bug. Do not broadcast it — report it instead.'
    ));
  }

  // Things that are true of a perfectly valid transfer but that the user
  // should still be told -- chiefly that the position's value is whatever the
  // indexer said it was and cannot be checked here (see planTransfer). A
  // warning that exists only in the plan object protects nobody, so every one
  // of them is rendered, not just the ones this screen happens to know about.
  for (const warning of plan.warnings || []) {
    card.appendChild(dom.el('p', { class: 'warning warning-advisory' }, warning.message));
  }

  card.appendChild(dom.el('div', { class: 'button-group' },
    dom.el('button', { class: 'btn btn-primary', onClick: onConfirm }, 'Confirm & Broadcast'),
    dom.el('button', { class: 'btn btn-secondary', onClick: onEdit }, 'Edit')
  ));

  return card;
}

async function renderSendReview() {
  if (!sendState.plan) {
    currentScreen = 'send';
    render();
    return;
  }

  const card = buildSendReviewCard({
    plan: { ...sendState.plan, toAddressChecked: sendState.toAddress },
    onConfirm: confirmSend,
    onEdit: goTo('send')
  });

  app.appendChild(dom.el('div', { class: 'screen screen-send-review' }, card));
}

async function confirmSend() {
  if (!sendState.plan) {
    alert('No transfer plan available. Please review again.');
    return;
  }

  try {
    uap.configureSecp(secp256k1);

    const buildResult = await buildTransferTx({
      secp: secp256k1,
      plan: sendState.plan,
      privKey: wallet.privKey
    });

    // Resolves only on a 2xx: the node accepted it into its mempool. That
    // is not confirmation -- see the same reasoning in confirmFill.
    const txid = await apiClient.broadcast(buildResult.rawHex);

    broadcastStatus = { txid, state: 'pending' };
    sendState = freshSendState();
    currentScreen = 'wallet';
    render();

    pollForConfirmation({ apiClient, address: wallet.address, txid }).then((result) => {
      if (!broadcastStatus || broadcastStatus.txid !== txid) return;
      broadcastStatus = {
        txid,
        state: result.status === 'confirmed' ? 'confirmed' : 'timeout'
      };
      if (currentScreen === 'wallet') render();
    });
  } catch (e) {
    alert(`Error sending tokens: ${e.message}`);
  }
}

// Market flow handlers
function goToMarket() {
  marketState = {
    orders: [],
    selectedOrder: null,
    fillPlan: null,
    fillPlanErrors: null
  };
  currentScreen = 'market';
  render();
}

async function renderMarket() {
  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Order Book'),
    dom.skeleton({ lines: 4 })
  );

  const screenDiv = dom.el('div', { class: 'screen screen-market' }, card);
  app.appendChild(screenDiv);

  // Fetch orders from API
  try {
    const orders = await apiClient.listOrders();
    dom.clear(card);

    card.appendChild(dom.el('h2', {}, 'Order Book'));

    // Same reasoning as the balance: a cached order book is worse than a
    // cached balance, because acting on it means signing a fill against an
    // order that may already be gone. Label it before the user reads prices
    // off it.
    const orderFreshness = staleInfo(orders);
    if (orderFreshness.stale) {
      card.appendChild(dom.staleNotice(orderFreshness.cachedAt));
    }

    if (!orders || orders.length === 0) {
      card.appendChild(dom.el('p', { class: 'placeholder' }, 'No orders available'));
    } else {
      const ordersList = dom.el('div', { class: 'orders-list' });

      for (const order of orders) {
        const desc = describeOrder(order);
        const orderRow = dom.el('div', { class: 'order-row' },
          dom.el('div', { class: 'order-desc' }, desc),
          dom.el('button',
            { class: 'btn btn-sm', onClick: () => selectOrderToFill(order) },
            'Fill'
          )
        );
        ordersList.appendChild(orderRow);
      }

      card.appendChild(ordersList);
    }

    // Back button
    card.appendChild(dom.el('div', { class: 'button-group' }, backButton('wallet')));
  } catch (e) {
    dom.clear(card);
    card.appendChild(dom.el('h2', {}, 'Order Book'));
    card.appendChild(dom.el('p', { class: 'error' },
      '❌ Failed to load orders: ' + e.message
    ));
    card.appendChild(dom.el('div', { class: 'button-group' }, backButton('wallet')));
  }
}

async function selectOrderToFill(order) {
  marketState.selectedOrder = order;
  currentScreen = 'market-fill';
  render();
}

async function renderMarketFill() {
  const order = marketState.selectedOrder;
  if (!order) {
    currentScreen = 'market';
    render();
    return;
  }

  const desc = describeOrder(order);

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Fill Order'),
    dom.el('div', { class: 'review-section' },
      dom.el('h3', {}, 'Order Details'),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Description:'),
        dom.el('code', {}, desc)
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Multiplier:'),
        dom.el('code', {}, order.multiplier.toString())
      ),
      dom.el('div', { class: 'review-row' },
        dom.el('span', {}, 'Payment Required:'),
        dom.el('code', {}, (order.payment_value / uap.COIN).toFixed(8) + ' coins')
      )
    )
  );

  // A skeleton for the part of the screen that depends on the fetches
  // below (position, taker UTXOs, fee rate) -- the order details above are
  // already known synchronously from `order` and don't need one.
  const planSkeleton = dom.el('div', { class: 'section' },
    dom.el('h3', {}, 'Transaction'),
    dom.skeleton({ lines: 3 })
  );
  card.appendChild(planSkeleton);

  const screenDiv = dom.el('div', { class: 'screen screen-market-fill' }, card);
  app.appendChild(screenDiv);

  // Fetch position and plan the fill
  try {
    // Get the position to know the token value
    const position = await apiClient.getPosition(order.txid, order.vout);

    // Get taker's UTXOs
    const takerUtxos = await apiClient.getUtxos(wallet.address);

    // Get fee rate
    const feeRateResp = await apiClient.getFeeRate();
    const feeRate = feeRateResp.sat_per_kb;

    // Plan the fill
    const taker_pubkey = secp256k1.getPublicKey(wallet.privKey, true);
    const planResult = planFill({
      order,
      position,
      takerUtxos,
      takerPubkey: taker_pubkey,
      feeRate
    });

    if (!planResult.ok) {
      dom.clear(card);
      card.appendChild(dom.el('h2', {}, 'Cannot Fill Order'));
      const errorsList = [];
      for (const error of planResult.errors) {
        errorsList.push(dom.el('div', { class: 'error-field' }, error));
      }
      card.appendChild(dom.el('div', { class: 'error-list' }, ...errorsList));

      card.appendChild(dom.el('div', { class: 'button-group' }, backButton('market')));
    } else {
      marketState.fillPlan = planResult.plan;

      // Show fill review
      dom.clear(card);
      card.appendChild(dom.el('h2', {}, 'Review Fill Transaction'));
      card.appendChild(dom.el('div', { class: 'review-section' },
        dom.el('h3', {}, 'Order'),
        dom.el('div', { class: 'review-row' },
          dom.el('span', {}, 'Description:'),
          dom.el('code', {}, desc)
        )
      ));
      card.appendChild(dom.el('div', { class: 'review-section' },
        dom.el('h3', {}, 'Transaction'),
        dom.el('div', { class: 'review-row' },
          dom.el('span', {}, 'Your Payment:'),
          dom.el('code', {}, (order.payment_value / uap.COIN).toFixed(8) + ' coins')
        ),
        dom.el('div', { class: 'review-row' },
          dom.el('span', {}, 'Token Received:'),
          dom.el('code', {}, (position.value / uap.COIN).toFixed(8) + ' coins')
        ),
        dom.el('div', { class: 'review-row' },
          dom.el('span', {}, 'Estimated Fee:'),
          dom.el('code', {}, (planResult.plan.fee / uap.COIN).toFixed(8) + ' coins')
        ),
        dom.el('div', { class: 'review-row' },
          dom.el('span', {}, 'Change:'),
          dom.el('code', {},
            planResult.plan.change > 0
              ? (planResult.plan.change / uap.COIN).toFixed(8) + ' coins'
              : '(none)'
          )
        )
      ));

      const confirmBtn = dom.el('button',
        { class: 'btn btn-primary', onClick: () => confirmFill(order, position, takerUtxos, feeRate) },
        'Confirm & Broadcast'
      );
      card.appendChild(dom.el('div', { class: 'button-group' }, confirmBtn, backButton('market')));
    }
  } catch (e) {
    dom.clear(card);
    card.appendChild(dom.el('h2', {}, 'Error'));
    card.appendChild(dom.el('p', { class: 'error' },
      'Failed to prepare fill: ' + e.message
    ));
    card.appendChild(dom.el('div', { class: 'button-group' }, backButton('market')));
  }
}

async function confirmFill(order, position, takerUtxos, feeRate) {
  if (!marketState.fillPlan) {
    alert('No fill plan available.');
    return;
  }

  try {
    // Configure secp
    uap.configureSecp(secp256k1);

    // Build the fill transaction
    const pubKey = secp256k1.getPublicKey(wallet.privKey, true);
    const buildResult = await buildFillTx({
      secp: secp256k1,
      plan: marketState.fillPlan,
      privKey: wallet.privKey,
      pubKey,
      order,
      position,
      takerUtxos
    });

    // Broadcast the transaction. This only resolves on a 2xx -- the node
    // has accepted the transaction into its mempool -- it does not mean
    // the transaction is confirmed.
    const txid = await apiClient.broadcast(buildResult.rawHex);

    // Optimistic UI: show the pending state immediately rather than
    // blocking the user behind another round trip. This is safe to do
    // *because* a block is only ~6s away -- the wait to actually confirm is
    // short, and the badge is worded so it can never be mistaken for a
    // confirmation (see dom.js txStatus). It is not a claim that the
    // transfer succeeded, only that the node accepted it.
    broadcastStatus = { txid, state: 'pending' };
    currentScreen = 'wallet';
    render();

    // Resolve the pending state in the background. If confirmation never
    // comes, pollForConfirmation still resolves -- to 'timeout' -- after
    // its poll budget (~90s by default), so this can never leave the badge
    // reading "pending" forever; the user just falls back to checking
    // again later, same as any other wallet that lost track of a tx.
    pollForConfirmation({ apiClient, address: wallet.address, txid }).then((result) => {
      // A newer broadcast may have replaced broadcastStatus by the time
      // this resolves; don't let a stale poll clobber it.
      if (!broadcastStatus || broadcastStatus.txid !== txid) return;
      broadcastStatus = {
        txid,
        state: result.status === 'confirmed' ? 'confirmed' : 'timeout'
      };
      if (currentScreen === 'wallet') render();
    });
  } catch (e) {
    alert(`Error filling order: ${e.message}`);
  }
}

// Initial render
render();

// =============================================================================
// SELL: the maker half of the marketplace
// =============================================================================

function startSell() {
  sellState = freshSellState();
  currentScreen = 'sell';
  render();
  loadSellPositions();
}

async function loadSellPositions() {
  try {
    const pubKeyHex = uap.bytesToHex(secp256k1.getPublicKey(wallet.privKey, true));
    const positions = await apiClient.getPositions(pubKeyHex, { unspentOnly: true });
    sellState.positions = positions;
    sellState.loadError = null;
    if (!sellState.selectedKey && positions.length > 0) {
      sellState.selectedKey = positionKey(positions[0]);
    }
  } catch (e) {
    // An empty list, not null: otherwise the screen would sit on a skeleton
    // forever just because the fetch failed (same reasoning as loadSendData).
    sellState.positions = [];
    sellState.loadError = e.message;
  }
  if (currentScreen === 'sell') render();
}

function selectedSellPosition() {
  if (!sellState.positions) return null;
  return sellState.positions.find((p) => positionKey(p) === sellState.selectedKey) || null;
}

/**
 * Build the Sell screen's card.
 *
 * Pure, like buildSendCard: reads `state` and calls the handlers it is
 * given, touches no module state. The "Selected …" hint and a validation
 * error are two different elements with two different classes (`hint` vs
 * `error`) -- previously a single element played both parts, flipping its
 * className at runtime, which meant a stale hint and a fresh error could
 * never be told apart by anything reading the DOM. Position selection is
 * also given real visual feedback here, the same convention buildSendCard
 * already uses (`position-row--selected`, aria-pressed, Select/Selected).
 *
 * Exported for tests.
 */
export function buildSellCard({
  state,
  onReview = () => {},
  onBack = () => {},
  onSelectPosition = () => {}
} = {}) {
  const card = dom.el('div', { class: 'card' }, dom.el('h2', {}, 'Sell a position'));

  if (state.loadError) {
    card.appendChild(dom.el('p', { class: 'error' }, '❌ Could not load your positions: ' + state.loadError));
  }

  if (state.positions === null) {
    card.appendChild(dom.skeleton({ lines: 3 }));
    card.appendChild(dom.el('div', { class: 'button-group' },
      dom.el('button', { class: 'btn btn-secondary', onClick: onBack }, 'Back')));
    return card;
  }

  if (state.positions.length === 0) {
    card.appendChild(dom.el('p', { class: 'placeholder' }, 'You have no positions to sell.'));
    card.appendChild(dom.el('div', { class: 'button-group' },
      dom.el('button', { class: 'btn btn-secondary', onClick: onBack }, 'Back')));
    return card;
  }

  const list = dom.el('div', { class: 'positions-list' });
  let selectedPosition = null;
  for (const p of state.positions) {
    const key = positionKey(p);
    const selected = key === state.selectedKey;
    if (selected) selectedPosition = p;
    const row = dom.el('div', { class: selected ? 'position-row position-row--selected' : 'position-row' },
      dom.el('div', { class: 'position-desc' },
        `x${p.multiplier} — ${p.value} sat — ${p.txid.slice(0, 12)}…:${p.vout}`),
      dom.el('button', {
        class: selected ? 'btn btn-sm btn-primary' : 'btn btn-sm',
        'aria-pressed': selected ? 'true' : 'false',
        onClick: () => onSelectPosition(p),
      }, selected ? 'Selected' : 'Select')
    );
    list.appendChild(row);
  }
  card.appendChild(list);

  if (selectedPosition) {
    card.appendChild(dom.el('p', { class: 'hint' },
      'Selected ' + selectedPosition.txid.slice(0, 12) + '…:' + selectedPosition.vout));
  }

  card.appendChild(dom.el('div', { class: 'form-group' },
    dom.el('label', { for: 'sell-price' }, 'Asking price (satoshis)'),
    dom.el('input', {
      id: 'sell-price', type: 'number', min: '1', step: '1', class: 'form-control',
      placeholder: 'Asking price in satoshis', value: state.priceSats,
    })));

  if (state.error) {
    card.appendChild(dom.el('p', { class: 'error' }, state.error));
  }

  card.appendChild(dom.el('div', { class: 'button-group' },
    dom.el('button', { class: 'btn btn-primary', onClick: onReview }, 'Review'),
    dom.el('button', { class: 'btn btn-secondary', onClick: onBack }, 'Back')));

  return card;
}

async function renderSell() {
  const card = buildSellCard({
    state: sellState,
    onReview: planSellFlow,
    onBack: goTo('wallet'),
    onSelectPosition: (p) => {
      sellState.selectedKey = positionKey(p);
      captureSellPriceInput();
      sellState.error = null;
      render();
    }
  });
  app.appendChild(dom.el('div', { class: 'screen screen-sell' }, card));
}

// Read the price field back into state so a re-render (selecting a
// position) doesn't throw away what has been typed -- same reasoning as
// captureSendInputs.
function captureSellPriceInput() {
  const priceEl = document.getElementById('sell-price');
  if (priceEl) sellState.priceSats = priceEl.value;
}

function planSellFlow() {
  captureSellPriceInput();

  const priceSats = Number(sellState.priceSats);
  const plan = planSell({
    position: selectedSellPosition(),
    priceSats: Number.isInteger(priceSats) ? priceSats : sellState.priceSats === '' ? NaN : priceSats,
    ownAddress: wallet.address,
  });

  if (!plan.ok) {
    sellState.error = plan.errors.join(' ');
    render();
    return;
  }

  sellState.error = null;
  sellState.plan = plan;
  currentScreen = 'sell-review';
  render();
}

/**
 * The sell review step, as a pure function of the state it displays --
 * same convention as buildMintReviewCard and buildSendReviewCard: the
 * figures here are the last thing a maker sees before signing an offer
 * anyone can take, so the builder is exported and dependency-injected
 * rather than buried in a render function no test can reach.
 *
 * `error`, if given, is a failed publish (see confirmSell) rendered inline
 * -- this used to be the one confirm/edit screen in the app that reported
 * that failure via alert() instead.
 */
export function buildSellReviewCard({ plan, error = null, onConfirm = () => {}, onEdit = () => {} } = {}) {
  const s = plan.summary;

  return dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Review this sale'),
    dom.el('dl', { class: 'review-list' },
      dom.el('dt', {}, 'Position'), dom.el('dd', {}, `${s.txid.slice(0, 16)}…:${s.vout}`),
      dom.el('dt', {}, 'Multiplier'), dom.el('dd', {}, 'x' + s.multiplier),
      dom.el('dt', {}, 'Token value'), dom.el('dd', {}, s.tokenValue + ' sat'),
      dom.el('dt', {}, 'Asking price'), dom.el('dd', {}, s.priceSats + ' sat'),
      dom.el('dt', {}, 'Paid to'), dom.el('dd', {}, s.payTo),
    ),
    // The maker is not choosing a counterparty, and should not think they
    // are. Say exactly what the signature does and does not commit to.
    dom.el('p', { class: 'warning' },
      'Publishing this signs an offer that ANYONE may take: whoever pays ' +
      `${s.priceSats} satoshis to your address gets this position. You are not ` +
      'choosing who buys it. The offer stands until you cancel it or the ' +
      'position is spent.'),
    error ? dom.el('p', { class: 'error' }, error) : null,
    dom.el('div', { class: 'button-group' },
      dom.el('button', { class: 'btn btn-primary', onClick: onConfirm }, 'Publish offer'),
      dom.el('button', { class: 'btn btn-secondary', onClick: onEdit }, 'Edit'))
  );
}

async function renderSellReview() {
  if (!sellState.plan) {
    currentScreen = 'sell';
    render();
    return;
  }

  const card = buildSellReviewCard({
    plan: sellState.plan,
    error: sellState.error,
    onConfirm: confirmSell,
    onEdit: goTo('sell')
  });
  app.appendChild(dom.el('div', { class: 'screen screen-sell-review' }, card));
}

/**
 * Sign and publish a sell order. Exported and pure with respect to module
 * state so the FAILURE path is testable: the screen-level confirmSell below
 * only decides what to do with the result.
 *
 * Returns {ok: true} or {ok: false, error} rather than throwing, because
 * every caller has to render the message either way.
 */
export async function publishSellOrder({ api, secp, plan, privKey, address }) {
  try {
    uap.configureSecp(secp);
    const s = plan.summary;
    const order = buildSellOrder({
      secp,
      position: { txid: s.txid, vout: s.vout, multiplier: s.multiplier },
      privKey,
      pubKey: secp.getPublicKey(privKey, true),
      priceSats: s.priceSats,
      ownAddress: address,
    });
    await api.publishOrder(order);
    return { ok: true };
  } catch (e) {
    return { ok: false, error: e.message };
  }
}

async function confirmSell() {
  const result = await publishSellOrder({
    api: apiClient,
    secp: secp256k1,
    plan: sellState.plan,
    privKey: wallet.privKey,
    address: wallet.address,
  });
  if (result.ok) {
    sellState.error = null;
    currentScreen = 'market';
  } else {
    // Inline, like every other flow's failure -- this used to be an
    // alert(), the one place in the app that broke that convention.
    sellState.error = result.error;
  }
  render();
}

// =============================================================================
// MY ORDERS: viewing and cancelling this wallet's own standing sell orders
// (Phase 6 of doc/uap-marketplace-website-plan.md)
// =============================================================================

function goToMyOrders() {
  myOrdersState = freshMyOrdersState();
  currentScreen = 'my-orders';
  render();
}

async function renderMyOrders() {
  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'My Orders'),
    dom.skeleton({ lines: 3 })
  );
  app.appendChild(dom.el('div', { class: 'screen screen-my-orders' }, card));

  const back = () => backButton('wallet');

  let mine;
  try {
    const ownPubKeyHex = uap.bytesToHex(secp256k1.getPublicKey(wallet.privKey, true));
    const orders = await apiClient.listOrders();
    mine = myOrders(orders, ownPubKeyHex);
    myOrdersState.orders = mine;
    myOrdersState.loadError = null;
  } catch (e) {
    myOrdersState.loadError = e.message;
    dom.clear(card);
    card.appendChild(dom.el('h2', {}, 'My Orders'));
    card.appendChild(dom.el('p', { class: 'error' }, '❌ Could not load your orders: ' + e.message));
    card.appendChild(dom.el('div', { class: 'button-group' }, back()));
    return;
  }

  dom.clear(card);
  card.appendChild(dom.el('h2', {}, 'My Orders'));

  if (mine.length === 0) {
    card.appendChild(dom.el('p', { class: 'placeholder' }, 'You have no open orders.'));
    card.appendChild(dom.el('div', { class: 'button-group' }, back()));
    return;
  }

  const list = dom.el('div', { class: 'orders-list' });
  for (const order of mine) {
    const desc = describeOrder(order);
    const row = dom.el('div', { class: 'order-row' },
      dom.el('div', { class: 'order-desc' }, desc),
      dom.el('div', { class: 'order-desc' },
        (order.payment_value / uap.COIN).toFixed(8) + ' coins asked'),
      dom.el('button',
        { class: 'btn btn-sm btn-danger', onClick: () => selectOrderToCancel(order) },
        'Cancel')
    );
    list.appendChild(row);
  }
  card.appendChild(list);
  card.appendChild(dom.el('div', { class: 'button-group' }, back()));
}

function selectOrderToCancel(order) {
  myOrdersState.selectedOrder = order;
  myOrdersState.cancelError = null;
  myOrdersState.cancelling = false;
  currentScreen = 'my-orders-cancel';
  render();
}

async function renderMyOrdersCancel() {
  const order = myOrdersState.selectedOrder;
  if (!order) {
    currentScreen = 'my-orders';
    render();
    return;
  }

  const desc = describeOrder(order);
  const plan = planCancel(order);

  const errorEl = dom.el('p', { class: 'error', style: myOrdersState.cancelError ? '' : 'display:none' },
    myOrdersState.cancelError || '');

  const backBtn = backButton('my-orders');

  if (!plan.ok) {
    // Malformed order data (shouldn't happen for anything the relay itself
    // returned) -- refuse before ever making the request, same discipline
    // as planSell.
    const card = dom.el('div', { class: 'card' },
      dom.el('h2', {}, 'Cannot cancel this order'),
      dom.el('div', { class: 'error-list' },
        ...plan.errors.map((e) => dom.el('div', { class: 'error-field' }, e))),
      dom.el('div', { class: 'button-group' }, backBtn)
    );
    app.appendChild(dom.el('div', { class: 'screen screen-my-orders-cancel' }, card));
    return;
  }

  const confirmBtn = dom.el('button', {
    class: 'btn btn-danger',
    onClick: confirmCancel,
    disabled: myOrdersState.cancelling ? 'disabled' : undefined,
  }, myOrdersState.cancelling ? 'Cancelling…' : 'Confirm Cancel');

  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'Cancel this order?'),
    dom.el('dl', { class: 'review-list' },
      dom.el('dt', {}, 'Position'), dom.el('dd', {}, `${order.txid.slice(0, 16)}…:${order.vout}`),
      dom.el('dt', {}, 'Multiplier'), dom.el('dd', {}, 'x' + order.multiplier),
      dom.el('dt', {}, 'Asking price'), dom.el('dd', {}, `${order.payment_value} sat`),
      dom.el('dt', {}, 'Description'), dom.el('dd', {}, desc),
    ),
    // Withdrawing here only stops *this* relay from continuing to offer it
    // (see the tombstone note in CancelOrder, contrib/uap-indexer/orders.go).
    // A peer that already mirrored the order can keep serving it, and the
    // only fully reliable way to kill an unwanted offer is to spend the
    // position elsewhere.
    dom.el('p', { class: 'warning' },
      'This withdraws the order from this relay. Peers that already copied ' +
      'it may keep offering it until it is spent or expires on their side.'),
    errorEl,
    dom.el('div', { class: 'button-group' }, confirmBtn, backBtn)
  );
  app.appendChild(dom.el('div', { class: 'screen screen-my-orders-cancel' }, card));
}

async function confirmCancel() {
  const order = myOrdersState.selectedOrder;
  const plan = planCancel(order);
  if (!plan.ok) return;

  myOrdersState.cancelling = true;
  myOrdersState.cancelError = null;
  render();

  try {
    await apiClient.cancelOrder(order.txid, order.vout, order.script_sig);
    currentScreen = 'my-orders';
    myOrdersState.selectedOrder = null;
    myOrdersState.cancelling = false;
    render();
  } catch (e) {
    myOrdersState.cancelling = false;
    myOrdersState.cancelError = describeCancelFailure(e.message);
    render();
  }
}

// =============================================================================
// MIGRATE: wallets created before the BIP39 derivation fix
// =============================================================================

async function renderMigrate() {
  const card = dom.el('div', { class: 'card' },
    dom.el('h2', {}, 'This wallet uses the old key derivation'),
    dom.el('p', {},
      'This wallet was created before a bug was fixed in how its private key ' +
      'was derived from your recovery words. Your funds are safe and this ' +
      'wallet keeps working exactly as before.'),
    dom.el('p', {},
      'What does not work is the promise those words made. They were labelled ' +
      'a BIP39 mnemonic, but the key was not derived the way BIP39 and BIP32 ' +
      'specify, so typing them into another wallet would show you an empty ' +
      `account rather than your coins. New wallets now derive at ${WHIPPET_KEY_PATH} ` +
      'and can be recovered anywhere.'),
    dom.el('p', { class: 'warning' },
      'To get that guarantee you need a new wallet and a transfer of your ' +
      'funds to it. That is a real transaction: it costs a fee, it is public, ' +
      'and your old recovery words will not restore the new wallet. Write the ' +
      'new words down before moving anything.'),
    dom.el('div', { class: 'button-group' },
      dom.el('button', { class: 'btn btn-primary', onClick: () => { currentScreen = 'create'; wallet = null; render(); } },
        'Create a new wallet'),
      dom.el('button', { class: 'btn btn-secondary', onClick: () => { currentScreen = 'wallet'; render(); } },
        'Keep using this one'))
  );
  app.appendChild(dom.el('div', { class: 'screen screen-migrate' }, card));
}
