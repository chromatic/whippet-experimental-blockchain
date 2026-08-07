// Shared byte/hex conversion helpers for the test files.
// These were previously copy-pasted, identically, into sha256.test.js,
// ripemd160.test.js and addr.test.js.

function hex(bytes) {
  return Buffer.from(bytes).toString('hex');
}

function fromHex(s) {
  return new Uint8Array(Buffer.from(s, 'hex'));
}

function fromUtf8(s) {
  return new Uint8Array(Buffer.from(s, 'utf8'));
}

export { hex, fromHex, fromUtf8 };
