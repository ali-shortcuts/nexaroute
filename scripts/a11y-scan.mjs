#!/usr/bin/env node
// M2-11/M2-13 evidence scan: drives a real Chromium (raw CDP, no
// puppeteer) over every admin-UI page in both themes against a
// locally built gateway, runs axe-core (wcag2a/wcag2aa/wcag22aa),
// and measures first contentful paint on localhost.
//
// Usage:
//   npm --prefix scripts/a11y install        # once, fetches axe-core
//   node scripts/a11y-scan.mjs [--out DIR]  # builds + runs the gateway
//
// Env: CHROME_BIN (default /opt/meta-chromium/chrome, falls back to
// `which chromium|google-chrome`), GO_ENV (default ~/go-env.sh).
// Exit code: 1 when any serious/critical axe violation exists or
// FCP >= 1000 ms; 0 otherwise. A JSON report is written to --out.
import { spawn, execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';
import path from 'node:path';
import os from 'node:os';

const repo = process.cwd();
const outDir = process.argv.includes('--out')
  ? process.argv[process.argv.indexOf('--out') + 1]
  : path.join(repo, 'docs/browser-evidence/m2-11');
const PORT = 18741, CDP_PORT = 9333;
const axePath = path.join(repo, 'scripts/a11y/node_modules/axe-core/axe.min.js');
if (!existsSync(axePath)) {
  console.error('axe-core not found. Run: npm --prefix scripts/a11y install');
  process.exit(2);
}
const axeSource = readFileSync(axePath, 'utf8');

function chromeBin() {
  if (process.env.CHROME_BIN) return process.env.CHROME_BIN;
  if (existsSync('/opt/meta-chromium/chrome')) return '/opt/meta-chromium/chrome';
  for (const name of ['chromium', 'chromium-browser', 'google-chrome', 'google-chrome-stable']) {
    try { return execFileSync('which', [name], { encoding: 'utf8' }).trim(); } catch { /* next */ }
  }
  throw new Error('no chromium binary found (set CHROME_BIN)');
}

// --- gateway fixture -------------------------------------------------
const tmp = execFileSync('mktemp', ['-d'], { encoding: 'utf8' }).trim();
const bin = path.join(tmp, 'nexaroute');
execFileSync('bash', ['-c', `source ${process.env.GO_ENV || path.join(os.homedir(), 'go-env.sh')}; cd ${repo} && go build -o ${bin} ./cmd/gateway`], { stdio: 'inherit' });
const cfgPath = path.join(tmp, 'config.json');
writeFileSync(cfgPath, JSON.stringify({ listen: `127.0.0.1:${PORT}` }, null, 2));
const gateway = spawn(bin, ['--config', cfgPath], { stdio: ['ignore', 'pipe', 'pipe'] });
let gatewayLog = '';
gateway.stdout.on('data', d => { gatewayLog += d; });
gateway.stderr.on('data', d => { gatewayLog += d; });

async function waitFor(fn, what, timeout = 30000) {
  const start = Date.now();
  for (;;) {
    try { if (await fn()) return; } catch { /* retry */ }
    if (Date.now() - start > timeout) throw new Error('timeout waiting for ' + what);
    await sleep(250);
  }
}

// --- minimal CDP client ----------------------------------------------
class CDP {
  constructor(ws) {
    this.ws = ws; this.id = 0; this.pending = new Map();
    ws.onmessage = ev => {
      const msg = JSON.parse(ev.data);
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) reject(new Error(msg.error.message)); else resolve(msg.result);
      }
    };
  }
  send(method, params = {}, sessionId) {
    const id = ++this.id;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
  }
}

const chrome = spawn(chromeBin(), [
  '--headless=new', '--disable-gpu', '--no-sandbox', '--hide-scrollbars',
  '--disable-features=LocalNetworkAccessChecks,PrivateNetworkAccessChecks,PrivateNetworkAccessSendPreflights',
  `--remote-debugging-port=${CDP_PORT}`, `--user-data-dir=${path.join(tmp, 'chrome')}`, 'about:blank',
], { stdio: ['ignore', 'ignore', 'pipe'] });

