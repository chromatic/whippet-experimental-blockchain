// test-node-harness.js -- spin up an isolated, disposable `whippetd`
// regtest instance for integration tests, and talk to it over JSON-RPC.
//
// This is test-only glue, not part of the published uap.js library. It is
// deliberately conservative about isolation: every instance gets its own
// freshly created temp datadir, its own non-default ports, and its own
// rpcuser/rpcpassword, and is only ever started with -regtest. See
// integration.test.js for the safety rationale.
//
// RPC transport: plain JSON-RPC over HTTP via fetch (not shelling out to
// whippet-cli for every call). This is chosen because (a) it gives
// structured JSON errors (including the node's exact rejection reason for
// negative-path tests) instead of parsing whippet-cli's text/exit-code
// output, and (b) it avoids spawning a fresh process for every single RPC
// call in tests that make many of them.

import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Default to the binaries of the checkout this harness actually lives in:
// contrib/uap-js/ -> repo root -> src/. The previous default was an
// absolute path into a different working tree, which meant these tests
// silently exercised whatever node that path happened to hold rather than
// the source they sit beside -- an integration suite for a consensus patch
// can pass or fail for reasons having nothing to do with the code under it.
// Resolving relative to import.meta.url keeps the two in step.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const WHIPPETD = process.env.WHIPPETD_BIN || path.join(REPO_ROOT, 'src', 'whippetd');
const WHIPPET_CLI = process.env.WHIPPET_CLI_BIN || path.join(REPO_ROOT, 'src', 'whippet-cli');

/**
 * Returns { whippetd, whippetCli } if the node binary exists, else null.
 *
 * Only whippetd is required. This harness speaks JSON-RPC over fetch and
 * never invokes whippet-cli (see the transport note above); gating on it
 * as well only produced spurious skips on a tree where the daemon was
 * built but the CLI was not. The path is still resolved and returned for
 * callers that want it.
 */
function resolveBinaries() {
  if (!existsSync(WHIPPETD)) return null;
  return { whippetd: WHIPPETD, whippetCli: WHIPPET_CLI };
}

class RpcError extends Error {
  constructor(code, message) {
    super(message);
    this.name = 'RpcError';
    this.code = code;
  }
}

/**
 * Start a fresh, isolated regtest whippetd. Returns a handle with:
 *   - rpc(method, ...params): JSON-RPC call, throws RpcError on failure
 *   - stop(): graceful shutdown (RPC `stop`, wait for exit, else SIGKILL),
 *     then removes the temp datadir. Safe to call more than once.
 *   - datadir, rpcport, port
 */
