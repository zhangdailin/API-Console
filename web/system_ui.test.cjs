const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
function setup(payload) {
  const nodes=new Map();const events={};const timers=[];const requests=[];
  const node=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',disabled:true,hidden:false,classList:{toggle(){}},addEventListener(name,fn){this[name]=fn;},setAttribute(){},style:{}});return nodes.get(id);};
  const context=vm.createContext({AbortController,crypto:{randomUUID:()=> 'unique-update-id'},localStorage:{getItem:()=>null,setItem(){},removeItem(){}},confirm:()=>true,window:{location:{href:'/admin/'}},document:{getElementById:node,querySelectorAll:()=>[],addEventListener:(name,fn)=>events[name]=fn},setTimeout:fn=>{timers.push(fn);return timers.length;},clearTimeout(){},setInterval(){},fetch:async(url,options)=>{requests.push({url,options});return {ok:true,json:async()=>structuredClone(payload)}}});
  for(const file of ['ui.js','system.js'])vm.runInContext(fs.readFileSync(__dirname+'/static/js/'+file,'utf8'),context);
  events.DOMContentLoaded();return {node,context,requests,timers};
}
const current={version:'v1.0.2+local',commit:'abcdef0',build_type:'local',repository:'zhangdailin/API-Console'};
const wait=()=>new Promise(resolve=>setImmediate(resolve));
test('failed discovery is never displayed as latest and cannot install',async()=>{const h=setup({current,available:false,warning:'GitHub offline',can_update:true});await wait();assert.equal(h.node('systemLatest').textContent,'未能检查');assert.match(h.node('systemCheckState').textContent,/offline/);assert(h.node('systemUpdate').disabled);});
test('unknown deployment capability prevents online upgrades while exposing release notes as text',async()=>{const h=setup({current,available:true,has_update:true,can_update:false,reason:'Docker',release:{tag_name:'v1.0.3',body:'<script>bad()</script>'}});await wait();assert(h.node('systemUpdate').disabled);assert.equal(h.node('systemReleaseNotes').textContent,'<script>bad()</script>');assert.match(h.node('systemCapability').textContent,/Docker/);});
test('running operations prevent another upgrade or rollback',async()=>{const h=setup({current,available:true,has_update:true,can_update:true,can_rollback:true,release:{tag_name:'v1.0.3'},operation:{phase:'restarting',message:'等待验证'}});await wait();assert(h.node('systemUpdate').disabled);assert(h.node('systemRollback').disabled);assert(h.timers.length>1);assert.equal(h.node('systemProgress').textContent,'等待验证');});
test('upgrade submission sends a stable idempotency key and exact release tag',async()=>{const h=setup({current,available:true,has_update:true,can_update:true,release:{tag_name:'v1.0.3'}});await wait();h.context.fetch=async(url,options)=>{h.requests.push({url,options});return{ok:true,json:async()=>({operation:{id:'op1',phase:'queued'}})}};await h.node('systemUpdate').click();const sent=h.requests.at(-1);assert.equal(sent.url,'/api/system/update');assert.equal(sent.options.headers['Idempotency-Key'],'unique-update-id');assert.deepEqual(JSON.parse(sent.options.body),{version:'v1.0.3'});});

test('generated Full Changelog lines are omitted while real release notes remain',async()=>{const h=setup({current,available:true,release:{tag_name:'v1.0.3',body:'Fixed Grok requests\n\n**Full Changelog**: https://github.com/example/compare/a...b\n\n**Full Changelog**: https://github.com/example/compare/a...b'}});await wait();assert.equal(h.node('systemReleaseNotes').textContent,'Fixed Grok requests');assert(!h.node('systemReleaseNotes').hidden);});
test('a changelog-only release has no empty notes panel',async()=>{const h=setup({current,available:true,release:{tag_name:'v1.0.3',body:'**Full Changelog**: [url](https://github.com/example/compare/a...b)'}});await wait();assert.equal(h.node('systemReleaseNotes').textContent,'');assert(h.node('systemReleaseNotes').hidden);});

