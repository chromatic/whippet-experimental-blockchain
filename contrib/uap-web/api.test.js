// API client tests - backend integration, error handling, response validation
import assert from 'assert';
import { test, assertEqual, assertDeepEqual, assertThrows, run } from './test-harness.js';

// =============================================================================
// FAKE FETCH FOR TESTING
// =============================================================================

class FakeFetch {
  constructor() {
    this.calls = [];
  }

  recordCall(url, options) {
    this.calls.push({ url, options: options || {} });
  }

  reset() {
    this.calls = [];
  }

  /**
   * Create a fetch implementation that returns a fixed response
   */
  static returning(status, body) {
    return async (url, options) => {
      const instance = new FakeFetch();
      instance.recordCall(url, options);
      return {
        status,
        json: async () => body,
        text: async () => typeof body === 'string' ? body : JSON.stringify(body)
      };
    };
  }

  /**
   * Create a fetch implementation that returns malformed JSON
   */
  static returningMalformed() {
    return async (url, options) => {
      return {
        status: 200,
        json: async () => { throw new Error('Invalid JSON'); },
        text: async () => 'not json'
      };
    };
  }

  /**
   * Create a fetch implementation that throws
   */
  static throwing(error) {
    return async (url, options) => {
      throw error;
    };
  }
}

// The exact JSON the indexer's GET /status returns -- the field names come
// from the Status struct's json tags in contrib/uap-indexer/index.go. If that
// struct changes, this fixture and api.js must change with it.
// A txid is 32 bytes of hex; the client will not accept anything else as
// evidence that a transaction was broadcast.
const TXID_FIXTURE = '3f2a' + '0'.repeat(60);

const STATUS_FIXTURE = {
  tip_height: 1000,
  tip_hash: '00000000000000000000000000000000000000000000000000000000000000ab',
  position_count: 7,
  order_count: 2
};

// =============================================================================
// TESTS
// =============================================================================

let api = null;

test('api module can be imported', async () => {
  try {
    const module = await import('./api.js');
    api = module;
    assert(api.API, 'api.API exists');
  } catch (e) {
    throw new Error(`Failed to import api.js: ${e.message}`);
  }
});

// =============================================================================
// BASIC API CONSTRUCTION
// =============================================================================

test('API constructor accepts baseUrl and fetchImpl', async () => {
  const fetchFake = async () => ({ status: 200, json: async () => ({}) });
  const client = new api.API({ baseUrl: 'http://example.com', fetchImpl: fetchFake });
  assert(client, 'API instance created');
});

test('API constructor throws without baseUrl', async () => {
  const fetchFake = async () => ({ status: 200, json: async () => ({}) });
  await assertThrows(
    () => new api.API({ fetchImpl: fetchFake }),
    'baseUrl'
  );
});

test('API constructor throws without fetchImpl', async () => {
  await assertThrows(
    () => new api.API({ baseUrl: 'http://example.com' }),
    'fetchImpl'
  );
});

// =============================================================================
// GETPOSITIONS
// =============================================================================

test('getPositions makes GET request with correct URL', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return {
      status: 200,
      json: async () => []
    };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.getPositions('ab12cd', { unspentOnly: true });

  assertEqual(calls.length, 1, 'one fetch call');
  assertEqual(calls[0].options.method, 'GET', 'GET method');
  assert(calls[0].url.includes('/positions'), 'URL contains /positions');
  assert(calls[0].url.includes('pubkey=ab12cd'), 'URL contains pubkey param');
  assert(calls[0].url.includes('unspent=true'), 'URL contains unspent param');
});

test('getPositions validates response is array', async () => {
  const fetchFake = FakeFetch.returning(200, { notAnArray: true });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getPositions('ab12cd', { unspentOnly: false }),
    'array'
  );
});

test('getPositions validates array entries', async () => {
  const fetchFake = FakeFetch.returning(200, [
    { txid: 'abc', vout: 0, height: 100, value: 5000 },
    { txid: 'def', vout: 1 }  // Missing height and value
  ]);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getPositions('ab12cd', {}),
    'height'
  );
});

// =============================================================================
// GETUTXOS
// =============================================================================

test('getUtxos makes GET request with correct URL', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return {
      status: 200,
      json: async () => []
    };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.getUtxos('WfakeaddressXYZ');

  assertEqual(calls.length, 1, 'one fetch call');
  assertEqual(calls[0].options.method, 'GET', 'GET method');
  assert(calls[0].url.includes('/utxos'), 'URL contains /utxos');
  assert(calls[0].url.includes('address=WfakeaddressXYZ'), 'URL contains address param');
});

