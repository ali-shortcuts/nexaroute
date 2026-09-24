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
    const k = prompt('Admin API key required');
    if (k !== null) {
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

/* ---------- navigation ---------- */
const subtitles = {
  overview: 'Ready Mesh: verified health, session affinity, capacity-aware routing and supervised recovery.',
  console: 'Every routing decision, probe, failover and recovery — as it happens.',
  providers: 'Upstream pools, credentials, endpoints and per-provider capacity.',
  models: 'Per-deployment routing state: health, latency and failure tracking.',
  health: 'Live provider pressure and capability-scoped circuit evidence.',
  compat: 'Per-model capability contracts and Claude Code readiness — separate from health.',
  cli: 'One-click connection snippets for coding agents and OpenAI-compatible tools.',
  settings: 'Hot-reloaded routing and probe configuration.'
};
$$('nav button').forEach(b => b.onclick = () => {
  $$('nav button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  $$('.tab').forEach(x => x.classList.remove('active'));
  $('#' + b.dataset.tab).classList.add('active');
  $('#title').textContent = b.dataset.title;
  $('#subtitle').textContent = subtitles[b.dataset.tab] || '';
  if (b.dataset.tab === 'settings') fillRuntimeSettings();
  if (b.dataset.tab === 'console') { consoleUnread = 0; $('#consoleDot').hidden = true; renderConsole(); }
  if (b.dataset.tab === 'cli') renderCLI();
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
  const ok = Number(c.healthy || 0), cd = Number(c.cooldown || 0);
  const total = snap.deployment_total ?? ds.length;
  $('#sHealthy').textContent = fmtInt(ok);
  $('#sCooldown').textContent = fmtInt(cd);
  $('#sHealthBar').style.width = total ? Math.round(ok / total * 100) + '%' : '0%';
  $('#sCooldownSub').textContent = cd ? 'recovering in background' : 'none recovering';
  $('#sSessions').textContent = fmtInt(snap.session_count ?? 0);
  const ls = (snap.events || []).filter(e => e.kind === 'route_ok' && e.latency_ms).map(e => e.latency_ms);
  const avg = ls.length ? Math.round(ls.reduce((a, b) => a + b, 0) / ls.length) : 0;
  $('#sLatency').textContent = avg ? avg + ' ms' : '—';
  renderRing(ds, h);
  renderDonut(c, total);
  renderProviders(h);
  renderModels(ds, h);
  renderHealthTab(h);
  renderCompat();
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
      stroke: st === 'healthy' ? 'rgba(63,224,140,.4)' : st === 'cooldown' ? 'rgba(255,110,125,.4)' : st === 'degraded' ? 'rgba(255,200,97,.4)' : st === 'half_open' ? 'rgba(201,184,255,.4)' : 'rgba(70,98,138,.3)',
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
    ['donutCooldown', 'cooldown', counts.cooldown || 0, 'var(--bad)'],
    ['donutCooldown', 'half_open', counts.half_open || 0, '#c9b8ff']
  ];
  const r = 48, circ = 2 * Math.PI * r;
  let offset = 0;
  const legend = [];
  for (const [id, label, count, color] of segs) {
    const el = $('#' + id);
    const frac = total ? count / total : 0;
    const len = frac * circ;
    if (id === 'donutCooldown' && label === 'half_open') {
      // reuse the cooldown circle element sequentially for half-open
      el.setAttribute('stroke-dasharray', `${len} ${circ - len}`);
      el.setAttribute('stroke-dashoffset', -offset);
      el.setAttribute('stroke', color);
    } else {
      el.setAttribute('stroke-dasharray', `${len} ${circ - len}`);
      el.setAttribute('stroke-dashoffset', -offset);
    }
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
  const incidents = Object.fromEntries((snap.provider_health || []).map(x => [x.provider, x]));
  const stats = Object.fromEntries((snap.provider_stats || []).map(x => [x.id, x]));
  $('#providerGrid').innerHTML = providerSummaries.map(p => {
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

/* ---------- compat ---------- */
function compatBadge(v) {
  const cls = v === 'PASS' || v === 'healthy' ? 'healthy' : v === 'FAIL' ? 'cooldown' : 'unknown';
  return `<span class="status ${cls}">${esc(v || 'UNKNOWN')}</span>`;
}
function renderCompat() {
  const cards = snap.compat?.scorecards || [];
  $('#compatRows').innerHTML = cards.map(c => {
    const stCls = c.status === 'CLAUDE_CODE_READY' ? 'healthy' : c.status === 'CHAT_READY' ? 'degraded' : 'unknown';
    return `<tr>
      <td>${esc(c.deployment)}</td>
      <td>${compatBadge(c.availability)}</td>
      <td>${compatBadge(c.basic_chat)}</td>
      <td>${compatBadge(c.streaming)}</td>
      <td>${compatBadge(c.tools)}</td>
      <td>${compatBadge(c.reasoning)}</td>
      <td><span class="status ${stCls}">${esc(c.status)}</span></td>
      <td style="color:var(--muted)">${esc(c.failure_reason || '—')}</td>
    </tr>`;
  }).join('') || '<tr><td colspan="8" style="color:var(--muted)">No capability contracts yet — route traffic or run a probe.</td></tr>';
  const sel = $('#compatDeploy');
  const cur = sel.value;
  const ids = (snap.deployments || []).map(d => d.id);
  sel.innerHTML = ids.map(id => `<option value="${esc(id)}">${esc(id)}</option>`).join('') || '<option value="">no deployments</option>';
  if (ids.includes(cur)) sel.value = cur;
}
async function runCompatProbe(mode) {
  const dep = $('#compatDeploy').value;
  if (!dep) { $('#compatProbeOut').textContent = 'No deployment selected.'; return; }
  $('#compatProbeOut').textContent = mode + ' probe running against ' + dep + '…';
  $('#compatProbeJson').textContent = '';
  try {
    const d = await api('/admin/api/compat/probe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ deployment: dep, mode }) });
    $('#compatProbeOut').textContent = mode + ' probe finished for ' + dep + '.';
    $('#compatProbeJson').textContent = JSON.stringify(d, null, 2);
    refresh();
  } catch (e) {
    $('#compatProbeOut').textContent = 'Probe failed: ' + (e.message || e);
  }
}
$('#compatQuickBtn').onclick = () => runCompatProbe('quick');
$('#compatFullBtn').onclick = () => runCompatProbe('full');
$('#compatAgentBtn').onclick = () => runCompatProbe('agent');

/* ---------- console ---------- */
const consoleKinds = {
  routes: new Set(['route_ok', 'route_attempt', 'route_fail', 'route_skip', 'route_timeout', 'failover', 'client_disconnect', 'response_decode_fail', 'stream_fail', 'gateway_overloaded', 'compat_repair_ok', 'compat_repair_fail', 'compat_repair_error']),
  errors: new Set(['route_fail', 'route_timeout', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail', 'gateway_overloaded', 'internal_panic', 'client_disconnect', 'probe_fail', 'probe_quarantine', 'recovery_fail', 'recovery_queue_full', 'compat_repair_error']),
  probes: new Set(['probe_ready', 'probe_fail', 'probe_quarantine', 'recovery_ready', 'recovery_fail', 'recovery_wait', 'recovery_deferred', 'recovery_cooldown', 'recovery_queue_full', 'stream_fail_precommit', 'compat_repair_ok', 'compat_repair_fail'])
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
  if (kind === 'claude') return {
    title: 'Claude Code / Anthropic clients',
    note: 'The placeholder key exists only for clients that require a non-empty value. Aliases such as auto/coding map many deployments behind one client model.',
    body:
`<span class="c"># Anthropic-compatible ingress</span>
export ANTHROPIC_BASE_URL=${base}
export ANTHROPIC_AUTH_TOKEN=local-placeholder
export ANTHROPIC_MODEL=coding

<span class="c"># or an explicit model / deployment</span>
export ANTHROPIC_MODEL=auto`
  };
  if (kind === 'openai') return {
    title: 'OpenAI-compatible tools',
    note: 'Chat Completions requests flow through the same routing plane and the same bulletproof protocol translation.',
    body:
`<span class="c"># OpenAI Chat Completions ingress</span>
export OPENAI_BASE_URL=${base}/v1
export OPENAI_API_KEY=local-placeholder

<span class="c"># direct curl</span>
curl ${base}/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}'`
  };
  if (kind === 'env') return {
    title: 'Session environment block',
    note: 'Drop this into .zshrc / .bashrc for the current machine.',
    body:
`<span class="c"># NexaRoute client environment</span>
export ANTHROPIC_BASE_URL=${base}
export ANTHROPIC_AUTH_TOKEN=local-placeholder
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
curl -s ${base}/v1/models`
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
    id: '', name: '', type: 'openai_compatible', base_url: '', api_key: '', api_key_env: '', credentials: [],
    auth_mode: 'bearer', headers: {}, forward_headers: null, proxy_url: '',
    chat_path: '/v1/chat/completions', messages_path: '/v1/messages', models_path: '/v1/models',
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
  chat2api: { name: 'Chat2API', id: 'chat2api', type: 'openai_compatible', base: 'http://127.0.0.1:5000/v1', auth: 'bearer' },
  anthropic: { name: 'Anthropic', id: 'anthropic', type: 'anthropic_compatible', base: 'https://api.anthropic.com', auth: 'x-api-key' },
  gemini: { name: 'Google Gemini', id: 'gemini', type: 'gemini', base: 'https://generativelanguage.googleapis.com', auth: 'x-goog-api-key' },
  openai: { name: 'OpenAI', id: 'openai', type: 'openai_compatible', base: 'https://api.openai.com/v1', auth: 'bearer' },
  openrouter: { name: 'OpenRouter', id: 'openrouter', type: 'openai_compatible', base: 'https://openrouter.ai/api/v1', auth: 'bearer' },
  deepseek: { name: 'DeepSeek', id: 'deepseek', type: 'openai_compatible', base: 'https://api.deepseek.com', auth: 'bearer' },
  groq: { name: 'Groq', id: 'groq', type: 'openai_compatible', base: 'https://api.groq.com/openai/v1', auth: 'bearer' },
  together: { name: 'Together AI', id: 'together', type: 'openai_compatible', base: 'https://api.together.xyz/v1', auth: 'bearer' },
  mistral: { name: 'Mistral', id: 'mistral', type: 'openai_compatible', base: 'https://api.mistral.ai/v1', auth: 'bearer' },
  xai: { name: 'xAI', id: 'xai', type: 'openai_compatible', base: 'https://api.x.ai/v1', auth: 'bearer' },
  ollama: { name: 'Ollama', id: 'ollama', type: 'openai_compatible', base: 'http://127.0.0.1:11434/v1', auth: 'none' }
};
function fillPresetSelect() {
  const sel = $('#pPreset');
  const current = sel.value;
  const entries = [];
  const seen = new Set();
  if (Array.isArray(serverPresets)) {
    // The admin API serializes presets as {id, name, category, type, base_url,
    // auth_mode, ...}; older builds may still send {key, label}, so accept both.
    for (const p of serverPresets) {
      const key = p && (p.id || p.key);
      if (!key || seen.has(key)) continue;
      seen.add(key);
      entries.push(`<option value="${esc(key)}">${esc(p.name || p.label || key)}</option>`);
    }
  }
  for (const k of Object.keys(providerPresets)) {
    if (k === 'custom' || seen.has(k)) continue;
    entries.push(`<option value="${esc(k)}">${esc(providerPresets[k].name)}</option>`);
  }
  sel.innerHTML = '<option value="custom">Custom Provider</option>' + entries.join('');
  sel.value = current || 'custom';
}
function applyPreset(k) {
  const p = serverPresets && Array.isArray(serverPresets) ? serverPresets.find(x => x && (x.id === k || x.key === k)) : null;
  if (p) {
    const name = p.name || p.label || p.id || p.key;
    if (editor.mode === 'add') {
      if (!$('#pName').value.trim()) $('#pName').value = name;
      if (!$('#pId').value.trim()) $('#pId').value = p.id || p.key;
    }
    $('#pType').value = p.type || 'openai_compatible';
    $('#pBase').value = p.base_url || p.base || '';
    $('#pAuth').value = p.auth_mode || p.auth || 'bearer';
    $('#pChatPath').value = p.chat_path || '/v1/chat/completions';
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
  $('#pMessagesPath').value = '/v1/messages';
  $('#pModelsPath').value = '/v1/models';
  $('#pCountPath').value = '/v1/messages/count_tokens';
}
$('#addProviderBtn').onclick = () => {
  editor = { mode: 'add', originalId: '', provider: emptyProvider(), detected: [], selected: new Set(), modelMeta: new Map(), secretDirty: true, secretSource: 'none' };
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
$('#pPreset').onchange = () => applyPreset($('#pPreset').value);
$('#pType').onchange = () => {
  const a = $('#pAuth');
  if ($('#pType').value === 'anthropic_compatible' && (a.value === 'bearer' || a.value === 'x-goog-api-key')) a.value = 'x-api-key';
  if ($('#pType').value === 'openai_compatible' && (a.value === 'x-api-key' || a.value === 'x-goog-api-key')) a.value = 'bearer';
  if ($('#pType').value === 'gemini' && (a.value === 'bearer' || a.value === 'x-api-key')) a.value = 'x-goog-api-key';
};
async function openEdit(id) {
  try {
    const d = await api('/admin/api/providers/' + encodeURIComponent(id) + '?reveal=1'), p = d.provider;
    // When the secret comes from an environment variable the resolved
    // literal must stay out of the form: any keystroke in the field would
    // flip preserve_secret off and persist the env secret into config.json.
    p.api_key = d.secret_source === 'env' ? '' : (d.resolved_api_key || p.api_key || '');
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
  $('#pProtocol').value = p.protocol || '';
  $('#pDialect').value = p.dialect || '';
  $('#pBase').value = p.base_url || '';
  // An empty auth_mode means "Auto (by provider type)", which ApplyDefaults
  // resolves server-side; preserve it instead of silently rewriting it to
  // bearer when an auto-configured provider is edited.
  $('#pAuth').value = p.auth_mode || '';
  $('#pEnabled').checked = p.enabled !== false;
  $('#pKey').value = p.api_key || '';
  $('#pKey').placeholder = editor.mode === 'edit' && editor.secretSource === 'env' ? 'stored in env var - leave blank to keep' : '';
  $('#pKey').type = 'password';
  $('#togglePKey').textContent = 'Show';
  $('#pKeyEnv').value = p.api_key_env || '';
  $('#pHeaders').value = Object.keys(p.headers || {}).length ? JSON.stringify(p.headers, null, 2) : '';
  $('#pProxy').value = p.proxy_url || '';
  $('#pConcurrency').value = p.max_concurrency || 32;
  $('#pStreamIdle').value = p.stream_idle_timeout_seconds || 180;
  $('#pChatPath').value = p.chat_path || '/v1/chat/completions';
  $('#pMessagesPath').value = p.messages_path || '/v1/messages';
  $('#pModelsPath').value = p.models_path || '/v1/models';
  $('#pCountPath').value = p.count_tokens_path || '/v1/messages/count_tokens';
  $('#pForwardHeaders').value = (p.forward_headers || []).join(', ');
  $('#pCredentials').value = (p.credentials || []).length ? JSON.stringify(p.credentials, null, 2) : '';
  const first = (p.models || [])[0] || {};
  const caps = first.capabilities || { streaming: true, tools: true, vision: false, reasoning: false };
  $('#pAliases').value = '';
  $('#pCapStreaming').checked = caps.streaming !== false;
  $('#pCapTools').checked = caps.tools !== false;
  $('#pCapVision').checked = !!caps.vision;
  $('#pCapReasoning').checked = !!caps.reasoning;
  $('#secretSource').textContent = editor.mode === 'edit' ? `saved source: ${editor.secretSource}` : '';
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
      capabilities: { streaming: c.streaming !== false, tools: c.tools !== false, vision: !!c.vision, reasoning: !!c.reasoning }
    };
  });
  const p = {
    id: $('#pId').value.trim(), name: $('#pName').value.trim(), type: $('#pType').value,
    protocol: $('#pProtocol').value, dialect: $('#pDialect').value,
    base_url: $('#pBase').value.trim(), api_key: $('#pKey').value, api_key_env: $('#pKeyEnv').value.trim(),
    credentials: creds, auth_mode: $('#pAuth').value, headers: hs,
    forward_headers: (() => {
      const v = $('#pForwardHeaders').value.split(',').map(x => x.trim()).filter(Boolean);
      return v.length ? v : (editor.mode === 'add' ? null : []);
    })(),
    proxy_url: $('#pProxy').value.trim(),
    chat_path: $('#pChatPath').value.trim(), messages_path: $('#pMessagesPath').value.trim(),
    models_path: $('#pModelsPath').value.trim(), count_tokens_path: $('#pCountPath').value.trim(),
    max_concurrency: Math.max(1, parseInt($('#pConcurrency').value || '32', 10)),
    stream_idle_timeout_seconds: Math.max(10, parseInt($('#pStreamIdle').value || '180', 10)),
    enabled: $('#pEnabled').checked, models
  };
  if (!p.id) throw new Error('Internal ID is required');
  if (!p.name) p.name = p.id;
  if (!p.base_url) throw new Error('Base URL is required');
  return p;
}
function payload(p) {
  return { provider: p, preserve_secret: editor.mode === 'edit' && !editor.secretDirty, test_models: [...editor.selected] };
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
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
  finally { $('#saveProviderBtn').disabled = false; }
};
$('#deleteProviderBtn').onclick = async () => {
  if (!confirm(`Delete provider "${editor.originalId}"?`)) return;
  try {
    await api('/admin/api/providers/' + encodeURIComponent(editor.originalId), { method: 'DELETE' });
    toast('Provider deleted');
    modal(false);
    await refresh();
  } catch (e) { toast(e.message, true); }
};

/* ---------- boot ---------- */
(async () => {
  try {
    const d = await api('/admin/api/provider-presets');
    serverPresets = d.presets || null;
  } catch { serverPresets = null; }
  fillPresetSelect();
  renderCLI();
  tick();
})();
