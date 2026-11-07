const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function mediaTools() {
  const context = vm.createContext({
    window: {}, console, URLSearchParams, Date, Number, String, Math, Promise,
    document: { readyState: 'complete', addEventListener() {}, getElementById: () => null },
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/grok-media-tools.js'), 'utf8'), context);
  return context.window.GrokMediaTools;
}

test('image tiles and video cards escape names and prompts', () => {
  const tools = mediaTools();
  const tile = tools.mediaImageTile({ name: '"><img src=x onerror=alert(1)>.jpg', view_url: '/files/a.jpg', preview_url: '/files/a.jpg', size_bytes: 2048, updated_at: 0 });
  assert.ok(!tile.includes('<img src=x'), 'a file name cannot inject markup');
  assert.match(tile, /data-image-select="&quot;&gt;&lt;img/);
  assert.match(tile, /gw-media-thumb/);

  const card = tools.mediaVideoCard({ id: 'video_1', status: 'failed', prompt: '<script>x</script>', model: 'grok-imagine-video', seconds: 6 });
  assert.ok(!card.includes('<script>'), 'a prompt cannot inject markup');
  assert.match(card, /gw-status is-bad/);
  assert.match(card, /失败/);
  assert.match(card, /data-video-delete="video_1"/, 'a failed task can be deleted');

  const running = tools.mediaVideoCard({ id: 'video_2', status: 'in_progress', progress: 40 });
  assert.ok(!running.includes('data-video-delete'), 'a running task cannot be deleted');
  assert.match(running, /40%/);
});

test('the tab status dots do not shadow the capability badges', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const script = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  // grok-tools.js finds the badge with [data-capability="…"]; a dot carrying
  // the same attribute earlier in the page would be found instead.
  const badges = page.indexOf('class="capability-badges"');
  const tabs = page.indexOf('id="grokToolsTabs"');
  assert.ok(tabs >= 0 && badges >= 0);
  assert.ok(!page.slice(tabs, badges).includes('data-capability="'), 'tab dots use data-capability-dot');
  assert.match(script, /data-capability-dot="\$\{tab\}"/);
});

test('the workbench stylesheet is loaded after the base one', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  assert.ok(page.indexOf('css/grok-workbench.css') > page.indexOf('css/grok-tools.css'));
});

function chatPayloadHarness() {
  const context = vm.createContext({
    window: {}, console, Date, Math, JSON, Number, String, Array, Object, Set, Map, Promise,
    escapeHtml: (value) => String(value),
    localStorage: { getItem: () => null, setItem() {} }, sessionStorage: { getItem: () => null, setItem() {} },
    document: { addEventListener() {}, getElementById: () => null, querySelector: () => null, querySelectorAll: () => [] },
  });
  const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  vm.runInContext(source.replace('document.addEventListener("DOMContentLoaded", init);', 'window.chatTest = { chatState, buildResponsesPayload };'), context);
  return context.window.chatTest;
}

// Grok Web routes publish supports_backend_search=false; X search used to be
// sent anyway and failed with "requires a native Responses provider".
test('hosted search tools are only sent to routes that support them', () => {
  const { chatState, buildResponsesPayload } = chatPayloadHarness();
  chatState.sessions = [{ id: 's', messages: [{ role: 'user', content: 'hi' }], webSearch: true, xSearch: true }];
  chatState.activeId = 's';

  chatState.routes = [{ id: 'grok-chat-fast', supports_backend_search: false }];
  chatState.model = 'grok-chat-fast';
  assert.equal(buildResponsesPayload().tools, undefined, 'a Web route gets no hosted search tools');

  chatState.routes = [{ id: 'grok-4.7', supports_backend_search: true }];
  chatState.model = 'grok-4.7';
  assert.deepEqual(JSON.parse(JSON.stringify(buildResponsesPayload().tools)), [{ type: 'web_search' }, { type: 'x_search' }]);
});

// The video and voice tabs were rebuilt; every element the scripts look up by
// id must still be in the page, or the tab silently stops reacting.
test('the rebuilt video and voice tabs keep every element their scripts use', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const ids = new Set([...page.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));
  const missing = new Set();
  for (const file of ['grok-tools.js', 'grok-imagine.js']) {
    const script = fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8');
    for (const m of script.matchAll(/getElementById\(\s*["'`]((?:video|voice)[A-Za-z]+)["'`]\s*\)/g)) {
      if (!ids.has(m[1])) missing.add(m[1]);
    }
  }
  assert.deepEqual([...missing], []);
});

test('chat suggestion chips carry the prompt they fill in', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const empty = page.slice(page.indexOf('id="grokChatEmpty"'));
  const chips = [...empty.matchAll(/data-chat-suggestion="([^"]*)"/g)].map((m) => m[1]);
  assert.ok(chips.length >= 2);
  for (const prompt of chips) assert.ok(prompt.trim().length > 0);
  const script = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  assert.match(script, /closest\?\.\("\[data-chat-suggestion\]"\)/);
  assert.match(script, /input\.value = suggestion\.dataset\.chatSuggestion/);
});
