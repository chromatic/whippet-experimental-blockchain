// Service worker for the Whippet wallet PWA.
//
// This is a *self-custody wallet*: app.js/wallet.js/uap-js are the code that
// derives keys and signs transactions. A service worker that serves those
// files stale is not a caching tradeoff, it is serving stale signing code --
// a correctness/security bug, not a performance knob. Every choice below is
// made with that in mind, in preference to whatever would make the "offline"
// story marginally smoother.
//
// Not a module: registered as a classic script (see sw-register.js), and it
// has no static imports of its own, so check-web-imports.mjs -- which only
// walks import/export specifiers -- has nothing to resolve here and nothing
// to break.

// Bump this string on every deploy that changes cached content. It is the
// only thing that makes old caches deletable: caches.delete needs a name,
// and "the cache from before this worker updated" has no other identity to
// delete by. Runtime-caching means we never precache a fixed asset list here
// (see handleAppShellRequest below), so bumping this is optional for
// individual file edits, but keeping the version live in the byte content of
// this file is what makes THIS file itself update promptly: browsers
// byte-compare a service worker script against its previous version on their
// periodic recheck, and a changed CACHE_VERSION guarantees this file is never
// byte-identical across a real content change.
const CACHE_VERSION = 'v1';
const APP_SHELL_CACHE = `whippet-shell-${CACHE_VERSION}`;
const API_CACHE = `whippet-api-${CACHE_VERSION}`;
const KNOWN_CACHES = new Set([APP_SHELL_CACHE, API_CACHE]);

// Mirrors the route table api.go registers on both `mux` (unprefixed) and
// `/api/...` (prefixed) -- see api.go's calls to mux.HandleFunc. Anything
// matching this is a read against live chain/mempool state (balances,
// UTXOs, order book, fee estimates) or the broadcast endpoint, never static
// app code, so it is handled by handleApiRequest's "never present as fresh
// when offline" path instead of the app-shell path.
const API_PATH_RE = /^\/(api\/)?(status|positions|position\/|utxos|tokens|token\/|orders|orders\/|broadcast|feerate)/;

self.addEventListener('install', () => {
  // Take over from any previous worker as soon as this one finishes
  // installing, rather than waiting for every open tab to close. The
  // alternative (the default lifecycle) leaves an old worker -- and
  // therefore its network-first-but-still-cached view of app code -- in
  // charge until the user closes every tab, which is a worse position for
  // a wallet than getting the new worker active immediately. It does not
  // retroactively patch JS a tab already has loaded in memory; see
  // sw-register.js and the report for the remaining gap and what closing
  // it needs on the app.js side.
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      // Delete every cache this worker did not just declare. Without this,
      // caches from old CACHE_VERSION strings accumulate forever (the Cache
      // Storage API has no TTL or LRU of its own) and, worse, a bug that
      // reverted CACHE_VERSION could resurrect an old cache full of stale
      // signing code that a stray cache.match() might still hit.
      const names = await caches.keys();
      await Promise.all(
        names.filter((n) => !KNOWN_CACHES.has(n)).map((n) => caches.delete(n))
      );
      await self.clients.claim();
    })()
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;

  // Only GET is ever intercepted. POST /api/broadcast (and any other
  // mutating call) must reach the network every time with no cache
  // involvement in either direction: caching a POST response would mean
  // potentially replaying a stale broadcast result, and there is nothing
  // useful an offline fallback could return for "submit this transaction"
  // anyway.
  if (req.method !== 'GET') {
    return;
  }

  const url = new URL(req.url);

  // Leave cross-origin requests alone entirely. The CSP already restricts
  // where the page's own script/style can come from, but the fetch handler
  // is a second place a same-origin service worker could end up touching
  // third-party traffic (e.g. if the API base URL is ever pointed at a
  // different host, per the plan's "configurable API base URL" note for
  // multi-instance deployments) -- and opaque cross-origin responses can't
  // be inspected for .ok, so blanket-caching them would be caching unknown
  // content under this origin's storage quota. Those requests just pass
  // straight through, uncached, offline fallback or not.
  if (url.origin !== self.location.origin) {
    return;
  }

  if (API_PATH_RE.test(url.pathname)) {
    event.respondWith(handleApiRequest(req));
  } else {
    event.respondWith(handleAppShellRequest(req));
  }
});

