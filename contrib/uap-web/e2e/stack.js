// The stack the browser tests drive: a regtest whippetd, a uap-indexer
// pointed at it, and nothing else.
//
// # Why the real indexer and not a stub
//
// A stub server answering /api from fixtures can tell you the wallet renders
// what it is given. It cannot tell you whether the covenant the wallet built
// is one the network will accept, which is the only question worth asking
// about a transaction. So this runs the real binary against a real node, and
// every broadcast is checked by mining a block and asking the node what it
// got.
//
// # The guard
//
// The indexer's -rpcport defaults to 33665, which is MAINNET RPC. Pointing
// it there would have it index the wrong chain into a temp database -- and,
// far worse, would mean a test suite reaching for a node that holds real
// funds. Nothing here defaults: every flag is passed explicitly, and
// startIndexer refuses to spawn unless the node it is about to be aimed at
// says it is regtest AND is the one this process started. See assertRegtest.
//
// The narrow RPC surface is the second line, not the first: the indexer
// calls only getblockcount, getblockhash, getblock, estimatesmartfee and
// sendrawtransaction -- no wallet RPCs at all -- so it cannot move coins even
// if misaimed. The guard exists so it is never misaimed.
import { spawn } from 'node:child_process';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync, openSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startNode, resolveBinaries } from '../../uap-js/test-node-harness.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const INDEXER_DIR = path.resolve(HERE, '../../uap-indexer');

// Ports for this suite alone. mint.integration.test.js uses 39501/39502 and
// market.integration.test.js uses 39601/39602; staying clear of both means
// this can run alongside them.
export const P2P_PORT = 39701;
export const RPC_PORT = 39702;
export const INDEXER_PORT = 39703;

/**
 * Refuse to go any further unless the node is regtest and is ours.
 *
 * Two separate checks, because they fail differently. `chain` catches being
 * aimed at a mainnet or testnet node. The port comparison catches the subtler
 * case: a regtest node that this process did not start, whose datadir and
 * wallet belong to somebody else.
 */
export async function assertRegtest(node, rpcPortAboutToBeUsed) {
  const info = await node.rpc('getblockchaininfo');
  if (info.chain !== 'regtest') {
    throw new Error(
      `refusing to run: node reports chain=${info.chain}, expected regtest. ` +
      `This suite mines blocks and broadcasts transactions; it must never ` +
      `touch a real chain.`
    );
  }
  if (rpcPortAboutToBeUsed !== node.rpcport) {
    throw new Error(
      `refusing to run: about to point the indexer at RPC port ` +
      `${rpcPortAboutToBeUsed}, but the node this suite started is on ` +
      `${node.rpcport}. Never aim it at a node this process did not spawn.`
    );
  }
  return info;
}

