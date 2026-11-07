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
