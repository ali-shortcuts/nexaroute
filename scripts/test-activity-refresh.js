'use strict';
// Exercise the actual refresh implementation and its render-time retry guard.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.env.ACTIVITY_APP_SOURCE || 'internal/httpapi/web/app.js', 'utf8');
const start = source.indexOf('  function trendState()');
const end = source.indexOf('  function ensureTrendPolling()', start);
assert(start >= 0 && end > start, 'trend refresh source not found');
assert(source.includes('if (!trendState().loading && Date.now() - Number(trendState().at || 0) > 5000) refreshTrend();'), 'render retry guard changed');
let calls = 0, renders = 0, clock = 100000;
const state = { page: 'overview' };
const context = { state, Date: { now: () => clock }, api: async () => { calls++; throw new Error('403 forbidden'); } };
context.render = () => {
  renders++;
  if (renders > 2) return;
  const tr = state.trend;
  if (!tr.loading && clock - tr.at > 5000) context.refreshTrend();
};
vm.createContext(context);
vm.runInContext(source.slice(start, end), context);
(async () => {
  await context.refreshTrend();
  assert.equal(calls, 1, 'failure must not immediately re-fetch on render');
  assert.equal(renders, 1);
  assert.equal(state.trend.forbidden, true);
  await context.refreshTrend(true);
  assert.equal(calls, 2, 'explicit retry must remain available');
  assert.equal(renders, 2);
  console.log('activity refresh error/retry regression PASS');
})().catch(e => { console.error(e); process.exitCode = 1; });
