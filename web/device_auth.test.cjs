const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const scripts = ['device-auth.js', 'workbuddy-auth.js'];

function browser({ storedSession = null, popupBlocked = false, beginError = null, userCode = '' } = {}) {
  const elements = new Map();
  const storage = new Map(storedSession ? [['workbuddy_login_v1', JSON.stringify(storedSession)]] : []);
  const requests = [];
  const toasts = [];
  let pollResult = { status: 'pending' };
  let popupClosed = false;
  let intervalFn = null;
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      id, hidden: true, checked: true, disabled: false, textContent: '',
      classList: { toggle() {} },
      removeAttribute(name) { delete this[name]; },
      appendChild(child) { child.parentNode = this; },
      insertAdjacentElement(_, child) { child.parentNode = this.parentNode; elements.set(child.id, child); },
    });
    return elements.get(id);
  };
  element('workbuddyLoginStatus').parentNode = {};
  const popup = {
    get closed() { return popupClosed; },
    close() { popupClosed = true; },
    location: { replace(url) { this.href = url; } },
  };
  const context = vm.createContext({
    document: {
      getElementById: (id) => elements.get(id) || null,
      createElement: () => ({ hidden: true, appendChild(child) { elements.set(child.id, child); }, removeAttribute(name) { delete this[name]; } }),
    },
    window: {
      isSecureContext: true,
      open: () => popupBlocked ? null : popup,
      addEventListener() {},
      localStorage: {
        getItem: (key) => storage.get(key) || null,
        setItem: (key, value) => storage.set(key, value),
        removeItem: (key) => storage.delete(key),
      },
    },
    fetch: async (url, options) => {
      requests.push({ url, options });
      if (options.method === 'POST') return beginError
        ? { ok: false, text: async () => JSON.stringify({ code: beginError }) }
        : { ok: true, json: async () => ({ id: 'login-1', verification_uri_complete: 'https://www.workbuddy.ai/authorize', ...(userCode ? { user_code: userCode } : {}) }) };
      if (options.method === 'DELETE') return { ok: true };
      return { ok: true, json: async () => pollResult };
    },
    setInterval(fn) { intervalFn = fn; return 1; },
    clearInterval() {},
    AbortController,
    showToast: (...args) => toasts.push(args),
    closeModal() {},
    loadAccounts() {},
  });
  for (const script of scripts) vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', script), 'utf8'), context);
  return { context, element, storage, requests, toasts, popup, setPollResult(value) { pollResult = value; }, closePopup() { popupClosed = true; }, runInterval() { if (intervalFn) intervalFn(); } };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));

test('WorkBuddy uses the enabled flag and fallback link when a popup is blocked', async () => {
  const ui = browser({ popupBlocked: true });
  ui.element('enabled').checked = false;
  ui.context.WorkBuddyLogin.start();
  await tick();
  assert.equal(ui.requests[0].url, '/api/workbuddy/login');
  assert.equal(JSON.parse(ui.requests[0].options.body).enabled, false);
  assert.equal(ui.element('workbuddyLoginLink').hidden, false);
  assert.equal(ui.element('workbuddyLoginLinkText').href, 'https://www.workbuddy.ai/authorize');
  assert.match(ui.element('workbuddyLoginStatus').textContent, /点击上方链接/);
  ui.context.WorkBuddyLogin.stop();
  await tick();
  assert.equal(ui.requests.at(-1).options.method, 'DELETE');
  assert.equal(ui.element('workbuddyLoginLink').hidden, true);
  assert.equal(ui.storage.has('workbuddy_login_v1'), false);
});

test('WorkBuddy resumes stored sessions without POST and retains its completion message', async () => {
  const ui = browser({ storedSession: { loginId: 'retained-1', expiresAt: Date.now() + 300000 } });
  ui.setPollResult({ status: 'complete' });
  ui.context.WorkBuddyLogin.start();
  await tick();
  assert.deepEqual(ui.requests.map(({ url }) => url), ['/api/workbuddy/login/retained-1']);
  assert.deepEqual(ui.toasts, [['WorkBuddy 官方登录完成，账号已保存', 'success']]);
  assert.equal(ui.storage.has('workbuddy_login_v1'), false);
});

test('shared login errors retain provider names and specialized upstream messages', async () => {
  for (const [code, message] of [['too_many_logins', /WorkBuddy 登录过多/], ['origin_mismatch', /同源/], ['upstream_unreachable', /www\.workbuddy\.ai/]]) {
    const ui = browser({ beginError: code });
    ui.context.WorkBuddyLogin.start();
    await tick();
    assert.match(ui.toasts[0][0], message);
    assert.equal(ui.toasts[0][1], 'error');
  }
});

// Authorization pages that send Cross-Origin-Opener-Policy make popup.closed
// read true while the page is still open, and a page may close itself right
// after approval. Neither may cancel the server transaction: that used to drop
// every Grok login six seconds after it started.
test('a closed popup keeps the server login alive and still completes', async () => {
  const ui = browser();
  ui.context.WorkBuddyLogin.start();
  await tick();
  ui.closePopup();
  for (let i = 0; i < 5; i++) {
    ui.runInterval();
    await tick();
  }
  assert.equal(ui.requests.some(({ options }) => options.method === 'DELETE'), false);
  assert.match(ui.element('workbuddyLoginStatus').textContent, /授权窗口已关闭或已脱离本页/);
  ui.setPollResult({ status: 'complete' });
  ui.runInterval();
  await tick();
  assert.equal(ui.toasts.at(-1)[1], 'success');
});

// The login modal shows the device code as its own large, copyable field
// (fooStatus → fooCode); it disappears again when the login ends.
test('a device code is shown in its own field and cleared on stop', async () => {
  const ui = browser({ userCode: 'CLBJ-WTBB' });
  ui.element('workbuddyLoginCode');
  ui.context.WorkBuddyLogin.start();
  await tick();
  assert.equal(ui.element('workbuddyLoginCode').textContent, 'CLBJ-WTBB');
  assert.equal(ui.element('workbuddyLoginCode').hidden, false);
  assert.match(ui.element('workbuddyLoginStatus').textContent, /CLBJ-WTBB/);
  ui.context.WorkBuddyLogin.stop();
  await tick();
  assert.equal(ui.element('workbuddyLoginCode').hidden, true);
  assert.equal(ui.element('workbuddyLoginCode').textContent, '');
});
