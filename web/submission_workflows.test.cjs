const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('./test-support.cjs');

function page(file) {
  const nodes = new Map(), notices = [];
  const make = () => ({ value: '', checked: false, disabled: false, children: [], style: {}, dataset: {},
    textContent: '', classList: { toggle() {}, add() {}, remove() {} }, setAttribute() {}, addEventListener() {},
    appendChild(node) { this.children.push(node); return node; }, replaceChildren(...nodes) { this.children = nodes; },
    querySelector() { return null; }, querySelectorAll() { return []; } });
  const node = id => { if (!nodes.has(id)) nodes.set(id, make()); return nodes.get(id); };
  const context = vm.createContext({ console, window: {}, document: { readyState: 'loading',
    addEventListener() {}, getElementById: node, createElement: make, querySelector: () => node('submit'), querySelectorAll: () => [] },
    showToast: (...args) => notices.push(args), encodeData: encodeURIComponent, decodeData: decodeURIComponent, escapeHtml: String });
  let source = fs.readFileSync(__dirname + '/static/js/' + file, 'utf8');
  if (file === 'alerts.js') source = source.replace(/\}\)\(\);\s*$/, 'globalThis.alertTest = { loadRules, saveRules, state };})();');
  vm.runInContext(source, context);
  return { context, node, notices };
}

test('model creation submits once and releases its lock after server failure', async () => {
  const { context, node } = page('models.js');
  node('modelChannel').value = 'Cline'; node('modelModelId').value = 'model-a';
  node('modelStatus').value = 'available';
  const requests = [];
  let respond;
  context.fetch = async (url, options) => {
    requests.push({ url, options });
    return new Promise(resolve => { respond = resolve; });
  };
  const first = context.saveModel({ preventDefault() {} });
  await context.saveModel({ preventDefault() {} });
  assert.equal(requests.length, 1);
  assert.equal(requests[0].options.method, 'POST');
  respond({ ok: false, status: 500, text: async () => 'unavailable' });
  await first;
  assert.equal(node('submit').disabled, false);
  const retry = context.saveModel({ preventDefault() {} });
  assert.equal(requests.length, 2);
  respond({ ok: false, status: 500, text: async () => 'unavailable' });
  await retry;
});

test('saving alert rules cancels older reads and ignores duplicate saves', async () => {
  const { context, node } = page('alerts.js');
  const rules = { MinRequests: 10, MinFailures: 2, SuccessRateWarning: 0.9,
    SuccessRateCritical: 0.7, ClearMargin: 0.03, RequireNoAvailableAccounts: true };
  for (const [key, value] of Object.entries(rules)) {
    node('rule' + key).value = String(key.startsWith('SuccessRate') || key === 'ClearMargin' ? value * 100 : value);
    node('rule' + key).checked = value === true;
  }
  const requests = [];
  context.fetch = (url, options) => new Promise(resolve => requests.push({ url, options, resolve }));
  const read = context.alertTest.loadRules();
  const save = context.alertTest.saveRules();
  await context.alertTest.saveRules();
  assert.equal(requests.length, 2);
  assert.equal(requests[0].options.signal.aborted, true);
  requests[1].resolve({ ok: true, json: async () => ({ rules }) });
  await save;
  requests[0].resolve({ ok: true, json: async () => ({ rules: {}, editable: false }) });
  await read;
  assert.equal(context.alertTest.state.editable, true);
  assert.equal(node('alertsSave').disabled, false);
  assert.equal(node('alertsState').textContent, '已保存并生效');
});

test('model row operations suppress conflicts but allow independent rows and retry', async () => {
  const { context } = page('models.js');
  vm.runInContext(`models = [
    { id: 'm1', model_id: 'one', channel: 'Cline', status: 'available' },
    { id: 'm2', model_id: 'two', channel: 'Cline', status: 'available' }
  ]; renderModels = () => {}; loadModels = async () => {};`, context);
  const requests = [];
  context.fetch = (url, options) => new Promise(resolve => requests.push({ url, options, resolve }));
  const first = context.toggleModelStatus('m1', false);
  await context.toggleModelStatus('m1', true);
  await context.deleteModel('m1');
  assert.equal(requests.length, 1);
  assert.match(context.toggleHtml({ id: 'm1', status: 'available' }), /disabled/);
  assert.match(context.actionButtonsHtml({ id: 'm1' }), /disabled/);
  const independent = context.toggleModelStatus('m2', false);
  assert.equal(requests.length, 2);
  requests[1].resolve({ ok: true, status: 204 });
  await independent;
  requests[0].resolve({ ok: false, status: 503, text: async () => 'offline' });
  await first;
  assert.doesNotMatch(context.toggleHtml({ id: 'm1', status: 'available' }), /disabled/);
  const retry = context.toggleModelStatus('m1', true);
  assert.equal(requests.length, 3);
  assert.equal(JSON.parse(requests[2].options.body).status, 'available');
  requests[2].resolve({ ok: true, status: 204 });
  await retry;
});

test('model editing owns the row until the post-save reload completes', async () => {
  const { context, node } = page('models.js');
  vm.runInContext("models = [{ id: 'm1', status: 'available' }]; renderModels = () => {};", context);
  node('modelId').value = 'm1';
  let finishLoad, calls = 0;
  context.loadModels = () => new Promise(resolve => { finishLoad = resolve; });
  context.fetch = async () => { calls++; return { ok: true, status: 204 }; };
  const save = context.saveModel({ preventDefault() {} });
  await new Promise(resolve => setImmediate(resolve));
  await context.toggleModelStatus('m1', false);
  assert.equal(calls, 1);
  finishLoad(); await save;
  assert.doesNotMatch(context.toggleHtml({ id: 'm1', status: 'available' }), /disabled/);
});

test('model batch actions count busy rows as failures and do not send conflicting writes', async () => {
  const { context, notices } = page('models.js');
  vm.runInContext("models = [{ id: 'm1', channel: 'Cline', status: 'available' }]; currentModelChannel = 'Cline'; renderModels = () => {}; loadModels = async () => {}; modelsSelectedIds.add('m1');", context);
  let respond, calls = 0;
  context.fetch = () => { calls++; return new Promise(resolve => { respond = resolve; }); };
  const row = context.toggleModelStatus('m1', false);
  await context.runModelBatch('enable');
  assert.equal(calls, 1);
  assert(notices.some(([message, kind]) => kind === 'error' && message.includes('成功 0，失败 1')));
  respond({ ok: true, status: 204 }); await row;
});
