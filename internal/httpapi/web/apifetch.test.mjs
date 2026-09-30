// F14 regression tests: one canonical window.NexaRoute.apiFetch implementation.
// Loads the real internal/httpapi/web/app.js AND control-plane-v2.js in a
// stubbed browser sandbox (app.js first, matching index.html script order)
// and drives the production fetch/auth path with a scripted window.fetch.
// Covers: successful request, JSON/error handling, and the 401/admin-key
// flow through BOTH UI surfaces (dashboard globals + v2-delegated entry
// points). No network, no /tmp, no randomness.
import { describe, it, before, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const APP_SRC = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const V2_SRC = fs.readFileSync(new URL('./control-plane-v2.js', import.meta.url), 'utf8');

function makeEl() {
  const el = {
    textContent: '', className: '', innerHTML: '', value: '', checked: false,
    disabled: false, hidden: false, id: '', href: '',
    dataset: {}, style: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    setAttribute() {}, getAttribute() { return null; }, removeAttribute() {},
    remove() {}, after() {}, before() {}, prepend() {}, append() {},
    appendChild() {}, addEventListener() {}, removeEventListener() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
    closest() { return null; },
    focus() {}, click() {}, scrollIntoView() {},
    childElementCount: 0, children: [], scrollTop: 0, scrollHeight: 0,
    clientWidth: 0, clientHeight: 0, offsetWidth: 0, offsetParent: null,
    onclick: null, onchange: null, oninput: null, onkeydown: null,
  };
  return el;
}

const toasts = [];
const consoleCalls = [];
const toastEl = makeEl();
let toastText = '';
Object.defineProperty(toastEl, 'textContent', {
  get() { return toastText; },
  set(v) { toastText = String(v); toasts.push(String(v)); },
  configurable: true,
});
const adminKeyEl = makeEl();

const sessionStore = new Map();
const localStore = new Map();

const sandbox = {
  console: {
    log: (...a) => consoleCalls.push(['log', ...a]),
    warn: (...a) => consoleCalls.push(['warn', ...a]),
    error: (...a) => consoleCalls.push(['error', ...a]),
    info: (...a) => consoleCalls.push(['info', ...a]),
    debug: (...a) => consoleCalls.push(['debug', ...a]),
  },
  document: {
    readyState: 'complete',
    querySelector: sel => (sel === '#toast' ? toastEl : sel === '#adminKey' ? adminKeyEl : makeEl()),
    querySelectorAll: () => [],
    createElement: () => makeEl(),
    createElementNS: () => makeEl(),
    addEventListener() {},
    removeEventListener() {},
    body: makeEl(),
    documentElement: { dataset: {}, lang: 'en', dir: 'ltr', setAttribute() {} },
    hidden: false,
  },
  sessionStorage: {
    getItem: k => (sessionStore.has(k) ? sessionStore.get(k) : null),
    setItem: (k, v) => { sessionStore.set(k, String(v)); },
    removeItem: k => { sessionStore.delete(k); },
  },
  localStorage: {
    getItem: k => (localStore.has(k) ? localStore.get(k) : null),
    setItem: (k, v) => { localStore.set(k, String(v)); },
    removeItem: k => { localStore.delete(k); },
  },
  location: { origin: 'http://127.0.0.1:8080' },
  navigator: {},
  MutationObserver: class { constructor() {} observe() {} disconnect() {} },
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
// Default fetch hangs so the app.js boot IIFE parks before any DOM-heavy
// polling/SSE work; each test installs a scripted fetch instead.
let fetchCalls = [];
let fetchImpl = () => new Promise(() => {});
sandbox.window = {
  fetch: (...a) => fetchImpl(...a),
  matchMedia: () => ({ matches: false }),
  addEventListener() {},
  removeEventListener() {},
  isSecureContext: false,
};
vm.createContext(sandbox);

const okResp = body => ({ ok: true, status: 200, json: async () => body });
const errResp = (status, body) => ({ ok: false, status, json: async () => body });

before(() => {
  // Load order matches index.html: app.js first, then control-plane-v2.js.
  vm.runInContext(APP_SRC, sandbox, { filename: 'app.js' });
  assert.ok(sandbox.window.NexaRoute, 'window.NexaRoute must be exposed by app.js');
  assert.equal(typeof sandbox.window.NexaRoute.apiFetch, 'function', 'canonical apiFetch exposed');
  vm.runInContext(V2_SRC, sandbox, { filename: 'control-plane-v2.js' });
  assert.ok(sandbox.window.NexaUI, 'window.NexaUI must be exposed by control-plane-v2.js');
  assert.equal(typeof sandbox.window.NexaUI.requestAdminKey, 'function', 'v2 provides requestAdminKey');
});

beforeEach(() => {
  sessionStore.clear();
  fetchCalls = [];
  toasts.length = 0;
  consoleCalls.length = 0;
  adminKeyEl.value = '';
  fetchImpl = () => new Promise(() => {});
  // Deterministic admin-key prompt stub (replaces the dialog-based one).
  sandbox.window.NexaUI.requestAdminKey = async () => null;
  // Reset canonical key state between tests.
  sandbox.window.NexaRoute.setAdminKey('');
  sessionStore.clear();
  adminKeyEl.value = '';
});

function scriptFetch(handler) {
  let n = 0;
  fetchImpl = async (url, opt = {}) => {
    n++;
    fetchCalls.push({ url: String(url), headers: { ...(opt.headers || {}) }, method: opt.method || 'GET', body: opt.body });
    return handler(n, url, opt);
  };
}

// Both UI surfaces must funnel through the canonical implementation:
// [name, fetchFn, apiFn] — dashboard globals (post-v2 delegation included).
function surfaces() {
  const NR = sandbox.window.NexaRoute;
  return [
    ['canonical window.NexaRoute.apiFetch', NR.apiFetch, NR.api],
    ['compat global apiFetch/api', sandbox.apiFetch, sandbox.api],
  ];
}

describe('F14 single canonical apiFetch', () => {
  it('exactly one implementation owns fetch/auth behavior; v2 only delegates', () => {
    const NR = sandbox.window.NexaRoute;
    assert.equal(typeof NR.apiFetch, 'function');
    assert.equal(typeof NR.api, 'function');
    assert.equal(typeof NR.getAdminKey, 'function');
    assert.equal(typeof NR.setAdminKey, 'function');
    // Compatibility entry points still exist (used across both UI surfaces).
    assert.equal(typeof sandbox.apiFetch, 'function', 'global apiFetch preserved');
    assert.equal(typeof sandbox.api, 'function', 'global api preserved');
    // v2 must not reimplement fetch/auth: no direct fetch, no admin-key header
    // write (the remaining "x-admin-key" mentions are user-facing i18n copy
    // for the key prompt, not header logic).
    assert.ok(!/window\.fetch/.test(V2_SRC), 'control-plane-v2.js contains no window.fetch');
    assert.ok(!/headers\s*\[[^\]]*admin-key/.test(V2_SRC), 'control-plane-v2.js sets no admin-key header');
    assert.ok(V2_SRC.includes('window.NexaRoute'), 'v2 references window.NexaRoute');
    assert.ok(V2_SRC.includes('NexaRoute.apiFetch'), 'v2 delegates to NexaRoute.apiFetch');
  });

  it('successful request on both surfaces: key attached, headers preserved, opt not mutated', async () => {
    const NR = sandbox.window.NexaRoute;
    NR.setAdminKey('  stored-key  ');
    assert.equal(NR.getAdminKey(), 'stored-key', 'key trimmed on set');
    scriptFetch(() => okResp({ ok: true }));
    for (const [name, fetchFn] of surfaces()) {
      fetchCalls = [];
      const frozen = { Authorization: 'Bearer abc' };
      const r = await fetchFn('/admin/api/snapshot', { headers: frozen });
      assert.equal(r.status, 200, `${name}: status`);
      assert.equal(fetchCalls.length, 1, `${name}: single fetch, no 401 retry`);
      assert.equal(fetchCalls[0].headers['x-admin-key'], 'stored-key', `${name}: key attached`);
      assert.equal(fetchCalls[0].headers.Authorization, 'Bearer abc', `${name}: caller headers preserved`);
      assert.deepEqual(frozen, { Authorization: 'Bearer abc' }, `${name}: caller opt not mutated`);
    }
  });

  it('api() JSON/error handling identical on both surfaces', async () => {
    for (const [name, , apiFn] of surfaces()) {
      scriptFetch(() => okResp({ providers: [{ id: 'p1' }] }));
      assert.deepEqual(await apiFn('/admin/api/providers'), { providers: [{ id: 'p1' }] }, `${name}: success body`);
      scriptFetch(() => errResp(500, { error: { message: 'backend boom' } }));
      await assert.rejects(() => apiFn('/x'), /backend boom/, `${name}: server message surfaced`);
      scriptFetch(() => errResp(500, { unexpected: true }));
      await assert.rejects(() => apiFn('/x'), /HTTP 500/, `${name}: HTTP fallback`);
      scriptFetch(() => ({ ok: false, status: 503, json: async () => { throw new SyntaxError('bad json'); } }));
      await assert.rejects(() => apiFn('/x'), /HTTP 503/, `${name}: malformed error body`);
      scriptFetch(() => ({ ok: true, status: 200, json: async () => { throw new SyntaxError('bad json'); } }));
      // JSON-serialized compare: the {} is allocated inside the vm realm, so
      // its prototype differs from this realm's Object.prototype.
      assert.equal(JSON.stringify(await apiFn('/x')), '{}', `${name}: malformed ok body yields {}`);
    }
  });

  it('401 key flow on both surfaces: retry, trim, persist, input sync', async () => {
    for (const [name, fetchFn] of surfaces()) {
      sandbox.window.NexaRoute.setAdminKey('');
      sessionStore.clear();
      adminKeyEl.value = '';
      fetchCalls = [];
      let prompts = 0;
      sandbox.window.NexaUI.requestAdminKey = async () => { prompts++; return '  new-admin-key  '; };
      scriptFetch(n => (n === 1 ? errResp(401, { error: { message: 'unauthorized' } }) : okResp({ ok: true })));
      const r = await fetchFn('/admin/api/snapshot');
      assert.equal(r.status, 200, `${name}: retried request succeeds`);
      assert.equal(prompts, 1, `${name}: key requested exactly once`);
      assert.equal(fetchCalls.length, 2, `${name}: exactly one retry`);
      assert.ok(!('x-admin-key' in fetchCalls[0].headers), `${name}: first attempt sent no key`);
      assert.equal(fetchCalls[1].headers['x-admin-key'], 'new-admin-key', `${name}: retry carries trimmed key`);
      assert.equal(sandbox.window.NexaRoute.getAdminKey(), 'new-admin-key', `${name}: key state updated`);
      assert.equal(sessionStore.get('nexaroute_admin_key'), 'new-admin-key', `${name}: key persisted`);
      assert.equal(adminKeyEl.value, 'new-admin-key', `${name}: #adminKey input synced`);
      // Follow-up calls reuse the stored key without prompting again.
      prompts = 0;
      scriptFetch(() => okResp({ ok: true }));
      await fetchFn('/admin/api/snapshot');
      assert.equal(prompts, 0, `${name}: no re-prompt once key stored`);
    }
  });

  it('401 without key (declined) returns 401 with a single fetch on both surfaces', async () => {
    for (const [name, fetchFn] of surfaces()) {
      sandbox.window.NexaRoute.setAdminKey('');
      sessionStore.clear();
      fetchCalls = [];
      sandbox.window.NexaUI.requestAdminKey = async () => null;
      scriptFetch(() => errResp(401, { error: { message: 'unauthorized' } }));
      const r = await fetchFn('/admin/api/snapshot');
      assert.equal(r.status, 401, `${name}: 401 returned`);
      assert.equal(fetchCalls.length, 1, `${name}: no retry when key declined`);
      assert.equal(sessionStore.has('nexaroute_admin_key'), false, `${name}: nothing persisted`);
    }
  });

  it('401 without a key provider never prompts and never retries', async () => {
    sandbox.window.NexaRoute.setAdminKey('');
    delete sandbox.window.NexaUI.requestAdminKey;
    try {
      scriptFetch(() => errResp(401, { error: { message: 'unauthorized' } }));
      const r = await sandbox.window.NexaRoute.apiFetch('/admin/api/snapshot');
      assert.equal(r.status, 401);
      assert.equal(fetchCalls.length, 1, 'no retry without provider');
    } finally {
      sandbox.window.NexaUI.requestAdminKey = async () => null;
    }
  });

  it('no secret surfaces in toasts, console or status projections', async () => {
    const SECRET = 'SECRET_MARKER_xyz_must_never_surface';
    sandbox.window.NexaRoute.setAdminKey(SECRET);
    sandbox.window.NexaUI.requestAdminKey = async () => SECRET;
    scriptFetch(() => okResp({ ok: true }));
    await sandbox.window.NexaRoute.apiFetch('/admin/api/snapshot');
    await sandbox.window.NexaRoute.api('/admin/api/snapshot');
    // Prompted rotation also passes key material only via headers/storage.
    sandbox.window.NexaRoute.setAdminKey('');
    scriptFetch(n => (n === 1 ? errResp(401, { error: { message: 'denied' } }) : okResp({ ok: true })));
    await sandbox.window.NexaRoute.apiFetch('/admin/api/snapshot');
    // Declined flow persists nothing new.
    sandbox.window.NexaUI.requestAdminKey = async () => null;
    sandbox.window.NexaRoute.setAdminKey('');
    scriptFetch(() => errResp(401, { error: { message: 'denied' } }));
    await sandbox.window.NexaRoute.apiFetch('/admin/api/snapshot');
    assert.equal(sessionStore.get('nexaroute_admin_key'), '', 'declined flow persists nothing');
    const leaked = [...toasts, JSON.stringify(consoleCalls)].join('\n');
    assert.ok(!leaked.includes(SECRET), 'admin key never written to toasts/console');
  });
});
