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

function harness({ confirmed = true, request } = {}) {
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
  let payload = providers.map((type, index) => account(index + 1, type));
  const context = vm.createContext({
    document: {
      getElementById: getNode, createElement: element, createDocumentFragment: () => element('fragment'),
      querySelectorAll: () => [], querySelector: () => null, addEventListener() {},
    },
    window: { OrchidsProviderRegistry: registry, location: { href: 'http://localhost/admin/' }, addEventListener() {} },
    console: { error: (...args) => logs.push(args), warn: (...args) => logs.push(args), log: (...args) => logs.push(args) },
    confirm: text => { prompts.push(text); return confirmed; },
    fetch: async (url, options) => {
      calls.push({ url, options });
      if (url === '/api/accounts') return { ok: true, status: 200, json: async () => payload };
      if (request) return request(url, options);
      return { ok: true, status: 200, json: async () => ({ status: 'updated', account_id: 1 }) };
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
  const click = (button = buttons()[0]) => {
    // Exercise the delegated handler from an icon/label nested in the button.
    const target = element('span');
    button.appendChild(target);
    return container.onclick({ target });
  };
  const displayed = () => JSON.stringify({
    prompts, toasts, logs,
    dom: [...nodes.values()].flatMap(node => [node, ...descendants(node)])
      .map(node => [node.textContent, node.innerHTML, node.title]),
  });
  return { context, calls, prompts, toasts, logs, container, buttons, click, displayed,
    setPayload(value) { payload = value; },
  };
}

function assertPrivate(h) {
  for (const secret of privateValues) assert.ok(!h.displayed().includes(secret), `private fixture appeared in the UI: ${secret}`);
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test('device identity action is Doubao-only in table and card DOM and reuses the refresh icon', () => {
  const h = harness();
  for (const view of ['table', 'cards']) {
    h.context.setAccountView(view);
    assert.equal(h.buttons().length, 1);
    const button = h.buttons()[0];
    assert.equal(button.dataset.id, '1');
    assert.match(button.innerHTML, /更新设备标识/);
    const refresh = h.container.querySelectorAll('[data-action="refresh"]')[0];
    assert.equal(button.innerHTML.match(/<svg.*?<\/svg>/)[0], refresh.innerHTML.match(/<svg.*?<\/svg>/)[0]);
  }
  assert.equal(h.calls.length, 0, 'rendering must not perform network requests');
  assertPrivate(h);
});

test('confirmed delegated click sends one same-origin POST, reloads, and keeps verification state', async () => {
  const h = harness();
  await h.click();
  assert.equal(h.prompts.length, 1);
  for (const field of ['device_id', 'web_id', 'tea_uuid']) assert.ok(h.prompts[0].includes(field));
  assert.match(h.prompts[0], /保留登录 Cookie 和当前验证状态/);
  assert.match(h.prompts[0], /不保证解决人机验证/);
  assert.match(h.prompts[0], /不会发起生成请求/);
  assert.deepEqual(h.calls.map(call => [call.url, call.options.method || 'GET']), [
    ['/api/accounts/1/device-identity', 'POST'], ['/api/accounts', 'GET'],
  ]);
  assert.equal(h.calls[0].options.credentials, 'same-origin');
  assert.equal(h.calls[0].options.body, undefined, 'the server owns random identity creation');
  assert.equal(h.buttons()[0].disabled, false, 'reloaded action must be enabled after finally');
  assert.equal(h.toasts.at(-1).tone, 'success');
  assert.equal(vm.runInContext('accounts[0].status_message', h.context), 'requires human verification');
  assertPrivate(h);
});

test('a successful update never reads or displays the endpoint response body', async () => {
  const h = harness({ request: () => ({
    ok: true, status: 200,
    json: () => assert.fail('the action must not read raw identity response data'),
    text: () => assert.fail('the action must not read raw identity response text'),
  }) });
  await h.click();
  assert.equal(h.toasts.at(-1).tone, 'success');
  assertPrivate(h);
});

test('cancelling confirmation sends no request and releases the action', async () => {
  const h = harness({ confirmed: false });
  await h.click();
  assert.equal(h.prompts.length, 1);
  assert.equal(h.calls.length, 0);
  assert.equal(h.toasts.length, 0);
  assert.equal(h.buttons()[0].disabled, false);
  assert.equal(vm.runInContext('deviceIdentityInFlight.size', h.context), 0);
});

test('concurrent clicks stay single-flight across row rerenders until request settles', async () => {
  const pending = deferred();
  const h = harness({ request: () => pending.promise });
  const first = h.click();
  assert.equal(h.buttons()[0].disabled, true);
  await h.click();
  h.context.renderAccounts();
  assert.equal(h.buttons()[0].disabled, true);
  await h.click();
  assert.equal(h.calls.length, 1);
  assert.equal(h.prompts.length, 1);
  pending.resolve({ ok: true, status: 200 });
  await first;
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
  assert.equal(h.buttons()[0].disabled, false);
});

test('an asynchronous ConsoleUI confirmation is single-flight before any request', async () => {
  const pending = deferred();
  const h = harness();
  h.context.ConsoleUI = { ...h.context.ConsoleUI, confirm: text => { h.prompts.push(text); return pending.promise; } };
  const first = h.click();
  await h.click();
  assert.equal(h.calls.length, 0);
  assert.equal(h.prompts.length, 1);
  pending.resolve(true);
  await first;
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
});

for (const failure of ['network', 'http']) {
  test(`${failure} failure uses a fixed safe error, reenables controls, and permits retry`, async () => {
    let attempts = 0;
    const h = harness({ request: () => {
      attempts++;
      if (attempts > 1) return { ok: true, status: 200 };
      const secret = privateValues.join(' ');
      if (failure === 'network') throw new Error(secret);
      return { ok: false, status: 500, text: async () => JSON.stringify({ error: secret }) };
    } });
    await h.click();
    assert.equal(h.calls.length, 1, 'failed update must not load, check, or verify accounts');
    assert.equal(h.toasts.at(-1).tone, 'error');
    assert.equal(h.toasts.at(-1).message, '更新设备标识失败，请稍后重试');
    assert.equal(h.buttons()[0].disabled, false);
    assert.equal(vm.runInContext('deviceIdentityInFlight.size', h.context), 0);
    assertPrivate(h);
    await h.click();
    assert.equal(attempts, 2);
    assert.equal(h.toasts.at(-1).tone, 'success');
    assertPrivate(h);
  });
}

test('direct action calls for other providers or unknown accounts cannot send requests', async () => {
  const h = harness();
  for (const id of [2, 3, 4, 5, 999]) await h.context.rotateDeviceIdentity(id);
  assert.equal(h.calls.length, 0);
  assert.equal(h.prompts.length, 0);
});
