// QR code scanning for addresses (Phase 7 of the marketplace plan).
//
// This module never reaches for `BarcodeDetector` or
// `navigator.mediaDevices` directly. Both are absent on Safari/iOS and on
// Firefox, so every call site must feature-detect before offering the
// scan button at all, and -- exactly like `fetchImpl` in api.js -- both
// come in as injected dependencies so tests can drive the whole scan flow
// (including error and cancel paths) without a real camera or a browser
// that implements the Shape Detection API.
//
// This module also does not touch `document`: the caller (app.js) builds
// whatever <video> element it needs via dom.js and hands it in, so this
// file has no DOM dependency of its own and its logic -- validation,
// the scan loop, and camera teardown -- can be unit tested with plain
// object mocks.

import { base58checkDecode, VERSIONS } from '../uap-js/addr.js';

// Recognized address version bytes, flattened once so parseScannedAddress
// doesn't rebuild this on every call. Keyed by network name so a caller
// that knows which network it's on (the wallet always does) can restrict
// to just that network's bytes, rather than accepting e.g. a testnet
// address into a mainnet send flow just because it happens to decode.
//
// PUBKEY_ADDRESS only -- SCRIPT_ADDRESS (P2SH) is deliberately NOT accepted.
// Nothing in this wallet can construct a spendable P2SH output:
// addr.js's addressToScript only emits pay-to-pubkey-hash, and it now
// throws rather than silently encoding a script hash as a pubkey hash
// (which would produce permanently unspendable outputs). Accepting a P2SH
// address here would mean validating an address the send flow cannot
// honour -- better to say so at the scan, where the user still has the
// recipient in front of them.
const VERSIONS_BY_NETWORK = VERSIONS;

function knownVersionBytes(network) {
  if (network) {
    const v = VERSIONS_BY_NETWORK[network];
    if (!v) return [];
    return [v.PUBKEY_ADDRESS];
  }
  const out = [];
  for (const v of Object.values(VERSIONS_BY_NETWORK)) {
    out.push(v.PUBKEY_ADDRESS);
  }
  return out;
}

// Whippet registers a `whippet:` URI scheme for desktop integration (see
// contrib/debian/README.md's "whippet: URI support"), modelled on BIP21
// `bitcoin:` URIs: scheme, then an address, then an optional
// `?amount=...&label=...` query string. This app has no BIP21 query
// parser anywhere -- there is no amount/label prefill in the send flow --
// so rather than reject a `whippet:` QR code outright (which a wallet's
// own "receive" screen might plausibly produce), a `whippet:` prefix is
// stripped and anything from the first `?` on is discarded, leaving just
// the address to validate normally. This is a deliberate, narrow choice:
// it only ever narrows a URI down to a bare address, never widens a bare
// address into anything else, and it does nothing with amount/label even
// if present.
const WHIPPET_URI_PREFIX = /^whippet:/i;

/**
 * Validate a string scanned from a QR code as a Whippet address.
 *
 * A scanned string is untrusted input -- it comes from whatever a camera
 * happened to be pointed at -- so this never assumes it looks like an
 * address. Reuses addr.js's base58checkDecode, the same checksum-validating
 * decode the rest of the app already relies on, rather than reimplementing
 * any of it here.
 *
 * @param {string} raw - the raw string decoded from the QR code
 * @param {Object} [opts]
 * @param {'mainnet'|'testnet'|'regtest'} [opts.network] - restrict to this
 *   network's address versions; if omitted, any known network is accepted
 * @returns {{ok: true, address: string} | {ok: false, error: string}}
 */
export function parseScannedAddress(raw, { network } = {}) {
  if (typeof raw !== 'string') {
    return { ok: false, error: 'Scanned code did not contain text' };
  }

  let candidate = raw.trim();

  if (WHIPPET_URI_PREFIX.test(candidate)) {
    candidate = candidate.replace(WHIPPET_URI_PREFIX, '');
    const queryIdx = candidate.indexOf('?');
    if (queryIdx !== -1) candidate = candidate.slice(0, queryIdx);
    candidate = candidate.trim();
  }

  if (!candidate) {
    return { ok: false, error: 'No address found in scanned code' };
  }

  let decoded;
  try {
    decoded = base58checkDecode(candidate);
  } catch (e) {
    return { ok: false, error: 'Scanned code is not a valid Whippet address' };
  }

  const allowed = knownVersionBytes(network);
  if (!allowed.includes(decoded.version)) {
    // Name the P2SH case rather than lumping it in with "unrecognized":
    // the address is perfectly valid, this wallet just cannot pay to it,
    // and a user staring at a good address needs to be told which of
    // those two things went wrong.
    const isScriptAddress = Object.values(VERSIONS_BY_NETWORK)
      .some((v) => v.SCRIPT_ADDRESS === decoded.version);
    if (isScriptAddress) {
      return {
        ok: false,
        error: 'P2SH addresses are not supported by this wallet; ask for a regular (pay-to-pubkey-hash) address'
      };
    }
    return {
      ok: false,
      error: network
        ? `Scanned address is not a valid ${network} address`
        : 'Scanned address has an unrecognized version byte'
    };
  }

  return { ok: true, address: candidate };
}

