// Run with: node --test web/model_usage.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const asset = (name) => fs.readFileSync(path.join(__dirname, 'static/js', name), 'utf8');
const text = (html) => String(html).replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();

// The page renders HTML strings; retain the real table/cell structure needed by
// ConsoleUI.responsiveTable instead of replacing the production renderer.
function element() {
  const classes = new Set();
  return {
    dataset: {}, children: [], textContent: '', hidden: false,
    classList: {
      add: (name) => classes.add(name),
      contains: (name) => classes.has(name),
      toggle: (name, on) => on ? classes.add(name) : classes.delete(name),
    },
    querySelectorAll() { return []; },
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
  };
}

function tableFromHTML(html) {
  if (!html.includes('<table')) return null;
  const table = element();
  const headers = [...html.matchAll(/<th\b[^>]*>([\s\S]*?)<\/th>/g)]
    .map((match) => ({ textContent: text(match[1]) }));
  const body = html.match(/<tbody>([\s\S]*?)<\/tbody>/)?.[1] || '';
  const rows = [...body.matchAll(/<tr\b[^>]*>([\s\S]*?)<\/tr>/g)].map((match) => {
    const row = element();
    row.children = [...match[1].matchAll(/<td\b([^>]*)>([\s\S]*?)<\/td>/g)].map((cellMatch) => ({
      ...element(), className: cellMatch[1].match(/class="([^"]*)"/)?.[1], innerHTML: cellMatch[2],
    }));
    return row;
  });
  table.querySelectorAll = (selector) => selector === 'thead th' ? headers : selector === 'tbody tr' ? rows : [];
  return table;
}

function loadModels(viewport = 1440) {
  const list = element();
  Object.defineProperty(list, 'innerHTML', {
    get() { return this.html || ''; },
    set(value) { this.html = value; this.table = tableFromHTML(value); },
  });
  list.querySelector = (selector) => selector === 'table' ? list.table : null;
  const context = vm.createContext({
    document: {
      getElementById: (id) => id === 'modelsList' ? list : null,
      querySelectorAll: () => [], addEventListener() {},
    },
    window: { innerWidth: viewport, matchMedia: () => ({ matches: viewport <= 640 }) },
    // No test should fetch an upstream or the admin API while rendering metadata.
    fetch() { throw new Error('Rendering must not make a network request'); },
  });
  for (const file of ['common.js', 'models.js']) vm.runInContext(asset(file), context);
  const render = (models) => {
    context.fixtureModels = models;
    vm.runInContext('models = fixtureModels; currentModelChannel = ""; renderModels();', context);
    return list;
  };
  return { context, render };
}

const warp = (usage) => ({ id: 'warp-model', channel: 'Warp', model_id: 'model-a', name: 'Model A', status: 'available', upstream_usage: usage });
const usage = (min, max = min) => ({ credit_multiplier_min: min, credit_multiplier_max: max, account_count: 2, source: 'warp_catalog', observed_at: '2026-11-15T08:00:00Z' });

test('Warp credit multipliers distinguish zero, single values and account conflicts', () => {
  const { context } = loadModels();
  for (const [min, max, expected] of [[0, 0, '0×'], [1.25, 1.25, '1.25×'], [0, 3.5, '0–3.5×']]) {
    const html = context.modelUsageHtml(warp(usage(min, max)));
    assert.match(text(html), new RegExp(`积分倍率 ~${expected}`));
    assert.doesNotMatch(html, /未知|免费|free|请求倍率/);
  }
});

test('missing or invalid credit values render nothing, never unknown or free', () => {
  const { context } = loadModels();
  const invalid = [undefined, null, {}, { credit_multiplier_min: 0 }, { credit_multiplier_max: 0 },
    usage(null), usage('0'), usage(false), usage(-1), usage(NaN), usage(Infinity), usage(-Infinity), usage(2, 1),
    usage(0, null), usage(0, '1'), usage(0, false), usage(0, -1), usage(0, NaN), usage(0, Infinity),
    { credit_multiplier_min: '<img src=x onerror=alert(1)>', credit_multiplier_max: 1 },
    usage(0, '<script>alert(1)</script>')];
  for (const value of invalid) {
    assert.equal(context.modelUsageHtml(warp(value)), '');
  }
});

