// DOM helper functions for safe element construction
// All strings go through text nodes, never innerHTML
// Event handlers use addEventListener, never on* attributes
// XSS vectors (javascript: hrefs, on* attributes) are explicitly rejected

// Allow injection of document for testing
let _doc = null;

export function setDocument(doc) {
  _doc = doc;
}

function getDocument() {
  if (_doc) return _doc;
  if (typeof document !== 'undefined') return document;
  throw new Error('No document available and none injected');
}

/**
 * Create an element safely.
 *
 * @param {string} tag - HTML tag name
 * @param {Object} attrs - attributes, where:
 *   - string values are set via setAttribute
 *   - onClick/onMouseEnter/etc (camelCase) become addEventListener calls
 *   - 'class' and 'className' set the class attribute
 *   - javascript: hrefs are rejected
 *   - on* attribute names from strings are rejected
 * @param {...(Element|string)} children - child elements or text strings
 * @returns {Element} constructed element
 */
export function el(tag, attrs = {}, ...children) {
  const doc = getDocument();
  const element = doc.createElement(tag);

  // Set attributes safely
  for (const [key, value] of Object.entries(attrs)) {
    // Reject on* attributes from strings
    if (key.startsWith('on') && typeof value === 'string') {
      throw new Error(`Refusing to set ${key} attribute from a string value (event handlers must use addEventListener)`);
    }

    // Handle event listeners (onClick, onMouseEnter, etc.)
    if (key.startsWith('on') && typeof value === 'function') {
      const eventType = key.slice(2).toLowerCase();
      // Convert camelCase to event name (onMouseEnter -> mouseenter, onClick -> click)
      const eventName = key.slice(2)
        .replace(/([A-Z])/g, (match, $1) => $1.toLowerCase())
        .replace(/^on/, '')
        .replace(/([A-Z])/g, (match, $1) => $1.toLowerCase());
      element.addEventListener(eventName, value);
      continue;
    }

    // Reject javascript: URIs in href/src attributes (case/whitespace insensitive)
    if ((key === 'href' || key === 'src') && typeof value === 'string') {
      const normalized = value
        .replace(/\s/g, '') // Remove all whitespace
        .toLowerCase();
      if (normalized.startsWith('javascript:')) {
        throw new Error(`Refusing to set ${key} to a javascript: URI`);
      }
    }

    // Handle class attribute
    if (key === 'class' || key === 'className') {
      element.setAttribute('class', value);
      continue;
    }

    // Set all other attributes normally
    if (value != null) {
      element.setAttribute(key, String(value));
    }
  }

  // Add children, converting strings to text nodes
  for (const child of children) {
    if (typeof child === 'string') {
      const doc = getDocument();
      element.appendChild(doc.createTextNode(child));
    } else if (child != null) {
      element.appendChild(child);
    }
  }

  return element;
}

/**
 * Create a text node.
 * @param {string} str - text content
 * @returns {Text} text node
 */
export function text(str) {
  const doc = getDocument();
  return doc.createTextNode(str);
}

/**
 * Build a skeleton loading placeholder: a block of lines with the same
 * rough footprint as the content that will replace it, so the layout
 * doesn't jump when a fetch resolves. The shimmer animation itself lives in
 * style.css (and is switched off there under prefers-reduced-motion) --
 * this only emits the markup and the CSS hooks (`skeleton`, `skeleton-line`)
 * for it to style. No caller-supplied text ever passes through this
 * function, so there is nothing here for the XSS corpus to hit; it exists
 * only to build inert placeholder <div>s via el().
 *
 * @param {Object} [opts]
 * @param {number} [opts.lines=1] - how many placeholder lines to stack
 * @param {string} [opts.className=''] - extra class(es) for sizing variants
 *   (e.g. 'skeleton-line--short'), appended to each line's own class
 * @returns {Element} a `div.skeleton` containing `opts.lines` `div.skeleton-line`s
 */