/**
 * Report whether QR scanning can actually work in this environment.
 *
 * Both the Shape Detection API (`BarcodeDetector`) and camera access
 * (`getUserMedia`) are missing on Safari/iOS and Firefox at the time this
 * was written, so this must be checked before a "Scan QR" button is ever
 * shown -- there must never be a button that, once tapped, can only fail.
 * Manual address entry has to remain the fully-working path when this
 * returns false.
 *
 * @param {Object} deps
 * @param {Function} [deps.detectorImpl] - a BarcodeDetector-shaped constructor
 * @param {Function} [deps.getUserMediaImpl] - a getUserMedia-shaped function
 * @returns {boolean}
 */
export function isScanSupported({ detectorImpl, getUserMediaImpl } = {}) {
  return typeof detectorImpl === 'function' && typeof getUserMediaImpl === 'function';
}

function makeAbortError() {
  // DOMException exists in browsers and in modern Node; fall back to a
  // plain Error with the same `.name` so `e.name === 'AbortError'` checks
  // work either way.
  if (typeof DOMException === 'function') {
    return new DOMException('Scan cancelled', 'AbortError');
  }
  const e = new Error('Scan cancelled');
  e.name = 'AbortError';
  return e;
}

// Waits `ms` milliseconds, or rejects immediately (or as soon as `signal`
// fires) with an AbortError. This is what lets a Cancel button interrupt
// the scan loop between poll attempts instead of only being checked at the
// top of the loop, which would leave a Cancel tap waiting out the full
// poll interval before doing anything.
function wait(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal && signal.aborted) {
      reject(makeAbortError());
      return;
    }
    const timer = setTimeout(() => {
      cleanup();
      resolve();
    }, ms);
    function onAbort() {
      cleanup();
      reject(makeAbortError());
    }
    function cleanup() {
      clearTimeout(timer);
      if (signal) signal.removeEventListener('abort', onAbort);
    }
    if (signal) signal.addEventListener('abort', onAbort);
  });
}

/**
 * Open the camera, poll it for a QR code, and resolve with a validated
 * address.
 *
 * Camera access is a permission prompt and a privacy cost, so this must
 * only be called in direct response to a user action (e.g. tapping "Scan
 * QR"), never from page load or a render function. Every exit path --
 * success, cancellation via `signal`, or any error including an invalid
 * scanned string -- stops every track on the acquired stream before this
 * function settles; the `finally` below is what guarantees that, so a
 * caller can never observe this function returning without the camera
 * light going back off.
 *
 * @param {Object} opts
 * @param {Object} opts.video - a video-element-shaped object: this
 *   function sets `.srcObject` and, if present, calls `.play()`. The
 *   caller owns creating and (dis)playing it; this module never touches
 *   `document`.
 * @param {Function} opts.getUserMediaImpl - getUserMedia-shaped function,
 *   e.g. `navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices)`
 * @param {Function} opts.detectorImpl - BarcodeDetector-shaped constructor
 * @param {'mainnet'|'testnet'|'regtest'} [opts.network] - passed through to
 *   parseScannedAddress
 * @param {AbortSignal} [opts.signal] - abort to cancel an in-progress scan
 * @param {number} [opts.pollIntervalMs=250] - delay between detect attempts
 * @param {string[]} [opts.formats=['qr_code']] - barcode formats to request
 * @returns {Promise<string>} the validated address
 */
export async function scanAddressFromCamera({
  video,
  getUserMediaImpl,
  detectorImpl,
  network,
  signal,
  pollIntervalMs = 250,
  formats = ['qr_code']
} = {}) {
  if (!video) throw new Error('scanAddressFromCamera requires a video element');
  if (!isScanSupported({ detectorImpl, getUserMediaImpl })) {
    throw new Error('QR scanning is not supported in this environment');
  }

  if (signal && signal.aborted) throw makeAbortError();

  let stream = null;
  try {
    stream = await getUserMediaImpl({ video: { facingMode: 'environment' } });

    if (signal && signal.aborted) throw makeAbortError();

    video.srcObject = stream;
    if (typeof video.play === 'function') {
      await video.play();
    }

    const detector = new detectorImpl({ formats });

    // eslint-disable-next-line no-constant-condition
    while (true) {
      if (signal && signal.aborted) throw makeAbortError();

      const codes = await detector.detect(video);
      if (codes && codes.length > 0) {
        const parsed = parseScannedAddress(codes[0].rawValue, { network });
        if (!parsed.ok) {
          throw new Error(parsed.error);
        }
        return parsed.address;
      }

      await wait(pollIntervalMs, signal);
    }
  } finally {
    // This runs on every exit -- return, throw, or an awaited rejection --
    // so success, cancel, and error all stop the camera the same way. A
    // wallet that leaves the camera light on after the user cancels or a
    // scan errors out is a serious bug, not a cosmetic one.
    if (stream && typeof stream.getTracks === 'function') {
      for (const track of stream.getTracks()) {
        track.stop();
      }
    }
    if (video) {
      video.srcObject = null;
    }
  }
}
