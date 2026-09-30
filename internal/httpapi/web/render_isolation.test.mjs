// F1 regression tests: dashboard render-failure isolation for absent DOM elements.
// Loads the real internal/httpapi/web/app.js in a stubbed browser sandbox and
// proves that removing one non-critical DOM node neither throws out of render()
// nor stops unrelated sections or future ticks, while producing a bounded,
// secret-free diagnostic. No network, no /tmp.
import { describe, it, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const APP_SRC = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const SECRET_MARKER = 'SECRET_MARKER_xyz_must_never_surface';

function makeEl(sel = '') {
  const attrs = {};
  const el = {
    __sel: sel,
    textContent: '', className: '', innerHTML: '', value: '', checked: false,
    disabled: false, hidden: false,
    dataset: {}, style: {},
    classList: {
      _s: new Set(),
      add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); },
      toggle(c, f) { if (f) this._s.add(c); else this._s.delete(c); },
      contains(c) { return this._s.has(c); },
      [Symbol.iterator]() { return this._s[Symbol.iterator](); },
    },
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return attrs[k] ?? null; },
    removeAttribute(k) { delete attrs[k]; },
    remove() {}, addEventListener() {}, removeEventListener() {},
    querySelector() { return makeEl(sel + ' child'); },
    querySelectorAll() { return []; },
    focus() {}, click() {}, select() {},
    closest() { return null; },
    childElementCount: 0, children: [],
    scrollTop: 0, scrollHeight: 0, clientHeight: 0,
    clientWidth: 0, offsetWidth: 0,
  };
  el.appendChild = c => { el.children.push(c); el.childElementCount = el.children.length; };
  return el;
}

// Selector-indexed element registry so tests can observe per-section writes.
// `missing` simulates an absent/renamed DOM node: querySelector returns null.
function makeDocument() {
  const registry = new Map();
  const missing = new Set();
  return {
    registry,
    missing,
    querySelector(sel) {
      if (missing.has(sel)) return null;
      if (!registry.has(sel)) registry.set(sel, makeEl(sel));
      return registry.get(sel);
    },
    querySelectorAll() { return []; },
    createElement(s) { return makeEl(s); },
    createElementNS(ns, s) { return makeEl(s); },
    addEventListener() {},
    removeEventListener() {},
    body: makeEl('body'),
    hidden: false,
    execCommand() { return false; },
  };
}

function makeSandbox(doc) {
  const warns = [];
  const sb = {
    console: {
      log() {}, info() {}, debug() {}, error() {},
      warn: (...a) => warns.push(a.map(String).join(' ')),
    },
    document: {
      querySelector: s => doc.querySelector(s),
      querySelectorAll: () => [],
      createElement: s => makeEl(s),
      createElementNS: (ns, s) => makeEl(s),
      addEventListener() {},
      removeEventListener() {},
      body: makeEl('body'),
      hidden: false,
      execCommand() { return false; },
    },
    sessionStorage: { getItem: () => '', setItem() {}, removeItem() {} },
    location: { origin: 'http://127.0.0.1:8080' },
    navigator: {},
    setTimeout: () => 0,
    clearTimeout() {},
    setInterval: () => 0,
    clearInterval() {},
    requestAnimationFrame: () => 0,
    cancelAnimationFrame() {},
    performance: { now: () => 0 },
    TextDecoder,
    AbortController,
  };
  sb.window = {
    fetch: () => new Promise(() => {}), // hang: boot stays parked, tests drive _render directly
    matchMedia: () => ({ matches: false }),
    addEventListener() {},
    removeEventListener() {},
    isSecureContext: false,
  };
  vm.createContext(sb);
  return { sb, warns };
}

function fixtureSnap() {
  return {
    deployments: [{ id: 'd1', model: 'm1', provider_id: 'p1', provider_name: 'P1' }],
    health: [{ deployment: 'd1', status: 'healthy', ewma_latency_ms: 12 }],
    events: [{
      kind: 'route_ok', seq: 1, request_id: 'r1', deployment: 'd1',
      latency_ms: 5, time: '2026-09-30T00:00:00.000Z', message: 'ok',
    }],
    config: {
      routing: { strategy: 'ready_mesh' },
      probe: { recovery_attempts: 5 },
      note_for_test: SECRET_MARKER,
    },
    provider_health: [], provider_stats: [], provider_pressure: [], scope_health: [],
    virtual_endpoints: [], route_profiles: [], candidate_pools: [], fallback_chains: [],
    cache: { hits: 8, misses: 2 },
    usage: { total_prompt_tokens: 100, total_completion_tokens: 50, total_estimated_cost_usd: 0.5 },
    session_count: 3, deployment_total: 1, request_total: 42, version: '0.12',
  };
}

function fixtureProviders() {
  return [{
    id: 'p1', name: 'P1', type: 'openai_compatible', model_count: 1,
    enabled: true, base_url: 'http://127.0.0.1:8080', has_secret: true, credential_count: 1,
  }];
}

const doc = makeDocument();
const { sb, warns } = makeSandbox(doc);
let NR;

