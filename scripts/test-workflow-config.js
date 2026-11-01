'use strict';
// Exercise the embedded workflow editor handler with DOM-like inputs. Browsers
// reject setSelectionRange on number inputs; model that restriction here.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapi/web/app.js', 'utf8');
const handler = source.split('\n').find(line => line.trimStart().startsWith('function wfEditorSetConfig('));
assert.ok(handler, 'workflow configuration handler exists');
const state = { page: 'workflows', wf: { editor: { selected: 'n1', def: { nodes: [{ id: 'n1' }] } } } };
let focused = false;
let selection = false;
let currentType = 'number';
const input = { get type() { return currentType; }, value: '42', focus() { focused = true; }, setSelectionRange() { if (currentType === 'number') throw new Error('InvalidStateError'); selection = true; } };
const context = { state, document: { getElementById: () => input }, $: () => ({}), workflowsView: () => '', bindWorkflows: () => {} };
vm.createContext(context);
vm.runInContext(handler, context);
context.wfEditorSetConfig('count', 'number', { value: '42' });
assert.equal(state.wf.editor.def.nodes[0].config.count, 42);
assert.ok(focused);
assert.equal(selection, false);
currentType = 'text';
context.wfEditorSetConfig('name', 'string', { value: 'example' });
assert.equal(state.wf.editor.def.nodes[0].config.name, 'example');
assert.ok(selection);
console.log('workflow configuration handler regression PASS');
