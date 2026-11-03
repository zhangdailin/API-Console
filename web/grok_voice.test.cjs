const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function voiceUI({ web = 0, build = 0, console = 0, modelsLoaded = true } = {}) {
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
  Object.assign(state.grokCapabilityState, { loaded: true, counts: { web, build, console } });
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

test('Build and Console accounts cannot supply a Web LiveKit token', () => {
  const ui = voiceUI({ build: 31, console: 1 });
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