test('getUtxos validates response is array', async () => {
  const fetchFake = FakeFetch.returning(200, 'not an array');
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getUtxos('Waddress'),
    'array'
  );
});

test('getUtxos validates array entries have required numeric fields', async () => {
  const fetchFake = FakeFetch.returning(200, [
    { txid: 'abc', vout: 0, value: 5000, height: 100 },
    { txid: 'def', vout: 'not-a-number', value: 1000, height: 101 }  // Invalid vout
  ]);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getUtxos('Waddress'),
    'vout'
  );
});

test('getUtxos validates each entry has string txid', async () => {
  const fetchFake = FakeFetch.returning(200, [
    { txid: 123, vout: 0, value: 5000, height: 100 }  // txid not a string
  ]);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getUtxos('Waddress'),
    'txid'
  );
});

test('getUtxos returns valid response', async () => {
  const utxos = [
    { txid: 'abc123', vout: 0, value: 50000, height: 100 },
    { txid: 'def456', vout: 1, value: 75000, height: 101 }
  ];
  const fetchFake = FakeFetch.returning(200, utxos);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  const result = await client.getUtxos('Waddress');
  assertDeepEqual(result, utxos, 'returned UTXOs match');
});

// =============================================================================
// GETSTATUS
// =============================================================================

test('getStatus makes GET request', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return {
      status: 200,
      json: async () => STATUS_FIXTURE
    };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.getStatus();

  assertEqual(calls.length, 1, 'one fetch call');
  assert(calls[0].url.includes('/status'), 'URL contains /status');
});

test('getStatus validates response has required fields', async () => {
  const fetchFake = FakeFetch.returning(200, { tip_hash: 'ff', position_count: 0 }); // no tip_height
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getStatus(),
    'tip_height'
  );
});

// Regression: getStatus once validated `synced`/`height`, field names the
// indexer has never sent. It threw on every successful response. This pins
// the client to the Status struct the server actually serializes -- see
// StatusSnapshot in contrib/uap-indexer/index.go.
test('getStatus accepts the indexer\'s real /status payload', async () => {
  const fetchFake = FakeFetch.returning(200, STATUS_FIXTURE);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  const status = await client.getStatus();
  assertEqual(status.tip_height, STATUS_FIXTURE.tip_height, 'tip_height passes through');
  assertEqual(status.tip_hash, STATUS_FIXTURE.tip_hash, 'tip_hash passes through');
  assertEqual(status.position_count, STATUS_FIXTURE.position_count, 'position_count passes through');
});