before(() => {
  vm.runInContext(APP_SRC, sb, { filename: 'app.js' });
  NR = sb.window.NexaRoute;
  assert.ok(NR, 'window.NexaRoute must be exposed by app.js');
  assert.equal(typeof NR._render, 'function', 'F1 test hook _render must exist');
  assert.equal(typeof NR._renderStats, 'function');
});

function seed() {
  doc.missing.clear();
  warns.length = 0;
  NR._renderResetStats();
  NR._setSnap(fixtureSnap());
  NR._setProviderSummaries(fixtureProviders());
}

describe('F1 dashboard render-failure isolation', () => {
  it('full DOM: every section renders, no diagnostics', () => {
    seed();
    assert.doesNotThrow(() => NR._render());
    assert.equal(doc.querySelector('#routerStrategyLabel').textContent, 'ready_mesh');
    assert.equal(doc.querySelector('#sHealthy').textContent, '1');
    assert.ok(doc.querySelector('#providerGrid').innerHTML.includes('P1'), 'providers section rendered');
    assert.ok(doc.querySelector('#modelRows').innerHTML.includes('d1'), 'models section rendered');
    assert.ok(doc.querySelector('#consoleLog').innerHTML.includes('route_ok'), 'console section rendered');
    assert.equal(doc.querySelector('#obsRequest').textContent, 'r1', 'observatory section rendered');
    assert.ok(doc.querySelector('#settingsJson').textContent.includes('ready_mesh'), 'footer section rendered');
    assert.deepEqual(JSON.parse(JSON.stringify(NR._renderStats())), { counts: {}, recent: [] }, 'no diagnostics on full DOM');
  });

  it('missing non-critical node: no throw, bounded secret-free diagnostic, unrelated sections + future ticks continue', () => {
    seed();
    doc.missing.add('#sHealthy'); // simulate absent/renamed KPI node
    assert.doesNotThrow(() => NR._render(), 'first tick with missing node must not throw');
    assert.doesNotThrow(() => NR._render(), 'second tick (future render) must still run');

    const stats = NR._renderStats();
    assert.equal(stats.counts.header, 2, 'header-section failure counted per render');
    assert.ok(stats.recent.length >= 1 && stats.recent.length <= 20, 'diagnostics bounded');
    assert.ok(stats.recent.every(r => r.message.length <= 200), 'messages length-bounded');
    assert.ok(stats.recent.some(r => r.message.includes('missing element #sHealthy')), 'useful diagnostic names the node');
    assert.ok(warns.some(w => w.includes('header') && w.includes('#sHealthy')), 'console.warn carries the section diagnostic');

    const leaked = JSON.stringify(stats) + '\n' + warns.join('\n');
    assert.ok(!leaked.includes(SECRET_MARKER), 'diagnostics never carry snapshot payloads/secrets');

    // Unrelated sections kept rendering despite the header failure.
    assert.ok(doc.querySelector('#providerGrid').innerHTML.includes('P1'), 'providers unaffected');
    assert.ok(doc.querySelector('#modelRows').innerHTML.includes('d1'), 'models unaffected');
    assert.ok(doc.querySelector('#consoleLog').innerHTML.includes('route_ok'), 'console unaffected');
    assert.equal(doc.querySelector('#obsRequest').textContent, 'r1', 'observatory unaffected');
    assert.ok(doc.querySelector('#settingsJson').textContent.includes('ready_mesh'), 'footer unaffected');

    // A restored DOM renders cleanly again (recovery, not a stuck state).
    doc.missing.clear();
    NR._renderResetStats();
    assert.doesNotThrow(() => NR._render());
    assert.equal(doc.querySelector('#sHealthy').textContent, '1', 'header recovers when the node returns');
    assert.deepEqual(JSON.parse(JSON.stringify(NR._renderStats())), { counts: {}, recent: [] });
  });

  it('diagnostic buffer stays bounded under repeated failures', () => {
    seed();
    doc.missing.add('#donutPct');
    for (let i = 0; i < 25; i++) NR._render();
    const stats = NR._renderStats();
    assert.equal(stats.counts.donut, 25, 'every failure counted');
    assert.ok(stats.recent.length <= 20, `recent buffer capped (got ${stats.recent.length})`);
    assert.ok(doc.querySelector('#providerGrid').innerHTML.includes('P1'), 'other sections still render on tick 25');
  });

  it('script evaluates with init-time nodes absent (boot never halts)', () => {
    const doc2 = makeDocument();
    for (const sel of ['#modelSearch', '#probeBtn', '#compatReset', '#addProviderBtn', '#pPreset', '#cliBody']) {
      doc2.missing.add(sel);
    }
    const again = makeSandbox(doc2);
    assert.doesNotThrow(() => vm.runInContext(APP_SRC, again.sb, { filename: 'app.js' }));
    const nr2 = again.sb.window.NexaRoute;
    assert.ok(nr2 && typeof nr2._render === 'function', 'test hooks exposed despite missing init nodes');
    nr2._setSnap(fixtureSnap());
    nr2._setProviderSummaries(fixtureProviders());
    assert.doesNotThrow(() => nr2._render(), 'render still runs with init nodes absent');
  });
});
