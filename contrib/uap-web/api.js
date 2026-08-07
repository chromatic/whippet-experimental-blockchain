// API client for backend integration
// Thin wrapper that validates all responses and uses injected fetch for testability

// Headers the service worker stamps on a response it served from cache
// because the network was unreachable (see sw.js). They are the only way
// the page can tell "this is the chain as of now" from "this is the chain
// as of whenever you were last online" -- the two are otherwise identical
// JSON, and for a wallet that difference is the whole point: a stale
// balance presented as live is a wrong balance.
const STALE_HEADER = 'X-Whippet-Stale';
const CACHED_AT_HEADER = 'X-Whippet-Cached-At';

// Staleness rides along with each result rather than on the client, because
// a client instance can have several requests in flight and a single
// `lastResponseWasStale` field would report whichever finished last. It is
// attached non-enumerably so it stays invisible to JSON.stringify, to
// deepStrictEqual (which compares own *enumerable* properties), and to any
// caller that iterates the result -- existing code cannot notice it.
const STALE_INFO = Symbol('whippet.staleInfo');

function attachStaleInfo(data, response) {
  if (data === null || typeof data !== 'object') return data;
  const headers = response && response.headers;
  if (!headers || typeof headers.get !== 'function') return data;
  if (headers.get(STALE_HEADER) !== '1') return data;
  Object.defineProperty(data, STALE_INFO, {
    value: { stale: true, cachedAt: headers.get(CACHED_AT_HEADER) || null },
    enumerable: false,
    configurable: true,
  });
  return data;
}

/**
 * Report whether a result came from the service worker's offline cache.
 *
 * Returns `{stale: false, cachedAt: null}` for anything fetched live, for a
 * primitive, and when no service worker is involved at all -- callers can
 * treat the answer as always-present rather than branching on undefined.
 *
 * @param {*} data - a value returned by one of the API methods
 * @returns {{stale: boolean, cachedAt: ?string}}
 */
export function staleInfo(data) {
  if (data === null || typeof data !== 'object') return { stale: false, cachedAt: null };
  return data[STALE_INFO] || { stale: false, cachedAt: null };
}

export class API {
  /**
   * Create an API client.
   * @param {Object} options
   * @param {string} options.baseUrl - Base URL for API (e.g., 'http://api.example.com')
   * @param {Function} options.fetchImpl - Fetch implementation (can be mocked in tests)
   */
  constructor({ baseUrl, fetchImpl } = {}) {
    if (!baseUrl) throw new Error('baseUrl required');
    if (!fetchImpl) throw new Error('fetchImpl required');

    this.baseUrl = baseUrl;
    this.fetchImpl = fetchImpl;
  }

  /**
   * Make a GET request and parse JSON response.
   * @private
   */
  async request(path, options = {}) {
    const url = this.baseUrl + path;
    const response = await this.fetchImpl(url, { method: 'GET', ...options });

    if (response.status < 200 || response.status >= 300) {
      let message = `HTTP ${response.status}`;
      try {
        const body = await response.json();
        // `error` is what the indexer's per-request failures carry.
        // `store_error` is the separate, latched one: the write path has
        // stopped and /status now answers 503 with the reason in that
        // field instead. Without the fallback the whole diagnosis --
        // which is the entire point of surfacing the condition -- gets
        // thrown away and the user sees a bare "HTTP 503".
        const reason = (body && (body.error || body.store_error)) || null;
        if (reason) {
          message = `${message}: ${reason}`;
        }
      } catch (_) {
        // Body is not JSON, use status only
      }
      throw new Error(message);
    }

    try {
      return attachStaleInfo(await response.json(), response);
    } catch (e) {
      throw new Error(`Failed to parse JSON response: ${e.message}`);
    }
  }

  /**
   * Get positions for a pubkey.
   * @param {string} pubkeyHex - Public key in hex
   * @param {Object} options
   * @param {boolean} options.unspentOnly - Filter to unspent positions
   * @returns {Promise<Array>} Array of position objects
   */
  async getPositions(pubkeyHex, { unspentOnly = false } = {}) {
    const params = new URLSearchParams();
    params.append('pubkey', pubkeyHex);
    params.append('unspent', unspentOnly ? 'true' : 'false');

    const result = await this.request(`/positions?${params.toString()}`);

    if (!Array.isArray(result)) {
      throw new Error('getPositions: response is not an array');
    }

    for (let i = 0; i < result.length; i++) {
      const entry = result[i];
      if (typeof entry.txid !== 'string') {
        throw new Error(`getPositions: entry ${i} has non-string txid`);
      }
      if (typeof entry.vout !== 'number') {
        throw new Error(`getPositions: entry ${i} has non-number vout`);
      }
      if (typeof entry.height !== 'number') {
        throw new Error(`getPositions: entry ${i} has non-number height`);
      }
      if (typeof entry.value !== 'number') {
        throw new Error(`getPositions: entry ${i} has non-number value`);
      }
    }

    return result;
  }

