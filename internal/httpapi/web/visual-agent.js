'use strict';
(() => {
  const MAX_EXECUTIONS = 64;
  const TERMINAL_SETTLE_MS = 4000;
  const state = { executions: new Map(), seen: new Set(), lastSeq: 0, snapshot: null, timer: 0, motion: 'on' };
  const terminalKinds = new Set(['route_ok', 'candidate_exhausted', 'route_timeout', 'client_disconnect', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail']);
  const failKinds = new Set(['route_fail', 'model_failed', 'stream_fail', 'stream_fail_precommit', 'response_decode_fail', 'route_timeout']);
  const failoverKinds = new Set(['failover', 'request_failover']);
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;' }[c]));
  const reduceMotion = () => state.motion === 'off' || state.motion === 'reduced' || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
  const now = () => Date.now();
  function reset(events) {
    state.executions.clear(); state.seen.clear(); state.lastSeq = 0;
    for (const event of events || []) apply(event, false);
    render();
  }
  function apply(event, animate = true) {
    if (!event || typeof event !== 'object') return;
    const seq = Number(event.seq || 0);
    if (seq && state.seen.has(seq)) return;
    if (seq) { state.seen.add(seq); state.lastSeq = Math.max(state.lastSeq, seq); if (state.seen.size > 256) state.seen.delete(state.seen.values().next().value); }
    const requestId = String(event.request_id || '').trim();
    if (!requestId) return;
    let execution = state.executions.get(requestId);
    if (!execution) {
      execution = { requestId, startedAt: event.time || new Date().toISOString(), status: 'active', deployment: '', publicModel: event.public_model || event.virtual_endpoint || '', latency: 0, attempts: [], terminal: false, updatedAt: now() };
      state.executions.set(requestId, execution);
      while (state.executions.size > MAX_EXECUTIONS) state.executions.delete(state.executions.keys().next().value);
    }
    if (event.public_model || event.virtual_endpoint) execution.publicModel = event.public_model || event.virtual_endpoint;
    if (event.deployment) {
      execution.deployment = event.deployment;
      const prior = execution.attempts[execution.attempts.length - 1];
      if (!prior || prior.deployment !== event.deployment) execution.attempts.push({ deployment: event.deployment, state: 'active', latency: 0 });
    }
    if (failoverKinds.has(event.kind)) execution.status = 'failover';
    else if (failKinds.has(event.kind)) execution.status = 'failed';
    else if (event.kind === 'route_ok') execution.status = 'success';
    else if (terminalKinds.has(event.kind)) execution.status = event.kind === 'client_disconnect' ? 'cancelled' : 'failed';
    if (event.latency_ms) execution.latency = Number(event.latency_ms) || 0;
    const attempt = execution.attempts[execution.attempts.length - 1];
    if (attempt && failKinds.has(event.kind)) { attempt.state = 'failed'; attempt.latency = execution.latency; }
    if (attempt && event.kind === 'route_ok') { attempt.state = 'success'; attempt.latency = execution.latency; }
    if (terminalKinds.has(event.kind) || event.kind === 'route_ok') {
      execution.terminal = true;
      if (animate) { clearTimeout(state.timer); state.timer = setTimeout(() => { state.executions.delete(requestId); render(); }, TERMINAL_SETTLE_MS); }
    }
    execution.updatedAt = now();
    if (animate) render();
  }
  function handleEvent(event) { apply(event, true); }
  function setSnapshot(snapshot) { state.snapshot = snapshot || null; }
  function nodeLabel(id) {
    const deployments = state.snapshot?.deployments || [];
    const d = deployments.find(x => x.id === id);
    if (!d) return id || 'Actual target';
    return d.model || d.id || id;
  }
  function render() {
    const topology = document.querySelector('.topology');
    if (!topology) return;
    const active = [...state.executions.values()].filter(x => !x.terminal).sort((a, b) => b.updatedAt - a.updatedAt)[0];
    const selected = active || [...state.executions.values()].sort((a, b) => b.updatedAt - a.updatedAt)[0];
    const status = selected?.status || 'idle';
    const statusText = selected ? `${selected.requestId} ${selected.publicModel || 'request'} ${selected.status}${selected.deployment ? ` at ${nodeLabel(selected.deployment)}` : ''}${selected.latency ? ` ${selected.latency} ms` : ''}` : 'Waiting for requests';
    const motionClass = reduceMotion() ? ' reduced' : '';
    const agent = selected && !selected.terminal ? `<span class="visual-agent${motionClass}" aria-hidden="true"><i></i><i></i><i></i><i></i><b></b></span>` : '';
    const target = selected?.deployment ? `<div class="topology-node target ${esc(status)}"><b>${esc(nodeLabel(selected.deployment))}</b><small>${esc(selected.deployment)}</small></div>` : `<div class="topology-targets"><div class="topology-empty"><b>No active execution</b><small>Real request activity will appear here.</small></div></div>`;
    topology.innerHTML = `<div class="topology-node client"><b>Client</b><small>Real request identity</small></div><div class="topology-line ${selected ? 'active' : ''}"><span></span></div><div class="topology-node router"><b>NexaRoute</b><small>${selected ? 'authoritative route execution' : 'waiting for requests'}</small>${agent}</div><div class="topology-line ${selected ? status : ''}"><span></span></div>${target}<div class="visual-summary"><strong>${esc(selected ? (status === 'idle' ? 'Waiting for requests' : status.toUpperCase()) : 'Waiting for requests')}</strong><span>${esc(statusText)}</span></div><div class="sr-only" role="status" aria-live="polite">${esc(statusText)}</div>`;
    topology.dataset.executionStatus = status;
  }
  window.NexaRouteVisualAgent = { reset, handleEvent, setSnapshot, render, setMotion(mode) { state.motion = ['on', 'reduced', 'off'].includes(mode) ? mode : 'on'; render(); } };
  window.addEventListener('pagehide', () => { clearTimeout(state.timer); state.executions.clear(); });
})();
