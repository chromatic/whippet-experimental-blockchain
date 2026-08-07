// Shared mini test harness for the uap-web suites.
//
// Every *.test.js file used to carry a byte-for-byte copy of this: the
// counters, the `pending` queue, the assertion helpers and the end-of-file
// runner. They now import it from here. Output format is unchanged.
//
// Each test file is its own Node process (see the `test` script in
// package.json), so the module-level counters below are per-suite state and
// never shared between suites.

let testCount = 0;
let passCount = 0;
let failCount = 0;

// Every test is queued and awaited by run() at the bottom of the test file.
// Calling fn() without awaiting it would make every async test vacuous: the
// promise's rejection would never be observed, so an async test could only
// ever "pass".
const pending = [];

export function test(name, fn) {
  testCount++;
  pending.push(async () => {
    try {
      await fn();
      passCount++;
      console.log(`✓ ${name}`);
    } catch (e) {
      failCount++;
      console.log(`✗ ${name}`);
      console.log(`  ${e.message}`);
    }
  });
}

export function assert(condition, message) {
  if (!condition) throw new Error(message);
}

export function assertEqual(actual, expected, message) {
  if (actual !== expected) {
    throw new Error(`${message}\n  Expected: ${expected}\n  Actual: ${actual}`);
  }
}

export function assertDeepEqual(actual, expected, message) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${message}\n  Expected: ${JSON.stringify(expected)}\n  Actual: ${JSON.stringify(actual)}`);
  }
}

export function assertArrayEquals(actual, expected, message) {
  if (actual.length !== expected.length) {
    throw new Error(`${message}: length mismatch (${actual.length} vs ${expected.length})`);
  }
  for (let i = 0; i < actual.length; i++) {
    if (actual[i] !== expected[i]) {
      throw new Error(`${message}: byte ${i} mismatch (${actual[i]} vs ${expected[i]})`);
    }
  }
}

// Awaits fn and requires it to reject/throw with a message containing
// messageSnippet.
//
// Two things here are deliberate, and both were bugs in the first version.
// (1) It awaits: an async fn that merely returns a rejected promise throws
//     nothing synchronously, so a non-awaiting version passes whatever the
//     code does. (2) The "nothing was thrown" sentinel is raised OUTSIDE the
//     try block. Raising it inside meant its own catch caught it -- and since
//     that message quotes messageSnippet, the containment check passed. Every
//     assertThrows in the file silently succeeded, whatever the code did.
export async function assertThrows(fn, messageSnippet) {
  let caught = null;
  let threw = false;
  try {
    await fn();
  } catch (e) {
    threw = true;
    caught = e;
  }
  if (!threw) {
    throw new Error(`Expected a throw whose message contains "${messageSnippet}", but nothing was thrown`);
  }
  const msg = (caught && caught.message) || String(caught);
  if (!msg.includes(messageSnippet)) {
    throw new Error(`Expected the error message to contain "${messageSnippet}", but got: ${msg}`);
  }
}

// Runs the queued tests and prints the summary.
//
// The suites did not all summarise the same way, so the two existing formats
// are kept verbatim rather than merged:
//   'divider' (wallet, dom, api) - rule line, "Tests: N/M passed", explicit
//                                  process.exit(0) on success.
//   'compact' (mint, market)     - "N/M tests passed", no explicit exit on
//                                  success.
// Both exit non-zero when any test failed.
export async function run({ banner = null, style = 'divider' } = {}) {
  if (banner !== null) console.log(banner);

  for (const t of pending) await t();

  if (style === 'compact') {
    console.log(`\n${passCount}/${testCount} tests passed`);
    if (failCount > 0) {
      console.log(`${failCount} test(s) failed`);
      process.exit(1);
    }
    return;
  }

  console.log(`\n${'='.repeat(60)}`);
  console.log(`Tests: ${passCount}/${testCount} passed`);
  if (failCount > 0) {
    console.log(`Failed: ${failCount}`);
    process.exit(1);
  } else {
    console.log('All tests passed!');
    process.exit(0);
  }
}
