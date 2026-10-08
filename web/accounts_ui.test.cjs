// Run with: node --test web/accounts_ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');
const strip = (html) => String(html).replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();

test('hidden provider sections cannot be made visible by component display rules', () => {
  const css = fs.readFileSync(path.join(__dirname, 'static/css/main.css'), 'utf8');
  assert.match(css, /\[hidden\]\s*\{[^}]*display:\s*none\s*!important/s);
});

function loadUI({ deferTimers = false, accountsPayload = [] } = {}) {
  const timers = new Map();
  let timerId = 0;
  const schedule = (fn, repeat) => {
    const id = ++timerId;
    timers.set(id, { fn, repeat });
    return id;
  };
  const scheduleTimeout = (fn) => {
    if (deferTimers) return schedule(fn, false);
    fn();
    return 0;
  };
  const requests = [];
  const storage = new Map();
  // Minimal element/DOM surface: enough for tab rendering and modal wiring.
  const makeElement = (tag) => {
    const classes = new Set();
    const children = [];
    const listeners = {};
    const element = {
      tagName: tag,
      value: '',
      hidden: false,
      required: false,
      disabled: false,
      checked: false,
      textContent: '',
      // escapeHtml() renders through a detached div, so the stub must mirror
      // textContent into innerHTML the way the DOM does.
      get innerHTML() { return this.__innerHTML !== undefined ? this.__innerHTML : this.textContent; },
      set innerHTML(value) { this.__innerHTML = value; children.splice(0); },
      style: {},
      dataset: {},
      children,
      reset() {},
      setAttribute(name,value) { this[name]=String(value); },
      removeAttribute(name) { delete this[name]; },
      replaceChildren(...nodes) { children.splice(0,children.length,...nodes); },
      querySelectorAll(selector) {
        const [ancestor, tag] = selector.split(' ');
        const descendants = (parent) => parent.children.flatMap((child) => [child, ...descendants(child)]);
        const all = descendants(this);
        return tag ? all.filter((child) => child.tagName === ancestor).flatMap((child) => descendants(child).filter((candidate) => candidate.tagName === tag))
          : all.filter((child) => child.tagName === selector);
      },
      querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
      addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
      click() { (listeners.click || []).forEach((fn) => fn({ target: this })); },
      appendChild(child) {
        if (child.tagName === 'fragment') children.push(...child.children.splice(0));
        else children.push(child);
        return child;
      },
      classList: {
        add: (name) => classes.add(name),
        remove: (name) => classes.delete(name),
        toggle: (name, on) => (on ? classes.add(name) : classes.delete(name)),
        contains: (name) => classes.has(name),
      },
    };
    return element;
  };
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement('div'));
    return elements.get(id);
  };
  const context = vm.createContext({
    document: {
      getElementById: node,
      querySelector: (selector) => selector === '#platformFilters .tab-item.active'
        ? node('platformFilters').children.find((tab) => tab.classList.contains('active')) || null
        : node(selector),
      createElement: makeElement,
      createDocumentFragment: () => makeElement('fragment'),
      querySelectorAll: (selector) => selector === '#platformFilters .tab-item' ? node('platformFilters').children : [],
      addEventListener() {},
    },
    window: {
      isSecureContext: true,
      location: {href:"http://localhost/admin/"},
      setInterval: (fn) => schedule(fn, true),
      clearInterval: (id) => timers.delete(id),
      setTimeout: scheduleTimeout,
      clearTimeout: (id) => timers.delete(id),
      addEventListener() {},
      dispatchEvent() {},
      matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
      innerWidth: 1440,
      localStorage: {
        getItem: (key) => (storage.has(key) ? storage.get(key) : null),
        setItem: (key, value) => storage.set(key, String(value)),
        removeItem: (key) => storage.delete(key),
      },
    },
    setInterval: (fn) => schedule(fn, true),
    clearInterval: (id) => timers.delete(id),
    requestAnimationFrame: (fn) => deferTimers ? schedule(fn, false) : fn(),
    setTimeout: scheduleTimeout,
    clearTimeout: (id) => timers.delete(id),
  });
  context.fetch = async (url, options = {}) => {
    requests.push({ url: String(url), method: options.method || 'GET' });
    if (url === '/api/providers') return { ok: true, json: async () => ({ defaultProviderKey:'workbuddy', providers:[
      {key:'workbuddy',label:'WorkBuddy'},
      {key:'qoder',label:'Qoder'},{key:'cline',label:'Cline'},{key:'grok',label:'Grok'},
    ]}) };
    return { ok: true, status: 200, headers: { get: () => 'application/json' },
      json: async () => url === '/api/accounts' ? accountsPayload : [] };
  };
  context.CustomEvent = class { constructor(type, init={}) { this.type=type; this.detail=init.detail; } };
  for (const file of ['common.js', 'provider-registry.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  const runTimers = async () => {
    // Run three clock turns, including callbacks scheduled by an earlier turn.
    // Bound the turns so an accidental recurring task cannot hang the suite.
    for (let turn = 0; turn < 3; turn++) {
      for (const [id, timer] of [...timers]) {
        if (!timers.has(id)) continue;
        if (!timer.repeat) timers.delete(id);
        await timer.fn();
      }
      await new Promise((resolve) => setImmediate(resolve));
    }
  };
  return { context, node, storage, requests, runTimers };
}

function workBuddyAccount(overrides = {}) {
  return {
    id: 11,
    account_type: 'workbuddy',
    workbuddy_access_token: 'eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1aWQifQ.sig',
    has_credential: true,
    workbuddy_uid: '07ab88c8-5596-4257-8d21-e9fcbe3a3810',
    enabled: true,
    weight: 1,
    ...overrides,
  };
}

