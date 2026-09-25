const fs = require('node:fs'), vm = require('node:vm'), assert = require('node:assert/strict');
const source = fs.readFileSync('internal/httpapi/web/app.js','utf8');
const block = source.slice(source.indexOf('let endpoint ='), source.indexOf('\nfunction renderCLI()'));
const nodes = new Map();
const node = id => { if(!nodes.has(id)) nodes.set(id,{value:'',textContent:''}); return nodes.get(id); };
let stored = {model:'nexaroute',api_key:'',deployments:2}, copied='';
const ctx = { $:node,location:{origin:'http://localhost:8080'},esc:x=>x,confirm:()=>true,toast:()=>{},refresh:async()=>{},copyText:x=>copied=x,renderCLI:()=>{},
 api:async(url,opt)=>{assert.equal(url,'/admin/api/endpoint');if(opt){assert.equal(opt.headers['Content-Type'],'application/json');const b=JSON.parse(opt.body);stored={...stored,model:b.model,api_key:b.rotate_key?'nx_rotated':'nx_created'};}return {...stored};}
};vm.createContext(ctx);vm.runInContext(block,ctx);
(async()=>{
 await vm.runInContext('loadEndpoint()',ctx);
 assert.equal(node('#endpointURL').value,'http://localhost:8080');
 node('#endpointModel').value='ali-coding'; await vm.runInContext('saveEndpoint(false)',ctx);
 assert.equal(node('#endpointKey').value,'nx_created');
 const snippet=vm.runInContext('cliSnippet("claude").body',ctx);
 for(const name of ['ANTHROPIC_MODEL','ANTHROPIC_DEFAULT_OPUS_MODEL','ANTHROPIC_DEFAULT_SONNET_MODEL','ANTHROPIC_DEFAULT_HAIKU_MODEL','CLAUDE_CODE_SUBAGENT_MODEL']) assert.ok(snippet.includes(`${name}='ali-coding'`));
 assert.ok(snippet.includes("ANTHROPIC_AUTH_TOKEN='nx_created'"));
 node('#endpointCopyKey').onclick({target:{}});assert.equal(copied,'nx_created');
 await vm.runInContext('saveEndpoint(true)',ctx);assert.equal(node('#endpointKey').value,'nx_rotated');
 console.log('ENDPOINT UI PASS: load, create, rotate, copy, unified Claude Code model settings');
})().catch(e=>{console.error(e);process.exitCode=1});
