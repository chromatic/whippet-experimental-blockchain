// Registers the service worker. This has to live in its own file rather than
// an inline <script> in index.html: the indexer's CSP is `script-src 'self'`
// with no 'unsafe-inline' and no nonce/hash, so an inline registration snippet
// would simply be blocked by the browser and fail silently.
//
// Registration failure here must never block the app from working -- a wallet
// that refuses to run without a service worker would fail hard on browsers
// with SW support disabled (some privacy modes, some embedded webviews), which
// is strictly worse than just not getting the offline/installable niceties.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err) => {
      // Swallow, not throw: see comment above. Surfacing this to the console
      // is enough for developers without disrupting anyone else.
      console.warn('service worker registration failed:', err);
    });
  });

  // sw.js calls skipWaiting(), so a newly deployed worker takes control of
  // this page as soon as it activates. That updates what *future* requests
  // fetch, but it cannot retroactively replace the JavaScript this tab has
  // already parsed and is running -- the old signing and key-derivation code
  // stays live in memory until the document is reloaded. Closing that window
  // is only possible from the page side, which is why it is handled here.
  //
  // A wallet left running on superseded code is exactly what the
  // network-first shell caching exists to prevent, so this reloads rather
  // than merely suggesting it.
  //
  // Whether there was a controller when this script ran is the thing that
  // distinguishes the two cases, and it has to be sampled now rather than
  // inside the handler. On a first visit the page loads uncontrolled and
  // controllerchange fires as soon as the freshly installed worker takes
  // over -- by then `controller` is set, so testing it inside the handler
  // says "yes" in both cases and would turn every first visit into a
  // spurious reload. Nothing is stale on a first visit: the page and the
  // worker came from the same deploy.
  const hadControllerAtLoad = Boolean(navigator.serviceWorker.controller);
  let reloading = false;
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (!hadControllerAtLoad) return; // first install, not an update
    if (reloading) return;            // controllerchange can fire more than once
    reloading = true;
    window.location.reload();
  });
}
