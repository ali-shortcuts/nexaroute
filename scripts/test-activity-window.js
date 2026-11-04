'use strict';
// Exercise the production pure chart model without a browser fixture.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapi/web/app.js', 'utf8');
const start = source.indexOf('/* activity-chart-logic:start');
const end = source.indexOf('/* activity-chart-logic:end */');
assert.ok(start >= 0 && end > start, 'chart block exists');
const context = {};
vm.runInNewContext(source.slice(start, end) + '\nthis.model = activityChartModel;', context);
const now = Date.UTC(2026, 10, 2);
for (const [bucketMs, windowMs] of [[30000, 45000], [600000, 60000], [30000, 300000]]) {
  const model = context.model([], now, { bucketMs, windowMs });
  assert.ok(model.windowEnd - model.windowStart <= windowMs, `lookback exceeds requested window ${windowMs}`);
  assert.equal(model.windowEnd, now);
}
console.log('activity window bounds OK');
