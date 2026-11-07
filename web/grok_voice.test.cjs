const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function voiceUI({ web = 0, build = 0, modelsLoaded = true } = {}) {
  const button = {
    disabled: false, dataset: {}, title: '',
    removeAttribute(name) { delete this[name]; },
  };
  const context = vm.createContext({
    window: {},
    escapeHtml: (value) => String(value),
    document: {
      addEventListener() {},
      getElementById(id) { return id === 'voiceStartBtn' ? button : null; },
    },
  });
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  vm.runInContext(source.replace('document.addEventListener("DOMContentLoaded", init);', `
    window.voiceTest = { grokCapabilityState, chatState, applyGrokCapabilityControls };
  `), context);
  const state = context.window.voiceTest;
  Object.assign(state.grokCapabilityState, { loaded: true, counts: { web, build } });
  state.chatState.modelsLoaded = modelsLoaded;
  return { button, state, apply: () => state.applyGrokCapabilityControls() };
}

test('a Web account enables LiveKit without voice models in the catalog', () => {
  for (const modelsLoaded of [false, true]) {
    const ui = voiceUI({ web: 1, modelsLoaded });
    ui.apply();
    assert.equal(ui.button.disabled, false);
    assert.equal(ui.button.dataset.capabilityBlocked, 'false');
  }
});

test('Build accounts cannot supply a Web LiveKit token', () => {
  const ui = voiceUI({ build: 31 });
  ui.state.chatState.capabilities.voice = true;
  ui.apply();
  assert.equal(ui.button.disabled, true);
  assert.match(ui.button.title, /缺少可用账号/);
});

test('adding a Web account clears the blocked button and stale account hint', () => {
  const ui = voiceUI();
  ui.apply();
  assert.equal(ui.button.disabled, true);
  ui.state.grokCapabilityState.counts.web = 1;
  ui.apply();
  assert.equal(ui.button.disabled, false);
  assert.equal(ui.button.title, undefined);
});

test('the Grok tools page carries no retired Console plane', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  assert.doesNotMatch(source, /provider === "console"|counts\.console|个 Console|"console"\]/);
  assert.doesNotMatch(page, /Console/);
});

test('chat markdown gives code blocks a language header and copy button, and keeps lists', () => {
  // The page escapes with common.js's escapeHtml; the sandbox provides the same contract.
  const escapeHtml = (value) => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  const context = vm.createContext({ window: {}, escapeHtml,
    document: { addEventListener() {}, getElementById() { return null; } } });
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  vm.runInContext(source.replace('document.addEventListener("DOMContentLoaded", init);', 'window.md = renderBasicMarkdown;'), context);
  const html = context.window.md('示例：\n\n```go\nfmt.Println("<hi>")\n```\n\n- 第一\n- 第二');
  assert.match(html, /<div class="code-block-wrap"><div class="code-block-head"><span>go<\/span><button type="button" class="code-copy-btn" data-code-copy>复制<\/button><\/div><pre class="code-block"><code class="language-go">/);
  assert.match(html, /fmt\.Println\(&quot;&lt;hi&gt;&quot;\)/, 'code stays escaped');
  assert.match(html, /<ul><li>第一<\/li><li>第二<\/li><\/ul>/);
  const plain = context.window.md('```\nx\n```');
  assert.match(plain, /<span>代码<\/span>/, 'a fence without a language is labelled 代码');
});

test('a bare video link from a video model renders as a player, and only that', () => {
  const escapeHtml = (value) => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  const context = vm.createContext({ window: {}, escapeHtml,
    document: { addEventListener() {}, getElementById() { return null; } } });
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  vm.runInContext(source.replace('document.addEventListener("DOMContentLoaded", init);', 'window.md = renderBasicMarkdown;'), context);
  const url = 'https://example.com/api/grok/tools/v1/files/video/abc.mp4';
  assert.match(context.window.md(url), new RegExp(`<video class="chat-video" src="${url.replace(/[.?/]/g, '\\$&')}" controls playsinline preload="metadata"></video>`));
  // A link written as Markdown stays a link; a non-video URL stays text.
  assert.doesNotMatch(context.window.md(`[下载](${url})`), /<video/);
  assert.doesNotMatch(context.window.md('https://example.com/page.html'), /<video/);
  // Script URLs never become a player source.
  assert.doesNotMatch(context.window.md('javascript:alert(1).mp4'), /<video/);
});

test('voice signal goes through the relay only for livekit.grok.com and only when chosen', () => {
  const context = vm.createContext({ window: {}, escapeHtml: String, URL,
    document: { addEventListener() {}, getElementById() { return null; } } });
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  vm.runInContext(source.replace('document.addEventListener("DOMContentLoaded", init);', 'window.signalURL = voiceSignalURL;'), context);
  const page = { protocol: 'https:', host: 'us1.daige.tech' };
  const relay = context.window.signalURL;
  assert.equal(relay('wss://livekit.grok.com', 'relay', page), 'wss://us1.daige.tech/api/grok/admin/voice/livekit');
  assert.equal(relay('wss://livekit.grok.com', 'relay', { protocol: 'http:', host: 'localhost:3000' }), 'ws://localhost:3000/api/grok/admin/voice/livekit');
  assert.equal(relay('wss://livekit.grok.com', 'direct', page), 'wss://livekit.grok.com');
  // The server relays one fixed host; any other URL is used as returned.
  assert.equal(relay('wss://other.livekit.cloud', 'relay', page), 'wss://other.livekit.cloud');
  assert.equal(relay('not a url', 'relay', page), 'not a url');
  const html = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  assert.match(html, /<select id="voiceTransport"[\s\S]*?<option value="relay" selected>/);
});
