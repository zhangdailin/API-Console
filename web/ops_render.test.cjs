// Run with: node --test web/ops_render.test.cjs
// Execute the page against deterministic in-file fixtures. Exact figure, row,
// failure-state and chart assertions catch cards that render the wrong data.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function makeElement(id) {
  const classes = new Set();
  return {
    id,
    textContent: '',
    innerHTML: '',
    value: '',
    hidden: false,
    children: [],
    dataset: {},
    style: {},
    className: '',
    classList: {
      add: (name) => classes.add(name),
      remove: (name) => classes.delete(name),
      toggle: (name, on) => (on ? classes.add(name) : classes.delete(name)),
      contains: (name) => classes.has(name),
    },
    replaceChildren(...nodes) {
      this.children = nodes;
      this.textContent = '';
    },
    appendChild(node) {
      this.children.push(node);
      return node;
    },
    addEventListener(event, handler) { this.listeners ||= {}; this.listeners[event] = handler; },
    setAttribute(name, value) {
      this[name] = String(value);
    },
    getAttribute(name) { return this[name]; },
    querySelector(selector) {
      if (selector === 'tbody') {
        this.tbody = this.tbody || makeElement('tbody');
        return this.tbody;
      }
      return makeElement(selector);
    },
    querySelectorAll() {
      return [];
    },
  };
}

const realPayload = {
  available: true,
  window_minutes: 180,
  since: '2026-09-12T11:48:19Z',
  until: '2026-09-12T14:48:19Z',
  retention_hours: 192,
  channels: ['grok', 'cline', 'workbuddy'],
  excluded_aggregates: ['http', 'probe'],
  totals: {
    requests: 248,
    success: 230,
    failed: 23,
    success_rate: 0.909,
    rpm: 1.4,
    duration_p95_ms: 8536,
    first_token_p95_ms: 3550,
    samples: 240,
  },
  concurrency: { accounts_refreshing: 2 },
  alerts: [{ key: 'success-rate:cline', severity: 'warning', channel: 'cline', title: 'cline 成功率 89%', detail: '窗口内 40 次请求' }],
  coverage: {
    entries: 10011,
    oldest: '2026-06-23T10:53:53Z',
    newest: '2026-09-12T14:48:12Z',
    counts: { request: 1974, operation: 10, system: 16 },
  },
  series: [
    { minute: '2026-09-12T13:53:00Z', requests: 3, success: 1, failed: 2 },
    { minute: '2026-09-12T13:54:00Z', requests: 5, success: 5, failed: 0 },
  ],
  matrix: [
    {
      channel: 'grok',
      accounts_enabled: 6,
      accounts_available: 6,
      accounts_needing_login: 0,
      model_cooldowns: 1,
      has_sample: true,
      summary: { requests: 20, success: 20, failed: 0, success_rate: 1, rpm: 0.1, duration_p95_ms: 8536, first_token_p95_ms: 3550, samples: 20 },
      models: [{ model: 'grok-4.6', requests: 8, success: 8, failed: 0, success_rate: 1, duration_p95_ms: 8536, samples: 8 }],
      series: [{ minute: '2026-09-12T13:53:00Z', requests: 2, success: 2, failed: 0 }],
    },
    {
      channel: 'cline',
      accounts_enabled: 1,
      accounts_available: 1,
      accounts_needing_login: 0,
      model_cooldowns: 0,
      has_sample: false,
      summary: { requests: 0, success: 0, failed: 0, success_rate: 0, rpm: 0, duration_p95_ms: 0, first_token_p95_ms: 0, samples: 0 },
      models: [],
      series: [],
    },
  ],
};