test('a superseded update check cannot overwrite a newer release or show its error', async () => {
  const h = setup({ current, available: true });
  await wait();
  const pending = [];
  h.context.fetch = (url, options) => new Promise((resolve, reject) => pending.push({ resolve, reject, options }));
  const first = h.node('systemCheck').click();
  const second = h.node('systemCheck').click();
  assert.equal(pending[0].options.signal.aborted, true);
  pending[1].resolve({ ok: true, json: async () => ({ current, available: true, release: { tag_name: 'v1.0.5' } }) });
  await second;
  pending[0].reject(new Error('stale request failure'));
  await first;
  assert.equal(h.node('systemLatest').textContent, 'v1.0.5');
  assert.doesNotMatch(h.node('systemCheckState').textContent, /stale/);
});

test('poll ticks cannot release a pending upgrade submission or start another action', async () => {
  const payload = { current, available: true, has_update: true, can_update: true, can_rollback: true, release: { tag_name: 'v1.0.3' } };
  const h = setup(payload); await wait();
  const calls = [];
  h.context.fetch = (url, options) => new Promise(resolve => calls.push({ url, options, resolve }));
  const first = h.node('systemUpdate').click();
  await h.timers.at(-1)();
  await h.node('systemUpdate').click();
  await h.node('systemRollback').click();
  await h.node('systemCheck').click();
  assert.equal(calls.length, 1);
  assert(h.node('systemUpdate').disabled);
  assert(h.node('systemRollback').disabled);
  calls[0].resolve({ ok: true, json: async () => ({ operation: { phase: 'queued', message: 'accepted' } }) });
  await first;
  const poll = h.timers.at(-1)();
  assert.equal(calls[1].url, '/api/system/operation');
  calls[1].resolve({ ok: true, json: async () => ({ operation: { phase: 'downloading', message: 'running' } }) });
  await poll;
  assert(h.node('systemUpdate').disabled);
  assert.equal(h.node('systemProgress').textContent, 'running');
});

test('a check started before submission cannot overwrite the accepted operation', async () => {
  const payload = { current, available: true, has_update: true, can_update: true, release: { tag_name: 'v1.0.3' } };
  const h = setup(payload); await wait();
  const calls = [];
  h.context.fetch = (url, options) => new Promise(resolve => calls.push({ url, options, resolve }));
  const oldCheck = h.node('systemCheck').click();
  const update = h.node('systemUpdate').click();
  assert.equal(calls[0].options.signal.aborted, true);
  calls[1].resolve({ ok: true, json: async () => ({ operation: { phase: 'queued' } }) });
  await update;
  calls[0].resolve({ ok: true, json: async () => payload });
  await oldCheck;
  await h.node('systemUpdate').click();
  assert.equal(calls.length, 2);
  assert(h.node('systemUpdate').disabled);
});

test('a poll superseded by a new submission cannot overwrite its operation', async () => {
  const payload = { current, available: true, has_update: true, can_update: true, release: { tag_name: 'v1.0.3' } };
  const h = setup(payload); await wait();
  h.context.fetch = async () => ({ ok: true, json: async () => ({ operation: { phase: 'queued' } }) });
  await h.node('systemUpdate').click();
  const calls = [];
  h.context.fetch = (url, options) => new Promise(resolve => calls.push({ url, options, resolve }));
  const oldPoll = h.timers.at(-1)();
  const check = h.node('systemCheck').click();
  calls[1].resolve({ ok: true, json: async () => payload }); await check;
  const update = h.node('systemUpdate').click();
  assert.equal(calls[0].options.signal.aborted, true);
  calls[2].resolve({ ok: true, json: async () => ({ operation: { phase: 'queued', message: 'new operation' } }) });
  await update;
  calls[0].resolve({ ok: true, json: async () => ({ operation: null }) }); await oldPoll;
  await h.node('systemUpdate').click();
  assert.equal(calls.length, 3);
  assert(h.node('systemUpdate').disabled);
});

test('failed submissions resume polling and permit retry after an idle confirmation', async () => {
  const h = setup({ current, available: true, has_update: true, can_update: true, release: { tag_name: 'v1.0.3' } });
  await wait();
  const calls = [];
  h.context.fetch = async (url, options) => {
    calls.push({ url, options });
    return url.endsWith('/operation') ? { ok: true, json: async () => ({ operation: null }) }
      : { ok: false, status: 503, text: async () => 'offline' };
  };
  await h.node('systemUpdate').click();
  assert.match(h.node('systemProgress').textContent, /offline/);
  await h.timers.at(-1)();
  assert.equal(h.node('systemUpdate').disabled, false);
  await h.node('systemUpdate').click();
  assert.equal(calls.filter(call => call.url.endsWith('/update')).length, 2);
});