const LOGIN_PLATFORMS = [
  { key: 'grok', label: 'Grok', group: 'grokDeviceLoginGroup' },
  { key: 'cline', label: 'Cline', group: 'clineLoginGroup' },
  { key: 'workbuddy', label: 'WorkBuddy', group: 'workbuddyLoginGroup' },
  { key: 'qoder', label: 'Qoder', group: 'qoderLoginGroup' },
];

for (const { key, label } of LOGIN_PLATFORMS) {
  test(`${label} tab opens only its official login and retains settings editing`, () => {
    const { context, node } = loadUI();
    context.renderPlatformTabs();
    const previous = LOGIN_PLATFORMS.find((provider) => provider.key !== key);
    context.filterByPlatform(previous.key);
    context.openModal();
    assert.equal(node(previous.group).hidden, false, 'seed a different provider login surface');
    const tab = node('platformFilters').children.find((candidate) => decodeURIComponent(candidate.dataset.platform || '') === key);
    assert.ok(tab, `no ${key} tab`);
    tab.click();
    assert.equal(vm.runInContext('currentPlatform', context), key);
    assert.equal(tab.classList.contains('active'), true);
    context.openModal();
    assert.equal(node('accountType').value, key);
    assert.equal(node('accountTypeDisplay').value, label);
    const checkGroups = () => {
      for (const provider of LOGIN_PLATFORMS) {
        assert.equal(node(provider.group).hidden, provider.key !== key, `${key}: ${provider.group}`);
      }
    };
    checkGroups();
    assert.equal(node('#accountForm button[type="submit"]').hidden, true, 'new accounts require official login');
    context.openModal({ id: 7, account_type: key, enabled: true });
    assert.equal(node('accountType').value, key);
    assert.equal(node('accountTypeDisplay').value, label);
    assert.equal(node('#accountForm button[type="submit"]').hidden, false, 'existing settings remain editable');
    checkGroups();
  });

  test(`${label} rejects manual creation without fetching`, async () => {
    const { context, node } = loadUI();
    node('accountType').value = key;
    node('accountId').value = '';
    const notices = [];
    const requests = [];
    let prevented = false;
    context.showToast = (message, kind) => notices.push({ message, kind });
    context.fetch = (...args) => {
      requests.push(args);
      throw new Error('manual creation must not send a request');
    };
    await context.saveAccount({ preventDefault() { prevented = true; } });
    assert.equal(prevented, true);
    assert.deepEqual(requests, [], 'even a caught fetch attempt must fail this test');
    assert.deepEqual(notices, [{ message: `请使用「使用 ${label} 官方网页登录」添加账号`, kind: 'error' }]);
  });
}

test('the active platform tab wins over a stale account-type field', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, toggle() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();
  context.filterByPlatform('grok');
  // A previous modal interaction must not leak its type into the next open.
  context.setAccountModalType('workbuddy');
  node('accountId').value = '';
  context.openModal();
  assert.equal(node('accountType').value, 'grok', 'the active platform tab wins over a stale field');
});

test('the visibly highlighted provider wins if in-memory state is stale', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, toggle() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();
  context.filterByPlatform('grok');
  const tabs = node('platformFilters').children;
  for (const tab of tabs) tab.classList.toggle('active', decodeURIComponent(tab.dataset.platform) === 'cline');
  assert.equal(vm.runInContext('currentPlatform', context), 'grok', 'only the visible selection changes');
  node('accountId').value = '';
  context.openModal();
  assert.equal(node('accountType').value, 'cline');
  assert.equal(node('accountTypeDisplay').value, 'Cline');
});

test('the account modal has no manual credential or batch import inputs', () => {
  const template = fs.readFileSync(path.join(__dirname, 'templates/components/modals/account-modal.html'), 'utf8');
  const source = fs.readFileSync(path.join(__dirname, 'static/js/accounts.js'), 'utf8');
  for (const id of ['clientCookie', 'ssoCredentialGroup', 'accountImportStatus', 'tokenLabel', 'tokenHint']) {
    assert.doesNotMatch(template, new RegExp(`id="${id}"`));
    assert.doesNotMatch(source, new RegExp(`getElementById\\("${id}"\\)`));
  }
  assert.doesNotMatch(source, /runAccountCreatePool|splitBatchCredentialInput|buildAccountPayload/);
});

test('loading accounts stays read-only after scheduled callbacks run', async () => {
  const accountsPayload = LOGIN_PLATFORMS.map(({ key }, index) => ({
    id: index + 1, account_type: key, enabled: true, has_credential: true,
    credential_type: 'oauth', grok_provider: key === 'grok' ? 'build' : undefined,
    // An unsynced snapshot used to trigger automatic upstream checks.
    last_checked_at: '', quota_supported: false,
  }));
  const { context, node, requests, runTimers } = loadUI({ deferTimers: true, accountsPayload });
  await context.loadAccounts();
  assert.deepEqual(requests, [{ url: '/api/accounts', method: 'GET' }]);
  assert.equal(vm.runInContext('accounts.length', context), 4);
  assert.equal(node('totalAccounts').textContent, 4, 'the real stats/render path must run');
  await runTimers();
  assert.deepEqual(requests, [{ url: '/api/accounts', method: 'GET' }], 'timer execution must not issue any upstream work');
  assert.equal(requests.some(({ url }) => /\/(?:check|refresh)(?:[/?]|$)/.test(url)), false);
});