// A latched write error stops the indexer tracking the chain while the
// database stays perfectly readable, so the node answers /status with 503
// and the reason in `store_error` (see writeStatus in
// contrib/uap-indexer/api.go). request() historically only looked for
// `error`, which meant the one field explaining *why* the service is
// degraded was parsed and then discarded, leaving the operator with a bare
// "HTTP 503".
test('getStatus surfaces the latched store error behind a 503', async () => {
  const fetchFake = FakeFetch.returning(503, {
    ...STATUS_FIXTURE,
    healthy: false,
    store_error: 'disk I/O error while writing block 419',
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  let thrown = null;
  try {
    await client.getStatus();
  } catch (e) {
    thrown = e;
  }

  assert(thrown !== null, 'a 503 must reject rather than return a body');
  assert(
    thrown.message.includes('503'),
    `message should name the status code, got: ${thrown.message}`
  );
  assert(
    thrown.message.includes('disk I/O error while writing block 419'),
    `message should carry store_error, got: ${thrown.message}`
  );
});

// =============================================================================
// OFFLINE STALENESS
// =============================================================================

// sw.js serves cached API responses during an outage and marks them with
// X-Whippet-Stale. Before staleInfo() existed, request() parsed the body and
// dropped the headers on the floor, so an offline wallet rendered a cached
// balance identically to a live one. For a wallet that is not a cosmetic
// gap: the number on screen would be the chain as of some unstated earlier
// moment, presented as current.
function fetchWithHeaders(status, body, headers) {
  return async () => ({
    status,
    headers: { get: (name) => (name in headers ? headers[name] : null) },
    json: async () => body,
  });
}

test('staleInfo reports a live response as not stale', async () => {
  const fetchFake = fetchWithHeaders(200, STATUS_FIXTURE, {});
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  const status = await client.getStatus();
  assertEqual(api.staleInfo(status).stale, false, 'a live read is not stale');
  assertEqual(api.staleInfo(status).cachedAt, null, 'no cache timestamp on a live read');
});

test('staleInfo surfaces a response the service worker served from cache', async () => {
  const when = '2026-08-05T12:00:00.000Z';
  const fetchFake = fetchWithHeaders(200, STATUS_FIXTURE, {
    'X-Whippet-Stale': '1',
    'X-Whippet-Cached-At': when,
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  const status = await client.getStatus();
  assertEqual(api.staleInfo(status).stale, true, 'an offline read is stale');
  assertEqual(api.staleInfo(status).cachedAt, when, 'the cache timestamp comes through');
});

test('stale marking survives array results and does not disturb them', async () => {
  const utxos = [{ txid: 'abc123', vout: 0, value: 50000, height: 100 }];
  const fetchFake = fetchWithHeaders(200, utxos, {
    'X-Whippet-Stale': '1',
    'X-Whippet-Cached-At': '2026-08-05T12:00:00.000Z',
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  const result = await client.getUtxos('Waddress');
  assertEqual(api.staleInfo(result).stale, true, 'arrays carry staleness too');
  // The marker must be invisible to everything that already reads results:
  // it is attached non-enumerably precisely so validation, iteration and
  // deep-equality keep behaving exactly as they did before it existed.
  assertDeepEqual(result, utxos, 'the marker does not change the value');
  assertEqual(Object.keys(result).length, 1, 'the marker is not an own enumerable key');
  assertEqual(JSON.parse(JSON.stringify(result)).length, 1, 'the marker does not serialize');
});

test('staleInfo is safe on values that never went through the API', async () => {
  assertEqual(api.staleInfo(null).stale, false, 'null is not stale');
  assertEqual(api.staleInfo(42).stale, false, 'a primitive is not stale');
  assertEqual(api.staleInfo({}).stale, false, 'an unmarked object is not stale');
});

// =============================================================================
// BROADCAST
// =============================================================================

test('broadcast makes POST request with correct payload', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return {
      status: 200,
      json: async () => TXID_FIXTURE
    };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.broadcast('deadbeef');

  assertEqual(calls.length, 1, 'one fetch call');
  assertEqual(calls[0].options.method, 'POST', 'POST method');
  assert(calls[0].url.includes('/broadcast'), 'URL contains /broadcast');
  // Body should be parsed from options
  const body = calls[0].options.body;
  assert(body, 'body present');
  const parsed = JSON.parse(body);
  assertEqual(parsed.hex, 'deadbeef', 'hex field in body');
});

// A rejection is a non-2xx carrying the node's message. This is the shape
// the indexer's handlers use for every other error, and what /broadcast will
// use for a sendrawtransaction failure.
test('broadcast surfaces the node rejection message from a non-2xx', async () => {
  const fetchFake = FakeFetch.returning(400, {
    error: '16: mandatory-script-verify-flag-failed (Operation not valid with the current stack size)'
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.broadcast('deadbeef'),
    'mandatory-script-verify-flag-failed'
  );
});

test('broadcast returns the txid on success', async () => {
  const txid = TXID_FIXTURE;
  const fetchFake = FakeFetch.returning(200, txid);
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  assertEqual(await client.broadcast('deadbeef'), txid, 'txid passes through');
});

// Defensive: if a 2xx ever carries something that is not a txid -- a proxy
// error page, a rejection string returned with the wrong status -- the client
// must not hand it back to the UI as if a transaction had been accepted.
test('broadcast rejects a 2xx body that is not a txid', async () => {
  for (const body of ['insufficient fees', '', 'a'.repeat(63), 'A'.repeat(64), 42, null]) {
    const client = new api.API({
      baseUrl: 'http://api.local',
      fetchImpl: FakeFetch.returning(200, body)
    });
    await assertThrows(
      () => client.broadcast('deadbeef'),
      'expected a 64-character hex txid'
    );
  }
});

// =============================================================================
// GETFEERATE
// =============================================================================

test('getFeeRate makes GET request', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return {
      status: 200,
      json: async () => ({ sat_per_kb: 1000, source: 'node' })
    };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.getFeeRate();

  assertEqual(calls.length, 1, 'one fetch call');
  assert(calls[0].url.includes('/feerate'), 'URL contains /feerate');
});

test('getFeeRate validates response is a number', async () => {
  const fetchFake = FakeFetch.returning(200, 'not a number');
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getFeeRate(),
    'object'
  );
});

// =============================================================================
// CANCELORDER
// =============================================================================

test('cancelOrder makes a DELETE request to /orders/{txid}/{vout} with sig as a query param', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push({ url, options });
    return { status: 204, json: async () => { throw new Error('no body on 204'); } };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.cancelOrder('a'.repeat(64), 3, 'deadbeef83');

  assertEqual(calls.length, 1, 'one fetch call');
  assertEqual(calls[0].options.method, 'DELETE', 'DELETE method');
  assert(calls[0].url.includes(`/orders/${'a'.repeat(64)}/3`), 'URL contains /orders/{txid}/{vout}');
  assert(calls[0].url.includes('sig=deadbeef83'), 'URL carries the cancel signature as the sig query param');
  assert(!calls[0].url.includes('script_sig='), 'the old script_sig contract must be gone, not just renamed');
});

test('cancelOrder resolves without trying to parse a body on 204 No Content', async () => {
  const fetchFake = async () => ({
    status: 204,
    json: async () => { throw new Error('cancelOrder must not call json() on a 204'); }
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  // Must not throw -- a 204 has no body, and json() on this fake fetch
  // throws if called at all.
  await client.cancelOrder('a'.repeat(64), 0, 'deadbeef83');
});

// CancelOrder in contrib/uap-indexer/orders.go returns this exact message
// when the order row is gone -- already filled, already cancelled, or the
// position was spent some other way. The client must not swallow it.
test('cancelOrder surfaces "no such order" from the relay', async () => {
  const fetchFake = FakeFetch.returning(400, { error: 'no such order' });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.cancelOrder('a'.repeat(64), 0, 'deadbeef83'),
    'no such order'
  );
});

// The relay's real auth for a cancel: a valid ECDSA signature by the
// position's own key over cancel_auth.go's cancelMessage (see CancelOrder
// in orders.go). This covers a wrong key, a signature for a different
// order, or a stale/replayed one -- CancelOrder does not distinguish them
// in its error text, and neither does the client.
test('cancelOrder surfaces an unauthorized cancel signature from the relay', async () => {
  const fetchFake = FakeFetch.returning(400, {
    error: 'cancel signature does not authorize this order: signature does not authorize cancelling this order',
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.cancelOrder('a'.repeat(64), 0, 'wrongsig'),
    'does not authorize'
  );
});

test('cancelOrder throws with the status code on a non-JSON failure body', async () => {
  const fetchFake = FakeFetch.returningMalformed();
  // returningMalformed() defaults to status 200; force a failure status by
  // wrapping it so cancelOrder sees a non-2xx with an unparsable body.
  const failingFetch = async (url, options) => ({
    status: 500,
    json: async () => { throw new Error('not json'); }
  });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: failingFetch });

  await assertThrows(
    () => client.cancelOrder('a'.repeat(64), 0, 'deadbeef83'),
    '500'
  );
});

// =============================================================================
// ERROR HANDLING
// =============================================================================

test('non-2xx response throws with status', async () => {
  const fetchFake = FakeFetch.returning(500, { error: 'Internal server error' });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getStatus(),
    '500'
  );
});

test('non-2xx response includes server error message if JSON', async () => {
  const fetchFake = FakeFetch.returning(400, { error: 'Invalid request format' });
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getStatus(),
    'Invalid request format'
  );
});

test('malformed JSON response throws', async () => {
  const fetchFake = FakeFetch.returningMalformed();
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await assertThrows(
    () => client.getStatus(),
    'JSON'
  );
});

// =============================================================================
// URL CONSTRUCTION
// =============================================================================

test('baseUrl has no trailing slash, paths have leading slash', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push(url);
    return { status: 200, json: async () => [] };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  await client.getUtxos('Wtest');

  assertEqual(calls.length, 1, 'one call');
  // Should not have double slashes
  assert(!calls[0].includes('//utxos'), 'no double slash in path');
  assert(calls[0].startsWith('http://api.local/'), 'URL starts with base and slash');
});