  /**
   * Get a single position by outpoint.
   * @param {string} txid - Transaction ID
   * @param {number} vout - Output index
   * @returns {Promise<Object>} Position object
   */
  async getPosition(txid, vout) {
    const result = await this.request(`/position/${txid}/${vout}`);

    if (result === null || typeof result !== 'object' || Array.isArray(result)) {
      throw new Error('getPosition: expected an object');
    }
    if (typeof result.txid !== 'string') {
      throw new Error('getPosition: response missing string txid field');
    }
    if (typeof result.vout !== 'number') {
      throw new Error('getPosition: response missing number vout field');
    }
    if (typeof result.height !== 'number') {
      throw new Error('getPosition: response missing number height field');
    }
    if (typeof result.value !== 'number') {
      throw new Error('getPosition: response missing number value field');
    }

    return result;
  }

  /**
   * Get UTXOs for an address.
   * @param {string} address - Address
   * @returns {Promise<Array>} Array of UTXO objects
   */
  async getUtxos(address) {
    const params = new URLSearchParams();
    params.append('address', address);

    const result = await this.request(`/utxos?${params.toString()}`);

    if (!Array.isArray(result)) {
      throw new Error('getUtxos: response is not an array');
    }

    for (let i = 0; i < result.length; i++) {
      const entry = result[i];
      if (typeof entry.txid !== 'string') {
        throw new Error(`getUtxos: entry ${i} has non-string txid`);
      }
      if (typeof entry.vout !== 'number') {
        throw new Error(`getUtxos: entry ${i} has non-number vout`);
      }
      if (typeof entry.value !== 'number') {
        throw new Error(`getUtxos: entry ${i} has non-number value`);
      }
      if (typeof entry.height !== 'number') {
        throw new Error(`getUtxos: entry ${i} has non-number height`);
      }
    }

    return result;
  }

  /**
   * Get indexer status.
   *
   * The shape checked here is the `Status` struct the indexer actually
   * serializes (see StatusSnapshot in contrib/uap-indexer/index.go):
   * tip_height, tip_hash, position_count, order_count, healthy. There is
   * no `synced` flag and no `height` -- validating against invented field
   * names would throw on every successful response.
   *
   * `healthy` is deliberately not validated here. An unhealthy indexer
   * answers 503, which request() has already turned into a throw before
   * this function sees a body, so the only responses reaching these checks
   * are healthy ones and asserting on the flag would be asserting on a
   * constant.
   *
   * @returns {Promise<Object>} {tip_height, tip_hash, position_count, order_count, healthy}
   */
  async getStatus() {
    const result = await this.request('/status');

    if (result === null || typeof result !== 'object' || Array.isArray(result)) {
      throw new Error('getStatus: expected an object');
    }
    if (typeof result.tip_height !== 'number') {
      throw new Error('getStatus: response missing numeric tip_height field');
    }
    if (typeof result.tip_hash !== 'string') {
      throw new Error('getStatus: response missing string tip_hash field');
    }
    if (typeof result.position_count !== 'number') {
      throw new Error('getStatus: response missing numeric position_count field');
    }

    return result;
  }

