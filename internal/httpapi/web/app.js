/* NexaRoute control plane — dashboard logic (vanilla JS, no dependencies) */
'use strict';

let snap = { deployments: [], health: [], events: [], config: {} };
let providerSummaries = [];
let editor = { mode: 'add', originalId: '', provider: null, detected: [], selected: new Set(), modelMeta: new Map(), secretDirty: false, secretSource: 'none' };
let serverPresets = null;
let paused = false;
let consoleFilter = 'all';
let consoleUnread = 0;
let latencyHistory = [];
const ringNodes = new Map();
const ringLinks = new Map();
let ringMore = null;
let liveSeq = 0;
let liveEventsAbort = null;
let liveTransport = 'polling';

const $ = q => document.querySelector(q), $$ = q => [...document.querySelectorAll(q)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;' }[c]));
const fmtInt = n => Number(n || 0).toLocaleString('en-US');
const fmtCompact = n => { if (!Number.isFinite(n) || n <= 0) return '0'; if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B'; if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M'; if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K'; return String(n); };
const fmtMs = n => (Number.isFinite(Number(n)) && Number(n) > 0) ? Math.round(Number(n)) + ' ms' : '—';

/* ---------- F1: section-level fault isolation for missing DOM nodes ----------
   `$()` stays a direct querySelector (no fake elements). Callers that need a
   missing-node tolerant read use needEl()/setText() below, which record a
   bounded diagnostic (selector + short message only, never values/payloads/
   secrets) and return null/false. render()/tick() wrap each section in
   uiSection() so one absent/renamed node aborts only its own section. */
const uiFaults = { count: 0, lastSection: '', lastError: '' };
function uiWarn(section, err) {
  const name = String(section || 'ui').slice(0, 64);
  let msg = '';
  try { msg = String((err && err.message) || err || 'ui fault'); } catch { msg = 'ui fault'; }
  msg = msg.slice(0, 120);
  uiFaults.count++;
  uiFaults.lastSection = name;
  uiFaults.lastError = msg;
  try { if (console && typeof console.warn === 'function') console.warn('[nexaroute:' + name + '] ' + msg); } catch {}
}
function uiSection(name, fn) {
  try { fn(); } catch (e) { uiWarn(name, e); }
}
function needEl(sel, section) {
  let el = null;
  try { el = document.querySelector(sel); } catch (e) { uiWarn(section || sel, e); return null; }
  if (!el) uiWarn(section || sel, 'missing element ' + String(sel).slice(0, 64));
  return el;
}
function setText(sel, val, section) {
  const el = needEl(sel, section || sel);
  if (!el) return false;
  try { el.textContent = val; return true; } catch (e) { uiWarn(section || sel, e); return false; }
}
function bind(sel, prop, fn, section) {
  const el = needEl(sel, section || ('bind:' + sel));
  if (!el) return null;
  try {
    el[prop] = (...a) => uiSection(section || sel, () => {
      const r = fn(...a);
      if (r && typeof r.catch === 'function') r.catch(e => uiWarn(section || sel, e));
    });
  } catch (e) { uiWarn(section || sel, e); return null; }
  return el;
}
function uiResetFaults() { uiFaults.count = 0; uiFaults.lastSection = ''; uiFaults.lastError = ''; }

let adminKey = sessionStorage.getItem('nexaroute_admin_key') || '';
async function apiFetch(url, opt = {}) {
  opt = { ...opt, headers: { ...(opt.headers || {}) } };
  if (adminKey) opt.headers['x-admin-key'] = adminKey;
  let r = await window.fetch(url, opt);
  if (r.status === 401 && window.NexaUI?.requestAdminKey) {
    const k = await window.NexaUI.requestAdminKey();
    if (k) {
      adminKey = k.trim();
      sessionStorage.setItem('nexaroute_admin_key', adminKey);
      $('#adminKey').value = adminKey;
      opt.headers['x-admin-key'] = adminKey;
      r = await window.fetch(url, opt);
    }
  }
  return r;
}
async function api(url, opt = {}) {
  const r = await apiFetch(url, opt);
  let b = {};
  try { b = await r.json(); } catch {}
  if (!r.ok) throw new Error(b?.error?.message || `HTTP ${r.status}`);
  return b;
}
function setLiveTransport(mode) {
  liveTransport = mode;
  $$('.live').forEach(el => { el.textContent = mode === 'sse' ? 'LIVE' : 'POLLING'; el.classList.toggle('polling', mode !== 'sse'); });
  const rs = document.querySelector('#ringStatus');
  if (rs) rs.dataset.transport = mode;
  updateRingStatus();
}
/* ---------- F8: bounded SSE retry with jitter + malformed-frame status ----------
   Client-only. The server resume protocol (?since=liveSeq) and the snapshot
   polling fallback are unchanged. Retry delays stay within
   [SSE_RETRY_MIN_MS, SSE_RETRY_MAX_MS] and subtract a small random jitter so
   reconnecting tabs do not thunder. Malformed frames only bump a counter —
   valid events keep flowing. No raw payloads or secrets are ever logged or
   exposed; sseStats() returns counts and transport state only. */
const SSE_RETRY_BASE_MS = 1500;
const SSE_RETRY_STEP_MS = 1000;
const SSE_RETRY_MAX_MS = 8000;
const SSE_RETRY_MIN_MS = 250;
const SSE_RETRY_JITTER_MS = 500;
const SSE_DEGRADED_TOAST_MS = 15000;
const sseHooks = {
  rand: () => Math.random(),
  sleep: ms => new Promise(resolve => setTimeout(resolve, ms)),
  now: () => Date.now(),
};
const sseStatsState = {
  state: 'idle',
  consecutiveFailures: 0,
  malformedFrames: 0,
  lastRetryMs: 0,
  lastError: '',
  degradedToastAt: 0,
};
function sseRetryDelayMs(failures, randFn) {
  const r = typeof randFn === 'function' ? randFn : sseHooks.rand;
  const n = Math.max(0, Math.floor(Number(failures) || 0));
  const base = Math.min(SSE_RETRY_MAX_MS, SSE_RETRY_BASE_MS + n * SSE_RETRY_STEP_MS);
  let jitter = 0;
  try { jitter = Math.floor(Number(r()) * SSE_RETRY_JITTER_MS); } catch { jitter = 0; }
  if (!Number.isFinite(jitter) || jitter < 0) jitter = 0;
  return Math.max(SSE_RETRY_MIN_MS, Math.min(SSE_RETRY_MAX_MS, base - jitter));
}
function sseParseFrame(frame) {
  if (frame == null || String(frame).startsWith(':')) return { skip: true };
  const lines = String(frame).split('\n');
  const dataLine = lines.find(x => x.startsWith('data: '));
  if (!dataLine) return { skip: true };
  const idLine = lines.find(x => x.startsWith('id: '));
  try {
    return { event: JSON.parse(dataLine.slice(6)), idLine };
  } catch {
    // Count only; never log the raw payload (it may carry request content).
    sseStatsState.malformedFrames++;
    return { malformed: true };
  }
}
function sseStats() {
  return {
    transport: liveTransport,
    state: sseStatsState.state,
    consecutiveFailures: sseStatsState.consecutiveFailures,
    malformedFrames: sseStatsState.malformedFrames,
    lastRetryMs: sseStatsState.lastRetryMs,
    lastError: sseStatsState.lastError,
  };
}
function sseResetStats() {
  sseStatsState.consecutiveFailures = 0;
  sseStatsState.malformedFrames = 0;
  sseStatsState.lastRetryMs = 0;
  sseStatsState.lastError = '';
  sseStatsState.degradedToastAt = 0;
  if (sseStatsState.state !== 'live') sseStatsState.state = 'idle';
}
function stopLiveEvents() {
  try { if (liveEventsAbort) liveEventsAbort.abort(); } catch {}
  liveEventsAbort = null;
  sseStatsState.state = 'idle';
}
function sseNoteOpen() {
  const wasDegraded = sseStatsState.consecutiveFailures > 0;
  sseStatsState.consecutiveFailures = 0;
  sseStatsState.lastRetryMs = 0;
  sseStatsState.lastError = '';
  sseStatsState.state = 'live';
  if (wasDegraded) { try { toast('Live events reconnected'); } catch {} }
}
function sseNoteFailure(message) {
  sseStatsState.consecutiveFailures++;
  sseStatsState.lastError = String(message || 'stream failed').slice(0, 120);
  sseStatsState.state = 'reconnecting';
  const now = sseHooks.now();
  if (now - sseStatsState.degradedToastAt >= SSE_DEGRADED_TOAST_MS) {
    sseStatsState.degradedToastAt = now;
    try { toast('Live events reconnecting — showing polled snapshot', true); } catch {}
  }
  return sseRetryDelayMs(sseStatsState.consecutiveFailures);
}
async function pollLiveEventsFallback(signal) {
  // Polling fallback: reuse the authoritative snapshot endpoint. Only new
  // seqs animate the ring (de-duplicated by seq/request_id in drainRingEvent).
  try {
    const b = await api('/admin/api/snapshot?limit=20&events=40');
    const evs = b.events || [];
    let maxSeq = liveSeq;
    for (const e of evs) {
      const seq = Number(e.seq || 0);
      if (seq) maxSeq = Math.max(maxSeq, seq);
    }
    const fresh = evs.filter(e => Number(e.seq || 0) > liveSeq);
    liveSeq = maxSeq;
    if (fresh.length && !paused) {
      snap.events = [...(snap.events || []), ...fresh.filter(e => !(snap.events || []).some(x => x.seq === e.seq))].slice(-100);
      for (const e of fresh) drainRingEvent(e);
      render();
    }
  } catch {}
  void signal;
}
async function consumeLiveEvents() {
  if (liveEventsAbort) liveEventsAbort.abort();
  liveEventsAbort = new AbortController();
  while (liveEventsAbort && !liveEventsAbort.signal.aborted) {
    try {
      const q = liveSeq ? `?since=${encodeURIComponent(liveSeq)}&limit=100` : '?limit=100';
      const response = await apiFetch('/admin/api/events/stream' + q, { signal: liveEventsAbort.signal });
      if (!response.ok) throw new Error('event stream HTTP ' + response.status);
      setLiveTransport('sse');
      sseNoteOpen();
      const reader = response.body?.getReader();
      if (!reader) throw new Error('event stream body unavailable');
      const decoder = new TextDecoder(); let buffer = '';
      while (true) {
        const part = await reader.read();
        if (part.done) throw new Error('event stream closed');
        buffer += decoder.decode(part.value, { stream: true });
        let cut;
        while ((cut = buffer.indexOf('\n\n')) >= 0) {
          const frame = buffer.slice(0, cut); buffer = buffer.slice(cut + 2);
          const parsed = sseParseFrame(frame);
          if (parsed.skip || parsed.malformed) continue;
          try {
            const event = parsed.event;
            const seq = Number(parsed.idLine?.slice(4) || event.seq || 0);
            if (seq && seq <= liveSeq) continue;
            if (seq) liveSeq = seq;
            else if (event.request_id && ringAnim.seenReq.has(event.request_id + '|' + event.kind + '|' + (event.deployment || ''))) continue;
            snap.events = [...(snap.events || []).filter(x => x.seq !== event.seq), event].slice(-100);
            drainRingEvent(event);
            render();
          } catch {}
        }
      }
    } catch (err) {
      if (!liveEventsAbort || liveEventsAbort.signal.aborted) return;
      setLiveTransport('polling');
      // Polling fallback drives the ring from real snapshot events only —
      // never synthetic timers. Back off gently while SSE is unavailable.
      await pollLiveEventsFallback(liveEventsAbort.signal);
      const wait = sseNoteFailure(err && err.message);
      sseStatsState.lastRetryMs = wait;
      await sseHooks.sleep(wait);
    }
  }
}
function toast(m, bad = false) {
  const t = needEl('#toast', 'toast');
  if (!t) return;
  try {
    t.textContent = m;
    t.className = 'toast show ' + (bad ? 'bad' : '');
    clearTimeout(toast.t);
    toast.t = setTimeout(() => { try { t.className = 'toast'; } catch (e) { uiWarn('toast', e); } }, 2600);
  } catch (e) { uiWarn('toast', e); }
}
function copyText(text, btn) {
  const done = () => { if (btn) { const o = btn.textContent; btn.textContent = 'Copied ✓'; setTimeout(() => btn.textContent = o, 1400); } toast('Copied to clipboard'); };
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done).catch(() => fallbackCopy(text, done));
  else fallbackCopy(text, done);
}
function fallbackCopy(text, done) {
  const ta = document.createElement('textarea');
  ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
  document.body.appendChild(ta); ta.select();
  try { document.execCommand('copy'); done(); } catch { toast('Copy failed — select the text manually', true); }
  ta.remove();
}

/* ---------- navigation ---------- */
const subtitles = {
  overview: 'Ready Mesh: verified health, session affinity, capacity-aware routing and supervised recovery.',
  console: 'Every routing decision, probe, failover and recovery — as it happens.',
  providers: 'Upstream pools, credentials, endpoints and per-provider capacity.',
  models: 'Per-deployment routing state: health, latency and failure tracking.',
  virtual: 'Virtual Endpoints: stable public model names → Route Profile → Candidate Pool. Change backends without client reconfig.',
  profiles: 'Route Profiles: reusable routing intent and policy.',
  pools: 'Candidate Pools and Fallback Chains: pools define configured candidates; health/compat defines runtime eligible per-request.',
  health: 'Live provider pressure and capability-scoped circuit evidence.',
  cli: 'One-click connection snippets for coding agents and OpenAI-compatible tools.',
  settings: 'Hot-reloaded routing and probe configuration.',
  compat: 'Universal Compatibility Engine: verified model capabilities, repairs and the Claude Code scorecard.'
};
$$('nav button').forEach(b => { b.onclick = () => uiSection('nav', () => {
  $$('nav button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  $$('.tab').forEach(x => x.classList.remove('active'));
  const tab = needEl('#' + b.dataset.tab, 'nav');
  if (tab) tab.classList.add('active');
  setText('#title', b.dataset.title, 'nav');
  setText('#subtitle', subtitles[b.dataset.tab] || '', 'nav');
  if (b.dataset.tab === 'settings') uiSection('settings', fillRuntimeSettings);
  if (b.dataset.tab === 'console') { consoleUnread = 0; const d = needEl('#consoleDot', 'nav'); if (d) d.hidden = true; uiSection('console', renderConsole); }
  if (b.dataset.tab === 'cli') uiSection('cli', renderCLI);
  if (b.dataset.tab === 'compat') uiSection('compat', loadCompat);
  if (b.dataset.tab === 'virtual') uiSection('virtual', renderVirtual);
  if (b.dataset.tab === 'profiles') uiSection('profiles', renderProfiles);
  if (b.dataset.tab === 'pools') uiSection('pools', renderPools);
}); });
bind('#pauseBtn', 'onclick', () => {
  paused = !paused;
  setText('#pauseBtn', paused ? '▶ Resume' : '⏸ Pause', 'pause');
  const pb = needEl('#pauseBtn', 'pause');
  if (pb && pb.classList) pb.classList.toggle('active-btn', paused);
  if (!paused) tick();
}, 'pause');

/* ---------- runtime settings ---------- */
function intVal(id, fallback, min = 0) {
  const el = document.querySelector(id);
  if (!el) { uiWarn('settings', 'missing element ' + String(id).slice(0, 64)); return fallback; }
  const n = parseInt(el.value, 10);
  return Number.isFinite(n) ? Math.max(min, n) : fallback;
}
function fillRuntimeSettings() {
  const r = snap.config?.routing || {}, p = snap.config?.probe || {};
  $('#rtStrategy').value = r.strategy || 'ready_mesh';
  $('#rtFallback').checked = r.fallback_on_unknown_model !== false;
  $('#rtSessionAffinity').checked = r.session_affinity !== false;
  $('#rtSessionTTL').value = r.session_ttl_seconds || 3600;
  $('#rtP2CWindow').value = r.p2c_window || 8;
  $('#rtCapacityWeight').value = Number.isFinite(r.capacity_weight) ? r.capacity_weight : 35;
  $('#rtAttempts').value = r.max_attempts || 4;
  $('#rtMaxInflight').value = r.max_inflight_requests || 128;
  $('#rtFailureThreshold').value = r.failure_threshold || 5;
  $('#rtProviderFailureThreshold').value = r.provider_failure_threshold || 3;
  $('#rtProviderFailureWindow').value = r.provider_failure_window_seconds || 20;
  $('#rtProviderCooldown').value = r.provider_cooldown_seconds || 30;
  $('#rtCapabilityThreshold').value = r.capability_failure_threshold || 2;
  $('#rtCapabilityCooldown').value = r.capability_cooldown_seconds || 300;
  $('#rtCooldown').value = r.cooldown_seconds || 1800;
  $('#rtTimeout').value = r.request_timeout_ms || 120000;
  $('#rtBackoff').value = Number.isFinite(r.retry_backoff_ms) ? r.retry_backoff_ms : 150;
  $('#rtRetryAfter').value = r.max_retry_after_seconds || 60;
  $('#rtHedgingEnabled').checked = !!r.hedging_enabled;
  $('#rtHedgingDelay').value = r.hedging_delay_ms || 1500;
  $('#prEnabled').checked = p.enabled !== false;
  $('#prOnStart').checked = !!p.on_start;
  $('#prInterval').value = p.interval_seconds || 120;
  $('#prReadyLease').value = p.ready_lease_seconds || 300;
  $('#prTimeout').value = p.timeout_ms || 8000;
  $('#prTokens').value = p.max_tokens || 1;
  $('#prConcurrency').value = p.concurrency || 16;
  $('#prRecoveryAttempts').value = p.recovery_attempts || 5;
  $('#prRecoveryRetry').value = Number.isFinite(p.recovery_retry_ms) ? p.recovery_retry_ms : 500;
}
function validateRuntimeSettingsForm() {
  const controls = $$('#settings input[type="number"], #settings select');
  for (const el of controls) {
    if (el.checkValidity()) continue;
    const label = el.closest('.field')?.querySelector(':scope > span')?.textContent?.trim() || el.id || 'runtime setting';
    toast('Invalid runtime setting: ' + label, true);
    el.focus();
    if (typeof el.reportValidity === 'function') el.reportValidity();
    return false;
  }
  return true;
}
bind('#saveRuntimeSettings', 'onclick', async () => {
  if (!validateRuntimeSettingsForm()) return;
  const old = snap.config || {}, r = old.routing || {}, p = old.probe || {};
  const body = {
    routing: {
      ...r,
      strategy: $('#rtStrategy').value,
      fallback_on_unknown_model: $('#rtFallback').checked,
      session_affinity: $('#rtSessionAffinity').checked,
      session_ttl_seconds: intVal('#rtSessionTTL', 3600, 1),
      p2c_window: intVal('#rtP2CWindow', 8, 1),
      capacity_weight: Number.parseFloat($('#rtCapacityWeight').value) || 0,
      max_attempts: intVal('#rtAttempts', 4, 1),
      max_inflight_requests: intVal('#rtMaxInflight', 128, 1),
      failure_threshold: intVal('#rtFailureThreshold', 5, 1),
      provider_failure_threshold: intVal('#rtProviderFailureThreshold', 3, 2),
      provider_failure_window_seconds: intVal('#rtProviderFailureWindow', 20, 1),
      provider_cooldown_seconds: intVal('#rtProviderCooldown', 30, 1),
      capability_failure_threshold: intVal('#rtCapabilityThreshold', 2, 1),
      capability_cooldown_seconds: intVal('#rtCapabilityCooldown', 300, 1),
      cooldown_seconds: intVal('#rtCooldown', 1800, 1),
      request_timeout_ms: intVal('#rtTimeout', 120000, 100),
      retry_backoff_ms: intVal('#rtBackoff', 150, 0),
      max_retry_after_seconds: intVal('#rtRetryAfter', 60, 1),
      hedging_enabled: $('#rtHedgingEnabled').checked,
      hedging_delay_ms: intVal('#rtHedgingDelay', 1500, 50)
    },
    probe: {
      ...p,
      enabled: $('#prEnabled').checked,
      on_start: $('#prOnStart').checked,
      interval_seconds: intVal('#prInterval', 120, 1),
      ready_lease_seconds: intVal('#prReadyLease', 300, 1),
      timeout_ms: intVal('#prTimeout', 8000, 100),
      max_tokens: intVal('#prTokens', 1, 1),
      concurrency: intVal('#prConcurrency', 16, 1),
      recovery_attempts: intVal('#prRecoveryAttempts', 5, 1),
      recovery_retry_ms: intVal('#prRecoveryRetry', 500, 0)
    }
  };
  try {
    const saveBtn = needEl('#saveRuntimeSettings', 'settings');
    if (saveBtn) saveBtn.disabled = true;
    await api('/admin/api/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    toast('Runtime settings saved and reloaded');
    await refresh();
    uiSection('settings', fillRuntimeSettings);
  } catch (e) { toast(e.message, true); }
  finally { const rb = document.querySelector('#saveRuntimeSettings'); if (rb) { try { rb.disabled = false; } catch (e) { uiWarn('settings', e); } } }
}, 'settings');
{ const ak = needEl('#adminKey', 'settings'); if (ak) { try { ak.value = adminKey; } catch (e) { uiWarn('settings', e); } } }
bind('#saveAdminKey', 'onclick', () => {
  const ak = needEl('#adminKey', 'settings');
  adminKey = ak ? ak.value.trim() : '';
  sessionStorage.setItem('nexaroute_admin_key', adminKey);
  toast('Admin key saved for this browser session');
}, 'settings');
bind('#toggleAdminKey', 'onclick', () => toggleSecret('#adminKey', '#toggleAdminKey'), 'settings');

bind('#probeBtn', 'onclick', async () => {
  try {
    const pb = needEl('#probeBtn', 'probe');
    if (pb) { pb.disabled = true; }
    setText('#probeBtn', 'Probing…', 'probe');
    const d = await api('/admin/api/probe?wait=1', { method: 'POST' });
    const r = d.result || {};
    toast(`Probe complete: ${r.passed || 0}/${r.total || 0} passed${r.skipped_cooldown ? `, ${r.skipped_cooldown} cooldown` : ''}`, !!r.failed);
    await refresh();
  } catch (e) { toast(e.message, true); }
  finally { const pb2 = document.querySelector('#probeBtn'); if (pb2) { try { pb2.disabled = false; pb2.textContent = '⚡ Probe all models'; } catch (e) { uiWarn('probe', e); } } }
}, 'probe');

/* ---------- derived state helpers ---------- */
function healthMap() { const m = {}; for (const h of snap.health || []) m[h.deployment] = h; return m; }
function healthCounts() {
  if (snap.health_counts) return snap.health_counts;
  const counts = {};
  for (const h of snap.health || []) counts[h.status] = (counts[h.status] || 0) + 1;
  return counts;
}

/* ---------- render ---------- */
function render() {
  const h = healthMap(), ds = snap.deployments || [];
  uiSection('render:core', () => {
    setText('#routerStrategyLabel', snap.config?.routing?.strategy || 'ready_mesh', 'render:core');
    setText('#coreSub', (snap.config?.routing?.strategy || 'ready_mesh').replace(/_/g, ' '), 'render:core');
    setText('#sDeploy', fmtInt(snap.deployment_total ?? ds.length), 'render:core');
  });
  const c = healthCounts();
  const ok = Number(c.healthy || 0), cd = Number(c.cooldown || 0), retired = Number(c.retired || 0);
  const total = snap.deployment_total ?? ds.length;
  uiSection('render:health-kpi', () => {
    setText('#sHealthy', fmtInt(ok), 'render:health-kpi');
    setText('#sCooldown', fmtInt(cd), 'render:health-kpi');
    const bar = needEl('#sHealthBar', 'render:health-kpi');
    if (bar && bar.style) bar.style.width = total ? Math.round(ok / total * 100) + '%' : '0%';
    setText('#sCooldownSub', retired ? `${cd} cooling · ${retired} retired` : (cd ? 'recovering in background' : 'none recovering'), 'render:health-kpi');
    setText('#sSessions', fmtInt(snap.session_count ?? 0), 'render:health-kpi');
    const ls = (snap.events || []).filter(e => e.kind === 'route_ok' && e.latency_ms).map(e => e.latency_ms);
    const avg = ls.length ? Math.round(ls.reduce((a, b) => a + b, 0) / ls.length) : 0;
    setText('#sLatency', avg ? avg + ' ms' : '—', 'render:health-kpi');
  });
  uiSection('render:ring', () => renderRing(ds, h));
  uiSection('render:donut', () => renderDonut(c, total));
  uiSection('render:providers', () => renderProviders(h));
  uiSection('render:models', () => renderModels(ds, h));
  uiSection('render:health', () => renderHealthTab(h));
  uiSection('render:observatory', () => renderRoutingObservatory(h, ds));
  uiSection('render:tabs', () => {
    const compat = document.querySelector('#compat');
    if (compat && compat.classList && compat.classList.contains('active')) renderCompat();
    const virt = document.querySelector('#virtual');
    if (virt && virt.classList && virt.classList.contains('active')) renderVirtual();
    const prof = document.querySelector('#profiles');
    if (prof && prof.classList && prof.classList.contains('active')) renderProfiles();
    const pools = document.querySelector('#pools');
    if (pools && pools.classList && pools.classList.contains('active')) renderPools();
  });
  uiSection('render:console', () => renderConsole());
  uiSection('render:meta', () => {
    setText('#settingsJson', JSON.stringify(snap.config || {}, null, 2), 'render:meta');
    const ch = Number(snap.cache?.hits ?? 0), cm = Number(snap.cache?.misses ?? 0);
    const rate = (ch + cm) ? Math.round(ch / (ch + cm) * 100) + '%' : '—';
    setText('#sCacheRate', rate, 'render:meta');
    setText('#sCacheSub', snap.cache?.hits != null ? (fmtInt(ch) + ' hits / ' + fmtInt(cm) + ' misses') : 'exact-match cache off', 'render:meta');
    const up = Number(snap.usage?.total_prompt_tokens ?? 0), ucp = Number(snap.usage?.total_completion_tokens ?? 0);
    setText('#sTokens', fmtCompact(up + ucp), 'render:meta');
    const cost = Number(snap.usage?.total_estimated_cost_usd ?? 0);
    setText('#sSpendSub', cost > 0 ? ('~$' + (cost >= 1 ? cost.toFixed(2) : cost.toFixed(4)) + ' est. spend') : 'no pricing configured', 'render:meta');
    setText('#footRequests', fmtInt(snap.request_total ?? 0) + ' requests', 'render:meta');
    setText('#brandVersion', 'v' + (snap.version || '0.5') + ' • control plane', 'render:meta');
  });
}

/* ---------- live routing ring v2: persistent SVG topology + real-event animation ----------
   Ring is reconciled by deployment id (no full teardown per refresh). Nodes sit
   on a tilted perspective ellipse. Animation is driven ONLY by real routing
   events from the SSE stream (snapshot polling fallback); particle drift is the
   sole ambient motion and honors prefers-reduced-motion. */
const ringAnim = {
  active: new Map(), inflight: new Map(), seenSeq: new Set(), seenReq: new Set(),
  coalesced: 0, lastAnimatedSeq: 0, particleT: 0, particleRaf: 0, particleLast: 0,
  geom: null, maxAnims: 12
};
function ringReducedMotion() {
  try { return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch { return false; }
}
function ringTilt() { return -18 * Math.PI / 180; }
function ringGeometry(W, H, count) {
  const cx = W / 2, cy = H / 2;
  const base = count > 24 ? Math.min(W * 0.36, 330) : Math.min(W * 0.38, 360);
  const rx = Math.max(90, base);
  const ry = Math.max(52, rx * 0.52);
  return { cx, cy, rx, ry, tilt: ringTilt() };
}
function ringNodePos(i, count, g) {
  const a = Math.PI * 2 * i / Math.max(count, 1) - Math.PI / 2;
  const ex = Math.cos(a) * g.rx, ey = Math.sin(a) * g.ry;
  const c = Math.cos(g.tilt), s = Math.sin(g.tilt);
  const depth = (Math.sin(a) + 1) / 2;
  return { x: g.cx + ex * c - ey * s, y: g.cy + ex * s + ey * c, depth };
}
function ensureRingLayers() {
  const svgNS = 'http://www.w3.org/2000/svg';
  let svg = document.querySelector('#ringSvg');
  if (!svg) return null;
  const ensure = (id, tag) => {
    let el = document.querySelector('#' + id);
    if (!el) { el = document.createElementNS(svgNS, tag); el.setAttribute('id', id); svg.appendChild(el); }
    return el;
  };
  ensure('ringGuides', 'g'); ensure('links', 'g'); ensure('ringFx', 'g'); ensure('ringParticles', 'g');
  return svg;
}
function layoutRingGuides(g) {
  const guides = document.querySelector('#ringGuides');
  if (!guides) return;
  // Persistent guide ellipses: update attributes in place, never rebuild.
  const svgNS = 'http://www.w3.org/2000/svg';
  while (guides.childElementCount < 2) {
    const e = document.createElementNS(svgNS, 'ellipse');
    e.setAttribute('class', 'ring-guide');
    guides.appendChild(e);
  }
  const [o1, o2] = [...guides.children];
  for (const [el, k] of [[o1, 1], [o2, 0.68]]) {
    el.setAttribute('cx', g.cx); el.setAttribute('cy', g.cy);
    el.setAttribute('rx', g.rx * k); el.setAttribute('ry', g.ry * k);
    el.setAttribute('transform', `rotate(${(g.tilt * 180 / Math.PI).toFixed(1)} ${g.cx} ${g.cy})`);
  }
}
function startRingParticles() {
  if (ringAnim.particleRaf || ringReducedMotion()) return;
  const svgNS = 'http://www.w3.org/2000/svg';
  const layer = document.querySelector('#ringParticles');
  if (!layer) return;
  const N = 14;
  while (layer.childElementCount < N) {
    const c = document.createElementNS(svgNS, 'circle');
    c.setAttribute('r', '2'); c.setAttribute('class', 'ring-particle');
    layer.appendChild(c);
  }
  ringAnim.particleLast = performance.now();
  const step = now => {
    ringAnim.particleRaf = requestAnimationFrame(step);
    if (document.hidden || ringReducedMotion()) { ringAnim.particleLast = now; return; }
    const dt = Math.min(0.1, (now - ringAnim.particleLast) / 1000);
    ringAnim.particleLast = now;
    ringAnim.particleT = (ringAnim.particleT + dt * 0.07) % 1;
    const g = ringAnim.geom;
    if (!g) return;
    const dots = layer.children;
    for (let i = 0; i < dots.length; i++) {
      const t = (ringAnim.particleT + i / dots.length) % 1;
      const a = t * Math.PI * 2 - Math.PI / 2;
      const ex = Math.cos(a) * g.rx, ey = Math.sin(a) * g.ry;
      const c = Math.cos(g.tilt), s = Math.sin(g.tilt);
      dots[i].setAttribute('cx', (g.cx + ex * c - ey * s).toFixed(1));
      dots[i].setAttribute('cy', (g.cy + ex * s + ey * c).toFixed(1));
      dots[i].setAttribute('opacity', (0.25 + 0.55 * ((Math.sin(a) + 1) / 2)).toFixed(2));
    }
  };
  ringAnim.particleRaf = requestAnimationFrame(step);
}
function updateRingStatus() {
  const el = document.querySelector('#ringStatus');
  if (!el) return;
  const inflight = ringAnim.inflight.size;
  const transport = (el.dataset && el.dataset.transport) || liveTransport || 'polling';
  const extra = ringAnim.coalesced > 0 ? ` · +${ringAnim.coalesced} coalesced` : '';
  el.textContent = `${transport === 'sse' ? 'live' : 'polling'} · ${inflight} in flight${extra}`;
}
function updateRingCoalesced() {
  const el = document.querySelector('#ringCoalesced');
  if (!el) return;
  if (ringAnim.coalesced > 0) { el.hidden = false; el.textContent = `+${ringAnim.coalesced} coalesced`; }
  else { el.hidden = true; el.textContent = ''; }
  updateRingStatus();
}
function brightenRingLinks() {
  for (const [id, l] of ringLinks) {
    const hot = ringAnim.inflight.has(id) || [...ringAnim.inflight.values()].some(v => (v.deployments || []).includes(id));
    l.setAttribute('opacity', hot ? '1' : '0.22');
    l.setAttribute('stroke-width', hot ? '2' : '1.1');
    if (hot) l.classList.add('link-hot'); else l.classList.remove('link-hot');
  }
}
function ringAnimKey(e) {
  const seq = Number(e.seq || 0);
  if (seq) return 'seq:' + seq;
  return `req:${e.request_id || ''}|${e.kind}|${e.deployment || ''}|${e.message || ''}|${e.latency_ms || 0}`;
}
function drainRingEvent(e) {
  if (!e || !e.kind) return;
  const kinds = new Set(['route_attempt', 'route_skip', 'route_fail', 'route_timeout', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail', 'failover', 'route_ok', 'candidate_exhausted']);
  if (!kinds.has(e.kind)) return;
  const key = ringAnimKey(e);
  if (ringAnim.seenReq.has(key)) return;
  ringAnim.seenReq.add(key);
  if (ringAnim.seenReq.size > 2000) { const first = ringAnim.seenReq.values().next().value; ringAnim.seenReq.delete(first); }
  const seq = Number(e.seq || 0);
  if (seq) {
    if (seq <= ringAnim.lastAnimatedSeq) return;
    ringAnim.lastAnimatedSeq = Math.max(ringAnim.lastAnimatedSeq, seq);
  }
  if (ringReducedMotion()) { trackRingInflight(e); return; }
  if (ringAnim.active.size >= ringAnim.maxAnims) {
    ringAnim.coalesced++;
    updateRingCoalesced();
    trackRingInflight(e);
    return;
  }
  applyRingEvent(e);
}
function trackRingInflight(e) {
  const rid = e.request_id || '';
  if (!rid) { brightenRingLinks(); return; }
  let rec = ringAnim.inflight.get(rid);
  if (!rec) { rec = { deployments: [], updated: Date.now() }; ringAnim.inflight.set(rid, rec); }
  if (e.deployment && !rec.deployments.includes(e.deployment)) rec.deployments.push(e.deployment);
  rec.updated = Date.now();
  const terminal = e.kind === 'route_ok' || e.kind === 'candidate_exhausted';
  if (terminal) {
    const dep = e.deployment, id = rid;
    setTimeout(() => {
      const r = ringAnim.inflight.get(id);
      if (!r) return;
      if (dep) r.deployments = r.deployments.filter(d => d !== dep);
      if (!r.deployments.length || e.kind === 'candidate_exhausted') ringAnim.inflight.delete(id);
      brightenRingLinks(); updateRingStatus();
    }, 2500);
  }
  if (ringAnim.inflight.size > 64) {
    const oldest = [...ringAnim.inflight.entries()].sort((a, b) => a[1].updated - b[1].updated)[0];
    if (oldest) ringAnim.inflight.delete(oldest[0]);
  }
  brightenRingLinks(); updateRingStatus();
}
function ringNodeLabel(n, icon, text) {
  let tag = n.querySelector('.ring-anim');
  if (!tag) { tag = document.createElement('span'); tag.className = 'ring-anim'; n.appendChild(tag); }
  tag.textContent = `${icon} ${text}`;
}
function applyRingEvent(e) {
  trackRingInflight(e);
  const finish = (id, ms) => {
    const t = setTimeout(() => {
      ringAnim.active.delete(id);
      const n = id === '__core__' ? document.querySelector('#ringCore') : ringNodes.get(id);
      if (n) n.classList.remove('anim-attempt', 'anim-skip', 'anim-fail', 'anim-failover', 'anim-ok');
      if (ringAnim.coalesced > 0) { ringAnim.coalesced--; updateRingCoalesced(); }
      brightenRingLinks(); updateRingStatus();
    }, ms);
    ringAnim.active.set(id, t);
  };
  if (e.kind === 'candidate_exhausted') {
    const core = document.querySelector('#ringCore');
    if (core) {
      const id = '__core__';
      if (ringAnim.active.has(id)) { clearTimeout(ringAnim.active.get(id)); ringAnim.active.delete(id); }
      core.classList.remove('anim-exhausted'); void core.offsetWidth;
      core.classList.add('anim-exhausted');
      ringNodeLabel(core, '!', 'exhausted');
      finish(id, 1600);
    }
    return;
  }
  const dep = e.deployment;
  if (!dep) return;
  const n = ringNodes.get(dep);
  const l = ringLinks.get(dep);
  if (l) { l.setAttribute('opacity', '1'); l.setAttribute('stroke-width', '2'); l.classList.add('link-hot'); }
  if (!n) return;
  const animId = dep + '|' + e.kind + '|' + (e.request_id || '') + '|' + (e.seq || Math.random());
  const reduced = ringReducedMotion();
  const clearPrev = () => n.classList.remove('anim-attempt', 'anim-skip', 'anim-fail', 'anim-failover', 'anim-ok');
  if (e.kind === 'route_attempt') {
    clearPrev(); n.classList.add('anim-attempt'); ringNodeLabel(n, '●', 'attempt');
    if (!reduced) { void n.offsetWidth; }
    finish(animId, 1400);
  } else if (e.kind === 'route_skip') {
    clearPrev(); n.classList.add('anim-skip'); ringNodeLabel(n, '≫', 'skip');
    finish(animId, 1200);
  } else if (e.kind === 'route_fail' || e.kind === 'stream_fail' || e.kind === 'route_timeout' || e.kind === 'stream_fail_precommit' || e.kind === 'response_decode_fail' || e.error_type === 'provider_timeout') {
    clearPrev(); n.classList.add('anim-fail'); ringNodeLabel(n, '✕', e.error_type || 'fail');
    finish(animId, 1800);
  } else if (e.kind === 'failover') {
    clearPrev(); n.classList.add('anim-failover'); ringNodeLabel(n, '↻', 'failover → next');
    finish(animId, 1600);
  } else if (e.kind === 'route_ok') {
    clearPrev(); n.classList.add('anim-ok');
    const lat = Number(e.latency_ms || 0);
    ringNodeLabel(n, '✓', lat ? `${lat} ms` : 'ok');
    finish(animId, 2000);
  }
  brightenRingLinks(); updateRingStatus();
}
function renderRing(ds, h) {
  const ring = $('#ring');
  if (!ring) return;
  ensureRingLayers();
  const svg = document.querySelector('#ringSvg');
  const linkLayer = document.querySelector('#links');
  const shown = ds.slice(0, 100), W = ring.clientWidth || 600, H = ring.clientHeight || 420;
  const g = ringGeometry(W, H, shown.length);
  ringAnim.geom = g;
  ring.classList.toggle('dense', shown.length > 24);
  const cx = g.cx, cy = g.cy;
  // Reconcile nodes/links by deployment id: add/update/remove deltas only.
  const active = new Set(shown.map(d => d.id));
  for (const [id, node] of [...ringNodes]) if (!active.has(id)) { node.remove(); ringNodes.delete(id); const l = ringLinks.get(id); if (l) l.remove(); ringLinks.delete(id); }
  for (const [id, l] of [...ringLinks]) if (!active.has(id)) { l.remove(); ringLinks.delete(id); }
  shown.forEach((d, i) => {
    const p = ringNodePos(i, shown.length, g);
    const st = (h[d.id] || {}).status || 'unknown';
    let n = ringNodes.get(d.id);
    if (!n) {
      n = document.createElement('div');
      n.dataset.depid = d.id;
      n.setAttribute('role', 'listitem');
      ringNodes.set(d.id, n);
      ring.appendChild(n);
    }
    // Preserve live animation classes across refresh (do not wipe anim-*).
    const anims = [...n.classList].filter(c => c.startsWith('anim-'));
    n.className = 'node ' + st + (anims.length ? ' ' + anims.join(' ') : '');
    n.style.left = p.x + 'px'; n.style.top = p.y + 'px';
    n.style.zIndex = String(10 + Math.round(p.depth * 20));
    n.style.opacity = (0.82 + p.depth * 0.18).toFixed(2);
    const scale = (0.88 + p.depth * 0.12).toFixed(3);
    n.style.transform = `translate(-50%,-50%) scale(${scale})`;
    n.title = `${d.model} • ${d.provider_name || d.provider_id} • ${st} • ${fmtMs((h[d.id] || {}).ewma_latency_ms)}`;
    const prevAnim = n.querySelector('.ring-anim');
    const prevText = prevAnim ? prevAnim.textContent : '';
    n.innerHTML = `<strong>${esc(d.model)}</strong><small>${esc(d.provider_name || d.provider_id)}</small><span class="badge">${esc(st)}</span>`;
    if (prevText) { const tag = document.createElement('span'); tag.className = 'ring-anim'; tag.textContent = prevText; n.appendChild(tag); }
    let l = ringLinks.get(d.id);
    if (!l) { l = document.createElementNS('http://www.w3.org/2000/svg', 'line'); l.dataset.depid = d.id; ringLinks.set(d.id, l); (linkLayer || svg).appendChild(l); }
    for (const [k, v] of Object.entries({
      x1: cx, y1: cy, x2: p.x, y2: p.y,
      stroke: st === 'healthy' ? 'rgba(63,224,140,.4)' : st === 'cooldown' ? 'rgba(255,110,125,.4)' : st === 'retired' ? 'rgba(142,151,168,.32)' : st === 'degraded' ? 'rgba(255,200,97,.4)' : st === 'half_open' ? 'rgba(201,184,255,.4)' : 'rgba(70,98,138,.3)',
      'stroke-width': 1.1, 'stroke-dasharray': '3 6', opacity: '0.22'
    })) l.setAttribute(k, v);
  });
  layoutRingGuides(g);
  startRingParticles();
  brightenRingLinks();
  updateRingStatus();
  if (ds.length > 100) {
    if (!ringMore) { ringMore = document.createElement('div'); ringMore.className = 'node degraded'; ringMore.setAttribute('role', 'listitem'); ring.appendChild(ringMore); }
    ringMore.style.left = cx + 'px'; ringMore.style.top = (H - 30) + 'px';
    ringMore.innerHTML = `<strong>+${ds.length - 100} more</strong><small>See Models table</small><span class="badge">+${ds.length - 100} more</span>`;
  } else if (ringMore) { ringMore.remove(); ringMore = null; }
}

function renderDonut(counts, total) {
  const segs = [
    ['donutGood', 'healthy', counts.healthy || 0, 'var(--good)'],
    ['donutUnknown', 'unknown', counts.unknown || 0, '#5c718a'],
    ['donutDegraded', 'degraded', counts.degraded || 0, 'var(--warn)'],
    ['donutHalfOpen', 'half_open', counts.half_open || 0, '#c9b8ff'],
    ['donutCooldown', 'cooldown', counts.cooldown || 0, 'var(--bad)'],
    ['donutRetired', 'retired', counts.retired || 0, '#8e97a8']
  ];
  const r = 48, circ = 2 * Math.PI * r;
  let offset = 0;
  const legend = [];
  for (const [id, label, count, color] of segs) {
    const el = needEl('#' + id, 'render:donut');
    if (!el) { if (count > 0 || label === 'healthy') legend.push(`<li><i style="background:${color}"></i>${label}<b>${fmtInt(count)}</b></li>`); continue; }
    const frac = total ? count / total : 0;
    const len = frac * circ;
    el.setAttribute('stroke-dasharray', `${len} ${circ - len}`);
    el.setAttribute('stroke-dashoffset', -offset);
    offset += len;
    if (count > 0 || label === 'healthy') legend.push(`<li><i style="background:${color}"></i>${label}<b>${fmtInt(count)}</b></li>`);
  }
  const dl = needEl('#donutLegend', 'render:donut');
  if (dl) dl.innerHTML = legend.join('');
  const readyPct = total ? Math.round((counts.healthy || 0) / total * 100) : 0;
  setText('#donutPct', readyPct + '%', 'render:donut');
}

function providerColor(id) {
  let hash = 0;
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0;
  const hues = [168, 190, 205, 250, 268, 150, 285, 220];
  const h = hues[hash % hues.length];
  return `linear-gradient(135deg,hsl(${h} 70% 52%),hsl(${(h + 40) % 360} 65% 45%))`;
}

function renderProviders(h) {
  const incidents = Object.fromEntries((snap.provider_health || []).map(x => [x.provider, x]));
  const stats = Object.fromEntries((snap.provider_stats || []).map(x => [x.id, x]));
  const grid = needEl('#providerGrid', 'render:providers');
  if (!grid) return;
  grid.innerHTML = providerSummaries.map(p => {
    const ds = (snap.deployments || []).filter(d => d.provider_id === p.id);
    const ok = ds.filter(d => (h[d.id] || {}).status === 'healthy').length;
    const pct = ds.length ? Math.round(ok / ds.length * 100) : 0;
    const inc = incidents[p.id] || { status: 'unknown' };
    const st = stats[p.id] || {};
    const effectiveReq = Number.isFinite(st.effective_remaining_requests) ? st.effective_remaining_requests : st.remaining_requests;
    const reservedReq = Number.isFinite(st.reserved_requests) && st.reserved_requests > 0 ? st.reserved_requests : 0;
    const quota = Number.isFinite(effectiveReq) && effectiveReq >= 0
      ? (Number.isFinite(st.request_limit) && st.request_limit > 0 ? `${effectiveReq}/${st.request_limit} req${reservedReq ? ` · ${reservedReq} reserved` : ''}` : `${effectiveReq} req left`)
      : 'quota unknown';
    return `<article class="provider-card">
      <div class="provider-card-top">
        <div class="provider-mark" style="background:${providerColor(p.id)}">${esc((p.name || p.id).slice(0, 1).toUpperCase())}</div>
        <div class="provider-copy"><h3 title="${esc(p.name || p.id)}">${esc(p.name || p.id)}</h3><p>${esc(p.type)} · ${p.model_count} model${p.model_count === 1 ? '' : 's'}</p></div>
        <span class="pill ${p.enabled ? 'on' : ''}">${p.enabled ? 'Enabled' : 'Disabled'}</span>
      </div>
      <div class="provider-url" title="${esc(p.base_url)}">${esc(p.base_url)}</div>
      <div class="provider-healthbar"><i style="width:${pct}%"></i></div>
      <div class="provider-meta"><span>${p.has_secret ? `${p.credential_count || 1} credential(s)` : 'No credential'}</span><span>${ok}/${ds.length} healthy</span><span>incident: ${esc(inc.status || 'unknown')}</span><span>${esc(quota)}</span></div>
      <button class="edit-provider btn secondary" data-id="${esc(p.id)}">Edit provider</button>
    </article>`;
  }).join('') || '<div class="empty-state"><strong>No providers yet.</strong><span>Add an OpenAI-compatible or Anthropic-compatible upstream to start routing.</span></div>';
  $$('.edit-provider').forEach(b => b.onclick = () => openEdit(b.dataset.id));
}

function renderModels(ds, h) {
  const search = document.querySelector('#modelSearch');
  const q = ((search && search.value) || '').toLowerCase();
  if (!search) uiWarn('render:models', 'missing element #modelSearch');
  const rows = ds.filter(d => !q || d.id.toLowerCase().includes(q) || (d.provider_id || '').toLowerCase().includes(q) || (d.model || '').toLowerCase().includes(q));
  const maxLat = Math.max(1, ...ds.map(d => Number((h[d.id] || {}).ewma_latency_ms || 0)));
  const body = needEl('#modelRows', 'render:models');
  if (!body) return;
  body.innerHTML = rows.map(d => {
    const x = h[d.id] || { status: 'unknown' };
    const lat = Number(x.ewma_latency_ms || 0);
    return `<tr>
      <td class="dep-id">${esc(d.id)}</td>
      <td>${esc(d.provider_name || d.provider_id)}</td>
      <td>${esc(d.model)}</td>
      <td><span class="status ${esc(x.status)}">${esc(x.status)}</span></td>
      <td><span class="latbar">${lat ? `<span class="track"><i style="width:${Math.round(lat / maxLat * 100)}%"></i></span>` : ''}${fmtMs(lat)}</span></td>
      <td>${fmtMs(Number(x.ewma_ttft_ms || 0))}</td>
      <td>${x.consecutive_failures || 0}</td>
    </tr>`;
  }).join('') || '<tr><td colspan="7" style="color:var(--muted)">No deployments match.</td></tr>';
}
bind('#modelSearch', 'oninput', () => renderModels(snap.deployments || [], healthMap()), 'render:models');

function renderHealthTab(h) {
  const incidents = Object.fromEntries((snap.provider_health || []).map(x => [x.provider, x]));
  const stats = Object.fromEntries((snap.provider_stats || []).map(x => [x.id, x]));
  const rows = (snap.provider_pressure || []);
  const phBody = needEl('#providerHealthRows', 'render:health');
  if (phBody) phBody.innerHTML = rows.map(p => {
    const id = p.provider_id || p.provider;
    const inc = incidents[id] || { status: 'unknown' };
    const st = stats[id] || {};
    const capPct = p.capacity ? Math.min(100, Math.round((p.active / p.capacity) * 100)) : 0;
    const effectiveReq = Number.isFinite(st.effective_remaining_requests) ? st.effective_remaining_requests : st.remaining_requests;
    const effectiveTok = Number.isFinite(st.effective_remaining_tokens) ? st.effective_remaining_tokens : st.remaining_tokens;
    const reqReserved = Number.isFinite(st.reserved_requests) && st.reserved_requests > 0 ? st.reserved_requests : 0;
    const tokReserved = Number.isFinite(st.reserved_tokens) && st.reserved_tokens > 0 ? st.reserved_tokens : 0;
    const reqQuota = Number.isFinite(effectiveReq) && effectiveReq >= 0
      ? (Number.isFinite(st.request_limit) && st.request_limit > 0 ? `${effectiveReq}/${st.request_limit}${reqReserved ? ` (-${reqReserved})` : ''}` : effectiveReq) : '—';
    const tokQuota = Number.isFinite(effectiveTok) && effectiveTok >= 0
      ? (Number.isFinite(st.token_limit) && st.token_limit > 0 ? `${effectiveTok}/${st.token_limit}${tokReserved ? ` (-${fmtInt(tokReserved)})` : ''}` : fmtInt(effectiveTok)) : '—';
    return `<tr>
      <td>${esc(p.provider_name || id)}</td>
      <td><span class="status ${esc(inc.status || 'unknown')}">${esc(inc.status || 'unknown')}</span></td>
      <td>${p.active ?? 0}</td><td>${p.waiting ?? 0}</td>
      <td><span class="latbar">${p.capacity ? `<span class="track"><i style="width:${capPct}%;background:${capPct > 85 ? 'var(--bad)' : capPct > 60 ? 'var(--warn)' : 'var(--good)'}"></i></span>` : ''}${p.capacity ?? '∞'}</span></td>
      <td>${esc(p.credentials ?? '—')}</td>
      <td>${p.cooling ? '<span class="status cooldown">cooling</span>' : '<span class="status healthy">ok</span>'}</td>
      <td>${reqQuota}</td><td>${tokQuota}</td>
    </tr>`;
  }).join('') || '<tr><td colspan="9" style="color:var(--muted)">No provider pressure data.</td></tr>';

  const scopes = snap.scope_health || [];
  const scopeBody = needEl('#scopeHealthList', 'render:health');
  if (scopeBody) scopeBody.innerHTML = scopes.map(s => {
    const st = s.status || 'unknown';
    return `<div class="scope-row">
      <div><strong>${esc(s.deployment)}</strong><br><small>scope: ${esc((s.scopes || []).join(', '))}</small></div>
      <span class="status ${esc(st)}">${esc(st)}</span>
      <div>${s.consecutive_failures || 0} fails</div>
      <div class="cap-bar"><i style="width:${st === 'healthy' ? 100 : Math.max(12, 100 - (s.consecutive_failures || 0) * 25)}%"></i></div>
    </div>`;
  }).join('') || '<p class="hint">No capability-scoped failures observed. Scopes activate when streaming, tools, vision or reasoning requests fail on a deployment.</p>';
}

/* ---------- routing observatory ---------- */
function renderRoutingObservatory(h, ds) {
  const all = snap.events || [];
  const routeKinds = new Set(['route_attempt', 'route_fail', 'failover', 'route_ok', 'route_timeout', 'model_unavailable', 'model_retired', 'candidate_exhausted', 'response_decode_fail', 'stream_fail_precommit', 'stream_fail']);
  const latest = [...all].reverse().find(e => e.request_id && routeKinds.has(e.kind));
  const byID = Object.fromEntries(ds.map(d => [d.id, d]));
  const modelLabel = id => {
    const d = byID[id];
    if (!d) return id || 'gateway';
    return d.model || d.id;
  };

  let requestEvents = [];
  if (latest) requestEvents = all.filter(e => e.request_id === latest.request_id && routeKinds.has(e.kind));

  const publicEvent = [...requestEvents].reverse().find(e => e.public_model);
  setText('#obsRequest', latest?.request_id ? latest.request_id.slice(0, 22) : '—', 'render:observatory');
  setText('#obsPublic', publicEvent?.public_model || '—', 'render:observatory');

  const failures = requestEvents.filter(e => e.kind === 'route_fail' || e.kind === 'route_timeout' || e.kind === 'response_decode_fail' || e.kind === 'stream_fail_precommit');
  setText('#obsAbsorbed', fmtInt(failures.length), 'render:observatory');

  const success = requestEvents.some(e => e.kind === 'route_ok');
  const exhausted = requestEvents.some(e => e.kind === 'candidate_exhausted');
  const result = success ? 'SUCCESS' : exhausted ? 'EXHAUSTED' : requestEvents.length ? 'ACTIVE' : 'IDLE';
  setText('#obsResult', result, 'render:observatory');
  const obsRes = document.querySelector('#obsResult');
  if (obsRes) { try { obsRes.className = result.toLowerCase(); } catch (e) { uiWarn('render:observatory', e); } }
  else uiWarn('render:observatory', 'missing element #obsResult');

  const stateByDeployment = {};
  const statePriority = { retired: 50, unavailable: 40, cooldown: 30, success: 25, failed: 20, active: 10 };
  const setDeploymentState = (deployment, next) => {
    const current = stateByDeployment[deployment];
    if (!current || (statePriority[next] || 0) >= (statePriority[current] || 0)) stateByDeployment[deployment] = next;
  };
  for (const e of requestEvents) {
    if (!e.deployment) continue;
    if (e.kind === 'model_retired') setDeploymentState(e.deployment, 'retired');
    else if (e.kind === 'model_unavailable') setDeploymentState(e.deployment, 'unavailable');
    else if (e.kind === 'route_ok') setDeploymentState(e.deployment, 'success');
    else if (e.kind === 'route_fail' || e.kind === 'route_timeout' || e.kind === 'response_decode_fail' || e.kind === 'stream_fail_precommit') {
      setDeploymentState(e.deployment, e.error_type === 'provider_rate_limited' ? 'cooldown' : 'failed');
    }
  }
  const attempts = requestEvents.filter(e => e.kind === 'route_attempt');
  const flow = needEl('#routeFlow', 'render:observatory');
  if (flow) flow.innerHTML = attempts.length ? attempts.slice(-10).map((e, i) => {
    const st = stateByDeployment[e.deployment] || (i === attempts.length - 1 && !success ? 'active' : 'failed');
    const d = byID[e.deployment] || {};
    return `<div class="route-hop ${esc(st)}" title="${esc(e.deployment || '')}">
      <strong>${esc(modelLabel(e.deployment))}</strong>
      <small>${esc(d.provider_name || d.provider_id || e.deployment || '')}</small>
      <em>${esc(st.replace(/_/g, ' '))}</em>
    </div>`;
  }).join('') : '<div class="obs-empty">No routed request yet.</div>';

  const journey = requestEvents.filter(e => routeKinds.has(e.kind)).slice(-12);
  const journeyEl = needEl('#routeJourney', 'render:observatory');
  if (journeyEl) journeyEl.innerHTML = journey.length ? journey.map(e => {
    const tm = e.time ? new Date(e.time).toLocaleTimeString('en-US', { hour12: false }) : '—';
    const dep = e.deployment ? modelLabel(e.deployment) : (e.public_model || 'gateway');
    return `<div class="journey-row">
      <time>${esc(tm)}</time><span class="jkind">${esc(e.kind)}</span>
      <span class="jdep" title="${esc(e.deployment || '')}">${esc(dep)}</span>
      <span class="jerr">${esc(e.error_type || (e.latency_ms ? e.latency_ms + ' ms' : ''))}</span>
    </div>`;
  }).join('') : '<div class="obs-empty">Routing events will appear here.</div>';

  const supervisorStates = new Set(['degraded', 'half_open', 'cooldown', 'retired']);
  const supervisor = Object.values(h).filter(x => supervisorStates.has(x.status)).sort((a, b) => {
    const rank = { retired: 0, cooldown: 1, half_open: 2, degraded: 3 };
    return (rank[a.status] ?? 9) - (rank[b.status] ?? 9);
  }).slice(0, 12);
  const supEl = needEl('#supervisorList', 'render:observatory');
  if (supEl) supEl.innerHTML = supervisor.length ? supervisor.map(x => {
    const d = byID[x.deployment] || {};
    let detail = x.last_error_class || (x.status === 'retired' ? 'model_retired' : x.status);
    if (x.status === 'cooldown' && x.cooldown_until) {
      const seconds = Math.max(0, Math.ceil((new Date(x.cooldown_until).getTime() - Date.now()) / 1000));
      detail = seconds ? `retry in ${seconds}s` : 'ready for half-open';
    } else if (x.recovery_failures) {
      detail = `recovery ${x.recovery_failures}/${snap.config?.probe?.recovery_attempts || 5}`;
    }
    return `<div class="supervisor-item ${esc(x.status)}">
      <strong title="${esc(x.deployment)}">${esc(d.model || x.deployment)}</strong><span class="sstate">${esc(x.status)}</span>
      <small>${esc(d.provider_name || d.provider_id || '')}</small><small>${esc(detail)}</small>
    </div>`;
  }).join('') : '<div class="obs-empty">No recovery, cooldown or retirement work.</div>';

  const failureCounts = {};
  for (const e of all.slice(-100)) {
    let key = e.error_type || '';
    if (!key && (e.kind === 'model_retired' || e.kind === 'model_unavailable' || e.kind === 'candidate_exhausted')) key = e.kind;
    if (!key) continue;
    failureCounts[key] = (failureCounts[key] || 0) + 1;
  }
  const radar = Object.entries(failureCounts).sort((a, b) => b[1] - a[1]).slice(0, 9);
  const radarEl = needEl('#failureRadar', 'render:observatory');
  if (radarEl) radarEl.innerHTML = radar.length ? radar.map(([k, n]) => `<span class="radar-chip">${esc(k)} <b>${fmtInt(n)}</b></span>`).join('') : '<div class="obs-empty">No recent failures.</div>';
}

/* ---------- console ---------- */
const consoleKinds = {
  routes: new Set(['route_ok', 'route_attempt', 'route_fail', 'route_skip', 'route_timeout', 'failover', 'model_unavailable', 'model_retired', 'candidate_exhausted', 'client_disconnect', 'response_decode_fail', 'stream_fail', 'gateway_overloaded']),
  errors: new Set(['route_fail', 'route_timeout', 'model_unavailable', 'model_retired', 'candidate_exhausted', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail', 'gateway_overloaded', 'internal_panic', 'client_disconnect', 'probe_fail', 'probe_quarantine', 'recovery_fail', 'recovery_queue_full']),
  probes: new Set(['probe_ready', 'probe_fail', 'probe_quarantine', 'recovery_ready', 'recovery_fail', 'recovery_wait', 'recovery_deferred', 'recovery_cooldown', 'recovery_queue_full', 'model_unavailable', 'model_retired', 'stream_fail_precommit'])
};
function renderConsole() {
  const box = needEl('#consoleLog', 'render:console');
  if (!box) return;
  const auto = document.querySelector('#consoleAuto');
  const stick = auto ? (auto.checked && (box.scrollHeight - box.scrollTop - box.clientHeight < 60)) : false;
  if (!auto) uiWarn('render:console', 'missing element #consoleAuto');
  const es = (snap.events || []).slice().reverse().filter(e => {
    if (consoleFilter === 'routes') return consoleKinds.routes.has(e.kind);
    if (consoleFilter === 'errors') return consoleKinds.errors.has(e.kind);
    if (consoleFilter === 'probes') return consoleKinds.probes.has(e.kind);
    return true;
  });
  box.innerHTML = es.map(e => {
    const kind = esc(e.kind);
    const dep = e.deployment ? `<span class="dep">${esc(e.deployment)}</span> ` : '';
    const err = e.error_type ? ` <span style="color:#54687f">[${esc(e.error_type)}]</span>` : '';
    return `<div class="cline k-${kind}"><time>${new Date(e.time).toLocaleTimeString('en-US', { hour12: false })}</time><span class="ckind">▸ ${kind}</span><span class="cmsg">${dep}${esc(e.message || '')}${err}</span><span class="clat">${e.latency_ms ? e.latency_ms + ' ms' : ''}</span></div>`;
  }).join('') || '<p style="color:#54687f;padding:8px">No events yet — route a request or run a probe.</p>';
  setText('#consoleCount', fmtInt(es.length) + ' events', 'render:console');
  if (stick) { try { box.scrollTop = box.scrollHeight; } catch (e) { uiWarn('render:console', e); } }
}
$$('#consoleFilter button').forEach(b => { b.onclick = () => uiSection('console', () => {
  $$('#consoleFilter button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  consoleFilter = b.dataset.f;
  renderConsole();
}); });

/* ---------- data refresh loop ---------- */
async function refresh() {
  try {
    const [s, p] = await Promise.all([
      api('/admin/api/snapshot?limit=500&events=100'),
      api('/admin/api/providers')
    ]);
    // Animate only genuinely new polled events (polling fallback path);
    // SSE path already drains live. De-duplicated by seq/request_id.
    const prevMax = liveSeq;
    const incoming = s.events || [];
    providerSummaries = p.providers || [];
    snap = s;
    liveSeq = Math.max(liveSeq, ...incoming.map(e => Number(e.seq || 0)));
    for (const e of incoming) {
      const seq = Number(e.seq || 0);
      if (seq && seq > prevMax) drainRingEvent(e);
      else if (!seq && e.request_id) drainRingEvent(e);
    }
    // latency timeline from recent successful routes
    const lats = (snap.events || []).filter(e => e.kind === 'route_ok' && e.latency_ms).map(e => e.latency_ms).slice(0, 60).reverse();
    if (lats.length) {
      latencyHistory = lats;
      uiSection('render:chart', () => drawChart(lats));
    }
    uiSection('render:conn', () => {
      const st = needEl('#apiState', 'render:conn');
      if (!st) return;
      st.className = 'conn ok';
      const txt = st.querySelector ? st.querySelector('.conn-text') : null;
      if (txt) txt.textContent = 'connected';
      else uiWarn('render:conn', 'missing element .conn-text');
    });
    render();
  } catch (e) {
    uiSection('render:conn', () => {
      const st = needEl('#apiState', 'render:conn');
      if (!st) return;
      st.className = 'conn err';
      const txt = st.querySelector ? st.querySelector('.conn-text') : null;
      if (txt) txt.textContent = 'disconnected';
      else uiWarn('render:conn', 'missing element .conn-text');
    });
  }
}
function drawChart(vals) {
  const W = 320, H = 96, max = Math.max(...vals, 1);
  const step = vals.length > 1 ? W / (vals.length - 1) : W;
  const pts = vals.map((v, i) => [i * step, H - 6 - (v / max) * (H - 16)]);
  const line = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  const cl = needEl('#chartLine', 'render:chart');
  if (cl && cl.setAttribute) cl.setAttribute('d', line);
  const ca = needEl('#chartArea', 'render:chart');
  if (ca && ca.setAttribute) ca.setAttribute('d', line + ` L ${W} ${H} L 0 ${H} Z`);
  const last = vals[vals.length - 1];
  setText('#chartLast', 'last: ' + Math.round(last) + ' ms', 'render:chart');
  setText('#chartMax', 'peak: ' + Math.round(max) + ' ms', 'render:chart');
  // KPI sparkline
  const sw = 120, sh = 28;
  const smax = Math.max(...vals, 1);
  const sstep = vals.length > 1 ? sw / (vals.length - 1) : sw;
  const spts = vals.map((v, i) => [i * sstep, sh - 2 - (v / smax) * (sh - 6)]);
  const sline = spts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  const sl = document.querySelector('#latencySpark .spark-line');
  if (sl && sl.setAttribute) sl.setAttribute('d', sline);
  else uiWarn('render:chart', 'missing element #latencySpark .spark-line');
  const sa = document.querySelector('#latencySpark .spark-area');
  if (sa && sa.setAttribute) sa.setAttribute('d', sline + ` L ${sw} ${sh} L 0 ${sh} Z`);
  else uiWarn('render:chart', 'missing element #latencySpark .spark-area');
}
async function tick() {
  try {
    if (!paused) {
      const had = (snap.events || []).length;
      await uiSectionAsync('tick:refresh', refresh);
      uiSection('tick:dot', () => {
        const now = (snap.events || []).length;
        const consoleTab = document.querySelector('#console');
        if (!consoleTab || !consoleTab.classList) { uiWarn('tick:dot', 'missing element #console'); return; }
        const dot = document.querySelector('#consoleDot');
        if (now > had && !consoleTab.classList.contains('active') && (snap.events || []).some(e => consoleKinds.errors.has(e.kind))) {
          if (dot) dot.hidden = false;
          else uiWarn('tick:dot', 'missing element #consoleDot');
          consoleUnread++;
        }
        if (consoleTab.classList.contains('active')) { consoleUnread = 0; if (dot) dot.hidden = true; }
      });
    }
  } catch (e) { uiWarn('tick', e); }
  const n = snap.deployment_total ?? (snap.deployments || []).length;
  const delay = n > 5000 ? 15000 : n > 1000 ? 8000 : n > 250 ? 4000 : 1800;
  setTimeout(tick, delay);
}
async function uiSectionAsync(name, fn) {
  try { await fn(); } catch (e) { uiWarn(name, e); }
}
let ringResizeQueued = false;
window.addEventListener('resize', () => {
  // Reposition only: never tear down nodes/links, never restart particle
  // progress or clear in-flight animation classes.
  if (ringResizeQueued) return;
  ringResizeQueued = true;
  requestAnimationFrame(() => { ringResizeQueued = false; uiSection('render:ring', () => renderRing(snap.deployments || [], healthMap())); });
});
setInterval(() => uiSection('tick:clock', () => { setText('#footClock', new Date().toLocaleTimeString('en-US', { hour12: false }), 'tick:clock'); }), 1000);

/* ---------- CLI tools tab ---------- */
function cliSnippet(kind) {
  const base = location.origin;
  const ves = snap.virtual_endpoints || [];
  const firstVE = ves.length ? ves[0] : null;
  const veModel = firstVE ? (firstVE.public_model || firstVE.id) : 'nexa-code';
  const veList = ves.length ? ves.map(v => v.public_model || v.id).join(', ') : 'auto, claude-auto';
  if (kind === 'claude') return {
    title: 'Claude Code / Anthropic clients (virtual endpoint)',
    note: ves.length ? `Virtual endpoints: ${veList}. Using ${veModel} routes through your configured pools without client reconfiguration.` : 'The placeholder key exists only for clients that require a non-empty value. Virtual endpoints provide stable public names.',
    body:
`<span class="c"># Anthropic-compatible ingress via virtual endpoint</span>
export ANTHROPIC_BASE_URL=${base}
export ANTHROPIC_AUTH_TOKEN=local-placeholder
export ANTHROPIC_MODEL=${veModel}

<span class="c"># virtual endpoints available: ${veList}</span>
<span class="c"># physical backend can change without client update</span>`
  };
  if (kind === 'openai') return {
    title: 'OpenAI-compatible tools (virtual endpoint)',
    note: ves.length ? `Virtual endpoint ${veModel} → Route Profile → Pool → existing router. Pool ∩ Eligibility = routable set.` : 'Chat Completions requests flow through the same routing plane and the same bulletproof protocol translation.',
    body:
`<span class="c"># OpenAI Chat Completions ingress via virtual endpoint</span>
export OPENAI_BASE_URL=${base}/v1
export OPENAI_API_KEY=local-placeholder

<span class="c"># direct curl with virtual model</span>
curl ${base}/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${veModel}","messages":[{"role":"user","content":"hi"}]}'

<span class="c"># virtual endpoints: ${veList}</span>`
  };
  if (kind === 'env') return {
    title: 'Session environment block',
    note: ves.length ? `Use virtual model ${veModel} for stable client identity.` : 'Drop this into .zshrc / .bashrc for the current machine.',
    body:
`<span class="c"># NexaRoute client environment (virtual endpoint)</span>
export ANTHROPIC_BASE_URL=${base}
export ANTHROPIC_AUTH_TOKEN=local-placeholder
export ANTHROPIC_MODEL=${veModel}
export OPENAI_BASE_URL=${base}/v1
export OPENAI_API_KEY=local-placeholder`
  };
  return {
    title: 'Health & diagnostics',
    note: 'Useful endpoints for monitoring and CI checks.',
    body:
`<span class="c"># process liveness</span>
curl -s ${base}/healthz

<span class="c"># routing readiness (ready queue populated)</span>
curl -s ${base}/readyz

<span class="c"># Prometheus metrics</span>
curl -s ${base}/metrics

<span class="c"># exposed models and aliases</span>
curl -s ${base}/v1/models

<span class="c"># virtual endpoints</span>
curl -s ${base}/admin/api/virtual-endpoints -H "x-admin-key: $ADMIN_KEY"

<span class="c"># example: ${veModel} → pool (configured) → router eligibility → physical deployment</span>`
  };
}

function renderCLI() {
  const tabs = $$('#cliTabs button');
  const active = tabs.find(b => b.classList && b.classList.contains('active')) || tabs[0];
  if (!active) { uiWarn('render:cli', 'missing element #cliTabs button'); return; }
  const s = cliSnippet(active.dataset.cli);
  const body = needEl('#cliBody', 'render:cli');
  if (!body) return;
  body.innerHTML = `
    <div class="cli-card">
      <div class="cli-card-head"><span>${esc(s.title)}</span><button class="copy-btn" id="cliCopy">Copy</button></div>
      <pre>${s.body}</pre>
    </div>
    <p class="cli-note">${esc(s.note)}</p>`;
  bind('#cliCopy', 'onclick', e => {
    const pre = document.querySelector('#cliBody pre');
    copyText(pre ? pre.innerText : '', e.target);
  }, 'render:cli');
}
$$('#cliTabs button').forEach(b => { b.onclick = () => uiSection('cli', () => {
  $$('#cliTabs button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  renderCLI();
}); });

/* ---------- provider editor ---------- */
function emptyProvider() {
  return {
    id: '', name: '', type: 'openai_compatible', dialect: '', base_url: '', api_key: '', api_key_env: '', credentials: [],
    auth_mode: 'bearer', headers: {}, forward_headers: null, proxy_url: '',
    chat_path: '/v1/chat/completions', responses_path: '/v1/responses', messages_path: '/v1/messages', models_path: '/v1/models',
    count_tokens_path: '/v1/messages/count_tokens', max_concurrency: 32, stream_idle_timeout_seconds: 180,
    enabled: true, models: []
  };
}
function modal(open) {
  const m = needEl('#providerModal', 'modal');
  if (!m) return;
  try {
    m.classList.toggle('open', open);
    m.setAttribute('aria-hidden', open ? 'false' : 'true');
    if (document.body && document.body.classList) document.body.classList.toggle('modal-open', open);
  } catch (e) { uiWarn('modal', e); }
}
function toggleSecret(i, b) {
  const el = needEl(i, 'secret');
  const btn = needEl(b, 'secret');
  if (!el || !btn) return;
  try {
    el.type = el.type === 'password' ? 'text' : 'password';
    btn.textContent = el.type === 'password' ? 'Show' : 'Hide';
  } catch (e) { uiWarn('secret', e); }
}
const providerPresets = {
  custom: null,
  chat2api: { name: 'Chat2API', id: 'chat2api', type: 'openai_compatible', base: 'http://127.0.0.1:5000/v1', auth: 'bearer', local: true },
  anthropic: { name: 'Anthropic', id: 'anthropic', type: 'anthropic_compatible', base: 'https://api.anthropic.com', auth: 'x-api-key' },
  openai: { name: 'OpenAI', id: 'openai', type: 'openai_compatible', base: 'https://api.openai.com/v1', auth: 'bearer' },
  openrouter: { name: 'OpenRouter', id: 'openrouter', type: 'openai_compatible', base: 'https://openrouter.ai/api/v1', auth: 'bearer' },
  deepseek: { name: 'DeepSeek', id: 'deepseek', type: 'openai_compatible', base: 'https://api.deepseek.com', auth: 'bearer' },
  groq: { name: 'Groq', id: 'groq', type: 'openai_compatible', base: 'https://api.groq.com/openai/v1', auth: 'bearer' },
  together: { name: 'Together AI', id: 'together', type: 'openai_compatible', base: 'https://api.together.xyz/v1', auth: 'bearer' },
  mistral: { name: 'Mistral', id: 'mistral', type: 'openai_compatible', base: 'https://api.mistral.ai/v1', auth: 'bearer' },
  xai: { name: 'xAI', id: 'xai', type: 'openai_compatible', base: 'https://api.x.ai/v1', auth: 'bearer' },
  ollama: { name: 'Ollama', id: 'ollama', type: 'openai_compatible', base: 'http://127.0.0.1:11434/v1', auth: 'none', local: true },
  lmstudio: { name: 'LM Studio', id: 'lmstudio', type: 'openai_compatible', base: 'http://127.0.0.1:1234/v1', auth: 'none', local: true },
  vllm: { name: 'vLLM', id: 'vllm', type: 'openai_compatible', base: 'http://127.0.0.1:8000/v1', auth: 'none', local: true }
};
function fillPresetSelect() {
  const sel = needEl('#pPreset', 'boot:presets');
  if (!sel) return;
  const current = sel.value;
  const apiEntries = [];
  const localEntries = [];
  const seen = new Set();
  const add = (key, label, local) => {
    if (!key || key === 'custom' || seen.has(key)) return;
    seen.add(key);
    const option = `<option value="${esc(key)}">${esc(label || key)}</option>`;
    (local ? localEntries : apiEntries).push(option);
  };
  if (Array.isArray(serverPresets)) {
    for (const p of serverPresets) {
      const key = p && (p.id || p.key);
      add(key, p && (p.name || p.label || key), !!(p && p.local));
    }
  }
  for (const [key, p] of Object.entries(providerPresets)) {
    if (!p) continue;
    add(key, p.name || key, !!p.local);
  }
  const apiGroup = apiEntries.length ? `<optgroup label="API Providers">${apiEntries.join('')}</optgroup>` : '';
  const localGroup = localEntries.length ? `<optgroup label="Local Providers">${localEntries.join('')}</optgroup>` : '';
  sel.innerHTML = '<option value="custom">Custom Provider</option>' + apiGroup + localGroup;
  sel.value = current || 'custom';
}
function applyPreset(k) {
  const p = serverPresets && Array.isArray(serverPresets) ? serverPresets.find(x => x && (x.id || x.key) === k) : null;
  if (p) {
    if (editor.mode === 'add') {
      const key = p.id || p.key;
      if (!$('#pName').value.trim()) $('#pName').value = p.name || p.label || key;
      if (!$('#pId').value.trim()) $('#pId').value = key;
    }
    $('#pType').value = p.type || 'openai_compatible';
    $('#pBase').value = p.base_url || p.base || '';
    $('#pAuth').value = p.auth_mode || 'bearer';
    $('#pChatPath').value = p.chat_path || '/v1/chat/completions';
    $('#pResponsesPath').value = p.responses_path || '/v1/responses';
    $('#pMessagesPath').value = p.messages_path || '/v1/messages';
    $('#pModelsPath').value = p.models_path || '/v1/models';
    $('#pCountPath').value = p.count_tokens_path || '/v1/messages/count_tokens';
    return;
  }
  const c = providerPresets[k];
  if (!c) return;
  if (editor.mode === 'add') {
    if (!$('#pName').value.trim()) $('#pName').value = c.name;
    if (!$('#pId').value.trim()) $('#pId').value = c.id;
  }
  $('#pType').value = c.type;
  $('#pBase').value = c.base;
  $('#pAuth').value = c.auth;
  $('#pChatPath').value = '/v1/chat/completions';
  $('#pResponsesPath').value = '/v1/responses';
  $('#pMessagesPath').value = '/v1/messages';
  $('#pModelsPath').value = '/v1/models';
  $('#pCountPath').value = '/v1/messages/count_tokens';
}
bind('#addProviderBtn', 'onclick', () => {
  editor = { mode: 'add', originalId: '', provider: emptyProvider(), detected: [], selected: new Set(), modelMeta: new Map(), secretDirty: true, secretSource: 'none' };
  fillForm(); modal(true);
}, 'provider');
bind('#closeProviderModal', 'onclick', () => modal(false), 'provider');
bind('#cancelProviderBtn', 'onclick', () => modal(false), 'provider');
$$('[data-close-modal]').forEach(x => { x.onclick = () => uiSection('provider', () => modal(false)); });
document.addEventListener('keydown', e => { if (e.key === 'Escape') uiSection('provider', () => modal(false)); });
bind('#togglePKey', 'onclick', () => toggleSecret('#pKey', '#togglePKey'), 'provider');
bind('#pKey', 'oninput', () => { if (editor) editor.secretDirty = true; }, 'provider');
bind('#pKeyEnv', 'oninput', () => { if (editor) editor.secretDirty = true; }, 'provider');
bind('#pCredentials', 'oninput', () => { if (editor) editor.secretDirty = true; }, 'provider');
bind('#pHeaders', 'oninput', () => { if (editor) editor.headersDirty = true; }, 'provider');
bind('#pProxy', 'oninput', () => { if (editor) editor.proxyDirty = true; }, 'provider');
bind('#pPreset', 'onchange', () => { const s = document.querySelector('#pPreset'); if (s) applyPreset(s.value); }, 'provider');
bind('#pType', 'onchange', () => {
  const a = document.querySelector('#pAuth'), t = document.querySelector('#pType');
  if (!a || !t) { uiWarn('provider', 'missing element #pAuth/#pType'); return; }
  const typ = t.value;
  if (typ === 'anthropic_compatible' && (a.value === 'bearer' || a.value === 'x-goog-api-key')) a.value = 'x-api-key';
  if (typ === 'gemini' && (a.value === 'bearer' || a.value === 'x-api-key')) a.value = 'x-goog-api-key';
  if ((typ === 'openai_compatible' || typ === 'openai_responses') && (a.value === 'x-api-key' || a.value === 'x-goog-api-key')) a.value = 'bearer';
}, 'provider');
async function openEdit(id) {
  try {
    const d = await api('/admin/api/providers/' + encodeURIComponent(id)), p = d.provider;
    // Saved keys never leave the server. Blank, untouched fields preserve them.
    p.api_key = '';
    editor = {
      mode: 'edit', originalId: id, provider: p,
      detected: (p.models || []).map(m => m.model),
      selected: new Set((p.models || []).map(m => m.model)),
      modelMeta: new Map((p.models || []).map(m => [m.model, m])),
      secretDirty: false, secretSource: d.secret_source || 'none'
    };
    fillForm(); modal(true);
  } catch (e) { toast(e.message, true); }
}
function fillForm() {
  const p = editor.provider;
  $('#pPreset').value = 'custom';
  $('#providerFormTitle').textContent = editor.mode === 'edit' ? 'Edit provider' : 'Add provider';
  $('#deleteProviderBtn').classList.toggle('hidden', editor.mode !== 'edit');
  $('#pName').value = p.name || '';
  $('#pId').value = p.id || '';
  $('#pId').disabled = editor.mode === 'edit';
  $('#pType').value = p.type || 'openai_compatible';
  $('#pBase').value = p.base_url || '';
  $('#pAuth').value = p.auth_mode || 'bearer';
  $('#pEnabled').checked = p.enabled !== false;
  $('#pKey').value = p.api_key || '';
  $('#pKey').placeholder = editor.mode === 'edit' ? 'Saved key is write-only; leave untouched to keep' : '';
  $('#pKey').type = 'password';
  $('#togglePKey').textContent = 'Show';
  $('#pKeyEnv').value = p.api_key_env || '';
  $('#pHeaders').value = Object.keys(p.headers || {}).length ? JSON.stringify(p.headers, null, 2) : '';
  $('#pProxy').value = '';
  $('#pProxy').placeholder = editor.mode === 'edit' ? 'Write-only; leave untouched to keep saved proxy' : '';
  $('#pHeaders').placeholder = editor.mode === 'edit' ? 'Write-only; leave untouched to keep saved headers' : '{}';
  $('#pConcurrency').value = p.max_concurrency || 32;
  $('#pStreamIdle').value = p.stream_idle_timeout_seconds || 180;
  $('#pChatPath').value = p.chat_path || '/v1/chat/completions';
  $('#pResponsesPath').value = p.responses_path || '/v1/responses';
  $('#pMessagesPath').value = p.messages_path || '/v1/messages';
  $('#pModelsPath').value = p.models_path || '/v1/models';
  $('#pCountPath').value = p.count_tokens_path || '/v1/messages/count_tokens';
  $('#pForwardHeaders').value = (p.forward_headers || []).join(', ');
  $('#pCredentials').value = '';
  $('#pCredentials').placeholder = editor.mode === 'edit' ? 'Saved pool is write-only. Leave untouched to keep, or enter a complete replacement.' : '[]';
  const first = (p.models || [])[0] || {};
  const caps = first.capabilities || { streaming: true, tools: true, vision: false, reasoning: false };
  $('#pAliases').value = '';
  $('#pCapStreaming').checked = caps.streaming !== false;
  $('#pCapTools').checked = caps.tools !== false;
  $('#pCapVision').checked = !!caps.vision;
  $('#pCapReasoning').checked = !!caps.reasoning;
  const source = $('#secretSource');
  if (source) source.textContent = editor.mode === 'edit' ? `Saved source: ${editor.secretSource}. Keys are write-only. Editing any credential field replaces the entire credential set; re-enter all keys you want to keep.` : '';
  $('#discoverStatus').textContent = '';
  $('#testResults').innerHTML = '';
  renderPicker();
}
function slug(s) {
  return s.toLowerCase().trim().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'model';
}
function defaultModelMeta(m, i = 0) {
  return {
    id: slug(m), model: m,
    aliases: $('#pAliases').value.split(',').map(x => x.trim()).filter(Boolean),
    enabled: true, priority: i, weight: 1,
    capabilities: { streaming: $('#pCapStreaming').checked, tools: $('#pCapTools').checked, vision: $('#pCapVision').checked, reasoning: $('#pCapReasoning').checked }
  };
}
function ensureModelMeta(m, i = 0) {
  if (!editor.modelMeta.has(m)) editor.modelMeta.set(m, defaultModelMeta(m, i));
  const x = editor.modelMeta.get(m);
  x.capabilities = x.capabilities || { streaming: true, tools: true, vision: false, reasoning: false };
  if (!Array.isArray(x.aliases)) x.aliases = [];
  if (!Number.isFinite(x.priority)) x.priority = i;
  if (!Number.isFinite(x.weight) || x.weight <= 0) x.weight = 1;
  return x;
}
function readForm() {
  let hs = {};
  const raw = $('#pHeaders').value.trim();
  if (raw) { try { hs = JSON.parse(raw); } catch { throw new Error('Extra headers must be valid JSON'); } }
  let creds = [];
  const cr = $('#pCredentials').value.trim();
  if (cr) {
    try {
      creds = JSON.parse(cr);
      if (!Array.isArray(creds)) throw new Error();
    } catch { throw new Error('Credential pool must be a JSON array'); }
  }
  const models = [...editor.selected].map((m, i) => {
    const x = ensureModelMeta(m, i), c = x.capabilities || {};
    return {
      id: x.id || slug(m), model: m,
      aliases: [...new Set((x.aliases || []).map(v => String(v).trim()).filter(Boolean))],
      enabled: x.enabled !== false,
      priority: Number.isFinite(Number(x.priority)) ? Number(x.priority) : i,
      weight: Number(x.weight) > 0 ? Number(x.weight) : 1,
      context_window: Number.isFinite(Number(x.context_window)) ? Math.max(0, Number(x.context_window)) : 0,
      input_cost_per_mtok: Number.isFinite(Number(x.input_cost_per_mtok)) ? Math.max(0, Number(x.input_cost_per_mtok)) : 0,
      output_cost_per_mtok: Number.isFinite(Number(x.output_cost_per_mtok)) ? Math.max(0, Number(x.output_cost_per_mtok)) : 0,
      capabilities: { streaming: c.streaming !== false, tools: c.tools !== false, vision: !!c.vision, reasoning: !!c.reasoning }
    };
  });
  const p = {
    id: $('#pId').value.trim(), name: $('#pName').value.trim(), type: $('#pType').value,
    dialect: editor.provider?.dialect || '',
    base_url: $('#pBase').value.trim(), api_key: $('#pKey').value, api_key_env: $('#pKeyEnv').value.trim(),
    credentials: creds, auth_mode: $('#pAuth').value, headers: hs,
    forward_headers: (() => {
      const v = $('#pForwardHeaders').value.split(',').map(x => x.trim()).filter(Boolean);
      return v.length ? v : (editor.mode === 'add' ? null : []);
    })(),
    proxy_url: $('#pProxy').value.trim(),
    chat_path: $('#pChatPath').value.trim(), responses_path: $('#pResponsesPath').value.trim(),
    messages_path: $('#pMessagesPath').value.trim(), models_path: $('#pModelsPath').value.trim(),
    count_tokens_path: $('#pCountPath').value.trim(),
    max_concurrency: Math.max(1, parseInt($('#pConcurrency').value || '32', 10)),
    stream_idle_timeout_seconds: Math.max(1, parseInt($('#pStreamIdle').value || '180', 10)),
    enabled: $('#pEnabled').checked, models
  };
  if (!p.id) throw new Error('Internal ID is required');
  if (!p.name) p.name = p.id;
  if (!p.base_url) throw new Error('Base URL is required');
  return p;
}
function payload(p) {
  return { provider: p, preserve_secret: editor.mode === 'edit' && !editor.secretDirty, preserve_headers: editor.mode === 'edit' && !editor.headersDirty, preserve_proxy: editor.mode === 'edit' && !editor.proxyDirty, test_models: [...editor.selected] };
}
function renderPicker() {
  const all = [...new Set([...editor.detected, ...editor.selected])];
  for (const [i, m] of all.entries()) ensureModelMeta(m, i);
  $('#modelPicker').innerHTML = all.length ? all.map((m, i) => {
    const x = ensureModelMeta(m, i), c = x.capabilities || {};
    return `<div class="model-option">
      <div class="model-option-head"><input class="model-select" type="checkbox" data-model="${esc(m)}" ${editor.selected.has(m) ? 'checked' : ''}><strong>${esc(m)}</strong></div>
      <div class="model-meta-grid">
        <label>Aliases<input data-model="${esc(m)}" data-meta="aliases" value="${esc((x.aliases || []).join(', '))}" placeholder="coding, auto"></label>
        <label>Priority<input data-model="${esc(m)}" data-meta="priority" type="number" value="${Number.isFinite(Number(x.priority)) ? Number(x.priority) : i}"></label>
        <label>Weight<input data-model="${esc(m)}" data-meta="weight" type="number" min="0.01" step="0.1" value="${Number(x.weight) > 0 ? Number(x.weight) : 1}"></label>
        <label>Context window<input data-model="${esc(m)}" data-meta="context_window" type="number" min="0" step="1000" value="${Number.isFinite(Number(x.context_window)) ? Number(x.context_window) : 0}" placeholder="e.g. 200000"></label>
        <label>Input $/MTok<input data-model="${esc(m)}" data-meta="input_cost_per_mtok" type="number" min="0" step="0.01" value="${Number.isFinite(Number(x.input_cost_per_mtok)) ? Number(x.input_cost_per_mtok) : 0}"></label>
        <label>Output $/MTok<input data-model="${esc(m)}" data-meta="output_cost_per_mtok" type="number" min="0" step="0.01" value="${Number.isFinite(Number(x.output_cost_per_mtok)) ? Number(x.output_cost_per_mtok) : 0}"></label>
      </div>
      <div class="model-cap-row">
        <label><input data-model="${esc(m)}" data-cap="streaming" type="checkbox" ${c.streaming !== false ? 'checked' : ''}>Streaming</label>
        <label><input data-model="${esc(m)}" data-cap="tools" type="checkbox" ${c.tools !== false ? 'checked' : ''}>Tools</label>
        <label><input data-model="${esc(m)}" data-cap="vision" type="checkbox" ${c.vision ? 'checked' : ''}>Vision</label>
        <label><input data-model="${esc(m)}" data-cap="reasoning" type="checkbox" ${c.reasoning ? 'checked' : ''}>Reasoning</label>
      </div>
    </div>`;
  }).join('') : '<div class="model-empty" style="color:var(--muted);font-size:10px">No models selected yet — detect or add one.</div>';
  $$('#modelPicker .model-select').forEach(x => x.onchange = () => x.checked ? editor.selected.add(x.dataset.model) : editor.selected.delete(x.dataset.model));
  $$('#modelPicker [data-meta]').forEach(x => x.oninput = () => {
    const m = x.dataset.model, meta = ensureModelMeta(m);
    if (x.dataset.meta === 'aliases') meta.aliases = x.value.split(',').map(v => v.trim()).filter(Boolean);
    else if (x.dataset.meta === 'priority') meta.priority = parseInt(x.value || '0', 10);
    else if (x.dataset.meta === 'weight') meta.weight = Math.max(.01, parseFloat(x.value || '1'));
    else if (x.dataset.meta === 'context_window') meta.context_window = Math.max(0, parseInt(x.value || '0', 10));
    else if (x.dataset.meta === 'input_cost_per_mtok') meta.input_cost_per_mtok = Math.max(0, parseFloat(x.value || '0'));
    else if (x.dataset.meta === 'output_cost_per_mtok') meta.output_cost_per_mtok = Math.max(0, parseFloat(x.value || '0'));
  });
  $$('#modelPicker [data-cap]').forEach(x => x.onchange = () => {
    const meta = ensureModelMeta(x.dataset.model);
    meta.capabilities = meta.capabilities || {};
    meta.capabilities[x.dataset.cap] = x.checked;
  });
}
bind('#addModelBtn', 'onclick', () => {
  const inp = document.querySelector('#manualModel');
  if (!inp) { uiWarn('provider', 'missing element #manualModel'); return; }
  const m = inp.value.trim();
  if (!m) return;
  editor.detected = [...new Set([...editor.detected, m])];
  editor.selected.add(m);
  ensureModelMeta(m, editor.detected.length - 1);
  inp.value = '';
  renderPicker();
}, 'provider');
bind('#manualModel', 'onkeydown', e => { if (e.key === 'Enter') { e.preventDefault(); const b = document.querySelector('#addModelBtn'); if (b && b.click) b.click(); } }, 'provider');
bind('#discoverBtn', 'onclick', async () => {
  try {
    const p = readForm();
    const db = document.querySelector('#discoverBtn');
    if (db) db.disabled = true;
    setText('#discoverStatus', 'Detecting…', 'provider');
    const d = await api('/admin/api/provider-discover', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    if (!d.ok) throw new Error(d.error || 'No models discovered');
    editor.detected = [...new Set([...(d.models || []), ...editor.detected])];
    (d.models || []).forEach((m, i) => ensureModelMeta(m, i));
    if (editor.selected.size === 0) (d.models || []).forEach(m => editor.selected.add(m));
    renderPicker();
    setText('#discoverStatus', `Found ${(d.models || []).length} model(s).`, 'provider');
  } catch (e) {
    setText('#discoverStatus', String(e.message || e).slice(0, 120), 'provider');
    toast(e.message, true);
  } finally { const db2 = document.querySelector('#discoverBtn'); if (db2) db2.disabled = false; }
}, 'provider');
bind('#checkConnectionBtn', 'onclick', async () => {
  try {
    const p = readForm();
    const cb = document.querySelector('#checkConnectionBtn');
    if (cb) cb.disabled = true;
    setText('#connectionStatus', 'Checking…', 'provider');
    const d = await api('/admin/api/provider-check', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    setText('#connectionStatus', d.ok ? `OK — reachable (${d.status_code || 200})` : `Failed: ${d.error || 'unreachable'}`, 'provider');
    const cs = document.querySelector('#connectionStatus');
    if (cs) cs.className = 'inline-status ' + (d.ok ? '' : 'badtext');
  } catch (e) {
    setText('#connectionStatus', String(e.message || e).slice(0, 120), 'provider');
    const cs2 = document.querySelector('#connectionStatus');
    if (cs2) cs2.className = 'inline-status badtext';
  } finally { const cb2 = document.querySelector('#checkConnectionBtn'); if (cb2) cb2.disabled = false; }
}, 'provider');
bind('#testProviderBtn', 'onclick', async () => {
  try {
    const p = readForm();
    if (!editor.selected.size) throw new Error('Select at least one model');
    const tb = document.querySelector('#testProviderBtn');
    if (tb) tb.disabled = true;
    const tr = needEl('#testResults', 'provider');
    if (tr) tr.innerHTML = '<div class="inline-status">Testing…</div>';
    const d = await api('/admin/api/provider-test', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    if (tr) tr.innerHTML = (d.results || []).map(x => `<div class="test-row ${x.ok ? 'ok' : 'fail'}"><strong>${esc(x.model)}</strong><span>${x.ok ? 'PASS' : 'FAIL'}</span><span>${x.status_code || '—'}</span><span>${x.latency_ms} ms</span><small>${esc(x.error || '')}</small></div>`).join('');
    toast(d.ok ? 'All selected models passed' : `${d.passed}/${d.total} models passed`, !d.ok);
  } catch (e) {
    const tr2 = document.querySelector('#testResults');
    if (tr2) tr2.innerHTML = `<div class="inline-status badtext">${esc(String(e.message || e).slice(0, 120))}</div>`;
    toast(e.message, true);
  } finally { const tb2 = document.querySelector('#testProviderBtn'); if (tb2) tb2.disabled = false; }
}, 'provider');
bind('#saveProviderBtn', 'onclick', async () => {
  try {
    const p = readForm(), b = JSON.stringify(payload(p));
    const sb = document.querySelector('#saveProviderBtn');
    if (sb) sb.disabled = true;
    if (editor.mode === 'add') await api('/admin/api/providers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: b });
    else await api('/admin/api/providers/' + encodeURIComponent(editor.originalId), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: b });
    toast('Provider saved and runtime reloaded');
    for (const id of ['#pKey', '#pCredentials', '#pHeaders', '#pProxy']) { const f = document.querySelector(id); if (f) f.value = ''; }
    editor = null;
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
  finally { const sb2 = document.querySelector('#saveProviderBtn'); if (sb2) sb2.disabled = false; }
}, 'provider');
bind('#deleteProviderBtn', 'onclick', async () => {
  const ok = window.NexaUI?.confirm ? await window.NexaUI.confirm('Delete provider', `Delete provider "${editor.originalId}"?`) : false;
  if (!ok) return;
  try {
    await api('/admin/api/providers/' + encodeURIComponent(editor.originalId), { method: 'DELETE' });
    toast('Provider deleted');
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
}, 'provider');

/* ---------- virtual endpoints / route profiles / pools ---------- */
function renderVirtual() {
  const tgt = needEl('#virtualRows', 'render:virtual');
  if (!tgt) return;
  const ves = snap.virtual_endpoints || [];
  const rows = ves.map(ve => {
    const enabled = ve.enabled !== false;
    const poolCount = ve.pool_member_count != null ? ve.pool_member_count : (ve.configured_candidate_count ?? ve.eligible ?? ve.eligible_deployments ?? '—');
    return `<tr>
      <td><strong>${esc(ve.id)}</strong><br><small>${esc(ve.name || '')}</small></td>
      <td><code>${esc(ve.public_model || ve.id)}</code></td>
      <td>${esc(ve.route_profile || '')}</td>
      <td>${enabled ? '<span class=\"pill on\">Enabled</span>' : '<span class=\"pill\">Disabled</span>'}</td>
      <td title="Configured pool members, not runtime eligible (health/compat filtered per-request)">${esc(poolCount)}</td>
      <td><button class=\"btn secondary\" onclick=\"editVirtual('${esc(ve.id)}')\">Edit</button> <button class=\"btn danger-ghost\" onclick=\"deleteVirtual('${esc(ve.id)}')\">Del</button></td>
    </tr>`;
  });
  tgt.innerHTML = rows.join('') || '<tr><td colspan="6" style="color:var(--muted)">No virtual endpoints configured. Create one to get stable public model names.</td></tr>';
}

function renderProfiles() {
  const tgt = needEl('#profileRows', 'render:profiles');
  if (!tgt) return;
  const rps = snap.route_profiles || [];
  const globalStrat = snap.config?.routing?.strategy || 'ready_mesh';
  const rows = rps.map(rp => {
    const strat = rp.strategy ? rp.strategy : `inherit (${esc(globalStrat)})`;
    return `<tr>
      <td><strong>${esc(rp.id)}</strong><br><small>${esc(rp.name || '')}</small></td>
      <td>${esc(rp.name || '')}</td>
      <td>${esc(rp.candidate_pool || '')}</td>
      <td>${esc(rp.fallback_chain || '—')}</td>
      <td title="Phase B: per-profile strategy deferred to Phase E; inherits global">${esc(strat)}</td>
      <td><button class=\"btn secondary\" onclick=\"editProfile('${esc(rp.id)}')\">Edit</button> <button class=\"btn danger-ghost\" onclick=\"deleteProfile('${esc(rp.id)}')\">Del</button></td>
    </tr>`;
  });
  tgt.innerHTML = rows.join('') || '<tr><td colspan="6" style="color:var(--muted)">No route profiles. Create a profile to describe routing intent. Strategy inherits global.</td></tr>';
}

function renderPools() {
  const poolTgt = needEl('#poolRows', 'render:pools');
  const chainTgt = needEl('#chainRows', 'render:pools');
  if (!poolTgt || !chainTgt) return;
  const cps = snap.candidate_pools || [];
  const fcs = snap.fallback_chains || [];
  const poolRows = cps.map(cp => {
    const members = (cp.deployments || []).join(', ') || (cp.mode === 'all' ? '<em>all eligible</em>' : '<em>empty</em>');
    const expanded = cp.expanded_count != null ? cp.expanded_count : (cp.expanded ? cp.expanded.length : '—');
    return `<tr>
      <td><strong>${esc(cp.id)}</strong><br><small>${esc(cp.name || '')}</small></td>
      <td>${esc(cp.mode || 'explicit')}</td>
      <td style=\"max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap\" title=\"${esc((cp.deployments || []).join(', '))}\">${members}</td>
      <td>${esc(expanded)}</td>
      <td><button class=\"btn secondary\" onclick=\"editPool('${esc(cp.id)}')\">Edit</button> <button class=\"btn danger-ghost\" onclick=\"deletePool('${esc(cp.id)}')\">Del</button></td>
    </tr>`;
  });
  poolTgt.innerHTML = poolRows.join('') || '<tr><td colspan="5" style="color:var(--muted)">No candidate pools.</td></tr>';

  const chainRows = fcs.map(fc => {
    const pools = (fc.pools || []).join(' → ');
    return `<tr>
      <td><strong>${esc(fc.id)}</strong><br><small>${esc(fc.name || '')}</small></td>
      <td>${esc(pools)}</td>
      <td><button class=\"btn secondary\" onclick=\"editChain('${esc(fc.id)}')\">Edit</button> <button class=\"btn danger-ghost\" onclick=\"deleteChain('${esc(fc.id)}')\">Del</button></td>
    </tr>`;
  });
  chainTgt.innerHTML = chainRows.join('') || '<tr><td colspan="3" style="color:var(--muted)">No fallback chains.</td></tr>';
}

// Control Plane v2 owns the human-friendly editors. These compatibility
// shims keep legacy inline handlers working without native browser dialogs.
async function editVirtual(id) { return window.NexaUI?.advancedVirtual?.(id); }
async function deleteVirtual(id) { return window.NexaUI?.deleteAdvanced?.('/admin/api/virtual-endpoints', id, 'virtual endpoint'); }
async function editProfile(id) { return window.NexaUI?.advancedProfile?.(id); }
async function deleteProfile(id) { return window.NexaUI?.deleteAdvanced?.('/admin/api/route-profiles', id, 'route profile'); }
async function editPool(id) { return window.NexaUI?.advancedPool?.(id); }
async function deletePool(id) { return window.NexaUI?.deleteAdvanced?.('/admin/api/candidate-pools', id, 'candidate pool'); }
async function editChain(id) { return window.NexaUI?.advancedChain?.(id); }
async function deleteChain(id) { return window.NexaUI?.deleteAdvanced?.('/admin/api/fallback-chains', id, 'fallback chain'); }

// Expose for inline onclick
window.editVirtual = editVirtual;
window.deleteVirtual = deleteVirtual;
window.editProfile = editProfile;
window.deleteProfile = deleteProfile;
window.editPool = editPool;
window.deletePool = deletePool;
window.editChain = editChain;
window.deleteChain = deleteChain;

// Add/edit actions are installed by control-plane-v2.js after app.js loads.

/* ---------- boot ---------- */
(async () => {
  try {
    const d = await api('/admin/api/provider-presets');
    serverPresets = d.presets || null;
  } catch { serverPresets = null; }
    uiSection('boot:presets', fillPresetSelect);
    uiSection('boot:cli', renderCLI);
    tick();
    consumeLiveEvents();
  })();


/* ---------- compatibility matrix (Universal Compatibility Engine) ---------- */
let compatData = { deployments: [] };
let compatBusy = false;

function capChip(v) {
  if (v === 'supported') return '<span class="cap pass">PASS</span>';
  if (v === 'unsupported') return '<span class="cap fail">UNSUP</span>';
  return '<span class="cap unknown">?</span>';
}

function statusChip(st) {
  const cls = st === 'CLAUDE_CODE_READY' ? 'ready' : st === 'CHAT_READY' ? 'chat' : st === 'NOT_AGENT_READY' ? 'not' : 'unverified';
  const label = st === 'CLAUDE_CODE_READY' ? 'AGENT READY' : st === 'CHAT_READY' ? 'CHAT READY' : st === 'NOT_AGENT_READY' ? 'NOT AGENT' : 'UNVERIFIED';
  return `<span class="statuschip ${cls}">${label}</span>`;
}

async function loadCompat() {
  try {
    compatData = await api('/admin/api/compat');
    uiSection('render:compat', renderCompat);
  } catch (e) {
    const tgt = needEl('#compatRows', 'render:compat');
    if (tgt) tgt.innerHTML = `<tr><td colspan="13">${esc(String(e.message || e).slice(0, 120))}</td></tr>`;
  }
}

function renderCompat() {
  const tgt = needEl('#compatRows', 'render:compat');
  if (!tgt) return;
  const rows = (compatData.deployments || []).map(d => {
    const c = d.scorecard?.capabilities || {};
    const sc = d.scorecard || {};
    const issue = d.last_compatibility_issue || '';
    const repair = d.last_repair || '';
    return `<tr>
      <td><strong>${esc(d.deployment)}</strong></td>
      <td>${esc(d.health)}</td>
      <td>${esc(sc.protocol || '—')}</td>
      <td>${capChip(c.text)}</td>
      <td>${capChip(c.streaming)}</td>
      <td>${capChip(c.tools)}</td>
      <td>${capChip(c.parallel_tool_calls)}</td>
      <td>${capChip(c.reasoning)}</td>
      <td>${capChip(c.vision)}</td>
      <td>${capChip(c.temperature)}</td>
      <td>${statusChip(sc.status || 'NOT_VERIFIED')}</td>
      <td>${esc(issue.length > 40 ? issue.slice(0, 40) + '…' : issue) || '<span class="cap na">—</span>'}</td>
      <td>${esc(repair.length > 40 ? repair.slice(0, 40) + '…' : repair) || '<span class="cap na">—</span>'}</td>
    </tr>`;
  });
  tgt.innerHTML = rows.join('') || '<tr><td colspan="13">No deployments configured.</td></tr>';
}

function reportText(r) {
  if (!r) return '';
  const lines = [];
  lines.push(`deployment: ${r.deployment}  model: ${r.model}  level: ${r.level}  ok: ${r.ok}`);
  (r.outcomes || []).forEach(o => lines.push(`  ${o.capability.padEnd(22)} ${o.verdict.padEnd(12)} ${o.detail || ''}`));
  (r.agent_steps || []).forEach(st => lines.push(`  step ${st.step}: ${st.passed ? 'PASS' : 'FAIL'} ${st.detail || ''} (${st.latency_ms} ms)`));
  return lines.join('\n');
}

async function runCompatSuite(mode) {
  if (compatBusy) return;
  const cfg = snap.config || {};
  const providers = (cfg.providers || []).filter(p => p.enabled);
  if (!providers.length) { toast('No enabled providers to test', true); return; }
  compatBusy = true;
  const buttons = [document.querySelector('#compatTestFull'), document.querySelector('#compatTestAgent')];
  buttons.forEach(b => b && (b.disabled = true));
  const report = needEl('#compatReport', 'compat');
  if (report) { try { report.hidden = false; report.textContent = `Running ${mode} suite… (this can take a while; bounded per deployment)`; } catch (e) { uiWarn('compat', e); } }
  try {
    const blocks = [];
    for (const p of providers) {
      const models = (p.models || []).filter(m => m.enabled).map(m => m.id);
      if (!models.length) continue;
      const d = await api('/admin/api/provider-test', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider: p, preserve_secret: true, preserve_headers: true, preserve_proxy: true, test_models: models, mode })
      });
      (d.results || []).forEach(x => {
        const r = mode === 'full' ? x.capability_report : x.agent_report;
        blocks.push(reportText(r) || `${x.model}: ${x.ok ? 'PASS' : 'FAIL'} ${x.error || ''}`);
      });
    }
    setText('#compatReport', blocks.join('\n\n') || 'No models to test.', 'compat');
    await loadCompat();
    toast(mode === 'full' ? 'Full capability suite finished' : 'Agent loop test finished');
  } catch (e) {
    setText('#compatReport', 'Test failed: ' + String(e.message || e).slice(0, 120), 'compat');
    toast(e.message, true);
  } finally {
    compatBusy = false;
    buttons.forEach(b => b && (b.disabled = false));
  }
}

bind('#compatTestFull', 'onclick', () => runCompatSuite('full'), 'compat');
bind('#compatTestAgent', 'onclick', () => runCompatSuite('claude_code'), 'compat');

/* ---------- B3 node telemetry popover (additive, real data only) ----------
   Consumes the additive snap.node_telemetry contract (see
   internal/httpapi/node_telemetry.go). Every displayed KPI shows its source
   and freshness; absent KPIs are omitted, never guessed. No routing decision
   input is read or written here. */
let nodeTelemetryPinned = false;
function nodeTelemetryById() {
  const m = {};
  for (const r of snap.node_telemetry || []) { if (r && r.deployment) m[r.deployment] = r; }
  return m;
}
function nodeTelemetryAgeText(kpi) {
  if (!kpi) return '';
  if (Number.isFinite(Number(kpi.age_ms))) {
    const s = Math.max(0, Math.round(Number(kpi.age_ms) / 1000));
    if (s < 1) return 'just now';
    if (s < 60) return s + 's ago';
    const min = Math.floor(s / 60);
    if (min < 60) return min + 'm ago';
    return Math.floor(min / 60) + 'h ago';
  }
  if (kpi.observed_at) {
    try { return new Date(kpi.observed_at).toLocaleString('en-US', { hour12: false }); } catch { return ''; }
  }
  return '';
}
function nodeTelemetryKpiRow(label, kpi, fmt) {
  if (!kpi || kpi.value === undefined || kpi.value === null || kpi.value === '') return '';
  const fresh = nodeTelemetryAgeText(kpi);
  return `<div><dt>${esc(label)}</dt><dd>${esc(fmt(kpi.value))}</dd>` +
    `<dd class="nt-src">src: ${esc(kpi.source || 'unknown')}${fresh ? ` · ${esc(fresh)}` : ''}</dd></div>`;
}
function nodeTelemetryRowHTML(r) {
  if (!r) return '';
  const fmtPct = v => (Number.isFinite(Number(v)) ? (Number(v) * 100).toFixed(1) + '%' : '—');
  const fmtTimeB3 = v => {
    if (!v) return '—';
    try { const t = new Date(v); return Number.isNaN(t.getTime()) ? '—' : t.toLocaleString('en-US', { hour12: false }); } catch { return '—'; }
  };
  const prov = r.provider ? esc(r.provider.value) + (r.provider_name ? ' · ' + esc(r.provider_name.value) : '') : '';
  const provSrc = r.provider ? `<dd class="nt-src">src: ${esc(r.provider.source || 'unknown')}</dd>` : '';
  // No raw error text is rendered: last_failure exposes only timestamp/source/freshness.
  return `<div class="nt-head"><strong>${esc(r.deployment)}</strong>` +
    (r.state ? `<span class="status ${esc(String(r.state.value).toLowerCase())}">${esc(r.state.value)}</span>` : '<span class="status unknown">UNKNOWN</span>') + '</div>' +
    `<dl class="nt-grid">` +
    (prov ? `<div><dt>Provider</dt><dd>${prov}</dd>${provSrc}</div>` : '') +
    nodeTelemetryKpiRow('Latency (EWMA)', r.latency_ms, v => fmtMs(Number(v))) +
    nodeTelemetryKpiRow('Error rate (EWMA)', r.error_rate, fmtPct) +
    nodeTelemetryKpiRow('Cooldown until', r.cooldown_until, fmtTimeB3) +
    nodeTelemetryKpiRow('Last success', r.last_success, fmtTimeB3) +
    nodeTelemetryKpiRow('Last failure', r.last_failure, fmtTimeB3) +
    (r.state ? `<div><dt>State source</dt><dd class="nt-src">${esc(r.state.source || 'unknown')}${nodeTelemetryAgeText(r.state) ? ` · ${esc(nodeTelemetryAgeText(r.state))}` : ''}</dd></div>` : '') +
    `</dl>`;
}
function showNodeTelemetryPopover(deploymentId, pinned = false) {
  const pop = document.querySelector('#nodeTelemetryPopover');
  if (!pop) return;
  nodeTelemetryPinned = !!pinned;
  const row = nodeTelemetryById()[deploymentId];
  if (!row) { hideNodeTelemetryPopover(); return; }
  pop.hidden = false;
  pop.innerHTML = nodeTelemetryRowHTML(row);
}
function hideNodeTelemetryPopover() {
  nodeTelemetryPinned = false;
  const pop = document.querySelector('#nodeTelemetryPopover');
  if (pop) pop.hidden = true;
}
document.addEventListener('click', ev => {
  const node = ev.target && ev.target.closest ? ev.target.closest('#ring .node') : null;
  if (node && node.dataset && node.dataset.depid) {
    ev.stopPropagation();
    showNodeTelemetryPopover(node.dataset.depid, true);
    return;
  }
  if (nodeTelemetryPinned && !(ev.target.closest && ev.target.closest('#nodeTelemetryPopover'))) hideNodeTelemetryPopover();
});
document.addEventListener('keydown', ev => { if (ev.key === 'Escape') hideNodeTelemetryPopover(); });
bind('#compatReset', 'onclick', async () => {
  try {
    await api('/admin/api/compat/reset', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ deployment: 'all' }) });
    toast('Capability cache reset; fresh probes will re-run');
    await loadCompat();
  } catch (e) { toast(e.message, true); }
}, 'compat');

/* ---------- F1 test hooks (additive; render/snap/faults only) ---------- */
function f1SetSnap(v) { snap = v; }
function f1SetProviders(v) { providerSummaries = v; }
function f1Snap() { return snap; }
function f1Faults() { return { count: uiFaults.count, lastSection: uiFaults.lastSection, lastError: uiFaults.lastError }; }

/* ---------- F8 status API (safe counts/transport only; never payloads/secrets) ---------- */
try {
  const sseScope = typeof window !== 'undefined' ? window : globalThis;
  sseScope.NexaRoute = Object.assign(sseScope.NexaRoute || {}, {
    sseStats,
    sseRetryDelayMs,
    _sseParseFrame: sseParseFrame,
    _sseNoteOpen: sseNoteOpen,
    _sseNoteFailure: sseNoteFailure,
    _sseResetStats: sseResetStats,
    _stopLiveEvents: stopLiveEvents,
    _consumeLiveEvents: consumeLiveEvents,
    _liveSeq: () => liveSeq,
    _sseHooks: sseHooks,
    _render: render,
    _setSnap: f1SetSnap,
    _setProviders: f1SetProviders,
    _snap: f1Snap,
    _uiFaults: f1Faults,
    _uiReset: uiResetFaults,
    _uiSection: uiSection,
  });
} catch {}
