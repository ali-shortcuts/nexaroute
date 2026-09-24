// Exercise saving an existing provider without dropping advanced settings.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const source = fs.readFileSync('internal/httpapi/web/app.js', 'utf8');
const form = source.slice(source.indexOf('function readForm()'), source.indexOf('\nfunction payload('));
const values = {pId:'p',pName:'P',pType:'gemini',pBase:'https://example.test',pAuth:'x-goog-api-key'};
const model = {id:'m',model:'gemini-test',context_window:65536,input_cost_per_mtok:2,output_cost_per_mtok:8,capabilities:{streaming:true}};
const context = {
 editor: {mode:'edit',provider:{responses_path:'/custom/responses',dialect:'gemini'},selected:new Set(['gemini-test'])},
 $: id => ({value:values[id.slice(1)] || '',checked:true}),
 ensureModelMeta: () => model,
 slug: x=>x
};
vm.createContext(context);
vm.runInContext(form+'\nresult=readForm();', context);
assert.equal(context.result.responses_path,'/custom/responses');
assert.equal(context.result.dialect,'gemini');
assert.equal(context.result.models[0].context_window,65536);
assert.equal(context.result.models[0].input_cost_per_mtok,2);
assert.equal(context.result.models[0].output_cost_per_mtok,8);
console.log('EDITOR PASS: advanced provider/model fields survive save');