async function startNode(opts) {
  opts = opts || {};
  const bins = resolveBinaries();
  if (!bins) {
    throw new Error(
      `whippetd not found at ${WHIPPETD} -- build it (make -C src whippetd) ` +
      `or point WHIPPETD_BIN at one`
    );
  }

  const datadir = mkdtempSync(path.join(tmpdir(), 'whippet-uapjs-it-'));
  // Non-default ports, chosen well away from mainnet (33665/33666) and
  // regtest's own defaults, to avoid any chance of colliding with a real
  // running node.
  const port = opts.port || 39501;
  const rpcport = opts.rpcport || 39502;
  const rpcuser = 'uapjsintegration';
  const rpcpassword = 'uapjsintegration';

  const confPath = path.join(datadir, 'whippet.conf');
  writeFileSync(
    confPath,
    [
      'regtest=1',
      'server=1',
      'listen=1',
      `rpcuser=${rpcuser}`,
      `rpcpassword=${rpcpassword}`,
      `port=${port}`,
      `rpcport=${rpcport}`,
      'rpcallowip=127.0.0.1',
      'keypool=10',
      // Needed so getrawtransaction can confirm non-wallet transactions
      // (e.g. OP_MINT/OP_MINT_TRANSFER spends, which involve no wallet
      // keys at all) once they've left the mempool.
      'txindex=1',
      '',
    ].join('\n')
  );

  const authHeader = 'Basic ' + Buffer.from(`${rpcuser}:${rpcpassword}`).toString('base64');
  const rpcUrl = `http://127.0.0.1:${rpcport}/`;

  async function rpc(method, ...params) {
    const res = await fetch(rpcUrl, {
      method: 'POST',
      headers: { 'content-type': 'text/plain;', authorization: authHeader },
      body: JSON.stringify({ jsonrpc: '1.0', id: 'uapjs-it', method, params }),
    });
    let body;
    const text = await res.text();
    try {
      body = JSON.parse(text);
    } catch (e) {
      throw new Error(`RPC ${method}: non-JSON response (HTTP ${res.status}): ${text.slice(0, 500)}`);
    }
    if (body.error) {
      throw new RpcError(body.error.code, body.error.message);
    }
    if (!res.ok) {
      throw new Error(`RPC ${method}: HTTP ${res.status} with no structured error: ${text.slice(0, 500)}`);
    }
    return body.result;
  }

  // IMPORTANT: -datadir is always explicit and always points at the fresh
  // temp dir created above; -regtest is always passed; ports are always
  // the non-default ones set above. Never invoke whippetd without these.
  const args = [
    '-regtest',
    `-datadir=${datadir}`,
    `-conf=${confPath}`,
    `-port=${port}`,
    `-rpcport=${rpcport}`,
    `-rpcuser=${rpcuser}`,
    `-rpcpassword=${rpcpassword}`,
    '-printtoconsole=0',
  ];

  const logPath = path.join(datadir, 'stdout.log');
  const child = spawn(bins.whippetd, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  const logChunks = [];
  child.stdout.on('data', (d) => logChunks.push(d));
  child.stderr.on('data', (d) => logChunks.push(d));

  let exited = false;
  let exitInfo = null;
  child.on('exit', (code, signal) => {
    exited = true;
    exitInfo = { code, signal };
  });

  async function waitForReady(timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    let lastErr = null;
    while (Date.now() < deadline) {
      if (exited) {
        throw new Error(
          `whippetd exited before becoming ready (code=${exitInfo.code}, signal=${exitInfo.signal}).\n` +
          `Output:\n${Buffer.concat(logChunks).toString('utf8')}`
        );
      }
      try {
        await rpc('getblockchaininfo');
        return;
      } catch (e) {
        lastErr = e;
        await new Promise((r) => setTimeout(r, 200));
      }
    }
    throw new Error(
      `whippetd did not become ready within ${timeoutMs}ms. Last error: ${lastErr && lastErr.message}\n` +
      `Output so far:\n${Buffer.concat(logChunks).toString('utf8')}`
    );
  }

  try {
    await waitForReady(opts.readyTimeoutMs || 30000);
  } catch (e) {
    // Best-effort cleanup if startup itself failed.
    try { child.kill('SIGKILL'); } catch (_) {}
    try { rmSync(datadir, { recursive: true, force: true }); } catch (_) {}
    throw e;
  }

  let stopped = false;
  async function stop() {
    if (stopped) return;
    stopped = true;
    if (!exited) {
      try {
        await rpc('stop');
      } catch (_) {
        // fall through to process-level wait/kill below regardless
      }
      const deadline = Date.now() + 15000;
      while (!exited && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 100));
      }
      if (!exited) {
        try { child.kill('SIGKILL'); } catch (_) {}
        const killDeadline = Date.now() + 5000;
        while (!exited && Date.now() < killDeadline) {
          await new Promise((r) => setTimeout(r, 100));
        }
      }
    }
    try { rmSync(datadir, { recursive: true, force: true }); } catch (_) {}
  }

  return { rpc, stop, datadir, port, rpcport, RpcError };
}

export { startNode, resolveBinaries, RpcError, WHIPPETD, WHIPPET_CLI };
