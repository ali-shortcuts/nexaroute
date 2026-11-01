'use strict';
// Exercise the actual embedded handlers. This is a DOM double, not browser acceptance.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapi/web/app.js', 'utf8');
function extract(start, end) {
  const a = source.indexOf('  ' + start), b = source.indexOf('\n  ' + end, a);
  assert.ok(a >= 0 && b > a, `${start} exists`);
  return source.slice(a, b);
}
const editor = { selected: 'n1', def: { id: 'test', nodes: [{ id: 'n1', config: {} }] }, dirty: false, validation: { valid: true } };
const state = { page: 'workflows', wf: { editor, defs: [JSON.parse(JSON.stringify(editor.def))] } };
const badge = { hidden: true }, validation = { innerHTML: '' }, content = { innerHTML: '' };
let messages = [], calls = [], release;
const context = {
  state, document: { getElementById: id => ({ 'wf-dirty': badge, 'wf-validation': validation })[id] },
  $: () => content, workflowsView: () => '', bindWorkflows: () => {}, wfEditorValidationHTML: () => 'stale',
  notice: text => messages.push(text), body: data => ({ body: JSON.stringify(data) }),
  api: (path, options) => { calls.push({ path, options }); return new Promise(resolve => { release = resolve; }); }
};
vm.createContext(context);
context.esc = v => String(v ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
context.num = v => v;
vm.runInContext(extract('function wfEditorConfigHTML(', 'function wfEditorSelectNode('), context);
vm.runInContext(extract('function wfEditorSetConfig(', 'function wfEditorValidationHTML('), context);
vm.runInContext(extract('async function wfEditorSave(', 'function wfEditorStartEdge('), context);
vm.runInContext(extract('async function wfEditorPublish(', 'function wfEditorRunDialog('), context);
(async () => {
  const cfg = editor.def.nodes[0].config;
  editor.def.nodes[0].node_id = 'node';
  state.wf.nodes = [{ id: 'node', config_schema: [{ name: 'count', type: 'number' }] }];
  cfg.count = '" onfocus="alert(1)';
  const html = context.wfEditorConfigHTML();
  assert.ok(html.includes('&quot; onfocus=&quot;alert(1)'), 'persisted numeric values are HTML escaped');
  delete cfg.count;

  context.wfEditorSetConfig('count', 'number', { value: '42' });
  assert.equal(cfg.count, 42); assert.equal(badge.hidden, false); assert.equal(validation.innerHTML, 'stale');
  assert.equal(content.innerHTML, '', 'field change does not rebuild the editor or steal focus');
  context.wfEditorSetConfig('name', 'string', { value: 'example' }); assert.equal(cfg.name, 'example');
  context.wfEditorSetConfig('enabled', 'boolean', { checked: true }); assert.equal(cfg.enabled, true);
  context.wfEditorSetConfig('enabled', 'boolean', { checked: false }); assert.equal(cfg.enabled, false);
  context.wfEditorSetConfig('options', 'object', { value: '{"x":1}' }); assert.equal(cfg.options.x, 1);
  context.wfEditorSetConfig('items', 'array', { value: '[1,2]' }); assert.equal(cfg.items.length, 2);
  const invalidObject = { value: '[]' }; context.wfEditorSetConfig('options', 'object', invalidObject);
  assert.equal(cfg.options.x, 1); assert.match(invalidObject.value, /"x"/);
  context.wfEditorSetConfig('items', 'array', { value: '' }); assert.equal('items' in cfg, false);
  state.wf.nodes[0].config_schema.push({ name: 'options', type: 'object' }, { name: 'items', type: 'array' });
  assert.match(context.wfEditorConfigHTML(), /<textarea[^>]+data-cfg-type="object"/);

  const bad = { value: 'NaN' }; context.wfEditorSetConfig('count', 'number', bad); assert.equal(cfg.count, 42); assert.equal(bad.value, 42);
  const badInput = { value: '', validity: { badInput: true } }; context.wfEditorSetConfig('count', 'number', badInput); assert.equal(cfg.count, 42);
  context.wfEditorSetConfig('count', 'number', { value: '' }); assert.equal('count' in cfg, false);
  await context.wfEditorPublish(); assert.equal(calls.length, 0, 'unsaved edits cannot publish');
  const saving = context.wfEditorSave(); assert.equal(calls.length, 1);
  await context.wfEditorPublish(); assert.equal(calls.length, 1, 'in-flight save cannot publish');
  context.wfEditorSetConfig('name', 'string', { value: 'newer' });
  release({}); await saving;
  assert.equal(editor.dirty, true, 'edits during save remain dirty');
  assert.equal(state.wf.defs[0].nodes[0].config.name, 'example', 'list tracks submitted snapshot');
  const second = context.wfEditorSave(); release({}); await second; assert.equal(editor.dirty, false);
  const publishing = context.wfEditorPublish(); assert.equal(calls.length, 3); release({ revision: 1, status: 'published' }); await publishing;
  console.log('workflow configuration and save regression PASS');
})().catch(e => { console.error(e); process.exitCode = 1; });
