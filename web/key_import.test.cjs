const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/static/js/api-key-import.js', 'utf8');
const registry = fs.readFileSync(__dirname + '/static/js/provider-registry.js', 'utf8');
function setup(fetcher) {
  const nodes = new Map(), calls = [];
  function node() {
    return { value: '', textContent: '', disabled: false, children: [], events: {},
      addEventListener(name, fn) { this.events[name] = fn; },
      appendChild(child) { this.children.push(child); },
      replaceChildren(...children) { this.children = children; this.value = children[0]?.value || ''; } };
  }
  const document = { querySelectorAll: () => [], addEventListener() {}, createElement: node,
    getElementById(id) { if (!nodes.has(id)) nodes.set(id, node()); return nodes.get(id); } };
  document.getElementById('keyImportApp').value = 'codex';
  const window = { location: { origin: 'https://gateway.example', href: '' }, addEventListener() {} };
  const context = { apiKeys: [{id:1,name:'test',secret_available:true,enabled:true}], keyStatus: row => row.enabled ? 'enabled' : 'disabled', getKeySecret: async () => 'sk-test+/=&', openModal() {}, closeModal() {}, setText: (id,text) => document.getElementById(id).textContent=text, window, document, URL, AbortController, Set, fetch: async (...args) => {
    calls.push(args); return fetcher ? fetcher(...args) : { ok: true, headers: new Headers({ 'content-type': 'application/json' }), json: async () => ({data: [{id: 'model/中文 +&?'}, {id: 'second'}]}) };
  } };
  vm.runInNewContext(registry, context); vm.runInNewContext(source, context);
  const el = id => document.getElementById('key' + id);
  const event = { preventDefault() {} };
  return { el, window, calls, event, context, async load(channel = 'workbuddy') {
    await context.openKeyImport(1);
    if (channel !== 'workbuddy') { el('ImportChannel').value = channel; await el('ImportChannel').events.change(); }
    el('ImportModel').value = 'model/中文 +&?'; el('ImportModel').events.change();
  } };
}
test('all four channels import Codex and Claude with encoded credentials and exact endpoints', async () => {
  for (const channel of ['workbuddy', 'qoder', 'cline', 'grok']) {
    const t = setup(); await t.load(channel);
    assert.equal(t.calls.at(-1)[0], 'https://gateway.example/' + channel + '/v1/models');
    assert.equal(t.calls.at(-1)[1].credentials, 'omit');
    assert.equal(t.calls.at(-1)[1].headers.Authorization, 'Bearer sk-test+/=&');
    for (const app of ['codex', 'claude']) {
      t.el('ImportApp').value = app; t.el('ImportApp').events.change();
      assert.equal(t.el('ImportButton').disabled, false);
      t.el('ImportForm').events.submit(t.event);
      const link = new URL(t.window.location.href), q = link.searchParams;
      assert.equal(link.protocol, 'ccswitch:'); assert.equal(link.hostname, 'v1'); assert.equal(link.pathname, '/import');
      assert.equal(q.get('resource'), 'provider'); assert.equal(q.get('app'), app);
      assert.equal(q.get('endpoint'), 'https://gateway.example/' + channel + (app === 'codex' ? '/v1' : ''));
      assert.equal(q.get('apiKey'), 'sk-test+/=&'); assert.equal(q.get('model'), 'model/中文 +&?');
      assert.equal(q.has('enabled'), false);
      for (const alias of ['haikuModel', 'sonnetModel', 'opusModel']) assert.equal(q.get(alias), app === 'claude' ? 'model/中文 +&?' : null);
      assert.match(t.el('ImportStatus').textContent, /请求打开/);
    }
  }
});
test('Key/channel changes invalidate catalog; arbitrary model cannot be imported', async () => {
  const t = setup(); await t.load();
  t.el('ImportModel').value = 'invented'; t.el('ImportModel').events.change();
  t.el('ImportForm').events.submit(t.event); assert.equal(t.window.location.href, '');
  t.el('ImportModel').value = 'second'; t.el('ImportModel').events.change();
  assert.equal(t.el('ImportButton').disabled, false);
  t.context.closeKeyImport(); assert.equal(t.el('ImportButton').disabled, true);
  t.el('ImportForm').events.submit(t.event); assert.equal(t.window.location.href, '');
});
test('failed, empty or malformed catalog never enables import or exposes error secrets', async () => {
  for (const value of [null, {}, {data: []}]) {
    const t = setup(async () => ({ok: true, headers: new Headers({'content-type':'application/json'}), json: async () => value}));
    await t.load(); assert.equal(t.el('ImportButton').disabled, true);
  }
  const t = setup(async () => { throw new Error('sk-secret-echo'); });
  await t.load(); assert.equal(t.window.location.href, ''); assert.equal(t.el('ImportButton').disabled, true);
  assert.doesNotMatch(t.el('CatalogStatus').textContent, /secret/);
});
test('late catalog cannot restore models after Key changes', async () => {
  let resolve, notify;
  const started = new Promise(r => {notify=r;});
  const t = setup(() => new Promise(r => { resolve = r; notify(); }));
  const pending = t.load();
  await started;
  t.context.closeKeyImport();
  resolve({ok: true, headers: new Headers({'content-type':'application/json'}), json: async () => ({data:[{id:'model/中文 +&?'}]})});
  await pending; assert.equal(t.el('ImportButton').disabled, true); assert.equal(t.el('ImportModel').disabled, true);
});
test('key page loads import logic without tutorials, budgets or secret storage', () => {
  const html = fs.readFileSync(__dirname + '/templates/pages/config.html', 'utf8');
  const modals = fs.readFileSync(__dirname + '/templates/components/modals/key-modals.html', 'utf8');
  const sidebar = fs.readFileSync(__dirname + '/templates/partials/sidebar.html', 'utf8');
  assert.ok(html.indexOf('provider-registry.js') < html.indexOf('api-key-import.js'));
  assert.match(modals, /keyImportForm/); assert.doesNotMatch(sidebar, /tab=tutorial|使用教程/);
  assert.doesNotMatch(modals, /billing|预算|账期|金额|消费/);
  assert.doesNotMatch(source, /localStorage|sessionStorage|console\./);
});
test('catalog failure can be retried and disabled keys cannot import', async () => {
  let fail = true;
  const t = setup(async () => { if (fail) throw Error('secret'); return {ok:true,headers:new Headers({'content-type':'application/json'}),json:async()=>({data:[{id:'second'}]})}; });
  await t.load(); fail=false; await t.el('LoadModels').events.click();
  t.el('ImportModel').value='second'; t.el('ImportModel').events.change();
  assert.equal(t.el('ImportButton').disabled,false);
  t.context.apiKeys[0].enabled=false;
  t.el('ImportForm').events.submit(t.event);
  assert.equal(t.window.location.href,'');
  assert.equal(t.el('ImportButton').disabled,true);
});
