// Tests for sw.js -- the PWA service worker.
//
// sw.js is deliberately a classic (non-module) worker script: it is
// registered without `{ type: 'module' }` (see sw-register.js), and it has
// no import/export statements of its own so that check-web-imports.mjs has
// nothing to resolve for it. That means it can't be `import`ed like the rest
// of this project's modules. Instead its source is loaded and executed with
// Node's `vm` module against a minimal sandbox standing in for the real
// ServiceWorkerGlobalScope (self, caches, fetch) -- close enough to exercise
// the actual caching decisions (network-first, offline fallback, staleness
// stamping, GET-only interception, cross-origin passthrough, old-cache
// eviction) without a browser, which this environment does not have.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';
import { test, assert, assertEqual, run } from './test-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SW_SOURCE = readFileSync(join(__dirname, 'sw.js'), 'utf8');

// A tiny in-memory stand-in for the real Cache Storage API: enough of
// open/match/put/keys/delete to drive the exact calls sw.js makes.
class MockCache {
  constructor() {
    this.store = new Map();
  }
  async match(request) {
    return this.store.get(typeof request === 'string' ? request : request.url);
  }
  async put(request, response) {
    this.store.set(typeof request === 'string' ? request : request.url, response);
  }
}

class MockCacheStorage {
  constructor() {
    this.named = new Map();
  }
  async open(name) {
    if (!this.named.has(name)) this.named.set(name, new MockCache());
    return this.named.get(name);
  }
  async keys() {
    return [...this.named.keys()];
  }
  async delete(name) {
    return this.named.delete(name);
  }
}

// Loads a fresh copy of sw.js into its own sandbox so tests don't share
// cache/listener state. Returns the captured event listeners plus the
// sandbox's cache storage and hooks for swapping the mocked `fetch` /
// observing self.skipWaiting().
function loadWorker() {
  const listeners = {};
  let fetchImpl = async () => {
    throw new Error('no fetch mock installed for this test');
  };
  let skipWaitingCalled = false;

  const cacheStorage = new MockCacheStorage();

  const sandbox = {
    self: {
      location: { origin: 'http://localhost' },
      addEventListener: (type, handler) => {
        listeners[type] = handler;
      },
      skipWaiting: () => {
        skipWaitingCalled = true;
      },
      clients: { claim: async () => {} },
    },
    caches: cacheStorage,
    // `fetch` is looked up dynamically at each call site (it's a free
    // variable, not bound at parse time), so reassigning this.fetchImpl
    // between scenarios changes what the already-compiled sw.js code calls.
    fetch: (...args) => fetchImpl(...args),
    Response,
    Headers,
    URL,
    console,
  };
  vm.createContext(sandbox);
  vm.runInContext(SW_SOURCE, sandbox, { filename: 'sw.js' });

  return {
    listeners,
    cacheStorage,
    setFetch(fn) {
      fetchImpl = fn;
    },
    skipWaitingCalled() {
      return skipWaitingCalled;
    },
  };
}

// Drives the 'fetch' listener the way the browser would: builds a fake
// FetchEvent whose respondWith() captures the promise, invokes the handler,
// and returns the resolved Response.
async function dispatchFetch(worker, request) {
  let captured;
  const event = {
    request,
    respondWith(promise) {
      captured = promise;
    },
  };
  worker.listeners.fetch(event);
  assert(captured, 'fetch handler did not call respondWith for ' + request.url);
  return captured;
}

test('install calls skipWaiting so a new worker activates without waiting for every tab to close', () => {
  const worker = loadWorker();
  worker.listeners.install();
  assert(worker.skipWaitingCalled(), 'install handler did not call self.skipWaiting()');
});

test('activate deletes caches from a previous CACHE_VERSION and keeps the current ones', async () => {
  const worker = loadWorker();
  // Seed a stale cache (as if left behind by a worker with an older
  // CACHE_VERSION) plus the two current-version caches.
  await worker.cacheStorage.open('whippet-shell-v0-stale');
  await worker.cacheStorage.open('whippet-shell-v1');
  await worker.cacheStorage.open('whippet-api-v1');

  let waited;
  worker.listeners.activate({ waitUntil: (p) => (waited = p) });
  await waited;

  const remaining = await worker.cacheStorage.keys();
  assert(!remaining.includes('whippet-shell-v0-stale'), 'stale cache from a previous version was not deleted');
  assert(remaining.includes('whippet-shell-v1'), 'current app-shell cache was wrongly deleted');
  assert(remaining.includes('whippet-api-v1'), 'current API cache was wrongly deleted');
});