test('URLSearchParams prevents query string injection', async () => {
  const calls = [];
  const fetchFake = async (url, options) => {
    calls.push(url);
    return { status: 200, json: async () => [] };
  };
  const client = new api.API({ baseUrl: 'http://api.local', fetchImpl: fetchFake });

  // Try to inject extra params
  await client.getUtxos('Waddress&evil=param');

  assertEqual(calls.length, 1, 'one call');
  // The injected param should be URL-encoded, not parsed as a separate param
  assert(calls[0].includes('address='), 'address param present');
  // The evil param should be part of the address value, not a separate param
  assert(calls[0].includes('%26evil'), 'injection is encoded');
});

// =============================================================================
// POLLFORCONFIRMATION (optimistic broadcast UI's confirmation source)
// =============================================================================

test('pollForConfirmation module export exists', async () => {
  assert(api.pollForConfirmation, 'api.pollForConfirmation exists');
});

// A fake apiClient (not the real API class -- pollForConfirmation only
// ever calls .getUtxos(address), so a minimal stand-in keeps these tests
// about the polling logic, not HTTP plumbing already covered above).
function fakeClient(utxosPerCall) {
  let call = 0;
  const calls = [];
  return {
    calls,
    getUtxos: async (address) => {
      calls.push(address);
      const result = utxosPerCall[Math.min(call, utxosPerCall.length - 1)];
      call++;
      if (result instanceof Error) throw result;
      return result;
    }
  };
}

