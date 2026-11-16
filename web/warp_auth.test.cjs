// Run with: node --test web/warp_auth.test.cjs
// All HTTP responses are local mocks. These tests never contact Warp.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');
const asset = (name) => fs.readFileSync(path.join(__dirname, 'static/js', name), 'utf8');
const tick = () => new Promise((resolve) => setImmediate(resolve));

function browser({ verificationURL = 'https://app.warp.dev/login', userCode = '', storedSession = null,
  popupBlocked = false, beginError = null, pollResponse = null, expiresAt = '', secure = true } = {}) {
  const elements = new Map();
  const storage = new Map(storedSession ? [['warp_login_v1', JSON.stringify(storedSession)]] : []);
  const requests = [];
  const toasts = [];
  const timers = new Map();
  const events = {};
  let nextTimer = 0;
  let pollResult = { status: 'pending' };
  let modalCloses = 0;
  let accountLoads = 0;
  let popupOpens = 0;
  const element = (id) => {
    if (!elements.has(id)) {
      const classes = new Set();
      elements.set(id, {
        id, hidden: true, checked: true, disabled: false, textContent: '', value: '', dataset: {},
        classList: { toggle(name, on) { if (on) classes.add(name); else classes.delete(name); }, remove(...names) { names.forEach(name => classes.delete(name)); } },
        removeAttribute(name) { delete this[name]; }, reset() {},
      });
    }
    return elements.get(id);
  };
  for (const id of ['warpLoginStatus', 'warpLoginButton', 'warpLoginLink', 'warpLoginLinkText', 'warpLoginCode', 'enabled']) element(id);
  const popup = { closed: false, close() { this.closed = true; }, location: { replace(url) { this.href = url; } } };
  const context = vm.createContext({
    document: {
      getElementById: (id) => elements.get(id) || null,
      addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
      createElement: () => ({ set textContent(value) { this.innerHTML = String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); } }),
    },
    window: {
      isSecureContext: secure,
      open() { popupOpens++; popup.closed = false; return popupBlocked ? null : popup; },
      addEventListener(name, fn) { (events[name] ||= []).push(fn); },
      localStorage: {
        getItem: (key) => storage.get(key) || null,
        setItem: (key, value) => storage.set(key, value),
        removeItem: (key) => storage.delete(key),
      },
    },
    fetch: async (url, options = {}) => {
      requests.push({ url, options });
      assert.ok(url.startsWith('/api/'), 'mock must only receive local API requests');
      if (options.method === 'POST') return beginError
        ? { ok: false, status: 502, text: async () => JSON.stringify({ code: beginError }) }
        : { ok: true, json: async () => ({ id: 'warp-login-1', verification_uri_complete: verificationURL,
          user_code: userCode, expires_at: expiresAt }) };
      if (options.method === 'DELETE') return { ok: true };
      return pollResponse ? pollResponse() : { ok: true, status: 200, json: async () => pollResult };
    },
    setInterval(fn) { const id = ++nextTimer; timers.set(id, fn); return id; },
    clearInterval(id) { timers.delete(id); },
    AbortController,
    showToast: (...args) => toasts.push(args),
    closeModal() { modalCloses++; }, loadAccounts() { accountLoads++; },
  });
  for (const script of ['device-auth.js', 'warp-auth.js']) vm.runInContext(asset(script), context);
  return { context, element, elements, storage, requests, toasts, timers, popup,
    setPollResult(value) { pollResult = value; },
    runInterval() { [...timers.values()].forEach(fn => fn()); },
    emit(name) { (events[name] || []).forEach(fn => fn()); },
    counts: () => ({ modalCloses, accountLoads, popupOpens }),
  };
}

