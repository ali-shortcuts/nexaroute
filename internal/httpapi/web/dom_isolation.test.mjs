// F1 regression: missing non-critical DOM node must not stop unrelated
// dashboard sections or future renders. Loads the real app.js in a stubbed
// browser sandbox, removes #sHealthy, and proves #providerGrid still renders,
// a second render still applies, and diagnostics stay bounded without secrets.
import { describe, it, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const APP_SRC = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const SECRET = 'SECRET_F1_xyz_must_never_surface';

function makeEl(sel = '') {
  const cls = {
    _s: new Set(),
    add(...c) { c.forEach(x => cls._s.add(x)); },
    remove(...c) { c.forEach(x => cls._s.delete(x)); },
    toggle(c, f) { if (f === undefined) { cls._s.has(c) ? cls._s.delete(c) : cls._s.add(c); } else if (f) cls._s.add(c); else cls._s.delete(c); },
    contains(c) { return cls._s.has(c); },
    [Symbol.iterator]() { return cls._s.values(); },
  };
  const el = {
    __sel: sel,
    textContent: '', className: '', innerHTML: '', value: '', checked: true,
    disabled: false, hidden: false, type: 'text', placeholder: '',
    dataset: {}, style: {},
    classList: cls,
    setAttribute() {}, getAttribute() { return null; },
    removeAttribute() {}, remove() {}, focus() {}, click() {},
    addEventListener() {}, removeEventListener() {},
    closest() { return null; },
    querySelector(q) { return makeEl(sel + ' ' + q); },
    querySelectorAll() { return []; },
    childElementCount: 0, children: [],
    scrollTop: 0, scrollHeight: 100, clientHeight: 100, clientWidth: 600, offsetWidth: 0,
    innerText: '',
  };
  el.appendChild = c => { el.children.push(c); el.childElementCount = el.children.length; return c; };
  return el;
}

const FULL_IDS = [
  '#toast', '#routerStrategyLabel', '#coreSub', '#sDeploy', '#sHealthy',
  '#sCooldown', '#sHealthBar', '#sCooldownSub', '#sSessions', '#sLatency',
  '#ring', '#donutGood', '#donutUnknown', '#donutDegraded', '#donutHalfOpen',
  '#donutCooldown', '#donutRetired', '#donutLegend', '#donutPct',
  '#providerGrid', '#modelSearch', '#modelRows', '#providerHealthRows',
  '#scopeHealthList', '#obsRequest', '#obsPublic', '#obsAbsorbed', '#obsResult',
  '#routeFlow', '#routeJourney', '#supervisorList', '#failureRadar',
  '#consoleLog', '#consoleAuto', '#consoleCount', '#settingsJson',
  '#sCacheRate', '#sCacheSub', '#sTokens', '#sSpendSub', '#footRequests',
  '#brandVersion', '#compat', '#virtual', '#profiles', '#pools',
  '#chartLine', '#chartArea', '#chartLast', '#chartMax', '#apiState',
  '#console', '#consoleDot', '#footClock', '#cliBody', '#cliTabs',
];

function loadApp(missing = new Set()) {
  const els = new Map();
  const warns = [];
  const cliTab = makeEl('#cliTabs button');
  cliTab.dataset = { cli: 'claude' };
  cliTab.classList.contains = () => true;
  const querySelector = sel => {
    if (missing.has(sel)) return null;
    if (!els.has(sel)) {
      const el = makeEl(sel);
      if (sel === '#modelSearch') el.value = '';
      els.set(sel, el);
    }
    return els.get(sel);
  };
  const sandbox = {
    console: {
      log() {}, info() {}, debug() {},
      warn: (...a) => warns.push(a.map(String).join(' ')),
      error: (...a) => warns.push(a.map(String).join(' ')),
    },
    document: {
      querySelector,
      querySelectorAll: sel => (sel === '#cliTabs button' ? [cliTab] : []),
      createElement: s => makeEl('created:' + s),
      createElementNS: (ns, s) => makeEl('created:' + s),
      addEventListener() {},
      body: makeEl('body'),
      hidden: false,
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
  sandbox.window = {
    fetch: () => new Promise(() => {}),
    matchMedia: () => ({ matches: false }),
    addEventListener() {},
    removeEventListener() {},
    isSecureContext: false,
  };
  vm.createContext(sandbox);
  vm.runInContext(APP_SRC, sandbox, { filename: 'app.js' });
  const NR = sandbox.window.NexaRoute;
  assert.ok(NR && typeof NR._render === 'function', 'test hooks exposed');
  return { NR, els, warns };
}

function seedSnap(NR) {
  NR._setProviders([{ id: 'p1', name: 'P One', type: 'openai_compatible', model_count: 1, enabled: true, base_url: 'http://x', has_secret: false }]);
  NR._setSnap({
    deployments: [{ id: 'd1', provider_id: 'p1', provider_name: 'P One', model: 'm1' }],
    health: [{ deployment: 'd1', status: 'healthy' }],
    events: [],
    config: { routing: { strategy: 'ready_mesh' }, probe: {}, tokens: SECRET },
    provider_health: [], provider_stats: [], provider_pressure: [], scope_health: [],
    virtual_endpoints: [], route_profiles: [], candidate_pools: [], fallback_chains: [],
  });
}

describe('F1 dashboard DOM isolation', () => {
  it('full DOM renders normally with no faults', () => {
    const { NR, els } = loadApp();
    seedSnap(NR);
    NR._uiReset();
    NR._render();
    assert.equal(els.get('#sHealthy').textContent, '1');
    assert.match(els.get('#providerGrid').innerHTML, /P One/);
    assert.equal(NR._uiFaults().count, 0);
  });

  it('missing #sHealthy does not stop providers; later renders continue; diagnostic bounded, no secrets', () => {
    const { NR, els, warns } = loadApp(new Set(['#sHealthy']));
    seedSnap(NR);
    NR._uiReset();
    NR._render();
    // Unrelated section still rendered.
    assert.match(els.get('#providerGrid').innerHTML, /P One/);
    // Bounded, useful diagnostic recorded.
    const f1 = NR._uiFaults();
    assert.ok(f1.count >= 1, 'fault recorded');
    assert.match(f1.lastSection, /render/);
    assert.ok(f1.lastError.length > 0 && f1.lastError.length <= 120, 'diagnostic bounded');
    assert.ok(warns.length >= 1, 'console.warn diagnostic emitted');
    assert.ok(warns.join('\n').length <= 4096, 'warnings bounded');
    const leaked = JSON.stringify(f1) + '\n' + warns.join('\n');
    assert.ok(!leaked.includes(SECRET), 'no secrets or payloads in diagnostics');
    // Future tick/render still applies (simulate second tick with new data).
    NR._setProviders([
      { id: 'p1', name: 'P One', type: 'openai_compatible', model_count: 1, enabled: true, base_url: 'http://x', has_secret: false },
      { id: 'p2', name: 'P Two', type: 'openai_compatible', model_count: 1, enabled: true, base_url: 'http://y', has_secret: false },
    ]);
    NR._render();
    assert.match(els.get('#providerGrid').innerHTML, /P Two/);
  });

  it('missing #toast never throws', () => {
    const { NR } = loadApp(new Set(['#toast']));
    seedSnap(NR);
    NR._uiReset();
    assert.doesNotThrow(() => NR._render());
  });
});
