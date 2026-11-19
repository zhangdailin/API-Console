const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const source = fs.readFileSync(path.join(__dirname, 'static/js/accounts.js'), 'utf8');
const providers = ['doubao', 'workbuddy', 'cline', 'warp', 'grok'];
const privateValues = ['private-cookie-value', 'private-token-value', 'private-device-value', 'private-web-value', 'private-tea-value'];
const account = (id, type = 'doubao') => ({
  id, account_type: type, name: `Account ${id}`, enabled: true, has_credential: true,
  status_message: 'requires human verification',
  client_cookie: privateValues[0], access_token: privateValues[1],
  device_id: privateValues[2], web_id: privateValues[3], tea_uuid: privateValues[4],
});

function harness() {
  const calls = [], prompts = [], toasts = [], logs = [];
  const nodes = new Map();
  const descendants = (node) => node.children.flatMap(child => [child, ...descendants(child)]);
  function matches(node, selector) {
    if (selector === '[data-action]') return Boolean(node.dataset.action);
    const action = selector.match(/^\[data-action="([^"]+)"\]$/);
    if (action) return node.dataset.action === action[1];
    if (selector.startsWith('.')) {
      const [name, state] = selector.slice(1).split(':');
      return node.className.split(' ').includes(name) && (!state || state === 'checked' && node.checked);
    }
    return node.tagName === selector;
  }
  function element(tag) {
    const node = {
      tagName: tag, children: [], dataset: {}, style: {}, className: '', textContent: '',
      disabled: false, checked: false, value: '', parentElement: null,
      get innerHTML() { return this.markup || this.textContent; },
      set innerHTML(value) { this.markup = value; this.children = []; },
      appendChild(child) {
        if (child.tagName === 'fragment') child.children.slice().forEach(item => this.appendChild(item));
        else { child.parentElement = this; this.children.push(child); }
        return child;
      },
      replaceChildren(...children) { this.children = []; children.forEach(child => this.appendChild(child)); },
      setAttribute(name, value) { this[name] = String(value); },
      addEventListener() {},
      contains(child) { return child === this || descendants(this).includes(child); },
      closest(selector) { return matches(this, selector) ? this : this.parentElement?.closest(selector) || null; },
      querySelectorAll(selector) {
        const parts = selector.split(' ');
        return descendants(this).filter(child => matches(child, parts.at(-1)) &&
          (parts.length === 1 || child.parentElement?.closest(parts[0])));
      },
      querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
    };
    node.classList = {
      add(name) { if (!node.className.split(' ').includes(name)) node.className += ` ${name}`; },
      remove(name) { node.className = node.className.split(' ').filter(value => value !== name).join(' '); },
      contains(name) { return node.className.split(' ').includes(name); },
      toggle(name, active) { if (active) this.add(name); else this.remove(name); },
    };
    return node;
  }
  const getNode = (id) => {
    if (!nodes.has(id)) nodes.set(id, element('div'));
    return nodes.get(id);
  };
  const registry = {
    keys: providers, providers: providers.map(key => ({ key, label: key })),
    label: key => key, get: key => ({ key }), defaultProviderKey: 'doubao',
  };
  const payload = providers.map((type, index) => account(index + 1, type));
  const context = vm.createContext({
    document: {
      getElementById: getNode, createElement: element, createDocumentFragment: () => element('fragment'),
      querySelectorAll: () => [], querySelector: () => null, addEventListener() {},
    },
    window: { OrchidsProviderRegistry: registry, location: { href: 'http://localhost/admin/' }, addEventListener() {} },
    console: { error: (...args) => logs.push(args), warn: (...args) => logs.push(args), log: (...args) => logs.push(args) },
    confirm: text => { prompts.push(text); return true; },
    fetch: async (url, options) => {
      calls.push({ url, options });
      if (url === '/api/accounts') return { ok: true, status: 200, json: async () => payload };
      assert.fail('account rendering must not invoke an identity mutation endpoint');
    },
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/common.js'), 'utf8'), context);
  vm.runInContext(source, context);
  context.showToast = (message, tone) => toasts.push({ message, tone });
  context.checkAccount = () => assert.fail('device updates must not check accounts');
  context.AccountVerification = { open: () => assert.fail('device updates must not open verification') };
  context.fixtureAccounts = payload;
  vm.runInContext('accounts = fixtureAccounts;', context);
  context.renderAccounts();
  const container = getNode('accountsList');
  const buttons = () => container.querySelectorAll('[data-action="rotate-device-identity"]');
  const displayed = () => JSON.stringify({
    prompts, toasts, logs,
    dom: [...nodes.values()].flatMap(node => [node, ...descendants(node)])
      .map(node => [node.textContent, node.innerHTML, node.title]),
  });
  return { context, calls, prompts, toasts, logs, container, buttons, displayed };
}

function assertPrivate(h) {
  for (const secret of privateValues) assert.ok(!h.displayed().includes(secret), `private fixture appeared in the UI: ${secret}`);
}

test('Doubao has one status refresh and no manual identity control in table and cards', () => {
  const h = harness();
  for (const view of ['table', 'cards']) {
    h.context.setAccountView(view);
    assert.equal(h.buttons().length, 0);
    const refresh = h.container.querySelectorAll('[data-action="refresh"]');
    assert.equal(refresh.length, providers.length);
    assert.equal(refresh.filter(button => button.dataset.id === '1').length, 1);
  }
  assert.equal(h.calls.length, 0, 'rendering must not rotate device identifiers');
  assert.equal(h.context.rotateDeviceIdentity, undefined);
  assertPrivate(h);
});

test('reloading accounts only reads rows and never POSTs an identity change', async () => {
  const h = harness();
  await h.context.loadAccounts();
  assert.deepEqual(h.calls.map(call => [call.url, call.options?.method || 'GET']), [['/api/accounts', 'GET']]);
  assert.equal(h.buttons().length, 0);
  assert.equal(h.prompts.length, 0);
  assertPrivate(h);
});