test('Warp uses the generic local login routes, enabled state, public link and device code', async () => {
  const ui = browser({ userCode: 'WARP-TEST', popupBlocked: true });
  ui.element('enabled').checked = false;
  ui.context.WarpLogin.start();
  ui.context.WarpLogin.start();
  await tick();
  assert.equal(ui.requests[0].url, '/api/warp/login');
  assert.equal(ui.requests[0].options.method, 'POST');
  assert.equal(ui.requests[0].options.credentials, 'same-origin');
  assert.deepEqual(JSON.parse(ui.requests[0].options.body), { enabled: false });
  assert.equal(ui.requests[1].url, '/api/warp/login/warp-login-1');
  assert.equal(ui.requests[1].options.credentials, 'same-origin');
  assert.equal(ui.counts().popupOpens, 1, 'duplicate clicks must not start another transaction');
  assert.equal(ui.element('warpLoginButton').disabled, true);
  assert.equal(ui.element('warpLoginLink').hidden, false);
  assert.equal(ui.element('warpLoginLinkText').href, 'https://app.warp.dev/login');
  assert.equal(ui.element('warpLoginCode').textContent, 'WARP-TEST');
  assert.equal(ui.element('warpLoginCode').hidden, false);
  const saved = JSON.parse(ui.storage.get('warp_login_v1'));
  assert.deepEqual(Object.keys(saved).sort(), ['expiresAt', 'loginId']);
  assert.equal(saved.loginId, 'warp-login-1');
  assert.ok(Number.isFinite(saved.expiresAt));
  ui.context.WarpLogin.stop();
  await tick();
  assert.equal(ui.requests.at(-1).url, '/api/warp/login/warp-login-1');
  assert.equal(ui.requests.at(-1).options.method, 'DELETE');
  assert.equal(ui.storage.size, 0);
  assert.equal(ui.timers.size, 0);
  assert.equal(ui.popup.closed, false, 'a blocked popup was never opened');
  assert.equal(ui.element('warpLoginButton').disabled, false);
  assert.equal(ui.element('warpLoginCode').textContent, '');
  assert.equal(ui.element('warpLoginCode').hidden, true);
  assert.equal(ui.element('warpLoginLink').hidden, true);
  assert.equal(ui.element('warpLoginLinkText').href, undefined);
  assert.equal(ui.element('warpLoginStatus').hidden, true);
});

for (const origin of ['https://app.warp.dev', 'https://warp.dev']) {
  test(`Warp navigates to the exact official origin ${origin}`, async () => {
    const url = `${origin}/authorize?transaction=test`;
    const ui = browser({ verificationURL: url });
    ui.context.WarpLogin.start();
    await tick();
    assert.equal(ui.popup.location.href, url);
    assert.equal(ui.element('warpLoginLinkText').href, url);
    ui.context.WarpLogin.stop();
  });
}

for (const url of ['https://warp.dev.evil.example/login', 'https://app.warp.dev.evil.example/login',
  'https://evil.example/warp.dev', 'http://app.warp.dev/login', 'https://app.warp.dev:444/login',
  'https://warp.dev@evil.example/login', 'https://user@app.warp.dev/login', 'javascript:alert(1)', '/relative']) {
  test(`Warp never opens or exposes an unapproved authorization URL: ${url}`, async () => {
    const ui = browser({ verificationURL: url });
    ui.context.WarpLogin.start();
    await tick();
    assert.equal(ui.popup.location.href, undefined);
    assert.equal(ui.element('warpLoginLinkText').href, undefined);
    assert.equal(ui.element('warpLoginLink').hidden, true);
    ui.context.WarpLogin.stop();
  });
}

test('Warp completes a transaction even when the official page severs its opener', async () => {
  const ui = browser();
  ui.context.WarpLogin.start();
  await tick();
  ui.popup.closed = true;
  ui.runInterval();
  await tick();
  assert.equal(ui.requests.some(request => request.options.method === 'DELETE'), false);
  ui.setPollResult({ status: 'complete' });
  ui.runInterval();
  await tick();
  assert.equal(ui.toasts.at(-1)[1], 'success');
  assert.deepEqual(ui.counts(), { modalCloses: 1, accountLoads: 1, popupOpens: 1 });
  assert.equal(ui.timers.size, 0);
  assert.equal(ui.storage.size, 0);
});

test('Warp pagehide retains only resumable metadata and resume polls without another POST', async () => {
  const ui = browser();
  ui.context.WarpLogin.start();
  await tick();
  ui.emit('pagehide');
  assert.equal(ui.timers.size, 0);
  assert.equal(ui.storage.size, 1);
  assert.equal(ui.requests.some(request => request.options.method === 'DELETE'), false);
  const session = JSON.parse(ui.storage.get('warp_login_v1'));
  const resumed = browser({ storedSession: session });
  resumed.setPollResult({ status: 'complete' });
  resumed.context.WarpLogin.start();
  await tick();
  assert.deepEqual(resumed.requests.map(request => request.url), ['/api/warp/login/warp-login-1']);
  assert.equal(resumed.toasts.at(-1)[1], 'success');
  assert.equal(resumed.storage.size, 0);
});

