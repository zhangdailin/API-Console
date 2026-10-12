const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
function setup(request) {
 const nodes = new Map(), calls = []; let reloads = 0;
 const el = id => { if (!nodes.has(id)) nodes.set(id, {value:'',disabled:false,textContent:'',children:[],style:{},classList:{toggle(){}},setAttribute(){},click(){this.clicked=true;},replaceChildren(){this.children=[];},appendChild(node){this.children.push(node);} }); return nodes.get(id); };
 const context = vm.createContext({document:{getElementById:el,createElement:()=>({textContent:''})},window:{OrchidsProviderRegistry:{get:key=>['workbuddy','qoder','cline','grok'].includes(key)?{label:key}:null}},loadAccounts(){reloads++;}});
 vm.runInContext(fs.readFileSync(__dirname+'/static/js/ui.js','utf8'),context);
 context.ConsoleUI={...context.ConsoleUI,modal:(id,open)=>{el(id).open=open;},setText:(id,value)=>{el(id).textContent=value;}};
 context.ConsoleAPI={...context.ConsoleAPI,json:async (...args)=>{calls.push(args);return request?request(...args):{total:1,imported:1,skipped:0,duplicates:0,invalid:0,failed:0};}};
 vm.runInContext(fs.readFileSync(__dirname+'/static/js/account-transfer.js','utf8'),context);
 const read = async (value, size=100) => {const input=el('accountImportFile');input.files=[{size,text:async()=>typeof value==='string'?value:JSON.stringify(value)}];await context.readAccountImport(input);};
 return {context,el,calls,read,get reloads(){return reloads;}};
}
const backup={version:1,accounts:[{account_type:'workbuddy',workbuddy_refresh_token:'secret'}]};
test('backup is previewed locally; only confirmation sends JSON once',async()=>{
 let resolve;const t=setup(()=>new Promise(r=>{resolve=r;}));await t.read(backup);
 assert.equal(t.calls.length,0);assert.equal(t.el('accountImportConfirm').disabled,false);assert.doesNotMatch(t.el('accountImportSummary').textContent,/secret/);
 const pending=t.context.confirmAccountImport();await t.context.confirmAccountImport();assert.equal(t.calls.length,1);
 assert.equal(t.calls[0][0],'/api/import');assert.deepEqual(JSON.parse(t.calls[0][1].body),backup);
 t.context.closeAccountImport();assert.equal(t.el('accountImportModal').open,true);
 resolve({total:1,imported:1,skipped:0,duplicates:0,invalid:0,failed:0});await pending;assert.equal(t.reloads,1);
 assert.match(t.el('accountImportSummary').textContent,/导入 1/);t.context.closeAccountImport();assert.equal(t.el('accountImportModal').open,false);
 await t.context.confirmAccountImport();assert.equal(t.calls.length,1);
});
test('invalid or oversized input never reaches server; parser errors never show secret input',async()=>{
 for(const [value,size] of [['secret invalid',100],[{version:2,accounts:[]},100],[{version:1,accounts:[]},100],[{version:1,accounts:[null]},100],[backup,9*1024*1024]]){
  const t=setup();await t.read(value,size);await t.context.confirmAccountImport();assert.equal(t.calls.length,0);assert.equal(t.el('accountImportConfirm').disabled,true);assert.doesNotMatch(t.el('accountImportSummary').textContent,/secret/);
 }
});
test('partial result distinguishes duplicate invalid and failures without reporting full success',async()=>{
 const t=setup(async()=>({total:4,imported:1,skipped:3,duplicates:1,invalid:1,failed:1,issues:[{index:2,reason:'duplicate_account'},{index:3,reason:'missing_refresh_token'},{index:4,reason:'storage_error'}]}));await t.read(backup);await t.context.confirmAccountImport();
 assert.match(t.el('accountImportSummary').textContent,/重复 1，无效 1，写入失败 1/);assert.equal(t.el('accountImportResult').children.length,3);assert.equal(t.reloads,1);
});
test('lost response clears secret buffer and warns about possible partial writes',async()=>{
 const t=setup(async()=>{throw new Error('secret response');});await t.read(backup);await t.context.confirmAccountImport();assert.match(t.el('accountImportSummary').textContent,/可能已有部分/);assert.doesNotMatch(t.el('accountImportSummary').textContent,/secret/);await t.context.confirmAccountImport();assert.equal(t.calls.length,1);assert.equal(t.el('accountImportOpen').disabled,false);
});
test('closing while file reads prevents stale preview restoring credentials',async()=>{
 let resolve;const t=setup();const input=t.el('accountImportFile');input.files=[{size:100,text:()=>new Promise(r=>{resolve=r;})}];const pending=t.context.readAccountImport(input);t.context.closeAccountImport();resolve(JSON.stringify(backup));await pending;await t.context.confirmAccountImport();assert.equal(t.calls.length,0);assert.equal(t.el('accountImportModal').open,false);
});
