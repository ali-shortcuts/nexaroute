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

const $ = q => document.querySelector(q), $$ = q => [...document.querySelectorAll(q)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;' }[c]));
const fmtInt = n => Number(n || 0).toLocaleString('en-US');
const fmtCompact = n => { if (!Number.isFinite(n) || n <= 0) return '0'; if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B'; if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M'; if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K'; return String(n); };
const fmtMs = n => (Number.isFinite(Number(n)) && Number(n) > 0) ? Math.round(Number(n)) + ' ms' : '—';

let adminKey = sessionStorage.getItem('nexaroute_admin_key') || '';
async function apiFetch(url, opt = {}) {
  opt = { ...opt, headers: { ...(opt.headers || {}) } };
  if (adminKey) opt.headers['x-admin-key'] = adminKey;
  let r = await window.fetch(url, opt);
  if (r.status === 401) {
    const k = await requestAdminKey();
    if (k) {
      adminKey = k.trim();
      sessionStorage.setItem('nexaroute_admin_key', adminKey);
      $('#adminKey').value = adminKey;
      opt.headers['x-admin-key'] = adminKey;
      r = await window.fetch(url, opt);
      if (r.status === 401) {
        sessionStorage.removeItem('nexaroute_admin_key');
        adminKey = '';
        $('#adminKeyDialogError').textContent = 'That admin key was not accepted.';
      }
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
function toast(m, bad = false) {
  const t = $('#toast');
  t.textContent = m;
  t.className = 'toast show ' + (bad ? 'bad' : '');
  clearTimeout(toast.t);
  toast.t = setTimeout(() => t.className = 'toast', 2600);
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

/* ---------- control-plane UX helpers ---------- */
const i18n = {
  en: {
    'nav.overview':'Overview','nav.providers':'Providers','nav.routing':'Routing','nav.models':'Models','nav.observability':'Observability','nav.health':'Health','nav.connect':'Connect','nav.settings':'Settings',
    'providers.title':'Providers','providers.subtitle':'Connect an API, discover its models, choose what NexaRoute can use, and save.',
    'routing.title':'Routing','routing.subtitle':'Create a stable public model and choose which saved deployments may serve it. NexaRoute keeps the advanced router underneath.'
  },
  fa: {
    'nav.overview':'نمای کلی','nav.providers':'ارائه‌دهنده‌ها','nav.routing':'مسیریابی','nav.models':'مدل‌ها','nav.observability':'نظارت','nav.health':'سلامت','nav.connect':'اتصال','nav.settings':'تنظیمات',
    'providers.title':'ارائه‌دهنده‌ها','providers.subtitle':'API را وصل کنید، مدل‌ها را شناسایی کنید، مدل‌های مورد استفاده را انتخاب کرده و ذخیره کنید.',
    'routing.title':'مسیریابی','routing.subtitle':'یک نام مدل عمومی ثابت بسازید و مدل‌هایی را که اجازه سرویس‌دهی دارند انتخاب کنید؛ موتور قدرتمند NexaRoute در پشت صحنه باقی می‌ماند.'
  }
};
let locale = localStorage.getItem('nexaroute_locale') || 'en';
function applyLocale(next = locale) {
  locale = i18n[next] ? next : 'en';
  localStorage.setItem('nexaroute_locale', locale);
  document.documentElement.lang = locale === 'fa' ? 'fa' : 'en';
  document.documentElement.dir = locale === 'fa' ? 'rtl' : 'ltr';
  document.body.classList.toggle('rtl', locale === 'fa');
  $('[data-i18n]').forEach(el => {
    const v = i18n[locale][el.dataset.i18n];
    if (v) el.textContent = v;
  });
  const sel = $('#languageSelect');
  if (sel) sel.value = locale;
}
let theme = localStorage.getItem('nexaroute_theme') || 'system';
function applyTheme(next = theme) {
  theme = ['system','dark','light'].includes(next) ? next : 'system';
  localStorage.setItem('nexaroute_theme', theme);
  const effective = theme === 'system' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
  document.body.classList.toggle('theme-light', effective === 'light');
  document.body.classList.toggle('theme-dark', effective === 'dark');
  const sel = $('#themeSelect');
  if (sel) sel.value = theme;
}
function requestAdminKey() {
  return new Promise(resolve => {
    const m = $('#adminKeyDialog'), inp = $('#adminKeyDialogInput'), err = $('#adminKeyDialogError');
    err.textContent = ''; inp.value = '';
    m.classList.add('open'); m.setAttribute('aria-hidden','false');
    document.body.classList.add('modal-open');
    const finish = value => {
      m.classList.remove('open'); m.setAttribute('aria-hidden','true');
      document.body.classList.remove('modal-open');
      $('#adminKeyDialogSave').onclick = null;
      inp.onkeydown = null;
      resolve(value);
    };
    $('#adminKeyDialogSave').onclick = () => finish(inp.value.trim());
    inp.onkeydown = e => { if (e.key === 'Enter') finish(inp.value.trim()); };
    setTimeout(() => inp.focus(), 0);
  });
}
function showConfirm({title='Confirm', message='', accept='Continue', danger=true} = {}) {
  return new Promise(resolve => {
    const m = $('#confirmDialog');
    $('#confirmTitle').textContent = title;
    $('#confirmMessage').textContent = message;
    const yes = $('#confirmAccept'), no = $('#confirmCancel');
    yes.textContent = accept;
    yes.className = danger ? 'btn danger' : 'btn primary';
    m.classList.add('open'); m.setAttribute('aria-hidden','false');
    document.body.classList.add('modal-open');
    const done = value => {
      m.classList.remove('open'); m.setAttribute('aria-hidden','true');
      document.body.classList.remove('modal-open');
      yes.onclick = no.onclick = null;
      $('[data-close-confirm]').forEach(x => x.onclick = null);
      resolve(value);
    };
    yes.onclick = () => done(true);
    no.onclick = () => done(false);
    $('[data-close-confirm]').forEach(x => x.onclick = () => done(false));
  });
}
function showFormDialog({title='Edit', subtitle='', fields=[]} = {}) {
  return new Promise(resolve => {
    const m = $('#formDialog'), box = $('#formDialogFields');
    $('#formDialogTitle').textContent = title;
    $('#formDialogSubtitle').textContent = subtitle || '';
    box.innerHTML = fields.map(f => {
      const value = f.value ?? '';
      if (f.type === 'select') return `<label class="field"><span>${esc(f.label)}</span><select data-key="${esc(f.key)}">${(f.options||[]).map(o => `<option value="${esc(o.value)}" ${String(o.value)===String(value)?'selected':''}>${esc(o.label)}</option>`).join('')}</select>${f.help?`<em>${esc(f.help)}</em>`:''}</label>`;
      if (f.type === 'checkbox') return `<label class="check-row dialog-check"><input data-key="${esc(f.key)}" type="checkbox" ${value ? 'checked':''}><span>${esc(f.label)}</span></label>`;
      if (f.type === 'textarea') return `<label class="field"><span>${esc(f.label)}</span><textarea data-key="${esc(f.key)}" rows="${f.rows||4}" placeholder="${esc(f.placeholder||'')}">${esc(value)}</textarea>${f.help?`<em>${esc(f.help)}</em>`:''}</label>`;
      return `<label class="field"><span>${esc(f.label)}</span><input data-key="${esc(f.key)}" type="${f.type||'text'}" value="${esc(value)}" placeholder="${esc(f.placeholder||'')}" ${f.readonly?'readonly':''}>${f.help?`<em>${esc(f.help)}</em>`:''}</label>`;
    }).join('');
    m.classList.add('open'); m.setAttribute('aria-hidden','false');
    document.body.classList.add('modal-open');
    const save = $('#formDialogSave'), cancel = $('#formDialogCancel');
    const done = value => {
      m.classList.remove('open'); m.setAttribute('aria-hidden','true');
      document.body.classList.remove('modal-open');
      save.onclick = cancel.onclick = $('#closeFormDialog').onclick = null;
      $('[data-close-form-dialog]').forEach(x => x.onclick = null);
      resolve(value);
    };
    save.onclick = () => {
      const out = {};
      fields.forEach(f => {
        const el = box.querySelector(`[data-key="${CSS.escape(f.key)}"]`);
        out[f.key] = f.type === 'checkbox' ? !!el.checked : el.value;
      });
      done(out);
    };
    cancel.onclick = $('#closeFormDialog').onclick = () => done(null);
    $('[data-close-form-dialog]').forEach(x => x.onclick = () => done(null));
    setTimeout(() => box.querySelector('input,select,textarea')?.focus(), 0);
  });
}

/* ---------- navigation ---------- */
const subtitles = {
  overview: 'Ready Mesh: verified health, session affinity, capacity-aware routing and supervised recovery.',
  console: 'Every routing decision, probe, failover and recovery — as it happens.',
  providers: 'Connect APIs, discover models and manage saved credentials without exposing secrets.',
  routing: 'Stable public models on top of NexaRoute’s existing pools, profiles and failover engine.',
  models: 'Per-deployment routing state: health, latency and failure tracking.',
  virtual: 'Virtual Endpoints: stable public model names → Route Profile → Candidate Pool. Change backends without client reconfig.',
  profiles: 'Route Profiles: reusable routing intent and policy.',
  pools: 'Candidate Pools and Fallback Chains: pools define configured candidates; health/compat defines runtime eligible per-request.',
  health: 'Live provider pressure and capability-scoped circuit evidence.',
  cli: 'One-click connection snippets for coding agents and OpenAI-compatible tools.',
  settings: 'Hot-reloaded routing and probe configuration.',
  compat: 'Universal Compatibility Engine: verified model capabilities, repairs and the Claude Code scorecard.'
};
$$('nav button').forEach(b => b.onclick = () => {
  $$('nav button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  $$('.tab').forEach(x => x.classList.remove('active'));
  $('#' + b.dataset.tab).classList.add('active');
  $('#title').textContent = b.dataset.title;
  $('#subtitle').textContent = subtitles[b.dataset.tab] || '';
  if (b.dataset.tab === 'settings') fillRuntimeSettings();
  if (b.dataset.tab === 'routing') renderRouting();
  if (b.dataset.tab === 'console') { consoleUnread = 0; $('#consoleDot').hidden = true; renderConsole(); }
  if (b.dataset.tab === 'cli') renderCLI();
  if (b.dataset.tab === 'compat') loadCompat();
  if (b.dataset.tab === 'virtual') renderVirtual();
  if (b.dataset.tab === 'profiles') renderProfiles();
  if (b.dataset.tab === 'pools') renderPools();
  if (b.dataset.tab === 'routing') { renderVirtual(); renderProfiles(); renderPools(); }
});
$('#pauseBtn').onclick = () => {
  paused = !paused;
  $('#pauseBtn').textContent = paused ? '▶ Resume' : '⏸ Pause';
  $('#pauseBtn').classList.toggle('active-btn', paused);
  if (!paused) tick();
};

/* ---------- runtime settings ---------- */
function intVal(id, fallback, min = 0) {
  const n = parseInt($(id).value, 10);
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
$('#saveRuntimeSettings').onclick = async () => {
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
    $('#saveRuntimeSettings').disabled = true;
    await api('/admin/api/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    toast('Runtime settings saved and reloaded');
    await refresh();
    fillRuntimeSettings();
  } catch (e) { toast(e.message, true); }
  finally { $('#saveRuntimeSettings').disabled = false; }
};
$('#adminKey').value = adminKey;
$('#saveAdminKey').onclick = () => {
  adminKey = $('#adminKey').value.trim();
  sessionStorage.setItem('nexaroute_admin_key', adminKey);
  toast('Admin key saved for this browser session');
};
$('#toggleAdminKey').onclick = () => toggleSecret('#adminKey', '#toggleAdminKey');

$('#probeBtn').onclick = async () => {
  try {
    $('#probeBtn').disabled = true;
    $('#probeBtn').textContent = 'Probing…';
    const d = await api('/admin/api/probe?wait=1', { method: 'POST' });
    const r = d.result || {};
    toast(`Probe complete: ${r.passed || 0}/${r.total || 0} passed${r.skipped_cooldown ? `, ${r.skipped_cooldown} cooldown` : ''}`, !!r.failed);
    await refresh();
  } catch (e) { toast(e.message, true); }
  finally { $('#probeBtn').disabled = false; $('#probeBtn').textContent = '⚡ Probe all models'; }
};

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
  $('#routerStrategyLabel').textContent = snap.config?.routing?.strategy || 'ready_mesh';
  $('#coreSub').textContent = (snap.config?.routing?.strategy || 'ready_mesh').replace(/_/g, ' ');
  $('#sDeploy').textContent = fmtInt(snap.deployment_total ?? ds.length);
  const c = healthCounts();
  const ok = Number(c.healthy || 0), cd = Number(c.cooldown || 0), retired = Number(c.retired || 0);
  const total = snap.deployment_total ?? ds.length;
  $('#sHealthy').textContent = fmtInt(ok);
  $('#sCooldown').textContent = fmtInt(cd);
  $('#sHealthBar').style.width = total ? Math.round(ok / total * 100) + '%' : '0%';
  $('#sCooldownSub').textContent = retired ? `${cd} cooling · ${retired} retired` : (cd ? 'recovering in background' : 'none recovering');
  $('#sSessions').textContent = fmtInt(snap.session_count ?? 0);
  const ls = (snap.events || []).filter(e => e.kind === 'route_ok' && e.latency_ms).map(e => e.latency_ms);
  const avg = ls.length ? Math.round(ls.reduce((a, b) => a + b, 0) / ls.length) : 0;
  $('#sLatency').textContent = avg ? avg + ' ms' : '—';
  renderRing(ds, h);
  renderDonut(c, total);
  renderProviders(h);
  renderModels(ds, h);
  renderHealthTab(h);
  renderRoutingObservatory(h, ds);
  if ($('#compat').classList.contains('active')) renderCompat();
  if ($('#virtual').classList.contains('active')) renderVirtual();
  if ($('#profiles').classList.contains('active')) renderProfiles();
  if ($('#pools').classList.contains('active')) renderPools();
  if ($('#routing').classList.contains('active')) renderRouting();
  renderConsole();
  $('#settingsJson').textContent = JSON.stringify(snap.config || {}, null, 2);
  // Cache + usage KPI cards (v0.5)
  const ch = Number(snap.cache?.hits ?? 0), cm = Number(snap.cache?.misses ?? 0);
  const rate = (ch + cm) ? Math.round(ch / (ch + cm) * 100) + '%' : '—';
  $('#sCacheRate').textContent = rate;
  $('#sCacheSub').textContent = snap.cache?.hits != null ? (fmtInt(ch) + ' hits / ' + fmtInt(cm) + ' misses') : 'exact-match cache off';
  const up = Number(snap.usage?.total_prompt_tokens ?? 0), ucp = Number(snap.usage?.total_completion_tokens ?? 0);
  $('#sTokens').textContent = fmtCompact(up + ucp);
  const cost = Number(snap.usage?.total_estimated_cost_usd ?? 0);
  $('#sSpendSub').textContent = cost > 0 ? ('~$' + (cost >= 1 ? cost.toFixed(2) : cost.toFixed(4)) + ' est. spend') : 'no pricing configured';
  $('#footRequests').textContent = fmtInt(snap.request_total ?? 0) + ' requests';
  $('#brandVersion').textContent = 'v' + (snap.version || '0.5') + ' • control plane';
}

function renderRing(ds, h) {
  const ring = $('#ring');
  [...ring.querySelectorAll('.node')].forEach(x => x.remove());
  const svg = $('#links');
  svg.innerHTML = '';
  const shown = ds.slice(0, 100), W = ring.clientWidth, H = ring.clientHeight, cx = W / 2, cy = H / 2;
  ring.classList.toggle('dense', shown.length > 24);
  const ringCount = Math.max(1, Math.min(4, Math.ceil(shown.length / 25)));
  shown.forEach((d, i) => {
    const ri = Math.floor(i / 25), start = ri * 25, count = Math.min(25, shown.length - start), pos = i - start;
    const a = Math.PI * 2 * pos / Math.max(count, 1) - Math.PI / 2;
    const frac = ringCount === 1 ? 1 : (ri + 1) / ringCount;
    const rx = shown.length > 24 ? 110 + frac * Math.min(W * .34, 300) : Math.min(W * .38, 360);
    const ry = shown.length > 24 ? 60 + frac * Math.min(H * .31, 210) : Math.min(H * .36, 190);
    const x = cx + Math.cos(a) * rx, y = cy + Math.sin(a) * ry;
    const st = (h[d.id] || {}).status || 'unknown';
    const n = document.createElement('div');
    n.className = 'node ' + st;
    n.style.left = x + 'px'; n.style.top = y + 'px';
    n.title = `${d.model} • ${d.provider_name || d.provider_id} • ${st} • ${fmtMs((h[d.id] || {}).ewma_latency_ms)}`;
    n.innerHTML = `<strong>${esc(d.model)}</strong><small>${esc(d.provider_name || d.provider_id)}</small><span class="badge">${esc(st)}</span>`;
    ring.appendChild(n);
    const l = document.createElementNS('http://www.w3.org/2000/svg', 'line');
    for (const [k, v] of Object.entries({
      x1: cx, y1: cy, x2: x, y2: y,
      stroke: st === 'healthy' ? 'rgba(63,224,140,.4)' : st === 'cooldown' ? 'rgba(255,110,125,.4)' : st === 'retired' ? 'rgba(142,151,168,.32)' : st === 'degraded' ? 'rgba(255,200,97,.4)' : st === 'half_open' ? 'rgba(201,184,255,.4)' : 'rgba(70,98,138,.3)',
      'stroke-width': 1.2, 'stroke-dasharray': '3 6'
    })) l.setAttribute(k, v);
    svg.appendChild(l);
  });
  if (ds.length > 100) {
    const n = document.createElement('div');
    n.className = 'node degraded';
    n.style.left = cx + 'px'; n.style.top = (H - 30) + 'px';
    n.innerHTML = `<strong>+${ds.length - 100} more</strong><small>See Models table</small>`;
    ring.appendChild(n);
  }
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
    const el = $('#' + id);
    const frac = total ? count / total : 0;
    const len = frac * circ;
    el.setAttribute('stroke-dasharray', `${len} ${circ - len}`);
    el.setAttribute('stroke-dashoffset', -offset);
    offset += len;
    if (count > 0 || label === 'healthy') legend.push(`<li><i style="background:${color}"></i>${label}<b>${fmtInt(count)}</b></li>`);
  }
  $('#donutLegend').innerHTML = legend.join('');
  const readyPct = total ? Math.round((counts.healthy || 0) / total * 100) : 0;
  $('#donutPct').textContent = readyPct + '%';
}

function providerColor(id) {
  let hash = 0;
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) >>> 0;
  const hues = [168, 190, 205, 250, 268, 150, 285, 220];
  const h = hues[hash % hues.length];
  return `linear-gradient(135deg,hsl(${h} 70% 52%),hsl(${(h + 40) % 360} 65% 45%))`;
}

function renderProviders(h) {
  const providerQuery = ($('#providerSearch')?.value || '').trim().toLowerCase();
  const incidents = Object.fromEntries((snap.provider_health || []).map(x => [x.provider, x]));
  const stats = Object.fromEntries((snap.provider_stats || []).map(x => [x.id, x]));
  $('#providerGrid').innerHTML = providerSummaries.filter(p => !providerQuery || (p.name || p.id || '').toLowerCase().includes(providerQuery) || (p.type || '').toLowerCase().includes(providerQuery) || (p.base_url || '').toLowerCase().includes(providerQuery)).map(p => {
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
  const q = ($('#modelSearch').value || '').toLowerCase();
  const rows = ds.filter(d => !q || d.id.toLowerCase().includes(q) || (d.provider_id || '').toLowerCase().includes(q) || (d.model || '').toLowerCase().includes(q));
  const maxLat = Math.max(1, ...ds.map(d => Number((h[d.id] || {}).ewma_latency_ms || 0)));
  $('#modelRows').innerHTML = rows.map(d => {
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
$('#modelSearch').oninput = () => renderModels(snap.deployments || [], healthMap());

function renderHealthTab(h) {
  const incidents = Object.fromEntries((snap.provider_health || []).map(x => [x.provider, x]));
  const stats = Object.fromEntries((snap.provider_stats || []).map(x => [x.id, x]));
  const rows = (snap.provider_pressure || []);
  $('#providerHealthRows').innerHTML = rows.map(p => {
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
  $('#scopeHealthList').innerHTML = scopes.map(s => {
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
  $('#obsRequest').textContent = latest?.request_id ? latest.request_id.slice(0, 22) : '—';
  $('#obsPublic').textContent = publicEvent?.public_model || '—';

  const failures = requestEvents.filter(e => e.kind === 'route_fail' || e.kind === 'route_timeout' || e.kind === 'response_decode_fail' || e.kind === 'stream_fail_precommit');
  $('#obsAbsorbed').textContent = fmtInt(failures.length);

  const success = requestEvents.some(e => e.kind === 'route_ok');
  const exhausted = requestEvents.some(e => e.kind === 'candidate_exhausted');
  const result = success ? 'SUCCESS' : exhausted ? 'EXHAUSTED' : requestEvents.length ? 'ACTIVE' : 'IDLE';
  $('#obsResult').textContent = result;
  $('#obsResult').className = result.toLowerCase();

  const stateByDeployment = {};
  for (const e of requestEvents) {
    if (!e.deployment) continue;
    if (e.kind === 'model_retired') stateByDeployment[e.deployment] = 'retired';
    else if (e.kind === 'model_unavailable') stateByDeployment[e.deployment] = 'unavailable';
    else if (e.kind === 'route_ok') stateByDeployment[e.deployment] = 'success';
    else if (e.kind === 'route_fail' || e.kind === 'route_timeout' || e.kind === 'response_decode_fail' || e.kind === 'stream_fail_precommit') {
      stateByDeployment[e.deployment] = e.error_type === 'provider_rate_limited' ? 'cooldown' : 'failed';
    }
  }
  const attempts = requestEvents.filter(e => e.kind === 'route_attempt');
  $('#routeFlow').innerHTML = attempts.length ? attempts.slice(-10).map((e, i) => {
    const st = stateByDeployment[e.deployment] || (i === attempts.length - 1 && !success ? 'active' : 'failed');
    const d = byID[e.deployment] || {};
    return `<div class="route-hop ${esc(st)}" title="${esc(e.deployment || '')}">
      <strong>${esc(modelLabel(e.deployment))}</strong>
      <small>${esc(d.provider_name || d.provider_id || e.deployment || '')}</small>
      <em>${esc(st.replace(/_/g, ' '))}</em>
    </div>`;
  }).join('') : '<div class="obs-empty">No routed request yet.</div>';

  const journey = requestEvents.filter(e => routeKinds.has(e.kind)).slice(-12);
  $('#routeJourney').innerHTML = journey.length ? journey.map(e => {
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
  $('#supervisorList').innerHTML = supervisor.length ? supervisor.map(x => {
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
  $('#failureRadar').innerHTML = radar.length ? radar.map(([k, n]) => `<span class="radar-chip">${esc(k)} <b>${fmtInt(n)}</b></span>`).join('') : '<div class="obs-empty">No recent failures.</div>';
}

/* ---------- console ---------- */
const consoleKinds = {
  routes: new Set(['route_ok', 'route_attempt', 'route_fail', 'route_skip', 'route_timeout', 'failover', 'model_unavailable', 'model_retired', 'candidate_exhausted', 'client_disconnect', 'response_decode_fail', 'stream_fail', 'gateway_overloaded']),
  errors: new Set(['route_fail', 'route_timeout', 'model_unavailable', 'model_retired', 'candidate_exhausted', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail', 'gateway_overloaded', 'internal_panic', 'client_disconnect', 'probe_fail', 'probe_quarantine', 'recovery_fail', 'recovery_queue_full']),
  probes: new Set(['probe_ready', 'probe_fail', 'probe_quarantine', 'recovery_ready', 'recovery_fail', 'recovery_wait', 'recovery_deferred', 'recovery_cooldown', 'recovery_queue_full', 'model_unavailable', 'model_retired', 'stream_fail_precommit'])
};
function renderConsole() {
  const box = $('#consoleLog');
  if (!box) return;
  const stick = $('#consoleAuto').checked && (box.scrollHeight - box.scrollTop - box.clientHeight < 60);
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
  $('#consoleCount').textContent = fmtInt(es.length) + ' events';
  if (stick) box.scrollTop = box.scrollHeight;
}
$$('#consoleFilter button').forEach(b => b.onclick = () => {
  $$('#consoleFilter button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  consoleFilter = b.dataset.f;
  renderConsole();
});

/* ---------- data refresh loop ---------- */
async function refresh() {
  try {
    const [s, p] = await Promise.all([
      api('/admin/api/snapshot?limit=500&events=100'),
      api('/admin/api/providers')
    ]);
    snap = s;
    providerSummaries = p.providers || [];
    // latency timeline from recent successful routes
    const lats = (snap.events || []).filter(e => e.kind === 'route_ok' && e.latency_ms).map(e => e.latency_ms).slice(0, 60).reverse();
    if (lats.length) {
      latencyHistory = lats;
      drawChart(lats);
    }
    const st = $('#apiState');
    st.className = 'conn ok';
    st.querySelector('.conn-text').textContent = 'connected';
    render();
  } catch (e) {
    const st = $('#apiState');
    st.className = 'conn err';
    st.querySelector('.conn-text').textContent = 'disconnected';
  }
}
function drawChart(vals) {
  const W = 320, H = 96, max = Math.max(...vals, 1);
  const step = vals.length > 1 ? W / (vals.length - 1) : W;
  const pts = vals.map((v, i) => [i * step, H - 6 - (v / max) * (H - 16)]);
  const line = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  $('#chartLine').setAttribute('d', line);
  $('#chartArea').setAttribute('d', line + ` L ${W} ${H} L 0 ${H} Z`);
  const last = vals[vals.length - 1];
  $('#chartLast').textContent = 'last: ' + Math.round(last) + ' ms';
  $('#chartMax').textContent = 'peak: ' + Math.round(max) + ' ms';
  // KPI sparkline
  const sw = 120, sh = 28;
  const smax = Math.max(...vals, 1);
  const sstep = vals.length > 1 ? sw / (vals.length - 1) : sw;
  const spts = vals.map((v, i) => [i * sstep, sh - 2 - (v / smax) * (sh - 6)]);
  const sline = spts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  $('#latencySpark .spark-line').setAttribute('d', sline);
  $('#latencySpark .spark-area').setAttribute('d', sline + ` L ${sw} ${sh} L 0 ${sh} Z`);
}
async function tick() {
  if (!paused) {
    const had = (snap.events || []).length;
    await refresh();
    const now = (snap.events || []).length;
    if (now > had && !$('#console').classList.contains('active') && (snap.events || []).some(e => consoleKinds.errors.has(e.kind))) {
      $('#consoleDot').hidden = false;
      consoleUnread++;
    }
    if ($('#console').classList.contains('active')) { consoleUnread = 0; $('#consoleDot').hidden = true; }
  }
  const n = snap.deployment_total ?? (snap.deployments || []).length;
  const delay = n > 5000 ? 15000 : n > 1000 ? 8000 : n > 250 ? 4000 : 1800;
  setTimeout(tick, delay);
}
window.addEventListener('resize', () => { renderRing(snap.deployments || [], healthMap()); });
setInterval(() => { $('#footClock').textContent = new Date().toLocaleTimeString('en-US', { hour12: false }); }, 1000);

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
  const active = tabs.find(b => b.classList.contains('active')) || tabs[0];
  const s = cliSnippet(active.dataset.cli);
  $('#cliBody').innerHTML = `
    <div class="cli-card">
      <div class="cli-card-head"><span>${esc(s.title)}</span><button class="copy-btn" id="cliCopy">Copy</button></div>
      <pre>${s.body}</pre>
    </div>
    <p class="cli-note">${esc(s.note)}</p>`;
  $('#cliCopy').onclick = e => copyText($('#cliBody pre').innerText, e.target);
}
$$('#cliTabs button').forEach(b => b.onclick = () => {
  $$('#cliTabs button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  renderCLI();
});

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
  $('#providerModal').classList.toggle('open', open);
  $('#providerModal').setAttribute('aria-hidden', open ? 'false' : 'true');
  document.body.classList.toggle('modal-open', open);
}
function toggleSecret(i, b) {
  const el = $(i);
  el.type = el.type === 'password' ? 'text' : 'password';
  $(b).textContent = el.type === 'password' ? 'Show' : 'Hide';
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
  const sel = $('#pPreset');
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
$('#addProviderBtn').onclick = () => {
  const n = Math.max(1, providerSummaries.length + 1);
  const p = emptyProvider();
  p.name = `Provider ${n}`;
  p.id = `provider-${n}`;
  editor = { mode: 'add', originalId: '', provider: p, detected: [], selected: new Set(), modelMeta: new Map(), secretDirty: true, secretSource: 'none', hasSecret: false };
  fillForm(); modal(true);
};
$('#closeProviderModal').onclick = () => modal(false);
$('#cancelProviderBtn').onclick = () => modal(false);
$$('[data-close-modal]').forEach(x => x.onclick = () => modal(false));
document.addEventListener('keydown', e => { if (e.key === 'Escape') modal(false); });
$('#togglePKey').onclick = () => toggleSecret('#pKey', '#togglePKey');
$('#pKey').oninput = () => editor.secretDirty = true;
$('#pKeyEnv').oninput = () => editor.secretDirty = true;
$('#pCredentials').oninput = () => editor.secretDirty = true;
$('#pHeaders').oninput = () => editor.headersDirty = true;
$('#pProxy').oninput = () => editor.proxyDirty = true;
$('#pPreset').onchange = () => applyPreset($('#pPreset').value);
$('#pType').onchange = () => {
  const a = $('#pAuth'), typ = $('#pType').value;
  if (typ === 'anthropic_compatible' && (a.value === 'bearer' || a.value === 'x-goog-api-key')) a.value = 'x-api-key';
  if (typ === 'gemini' && (a.value === 'bearer' || a.value === 'x-api-key')) a.value = 'x-goog-api-key';
  if ((typ === 'openai_compatible' || typ === 'openai_responses') && (a.value === 'x-api-key' || a.value === 'x-goog-api-key')) a.value = 'bearer';
};
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
      secretDirty: false, secretSource: d.secret_source || 'none', hasSecret: !!d.has_secret
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
  const hasSaved = editor.mode === 'edit' && (editor.hasSecret || editor.secretSource !== 'none');
  $('#savedCredential').hidden = !hasSaved;
  $('#apiKeyField').hidden = hasSaved;
  $('#savedCredentialMeta').textContent = editor.secretSource && editor.secretSource !== 'none' ? `Saved via ${editor.secretSource}; value is never returned to the browser.` : 'Stored securely; value is never returned to the browser.';
  $('#secretSource').textContent = '';
  $('#advancedProvider').open = false;
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
  const q = ($('#modelPickerSearch')?.value || '').trim().toLowerCase();
  const visible = all.filter(m => !q || m.toLowerCase().includes(q));
  $('#modelPicker').innerHTML = visible.length ? visible.map((m, i) => {
    const x = ensureModelMeta(m, i), c = x.capabilities || {};
    const savedOnly = editor.mode === 'edit' && editor.selected.has(m) && !editor.detected.includes(m);
    return `<div class="model-option compact">
      <label class="model-option-head"><input class="model-select" type="checkbox" data-model="${esc(m)}" ${editor.selected.has(m) ? 'checked' : ''}><span class="model-main"><strong>${esc(m)}</strong><small>${savedOnly ? 'Saved model · not currently discovered' : 'Detected model'}</small></span><span class="model-badges">${c.tools!==false?'<i>Tools</i>':''}${c.vision?'<i>Vision</i>':''}${c.reasoning?'<i>Reasoning</i>':''}${c.streaming!==false?'<i>Stream</i>':''}</span></label>
      <details class="model-advanced"><summary>Advanced model settings</summary>
        <div class="model-meta-grid">
          <label>Aliases<input data-model="${esc(m)}" data-meta="aliases" value="${esc((x.aliases || []).join(', '))}" placeholder="coding, auto"></label>
          <label>Priority<input data-model="${esc(m)}" data-meta="priority" type="number" value="${Number.isFinite(Number(x.priority)) ? Number(x.priority) : i}"></label>
          <label>Weight<input data-model="${esc(m)}" data-meta="weight" type="number" min="0.01" step="0.1" value="${Number(x.weight) > 0 ? Number(x.weight) : 1}"></label>
          <label>Context window<input data-model="${esc(m)}" data-meta="context_window" type="number" min="0" step="1000" value="${Number.isFinite(Number(x.context_window)) ? Number(x.context_window) : 0}"></label>
          <label>Input $/MTok<input data-model="${esc(m)}" data-meta="input_cost_per_mtok" type="number" min="0" step="0.01" value="${Number.isFinite(Number(x.input_cost_per_mtok)) ? Number(x.input_cost_per_mtok) : 0}"></label>
          <label>Output $/MTok<input data-model="${esc(m)}" data-meta="output_cost_per_mtok" type="number" min="0" step="0.01" value="${Number.isFinite(Number(x.output_cost_per_mtok)) ? Number(x.output_cost_per_mtok) : 0}"></label>
        </div>
        <div class="model-cap-row">
          <label><input data-model="${esc(m)}" data-cap="streaming" type="checkbox" ${c.streaming !== false ? 'checked' : ''}>Streaming</label>
          <label><input data-model="${esc(m)}" data-cap="tools" type="checkbox" ${c.tools !== false ? 'checked' : ''}>Tools</label>
          <label><input data-model="${esc(m)}" data-cap="vision" type="checkbox" ${c.vision ? 'checked' : ''}>Vision</label>
          <label><input data-model="${esc(m)}" data-cap="reasoning" type="checkbox" ${c.reasoning ? 'checked' : ''}>Reasoning</label>
        </div>
      </details>
    </div>`;
  }).join('') : '<div class="model-empty">No models match this search. Detect models or add a model ID manually.</div>';
  const updateCount = () => { if ($('#modelSelectedCount')) $('#modelSelectedCount').textContent = `${editor.selected.size} selected`; };
  $$('#modelPicker .model-select').forEach(x => x.onchange = () => { x.checked ? editor.selected.add(x.dataset.model) : editor.selected.delete(x.dataset.model); updateCount(); });
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
  updateCount();
}
$('#providerSearch')?.addEventListener('input', () => renderProviders(healthMap()));
$('#languageSelect')?.addEventListener('change', e => applyLocale(e.target.value));
$('#themeSelect')?.addEventListener('change', e => applyTheme(e.target.value));
matchMedia('(prefers-color-scheme: light)').addEventListener?.('change', () => { if (theme === 'system') applyTheme('system'); });
$('#replaceCredentialBtn')?.addEventListener('click', () => {
  $('#savedCredential').hidden = true;
  $('#apiKeyField').hidden = false;
  $('#pKey').focus();
  editor.secretDirty = true;
});
$('#modelPickerSearch')?.addEventListener('input', renderPicker);
$('#selectAllModels')?.addEventListener('click', () => { [...new Set([...editor.detected, ...editor.selected])].forEach(m => editor.selected.add(m)); renderPicker(); });
$('#selectVisibleModels')?.addEventListener('click', () => {
  const q = ($('#modelPickerSearch')?.value || '').trim().toLowerCase();
  [...new Set([...editor.detected, ...editor.selected])].filter(m => !q || m.toLowerCase().includes(q)).forEach(m => editor.selected.add(m));
  renderPicker();
});
$('#clearModels')?.addEventListener('click', () => { editor.selected.clear(); renderPicker(); });

$('#addModelBtn').onclick = () => {
  const m = $('#manualModel').value.trim();
  if (!m) return;
  editor.detected = [...new Set([...editor.detected, m])];
  editor.selected.add(m);
  ensureModelMeta(m, editor.detected.length - 1);
  $('#manualModel').value = '';
  renderPicker();
};
$('#manualModel').onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); $('#addModelBtn').click(); } };
$('#discoverBtn').onclick = async () => {
  try {
    const p = readForm();
    $('#discoverBtn').disabled = true;
    $('#discoverStatus').textContent = 'Detecting…';
    const d = await api('/admin/api/provider-discover', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    if (!d.ok) throw new Error(d.error || 'No models discovered');
    editor.detected = [...new Set([...(d.models || []), ...editor.detected])];
    (d.models || []).forEach((m, i) => ensureModelMeta(m, i));
    if (editor.selected.size === 0) (d.models || []).forEach(m => editor.selected.add(m));
    renderPicker();
    $('#discoverStatus').textContent = `Found ${(d.models || []).length} model(s).`;
  } catch (e) {
    $('#discoverStatus').textContent = e.message;
    toast(e.message, true);
  } finally { $('#discoverBtn').disabled = false; }
};
$('#checkConnectionBtn').onclick = async () => {
  try {
    const p = readForm();
    $('#checkConnectionBtn').disabled = true;
    $('#connectionStatus').textContent = 'Checking…';
    const d = await api('/admin/api/provider-check', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    $('#connectionStatus').textContent = d.ok ? `OK — reachable (${d.status_code || 200})` : `Failed: ${d.error || 'unreachable'}`;
    $('#connectionStatus').className = 'inline-status ' + (d.ok ? '' : 'badtext');
  } catch (e) {
    $('#connectionStatus').textContent = e.message;
    $('#connectionStatus').className = 'inline-status badtext';
  } finally { $('#checkConnectionBtn').disabled = false; }
};
$('#testProviderBtn').onclick = async () => {
  try {
    const p = readForm();
    if (!editor.selected.size) throw new Error('Select at least one model');
    $('#testProviderBtn').disabled = true;
    $('#testResults').innerHTML = '<div class="inline-status">Testing…</div>';
    const d = await api('/admin/api/provider-test', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload(p)) });
    $('#testResults').innerHTML = (d.results || []).map(x => `<div class="test-row ${x.ok ? 'ok' : 'fail'}"><strong>${esc(x.model)}</strong><span>${x.ok ? 'PASS' : 'FAIL'}</span><span>${x.status_code || '—'}</span><span>${x.latency_ms} ms</span><small>${esc(x.error || '')}</small></div>`).join('');
    toast(d.ok ? 'All selected models passed' : `${d.passed}/${d.total} models passed`, !d.ok);
  } catch (e) {
    $('#testResults').innerHTML = `<div class="inline-status badtext">${esc(e.message)}</div>`;
    toast(e.message, true);
  } finally { $('#testProviderBtn').disabled = false; }
};
$('#saveProviderBtn').onclick = async () => {
  try {
    const p = readForm(), b = JSON.stringify(payload(p));
    $('#saveProviderBtn').disabled = true;
    if (editor.mode === 'add') await api('/admin/api/providers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: b });
    else await api('/admin/api/providers/' + encodeURIComponent(editor.originalId), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: b });
    toast('Provider saved and runtime reloaded');
    $('#pKey').value = '';
    $('#pCredentials').value = '';
    $('#pHeaders').value = '';
    $('#pProxy').value = '';
    editor = null;
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
  finally { $('#saveProviderBtn').disabled = false; }
};
$('#deleteProviderBtn').onclick = async () => {
  if (!(await showConfirm({title:'Delete provider?', message:`Delete provider "${editor.originalId}" and its configured deployments?`, accept:'Delete provider'}))) return;
  try {
    await api('/admin/api/providers/' + encodeURIComponent(editor.originalId), { method: 'DELETE' });
    toast('Provider deleted');
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
};

/* ---------- virtual endpoints / route profiles / pools ---------- */
function renderVirtual() {
  const ves = snap.virtual_endpoints || [];
  $('#virtualRows').innerHTML = ves.map(ve => `<tr><td><strong>${esc(ve.id)}</strong><br><small>${esc(ve.name||'')}</small></td><td><code>${esc(ve.public_model||ve.id)}</code></td><td>${esc(ve.route_profile||'')}</td><td>${ve.enabled!==false?'<span class="pill on">Enabled</span>':'<span class="pill">Disabled</span>'}</td><td>${esc(ve.pool_member_count ?? ve.configured_candidate_count ?? '—')}</td><td><button class="btn secondary" onclick="editVirtual('${esc(ve.id)}')">Edit</button> <button class="btn danger-ghost" onclick="deleteVirtual('${esc(ve.id)}')">Delete</button></td></tr>`).join('') || '<tr><td colspan="6" class="muted-cell">No virtual endpoints.</td></tr>';
}
function renderProfiles() {
  const globalStrat = snap.config?.routing?.strategy || 'ready_mesh';
  $('#profileRows').innerHTML = (snap.route_profiles||[]).map(rp => `<tr><td><strong>${esc(rp.id)}</strong></td><td>${esc(rp.name||'')}</td><td>${esc(rp.candidate_pool||'')}</td><td>${esc(rp.fallback_chain||'—')}</td><td>${esc(rp.strategy||`inherit (${globalStrat})`)}</td><td><button class="btn secondary" onclick="editProfile('${esc(rp.id)}')">Edit</button> <button class="btn danger-ghost" onclick="deleteProfile('${esc(rp.id)}')">Delete</button></td></tr>`).join('') || '<tr><td colspan="6" class="muted-cell">No route profiles.</td></tr>';
}
function renderPools() {
  $('#poolRows').innerHTML = (snap.candidate_pools||[]).map(cp => `<tr><td><strong>${esc(cp.id)}</strong></td><td>${esc(cp.mode||'explicit')}</td><td class="truncate-cell" title="${esc((cp.deployments||[]).join(', '))}">${esc((cp.deployments||[]).join(', ') || (cp.mode==='all'?'all eligible':'empty'))}</td><td>${esc(cp.expanded_count ?? cp.expanded?.length ?? '—')}</td><td><button class="btn secondary" onclick="editPool('${esc(cp.id)}')">Edit</button> <button class="btn danger-ghost" onclick="deletePool('${esc(cp.id)}')">Delete</button></td></tr>`).join('') || '<tr><td colspan="5" class="muted-cell">No candidate pools.</td></tr>';
  $('#chainRows').innerHTML = (snap.fallback_chains||[]).map(fc => `<tr><td><strong>${esc(fc.id)}</strong></td><td>${esc((fc.pools||[]).join(' → '))}</td><td><button class="btn secondary" onclick="editChain('${esc(fc.id)}')">Edit</button> <button class="btn danger-ghost" onclick="deleteChain('${esc(fc.id)}')">Delete</button></td></tr>`).join('') || '<tr><td colspan="3" class="muted-cell">No fallback chains.</td></tr>';
}
async function editVirtual(id, create=false) {
  const existing = (snap.virtual_endpoints||[]).find(v=>v.id===id);
  const d = await showFormDialog({title: existing?'Edit virtual endpoint':'New virtual endpoint', subtitle:'Stable public model name backed by a route profile.', fields:[
    {key:'id',label:'Internal ID',value:id||'',readonly:!!existing},
    {key:'public_model',label:'Public model',value:existing?.public_model||'coding'},
    {key:'route_profile',label:'Route profile',value:existing?.route_profile||'default'},
    {key:'name',label:'Display name',value:existing?.name||''},
    {key:'enabled',label:'Enabled',type:'checkbox',value:existing?.enabled!==false}
  ]}); if(!d) return;
  const body={id:d.id.trim(),public_model:d.public_model.trim(),route_profile:d.route_profile.trim(),name:d.name.trim()||undefined,enabled:d.enabled};
  try { await api(existing?'/admin/api/virtual-endpoints/'+encodeURIComponent(existing.id):'/admin/api/virtual-endpoints',{method:existing?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}); toast('Virtual endpoint saved'); await refresh(); } catch(e){toast(e.message,true);}
}
async function deleteVirtual(id){ if(!(await showConfirm({title:'Delete virtual endpoint?',message:`Clients using "${id}" will stop resolving through this endpoint.`,accept:'Delete'}))) return; try{await api('/admin/api/virtual-endpoints/'+encodeURIComponent(id),{method:'DELETE'});toast('Virtual endpoint deleted');await refresh();}catch(e){toast(e.message,true);} }
async function editProfile(id){
  const existing=(snap.route_profiles||[]).find(x=>x.id===id);
  const pools=(snap.candidate_pools||[]).map(x=>({value:x.id,label:x.name?x.name+' · '+x.id:x.id}));
  const chains=[{value:'',label:'No fallback chain'},...(snap.fallback_chains||[]).map(x=>({value:x.id,label:x.name?x.name+' · '+x.id:x.id}))];
  const d=await showFormDialog({title:existing?'Edit route profile':'New route profile',subtitle:'A profile points the router at a candidate pool and optional fallback chain.',fields:[
    {key:'id',label:'Internal ID',value:id||'',readonly:!!existing},{key:'name',label:'Display name',value:existing?.name||''},
    {key:'candidate_pool',label:'Candidate pool',type:'select',value:existing?.candidate_pool||pools[0]?.value||'',options:pools},
    {key:'fallback_chain',label:'Fallback chain',type:'select',value:existing?.fallback_chain||'',options:chains}
  ]}); if(!d)return;
  const body={id:d.id.trim(),name:d.name.trim()||undefined,candidate_pool:d.candidate_pool,fallback_chain:d.fallback_chain||undefined};
  try{await api(existing?'/admin/api/route-profiles/'+encodeURIComponent(existing.id):'/admin/api/route-profiles',{method:existing?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});toast('Route profile saved');await refresh();}catch(e){toast(e.message,true);}
}
async function deleteProfile(id){if(!(await showConfirm({title:'Delete route profile?',message:`Delete route profile "${id}"? Endpoints referencing it must be changed first.`,accept:'Delete'})))return;try{await api('/admin/api/route-profiles/'+encodeURIComponent(id),{method:'DELETE'});toast('Route profile deleted');await refresh();}catch(e){toast(e.message,true);}}
async function editPool(id){
  const existing=(snap.candidate_pools||[]).find(x=>x.id===id);
  const d=await showFormDialog({title:existing?'Edit candidate pool':'New candidate pool',subtitle:'Choose explicit deployment IDs or let the router consider all eligible deployments.',fields:[
    {key:'id',label:'Internal ID',value:id||'',readonly:!!existing},{key:'name',label:'Display name',value:existing?.name||''},
    {key:'mode',label:'Mode',type:'select',value:existing?.mode||'explicit',options:[{value:'explicit',label:'Explicit deployments'},{value:'all',label:'All eligible deployments'}]},
    {key:'deployments',label:'Deployments (comma separated)',type:'textarea',value:(existing?.deployments||[]).join(', '),help:'Example: openai/gpt-5, anthropic/claude-sonnet'}
  ]}); if(!d)return;
  const body={id:d.id.trim(),name:d.name.trim()||undefined,mode:d.mode,deployments:d.mode==='all'?[]:d.deployments.split(',').map(x=>x.trim()).filter(Boolean)};
  try{await api(existing?'/admin/api/candidate-pools/'+encodeURIComponent(existing.id):'/admin/api/candidate-pools',{method:existing?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});toast('Candidate pool saved');await refresh();}catch(e){toast(e.message,true);}
}
async function deletePool(id){if(!(await showConfirm({title:'Delete candidate pool?',message:`Delete pool "${id}"? Profiles using it must be changed first.`,accept:'Delete'})))return;try{await api('/admin/api/candidate-pools/'+encodeURIComponent(id),{method:'DELETE'});toast('Candidate pool deleted');await refresh();}catch(e){toast(e.message,true);}}
async function editChain(id){
  const existing=(snap.fallback_chains||[]).find(x=>x.id===id);
  const d=await showFormDialog({title:existing?'Edit fallback chain':'New fallback chain',subtitle:'Order candidate pools from first choice to last resort.',fields:[
    {key:'id',label:'Internal ID',value:id||'',readonly:!!existing},{key:'name',label:'Display name',value:existing?.name||''},
    {key:'pools',label:'Pools in order (comma separated)',type:'textarea',value:(existing?.pools||[]).join(', '),help:'At least one existing pool is required.'}
  ]}); if(!d)return;
  const pools=d.pools.split(',').map(x=>x.trim()).filter(Boolean); if(!pools.length){toast('At least one pool is required',true);return;}
  const body={id:d.id.trim(),name:d.name.trim()||undefined,pools};
  try{await api(existing?'/admin/api/fallback-chains/'+encodeURIComponent(existing.id):'/admin/api/fallback-chains',{method:existing?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});toast('Fallback chain saved');await refresh();}catch(e){toast(e.message,true);}
}
async function deleteChain(id){if(!(await showConfirm({title:'Delete fallback chain?',message:`Delete fallback chain "${id}"?`,accept:'Delete'})))return;try{await api('/admin/api/fallback-chains/'+encodeURIComponent(id),{method:'DELETE'});toast('Fallback chain deleted');await refresh();}catch(e){toast(e.message,true);}}

window.editVirtual=editVirtual;window.deleteVirtual=deleteVirtual;window.editProfile=editProfile;window.deleteProfile=deleteProfile;window.editPool=editPool;window.deletePool=deletePool;window.editChain=editChain;window.deleteChain=deleteChain;

$('#addVirtualBtn')?.addEventListener('click',()=>editVirtual(`endpoint-${(snap.virtual_endpoints||[]).length+1}`,true));
$('#addProfileBtn')?.addEventListener('click',()=>editProfile(`profile-${(snap.route_profiles||[]).length+1}`));
$('#addPoolBtn')?.addEventListener('click',()=>editPool(`pool-${(snap.candidate_pools||[]).length+1}`));
$('#addChainBtn')?.addEventListener('click',()=>editChain(`fallback-${(snap.fallback_chains||[]).length+1}`));

/* ---------- simple route composer ---------- */
let routeEditor={mode:'add',originalId:'',selected:new Set()};
function routeParts(ve){
  const rp=(snap.route_profiles||[]).find(x=>x.id===ve.route_profile);
  const pool=(snap.candidate_pools||[]).find(x=>x.id===rp?.candidate_pool);
  return {rp,pool};
}
function renderRouting(){
  const ves=snap.virtual_endpoints||[];
  $('#routeEmpty').hidden=!!ves.length;
  $('#routeCards').innerHTML=ves.map(ve=>{
    const {rp,pool}=routeParts(ve), members=pool?.mode==='all'?['All eligible deployments']:(pool?.deployments||[]);
    return `<article class="route-card"><div class="route-card-head"><div><span class="route-public">${esc(ve.public_model||ve.id)}</span><h3>${esc(ve.name||ve.id)}</h3></div><span class="pill ${ve.enabled!==false?'on':''}">${ve.enabled!==false?'Enabled':'Disabled'}</span></div><p>${members.length?members.slice(0,3).map(esc).join(' · '):'No model members'}${members.length>3?` · +${members.length-3} more`:''}</p><div class="route-card-meta"><span>${esc(snap.config?.routing?.strategy||'ready_mesh')}</span><span>${members.length} member${members.length===1?'':'s'}</span></div><div class="route-card-actions"><button class="btn secondary edit-simple-route" data-id="${esc(ve.id)}">Edit</button><button class="btn ghost copy-route" data-model="${esc(ve.public_model||ve.id)}">Copy model</button></div></article>`;
  }).join('');
  $$('.edit-simple-route').forEach(b=>b.onclick=()=>openRouteEditor(b.dataset.id));
  $$('.copy-route').forEach(b=>b.onclick=e=>copyText(b.dataset.model,e.currentTarget));
}
function setRouteModal(open){const m=$('#routeModal');m.classList.toggle('open',open);m.setAttribute('aria-hidden',open?'false':'true');document.body.classList.toggle('modal-open',open);}
function routeDeployments(){
  const q=($('#routeModelSearch')?.value||'').trim().toLowerCase();
  return (snap.deployments||[]).filter(d=>!q||d.model.toLowerCase().includes(q)||(d.provider_name||d.provider_id||'').toLowerCase().includes(q)||d.id.toLowerCase().includes(q));
}
function renderRouteModelPicker(){
  const h=healthMap(), rows=routeDeployments();
  $('#routeModelPicker').innerHTML=rows.map(d=>{const st=(h[d.id]||{}).status||'unknown';return `<label class="route-model-row"><input type="checkbox" data-route-dep="${esc(d.id)}" ${routeEditor.selected.has(d.id)?'checked':''}><span><strong>${esc(d.model)}</strong><small>${esc(d.provider_name||d.provider_id)} · ${esc(d.id)}</small></span><i class="status-dot ${esc(st)}"></i><em>${esc(st)}</em></label>`;}).join('')||'<div class="model-empty">No saved deployments match.</div>';
  $$('[data-route-dep]').forEach(x=>x.onchange=()=>{x.checked?routeEditor.selected.add(x.dataset.routeDep):routeEditor.selected.delete(x.dataset.routeDep);$('#routeSelectionCount').textContent=`${routeEditor.selected.size} selected`;});
  $('#routeSelectionCount').textContent=`${routeEditor.selected.size} selected`;
}
function uniqueRouteId(base){
  const stem=slug(base||'route'); let id=stem,n=2; const ids=new Set((snap.virtual_endpoints||[]).map(x=>x.id)); while(ids.has(id)){id=`${stem}-${n++}`;} return id;
}
function openRouteEditor(id=''){
  const existing=(snap.virtual_endpoints||[]).find(x=>x.id===id);
  const {pool}=existing?routeParts(existing):{};
  routeEditor={mode:existing?'edit':'add',originalId:existing?.id||'',selected:new Set(pool?.mode==='all'?(snap.deployments||[]).map(d=>d.id):(pool?.deployments||[]))};
  $('#routeFormTitle').textContent=existing?'Edit route':'Create route';
  $('#routeName').value=existing?.name||'Coding';
  $('#routePublicModel').value=existing?.public_model||'coding';
  $('#routeId').value=existing?.id||uniqueRouteId('coding');
  $('#routeId').readOnly=!!existing;
  $('#routeEnabled').value=String(existing?.enabled!==false);
  $('#routeMode').value='automatic';
  $('#deleteRouteBtn').classList.toggle('hidden',!existing);
  $('#routeFormError').textContent=''; $('#routeModelSearch').value='';
  renderRouteModelPicker(); setRouteModal(true);
}
async function saveSimpleRoute(){
  const name=$('#routeName').value.trim()||'Route', pub=$('#routePublicModel').value.trim(), id=$('#routeId').value.trim()||uniqueRouteId(pub);
  if(!pub){$('#routeFormError').textContent='Public model is required.';return;}
  if(!routeEditor.selected.size){$('#routeFormError').textContent='Select at least one model deployment.';return;}
  const poolId=`${id}-pool`, profileId=`${id}-profile`;
  const poolBody={id:poolId,name:`${name} models`,mode:'explicit',deployments:[...routeEditor.selected]};
  const profileBody={id:profileId,name:`${name} route`,candidate_pool:poolId};
  const endpointBody={id,name,public_model:pub,route_profile:profileId,enabled:$('#routeEnabled').value==='true'};
  $('#saveRouteBtn').disabled=true; $('#routeFormError').textContent='';
  try{
    const existing=(snap.virtual_endpoints||[]).find(x=>x.id===routeEditor.originalId);
    const old=existing?routeParts(existing):{};
    if(existing){
      const actualPoolId=old.pool?.id||poolId, actualProfileId=old.rp?.id||profileId;
      poolBody.id=actualPoolId; profileBody.id=actualProfileId; profileBody.candidate_pool=actualPoolId; endpointBody.route_profile=actualProfileId;
      if(old.pool) await api('/admin/api/candidate-pools/'+encodeURIComponent(actualPoolId),{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(poolBody)}); else await api('/admin/api/candidate-pools',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(poolBody)});
      if(old.rp) await api('/admin/api/route-profiles/'+encodeURIComponent(actualProfileId),{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(profileBody)}); else await api('/admin/api/route-profiles',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(profileBody)});
      await api('/admin/api/virtual-endpoints/'+encodeURIComponent(existing.id),{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(endpointBody)});
    }else{
      await api('/admin/api/candidate-pools',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(poolBody)});
      await api('/admin/api/route-profiles',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(profileBody)});
      await api('/admin/api/virtual-endpoints',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(endpointBody)});
    }
    toast(existing?'Route updated':'Route created'); setRouteModal(false); await refresh();
  }catch(e){$('#routeFormError').textContent=e.message;toast(e.message,true);}finally{$('#saveRouteBtn').disabled=false;}
}
async function deleteSimpleRoute(){
  const existing=(snap.virtual_endpoints||[]).find(x=>x.id===routeEditor.originalId); if(!existing)return;
  if(!(await showConfirm({title:'Delete route?',message:`Delete route "${existing.name||existing.public_model}" and its dedicated routing primitives?`,accept:'Delete route'})))return;
  const {rp,pool}=routeParts(existing);
  try{
    await api('/admin/api/virtual-endpoints/'+encodeURIComponent(existing.id),{method:'DELETE'});
    if(rp) await api('/admin/api/route-profiles/'+encodeURIComponent(rp.id),{method:'DELETE'});
    if(pool) await api('/admin/api/candidate-pools/'+encodeURIComponent(pool.id),{method:'DELETE'});
    setRouteModal(false);toast('Route deleted');await refresh();
  }catch(e){toast(e.message,true);}
}
$('#addRouteBtn')?.addEventListener('click',()=>openRouteEditor());
$('#addFirstRouteBtn')?.addEventListener('click',()=>openRouteEditor());
$('#closeRouteModal')?.addEventListener('click',()=>setRouteModal(false));
$('#cancelRouteBtn')?.addEventListener('click',()=>setRouteModal(false));
$$('[data-close-route]').forEach(x=>x.onclick=()=>setRouteModal(false));
$('#saveRouteBtn')?.addEventListener('click',saveSimpleRoute);
$('#deleteRouteBtn')?.addEventListener('click',deleteSimpleRoute);
$('#routeModelSearch')?.addEventListener('input',renderRouteModelPicker);
$('#routeSelectAll')?.addEventListener('click',()=>{routeDeployments().forEach(d=>routeEditor.selected.add(d.id));renderRouteModelPicker();});
$('#routeClearAll')?.addEventListener('click',()=>{routeEditor.selected.clear();renderRouteModelPicker();});
/* ---------- boot ---------- */
applyLocale();
applyTheme();
(async () => {
  try {
    const d = await api('/admin/api/provider-presets');
    serverPresets = d.presets || null;
  } catch { serverPresets = null; }
  fillPresetSelect();
  renderCLI();
  tick();
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
    renderCompat();
  } catch (e) {
    $('#compatRows').innerHTML = `<tr><td colspan="13">${esc(e.message)}</td></tr>`;
  }
}

function renderCompat() {
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
  $('#compatRows').innerHTML = rows.join('') || '<tr><td colspan="13">No deployments configured.</td></tr>';
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
  const buttons = [$('#compatTestFull'), $('#compatTestAgent')];
  buttons.forEach(b => b && (b.disabled = true));
  $('#compatReport').hidden = false;
  $('#compatReport').textContent = `Running ${mode} suite… (this can take a while; bounded per deployment)`;
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
    $('#compatReport').textContent = blocks.join('\n\n') || 'No models to test.';
    await loadCompat();
    toast(mode === 'full' ? 'Full capability suite finished' : 'Agent loop test finished');
  } catch (e) {
    $('#compatReport').textContent = 'Test failed: ' + e.message;
    toast(e.message, true);
  } finally {
    compatBusy = false;
    buttons.forEach(b => b && (b.disabled = false));
  }
}

$('#compatTestFull').onclick = () => runCompatSuite('full');
$('#compatTestAgent').onclick = () => runCompatSuite('claude_code');
$('#compatReset').onclick = async () => {
  try {
    await api('/admin/api/compat/reset', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ deployment: 'all' }) });
    toast('Capability cache reset; fresh probes will re-run');
    await loadCompat();
  } catch (e) { toast(e.message, true); }
};
