// Run with: node --test web/warp_quota.test.cjs
// Snapshot fixtures only: no upstream requests or live account data.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function load() {
  const element = () => ({ style: {}, dataset: {}, value: '', classList: { add() {}, remove() {}, toggle() {} },
    addEventListener() {}, setAttribute() {}, appendChild() {} });
  const context = vm.createContext({
    document: { getElementById: element, querySelector: element, querySelectorAll: () => [],
      createElement: element, addEventListener() {}, body: element() },
    window: { addEventListener() {}, setInterval() {}, clearInterval() {},
      matchMedia: () => ({ matches: false, addEventListener() {} }),
      location: { pathname: '/' }, history: { replaceState() {} },
      localStorage: { getItem: () => null, setItem() {}, removeItem() {} } },
    setTimeout() {}, clearTimeout() {}, console,
  });
  for (const file of ['common.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return context;
}

function pool(overrides = {}) {
  return { scope: 'workspace_monthly', unit: 'credits', limit: 100, used: 25,
    remaining: 75, unlimited: false, has_unlimited: true, reset_at: '', ...overrides };
}

function account(windows = [pool()], overrides = {}) {
  return { id: 7, account_type: 'warp', has_credential: true, enabled: true,
    quota_supported: true, quota_mode: 'warp_pools',
    warp_quota: { status: 'ok', windows, synced_at: '2026-11-15T08:00:00Z',
      attempted_at: '2026-11-15T08:00:00Z' }, ...overrides };
}

const text = markup => markup.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ').trim();

test('Warp returns independent snapshot pools with nullable numbers and no aggregate', () => {
  const ui = load();
  const acc = account([pool(), pool({ remaining: 50 }), pool({ scope: 'personal', limit: 10, used: null, remaining: 4 })]);
  const quota = ui.getQuotaStats(acc);
  assert.equal(quota.supported, true);
  assert.equal(quota.warpPools.windows.length, 3);
  assert.equal(quota.warpPools.windows[2].used, null);
  for (const key of ['limit', 'remaining', 'used', 'pctRemaining']) assert.equal(quota[key], undefined);
  const output = text(ui.buildQuotaMarkup(acc));
  assert.match(output, /工作区月度包含池 1.*剩余 75.*总额 100/);
  assert.match(output, /工作区月度包含池 2.*剩余 50.*总额 100/);
  assert.match(output, /个人包含池 1.*剩余 4.*总额 10.*已用 未知/);
  assert.doesNotMatch(output, /总额 210|剩余 129/);
  assert.match(output, /仅包含额度，不含额外\/按量余额/);
});

test('Warp never falls back to generic quotas or local usage counters', () => {
  const ui = load();
  for (const warp_quota of [undefined, null, {}, { status: 'unknown', windows: [] }]) {
    const acc = account([], { warp_quota, usage_limit: 99999, usage_current: 88888,
      quota_limit: 77777, quota_used: 66666, quota_remaining: 55555 });
    const quota = ui.getQuotaStats(acc);
    assert.equal(quota.supported, false);
    assert.equal(quota.warpPools.status, 'unknown');
    assert.match(text(ui.buildQuotaMarkup(acc)), /未知/);
    assert.doesNotMatch(text(ui.buildQuotaMarkup(acc)), /99999|88888|77777|66666|55555|无限|免费/);
  }
});

test('null, omitted, invalid and non-numeric values stay unknown rather than zero', () => {
  const ui = load();
  for (const value of [null, undefined, '', '0', '25', false, NaN, Infinity, -1]) {
    const acc = account([pool({ limit: value, used: value, remaining: value })]);
    const quota = ui.getQuotaStats(acc);
    assert.equal(quota.supported, false);
    assert.equal(quota.warpPools.windows[0].remaining, null);
    const output = text(ui.buildQuotaMarkup(acc));
    assert.match(output, /剩余 未知 \/ 总额 未知.*已用 未知/);
    assert.doesNotMatch(output, /已用尽|无限|剩余 0/);
  }
});

test('explicit zero is rendered as exhausted but never changes Warp account health', () => {
  const ui = load();
  const acc = account([pool({ limit: 0, used: 0, remaining: 0 })], { quota_exhausted: true });
  const quota = ui.getQuotaStats(acc);
  assert.equal(quota.supported, true);
  assert.match(text(ui.buildQuotaMarkup(acc)), /剩余 0 \/ 总额 0 · 包含额度已用尽/);
  assert.equal(ui.isQuotaExhaustedQuota(acc), false);
  assert.equal(ui.isQuotaOnlyStatus(acc), false);
  assert.equal(ui.isAccountCapacityLimited(acc), false);
  assert.equal(ui.evaluateAccountStatus(acc).text, '正常');
  assert.equal(ui.isSidebarAccountAbnormal(acc), false);
  const failed = { ...acc, status_code: '401' };
  assert.equal(ui.evaluateAccountStatus(failed).text, '未授权');
  assert.equal(ui.isSidebarAccountAbnormal(failed), true, 'a display-only pool must not hide auth errors');
});

test('only a known unlimited flag means unlimited, even with remaining zero', () => {
  const ui = load();
  const known = account([pool({ limit: null, used: 8, remaining: 0, unlimited: true, has_unlimited: true })]);
  assert.match(text(ui.buildQuotaMarkup(known)), /剩余 无限 \/ 总额 无限/);
  assert.doesNotMatch(text(ui.buildQuotaMarkup(known)), /已用尽/);
  for (const flags of [{ unlimited: true, has_unlimited: false }, { unlimited: false, has_unlimited: true }, {}]) {
    const acc = account([pool({ ...flags, remaining: null, limit: null })]);
    assert.doesNotMatch(text(ui.buildQuotaMarkup(acc)), /无限/);
    assert.match(text(ui.buildQuotaMarkup(acc)), /剩余 未知/);
  }
});

test('a refresh error keeps the last values and sync time with an explicit stale label', () => {
  const ui = load();
  const acc = account([pool({ remaining: 12.5 })]);
  acc.warp_quota.status = 'error';
  acc.warp_quota.attempted_at = '2026-11-15T09:00:00Z';
  const quota = ui.getQuotaStats(acc);
  assert.equal(quota.warpPools.synced_at, '2026-11-15T08:00:00Z');
  assert.equal(quota.warpPools.attempted_at, '2026-11-15T09:00:00Z');
  assert.match(text(ui.buildQuotaMarkup(acc)), /上次记录，刷新失败.*剩余 12\.50/);
  assert.match(ui.quotaTooltip(acc, quota), /上次记录，刷新失败.*上次同步:.*最近尝试:/);
  assert.equal(ui.evaluateAccountStatus(acc).text, '正常');
  const empty = account([], { warp_quota: { status: 'error', windows: [] } });
  assert.match(text(ui.buildQuotaMarkup(empty)), /未知，刷新失败/);
  assert.doesNotMatch(text(ui.buildQuotaMarkup(empty)), /上次记录/);
});

test('partial results remain visible, independent and clearly labeled', () => {
  const ui = load();
  const acc = account([pool(), pool({ scope: 'personal', remaining: null })]);
  acc.warp_quota.status = 'partial';
  assert.match(text(ui.buildQuotaMarkup(acc)), /部分同步.*工作区月度包含池 1.*个人包含池 1.*剩余 未知/);
  assert.match(ui.quotaTooltip(acc, ui.getQuotaStats(acc)), /部分同步.*各池独立|各池独立.*部分同步/);
});

test('legacy request windows retain their units and reported usage is not capped or inferred', () => {
  const ui = load();
  const acc = account([pool({ scope: 'legacy_requests', unit: 'requests', limit: 10, used: 12, remaining: null }),
    pool({ scope: 'personal', unit: 'credits', limit: 2.5, used: null, remaining: 0.25 })]);
  const output = text(ui.buildQuotaMarkup(acc));
  assert.match(output, /旧版请求池 1 \(requests\).*剩余 未知 \/ 总额 10.*已用 12/);
  assert.match(output, /个人包含池 1 \(credits\).*剩余 0\.25 \/ 总额 2\.50.*已用 未知/);
});

test('reset dates are compact and Go zero dates remain unknown', () => {
  const ui = load();
  const future = new Date(Date.now() + 4 * 3600000).toISOString();
  const acc = account([pool({ reset_at: future }), pool({ reset_at: '0001-01-01T00:00:00Z' })]);
  assert.match(text(ui.buildQuotaMarkup(acc)), /小时后重置.*重置时间未知/);
  const zero = account([], { warp_quota: { status: 'unknown', windows: [],
    synced_at: '0001-01-01T00:00:00Z', attempted_at: 'not-a-date' } });
  const tip = ui.quotaTooltip(zero, ui.getQuotaStats(zero));
  assert.doesNotMatch(tip, /0001|not-a-date|上次同步|最近尝试/);
});

test('Warp projection and markup never expose workspace identifiers or arbitrary upstream strings', () => {
  const ui = load();
  const injected = '<img src=x onerror="boom">';
  const acc = account([pool({ workspace_id: 'private-workspace-id', name: injected, bonus_remaining: 99999 }),
    pool({ scope: injected }), pool({ unit: injected }), pool({ reset_at: injected })], {
    quota_note: injected, quota_source: injected,
  });
  acc.warp_quota.source = injected;
  acc.warp_quota.status = injected;
  const quota = ui.getQuotaStats(acc);
  assert.equal(quota.warpPools.status, 'unknown');
  assert.equal(quota.warpPools.windows.length, 2);
  const output = ui.buildQuotaMarkup(acc) + ui.quotaTooltip(acc, quota) + JSON.stringify(quota);
  assert.doesNotMatch(output, /private-workspace-id|workspace_id|onerror|bonus_remaining|99999/);
  // All dynamic formatter output is escaped even if a caller replaces a helper.
  ui.formatCredit = () => injected;
  ui.formatQuotaReset = () => injected;
  const escaped = ui.buildQuotaMarkup(account([pool({ reset_at: '2027-01-01T00:00:00Z' })]));
  assert.match(escaped, /&lt;img src=x onerror=&quot;boom&quot;&gt;/);
  assert.doesNotMatch(escaped, /<img/);
});

test('Warp snapshots do not alter another provider quota or exhaustion behavior', () => {
  const ui = load();
  const acc = { account_type: 'cline', enabled: true, has_credential: true,
    quota_supported: true, quota_limit: 100, quota_remaining: 0,
    warp_quota: account().warp_quota };
  const quota = ui.getQuotaStats(acc);
  assert.equal(quota.warpPools, undefined);
  assert.equal(quota.limit, 100);
  assert.equal(ui.isQuotaOnlyStatus(acc), true);
  assert.match(text(ui.buildQuotaMarkup(acc)), /0 \/ 100/);
});
