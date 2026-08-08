// A minimal Chrome DevTools Protocol driver.
//
// Node has had a WebSocket global since 22, so speaking CDP directly costs
// nothing but the code below -- which matters here: contrib/uap-web ships
// with zero runtime dependencies and one devDependency, and a browser
// automation framework would be by far the largest thing in the tree.
//
// # Personas
//
// Each persona gets its own browser context. The wallet keeps nothing on disk
// (app.js's storage is an in-memory Map) so wallet isolation would come free
// from a second tab, but a context also separates Cache Storage, which the
// service worker uses. Two personas sharing one cache is a source of
// cross-talk nobody would think to look for.
//
// # Dialogs
//
// An open alert()/confirm() freezes the renderer, and every subsequent
// Runtime.evaluate hangs forever -- not fails, hangs. That is how an earlier
// version of this driver appeared to lock up on a click. Dialogs are
// therefore dismissed automatically AND recorded: the app is not supposed to
// have any left, so one appearing is a finding, and reporting it as a finding
// is only possible if the run survives it.
import { spawn } from 'node:child_process';
import fs from 'node:fs/promises';
import path from 'node:path';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export async function launchChrome({ binary, workdir, port = 9422 }) {
  const proc = spawn(binary, [
    '--headless=new',
    `--remote-debugging-port=${port}`,
    '--no-sandbox',
    '--disable-gpu',
    '--disable-dev-shm-usage',
    '--no-first-run',
    '--no-default-browser-check',
    `--user-data-dir=${path.join(workdir, 'chrome-profile')}`,
    'about:blank'
  ], { stdio: ['ignore', 'pipe', 'pipe'] });

  let wsUrl = null;
  for (let i = 0; i < 80 && !wsUrl; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/version`);
      wsUrl = (await r.json()).webSocketDebuggerUrl;
    } catch (_) { await sleep(250); }
  }
  if (!wsUrl) {
    proc.kill('SIGKILL');
    throw new Error(`chrome (${binary}) never opened a debugging port on ${port}`);
  }

  const ws = new WebSocket(wsUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve, { once: true });
    ws.addEventListener('error', () => reject(new Error('CDP websocket failed to open')), { once: true });
  });

  let nextId = 0;
  const pending = new Map();
  // Handlers keyed by sessionId, so each persona sees only its own events.
  const sessionHandlers = new Map();

  ws.addEventListener('message', (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      if (msg.error) reject(new Error(JSON.stringify(msg.error)));
      else resolve(msg.result);
      return;
    }
    if (msg.method && msg.sessionId) {
      const h = sessionHandlers.get(msg.sessionId);
      if (h) h(msg);
    }
  });

  const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params, sessionId }));
    // A hung CDP call is the single most confusing failure mode here, so it
    // is given a deadline rather than being allowed to stall the run.
    setTimeout(() => {
      if (pending.has(id)) {
        pending.delete(id);
        reject(new Error(`CDP ${method} timed out after 30s`));
      }
    }, 30000);
  });

  const close = async () => {
    try { ws.close(); } catch (_) {}
    proc.kill('SIGTERM');
    for (let i = 0; i < 20 && proc.exitCode === null; i++) await sleep(100);
    if (proc.exitCode === null) proc.kill('SIGKILL');
  };

  return { send, sessionHandlers, close, workdir };
}

/**
 * Open an isolated page. `name` is used for screenshot filenames and in
 * failure messages.
 */
export async function newPersona(chrome, name, { width = 390, height = 844 } = {}) {
  const { browserContextId } = await chrome.send('Target.createBrowserContext', {
    disposeOnDetach: true
  });
  const { targetId } = await chrome.send('Target.createTarget', {
    url: 'about:blank',
    browserContextId
  });
  const { sessionId } = await chrome.send('Target.attachToTarget', { targetId, flatten: true });

  const dialogs = [];
  const consoleErrors = [];
  // Patterns for console errors a phase deliberately provoked. A phase that
  // makes the node reject a transaction on purpose has to say so; everything
  // else is a finding.
  const expectedConsoleErrors = [];

  chrome.sessionHandlers.set(sessionId, (msg) => {
    if (msg.method === 'Page.javascriptDialogOpening') {
      dialogs.push(msg.params.message);
      chrome.send('Page.handleJavaScriptDialog', { accept: true }, sessionId).catch(() => {});
    } else if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
      consoleErrors.push(msg.params.entry.text);
    } else if (msg.method === 'Runtime.exceptionThrown') {
      const d = msg.params.exceptionDetails;
      consoleErrors.push((d.exception && d.exception.description) || d.text || 'exception');
    }
  });

  const S = (method, params) => chrome.send(method, params, sessionId);

  await S('Page.enable');
  await S('Runtime.enable');
  await S('Log.enable');
  await S('Emulation.setDeviceMetricsOverride', {
    width, height, deviceScaleFactor: 2, mobile: true
  });

  const evaluate = async (expression) => {
    const r = await S('Runtime.evaluate', {
      expression, awaitPromise: true, returnByValue: true
    });
    if (r.exceptionDetails) {
      const d = r.exceptionDetails;
      throw new Error(
        `[${name}] page threw: ` +
        ((d.exception && d.exception.description) || d.text || 'unknown')
      );
    }
    return r.result.value;
  };

  const text = () => evaluate('document.body.innerText');

  const screenshot = async (label) => {
    const shot = await S('Page.captureScreenshot', { format: 'png' });
    const file = path.join(chrome.workdir, `${name}-${label}.png`);
    await fs.writeFile(file, Buffer.from(shot.data, 'base64'));
    return file;
  };

  /**
   * Poll a page-side expression until it is truthy.
   *
   * Sleeping a fixed amount between steps is what the first version of this
   * did, and it is the wrong tool twice over: too short and it fails on a
   * loaded machine, too long and it wastes minutes. Worse, a fixed sleep can
   * pass for the wrong reason -- reading the previous screen before the new
   * one has rendered.
   */
  const waitFor = async (expression, { timeoutMs = 20000, what = expression } = {}) => {
    const deadline = Date.now() + timeoutMs;
    let lastErr = null;
    while (Date.now() < deadline) {
      try {
        if (await evaluate(`!!(${expression})`)) return true;
        lastErr = null;
      } catch (e) { lastErr = e; }
      await sleep(100);
    }
    const screen = await text().catch(() => '(could not read the page)');
    const shot = await screenshot('timeout').catch(() => '(no screenshot)');
    throw new Error(
      `[${name}] timed out after ${timeoutMs}ms waiting for: ${what}\n` +
      (lastErr ? `last evaluation error: ${lastErr.message}\n` : '') +
      `console errors: ${consoleErrors.length ? consoleErrors.join(' | ') : '(none)'}\n` +
      `dialogs: ${dialogs.length ? JSON.stringify(dialogs) : '(none)'}\n` +
      `screenshot: ${shot}\n` +
      `--- screen ---\n${screen}`
    );
  };

  /** Wait for a screen by its screen-<name> class. */
  const waitForScreen = (screenName, opts = {}) =>
    waitFor(`document.querySelector('.screen-${screenName}')`,
      { what: `the ${screenName} screen`, ...opts });

  /** Click the first button whose visible label contains `label`. */
  const click = async (label) => {
    const ok = await evaluate(`
      (() => {
        const b = Array.from(document.querySelectorAll('button'))
          .find(el => (el.innerText || '').includes(${JSON.stringify(label)}) && !el.disabled);
        if (!b) return false;
        b.click();
        return true;
      })()
    `);
    if (!ok) {
      throw new Error(
        `[${name}] no enabled button labelled ${JSON.stringify(label)}.\n` +
        `buttons present: ${JSON.stringify(await evaluate(
          `Array.from(document.querySelectorAll('button')).map(b => b.innerText)`))}\n` +
        `--- screen ---\n${await text()}`
      );
    }
  };

  /**
   * Set an input's value.
   *
   * Both events are dispatched because the app listens for different ones in
   * different places, and a value set without them is a value the app never
   * sees.
   */
  const fill = async (id, value) => {
    const ok = await evaluate(`
      (() => {
        const el = document.getElementById(${JSON.stringify(id)});
        if (!el) return false;
        el.value = ${JSON.stringify(String(value))};
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
      })()
    `);
    if (!ok) {
      throw new Error(
        `[${name}] no field with id ${JSON.stringify(id)}.\n` +
        `fields present: ${JSON.stringify(await evaluate(
          `Array.from(document.querySelectorAll('input,select,textarea')).map(e => e.id)`))}`
      );
    }
  };

  const navigate = async (url) => {
    await S('Page.navigate', { url });
    await waitFor('document.querySelector("#app") && document.querySelector(".screen")',
      { what: 'the app to render its first screen' });
  };

  return {
    name, sessionId, dialogs, consoleErrors, expectedConsoleErrors,
    evaluate, text, click, fill, waitFor, waitForScreen, navigate, screenshot,
    /** Text of the first element matching `sel`, or null. */
    async textOf(sel) {
      return evaluate(`
        (() => { const e = document.querySelector(${JSON.stringify(sel)});
                 return e ? e.innerText : null; })()
      `);
    },
    /** Whether any element matches `sel`. */
    async has(sel) {
      return evaluate(`!!document.querySelector(${JSON.stringify(sel)})`);
    }
  };
}

export { sleep };