// App code (HTML/CSS/JS, the manifest, icons): network-first. The network is
// tried on every single load, not just the first one, so a fix to signing
// code reaches the user the moment they have connectivity -- there is no
// window where "it's in the cache" quietly overrides "it's on the server".
// The cache is populated purely as a side effect of successful fetches and
// is consulted only when the network request itself fails, i.e. only when
// truly offline. That is the offline-fallback shape the task calls for,
// as opposed to stale-while-revalidate, which would hand back last time's
// code immediately and correct it only for the load after that -- one
// extra load's worth of possibly-stale signing code that network-first
// does not accept.
async function handleAppShellRequest(req) {
  try {
    const fresh = await fetch(req);
    if (fresh.ok) {
      const cache = await caches.open(APP_SHELL_CACHE);
      // Fire-and-forget from the response's point of view: cache.put takes
      // its own copy, so awaiting it before returning would only delay the
      // response to the page for no benefit. Not awaited deliberately.
      cache.put(req, fresh.clone());
    }
    return fresh;
  } catch (err) {
    const cache = await caches.open(APP_SHELL_CACHE);
    const cached = await cache.match(req);
    if (cached) {
      return cached;
    }
    // No network and nothing cached (e.g. first-ever visit happened
    // offline): let the failure surface rather than manufacturing a
    // response, since there is nothing honest to serve.
    throw err;
  }
}

// API reads (balances, positions, utxos, order book, fee estimate):
// network-first for the same reason as app code, but on falling back to
// cache the response is never handed back looking like a fresh read. It is
// re-wrapped with X-Whippet-Stale and X-Whippet-Cached-At headers so the
// caller can tell the difference between "this is your balance right now"
// and "this was your balance as of <time>, and we're offline" -- the two
// must never be presented as the same thing to a wallet user. This worker
// only sets the headers; app.js/dom.js need to read them and label the UI
// accordingly (see the report -- those files are out of scope here).
async function handleApiRequest(req) {
  const cache = await caches.open(API_CACHE);
  try {
    const fresh = await fetch(req);
    if (fresh.ok) {
      await cacheStampedResponse(cache, req, fresh);
    }
    return fresh;
  } catch (err) {
    const stale = await readStampedResponse(cache, req);
    if (stale) {
      return stale;
    }
    throw err;
  }
}

// Stores a copy of `response` in `cache` with a baked-in X-Whippet-Cached-At
// timestamp, recorded at write time. Response headers are immutable once
// constructed, so getting a timestamp onto the cached copy means building a
// new Response around the same body rather than mutating the one already in
// hand -- and doing it now, once, is cheaper than re-deriving "when was this
// cached" out of thin air later on every offline read.
async function cacheStampedResponse(cache, request, response) {
  const body = await response.clone().arrayBuffer();
  const headers = new Headers(response.headers);
  headers.set('X-Whippet-Cached-At', new Date().toISOString());
  const stamped = new Response(body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
  await cache.put(request, stamped);
}

// Reads a previously-stamped response back out of `cache`, if present, and
// adds X-Whippet-Stale so the caller can never mistake it for a live read.
// This is the only place that header is set: it is added at serve time, not
// cache time, because the same cached bytes are perfectly fresh data right
// up until the moment they are the only thing left to answer with.
async function readStampedResponse(cache, request) {
  const cached = await cache.match(request);
  if (!cached) {
    return null;
  }
  const body = await cached.clone().arrayBuffer();
  const headers = new Headers(cached.headers);
  headers.set('X-Whippet-Stale', '1');
  return new Response(body, {
    status: cached.status,
    statusText: cached.statusText,
    headers,
  });
}

// No key material ever crosses this boundary to check for. wallet.js keeps
// the mnemonic and private key in memory only (see its "Persistence rule"
// comment) and app.js's Wallet is constructed with a plain in-memory Map as
// storage -- nothing sensitive is written to localStorage/IndexedDB for a
// service worker or its Cache Storage entries to ever pick up, and the only
// thing this worker sends over the network unmodified is already-signed,
// public transaction hex via POST, which is excluded above by the
// GET-only check.