test('Cline settings save succeeds without submitting credentials', async () => {
  const { context, node } = loadUI();
  node('accountType').value = 'cline';
  node('accountId').value = '7';
  node('enabled').checked = true;
  vm.runInContext('accounts = [{ id: 7, account_type: "cline", weight: 2, has_credential: true }]', context);
  let sent;
  context.fetch = async (url, options) => { sent = { url, options }; return { ok: true }; };
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  context.showToast = () => {};
  await context.saveAccount({ preventDefault() {} });
  assert.equal(sent.url, '/api/accounts/7');
  assert.equal(sent.options.method, 'PUT');
  assert.deepEqual(JSON.parse(sent.options.body), { account_type: 'cline', weight: 2, enabled: true });
});

test('opening the WorkBuddy modal never starts a login on its own', () => {
  const { context, node } = loadUI();
  vm.runInContext(
    'globalThis.WorkBuddyLogin = { start() { globalThis.__wbStarted = (globalThis.__wbStarted || 0) + 1; }, stop() {} };',
    context,
  );
  node('accountModal').classList = { add() {}, remove() {}, toggle() {}, contains() { return true; } };
  node('accountType').value = 'workbuddy';
  node('accountId').value = '';
  node('enabled').checked = true;

  context.openModal();
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 0,
    'opening the add-account modal must not open the login page');
  assert.equal(node('workbuddyLoginGroup').hidden, false, 'the login button must be presented');
  assert.equal(node('workbuddyLoginStatus').hidden, true, 'no status should be shown before a click');

  // Editing an existing account must not navigate either.
  context.openModal(workBuddyAccount());
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 0,
    'opening the edit modal must not open the login page');

  // Only an explicit click on the login button starts the flow.
  vm.runInContext('globalThis.WorkBuddyLogin.start()', context);
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 1);
});

test('WorkBuddy edits may keep the stored credential and never display the refresh token', async () => {
  const { context, node } = loadUI();
  const account = workBuddyAccount();
  vm.runInContext(`accounts = [${JSON.stringify(account)}]`, context);
  assert.equal(context.hasSidebarAccountCredential(account), true);
  assert.equal(context.hasSidebarAccountCredential({ ...account, has_credential: false }), false,
    'a visible access token must not override the server credential verdict');
  assert.equal(context.hasSidebarAccountCredential({ account_type: 'workbuddy', enabled: true }), false);

  node('accountType').value = 'workbuddy';
  node('accountId').value = '11';
  node('enabled').checked = true;
  // Settings updates never carry any credential fields.
  let sent;
  context.fetch = async (url, options) => { sent = { url, options }; return { ok: true }; };
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  context.showToast = () => {};
  await context.saveAccount({ preventDefault() {} });

  assert.equal(sent.url, '/api/accounts/11');
  assert.equal(sent.options.method, 'PUT');
  const body = JSON.parse(sent.options.body);
  assert.equal(body.account_type, 'workbuddy');
  assert.equal(body.client_cookie, undefined);
  assert.equal(body.refresh_token, undefined);
});

test('WorkBuddy rows show the metered credits, plan label and signed-in email', () => {
  const { context } = loadUI();
  const account = workBuddyAccount({
    email: 'operator@example.com',
    workbuddy_uid: '07ab88c8-5596-4257-8d21-e9fcbe3a3810',
    quota_supported: true,
    quota_limit: 350,
    quota_remaining: 147.28,
    quota_used: 202.72,
    quota_unit: 'credit',
    quota_plan: 'Free Plan Subscription',
    quota_consumed_units: 202,
    quota_reset_at: new Date(Date.now() + 12 * 86400000).toISOString(),
    usage_limit: 350,
    usage_current: 147.28,
    request_count: 0,
  });

  // The meter reports REMAINING; reading usage_current as "used" would invert it.
  const quota = context.getQuotaStats(account);
  assert.equal(quota.workbuddy, true);
  assert.equal(quota.limit, 350);
  assert.equal(quota.remaining, 147.28);
  assert.equal(quota.used, 350 - 147.28, 'used must be derived from the meter');
  assert.equal(quota.pctRemaining, 42, 'the displayed percentage rounds to whole points');

  const markup = context.buildQuotaMarkup(account);
  assert.match(markup, /147\.28 \/ 350/);
  assert.match(markup, /剩余/);
  assert.match(markup, /天后重置/);

  // 等级 shows the upstream plan label, not a guessed subscription tier.
  assert.match(context.buildSubscriptionMarkup(account), /Free Plan Subscription/);

  // 调用 falls back to meter consumption because this channel has no request counter.
  assert.equal(context.accountUsageCounter(account), 202);
  assert.equal(context.accountUsageCounter({ account_type: 'cline', request_count: 7 }), 7);

  // The identity leads with the email; the secondary credential summary uses
  // the server flag rather than exposing any access or refresh token.
  assert.equal(context.accountIdentityPrimary(account), 'operator@example.com');
  const tokenCell = context.formatTokenDisplay(account);
  assert.equal(tokenCell, '凭证已配置');
  assert.doesNotMatch(tokenCell, /workbuddy_refresh_token/);
});

test('WorkBuddy and Qoder share snapshot parsing without mixing spent-credit semantics', () => {
  const { context } = loadUI();
  const fields = { quota_supported: true, quota_limit: 350, quota_remaining: 120.5,
    quota_used: 19.25, quota_unit: 'credit', quota_reset_at: '2027-01-01T00:00:00Z' };
  const workbuddy = context.getQuotaStats(workBuddyAccount(fields));
  const qoder = context.getQuotaStats(qoderTrialAccount({ ...fields, quota_exhausted: true,
    quota_upgrade_url: ' https://qoder.com/upgrade ' }));
  assert.equal(workbuddy.used, 229.5, 'WorkBuddy spent amount derives from the remaining meter');
  assert.equal(qoder.used, 19.25, 'Qoder spent amount comes from quota_used');
  assert.equal(workbuddy.workbuddy, true);
  assert.equal(qoder.qoder, true);
  assert.equal(qoder.exhausted, true);
  assert.equal(qoder.upgradeUrl, 'https://qoder.com/upgrade');
  assert.equal(workbuddy.resetAt, qoder.resetAt);
});

