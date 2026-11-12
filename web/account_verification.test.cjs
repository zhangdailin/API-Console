const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function harness({ challenge, getError } = {}) {
  const calls = [];
  const toasts = [];
  const posted = [];
  const listeners = [];
  const frameWindow = { postMessage: (data, origin) => posted.push({ data, origin }) };
  const frame = { contentWindow: frameWindow, srcdoc: '', removeAttribute(name) { delete this[name]; } };
  const status = { textContent: '', dataset: {} };
  const modals = [];
  const context = vm.createContext({
    window: {
      addEventListener: (type, fn) => listeners.push(fn),
      removeEventListener: (type, fn) => { const i = listeners.indexOf(fn); if (i >= 0) listeners.splice(i, 1); },
    },
    document: { getElementById: (id) => ({ verificationFrame: frame, verificationStatus: status })[id] || null },
    ConsoleUI: { modal: (id, open) => modals.push([id, open]) },
    ConsoleAPI: {
      jsonOptions: (method, value) => ({ method, body: JSON.stringify(value) }),
      json: async (url, options) => {
        calls.push([url, options && options.method || 'GET']);
        if (!options && getError) throw new Error(getError);
        return options ? {} : challenge;
      },
    },
    showToast: (text, tone) => toasts.push([text, tone]),
    loadAccounts: () => calls.push(['reload']),
  });
  context.globalThis = context;
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/account-verification.js'), 'utf8'), context);
  const dispatch = (source, data) => listeners.slice().forEach(fn => fn({ source, data }));
  return { api: context.AccountVerification, calls, toasts, posted, frame, frameWindow, status, modals, dispatch, listeners };
}

const challenge = { sdk: 'bytedance_captcha', params: { info: { did: '75', fp: 'verify_x' }, verify_data: '{"type":"verify"}' } };

test('the captcha SDK runs only in a sandboxed frame without same-origin access', () => {
  const html = fs.readFileSync(path.join(__dirname, 'templates/pages/accounts.html'), 'utf8');
  const frameTag = html.match(/<iframe[^>]*id="verificationFrame"[^>]*>/)[0];
  assert.match(frameTag, /sandbox="allow-scripts"/);
  assert.doesNotMatch(frameTag, /allow-same-origin/);
  const doc = harness().api.frameDocument();
  assert.match(doc, /rmc-captcha\/1\.0\.0\.739\/captcha\.js/);
  assert.match(doc, /event\.source !== parent/);
});

test('the challenge reaches only the frame and an answer lifts the park', async () => {
  const h = harness({ challenge });
  await h.api.open(7);
  assert.deepEqual(h.calls[0], ['/api/accounts/7/verification', 'GET']);
  assert.ok(h.frame.srcdoc.includes('bdCaptcha'));
  // Messages from any other window are ignored.
  h.dispatch({}, { source: 'doubao-verify', type: 'ready' });
  h.dispatch({}, { source: 'doubao-verify', type: 'success' });
  assert.equal(h.posted.length, 0);
  assert.equal(h.calls.length, 1);
  h.dispatch(h.frameWindow, { source: 'doubao-verify', type: 'ready' });
  assert.equal(h.posted.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(h.posted[0].data.params)), challenge.params);
  h.dispatch(h.frameWindow, { source: 'doubao-verify', type: 'success' });
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(h.calls[1], ['/api/accounts/7/verification', 'POST']);
  assert.deepEqual(h.calls[2], ['reload']);
  assert.equal(h.toasts.at(-1)[1], 'success');
  assert.equal(h.listeners.length, 0, 'the message listener outlived the dialog');
});

test('an account without a recorded challenge says what to do', async () => {
  const h = harness({ getError: 'no recent verification challenge is recorded for this account' });
  await h.api.open(7);
  assert.match(h.toasts[0][0], /先让它处理一次请求/);
  assert.equal(h.frame.srcdoc, '');
});
