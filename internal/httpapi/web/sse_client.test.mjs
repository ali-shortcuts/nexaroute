// F8 regression tests: bounded jittered SSE retry + malformed-frame status.
// Loads the real internal/httpapi/web/app.js in a stubbed browser sandbox and
// drives its production consumeLiveEvents/parse/status path with injected fake
// clock (now), fake random (rand) and fake sleep. No network, no /tmp.
import { describe, it, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const APP_SRC = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const SECRET_MARKER = 'SECRET_MARKER_xyz_must_never_surface';

function makeEl() {
  const el = {
    textContent: '', className: '', innerHTML: '', value: '', checked: false,
    disabled: false, hidden: false,
    dataset: {}, style: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    setAttribute() {}, getAttribute() { return null; }, removeAttribute() {},
    remove() {}, addEventListener() {}, removeEventListener() {},
    querySelector() { return makeEl(); }, querySelectorAll() { return []; },
    focus() {}, click() {}, closest() { return null; },
    childElementCount: 0, children: [], scrollTop: 0, scrollHeight: 0,
    clientWidth: 0, clientHeight: 0, offsetWidth: 0,
  };
  el.appendChild = c => { el.children.push(c); el.childElementCount = el.children.length; };
  return el;
}

let currentFetch = () => new Promise(() => {}); // hang by default so boot stays parked
const toasts = [];
const consoleCalls = [];
const toastEl = makeEl();
let toastText = '';
Object.defineProperty(toastEl, 'textContent', {
  get() { return toastText; },
  set(v) { toastText = String(v); toasts.push(String(v)); },
  configurable: true,
});

const sandbox = {
  console: {
    log: (...a) => consoleCalls.push(['log', ...a]),
    warn: (...a) => consoleCalls.push(['warn', ...a]),
    error: (...a) => consoleCalls.push(['error', ...a]),
    info: (...a) => consoleCalls.push(['info', ...a]),
    debug: (...a) => consoleCalls.push(['debug', ...a]),
  },
  document: {
    querySelector: sel => (sel === '#toast' ? toastEl : makeEl()),
    querySelectorAll: () => [],
    createElement: () => makeEl(),
    createElementNS: () => makeEl(),
    addEventListener() {},
    body: makeEl(),
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
  fetch: (...a) => currentFetch(...a),
  matchMedia: () => ({ matches: false }),
  addEventListener() {},
  removeEventListener() {},
  isSecureContext: false,
};
vm.createContext(sandbox);

let NR;
let nowMs = 1_000_000;

before(() => {
  vm.runInContext(APP_SRC, sandbox, { filename: 'app.js' });
  NR = sandbox.window.NexaRoute;
  assert.ok(NR, 'window.NexaRoute must be exposed by app.js');
  assert.equal(typeof NR.sseStats, 'function');
});

function reset() {
  NR._sseResetStats();
  NR._stopLiveEvents();
  NR._sseHooks.rand = Math.random;
  NR._sseHooks.sleep = () => Promise.resolve();
  NR._sseHooks.now = () => nowMs;
  toasts.length = 0;
  consoleCalls.length = 0;
}

const okResp = body => ({ ok: true, status: 200, json: async () => body });

describe('F8 client SSE retry + malformed-frame status', () => {
  it('retry delay stays bounded, capped, jittered and deterministic', () => {
    reset();
    const noJitter = [];
    for (let f = 1; f <= 30; f++) noJitter.push(NR.sseRetryDelayMs(f, () => 0));
    for (const d of noJitter) assert.ok(d >= 250 && d <= 8000, `delay ${d} in [250,8000]`);
    // Cap: 1500+7*1000 overflows, so failure 7+ pins at 8000.
    assert.equal(noJitter[6], 8000);
    assert.ok(noJitter.slice(6).every(d => d === 8000), 'stays capped at 8000');
    for (let i = 1; i < 6; i++) assert.ok(noJitter[i] >= noJitter[i - 1], 'backoff grows before cap');
    // Jitter: same failure count, different injected rand -> different delay.
    const hi = NR.sseRetryDelayMs(3, () => 0);
    const lo = NR.sseRetryDelayMs(3, () => 0.9999);
    assert.ok(lo < hi, `jitter separates delays (${lo} < ${hi})`);
    assert.ok(lo >= 250, 'jitter floor holds');
    // Deterministic: identical inputs -> identical output; hook rand honored.
    assert.equal(NR.sseRetryDelayMs(3, () => 0.25), NR.sseRetryDelayMs(3, () => 0.25));
    NR._sseHooks.rand = () => 0.5;
    assert.equal(NR.sseRetryDelayMs(4), NR.sseRetryDelayMs(4, () => 0.5));
  });

  it('malformed frames bump the counter; valid events still flow; no payload leak', async () => {
    reset();
    const enc = new TextEncoder();
    const frames = [
      'id: 41\ndata: {"kind":"route_ok","seq":41,"request_id":"r1","deployment":"d1","latency_ms":5}\n\n',
      `data: {"kind": "broken", ${SECRET_MARKER}\n\n`,
      'id: 42\ndata: {"kind":"route_ok","seq":42,"request_id":"r2","deployment":"d1","latency_ms":6}\n\n',
    ];
    const chunks = [frames[0].slice(0, 17), frames[0].slice(17) + frames[1], frames[2]];
    const seenUrls = [];
    currentFetch = async url => {
      seenUrls.push(String(url));
      if (String(url).includes('/admin/api/events/stream')) {
        let i = 0;
        return {
          ok: true, status: 200,
          body: { getReader: () => ({ read: async () => (i < chunks.length
            ? { done: false, value: enc.encode(chunks[i++]) }
            : { done: true }) }) },
        };
      }
      return okResp({ events: [] });
    };
    const sleeps = [];
    NR._sseHooks.sleep = ms => { sleeps.push(ms); NR._stopLiveEvents(); return Promise.resolve(); };

    await NR._consumeLiveEvents();

    const st = NR.sseStats();
    assert.equal(st.malformedFrames, 1, 'exactly one malformed frame counted');
    assert.equal(NR._liveSeq(), 42, 'valid events after the malformed frame still applied');
    assert.equal(st.consecutiveFailures, 1, 'clean stream close counts as one failure');
    assert.ok(sleeps.length === 1 && sleeps[0] >= 250 && sleeps[0] <= 8000, 'retry bounded');
    assert.ok(seenUrls.some(u => u.includes('/admin/api/events/stream')), 'server resume endpoint preserved');
    assert.ok(seenUrls.some(u => u.includes('/admin/api/snapshot')), 'polling fallback preserved');
    assert.ok(toasts.some(t => t.includes('reconnecting')), 'degraded signal surfaced');
    const leaked = [...toasts, JSON.stringify(st), JSON.stringify(consoleCalls)].join('\n');
    assert.ok(!leaked.includes(SECRET_MARKER), 'raw payload never surfaces in toast/stats/logs');
  });

  it('successful recovery resets failures and signals exactly once', () => {
    reset();
    NR._sseNoteFailure('boom');
    NR._sseNoteFailure('boom');
    assert.equal(NR.sseStats().consecutiveFailures, 2);
    assert.equal(NR.sseStats().state, 'reconnecting');
    assert.equal(toasts.filter(t => t.includes('reconnecting')).length, 1, 'degraded toast throttled, not flooded');
    NR._sseNoteOpen();
    const st = NR.sseStats();
    assert.equal(st.consecutiveFailures, 0);
    assert.equal(st.state, 'live');
    assert.equal(st.lastError, '');
    assert.equal(toasts.filter(t => t.includes('reconnected')).length, 1, 'recovery signaled once');
    NR._sseNoteOpen();
    assert.equal(toasts.filter(t => t.includes('reconnected')).length, 1, 'no repeat toast when already live');
    assert.deepEqual(Object.keys(NR.sseStats()).sort(),
      ['consecutiveFailures', 'lastError', 'lastRetryMs', 'malformedFrames', 'state', 'transport'],
      'status API stays small and safe');
  });

  it('cleanup stops the retry loop and resets status', async () => {
    reset();
    NR._sseHooks.rand = () => 0; // deterministic: 2500, 3500, ...
    let fetches = 0;
    currentFetch = async () => { fetches++; throw new Error('down'); };
    const sleeps = [];
    NR._sseHooks.sleep = ms => {
      sleeps.push(ms);
      if (sleeps.length >= 5) NR._stopLiveEvents();
      return Promise.resolve();
    };

    await NR._consumeLiveEvents();

    assert.equal(sleeps.length, 5, 'loop ran until stopped');
    assert.ok(sleeps.every(d => d >= 250 && d <= 8000), 'every retry bounded');
    assert.deepEqual(sleeps, [2500, 3500, 4500, 5500, 6500], 'deterministic under injected rand');
    assert.equal(NR.sseStats().consecutiveFailures, 5);
    assert.equal(NR.sseStats().state, 'idle', 'stop clears reconnecting state');
    const frozen = fetches;
    await new Promise(r => setImmediate(r));
    assert.equal(fetches, frozen, 'no further fetch after stop (loop cleaned up)');
    NR._sseResetStats();
    assert.deepEqual([NR.sseStats().consecutiveFailures, NR.sseStats().malformedFrames, NR.sseStats().lastRetryMs],
      [0, 0, 0], 'reset clears counters');
  });
});