test('estimated and upstream-confirmed Grok Free windows retain distinct provenance', () => {
  const { context } = loadUI();
  const base = { account_type: 'grok', credential_type: 'oauth', grok_provider: 'build',
    quota_limit: 500, quota_used: 125, quota_window_hours: 24, quota_reset_at: '2027-01-01',
    quota_source: 'billingProfile', quota_confidence: 'estimated', quota_limit_known: false };
  const estimate = context.getQuotaStats(base);
  const confirmed = context.getQuotaStats({ ...base, quota_source: 'upstreamExhaustion',
    quota_confidence: 'confirmed' });
  assert.equal(estimate.estimated, true);
  assert.equal(estimate.confirmedFree, undefined);
  assert.equal(estimate.limitKnown, false);
  assert.equal(estimate.resetAt, '');
  assert.match(context.buildQuotaMarkup(base), /≈/);
  assert.equal(confirmed.confirmedFree, true);
  assert.equal(confirmed.estimated, undefined);
  assert.equal(confirmed.limitKnown, true);
  assert.equal(confirmed.resetAt, base.quota_reset_at);
  assert.doesNotMatch(context.buildQuotaMarkup({ ...base, quota_source: 'upstreamExhaustion',
    quota_confidence: 'confirmed' }), /≈/);
});

test('official-login cleanup stops each provider and hides its own status and link', () => {
  const { context, node } = loadUI();
  const calls = [];
  for (const [provider, stop, status, link] of [
    ['WorkBuddyLogin', 'stopWorkBuddyLogin', 'workbuddyLoginStatus', ''],
    ['QoderLogin', 'stopQoderLogin', 'qoderLoginStatus', 'qoderLoginLink'],
    ['ClineLogin', 'stopClineLogin', 'clineLoginStatus', 'clineLoginLink'],
  ]) {
    context[provider] = { stop: () => calls.push(provider) };
    node(status).hidden = false;
    node(status).textContent = 'pending';
    if (link) node(link).hidden = false;
    context[stop]();
    assert.equal(node(status).hidden, true);
    assert.equal(node(status).textContent, '');
    if (link) assert.equal(node(link).hidden, true);
  }
  assert.deepEqual(calls, ['WorkBuddyLogin', 'QoderLogin', 'ClineLogin']);
});

test('WorkBuddy without a meter snapshot says so instead of showing a fake quota', () => {
  const { context } = loadUI();
  const account = workBuddyAccount({ email: 'operator@example.com' });
  assert.equal(context.getSidebarQuotaStats(account), null);
  const quota = context.getQuotaStats(account);
  assert.equal(quota.unknown, true);
  assert.match(context.buildQuotaMarkup(account), /WorkBuddy 计量接口未返回数据/);
  assert.match(context.buildSubscriptionMarkup(account), /未同步/);
  // usage_current must never be interpreted as a remaining balance without the
  // explicit quota_* fields the server sends.
  assert.equal(context.getQuotaStats({ account_type: 'workbuddy', usage_limit: 350, usage_current: 147.28 }).unknown, true);
});

test('WorkBuddy delegates to the shared browser driver without persisting tokens', () => {
  const wrapper = fs.readFileSync(path.join(__dirname, 'static/js/workbuddy-auth.js'), 'utf8');
  const driver = fs.readFileSync(path.join(__dirname, 'static/js/device-auth.js'), 'utf8');
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/accounts.html'), 'utf8');
  assert.match(wrapper, /DeviceAuthLogin\.create\(/);
  assert.match(wrapper, /api\/workbuddy\/login/);
  assert.match(wrapper, /workbuddy_login_v1/);
  assert.match(wrapper, /workbuddyLoginLink/);
  assert.ok(page.indexOf('/js/device-auth.js') < page.indexOf('/js/workbuddy-auth.js'), 'load the driver first');
  assert.doesNotMatch(wrapper, /localStorage\.setItem\([^)]*token/i);
  assert.doesNotMatch(wrapper, /document\.cookie/);
  assert.match(driver, /window\.open\(/);
  assert.match(driver, /setLink\(authURL\)/);
});

test('WorkBuddy keeps operator-facing server errors and original terminal messages', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/workbuddy-auth.js'), 'utf8');
  for (const code of [
    'upstream_unreachable', 'upstream_rejected', 'origin_mismatch',
    'insecure_origin', 'store_unavailable', 'too_many_logins',
    'unsupported_media_type', 'transaction_failed',
  ]) {
    assert.match(source, new RegExp(`${code}:`), `no message for ${code}`);
  }
  assert.match(source, /complete: 'WorkBuddy 官方登录完成，账号已保存'/);
  assert.match(source, /failed: 'WorkBuddy 授权失败，请重新发起登录。'/);
  assert.match(source, /expired: 'WorkBuddy 授权已超时，请重新发起登录。'/);
  assert.match(fs.readFileSync(path.join(__dirname, 'static/js/device-auth.js'), 'utf8'), /readErrorPayload\(response\)/);
});

test('Qoder is OAuth-only in the modal: no manual credential field and no PAT entry', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.QoderLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, toggle() {}, contains() { return true; } };
  node('accountId').value = '';
  node('enabled').checked = true;
  context.filterByPlatform('qoder');
  context.openModal();

  assert.equal(node('accountType').value, 'qoder', 'modal type');
  assert.equal(node('accountTypeDisplay').value, 'Qoder', 'modal type label');
  assert.equal(node('qoderLoginGroup').hidden, false, 'the qoder login must be visible');
  assert.equal(node('workbuddyLoginGroup').hidden, true, 'the workbuddy login must stay hidden');
  // The channel is OAuth-only: there must be no credential field to type a PAT
  // into, and no submit button for a new account.
  assert.equal(node('#accountForm button[type="submit"]').hidden, true, 'new Qoder account uses official login');
});

