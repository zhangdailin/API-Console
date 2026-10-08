const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

function setup(fetcher) {
 const nodes=new Map(), notices=[];
 const node=id=>{if(!nodes.has(id))nodes.set(id,{value:'',disabled:false,textContent:'',style:{},classList:{toggle(){}},setAttribute(){},replaceChildren(){}});return nodes.get(id);};
 const context={document:{getElementById:node,addEventListener(){},querySelectorAll:()=>[]},window:{location:{},addEventListener(){}},URL,Date,Set,console,
 fetch:fetcher,showToast:(...args)=>notices.push(args),copyToClipboard:async value=>{context.copied=value;}};
 vm.createContext(context);
 for(const file of ['ui.js','config.js','api-keys.js']) vm.runInContext(fs.readFileSync(__dirname+'/static/js/'+file,'utf8'),context);
 vm.runInContext('renderApiKeys=()=>{};globalThis.setKeys=value=>apiKeys=value;globalThis.probe={keyPolicy,keyStatus,filteredKeys,bindApiKeyActions};',context);
 return {context,node,notices};
}
const json=value=>({ok:true,headers:{get:()=> 'application/json'},json:async()=>value});

test('delegated actions use key identity and current status for table and cards',()=>{
 const t=setup();t.context.setKeys([{id:1,name:'client',enabled:true}]);const calls=[];
 for(const name of ['copyApiKey','openKeyImport','openEditKeyModal','rotateApiKey','toggleKeyStatus','openDeleteKeyModal'])t.context[name]=(...args)=>calls.push([name,...args]);
 const container={contains:()=>true};t.context.probe.bindApiKeyActions(container);
 for(const action of ['copy-key','import-key','edit-key','rotate-key','toggle-key','delete-key'])container.onclick({target:{closest:()=>({dataset:{action,id:'1'}})}});
 assert.deepEqual(calls,[['copyApiKey','1'],['openKeyImport','1'],['openEditKeyModal','1'],['rotateApiKey','1'],['toggleKeyStatus','1',false],['openDeleteKeyModal','1','client']]);
});
test('name/suffix filters and expiry status are deterministic',()=>{
 const t=setup();t.context.setKeys([{id:1,name:'production',key_suffix:'ab12',enabled:true},{id:2,name:'test',enabled:false},{id:3,name:'expired',enabled:true,expires_at:'2000-01-01T00:00:00Z'}]);
 t.node('keySearch').value='AB12';assert.equal(t.context.probe.filteredKeys()[0].id,1);
 t.node('keySearch').value='';t.node('keyStatusFilter').value='expired';assert.equal(t.context.probe.filteredKeys()[0].id,3);
});
test('create/edit submit access policy only and retain failed edits',async()=>{
 const calls=[];let fail=false;
 const t=setup(async(url,options)=>{calls.push([url,options]);if(fail)return {ok:false,status:503,text:async()=> 'temporarily unavailable'};return json({key:'sk-new-secret'});});
 t.context.loadApiKeys=async()=>{};t.context.renderCreatedKeys=()=>{};
 t.node('keyName').value='first\nsecond';t.node('keyAllowedModels').value='model-a, model-b';t.node('keyRPMLimit').value='15';t.node('keyMaxConcurrent').value='3';
 await t.context.createApiKey({preventDefault(){}});
 assert.equal(calls.length,2);
 for(const [,options] of calls){const body=JSON.parse(options.body);assert.deepEqual(Object.keys(body).sort(),['name','allowed_models','rpm_limit','max_concurrent','expires_at'].sort());assert.equal(body.max_concurrent,3);}
 t.node('editKeyId').value='1';t.node('editKeyAllowedModels').value='model-a';t.node('editKeyRPMLimit').value='5';t.node('editKeyMaxConcurrent').value='4';
 await t.context.saveKeyPolicy({preventDefault(){}});
 assert.doesNotMatch(calls.at(-1)[1].body,/billing|budget/);
 fail=true;await t.context.saveKeyPolicy({preventDefault(){}});
 assert.equal(t.node('editKeyAllowedModels').value,'model-a');assert.equal(t.node('editKeySubmit').disabled,false);assert.match(t.node('editKeyError').textContent,/unavailable/);
 t.node('editKeyMaxConcurrent').value='1025';const count=calls.length;await t.context.saveKeyPolicy({preventDefault(){}});assert.equal(calls.length,count);
});
test('copy fetches secret on demand without caching; show dialog clears secrets',async()=>{
 const calls=[];const t=setup(async(url,options)=>{calls.push([url,options]);return json({key:'sk-full-secret'});});
 await t.context.copyApiKey(7);assert.equal(t.context.copied,'sk-full-secret');assert.equal(calls[0][0],'/api/keys/7/secret');assert.equal(calls[0][1].cache,'no-store');assert.equal(calls[0][1].credentials,'same-origin');
 vm.runInContext('createdKeys=[{key:"sk-full-secret"}];',t.context);t.context.closeShowKeyModal();assert.equal(vm.runInContext('createdKeys.length',t.context),0);
});