  /**
   * Broadcast a transaction.
   * @param {string} rawHex - Raw transaction hex
   * @returns {Promise<string>} Transaction ID
   */
  async broadcast(rawHex) {
    const options = {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hex: rawHex })
    };

    const url = this.baseUrl + '/broadcast';
    const response = await this.fetchImpl(url, options);

    if (response.status < 200 || response.status >= 300) {
      let message = `HTTP ${response.status}`;
      try {
        const body = await response.json();
        if (body && body.error) {
          message = `${message}: ${body.error}`;
        }
      } catch (_) {
        // Body is not JSON
      }
      throw new Error(message);
    }

    let txid;
    try {
      txid = await response.json();
    } catch (e) {
      throw new Error(`Failed to parse broadcast response: ${e.message}`);
    }

    // A rejection is a non-2xx with a JSON error body, handled above. A 2xx
    // means the node accepted the transaction and the body is its txid.
    //
    // Validate that it really looks like one rather than scanning the string
    // for words like "error" or "invalid": substring sniffing both misses
    // rejection text that avoids those words and would misclassify a
    // legitimate txid that happened to contain them. A txid is 32 bytes of
    // hex, and nothing else is acceptable here -- whatever arrived instead
    // gets quoted in the error so the failure is diagnosable.
    if (typeof txid !== 'string' || !/^[0-9a-f]{64}$/.test(txid)) {
      throw new Error(
        `broadcast: expected a 64-character hex txid, got ${JSON.stringify(txid)}`
      );
    }

    return txid;
  }

  /**
   * Get current fee rate.
   * @returns {Promise<Object>} {sat_per_kb, source}
   */
  async getFeeRate() {
    const result = await this.request('/feerate');

    if (result === null || typeof result !== 'object' || Array.isArray(result)) {
      throw new Error('getFeeRate: expected an object');
    }
    if (typeof result.sat_per_kb !== 'number') {
      throw new Error('getFeeRate: response missing numeric sat_per_kb field');
    }
    if (typeof result.source !== 'string') {
      throw new Error('getFeeRate: response missing string source field');
    }

    return result;
  }

  /**
   * List open orders, optionally filtered by multiplier.
   * @param {Object} options
   * @param {number} options.multiplier - Filter to orders for this multiplier
   * @returns {Promise<Array>} Array of order objects
   */
  async listOrders({ multiplier } = {}) {
    let path = '/orders';
    if (multiplier !== undefined) {
      path += `?multiplier=${multiplier}`;
    }

    const result = await this.request(path);

    if (!Array.isArray(result)) {
      throw new Error('listOrders: response is not an array');
    }

    for (let i = 0; i < result.length; i++) {
      const entry = result[i];
      if (typeof entry.txid !== 'string') {
        throw new Error(`listOrders: entry ${i} has non-string txid`);
      }
      if (typeof entry.vout !== 'number') {
        throw new Error(`listOrders: entry ${i} has non-number vout`);
      }
      if (typeof entry.multiplier !== 'number') {
        throw new Error(`listOrders: entry ${i} has non-number multiplier`);
      }
      if (typeof entry.pubkey !== 'string') {
        throw new Error(`listOrders: entry ${i} has non-string pubkey`);
      }
      if (typeof entry.script_sig !== 'string') {
        throw new Error(`listOrders: entry ${i} has non-string script_sig`);
      }
      if (typeof entry.payment_script !== 'string') {
        throw new Error(`listOrders: entry ${i} has non-string payment_script`);
      }
      if (typeof entry.payment_value !== 'number') {
        throw new Error(`listOrders: entry ${i} has non-number payment_value`);
      }
      if (typeof entry.created_at !== 'number') {
        throw new Error(`listOrders: entry ${i} has non-number created_at`);
      }
    }

    return result;
  }

  /**
   * Fetch a single order by outpoint.
   * @param {string} txid - Transaction ID
   * @param {number} vout - Output index
   * @returns {Promise<Object>} Order object
   */
  async getOrder(txid, vout) {
    const result = await this.request(`/orders/${txid}/${vout}`);

    if (result === null || typeof result !== 'object' || Array.isArray(result)) {
      throw new Error('getOrder: expected an object');
    }
    if (typeof result.txid !== 'string') {
      throw new Error('getOrder: response missing string txid field');
    }
    if (typeof result.vout !== 'number') {
      throw new Error('getOrder: response missing number vout field');
    }
    if (typeof result.multiplier !== 'number') {
      throw new Error('getOrder: response missing number multiplier field');
    }
    if (typeof result.pubkey !== 'string') {
      throw new Error('getOrder: response missing string pubkey field');
    }
    if (typeof result.script_sig !== 'string') {
      throw new Error('getOrder: response missing string script_sig field');
    }
    if (typeof result.payment_script !== 'string') {
      throw new Error('getOrder: response missing string payment_script field');
    }
    if (typeof result.payment_value !== 'number') {
      throw new Error('getOrder: response missing number payment_value field');
    }
    if (typeof result.created_at !== 'number') {
      throw new Error('getOrder: response missing number created_at field');
    }

    return result;
  }

  /**
   * Publish a signed maker order.
   * @param {Object} order - Order object with txid, vout, multiplier, script_sig, payment_script, payment_value
   * @returns {Promise<Object>} Published order object
   */
  async publishOrder(order) {
    const options = {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(order)
    };

    const url = this.baseUrl + '/orders';
    const response = await this.fetchImpl(url, options);

    if (response.status < 200 || response.status >= 300) {
      let message = `HTTP ${response.status}`;
      try {
        const body = await response.json();
        if (body && body.error) {
          message = `${message}: ${body.error}`;
        }
      } catch (_) {
        // Body is not JSON
      }
      throw new Error(message);
    }

    let result;
    try {
      result = await response.json();
    } catch (e) {
      throw new Error(`Failed to parse order response: ${e.message}`);
    }

    if (result === null || typeof result !== 'object' || Array.isArray(result)) {
      throw new Error('publishOrder: expected an object');
    }
    if (typeof result.txid !== 'string') {
      throw new Error('publishOrder: response missing string txid field');
    }
    if (typeof result.vout !== 'number') {
      throw new Error('publishOrder: response missing number vout field');
    }
    if (typeof result.multiplier !== 'number') {
      throw new Error('publishOrder: response missing number multiplier field');
    }
    if (typeof result.pubkey !== 'string') {
      throw new Error('publishOrder: response missing string pubkey field');
    }
    if (typeof result.script_sig !== 'string') {
      throw new Error('publishOrder: response missing string script_sig field');
    }
    if (typeof result.payment_script !== 'string') {
      throw new Error('publishOrder: response missing string payment_script field');
    }
    if (typeof result.payment_value !== 'number') {
      throw new Error('publishOrder: response missing number payment_value field');
    }
    if (typeof result.created_at !== 'number') {
      throw new Error('publishOrder: response missing number created_at field');
    }

    return result;
  }
}