test('the qoder-auth module drives the server flow without carrying credentials', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/qoder-auth.js'), 'utf8');
  assert.match(source, /api\/qoder\/login/);
  assert.match(source, /DeviceAuthLogin/);
  assert.match(source, /qoder_login_v1/);
  // The channel is OAuth-only: no PAT field, no token persistence, no cookies.
  assert.doesNotMatch(source, /personal_token/i);
  assert.doesNotMatch(source, /localStorage\.setItem\([^)]*token/i);
  assert.doesNotMatch(source, /document\.cookie/);
});

test('the shared device-auth driver reserves the popup before its first await', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/device-auth.js'), 'utf8');
  const startIndex = source.indexOf('function start()');
  const reserveIndex = source.indexOf("window.open('about:blank'", startIndex);
  const awaitIndex = source.indexOf('await begin(', startIndex);
  assert.ok(reserveIndex > startIndex, 'popup is not reserved in start()');
  assert.ok(awaitIndex === -1 || reserveIndex < awaitIndex, 'popup must be reserved before the first await');
  // The authorization URL must be surfaced so a blocked popup or a remote
  // session can still complete the flow.
  assert.match(source, /setLink\(authURL\)/);
});

test('Grok device login keeps its provider URL allowlist isolated', async () => {
  const cases = [
    { provider: 'Grok', start: 'startGrokDeviceLogin()', endpoint: '/api/grok/device-auth', statusId: 'grokDeviceLoginStatus', buttonId: 'grokDeviceLoginButton', allowed: 'https://auth.x.ai/device?code=ok', rejected: 'https://auth.example.com/device?code=wrong' },
  ];
  for (const scenario of cases) {
    for (const [url, shouldOpen] of [[scenario.allowed, true], [scenario.rejected, false]]) {
      const { context, node } = loadUI();
      const requests = [];
      const opened = [];
      context.URL = URL;
      context.window.open = () => ({closed:false,close() {this.closed=true;}, location:{replace(target) {opened.push(target);}}});
      context.fetch = async (target, options = {}) => {
        requests.push([target, options.method || 'GET']);
        if (target === scenario.endpoint) return { ok: true, json: async () => ({ id: 'login/id', status: 'pending', user_code: 'ABCD', verification_uri_complete: url }) };
        if (target === `${scenario.endpoint}/login%2Fid`) return { ok: true, json: async () => ({ status: 'complete' }) };
        throw new Error(`unexpected request ${target}`);
      };
      context.loadAccounts = () => {};
      context.closeModal = () => {};
      context.showToast = () => {};
      await vm.runInContext(scenario.start, context);
      await new Promise((resolve) => setImmediate(resolve));
      assert.deepEqual(requests.slice(0, 2), [[scenario.endpoint, 'POST'], [`${scenario.endpoint}/login%2Fid`, 'GET']]);
      assert.deepEqual(opened, shouldOpen ? [url] : [], `${scenario.provider}: cross-provider URL must not open`);
      assert.equal(node(scenario.buttonId).disabled, false);
      assert.match(node(scenario.statusId).textContent, /授权完成/);
    }
  }
});

test('the shared device login lifecycle preserves cancellation and ignores stale poll results', async () => {
  for (const scenario of [
    { start: 'startGrokDeviceLogin()', stop: 'stopGrokDeviceLogin(true)', endpoint: '/api/grok/device-auth', url: 'https://auth.x.ai/device' },
  ]) {
    const { context } = loadUI();
    const requests = [];
    let releasePoll;
    context.URL = URL;
    context.window.open = () => {};
    context.fetch = async (target, options = {}) => {
      requests.push([target, options.method || 'GET']);
      if (target === scenario.endpoint) return { ok: true, json: async () => ({ id: 'cancel-me', status: 'pending', verification_uri: scenario.url }) };
      if (options.method === 'DELETE') return { ok: true };
      return new Promise((resolve) => { releasePoll = () => resolve({ ok: true, json: async () => ({ status: 'complete' }) }); });
    };
    await vm.runInContext(scenario.start, context);
    await new Promise((resolve) => setImmediate(resolve));
    vm.runInContext(scenario.stop, context);
    assert.deepEqual(requests.at(-1), [`${scenario.endpoint}/cancel-me`, 'DELETE']);
    releasePoll();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(requests.filter(([, method]) => method === 'DELETE').length, 1);
  }
});

test('a rejected credential shows the reason, not the raw error envelope', () => {
  const { context } = loadUI();
  // The server answers rejected credentials with the standard admin envelope.
  const envelope = JSON.stringify({
    error: { type: 'authentication_error', message: 'account was rejected by the upstream and was not saved: 401 unauthorized' },
    type: 'error',
  });
  assert.equal(
    vm.runInContext(`extractAdminErrorDetail(${JSON.stringify(envelope)})`, context),
    'account was rejected by the upstream and was not saved: 401 unauthorized',
    'JSON envelope must be unwrapped to its message',
  );
  assert.equal(
    vm.runInContext('extractAdminErrorDetail("missing sso token")', context),
    'missing sso token',
    'a plain-text body is already the detail',
  );
  assert.equal(vm.runInContext('extractAdminErrorDetail("")', context), '', 'an empty body stays empty');
});