let exitCode = 0;
try {
  await waitFor(async () => (await fetch(`http://127.0.0.1:${PORT}/healthz`)).ok, 'gateway healthz');
  await waitFor(async () => (await fetch(`http://127.0.0.1:${CDP_PORT}/json/version`)).ok, 'chrome CDP');
  const version = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/version`)).json();
  const ws = new WebSocket(version.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  const cdp = new CDP(ws);
  const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
  const S = (m, p) => cdp.send(m, p, sessionId);
  await S('Page.enable');
  await S('Runtime.enable');
  const evalJs = async expr => {
    const r = await S('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error('page evaluate failed: ' + (r.exceptionDetails.text || '') + ' ' + (r.exceptionDetails.exception?.description || ''));
    return r.result.value;
  };

  const report = { generated_at: new Date().toISOString(), url: `http://127.0.0.1:${PORT}/`, themes: {}, fcp_ms: null };
  for (const theme of ['dark', 'light']) {
    await S('Page.navigate', { url: `http://127.0.0.1:${PORT}/` });
    await sleep(600);
    const bootHref = await evalJs(`location.href`).catch(() => '');
    if (String(bootHref).startsWith('chrome-error://')) {
      throw new Error('the browser refused the loopback navigation (Local Network Access checks). This Chromium build cannot scan a local gateway; run the scan on a standard Chromium/Chrome host. See docs/browser-evidence/m2-11/README.md.');
    }
    await waitFor(async () => evalJs(`!!document.querySelector('#primary-nav button')`), 'UI boot');
    if (theme === 'light') {
      await evalJs(`localStorage.setItem('nexaroute_theme','light'); document.documentElement.dataset.theme='light';`);
    } else {
      await evalJs(`localStorage.setItem('nexaroute_theme','dark'); document.documentElement.dataset.theme='dark';`);
    }
    await sleep(400);
    if (theme === 'dark') {
      const paints = await evalJs(`performance.getEntriesByType('paint').map(p => ({name: p.name, startTime: Math.round(p.startTime)}))`);
      const fcp = (paints || []).find(p => p.name === 'first-contentful-paint');
      report.fcp_ms = fcp ? fcp.startTime : null;
    }
    await evalJs(axeSource);
    const pages = await evalJs(`[...document.querySelectorAll('#primary-nav button')].map(b => b.dataset.page)`);
    report.themes[theme] = {};
    for (const page of pages) {
      await evalJs(`document.querySelector('#primary-nav button[data-page="${page}"]').click()`);
      await sleep(350);
      const res = await evalJs(`axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a','wcag2aa','wcag22aa'] } }).then(r => ({ violations: r.violations.map(v => ({ id: v.id, impact: v.impact, nodes: v.nodes.length, help: v.help })), passes: r.passes.length }))`);
      report.themes[theme][page] = res;
      const bad = res.violations.filter(v => v.impact === 'serious' || v.impact === 'critical');
      if (bad.length) { exitCode = 1; console.log(`AXE ${theme}/${page}: ${bad.map(v => `${v.id}(${v.impact},${v.nodes})`).join(' ')}`); }
      else console.log(`AXE ${theme}/${page}: clean (${res.passes} passes, ${res.violations.length} minor/none)`);
    }
  }
  console.log('FCP:', report.fcp_ms, 'ms');
  if (!(report.fcp_ms !== null && report.fcp_ms < 1000)) { exitCode = 1; console.log('FCP budget (<1000ms) NOT met'); }
  mkdirSync(outDir, { recursive: true });
  writeFileSync(path.join(outDir, 'axe-report.json'), JSON.stringify(report, null, 2));
  console.log('report written to', path.join(outDir, 'axe-report.json'));
} catch (e) {
  console.error('scan failed:', e.message);
  console.error('gateway log tail:', gatewayLog.slice(-800));
  exitCode = 2;
} finally {
  try { chrome.kill('SIGKILL'); } catch { /* gone */ }
  try { gateway.kill('SIGKILL'); } catch { /* gone */ }
}
process.exit(exitCode);