test('deprecated request multipliers never render, with or without actual credits', () => {
  const { context } = loadModels();
  const legacy = { request_multiplier_min: 0, request_multiplier_max: 2, requestMultiplier: 3, account_count: 1 };
  assert.equal(context.modelUsageHtml(warp(legacy)), '');
  assert.equal(context.modelUsageHtml(warp({ requestMultiplier: 3 })), '');
  assert.equal(context.modelUsageHtml(warp({ ...usage(null), ...legacy })), '');
  assert.equal(context.modelUsageHtml(warp({ ...usage(3), ...legacy })), context.modelUsageHtml(warp(usage(3))));
});

test('Warp usage shows only the compact credit badge, without coverage, notes or arbitrary metadata', () => {
  const { context } = loadModels();
  const metadata = {
    missing_account_count: 1,
    source: '<img src=x onerror="alert(1)"> account-private-id secret-fixture',
    observed_at: '"><script>alert(1)</script>',
    account_ids: ['account-private-id'], credentials: 'secret-fixture',
  };
  const html = context.modelUsageHtml(warp({ ...usage(1.5), ...metadata }));
  assert.equal(text(html), '积分倍率 ~1.5×');
  assert.equal((html.match(/class="models-usage-badge"/g) || []).length, 1);
  assert.doesNotMatch(html, /来源|目录账号|倍率缺失|非固定每次调用价格|账号、套餐和工作区|用量、时长及 Tokens|models-usage-meta|models-usage-note/);
  assert.doesNotMatch(html, /<img|<script|onerror|account-private-id|secret-fixture/);
  assert.equal(context.modelUsageHtml(warp(metadata)), '');
  for (const count of [-1, '2', Infinity, NaN, '<script>']) {
    assert.equal(context.modelUsageHtml(warp({ ...usage(1.5), account_count: count, missing_account_count: count })), html);
  }
});

test('non-Warp channels render no usage metadata even if the field is present', () => {
  const { context, render } = loadModels();
  for (const channel of ['Cline', 'Grok', 'WorkBuddy', '', 'not-warp']) {
    const model = { ...warp(usage(0)), channel };
    assert.equal(context.modelUsageHtml(model), '');
    assert.doesNotMatch(render([model]).innerHTML, /models-usage|积分倍率|请求倍率/);
  }
  assert.match(context.modelUsageHtml({ ...warp(usage(2)), channel: ' wArP ' }), /积分倍率 ~2×/);
});

test('desktop and mobile share the actual model-cell renderer and escape model text', () => {
  for (const viewport of [1440, 390]) {
    const { render } = loadModels(viewport);
    const fixture = { ...warp(usage(0, 2)), name: '<img src=x onerror="boom">', model_id: '<script>boom</script>' };
    const list = render([fixture]);
    const table = list.querySelector('table');
    assert.ok(table, `${viewport}: model table is rendered`);
    assert.equal(table.classList.contains('responsive-table'), true);
    const row = table.querySelectorAll('tbody tr')[0];
    const cell = row.children.find((child) => child.className === 'col-model');
    assert.equal(row.children.length, 7, 'metadata adds no new table column');
    assert.equal(cell.dataset.label, '模型');
    assert.match(text(cell.innerHTML), /积分倍率 ~0–2×/);
    assert.match(cell.innerHTML, /&lt;img.*&quot;boom&quot;&gt;/);
    assert.match(cell.innerHTML, /&lt;script&gt;boom&lt;\/script&gt;/);
    assert.doesNotMatch(cell.innerHTML, /<img|<script>/);
    assert.equal((cell.innerHTML.match(/class="models-usage"/g) || []).length, 1);
    assert.doesNotMatch(cell.innerHTML, /请求倍率|未知|来源|目录账号|倍率缺失|models-usage-meta|models-usage-note/);
    for (const value of [undefined, null, usage(null), { request_multiplier_min: 1, request_multiplier_max: 2, requestMultiplier: 3 }]) {
      const unknown = render([warp(value)]);
      assert.ok(unknown.querySelector('table'), `${viewport}: model remains visible without credit data`);
      assert.match(unknown.innerHTML, /Model A/);
      assert.doesNotMatch(unknown.innerHTML, /models-usage|积分倍率|请求倍率|未知|来源|目录账号|倍率缺失/);
    }
  }
});