test('the session fingerprint never exposes the credential', () => {
  const { context } = loadUI();
  const rendered = vm.runInContext(
    "formatTokenDisplay({ account_type: 'grok', credential_type: 'oauth', refresh_token: 'secret-session-token', has_credential: true })",
    context,
  );
  assert.ok(!rendered.includes('secret-session-token'), 'the raw session token leaked into the table');
});

// ---------------------------------------------------------------------------
// Qoder account row: 等级 / 配额 / 状态 must all render.
//
// A Qoder "Pro Trial" fixture has a daily credit window, and the
// allowance is reported by the channel's quota read. Before these cases the row
// was blank because the channel was not in any of the renderer's branches: 等级
// fell through to a generic badge, 配额 read the generic usage columns (which are
// 0 for this channel) and 状态 had no label mapping.
// ---------------------------------------------------------------------------

function qoderTrialAccount(overrides = {}) {
  return {
    account_type: 'qoder',
    enabled: true,
    name: 'trial-user@example.com',
    email: 'trial-user@example.com',
    qoder_user_id: '00000000-0000-4000-8000-000000000001',
    qoder_access_token: 'eyJhbGciOi.access.token',
    has_credential: true,
    quota_supported: true,
    quota_plan: 'Pro Trial',
    quota_unit: 'credits',
    quota_limit: 300,
    quota_remaining: 300,
    quota_used: 0,
    quota_exhausted: false,
    quota_upgrade_url: 'https://qoder.com/pricing?client=qoder',
    quota_reset_at: '2026-09-14T19:47:13Z',
    ...overrides,
  };
}

test('a Qoder trial account renders 等级, 配额 and 状态 instead of blank cells', () => {
  const { context } = loadUI();
  const account = qoderTrialAccount();

  // 等级 comes from the plan tier the channel reports.
  const tier = context.buildSubscriptionMarkup(account);
  assert.match(tier, /Pro Trial/, `tier markup was ${tier}`);

  // 配额 shows the remaining share of the window, not the generic usage columns.
  const quota = context.getQuotaStats(account);
  assert.equal(quota.remaining, 300, 'the remaining allowance must be read from the quota fields');
  assert.equal(quota.limit, 300);
  const quotaMarkup = context.buildQuotaMarkup(account);
  assert.equal(quota.used, 0);
  assert.equal(quota.pctRemaining, 100);
  assert.equal(strip(quotaMarkup), '300 / 300 (剩余)');

  // 状态 must be a real label, not the fallback.
  const badge = context.statusBadge(account);
  assert.equal(badge.text, '正常');
});

test('a Qoder account with no quota snapshot says so instead of showing zero', () => {
  const { context } = loadUI();
  const account = qoderTrialAccount({
    quota_supported: false,
    quota_plan: '',
    quota_limit: 0,
    quota_remaining: 0,
  });

  const quota = context.getQuotaStats(account);
  assert.equal(quota.unknown, true, 'a missing snapshot must be reported as unknown, not as 0');
  assert.match(context.buildQuotaMarkup(account), /未知/);
  assert.match(context.buildSubscriptionMarkup(account), /未同步|未知/);
});

test('an exhausted Qoder account shows the reset and the upgrade link, not an error', () => {
  const { context } = loadUI();
  const account = qoderTrialAccount({
    status_code: '402',
    quota_remaining: 0,
    quota_used: 300,
    quota_exhausted: true,
  });

  // 402 is a quota state: the row must stay readable and say what to do.
  const badge = context.statusBadge(account);
  assert.equal(badge.text, '额度不足');
  const quotaMarkup = context.buildQuotaMarkup(account);
  assert.equal(strip(quotaMarkup), '0 / 300 (剩余)');
  const quota = context.getQuotaStats(account);
  assert.equal(quota.remaining, 0);
  assert.equal(quota.limit, 300);
  assert.equal(quota.used, 300);
  assert.equal(quota.pctRemaining, 0);
  assert.equal(quota.resetAt, account.quota_reset_at);
  assert.equal(quota.upgradeUrl, account.quota_upgrade_url);
});

test('the Qoder identity column leads with the signed-in address', () => {
  const { context } = loadUI();
  const account = qoderTrialAccount();
  assert.equal(context.accountIdentityPrimary(account), 'trial-user@example.com');
  // The device credential is never shown, not even truncated.
  const token = context.formatTokenDisplay(account);
  assert.doesNotMatch(token, /eyJhbGciOi/);
});

test('Grok identity shows the email together with the login method', () => {
  const { context } = loadUI();
  assert.equal(context.accountIdentityPrimary({
    account_type: 'grok',
    credential_type: 'oauth',
    email: 'oauth@example.com',
    name: 'grok-device-login',
    has_credential: true,
  }), 'oauth@example.com · Build OAuth');
});

test('the Qoder quota tooltip carries the plan, the reset and the upgrade link', () => {
  const { context, node } = loadUI();
  const account = qoderTrialAccount({ id: 901, status_code: '402', quota_remaining: 0, quota_used: 300, quota_exhausted: true });
  vm.runInContext(`accounts = [${JSON.stringify(account)}]; currentPlatform = 'qoder'; renderAccounts();`, context);
  const row = node('accountsList').querySelectorAll('tbody tr')[0];
  const cell = row.children.find((candidate) => candidate.className === 'col-quota');
  assert.equal(strip(cell.innerHTML), '0 / 300 (剩余)');
  assert.equal(cell.title, [
    '计划: Pro Trial', '单位: credits', '口径: 当前窗口剩余 / 窗口额度',
    '该账号额度已用尽，窗口重置后自动恢复',
    `重置: ${new Date(account.quota_reset_at).toLocaleString()}`,
    `升级: ${account.quota_upgrade_url}`,
  ].join(' · '));
});

