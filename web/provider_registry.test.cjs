const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const root = __dirname;
const registrySource = fs.readFileSync(path.join(root, 'static/js/provider-registry.js'), 'utf8');
const accounts = fs.readFileSync(path.join(root, 'static/js/accounts.js'), 'utf8');
const models = fs.readFileSync(path.join(root, 'static/js/models.js'), 'utf8');
const payload = {
  defaultProviderKey: 'workbuddy',
  providers: [
    { key: 'workbuddy', label: 'WorkBuddy' },
    { key: 'cline', label: 'Cline' },
    { key: 'grok', label: 'Grok' },
    { key: 'doubao', label: 'Doubao' },
  ],
};

async function registry() {
  const window = {};
  vm.runInNewContext(registrySource, { window, Object, Promise });
  await window.OrchidsProviderRegistry.ready;
  return window.OrchidsProviderRegistry;
}

test('frontend provider registry exposes ordered immutable metadata and lookups', async () => {
  const value = await registry();
  assert.deepEqual(Array.from(value.keys), payload.providers.map((item) => item.key));
  assert.deepEqual(Array.from(value.channels), payload.providers.map((item) => item.label));
  assert.equal(value.defaultProviderKey, payload.defaultProviderKey);
  assert.ok(Object.isFrozen(value));
  assert.ok(Object.isFrozen(value.providers));
  assert.ok(value.providers.every(Object.isFrozen));
  for (const item of payload.providers) {
    assert.equal(value.get(` ${item.key.toUpperCase()} `).label, item.label);
    assert.equal(value.label(item.key), item.label);
  }
  assert.equal(value.get('unknown-provider'), null);
  assert.equal(value.label(' unknown-provider '), 'unknown-provider');
  // Backend/manifest equality is checked without writing files by
  // scripts/check-provider-registry.sh; this test covers the browser contract.
  assert.match(registrySource, /Code generated from internal\/channel definitions/);
});

test('accounts and models consume the backend-fed provider registry', () => {
  assert.match(accounts, /OrchidsProviderRegistry\?\.keys/);
  assert.match(accounts, /OrchidsProviderRegistry\?\.providers/);
  assert.match(models, /OrchidsProviderRegistry\?\.channels/);
  assert.doesNotMatch(accounts, /\["workbuddy", "cline", "grok"\]/);
  assert.doesNotMatch(models, /\["WorkBuddy", "Cline", "Grok"\]/);
});

test('all provider pages load the registry before page code', () => {
  for (const [name, script] of [['accounts', 'accounts.js'], ['models', 'models.js']]) {
    const html = fs.readFileSync(path.join(root, `templates/pages/${name}.html`), 'utf8');
    assert.ok(html.indexOf('provider-registry.js') >= 0, `${name} missing registry`);
    assert.ok(html.indexOf('provider-registry.js') < html.indexOf(script), `${name} loads registry too late`);
  }
});
