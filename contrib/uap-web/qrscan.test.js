// QR address scanning tests: feature detection, address validation, and
// the camera scan loop (success, retry, invalid data, cancel, and error --
// each checked for whether it leaves the camera stopped).

import * as addr from '../uap-js/addr.js';
import * as secp256k1 from '../uap-js/secp.js';
import {
  parseScannedAddress,
  isScanSupported,
  scanAddressFromCamera
} from './qrscan.js';
import { test, assert, assertEqual, assertThrows, run } from './test-harness.js';

// =============================================================================
// FIXTURES
// =============================================================================

const TEST_PRIVKEY = new Uint8Array(32).fill(0x11);
const TEST_PUBKEY = secp256k1.getPublicKey(TEST_PRIVKEY, true);
const REGTEST_ADDRESS = addr.pubkeyToAddress(TEST_PUBKEY, addr.VERSIONS.regtest.PUBKEY_ADDRESS);
const MAINNET_ADDRESS = addr.pubkeyToAddress(TEST_PUBKEY, addr.VERSIONS.mainnet.PUBKEY_ADDRESS);
// regtest and mainnet deliberately share PUBKEY_ADDRESS (0x49) in addr.js's
// VERSIONS table, so a network-restriction test needs a pair that actually
// differs: testnet (0x71) vs regtest (0x49).
const TESTNET_ADDRESS = addr.pubkeyToAddress(TEST_PUBKEY, addr.VERSIONS.testnet.PUBKEY_ADDRESS);
// P2SH addresses: same 20-byte hash160 payload, SCRIPT_ADDRESS version byte.
// Built with base58checkEncode directly because pubkeyToAddress is (rightly)
// only about pubkey hashes.
const REGTEST_P2SH_ADDRESS = addr.base58checkEncode(
  addr.hash160(TEST_PUBKEY), addr.VERSIONS.regtest.SCRIPT_ADDRESS);
const MAINNET_P2SH_ADDRESS = addr.base58checkEncode(
  addr.hash160(TEST_PUBKEY), addr.VERSIONS.mainnet.SCRIPT_ADDRESS);

// A minimal video-element-shaped mock: scanAddressFromCamera only ever sets
// `.srcObject` and, if present, calls `.play()`.
function makeVideo() {
  return { srcObject: undefined, play: async () => {} };
}

// A MediaStream-shaped mock whose tracks record whether they were stopped,
// so tests can assert the camera was actually released.
function makeStream(trackCount = 1) {
  const tracks = [];
  for (let i = 0; i < trackCount; i++) {
    tracks.push({ stopped: false, stop() { this.stopped = true; } });
  }
  return {
    tracks,
    getTracks() {
      return tracks;
    }
  };
}

// Builds a BarcodeDetector-shaped constructor whose `detect()` calls pop
// one entry off `responses` per call (an array of codes, or a function
// that throws/returns one). Running out of scripted responses means "no
// code found" (empty array), so a test only has to script as many calls
// as it cares about.
function makeDetectorImpl(responses) {
  let i = 0;
  return class FakeDetector {
    constructor(opts) {
      this.opts = opts;
    }
    async detect(video) {
      const entry = i < responses.length ? responses[i] : [];
      i++;
      if (typeof entry === 'function') return entry(video);
      return entry;
    }
  };
}

// =============================================================================
// isScanSupported
// =============================================================================

test('isScanSupported is false with no dependencies', () => {
  assertEqual(isScanSupported(), false, 'no deps at all');
  assertEqual(isScanSupported({}), false, 'empty deps object');
});

test('isScanSupported is false when BarcodeDetector is absent (Safari/Firefox)', () => {
  assertEqual(
    isScanSupported({ getUserMediaImpl: async () => {} }),
    false,
    'getUserMedia alone is not enough -- this is the actual Safari/Firefox case'
  );
});

test('isScanSupported is false when getUserMedia is absent', () => {
  assertEqual(
    isScanSupported({ detectorImpl: makeDetectorImpl([]) }),
    false,
    'BarcodeDetector alone is not enough'
  );
});

test('isScanSupported is true when both are present', () => {
  assertEqual(
    isScanSupported({ detectorImpl: makeDetectorImpl([]), getUserMediaImpl: async () => {} }),
    true
  );
});

// =============================================================================
// parseScannedAddress
// =============================================================================

test('parseScannedAddress accepts a valid bare address', () => {
  const result = parseScannedAddress(REGTEST_ADDRESS, { network: 'regtest' });
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
  assertEqual(result.address, REGTEST_ADDRESS, 'address preserved verbatim');
});

test('parseScannedAddress trims surrounding whitespace', () => {
  const result = parseScannedAddress(`  ${REGTEST_ADDRESS}\n`, { network: 'regtest' });
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
  assertEqual(result.address, REGTEST_ADDRESS);
});

test('parseScannedAddress rejects garbage text', () => {
  const result = parseScannedAddress('not even close to an address', {});
  assertEqual(result.ok, false, 'garbage must not validate');
  assert(typeof result.error === 'string' && result.error.length > 0, 'has an error message');
});

