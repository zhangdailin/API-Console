const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('./test-support.cjs');

function setup() {
  const requests = [], listeners = {}, attributes = {};
  const button = { disabled: true, textContent: 'loading',
    addEventListener(name, fn) { listeners[name] = fn; },
    setAttribute(name, value) { attributes[name] = value; } };
  const context = vm.createContext({ window: { location: {} }, document: {
    readyState: 'loading', addEventListener() {},
    getElementById: id => id === 'logsDiagnosticsToggle' ? button : null,
  }, fetch: (url, options) => new Promise(resolve => requests.push({ url, options, resolve })) });
  const source = fs.readFileSync(__dirname + '/static/js/logs.js', 'utf8')
    .replace(/\}\)\(\);\s*$/, 'globalThis.initDiagnosticToggle = initDiagnosticToggle;})();');
  vm.runInContext(source, context);
  return { button, attributes, requests, init: context.initDiagnosticToggle,
    click: () => listeners.click() };
}

const json = enabled => ({ ok: true, json: async () => ({ enabled }) });

for (const enabled of [false, true]) {
  test('initial diagnostic read unlocks the button and supports both toggles: ' + enabled, async () => {
    const h = setup();
    const init = h.init();
    assert.equal(h.button.disabled, true);
    assert.equal(h.requests[0].url, '/api/journal/diagnostics/settings');
    assert.equal(h.requests[0].options.method, undefined);
    assert.equal(h.requests[0].options.cache, 'no-store');
    h.requests[0].resolve(json(enabled)); await init;
    assert.equal(h.button.disabled, false);
    assert.equal(h.attributes['aria-pressed'], String(enabled));
    for (const next of [!enabled, enabled]) {
      const change = h.click();
      assert.equal(h.button.disabled, true);
      await h.click();
      const request = h.requests.at(-1);
      assert.equal(request.options.method, 'PUT');
      assert.equal(request.options.credentials, 'same-origin');
      assert.deepEqual(JSON.parse(request.options.body), { enabled: next });
      request.resolve(json(next)); await change;
      assert.equal(h.button.disabled, false);
      assert.equal(h.attributes['aria-pressed'], String(next));
    }
    assert.equal(h.requests.length, 3);
  });
}

for (const response of [
  { ok: false, status: 503, text: async () => 'offline' },
  { ok: true, json: async () => ({ enabled: 'false' }) },
]) {
  test('failed or invalid initial settings keep the read-retry button usable: ' + response.ok, async () => {
    const h = setup(), init = h.init();
    h.requests[0].resolve(response); await init;
    assert.equal(h.button.disabled, false);
    assert.match(h.button.textContent, /读取失败/);
    const retry = h.click();
    assert.equal(h.requests[1].options.method, undefined);
    h.requests[1].resolve(json(false)); await retry;
    assert.equal(h.button.disabled, false);
    assert.equal(h.attributes['aria-pressed'], 'false');
  });
}

test('failed diagnostic writes retain the last known state and allow retry', async () => {
  const h = setup(), init = h.init();
  h.requests[0].resolve(json(false)); await init;
  const save = h.click();
  h.requests[1].resolve({ ok: false, status: 503, text: async () => 'storage unavailable' });
  await save;
  assert.equal(h.button.disabled, false);
  assert.equal(h.attributes['aria-pressed'], 'false');
  assert.match(h.button.textContent, /保存失败/);
  const retry = h.click();
  assert.deepEqual(JSON.parse(h.requests[2].options.body), { enabled: true });
  h.requests[2].resolve(json(true)); await retry;
  assert.equal(h.button.disabled, false);
  assert.equal(h.attributes['aria-pressed'], 'true');
});

test('the template keeps diagnostic settings disabled until their initial read', () => {
  const html = fs.readFileSync(__dirname + '/templates/pages/logs.html', 'utf8');
  assert.match(html, /<button[^>]+id="logsDiagnosticsToggle"[^>]+disabled/);
});