test('an exhausted Qoder quota is reported as a quota state, not as a fault', () => {
  const { context } = loadUI();
  const exhausted = qoderTrialAccount({
    status_code: '402',
    quota_remaining: 0,
    quota_exhausted: true,
  });
  assert.equal(context.getQuotaStats(exhausted).exhausted, true);
  assert.equal(context.getQuotaStats(qoderTrialAccount()).exhausted, false);
  // 402 must not turn the account into an error row: the credential is fine.
  assert.equal(context.isSidebarAccountAbnormal({ ...exhausted, has_credential: true, status_code: '' }), false);
});

// Anonymized regression fixtures retain the server payload shape and quota
// values without committing an operator's account address or identifier.
test('anonymized Qoder payloads render exact 等级 / 配额 / 状态', () => {
  const fixtures = [
    {
      id: 901,
      account_type: 'qoder',
      enabled: true,
      email: 'trial-user@example.com',
      qoder_user_id: '00000000-0000-4000-8000-000000000001',
      qoder_access_token: 'access-token-placeholder',
      has_credential: true,
      usage_limit: 300,
      usage_current: 300,
      quota_supported: true,
      quota_plan: 'Pro Trial',
      quota_unit: 'credits',
      quota_limit: 300,
      quota_remaining: 300,
      quota_used: 0,
      quota_exhausted: false,
      quota_upgrade_url: 'https://qoder.com/pricing?client=qoder',
      quota_reset_at: '2026-09-14T19:47:13Z',
      expect: { tier: 'Pro Trial', quota: '300 / 300 (剩余)', remaining: 300, limit: 300, used: 0, pctRemaining: 100, status: '正常' },
    },
    {
      id: 902,
      account_type: 'qoder',
      enabled: true,
      email: 'free-user@example.net',
      qoder_user_id: '00000000-0000-4000-8000-000000000002',
      qoder_access_token: 'access-token-placeholder',
      has_credential: true,
      status_code: '402',
      usage_limit: 0,
      usage_current: 0,
      quota_supported: true,
      quota_plan: 'Free',
      quota_unit: 'credits',
      quota_limit: 0,
      quota_remaining: 0,
      quota_used: 0,
      quota_exhausted: true,
      quota_upgrade_url: 'https://qoder.com/pricing?client=qoder',
      quota_reset_at: '2026-09-14T02:39:48Z',
      expect: { tier: 'Free', quota: '0 / 0 (剩余)', remaining: 0, limit: 0, used: 0, pctRemaining: 0, status: '额度不足' },
    },
  ];

  const { context } = loadUI();
  for (const fixture of fixtures) {
    const { expect, ...account } = fixture;
    const tier = context.buildSubscriptionMarkup(account);
    const quota = context.buildQuotaMarkup(account);
    const status = context.statusBadge(account);

    assert.equal(strip(tier), expect.tier, `id ${account.id}: 等级`);
    assert.doesNotMatch(tier, />-</, `id ${account.id}: 等级 fell through to the empty placeholder`);
    assert.equal(strip(quota), expect.quota, `id ${account.id}: 配额`);
    const stats = context.getQuotaStats(account);
    for (const field of ['remaining', 'limit', 'used', 'pctRemaining']) {
      assert.equal(stats[field], expect[field], `id ${account.id}: ${field}`);
    }
    assert.doesNotMatch(quota, /未知/, `id ${account.id}: 配额 claimed to be unknown while a snapshot existed`);
    assert.equal(status.text, expect.status, `id ${account.id}: 状态 was ${status.text}`);
  }
});

// ---------------------------------------------------------------------------
// Channel enumeration drift.
//
// Channel forms and badges consume the shared provider registry.

const CHANNEL_SELECT_TEMPLATES = [
  'templates/components/modals/model-modal.html',
];

const CHANNEL_KEYS = ['workbuddy', 'qoder', 'cline', 'grok'];

test('channel forms are populated from the backend provider registry', () => {
  const template = fs.readFileSync(path.join(__dirname, CHANNEL_SELECT_TEMPLATES[0]), 'utf8');
  const models = fs.readFileSync(path.join(__dirname, 'static/js/models.js'), 'utf8');
  assert.match(template, /id="modelChannel"/);
  assert.match(models, /OrchidsProviderRegistry\?\.channels/);
  for (const key of CHANNEL_KEYS) assert.doesNotMatch(template, new RegExp(`<option value="${key}">`, 'i'));
});

test('every channel has a badge style, for channel labels', () => {
  const css = fs.readFileSync(path.join(__dirname, 'static/css/main.css'), 'utf8');
  for (const key of CHANNEL_KEYS) {
    assert.match(css, new RegExp(`\\.badge-${key}\\b`), `no CSS rule for .badge-${key}`);
  }
});

// A Grok Build Free account has no plan name from the identity endpoint, so the
// server records "unknown" — and the tier column showed 未知 while the quota
// column already knew the account was Free. The server now emits "free" once its
// own Free inference fires, and the badge must render that as a tier.
test('Grok OAuth API payloads render the Free tier and quota provenance', () => {
  const { context } = loadUI();
  const strip = (html) => String(html).replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();

  const free = {
    id: 142,
    account_type: 'grok',
    credential_type: 'oauth',
    grok_provider: 'build',
    subscription: 'free',
    enabled: true,
    quota_supported: true,
    quota_type: 'free',
    quota_source: 'upstreamExhaustion',
    quota_confidence: 'confirmed',
    quota_limit: 500000,
    quota_used: 500000,
    quota_unit: 'tokens',
    quota_window_hours: 24,
  };
  assert.equal(strip(context.buildSubscriptionMarkup(free)), 'Free');
  const confirmedQuota = strip(context.buildQuotaMarkup(free));
  assert.match(confirmedQuota, /500[,.]?000/);
  assert.match(confirmedQuota, /Free 实报/);
  assert.doesNotMatch(confirmedQuota, /^≈/);

  const estimated = { ...free, subscription: 'free', quota_source: 'billingProfile', quota_confidence: 'estimated', quota_limit_known: false };
  assert.equal(strip(context.buildSubscriptionMarkup(estimated)), 'Free');
  const estimatedQuota = strip(context.buildQuotaMarkup(estimated));
  assert.match(estimatedQuota, /^≈/);
  assert.match(estimatedQuota, /Free 估算/);

  // An account the server could not characterise stays honest.
  const unknown = { ...free, subscription: 'unknown' };
  assert.equal(strip(context.buildSubscriptionMarkup(unknown)), '未知');
  // A paid plan is never relabelled.
  assert.equal(strip(context.buildSubscriptionMarkup({ ...free, subscription: 'XPremium' })), 'X Premium');
});

