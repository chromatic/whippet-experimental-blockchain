#!/usr/bin/env node
// Verify that every ES module import specifier reachable from web/ resolves
// against what is actually present in web/.
//
// The frontend under ../uap-web is written against its own directory layout:
// it imports '../uap-js/*.js', a sibling directory in the source tree. The
// Makefile's `web` target flattens those sources into web/ for //go:embed, so
// nothing guarantees the specifiers still resolve once the assets are served
// over HTTP. This script checks that they do.
//
// It also fails any bare specifier with no import map entry, which is not
// pedantry: uap-web used to import '@noble/secp256k1' bare. Node resolved it
// through node_modules and every test passed, but a browser cannot resolve a
// bare specifier at all, and this service's `script-src 'self'` CSP rules out
// the inline import map that would fix it. The library is vendored into
// uap-js instead. Anything reintroducing a bare specifier is unloadable.
//
// Usage: node check-web-imports.mjs [webdir]   (default: ./web)
// Exits non-zero if any specifier fails to resolve.

import { readdirSync, readFileSync, statSync, existsSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

const webDir = resolve(process.argv[2] || 'web');

if (!existsSync(webDir)) {
  console.error(`✗ ${webDir} does not exist -- run 'make web' first`);
  process.exit(1);
}

// Recursively list every file under dir, as absolute paths.
function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}

const files = walk(webDir);
const jsFiles = files.filter((f) => f.endsWith('.js'));
const htmlFiles = files.filter((f) => f.endsWith('.html'));

// Collect import-map entries declared in any HTML file. Browsers resolve bare
// specifiers only through an import map, so a bare specifier with no map entry
// is a hard failure in a browser even though node resolves it fine.
const importMap = { imports: {}, sources: [] };
for (const html of htmlFiles) {
  const src = readFileSync(html, 'utf8');
  const re = /<script[^>]*type\s*=\s*["']importmap["'][^>]*>([\s\S]*?)<\/script>/gi;
  let m;
  while ((m = re.exec(src)) !== null) {
    try {
      const parsed = JSON.parse(m[1]);
      Object.assign(importMap.imports, parsed.imports || {});
      importMap.sources.push(relative(webDir, html));
    } catch (e) {
      console.error(`✗ ${relative(webDir, html)}: malformed import map: ${e.message}`);
      process.exitCode = 1;
    }
  }
}

// Extract static import/export-from specifiers plus dynamic import('...').
// Deliberately simple: these are hand-written source modules, not minified
// bundles, and every real specifier in them sits on its own statement.
function specifiersOf(src) {
  const specs = [];
  const patterns = [
    /(?:^|\n)\s*import\s+[^'";]*?from\s*['"]([^'"]+)['"]/g, // import x from 'y'
    /(?:^|\n)\s*import\s*['"]([^'"]+)['"]/g, // side-effect import 'y'
    /(?:^|\n)\s*export\s+[^'";]*?from\s*['"]([^'"]+)['"]/g, // export ... from 'y'
    /\bimport\s*\(\s*['"]([^'"]+)['"]\s*\)/g, // dynamic import('y')
  ];
  for (const re of patterns) {
    let m;
    while ((m = re.exec(src)) !== null) specs.push(m[1]);
  }
  return specs;
}

const results = [];
for (const file of jsFiles) {
  const rel = relative(webDir, file);
  for (const spec of specifiersOf(readFileSync(file, 'utf8'))) {
    if (spec.startsWith('./') || spec.startsWith('../') || spec.startsWith('/')) {
      // Relative/absolute: resolve the way a *browser* does -- against the URL
      // of the importing module, not against the filesystem. This matters for
      // the '../uap-js/...' specifiers: URL resolution clamps leading '..' at
      // the document root, so '../uap-js/uap.js' imported by /app.js requests
      // /uap-js/uap.js, which serveWeb() maps back to web/uap-js/uap.js.
      const url = new URL(spec, 'http://localhost/' + rel);
      const urlPath = decodeURIComponent(url.pathname).replace(/^\//, '');
      const target = join(webDir, urlPath);
      const ok = existsSync(target);
      results.push({
        file: rel,
        spec,
        ok,
        detail: ok ? `/${urlPath}` : `missing /${urlPath}`,
      });
    } else {
      // Bare specifier: only resolvable through an import map.
      const mapped =
        importMap.imports[spec] ||
        Object.entries(importMap.imports)
          .filter(([k]) => k.endsWith('/') && spec.startsWith(k))
          .map(([k, v]) => v + spec.slice(k.length))[0];
      if (!mapped) {
        results.push({ file: rel, spec, ok: false, detail: 'bare specifier, no import map entry' });
        continue;
      }
      // The map may point outside web/ (a CDN URL); only same-origin relative
      // targets can be checked, and those must exist.
      if (/^[a-z]+:\/\//i.test(mapped)) {
        results.push({ file: rel, spec, ok: true, detail: `import map -> ${mapped} (external)` });
        continue;
      }
      const target = join(webDir, mapped.replace(/^\.?\//, ''));
      const ok = existsSync(target);
      results.push({
        file: rel,
        spec,
        ok,
        detail: ok ? `import map -> ${mapped}` : `import map -> ${mapped} (missing)`,
      });
    }
  }
}

results.sort((a, b) => a.file.localeCompare(b.file) || a.spec.localeCompare(b.spec));

console.log(`Checking module imports under ${webDir}`);
console.log(`  ${jsFiles.length} JS file(s), ${htmlFiles.length} HTML file(s)`);
if (importMap.sources.length) {
  console.log(`  import map from: ${importMap.sources.join(', ')}`);
} else {
  console.log('  import map: none');
}
console.log('');

let failed = 0;
for (const r of results) {
  if (!r.ok) failed++;
  console.log(`  ${r.ok ? 'OK  ' : 'FAIL'}  ${r.file} -> '${r.spec}'  [${r.detail}]`);
}

console.log('');
if (failed) {
  console.log(`✗ ${failed} of ${results.length} import specifier(s) do not resolve`);
  process.exit(1);
}
console.log(`✓ all ${results.length} import specifier(s) resolve`);