/** Where the Chrome binary is, or null. Probed, not assumed. */
export function resolveChrome() {
  const candidates = [
    process.env.CHROME_BIN,
    'chromium',
    'chromium-browser',
    'google-chrome',
    'google-chrome-stable'
  ].filter(Boolean);
  for (const c of candidates) {
    try {
      const p = execFileSync('sh', ['-c', `command -v ${JSON.stringify(c)}`], {
        encoding: 'utf8'
      }).trim();
      if (p) return p;
    } catch (_) { /* not on PATH; try the next */ }
  }
  return null;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * Build the indexer, which also flattens contrib/uap-web (and uap-js) into
 * web/ for //go:embed.
 *
 * Testing that flattened copy rather than the source tree is deliberate: it
 * is what actually ships, and the flattening step has broken cross-directory
 * imports before. A suite that loaded the sources directly would go green on
 * a binary that serves a wallet which dies on its first import.
 */
export function buildIndexer() {
  execFileSync('make', ['-C', INDEXER_DIR, 'build'], { stdio: 'pipe' });
  const bin = path.join(INDEXER_DIR, 'uap-indexer');
  if (!existsSync(bin)) throw new Error(`make succeeded but ${bin} is missing`);
  return bin;
}

/**
 * Bring the whole stack up. Returns { node, indexer, api, chrome, workdir,
 * stop }. Call stop() from a finally -- it is idempotent.
 */
export async function startStack({ log = () => {} } = {}) {
  const workdir = mkdtempSync(path.join(tmpdir(), 'whippet-e2e-'));

  log('starting whippetd (regtest)');
  const node = await startNode({ port: P2P_PORT, rpcport: RPC_PORT });

  let indexerProc = null;
  const stop = async () => {
    if (indexerProc && !indexerProc.killed) {
      indexerProc.kill('SIGTERM');
      // It has a 5s graceful HTTP shutdown; give it a moment, then insist.
      for (let i = 0; i < 30 && indexerProc.exitCode === null; i++) await sleep(200);
      if (indexerProc.exitCode === null) indexerProc.kill('SIGKILL');
    }
    await node.stop();
    try { rmSync(workdir, { recursive: true, force: true }); } catch (_) {}
  };

  try {
    const info = await assertRegtest(node, RPC_PORT);
    log(`node is regtest at height ${info.blocks}, rpc ${node.rpcport}`);

    log('building the indexer (this also rebuilds the embedded frontend)');
    const bin = buildIndexer();

    const statefile = path.join(workdir, 'uap-index.sqlite');
    const logPath = path.join(workdir, 'indexer.log');
    const logFd = openSync(logPath, 'a');

    // Every path explicit, nothing defaulted. In particular -mirror is
    // omitted: it is the only flag that would make this process talk to
    // anything off this machine.
    indexerProc = spawn(bin, [
      `-rpchost=127.0.0.1`,
      `-rpcport=${node.rpcport}`,
      `-rpcuser=${node.rpcuser}`,
      `-rpcpassword=${node.rpcpassword}`,
      `-statefile=${statefile}`,
      `-listen=127.0.0.1:${INDEXER_PORT}`,
      `-startheight=0`,
      `-pollinterval=1s`
    ], { stdio: ['ignore', logFd, logFd] });

    indexerProc.on('exit', (code, signal) => {
      if (code !== 0 && code !== null) log(`indexer exited: code=${code} signal=${signal}`);
    });

    const base = `http://127.0.0.1:${INDEXER_PORT}`;
    const api = makeApi(base, logPath);

    log('waiting for the indexer to answer');
    await api.waitReady();

    return { node, indexer: indexerProc, api, base, workdir, logPath, stop };
  } catch (e) {
    await stop();
    throw e;
  }
}

function makeApi(base, logPath) {
  const get = async (p) => {
    const r = await fetch(base + p);
    if (!r.ok) throw new Error(`GET ${p} -> ${r.status} ${await r.text()}`);
    return r.json();
  };

  return {
    base,
    get,

    async waitReady(timeoutMs = 30000) {
      const deadline = Date.now() + timeoutMs;
      let last = null;
      while (Date.now() < deadline) {
        try {
          return await get('/api/status');
        } catch (e) {
          last = e;
          await sleep(200);
        }
      }
      throw new Error(
        `indexer never answered on ${base} (${last && last.message}). ` +
        `Its log is at ${logPath}`
      );
    },

    /**
     * Block until the indexer has caught up to `height`.
     *
     * Every assertion made against /api/* after mining a block needs this.
     * The indexer polls the node on a 1s tick, so a read taken immediately
     * after `generate` returns is a read of the previous state -- and a test
     * that sleeps a fixed amount instead is a test that fails on a slow
     * machine and, worse, passes for the wrong reason on a fast one.
     */
    async syncTo(height, timeoutMs = 30000) {
      const deadline = Date.now() + timeoutMs;
      let status = null;
      while (Date.now() < deadline) {
        status = await get('/api/status');
        if (typeof status.tip_height !== 'number') {
          throw new Error(`/api/status has no numeric tip_height: ${JSON.stringify(status)}`);
        }
        if (status.tip_height >= height) return status;
        await sleep(150);
      }
      throw new Error(
        `indexer stuck at ${status && status.tip_height}, waiting for ${height}. ` +
        `Its log is at ${logPath}`
      );
    }
  };
}

export { resolveBinaries, sleep };