/**
 * Poll for confirmation of a transaction that has already been broadcast.
 *
 * A 2xx from `broadcast()` means the node's mempool ACCEPTED the
 * transaction -- it is not yet in a block. This is the piece that turns
 * that accept into an actual confirmation signal: it polls
 * `getUtxos(address)` and looks for an output belonging to `txid`, which
 * only appears once the indexer has processed a block containing it.
 *
 * This resolves in exactly one of two ways, and always resolves -- it never
 * hangs waiting forever:
 *   - `{ status: 'confirmed', utxo }` the first time a matching UTXO shows
 *     up.
 *   - `{ status: 'timeout' }` after `maxAttempts` polls with no match. This
 *     is NOT a claim that the transaction failed. The node already accepted
 *     it, so it can still confirm later; a reorg, a slow indexer, or simple
 *     bad luck can all make confirmation take longer than the poll budget.
 *     'timeout' only means this call stopped watching -- callers must not
 *     render it as "failed", only as "not yet confirmed, check again."
 *
 * A `getUtxos()` error (network blip, indexer hiccup) is treated as a poll
 * that found nothing rather than aborting the wait, since a single failed
 * request says nothing about whether the transaction confirmed; it is
 * still reported to the caller via `onError` so it isn't silently dropped.
 *
 * @param {Object} options
 * @param {API} options.apiClient
 * @param {string} options.address - address whose UTXOs to poll
 * @param {string} options.txid - txid to watch for
 * @param {number} [options.maxAttempts=15] - ~90s of waiting at the default interval
 * @param {number} [options.intervalMs=6000] - matches Whippet's ~6s block time
 * @param {Function} [options.sleep] - `(ms) => Promise<void>`, injectable so
 *   tests don't have to actually wait; defaults to a real timer-based sleep
 * @param {Function} [options.onError] - called with each transient error encountered
 * @returns {Promise<{status: 'confirmed', utxo: Object} | {status: 'timeout'}>}
 */
export async function pollForConfirmation({
  apiClient,
  address,
  txid,
  maxAttempts = 15,
  intervalMs = 6000,
  sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  onError = () => {}
}) {
  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    if (attempt > 0) {
      await sleep(intervalMs);
    }
    try {
      const utxos = await apiClient.getUtxos(address);
      const match = utxos.find((u) => u.txid === txid);
      if (match) {
        return { status: 'confirmed', utxo: match };
      }
    } catch (e) {
      onError(e);
    }
  }
  return { status: 'timeout' };
}
