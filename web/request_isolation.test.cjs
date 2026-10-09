const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('./test-support.cjs');
const asset = name => fs.readFileSync(__dirname + '/static/js/' + name, 'utf8');
const deferred = () => { let resolve, reject; const promise = new Promise((a,b) => { resolve=a; reject=b; }); return {promise,resolve,reject}; };

test('request tickets isolate old errors, cancellation, modal close and retry', () => {
  const c = vm.createContext({ AbortController, document: {}, window: {} });
  vm.runInContext(asset('ui.js'), c);
  const gate = c.ConsoleUI.requestGate();
  const old = gate.begin(), current = gate.begin();
  assert.equal(old.signal.aborted, true);
  assert.equal(old.accepts(new Error('late failure')), false);
  assert.equal(current.isCurrent(), true);
  assert.equal(current.accepts({name:'AbortError'}), false);
  gate.cancel();
  assert.equal(current.isCurrent(), false);
  assert.equal(gate.begin().isCurrent(), true);
});

for (const [page, load, state, overrides] of [
  ['accounts.js', 'loadAccounts', 'accounts', 'sortAccounts=renderPlatformTabs=renderAccounts=updateStats=()=>{};'],
  ['models.js', 'loadModels', 'models', 'renderChannelTabs=updateModelChannelOptions=renderModels=updateRefreshButton=()=>{};'],
]) {
  test(page + ' ignores out-of-order responses and obsolete failures', async () => {
    const calls=[], toasts=[];
    const c = vm.createContext({ AbortController, console, URL, Set, document: {addEventListener(){},getElementById(){return null;},querySelectorAll(){return [];}}, window: {addEventListener(){}}, showToast: (...args)=>toasts.push(args), fetch: (url, options) => {const d=deferred(); calls.push({...d, options}); return d.promise;} });
    vm.runInContext(asset(page), c);
    vm.runInContext(overrides, c);
    const a=c[load](), b=c[load]();
    assert.equal(calls[0].options.signal.aborted,true);
    calls[1].resolve({ok:true,json:async()=>[{id:2}]}); await b;
    calls[0].resolve({ok:true,json:async()=>[{id:1}]}); await a;
    assert.equal(vm.runInContext(state+'[0].id',c),2);
    const old=c[load](), retry=c[load]();
    calls[3].resolve({ok:true,json:async()=>[{id:4}]}); await retry;
    calls[2].reject(new Error('obsolete error')); await old;
    assert.equal(vm.runInContext(state+'[0].id',c),4);
    assert.equal(toasts.length,0);
  });
}

test('model statuses reject booleans, aliases, unknown values and case variants', () => {
  const c = vm.createContext({ AbortController, document: {addEventListener(){},getElementById(){return null;}}, window: {} });
  vm.runInContext(asset('models.js'),c);
  for (const value of [true,false,'enabled','true','Available',' available ','other',null]) assert.equal(c.normalizeModelStatus(value),'offline');
  for (const value of ['available','maintenance','offline']) assert.equal(c.normalizeModelStatus(value),value);
});

test('key list obsolete failures cannot reset the current refresh button or data', async () => {
  const calls = [], nodes = new Map();
  const node = id => { if (!nodes.has(id)) nodes.set(id, {disabled:false,replaceChildren(){}}); return nodes.get(id); };
  const c = vm.createContext({AbortController, document:{getElementById:node}, window:{}, fetch:(_, options)=>{const d=deferred();calls.push({...d,options});return d.promise;}});
  vm.runInContext(asset('ui.js'),c);
  vm.runInContext('let apiKeys=[]; function setText(){}; function make(){};',c);
  vm.runInContext(asset('api-keys.js'),c);
  vm.runInContext('renderApiKeys=()=>{};',c);
  const old=c.loadApiKeys(), current=c.loadApiKeys();
  calls[0].reject(new Error('old error')); await old;
  assert.equal(node('keyRefresh').disabled,true);
  calls[1].resolve({ok:true,json:async()=>[{id:2}]}); await current;
  assert.equal(node('keyRefresh').disabled,false);
  assert.equal(vm.runInContext('apiKeys[0].id',c),2);
});

test('ops coalesces filter changes and suppresses cancelled errors before retrying the current scope', async () => {
  const calls=[], scheduled=[], statuses=[];
  const c=vm.createContext({AbortController, console, Date, URLSearchParams, setInterval(){}, document:{readyState:'loading',hidden:false,addEventListener(){},getElementById(){return null;}},window:{setTimeout:f=>scheduled.push(f)},fetch:(url,options)=>{const d=deferred();calls.push({...d,url,options});return d.promise;}});
  vm.runInContext(asset('ui.js'),c);
  let source=asset('ops.js').replace(/\}\)\(\);\s*$/, `
    renderSkeletons=syncUrl=renderKpis=renderHero=renderTrends=renderDistributions=renderConcurrency=renderMatrix=renderCoverage=updateChannelOptions=updateModelOptions=renderResources=()=>{};
    setStatus=(value)=>globalThis.statuses.push(value);
    globalThis.review={state,load};})();`);
  c.statuses=statuses; vm.runInContext(source,c);
  const initial=c.review.load(); c.review.state.channel='grok'; await c.review.load(); c.review.state.channel='cline'; await c.review.load();
  assert.equal(calls.length,2,'one overview and one runtime request stay single-flight');
  assert.equal(calls[0].options.signal.aborted,true);
  calls[0].reject(new Error('obsolete failure')); calls[1].resolve({ok:false}); await initial;
  assert.equal(statuses.includes('读取失败'),false); assert.equal(scheduled.length,1);
  const retry=scheduled.shift()(); assert.match(calls[2].url,/channel=cline/);
  calls[2].resolve({ok:true,json:async()=>({matrix:[],channels:[]})}); calls[3].resolve({ok:false}); await retry;
  assert.equal(c.review.state.loading,false); assert.equal(statuses.at(-1),'就绪');
});