export function skeleton({ lines = 1, className = '' } = {}) {
  const rows = [];
  for (let i = 0; i < lines; i++) {
    const cls = className ? `skeleton-line ${className}` : 'skeleton-line';
    rows.push(el('div', { class: cls }));
  }
  // role="status" + aria-label tell assistive tech this region is loading,
  // rather than reading out an empty div; aria-hidden on the lines keeps
  // the (contentless) placeholder rows themselves out of the accessibility
  // tree so only the single "Loading" announcement is heard.
  const container = el('div', { class: 'skeleton', role: 'status', 'aria-label': 'Loading' }, ...rows);
  for (const row of rows) {
    row.setAttribute('aria-hidden', 'true');
  }
  return container;
}

// Human-readable, unambiguous labels for each state a broadcast transaction
// can be in from the wallet's point of view. Keyed here (not inline in
// txStatus) so the three strings are easy to audit together: the pending
// and timeout wording must never be mistakable for the confirmed one.
const TX_STATUS_LABELS = {
  // Shown the instant broadcast() resolves, i.e. the instant the node's
  // mempool accepted the transaction. This is the "optimistic" state: the
  // UI shows it immediately, on the strength of a ~6s block time, rather
  // than blocking the user on a wait for confirmation. It is explicit that
  // confirmation is still pending, precisely so it cannot be read as done.
  pending: 'Broadcast — waiting for confirmation (~6s per block)',
  // Only reached once polling has actually observed the resulting output,
  // never on a timer and never merely because broadcast() didn't throw.
  confirmed: 'Confirmed',
  // Reached if confirmation hasn't been observed within the poll budget.
  // This is deliberately NOT "Failed": the node accepted the transaction,
  // so it may still confirm later. It only means this page stopped
  // watching for it.
  timeout: 'Not yet confirmed — check back later'
};

/**
 * Build a status badge for a broadcast transaction.
 *
 * THIS IS A WALLET: a transaction the node has accepted is not yet a
 * transaction that has happened. The three states are worded so that
 * neither unconfirmed state ('pending', 'timeout') can be mistaken for the
 * confirmed one, and are given distinct CSS classes so they are also
 * visually distinct in style.css.
 *
 * @param {'pending'|'confirmed'|'timeout'} state
 * @param {string} txid
 * @returns {Element}
 */
export function txStatus(state, txid) {
  const label = TX_STATUS_LABELS[state];
  if (!label) {
    throw new Error(`txStatus: unknown state ${JSON.stringify(state)}`);
  }
  return el('div', { class: `tx-status tx-status-${state}`, role: 'status' },
    el('span', { class: 'tx-status-label' }, label),
    el('code', { class: 'tx-status-txid' }, txid)
  );
}

/**
 * A banner marking figures that came from the service worker's offline
 * cache rather than from the network.
 *
 * The wallet's whole job is to tell the user what they hold, so a balance
 * rendered from cache and one rendered live must not look identical. sw.js
 * stamps X-Whippet-Stale on anything it serves during an outage and
 * api.js's staleInfo() reads it back; this is where that finally becomes
 * something a user can see. Without it the offline behaviour is not
 * "degraded", it is "confidently wrong".
 *
 * The timestamp is rendered as the raw ISO string the worker recorded: it
 * is unambiguous, needs no locale handling, and -- unlike "3 hours ago" --
 * cannot quietly become wrong just because the tab stayed open.
 *
 * @param {?string} cachedAt - ISO timestamp from X-Whippet-Cached-At, if any
 * @returns {Element}
 */
export function staleNotice(cachedAt) {
  const suffix = cachedAt ? ` Last updated ${cachedAt}.` : '';
  return el('div', { class: 'stale-notice', role: 'status' },
    `Offline — showing cached data, which may be out of date.${suffix}`
  );
}

/**
 * Remove all children from an element.
 * @param {Element} node - element to clear
 */
export function clear(node) {
  // removeChild in a loop, not `node.children.length = 0`. In a real
  // browser `children` is a live read-only HTMLCollection, so assigning to
  // its length is silently a no-op -- the node would keep every child and
  // each re-render would append another copy of the screen. Only a test
  // double whose `children` is a plain array would make that look correct.
  while (node.lastChild) {
    node.removeChild(node.lastChild);
  }
}