test('parseScannedAddress rejects a non-string payload', () => {
  const result = parseScannedAddress(null, {});
  assertEqual(result.ok, false);
});

test('parseScannedAddress rejects an address with a corrupted checksum', () => {
  // Flip the address's last character; base58checkDecode's checksum
  // validation should catch this the same way it does everywhere else in
  // the app.
  const corrupted = REGTEST_ADDRESS.slice(0, -1) + (REGTEST_ADDRESS.slice(-1) === '1' ? '2' : '1');
  const result = parseScannedAddress(corrupted, {});
  assertEqual(result.ok, false, 'corrupted checksum must not validate');
});

test('parseScannedAddress accepts a whippet: URI and extracts the address', () => {
  const result = parseScannedAddress(`whippet:${REGTEST_ADDRESS}`, { network: 'regtest' });
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
  assertEqual(result.address, REGTEST_ADDRESS);
});

test('parseScannedAddress strips query parameters from a whippet: URI', () => {
  const result = parseScannedAddress(`whippet:${REGTEST_ADDRESS}?amount=5&label=coffee`, { network: 'regtest' });
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
  assertEqual(result.address, REGTEST_ADDRESS);
});

test('parseScannedAddress is case-insensitive about the whippet: scheme', () => {
  const result = parseScannedAddress(`WHIPPET:${REGTEST_ADDRESS}`, { network: 'regtest' });
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
});

test('parseScannedAddress rejects a whippet: URI with nothing after the scheme', () => {
  const result = parseScannedAddress('whippet:', {});
  assertEqual(result.ok, false);
});

test('parseScannedAddress restricts to the requested network', () => {
  const result = parseScannedAddress(TESTNET_ADDRESS, { network: 'regtest' });
  assertEqual(result.ok, false, 'a testnet address must not pass as a regtest address');
});

test('parseScannedAddress accepts any known network when none is specified', () => {
  const result = parseScannedAddress(MAINNET_ADDRESS, {});
  assert(result.ok, `expected ok, got: ${JSON.stringify(result)}`);
});

// A P2SH address is a valid, correctly-checksummed Whippet address. It is
// rejected here not because it is malformed but because nothing in this
// wallet can pay to one: addr.js's addressToScript only emits
// pay-to-pubkey-hash, and encoding a *script* hash into that template would
// create a permanently unspendable output. A scan is the last moment the
// user still has the recipient in front of them, so the refusal belongs
// here rather than three screens later.
test('parseScannedAddress rejects a P2SH address for its own network', () => {
  const result = parseScannedAddress(REGTEST_P2SH_ADDRESS, { network: 'regtest' });
  assertEqual(result.ok, false, 'a P2SH address must not be accepted into the send flow');
  assert(
    typeof result.error === 'string' && /P2SH/.test(result.error),
    `error should name P2SH, got: ${JSON.stringify(result.error)}`
  );
});

test('parseScannedAddress rejects a P2SH address when no network is specified', () => {
  const result = parseScannedAddress(MAINNET_P2SH_ADDRESS, {});
  assertEqual(result.ok, false, 'an unrestricted parse must still reject P2SH');
  assert(
    typeof result.error === 'string' && /P2SH/.test(result.error),
    `error should name P2SH, got: ${JSON.stringify(result.error)}`
  );
});

test('scanAddressFromCamera refuses a scanned P2SH address', async () => {
  const stream = makeStream();
  const detectorImpl = makeDetectorImpl([[{ rawValue: MAINNET_P2SH_ADDRESS }]]);
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => stream,
      detectorImpl,
      network: 'mainnet',
      pollIntervalMs: 1
    }),
    'P2SH'
  );
  assert(stream.tracks[0].stopped, 'camera must be released after refusing a P2SH scan');
});

// =============================================================================
// scanAddressFromCamera -- support & argument checks
// =============================================================================

test('scanAddressFromCamera rejects when unsupported, without ever requesting the camera', async () => {
  let requested = false;
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => { requested = true; return makeStream(); },
      detectorImpl: undefined // absent, as on Safari/Firefox
    }),
    'not supported'
  );
  assertEqual(requested, false, 'getUserMedia must never be called when scanning is unsupported');
});

test('scanAddressFromCamera rejects without a video element', async () => {
  await assertThrows(
    () => scanAddressFromCamera({
      getUserMediaImpl: async () => makeStream(),
      detectorImpl: makeDetectorImpl([])
    }),
    'video element'
  );
});

// =============================================================================
// scanAddressFromCamera -- success
// =============================================================================

test('scanAddressFromCamera resolves with a validated address on a successful scan', async () => {
  const stream = makeStream();
  const video = makeVideo();
  const address = await scanAddressFromCamera({
    video,
    getUserMediaImpl: async () => stream,
    detectorImpl: makeDetectorImpl([[{ rawValue: REGTEST_ADDRESS }]]),
    network: 'regtest',
    pollIntervalMs: 1
  });
  assertEqual(address, REGTEST_ADDRESS);
});