test('账号管理 and 运维总览 count the same 异常 accounts from one predicate', () => {
  const { context } = loadUI();

  const rows = [
    // A drained allowance on every channel is a business limit, not a fault.
    { id: 1, account_type: 'cline', enabled: true, has_credential: true, status_code: 'cline_quota_exhausted', quota_supported: true, quota_limit: 1000, quota_remaining: 0, quota_confidence: 'confirmed' },
    { id: 2, account_type: 'workbuddy', enabled: true, has_credential: true, status_code: 'workbuddy_quota_exhausted', quota_supported: true, quota_limit: 350, quota_remaining: 0, quota_confidence: 'confirmed' },
    { id: 3, account_type: 'grok', enabled: true, has_credential: true, status_code: '', quota_supported: true, quota_limit: 500000, quota_remaining: 0, quota_confidence: 'confirmed' },
    // An INFERRED window that reads zero is still drained: this row used to be
    // 异常 on 账号管理 and 正常 everywhere else.
    { id: 4, account_type: 'grok', enabled: true, has_credential: true, status_code: '', quota_supported: true, quota_limit: 500000, quota_remaining: 0, quota_confidence: 'estimated' },
    // Genuine faults must still be counted.
    { id: 6, account_type: 'grok', enabled: false, has_credential: true, status_code: '' },
    { id: 7, account_type: 'qoder', enabled: true, has_credential: true, status_code: '401' },
    { id: 8, account_type: 'cline', enabled: true, has_credential: false, status_code: '' },
    // A healthy account with an untouched window stays normal.
    { id: 9, account_type: 'grok', enabled: true, has_credential: true, status_code: '', quota_supported: true, quota_limit: 500000, quota_remaining: 471863, quota_confidence: 'estimated' },
  ];

  // common.js owns the number on every page except 账号管理 …
  const sidebar = context.computeSidebarAccountStats(rows);
  assert.equal(sidebar.abnormal, 3, 'disabled, 401 and missing-credential rows are the only faults');
  assert.equal(sidebar.total, 8);

  // … and 账号管理 must land on the same number now that updateStats() defers to
  // the shared predicate instead of computing its own verdict.
  assert.equal(rows.filter(context.isSidebarAccountAbnormal).length, sidebar.abnormal);

  const source = fs.readFileSync(path.join(__dirname, 'static/js/accounts.js'), 'utf8');
  assert.match(source, /const stats = computeSidebarAccountStats\(accounts\);/);

  // The row badge agrees: a drained allowance is an orange business limit, not a
  // red fault, and the 清空异常 button therefore leaves those rows alone.
  for (const row of rows.filter((candidate) => candidate.quota_remaining === 0 && candidate.enabled)) {
    const verdict = context.evaluateAccountStatus(row);
    assert.equal(verdict.normal, true, `account ${row.id} badge`);
    assert.equal(verdict.text, '额度不足', `account ${row.id} badge text`);
    assert.equal(verdict.quotaOnly, true, `account ${row.id} quotaOnly`);
  }
  assert.equal(context.isAccountAbnormal(rows[3]), false);

  // The status comes from the server's explicit credential verdict, even when
  // the provider registry is unavailable during script startup.
  const registry = context.window.OrchidsProviderRegistry;
  context.window.OrchidsProviderRegistry = undefined;
  try {
    assert.equal(rows.filter(context.isSidebarAccountAbnormal).length, sidebar.abnormal, 'verdict must not depend on a loaded registry');
  } finally {
    context.window.OrchidsProviderRegistry = registry;
  }
});

test('credential verdict drives sidebar counters and channel-specific row badges', () => {
  const { context } = loadUI();
  for (const [type, missingText, missingTip] of [
    ['grok', '待登录', /Build OAuth/],
    ['workbuddy', '待补全', /WorkBuddy/],
    ['qoder', '待补全', /Qoder/],
    ['cline', '待补全', /Cline/],
  ]) {
    const account = { id: 101, account_type: type, enabled: true, status_code: '', has_credential: true };
    assert.equal(context.isSidebarAccountAbnormal(account), false, `${type}: credentialed sidebar`);
    assert.equal(context.evaluateAccountStatus(account).text, '正常', `${type}: credentialed badge`);
    const missing = { ...account, has_credential: false,
      token: 'obsolete-token', credential_type: 'oauth', workbuddy_access_token: 'visible-token',
      qoder_access_token: 'visible-token', cline_access_token: 'visible-token' };
    assert.equal(context.isSidebarAccountAbnormal(missing), true, `${type}: missing credential sidebar`);
    const badge = context.evaluateAccountStatus(missing);
    assert.equal(badge.text, missingText, `${type}: missing credential badge`);
    assert.match(badge.tip, missingTip);
    assert.equal(badge.normal, false);
  }
});