test('Warp escapes resumed login IDs and replaces expired metadata with a fresh transaction', async () => {
  const resumed = browser({ storedSession: { loginId: 'id/with?query', expiresAt: Date.now() + 60000 } });
  resumed.context.WarpLogin.start();
  await tick();
  assert.equal(resumed.requests[0].url, '/api/warp/login/id%2Fwith%3Fquery');
  resumed.context.WarpLogin.stop();
  const expired = browser({ storedSession: { loginId: 'stale', expiresAt: 1 } });
  expired.context.WarpLogin.start();
  await tick();
  assert.equal(expired.requests[0].options.method, 'POST');
  expired.context.WarpLogin.stop();
});

test('Warp reports failures and expiry and cleans the transaction without saving an account', async () => {
  for (const status of ['failed', 'expired']) {
    const ui = browser();
    ui.setPollResult({ status });
    ui.context.WarpLogin.start();
    await tick();
    assert.equal(ui.toasts.at(-1)[1], 'error');
    assert.equal(ui.counts().accountLoads, 0);
    assert.equal(ui.requests.at(-1).options.method, 'DELETE');
    assert.equal(ui.storage.size, 0);
    assert.equal(ui.element('warpLoginButton').disabled, false);
  }
  const ui = browser({ expiresAt: '2000-01-01T00:00:00Z' });
  ui.context.WarpLogin.start();
  await tick();
  ui.runInterval();
  await tick();
  assert.match(ui.toasts.at(-1)[0], /Warp authorization expired/);
  assert.equal(ui.timers.size, 0);
});

test('Warp reports server login errors and refuses insecure browser contexts', async () => {
  const ui = browser({ beginError: 'upstream_unreachable' });
  ui.context.WarpLogin.start();
  await tick();
  assert.match(ui.toasts.at(-1)[0], /official Warp login service/);
  assert.equal(ui.element('warpLoginButton').disabled, false);
  const insecure = browser({ secure: false });
  insecure.context.WarpLogin.start();
  assert.equal(insecure.requests.length, 0);
  assert.equal(insecure.counts().popupOpens, 0);
});

test('Warp ignores a poll response arriving after stop and reset clears stale code/link', async () => {
  let resolvePoll;
  const ui = browser({ pollResponse: () => new Promise(resolve => { resolvePoll = resolve; }) });
  ui.context.WarpLogin.start();
  await tick();
  const signal = ui.requests.find(request => !request.options.method).options.signal;
  ui.context.WarpLogin.stop();
  assert.equal(signal.aborted, true);
  resolvePoll({ ok: true, json: async () => ({ status: 'complete' }) });
  await tick();
  assert.equal(ui.toasts.length, 0);
  assert.equal(ui.counts().accountLoads, 0);
  ui.element('warpLoginCode').textContent = 'stale-code';
  ui.element('warpLoginLink').hidden = false;
  ui.element('warpLoginLinkText').href = 'https://warp.dev/login';
  ui.context.WarpLogin.reset();
  assert.equal(ui.element('warpLoginCode').textContent, '');
  assert.equal(ui.element('warpLoginLinkText').href, undefined);
  assert.equal(ui.element('warpLoginLink').hidden, true);
});

function accountsUI(ui = browser()) {
  for (const script of ['common.js', 'provider-registry.js', 'accounts.js']) vm.runInContext(asset(script), ui.context);
  return ui;
}

test('Warp account modal uses only its official driver and cancels/resets on open and close', () => {
  const ui = accountsUI();
  const { context, element } = ui;
  for (const id of ['accountType', 'accountTypeDisplay', 'accountId', 'accountForm', 'modalTitle']) element(id);
  const calls = [];
  context.WarpLogin = { start() { calls.push('start'); }, stop() { calls.push('stop'); }, reset() { calls.push('reset'); } };
  context.ConsoleUI.modal = () => {};
  const warpGroup = element('warpLoginGroup'); warpGroup.dataset.loginGroup = 'warp';
  const clineGroup = element('clineLoginGroup'); clineGroup.dataset.loginGroup = 'cline';
  const save = element('save');
  context.document.querySelectorAll = (selector) => selector === '#accountForm [data-login-group]' ? [warpGroup, clineGroup] : [];
  context.document.querySelector = (selector) => selector === '#accountForm button[type="submit"]' ? save : null;
  // The generated registry is owned by the lead. The modal contract does not
  // depend on loading its eventual new channel metadata in this unit test.
  const registry = context.window.OrchidsProviderRegistry;
  context.window.OrchidsProviderRegistry = { ...registry, label: key => key === 'warp' ? 'Warp' : registry.label(key) };
  vm.runInContext("currentPlatform = 'warp'", context);
  context.openModal();
  assert.deepEqual(calls, ['stop', 'reset']);
  assert.equal(element('accountType').value, 'warp');
  assert.equal(element('accountTypeDisplay').value, 'Warp');
  assert.equal(warpGroup.hidden, false);
  assert.equal(clineGroup.hidden, true);
  assert.equal(save.hidden, true);
  context.startOfficialLogin('cline');
  assert.deepEqual(calls, ['stop', 'reset']);
  context.startOfficialLogin('warp');
  assert.equal(calls.at(-1), 'start');
  context.closeModal();
  assert.deepEqual(calls.slice(-2), ['stop', 'reset']);
});