test('scanAddressFromCamera stops every camera track on success', async () => {
  const stream = makeStream(2);
  await scanAddressFromCamera({
    video: makeVideo(),
    getUserMediaImpl: async () => stream,
    detectorImpl: makeDetectorImpl([[{ rawValue: REGTEST_ADDRESS }]]),
    network: 'regtest',
    pollIntervalMs: 1
  });
  for (const t of stream.tracks) {
    assert(t.stopped, 'every track must be stopped after a successful scan');
  }
});

test('scanAddressFromCamera clears video.srcObject when it settles', async () => {
  const video = makeVideo();
  await scanAddressFromCamera({
    video,
    getUserMediaImpl: async () => makeStream(),
    detectorImpl: makeDetectorImpl([[{ rawValue: REGTEST_ADDRESS }]]),
    network: 'regtest',
    pollIntervalMs: 1
  });
  assertEqual(video.srcObject, null, 'srcObject must be released, not left pointing at the stream');
});

test('scanAddressFromCamera retries until a code is found', async () => {
  const address = await scanAddressFromCamera({
    video: makeVideo(),
    getUserMediaImpl: async () => makeStream(),
    // First two attempts find nothing, third finds the code.
    detectorImpl: makeDetectorImpl([[], [], [{ rawValue: REGTEST_ADDRESS }]]),
    network: 'regtest',
    pollIntervalMs: 1
  });
  assertEqual(address, REGTEST_ADDRESS);
});

// =============================================================================
// scanAddressFromCamera -- invalid scanned data
// =============================================================================

test('scanAddressFromCamera rejects an invalid scanned string instead of resolving', async () => {
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => makeStream(),
      detectorImpl: makeDetectorImpl([[{ rawValue: 'this is not an address' }]]),
      pollIntervalMs: 1
    }),
    'not a valid Whippet address'
  );
});

test('scanAddressFromCamera stops the camera even when the scanned string is invalid', async () => {
  const stream = makeStream();
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => stream,
      detectorImpl: makeDetectorImpl([[{ rawValue: 'garbage' }]]),
      pollIntervalMs: 1
    }),
    'not a valid Whippet address'
  );
  assert(stream.tracks[0].stopped, 'camera must be released even though the scan produced no usable address');
});

// =============================================================================
// scanAddressFromCamera -- cancel
// =============================================================================

test('scanAddressFromCamera rejects with AbortError when cancelled', async () => {
  const controller = new AbortController();
  // Abort as soon as the first (empty) detect attempt has happened, i.e.
  // while the loop is waiting between polls -- the same moment a user's
  // Cancel tap would land.
  const detectorImpl = makeDetectorImpl([
    () => { controller.abort(); return []; }
  ]);
  let caught = null;
  try {
    await scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => makeStream(),
      detectorImpl,
      pollIntervalMs: 20,
      signal: controller.signal
    });
  } catch (e) {
    caught = e;
  }
  assert(caught, 'expected the scan to reject after cancellation');
  assertEqual(caught.name, 'AbortError', `expected AbortError, got ${caught.name}: ${caught.message}`);
});

test('scanAddressFromCamera stops the camera on cancel', async () => {
  const stream = makeStream();
  const controller = new AbortController();
  const detectorImpl = makeDetectorImpl([
    () => { controller.abort(); return []; }
  ]);
  try {
    await scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => stream,
      detectorImpl,
      pollIntervalMs: 20,
      signal: controller.signal
    });
  } catch (e) {
    // expected -- assertions are below regardless of outcome
  }
  assert(stream.tracks[0].stopped, 'camera must be released when the user cancels');
});

test('scanAddressFromCamera rejects immediately if already cancelled before it starts', async () => {
  const controller = new AbortController();
  controller.abort();
  let requested = false;
  let caught = null;
  try {
    await scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => { requested = true; return makeStream(); },
      detectorImpl: makeDetectorImpl([]),
      signal: controller.signal
    });
  } catch (e) {
    caught = e;
  }
  assert(caught, 'expected a pre-cancelled scan to reject');
  assertEqual(caught.name, 'AbortError', `expected AbortError, got ${caught.name}: ${caught.message}`);
  assertEqual(requested, false, 'a pre-cancelled scan must never request the camera at all');
});

// =============================================================================
// scanAddressFromCamera -- errors (not cancellation)
// =============================================================================

test('scanAddressFromCamera propagates a getUserMedia failure (e.g. permission denied)', async () => {
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => { throw new Error('Permission denied'); },
      detectorImpl: makeDetectorImpl([])
    }),
    'Permission denied'
  );
});

test('scanAddressFromCamera stops the camera when the detector itself throws mid-scan', async () => {
  const stream = makeStream();
  const detectorImpl = makeDetectorImpl([
    [],
    () => { throw new Error('detector crashed'); }
  ]);
  await assertThrows(
    () => scanAddressFromCamera({
      video: makeVideo(),
      getUserMediaImpl: async () => stream,
      detectorImpl,
      pollIntervalMs: 1
    }),
    'detector crashed'
  );
  assert(stream.tracks[0].stopped, 'camera must be released even when the detector errors out mid-scan');
});

run({ style: 'compact' });
