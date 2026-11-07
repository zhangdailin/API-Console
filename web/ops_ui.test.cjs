// Run with: node --test web/ops_ui.test.cjs
// Assertions over the operations overview and log centre markup: the pages are
// rendered by Go templates and hydrated by plain JS, so these checks are the
// only place the structure is verified without a browser.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const read = (relative) => fs.readFileSync(path.join(__dirname, relative), 'utf8');

test('the overview page carries the filters the design calls for', () => {
  const page = read('templates/pages/ops.html');
  // Fixed time range on top, channel filter beside it.
  for (const window of ['60', '180', '720', '1440']) {
    assert.match(page, new RegExp(`<option value="${window}"`), `missing ${window}-minute range`);
  }
  assert.match(page, /id="opsChannel"/, 'missing channel filter');
  assert.match(page, /id="opsKpis"/, 'missing KPI container');
  assert.match(page, /id="opsThroughput"/, 'missing trend chart container');
  assert.match(page, /id="opsMatrix"/, 'missing channel × model matrix');
  assert.match(page, /id="opsAlertTable"/, 'missing alert table');
});

test('the overview states every KPI the operations spec lists', () => {
  const script = read('static/js/ops.js');
  for (const label of ['请求', 'SLA', '请求错误', '请求时长', 'TTFT', '上游错误']) {
    assert.ok(script.includes(label), `KPI ${label} is not rendered`);
  }
  // No traffic must read as 暂无样本, never as a healthy zero.
  assert.ok(script.includes('暂无样本'), 'the no-sample wording is missing');
});

test('the trend marks failed buckets apart from successful ones', () => {
  const script = read('static/js/ops.js');
  assert.ok(script.includes('is-bad'), 'failed minutes are not distinguished');
  assert.ok(script.includes('point.failed'), 'failed counts are not plotted');
});

test('the log centre has the three tabs and one shared detail panel', () => {
  const page = read('templates/pages/logs.html');
  for (const kind of ['request', 'operation', 'system']) {
    assert.match(page, new RegExp(`data-kind="${kind}"`), `missing ${kind} journal tab`);
  }
  for (const filter of ['filterChannel', 'filterModel', 'filterStatus', 'filterActor', 'filterAction']) {
    assert.ok(page.includes(filter), `missing filter ${filter}`);
  }
  assert.match(page, /id="logsRows"/, 'missing record list');
  assert.match(page, /id="logsDetail"/, 'missing detail panel');
  for (const control of ['logsPager', 'logsFirst', 'logsPrev', 'logsNext', 'logsPageSize']) {
    assert.match(page, new RegExp(`id="${control}"`), `missing pagination control ${control}`);
  }
});

test('request details show every upstream attempt and operation details show the change', () => {
  const script = read('static/js/logs.js');
  assert.ok(script.includes('上游尝试'), 'request detail does not label upstream attempts');
  assert.ok(script.includes('record.attempts'), 'request detail does not read the attempts list');
  assert.ok(script.includes('变更摘要'), 'operation detail does not label the change summary');
  assert.ok(script.includes('已脱敏字段'), 'operation detail does not disclose the masked fields');
});

test('the overview and log centre carry no coverage explanation text', () => {
  for (const file of ['static/js/logs.js', 'static/js/ops.js', 'templates/pages/ops.html', 'templates/pages/logs.html']) {
    const text = read(file);
    assert.ok(!text.includes('保留窗口内') && !text.includes('数据覆盖与统计口径'), `${file} still explains its coverage`);
  }
});

test('运维总览 is reachable from the sidebar and is the landing tab', () => {
  const sidebar = read('templates/partials/sidebar.html');
  assert.match(sidebar, /href="\{\{\.AdminPath\}\}\/\?tab=ops"/, 'sidebar has no 运维总览 entry');
  assert.match(sidebar, /href="\{\{\.AdminPath\}\}\/\?tab=logs"/, 'sidebar has no 日志中心 entry');
  assert.doesNotMatch(sidebar, /onclick="switchTab\(/, 'sidebar navigation still depends on inline JavaScript');
  assert.ok(sidebar.includes('运维总览'), 'the sidebar label is missing');
  assert.ok(sidebar.includes('日志中心'), 'the sidebar label is missing');

  const template = fs.readFileSync(path.join(__dirname, '..', 'internal', 'template', 'template.go'), 'utf8');
  assert.match(template, /tab = "ops"/, 'the default tab is not the operations overview');
  assert.match(template, /case "ops":/, 'the ops tab is not routed');
  assert.match(template, /case "logs":/, 'the logs tab is not routed');
});

test('the matrix renders per-model rows and never paints a traffic-free channel green', () => {
  const script = read('static/js/ops.js');
  assert.ok(script.includes('is-model'), 'per-model rows are not distinguished in the matrix');
  assert.ok(script.includes('row.models'), 'the matrix does not render the per-model rows');
  // A channel with no samples must read 暂无样本 rather than a healthy 100%.
  assert.ok(script.includes("formatRate(r, samples)"), 'the success rate is rendered without its sample count');
  assert.ok(script.includes("ops-empty"), 'the no-sample cell style is missing');
});

test('the overview follows the live flow stream instead of a refresh countdown', () => {
  const page = read('templates/pages/ops.html');
  const script = read('static/js/ops.js');
  const flow = read('static/js/ops-flow.js');
  assert.ok(!page.includes('opsCountdown') && !script.includes('refreshSeconds'), 'the 15-second countdown is still there');
  assert.ok(flow.includes("'ops:flow'") && script.includes("'ops:flow'"), 'the overview does not listen to the flow stream');
  assert.ok(script.includes('LIVE_MIN_GAP_MS'), 'live reads are not spaced');
});