function renderPage(payload, fetcher, liveWindow = 1) {
  const elements = new Map();
  const liveButtons = [1, 5, 30, 60].map(minutes => { const button = makeElement("live-" + minutes); button.setAttribute("data-live", minutes); return button; });
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  };
  const context = vm.createContext({
    console,
    document: {
      readyState: 'complete',
      getElementById: node,
      createElement: (tag) => makeElement(tag),
      createElementNS: (_namespace, tag) => makeElement(tag),
      querySelector: node,
      querySelectorAll: selector => selector === "#opsLiveTabs .ops-chip" ? liveButtons : [],
      addEventListener() {},
    },
    window: { setInterval: () => 0, clearInterval() {}, addEventListener() {}, setTimeout: (fn) => setImmediate(fn) },
    setInterval: () => 0,
    setTimeout: (fn) => { fn(); return 0; },
    URLSearchParams,
    fetch: fetcher || (async () => ({
      ok: true,
      status: 200,
      json: async () => payload,
    })),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/ops.js'), 'utf8').replace('liveWindow: 1,', 'liveWindow: ' + liveWindow + ','), context);
  return { context, node, liveButtons };
}

const settle = async () => { for (let i = 0; i < 5; i++) await new Promise((resolve) => setImmediate(resolve)); };

test('time and channel changes reload alerts with the overview boundaries', async () => {
  const calls = [];
  const until = new Date('2026-09-30T08:00:00Z');
  const fetcher = async (url) => {
    const params = new URL(url, 'http://localhost').searchParams;
    calls.push(url);
    if (url.startsWith('/api/ops/overview')) {
      const minutes = Number(params.get('window'));
      return { ok: true, json: async () => ({ ...realPayload, window_minutes: minutes, since: new Date(until - minutes * 60000).toISOString(), until: until.toISOString() }) };
    }
    if (url.startsWith('/api/journal/records')) {
      const oneHour = params.get('since') === '2026-09-30T07:00:00.000Z';
      return { ok: true, json: async () => ({ data: oneHour ? [] : [{ event: { action: 'alert_fired', timestamp: '2026-09-30T06:00:00Z', channel: 'grok', error: 'older alert' } }] }) };
    }
    return { ok: true, json: async () => ({ metrics: [] }) };
  };
  const { node } = renderPage(realPayload, fetcher);
  await settle();
  assert.equal(node('opsAlertTable').querySelector('tbody').children.length, 1);
  node('opsWindow').value = '60';
  node('opsWindow').listeners.change();
  await settle();
  assert.match(node('opsAlertTable').querySelector('tbody').children[0].children[0].textContent, /暂无告警/);
  node('opsChannel').value = 'grok';
  node('opsChannel').listeners.change();
  await settle();
  const alerts = calls.filter((url) => url.startsWith('/api/journal/records'));
  assert.equal(alerts.length, 3, 'each scope refresh must load alerts exactly once');
  const params = new URL(alerts.at(-1), 'http://localhost').searchParams;
  assert.equal(params.get('channel'), 'grok');
  assert.equal(params.get('since'), '2026-09-30T07:00:00.000Z');
  assert.equal(params.get('until'), until.toISOString());
});

test('an older alert response cannot repaint a changed time range', async () => {
  let release;
  let count = 0;
  const fetcher = async (url) => {
    if (url.startsWith('/api/journal/records')) {
      if (++count === 1) return new Promise((resolve) => { release = () => resolve({ ok: true, json: async () => ({ data: [{ event: { action: 'alert_fired', timestamp: realPayload.until, error: 'stale alert' } }] }) }); });
      return { ok: true, json: async () => ({ data: [] }) };
    }
    const minutes = Number(new URL(url, 'http://localhost').searchParams.get('window')) || 180;
    return { ok: true, json: async () => ({ ...realPayload, window_minutes: minutes }) };
  };
  const { node } = renderPage(realPayload, fetcher);
  await settle();
  node('opsWindow').value = '60';
  node('opsWindow').listeners.change();
  release();
  await settle();
  assert.equal(count, 2);
  assert.match(node('opsAlertTable').querySelector('tbody').children[0].children[0].textContent, /暂无告警/);
});

test('failed window refresh clears historical charts and matrix instead of showing the previous scope', async () => {
  let broken = false;
  const { node } = renderPage(realPayload, async (url) => {
    if (broken && url.startsWith('/api/ops/overview')) return { ok: false, status: 503, text: async () => '' };
    return { ok: true, json: async () => url.startsWith('/api/journal/records') ? { data: [] } : realPayload };
  });
  await settle();
  assert.ok(node('opsMatrix').querySelector('tbody').children.length > 1);
  broken = true;
  node('opsWindow').value = '60';
  node('opsWindow').listeners.change();
  await settle();
  assert.equal(node('opsMatrix').querySelector('tbody').children.length, 1);
  assert.match(node('opsThroughput').children[0].textContent, /读取失败/);
});

test('the KPI cards render the figures and mark no-sample ones', async () => {
  const { node } = renderPage(realPayload);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const cards = node('opsKpis').children;
  assert.ok(cards.length >= 6, `expected KPI cards, got ${cards.length}`);
  const values = cards.map((card) => (card.children[1] ? card.children[1].textContent : ''));
  assert.ok(values.includes('248'), `real request count missing from ${values.join(',')}`);
  assert.ok(values.some((value) => /90\.90*%/.test(value)), `success rate missing from ${values.join(',')}`);
});

test('the matrix renders provider rows plus their model rows', async () => {
  const { node } = renderPage(realPayload);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const rows = node('opsMatrix').querySelector('tbody').children;
  assert.equal(rows.length, 4, `expected grok + grok-4.6 + unattributed + cline rows, got ${rows.length}`);
  const labels = rows.map((row) => (row.children[0] ? row.children[0].textContent : ''));
  assert.deepEqual(labels, ['grok', 'grok-4.6', '未识别模型', 'cline']);
  // grok has 20 requests and its one model row owns 8: the other 12 ended
  // before a model was resolved and get a row of their own, so the model rows
  // cannot read healthier than the channel.
  const remainder = rows[2].children.map((cell) => cell.textContent);
  assert.ok(remainder.includes('12'), `unattributed row shows ${remainder.join('|')}`);
  assert.ok(remainder.includes('100.0%'), `unattributed success rate: ${remainder.join('|')}`);
  // The traffic-free channel must read 暂无样本, never a healthy 100%.
  const clineCells = rows[3].children.map((cell) => cell.textContent);
  assert.ok(clineCells.includes('暂无样本'), `cline row shows ${clineCells.join('|')}`);
});

test('a failed fetch still explains itself instead of leaving the card blank', async () => {
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  };
  const context = vm.createContext({
    console,
    document: { readyState: 'complete', getElementById: node, createElement: (tag) => makeElement(tag), createElementNS: (_namespace, tag) => makeElement(tag), querySelector: node, querySelectorAll: () => [], addEventListener() {} },
    window: { setInterval: () => 0, clearInterval() {}, addEventListener() {} },
    setInterval: () => 0,
    setTimeout: (fn) => { fn(); return 0; },
    URLSearchParams,
    fetch: async () => ({ ok: false, status: 401, text: async () => '', json: async () => ({}) }),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/ops.js'), 'utf8'), context);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.ok(node('opsKpis').children.length > 0, 'the failure is not surfaced in the KPIs');
  assert.equal(node('opsStatusText').textContent, '读取失败');
});

test('an aggregation-disabled response states the reason in the status line', async () => {
  const { node } = renderPage({ available: false, note: '指标聚合需要 Redis；当前部署未启用。', window_minutes: 180, coverage: {}, excluded_aggregates: [] });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(node('opsStatusText').textContent, /Redis/, 'the disabled reason is not shown');
});

test('collection loss stays visible even when overview queries succeed', async () => {
  const { node } = renderPage({ ...realPayload, ingestion: { queue: 2, queued_bytes: 128, written: 10, dropped: 3, write_failed: 1 } });
  await settle();
  assert.equal(node('opsStatusText').textContent, '日志采集存在丢失');
});

test('aggregation failures are distinct from healthy audit ingestion', async () => {
  const { node } = renderPage({ ...realPayload, ingestion: { queue: 0, queued_bytes: 0, written: 10, dropped: 0, write_failed: 0 }, aggregation_health: { available: true, write_failed: 2 } });
  await settle();
  assert.equal(node('opsStatusText').textContent, '指标采集存在丢失');
});


test('throughput with real token usage draws TPS and closes the QPS area', async () => {
  const payload = { ...realPayload, series: [
    { minute: '2026-09-12T14:47:00Z', requests: 6, input_tokens: 900, output_tokens: 300, usage_samples: 6 },
    { minute: '2026-09-12T14:48:00Z', requests: 2, input_tokens: 100, output_tokens: 90, usage_samples: 2 },
  ] };
  const { node } = renderPage(payload, undefined, 5);
  await settle();
  const svg = node('opsThroughput').children[0];
  const path = svg.children.find(child => child.class === 'series-tps');
  assert.ok(path, 'TPS series missing despite reported usage');
  assert.match(path.d, /^M[\d.]+,[\d.]+ L/, 'TPS path has no segments');
  assert.doesNotMatch(path.d, /NaN|undefined/);
  const vertices = path.d.match(/[ML][\d.]+,([\d.]+)/g);
  assert.ok(new Set(vertices.map(point => point.split(',')[1])).size > 1, 'TPS flattened onto baseline');
  const area = svg.children.find(child => child.class === 'series-area');
  assert.match(area.d, /^M[\d.]+,[\d.]+ L/);
  assert.ok(area.d.endsWith(' Z'), 'QPS area not closed');
  assert.match(node('opsThroughputHint').textContent, /右轴 输出 TPS/);

});

test('throughput without usage keeps TPS absent and explains why', async () => {
  const { node } = renderPage(realPayload);
  await settle();
  const svg = node('opsThroughput').children[0];
  assert.equal(svg.children.some(child => child.class === 'series-tps'), false);
  assert.match(node('opsThroughputHint').textContent, /未上报用量/);
});

test('latencies are printed in readable units, not raw milliseconds', async () => {
  const totals = { ...realPayload.totals, duration: { p50_ms: 640, p90_ms: 8536, p95_ms: 41200, p99_ms: 88000, avg_ms: 5200, max_ms: 212000, samples: 240 } };
  const { node } = renderPage({ ...realPayload, totals });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  const text = (n) => [n.textContent || '', ...(n.children || []).map(text)].join(' ');
  const kpis = text(node('opsKpis'));
  for (const expected of ['640 ms', '8.54 s', '41.2 s', '1 分 28 秒', '3 分 32 秒']) {
    assert.ok(kpis.includes(expected), `KPI cards miss ${expected}`);
  }
  assert.doesNotMatch(kpis, /\d{4,} ms/, 'a latency of a second or more is still printed in ms');
  // The matrix uses the same formatter: grok's 8536 ms P95.
  const grok = node('opsMatrix').querySelector('tbody').children[0].children.map((cell) => cell.textContent);
  assert.ok(grok.includes('8.54 s'), `matrix shows ${grok.join('|')}`);
});

test('gateway refusals are shown apart from upstream errors and token rates are named', async () => {
  const totals = { ...realPayload.totals, server_errors: 1, stream_errors: 0, gateway_unavailable: 154,
    input_tokens: 5400, output_tokens: 600, usage_samples: 10, tps: 0.6, output_tps: 0.06,
    detailed_requests: realPayload.totals.requests };
  const { node } = renderPage({ ...realPayload, totals });
  await settle();
  const text = (n) => [n.textContent || '', ...(n.children || []).map(text)].join(' ');
  const kpis = text(node('opsKpis'));
  assert.match(kpis, /Token 吞吐（含输入）\s*0\.6 token\/秒/);
  assert.match(kpis, /输出 TPS\s*0\.1 token\/秒/);
  assert.match(kpis, /未到上游 5xx\s*154/);
  assert.match(kpis, /未到上游 5xx（网关无可用账号等，未计入）\s*154/);
  // The upstream-error headline counts the provider's failures only.
  const upstreamCard = node('opsKpis').children.find((card) => text(card).includes('上游错误'));
  assert.match(text(upstreamCard), /上游错误[\s\S]*?\b1\b\s*次/);
  const mix = text(node('opsErrorMix'));
  assert.match(mix, /未到上游\s*154/);
});