test('a mutating (POST) request is never intercepted, even to an API path', async () => {
  const worker = loadWorker();
  let networkHit = false;
  worker.setFetch(async () => {
    networkHit = true;
    return new Response('{"txid":"abc"}', { status: 200 });
  });

  const request = { method: 'POST', url: 'http://localhost/api/broadcast' };
  let captured;
  const event = {
    request,
    respondWith(p) {
      captured = p;
    },
  };
  worker.listeners.fetch(event);
  assert(!captured, 'fetch handler called respondWith() for a POST request; POST must pass through untouched');
  assert(!networkHit, 'mock fetch should not have been invoked by the worker for a POST (browser handles it directly)');
});

test('a cross-origin GET request is passed through, not cached', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => new Response('should not be reached', { status: 200 }));

  const request = { method: 'GET', url: 'http://evil.example/status' };
  let captured;
  const event = {
    request,
    respondWith(p) {
      captured = p;
    },
  };
  worker.listeners.fetch(event);
  assert(!captured, 'fetch handler intercepted a cross-origin request; it must only handle same-origin GETs');
});

test('app-shell request: successful network response is served and also cached', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => new Response('console.log(1)', { status: 200, headers: { 'Content-Type': 'application/javascript' } }));

  const request = { method: 'GET', url: 'http://localhost/app.js' };
  const response = await dispatchFetch(worker, request);
  assertEqual(response.status, 200, 'network response status was not passed through');
  const body = await response.clone().text();
  assertEqual(body, 'console.log(1)', 'network response body was not passed through');

  const shellCache = await worker.cacheStorage.open('whippet-shell-v1');
  const cached = await shellCache.match(request);
  assert(cached, 'successful app-shell response was not cached for offline fallback');
});

test('app-shell request: offline (network throws) falls back to a previously cached response', async () => {
  const worker = loadWorker();

  // First load: online, populates the cache.
  worker.setFetch(async () => new Response('v1 code', { status: 200 }));
  const request = { method: 'GET', url: 'http://localhost/app.js' };
  await dispatchFetch(worker, request);

  // Second load: offline.
  worker.setFetch(async () => {
    throw new TypeError('network error');
  });
  const response = await dispatchFetch(worker, request);
  const body = await response.clone().text();
  assertEqual(body, 'v1 code', 'offline app-shell request did not fall back to the cached copy');
});

test('app-shell request: offline with nothing cached rejects rather than fabricating a response', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => {
    throw new TypeError('network error');
  });

  const request = { method: 'GET', url: 'http://localhost/never-visited.js' };
  let threw = false;
  try {
    await dispatchFetch(worker, request);
  } catch (e) {
    threw = true;
  }
  assert(threw, 'offline request for an asset with no cached copy should reject, not fabricate a 200');
});

test('API request while online is served fresh with no staleness headers', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => new Response('{"height":100}', { status: 200 }));

  const request = { method: 'GET', url: 'http://localhost/api/status' };
  const response = await dispatchFetch(worker, request);
  assertEqual(response.headers.get('X-Whippet-Stale'), null, 'a live API response must not carry the stale marker');
});

test('API request while offline falls back to cache and is clearly marked stale', async () => {
  const worker = loadWorker();

  // Populate the cache while "online".
  worker.setFetch(async () => new Response('{"height":100}', { status: 200 }));
  const request = { method: 'GET', url: 'http://localhost/api/status' };
  await dispatchFetch(worker, request);

  // Now offline: the same request must come back from cache, explicitly
  // labelled stale so the UI can never present it as a live read.
  worker.setFetch(async () => {
    throw new TypeError('network error');
  });
  const response = await dispatchFetch(worker, request);
  assertEqual(response.headers.get('X-Whippet-Stale'), '1', 'offline API fallback was not marked stale via X-Whippet-Stale');
  assert(response.headers.get('X-Whippet-Cached-At'), 'offline API fallback did not carry a cached-at timestamp');
  const body = await response.clone().text();
  assertEqual(body, '{"height":100}', 'stale fallback body did not match what was cached');
});

test('API request while offline with nothing cached rejects rather than fabricating a balance', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => {
    throw new TypeError('network error');
  });

  const request = { method: 'GET', url: 'http://localhost/api/positions?pubkey=deadbeef' };
  let threw = false;
  try {
    await dispatchFetch(worker, request);
  } catch (e) {
    threw = true;
  }
  assert(threw, 'offline API request with nothing cached should reject rather than inventing a balance');
});

test('unprefixed API paths (/status, /positions, ...) are recognized the same as /api/-prefixed ones', async () => {
  const worker = loadWorker();
  worker.setFetch(async () => new Response('{"height":1}', { status: 200 }));
  await dispatchFetch(worker, { method: 'GET', url: 'http://localhost/status' });

  worker.setFetch(async () => {
    throw new TypeError('network error');
  });
  const response = await dispatchFetch(worker, { method: 'GET', url: 'http://localhost/status' });
  assertEqual(response.headers.get('X-Whippet-Stale'), '1', 'unprefixed /status was not routed through the API (staleness) path');
});

await run();