// Every test injects a no-op sleep so the suite doesn't actually wait
// real seconds for the (default 6s) poll interval.
const noSleep = async () => {};

test('pollForConfirmation resolves confirmed as soon as a matching UTXO appears', async () => {
  const txid = 'f'.repeat(64);
  const client = fakeClient([[{ txid, vout: 0, value: 1000, height: 5 }]]);

  const result = await api.pollForConfirmation({
    apiClient: client,
    address: 'Waddress',
    txid,
    sleep: noSleep
  });

  assertEqual(result.status, 'confirmed', 'resolves confirmed');
  assertEqual(result.utxo.txid, txid, 'returns the matching utxo');
  assertEqual(client.calls.length, 1, 'stopped polling after the first match');
});

test('pollForConfirmation keeps polling until a match shows up', async () => {
  const txid = 'e'.repeat(64);
  let sleeps = 0;
  const client = fakeClient([
    [],
    [{ txid: 'other', vout: 0, value: 1, height: 1 }],
    [{ txid, vout: 1, value: 2000, height: 9 }]
  ]);

  const result = await api.pollForConfirmation({
    apiClient: client,
    address: 'Waddress',
    txid,
    maxAttempts: 10,
    sleep: async () => { sleeps++; }
  });

  assertEqual(result.status, 'confirmed', 'eventually resolves confirmed');
  assertEqual(client.calls.length, 3, 'polled three times before matching');
  assertEqual(sleeps, 2, 'slept between polls, not before the first one');
});

test('pollForConfirmation resolves timeout, never confirmed, when the tx never shows up', async () => {
  const txid = 'a'.repeat(64);
  const client = fakeClient([[]]);

  const result = await api.pollForConfirmation({
    apiClient: client,
    address: 'Waddress',
    txid,
    maxAttempts: 4,
    sleep: noSleep
  });

  assertEqual(result.status, 'timeout', 'resolves timeout rather than hanging');
  assert(!('utxo' in result), 'timeout result carries no utxo, so callers cannot mistake it for a match');
  assertEqual(client.calls.length, 4, 'polled exactly maxAttempts times');
});

test('pollForConfirmation treats a getUtxos error as an empty poll, not an abort', async () => {
  const txid = 'b'.repeat(64);
  const errors = [];
  const client = fakeClient([new Error('network blip'), new Error('network blip')]);

  const result = await api.pollForConfirmation({
    apiClient: client,
    address: 'Waddress',
    txid,
    maxAttempts: 2,
    sleep: noSleep,
    onError: (e) => errors.push(e)
  });

  assertEqual(result.status, 'timeout', 'still resolves rather than throwing/hanging on repeated errors');
  assertEqual(errors.length, 2, 'each transient error was reported via onError');
});

test('pollForConfirmation resolves even when a match arrives on the very last attempt', async () => {
  const txid = 'c'.repeat(64);
  const client = fakeClient([
    [],
    [],
    [{ txid, vout: 0, value: 500, height: 2 }]
  ]);

  const result = await api.pollForConfirmation({
    apiClient: client,
    address: 'Waddress',
    txid,
    maxAttempts: 3,
    sleep: noSleep
  });

  assertEqual(result.status, 'confirmed', 'last-attempt match is still caught');
});

// =============================================================================
// RUN TESTS
// =============================================================================

run();