test('Warp account identity, settings and quota use non-secret generic projections', async () => {
  const ui = accountsUI();
  const { context, element } = ui;
  const account = { account_type: 'warp', email: 'warp-user@example.test', has_credential: true,
    enabled: true, plan_tier: 'unknown', quota_note: 'Numeric allowance is not reported <yet>.',
    usage_limit: 99999, usage_current: 99999 };
  assert.equal(context.accountIdentityPrimary(account), 'warp-user@example.test');
  assert.equal(context.formatTokenDisplay(account), '凭证已配置');
  assert.equal(context.accountIdentityPrimary({ account_type: 'warp', has_credential: false }), '待登录');
  const quota = context.getQuotaStats(account);
  assert.equal(quota.warpPools.status, 'unknown');
  assert.equal(quota.supported, false);
  assert.equal(quota.limit, undefined, 'legacy counters must not invent a Warp allowance');
  const markup = context.buildQuotaMarkup(account);
  assert.match(markup, /未知/);
  assert.match(markup, /仅包含额度，不含额外\/按量余额/);
  assert.doesNotMatch(markup, /99999|免费|未计量/);
  assert.equal(context.subscriptionBadge(account).text, '未知');
  assert.equal(context.isSidebarAccountAbnormal(account), false);
  assert.equal(context.getQuotaStats({ ...account, quota_supported: true, quota_limit: 25, quota_remaining: 20 }).remaining, undefined);
  element('accountType').value = 'warp';
  element('accountId').value = '';
  const requestsBefore = ui.requests.length;
  context.showToast = (...args) => ui.toasts.push(args);
  await context.saveAccount({ preventDefault() {} });
  assert.equal(ui.requests.length, requestsBefore, 'manual Warp creation must not make a request');

  element('accountId').value = '17';
  element('enabled').checked = false;
  vm.runInContext("accounts = [{ id: 17, account_type: 'warp', weight: 3 }]", context);
  const saves = [];
  context.ConsoleAPI = { ...context.ConsoleAPI, checked: async (url, options) => saves.push({ url, options }) };
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  await context.saveAccount({ preventDefault() {} });
  assert.equal(saves[0].url, '/api/accounts/17');
  assert.deepEqual(JSON.parse(saves[0].options.body), { account_type: 'warp', weight: 3, enabled: false });
});

test('Warp template exposes browser link/code only and scripts load after the shared driver', () => {
  const modal = fs.readFileSync(path.join(__dirname, 'templates/components/modals/account-modal.html'), 'utf8');
  const start = modal.indexOf('id="warpLoginGroup"');
  const end = modal.indexOf('id="grokSSOImportGroup"');
  assert.ok(start >= 0 && end > start);
  const warp = modal.slice(start, end);
  for (const id of ['warpLoginButton', 'warpLoginStatus', 'warpLoginLink', 'warpLoginLinkText', 'warpLoginCode']) {
    assert.ok(warp.includes(`id="${id}"`), `missing ${id}`);
  }
  assert.match(warp, /data-login-provider="warp"/);
  assert.match(warp, /data-oauth-open="warpLoginLinkText"/);
  assert.match(warp, /data-oauth-copy="warpLoginCode"/);
  assert.match(warp, /app\.warp\.dev or warp\.dev/);
  assert.doesNotMatch(warp, /<(?:input|textarea|select)\b|type="file"/);
  const html = fs.readFileSync(path.join(__dirname, 'templates/pages/accounts.html'), 'utf8');
  assert.ok(html.indexOf('device-auth.js') < html.indexOf('warp-auth.js'));
  assert.ok(html.indexOf('warp-auth.js') < html.indexOf('/js/accounts.js'));
  const css = fs.readFileSync(path.join(__dirname, 'static/css/main.css'), 'utf8');
  assert.match(css, /\.acc-tab-mark\[data-theme="purple"\][^}]*--mark:\s*var\(--accent-purple\)/);
});
