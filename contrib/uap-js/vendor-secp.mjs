#!/usr/bin/env node
// vendor-secp.mjs -- copy the installed @noble/secp256k1 ESM build into
// noble-secp256k1.js, stamping a provenance header on top.
//
// Run after changing the "@noble/secp256k1" devDependency:
//
//   cd contrib/uap-js && npm install && npm run vendor:secp
//
// This is a copy, not a build: the only transformation is the header. See
// noble-secp256k1.js for why the library is vendored at all.

import { readFileSync, writeFileSync } from 'node:fs';

const PKG = new URL('./node_modules/@noble/secp256k1/package.json', import.meta.url);
const SRC = new URL('./node_modules/@noble/secp256k1/index.js', import.meta.url);
const DEST = new URL('./noble-secp256k1.js', import.meta.url);

const version = JSON.parse(readFileSync(PKG, 'utf8')).version;
if (!version.startsWith('2.')) {
  console.error(
    `refusing to vendor @noble/secp256k1 v${version}: uap-js/secp.js is written ` +
      'against the 2.x API (sign() returning a Signature, etc.hmacSha256Sync, ' +
      'no built-in DER). Update secp.js first.'
  );
  process.exit(1);
}

const header = [
  '// noble-secp256k1.js -- VENDORED FILE, DO NOT EDIT BY HAND.',
  '//',
  `// Verbatim copy of @noble/secp256k1 v${version}, taken from`,
  '//   contrib/uap-js/node_modules/@noble/secp256k1/index.js',
  '// (the package\'s single-file, browser-native ESM build).',
  '//',
  '// Regenerate with:  cd contrib/uap-js && npm run vendor:secp',
  '// The version above comes from the devDependency in package.json; bump that,',
  '// npm install, re-run the script, and re-run `npm test` (secp-equivalence.test.js',
  '// pins the exact signature bytes this library must keep producing).',
  '//',
  '// WHY VENDORED: the frontend is served as plain ES modules by uap-indexer under',
  '// a `script-src \'self\'` CSP. A browser cannot resolve the bare specifier',
  '// \'@noble/secp256k1\' without an import map, and the CSP forbids an inline one,',
  '// so the library has to exist as a real file next to the code that imports it.',
  '// uap-indexer\'s `make web` copies contrib/uap-js/*.js into the embedded web/',
  '// tree, which is how this file reaches the browser.',
  '//',
  '// Import it through ./secp.js, not directly -- secp.js adds the DER encoding',
  '// that the 2.x line dropped and that Whippet scriptSigs require.',
  '',
].join('\n');

writeFileSync(DEST, header + readFileSync(SRC, 'utf8'));
console.log(`vendored @noble/secp256k1 v${version} -> uap-js/noble-secp256k1.js`);
