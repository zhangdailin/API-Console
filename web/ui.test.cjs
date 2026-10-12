const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
function runtime(fetch){const context=vm.createContext({fetch,window:{location:{href:'/admin/'}},document:{}});vm.runInContext(fs.readFileSync(__dirname+'/static/js/ui.js','utf8'),context);return context;}
test('shared requests preserve credentials and cancellation without mutation retries',async()=>{let calls=0;let sent;const signal=new AbortController().signal;const c=runtime(async(url,options)=>{calls++;sent=options;throw new Error('offline');});await assert.rejects(c.ConsoleAPI.json('/api/keys',{method:'POST',signal}),/offline/);assert.equal(calls,1);assert.equal(sent.signal,signal);assert.equal(sent.credentials,'same-origin');});
test('list errors do not become empty successful lists',async()=>{const c=runtime(async()=>({ok:true,json:async()=>({error:'bad'})}));await assert.rejects(c.ConsoleAPI.json('/api/accounts',{}, {array:true}),/格式无效/);});
test('HTTP JSON errors expose the message and permission denials keep the page',async()=>{const c=runtime(async()=>({ok:false,status:403,text:async()=>JSON.stringify({error:{message:'denied'}})}));await assert.rejects(c.ConsoleAPI.json('/api/accounts'),/^Error: denied$/);assert.equal(c.window.location.href,'/admin/');});
test('expired sessions redirect to the admin login',async()=>{const c=runtime(async()=>({ok:false,status:401,text:async()=> 'Unauthorized'}));await assert.rejects(c.ConsoleAPI.json('/api/accounts'),/Unauthorized/);assert.equal(c.window.location.href,'./login.html');});
test('HTML and malformed envelopes are rejected explicitly',async()=>{const c=runtime(async()=>({ok:true,headers:{get:()=> 'text/html'},json:async()=>({})}));await assert.rejects(c.ConsoleAPI.json('/api/config/list'),/JSON/);c.fetch=async()=>({ok:true,json:async()=>null});await assert.rejects(c.ConsoleAPI.json('/api/config/list',{}, {envelope:true}),/请求失败/);c.fetch=async()=>({ok:true,json:async()=>({code:0,data:[1]})});assert.deepEqual(Array.from(await c.ConsoleAPI.json('/api/config/list',{}, {envelope:true,array:true})),[1]);});

test('checked deletion accepts an empty 204 and batches count failed HTTP responses', async () => {
  const calls = [];
  const c = runtime(async url => {
    calls.push(url);
    return url === '/2' ? { ok: false, status: 403, text: async () => '{"error":{"message":"denied"}}' }
      : { ok: true, status: 204, json: () => { throw new Error('empty body'); } };
  });
  const result = await c.ConsoleAPI.batch([1, 2, 3], id => c.ConsoleAPI.checked('/' + id, { method: 'DELETE' }));
  assert.deepEqual(calls, ['/1', '/2', '/3']);
  assert.deepEqual(Array.from(result.succeeded), [1, 3]);
  assert.equal(result.failed[0].item, 2);
  assert.equal(result.failed[0].error.message, 'denied');
  assert.equal(result.failed[0].error.status, 403);
});

test('JSON options preserve headers and cancellation without changing caller options', () => {
  const c = runtime();
  const options = { signal: new AbortController().signal, headers: { 'Idempotency-Key': 'once' } };
  const sent = c.ConsoleAPI.jsonOptions('POST', { enabled: false }, options);
  assert.equal(sent.signal, options.signal);
  assert.equal(sent.headers['Idempotency-Key'], 'once');
  assert.equal(sent.headers['Content-Type'], 'application/json');
  assert.equal(options.headers['Content-Type'], undefined);
  assert.deepEqual(JSON.parse(sent.body), { enabled: false });
});

test('submission locks suppress repeated calls and restore controls after a rejection', async () => {
  const c = runtime();
  const gate = c.ConsoleUI.submission();
  const button = { disabled: false }, readonly = { disabled: true };
  let reject, calls = 0;
  const pending = gate.run(() => { calls++; return new Promise((_, fail) => { reject = fail; }); }, [button, readonly]);
  await gate.run(() => { calls++; }, [button]);
  assert.equal(calls, 1);
  assert.equal(button.disabled, true);
  reject(new Error('offline'));
  await assert.rejects(pending, /offline/);
  assert.equal(button.disabled, false);
  assert.equal(readonly.disabled, true);
  assert.equal(gate.busy, false);
  await gate.run(() => { calls++; });
  assert.equal(calls, 2);
});

test('journal requests preserve pagination and coverage, and reject malformed rows', async () => {
  const payload = { data: [{ event: { action: 'alert_fired' } }], next_cursor: 'cursor', coverage: { entries: 10 } };
  const c = runtime(async () => ({ ok: true, json: async () => payload }));
  const result = await c.ConsoleAPI.journal(new URLSearchParams({ kind: 'system' }));
  assert.equal(result.data, payload.data);
  assert.equal(result.next_cursor, 'cursor');
  assert.equal(result.coverage, payload.coverage);
  c.fetch = async () => ({ ok: true, json: async () => ({ data: {} }) });
  await assert.rejects(c.ConsoleAPI.journal(new URLSearchParams()), /日志列表格式无效/);
});

test('keyed submissions lock only the same identity and release on failure', async () => {
  const gate = runtime().ConsoleUI.keyedSubmission();
  let reject, calls = 0;
  const first = gate.run(1, () => { calls++; return new Promise((_, fail) => { reject = fail; }); });
  assert.equal(gate.busy('1'), true);
  await gate.run('1', () => { calls++; });
  assert.equal(calls, 1);
  assert.equal(await gate.run(2, () => 'independent'), 'independent');
  reject(new Error('offline'));
  await assert.rejects(first, /offline/);
  assert.equal(gate.busy(1), false);
  await gate.run('1', () => { calls++; });
  assert.equal(calls, 2);
});
