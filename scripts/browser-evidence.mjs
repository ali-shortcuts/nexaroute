#!/usr/bin/env node
// M2-14 evidence runner: Playwright (playwright-core + the system
// Chromium; no browser download) captures every admin-UI page
// across themes (dark/light), languages (en + fa/RTL) and
// viewports (desktop 1440x900, mobile 390x844), runs axe per page,
// and writes manifest.json with a SHA-256 per screenshot so a
// re-run reports exactly which shots changed (the diff baseline).
//
// Usage:
//   npm --prefix scripts/a11y install
//   node scripts/browser-evidence.mjs [--out DIR]
//
// Exit codes: 0 = captured; 1 = axe serious/critical found;
// 2 = environment cannot run it (e.g. the sandbox Chromium build
// that refuses loopback navigations — see
// docs/browser-evidence/m2-11/README.md). A blocked environment
// produces NO screenshots rather than empty evidence.
import { spawn, execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { setTimeout as sleep } from 'node:timers/promises';
import path from 'node:path';
import os from 'node:os';

const repo = process.cwd();
const outDir = process.argv.includes('--out')
  ? process.argv[process.argv.indexOf('--out') + 1]
  : path.join(repo, 'docs/browser-evidence/m2-14');
const PORT = 18741;
const toolsDir = path.join(repo, 'scripts/a11y/node_modules');
const axePath = path.join(toolsDir, 'axe-core/axe.min.js');
let chromium;
try {
  ({ chromium } = await import(path.join(toolsDir, 'playwright-core/lib/server/registry/index.js')).catch(() => import('playwright-core')));
} catch {
  try { ({ chromium } = await import('playwright-core')); } catch { /* handled below */ }
}
if (!chromium) {
  // playwright-core is resolved relative to this script's tools dir.
  const { createRequire } = await import('node:module');
  const req = createRequire(path.join(repo, 'scripts/a11y/package.json'));
  try { ({ chromium } = req('playwright-core')); } catch { /* fallthrough */ }
}
if (!chromium || !existsSync(axePath)) {
  console.error('browser tools not installed. Run: npm --prefix scripts/a11y install');
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

const tmp = execFileSync('mktemp', ['-d'], { encoding: 'utf8' }).trim();
const bin = path.join(tmp, 'nexaroute');
execFileSync('bash', ['-c', `source ${process.env.GO_ENV || path.join(os.homedir(), 'go-env.sh')}; cd ${repo} && go build -o ${bin} ./cmd/gateway`], { stdio: 'inherit' });
const cfgPath = path.join(tmp, 'config.json');
writeFileSync(cfgPath, JSON.stringify({ listen: `127.0.0.1:${PORT}` }, null, 2));
const gateway = spawn(bin, ['--config', cfgPath], { stdio: ['ignore', 'ignore', 'ignore'] });

const sha256 = buf => createHash('sha256').update(buf).digest('hex');
let exitCode = 0;
try {
  for (let i = 0; i < 60; i++) {
    try { if ((await fetch(`http://127.0.0.1:${PORT}/healthz`)).ok) break; } catch { /* wait */ }
    await sleep(250);
  }
  const browser = await chromium.launch({ executablePath: chromeBin(), args: ['--no-sandbox', '--no-proxy-server'] });
  const manifest = { generated_at: new Date().toISOString(), base: `http://127.0.0.1:${PORT}/`, shots: [], axe: {} };
  const combos = [];
  for (const theme of ['dark', 'light']) for (const lang of ['en', 'fa']) for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) combos.push({ theme, lang, vp });
  for (const { theme, lang, vp } of combos) {
    const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    await context.addInitScript(([th, lg]) => { localStorage.setItem('nexaroute_theme', th); localStorage.setItem('nexaroute_lang', lg); }, [theme, lang]);
    const page = await context.newPage();
    await page.goto(manifest.base, { waitUntil: 'load' });
    if (page.url().startsWith('chrome-error://')) {
      console.error('ENVIRONMENT BLOCKED: the browser refused the loopback navigation (Local Network Access checks). No screenshots were captured. See docs/browser-evidence/m2-11/README.md.');
      process.exitCode = 2;
      await browser.close();
      gateway.kill('SIGKILL');
      process.exit(2);
    }
    await page.waitForSelector('#primary-nav button', { timeout: 15000 });
    const pages = await page.$$eval('#primary-nav button', btns => btns.map(b => b.dataset.page));
    const dir = path.join(outDir, `${theme}-${lang}-${vp.name}`);
    mkdirSync(dir, { recursive: true });
    for (const p of pages) {
      await page.click(`#primary-nav button[data-page="${p}"]`);
      await page.waitForTimeout(350);
      const file = path.join(dir, `${p}.png`);
      const buf = await page.screenshot({ fullPage: true });
      writeFileSync(file, buf);
      manifest.shots.push({ file: path.relative(outDir, file), page: p, theme, lang, viewport: vp.name, sha256: sha256(buf) });
      if (vp.name === 'desktop' && lang === 'en') {
        await page.evaluate(axeSource);
        const res = await page.evaluate(() => axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag22aa'] } }).then(r => r.violations.map(v => ({ id: v.id, impact: v.impact, nodes: v.nodes.length }))));
        manifest.axe[`${theme}/${p}`] = res;
        if (res.some(v => v.impact === 'serious' || v.impact === 'critical')) exitCode = 1;
      }
    }
    await context.close();
    console.log(`captured ${theme}-${lang}-${vp.name}: ${pages.length} pages`);
  }
  await browser.close();
  const manifestPath = path.join(outDir, 'manifest.json');
  let changed = [];
  if (existsSync(manifestPath)) {
    const prev = JSON.parse(readFileSync(manifestPath, 'utf8'));
    const prevMap = new Map(prev.shots.map(s => [s.file, s.sha256]));
    changed = manifest.shots.filter(s => prevMap.has(s.file) && prevMap.get(s.file) !== s.sha256).map(s => s.file);
    const removed = [...prevMap.keys()].filter(f => !manifest.shots.some(s => s.file === f));
    console.log('changed since previous manifest:', changed.length ? changed.join(', ') : 'none');
    if (removed.length) console.log('no longer captured:', removed.join(', '));
  }
  manifest.changed_since_previous = changed;
  writeFileSync(manifestPath, JSON.stringify(manifest, null, 2));
  console.log('manifest written:', manifestPath, `(${manifest.shots.length} shots)`);
} catch (e) {
  console.error('evidence run failed:', e.message);
  exitCode = exitCode || 2;
} finally {
  try { gateway.kill('SIGKILL'); } catch { /* gone */ }
}
process.exit(exitCode);
