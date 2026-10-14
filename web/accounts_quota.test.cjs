// Run with: node --test web/accounts_quota.test.cjs
//
// The accounts table must be able to say "免费（推断）" and show an estimated window
// without ever presenting the estimate as a balance the upstream reported. These
// assertions pin both halves: the estimate is rendered (with ≈ and its provenance),
// and an account with no signal at all still reads 未知.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

function load({ accountPage = true } = {}) {
  const makeElement = (tag) => ({
    tagName: tag,
    value: '',
    hidden: false,
    textContent: '',
    innerHTML: '',
    style: {},
    dataset: {},
    children: [],
    appendChild(child) { this.children.push(child); return child; },
    addEventListener() {},
    setAttribute() {},
    removeAttribute() {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  });
  const context = vm.createContext({
    document: {
      getElementById: () => makeElement('div'),
      querySelector: () => makeElement('div'),
      createElement: makeElement,
      createDocumentFragment: () => makeElement('fragment'),
      querySelectorAll: () => [],
      addEventListener() {},
      body: makeElement('body'),
    },
    window: {
      setInterval: () => 0,
      clearInterval: () => {},
      addEventListener() {},
      matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
      innerWidth: 1440,
      location: { href: '', pathname: '/' },
      history: { replaceState() {} },
      localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    },
    setTimeout: (fn) => { fn(); return 0; },
    clearTimeout: () => {},
    console,
  });
  for (const file of accountPage ? ['common.js', 'accounts.js'] : ['common.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return context;
}

// A Grok Build OAuth account exactly as the server projects it: no upstream plan
// name, no numeric window, and a Free inference carrying its provenance.
function buildFreeAccount(overrides = {}) {
  return {
    id: 143,
    account_type: 'grok',
    credential_type: 'oauth',
    grok_provider: 'build',
    enabled: true,
    weight: 1,
    // The plan string upstream never sent is what makes this the Free case.
    subscription: '',
    quota_mode: 'estimated_free',
    quota_unit: 'tokens',
    quota_supported: true,
    quota_type: 'free',
    quota_source: 'billingProfile',
    quota_confidence: 'estimated',
    quota_limit_known: false,
    quota_observed: true,
    quota_window_hours: 24,
    quota_note: '上游未下发数值额度；按免费画像估算，用量为本网关在窗口内观测到的 token',
    plan_name: '',
    plan_tier: 'free',
    plan_confidence: 'inferred',
    quota_limit: 500000,
    quota_used: 12000,
    quota_remaining: 488000,
    ...overrides,
  };
}

test('a Free-inferred Build account renders an estimated window instead of 未知', () => {
  const context = load();
  const acc = buildFreeAccount();

  const quota = context.getQuotaStats(acc);
  assert.equal(quota.estimated, true, 'the estimate is not marked as such');
  assert.equal(quota.limit, 500000);
  assert.equal(quota.used, 12000);
  assert.equal(quota.remaining, 488000);
  assert.equal(quota.limitKnown, false);
  assert.equal(quota.windowHours, 24);

  const markup = context.buildQuotaMarkup(acc);
  assert.match(markup, /≈/, 'an estimated number must be prefixed with ≈');
  assert.match(markup, /12,000/);
  assert.match(markup, /500,000/);
  assert.match(markup, /免费估算/);
  assert.match(markup, /滚动 24h/);
  assert.doesNotMatch(markup, />未知</, 'an inferred account must not read 未知');
});

test('the provenance travels with the number so it cannot be mistaken for a balance', () => {
  const context = load();
  const acc = buildFreeAccount();
  const tip = context.quotaTooltip(acc, context.getQuotaStats(acc));

  assert.match(tip, /免费（推断）估算额度/);
  assert.match(tip, /免费账单画像推断/);
  assert.match(tip, /置信度: 估算/);
  assert.match(tip, /限额未经上游确认/);
  assert.match(tip, /滚动 24 小时/);
});

test('an unmeasured window is shown as 未统计 rather than as zero usage', () => {
  const context = load();
  const acc = buildFreeAccount({ quota_observed: false, quota_used: 0, quota_remaining: 500000 });

  const markup = context.buildQuotaMarkup(acc);
  assert.match(markup, /未统计/, 'zero usage must not be presented as a measurement');
  assert.doesNotMatch(markup, /0 \/ 500,000/);
  assert.match(context.quotaTooltip(acc, context.getQuotaStats(acc)), /本网关未统计到窗口内用量/);
});

test('the tier column says 免费（推断）when upstream published no plan name', () => {
  const context = load();
  const badge = context.subscriptionBadge(buildFreeAccount());
  assert.equal(badge.text, '免费（推断）');
  assert.match(badge.tip, /推断为免费/);
  assert.match(badge.tip, /免费账单画像推断/);
});

test('a Free window the upstream confirmed is shown as a balance, not an estimate', () => {
  const context = load();
  const acc = buildFreeAccount({
    quota_mode: 'confirmed_free',
    quota_type: 'free',
    quota_source: 'upstreamExhaustion',
    quota_confidence: 'confirmed',
    quota_limit_known: true,
    quota_observed: true,
    quota_limit: 300000,
    quota_used: 300000,
    quota_remaining: 0,
    quota_note: '上游额度耗尽时返回的真实免费窗口（tokens actual/limit）',
    plan_confidence: 'confirmed',
  });

  const quota = context.getQuotaStats(acc);
  assert.equal(quota.confirmedFree, true);
  assert.equal(quota.estimated, undefined);
  assert.equal(quota.limit, 300000);

  const markup = context.buildQuotaMarkup(acc);
  assert.doesNotMatch(markup, /≈/, 'a confirmed window is not an estimate');
  assert.match(markup, /0 \/ 300,000/);
  assert.match(markup, /免费实报/);

  const tip = context.quotaTooltip(acc, quota);
  assert.match(tip, /上游额度耗尽实报/);
  assert.match(tip, /非估算/);
  assert.equal(context.subscriptionBadge(acc).text, '免费', 'a confirmed free window is not a guess');
});

test('a paid plan without a numeric window stays unknown and invents nothing', () => {
  const context = load();
  const acc = buildFreeAccount({
    subscription: 'supergrok',
    quota_mode: 'unknown',
    quota_supported: false,
    quota_type: 'paid',
    quota_source: 'planMetadata',
    quota_confidence: 'confirmed',
    quota_limit_known: false,
    quota_observed: false,
    quota_window_hours: undefined,
    quota_note: '官方身份接口报告为付费套餐，但未下发数值额度窗口',
    quota_limit: 0,
    quota_used: 0,
    quota_remaining: 0,
  });

  const quota = context.getQuotaStats(acc);
  assert.equal(quota.quotaUnavailable, true);
  assert.doesNotMatch(context.buildQuotaMarkup(acc), /≈/);
  assert.match(context.quotaTooltip(acc, quota), /付费额度/);
  assert.match(context.quotaTooltip(acc, quota), /官方套餐标记/);
});

test('an account with no signal at all still reads 未知', () => {
  const context = load();
  const acc = buildFreeAccount({
    subscription: '',
    quota_mode: 'unknown',
    quota_supported: false,
    quota_type: 'unknown',
    quota_source: 'unknown',
    quota_confidence: '',
    quota_limit_known: false,
    quota_observed: false,
    quota_note: '尚未同步到上游套餐或额度信息；点刷新立即同步',
    plan_tier: 'unsynced',
    plan_confidence: '',
    quota_limit: 0,
    quota_used: 0,
    quota_remaining: 0,
  });

  assert.equal(context.getQuotaStats(acc).quotaUnavailable, true);
  assert.match(context.buildQuotaMarkup(acc), /未知/);
  assert.equal(context.subscriptionBadge(acc).text, '未同步', 'no signal means no free claim');
  assert.match(context.quotaTooltip(acc, context.getQuotaStats(acc)), /尚未同步/);
});

test('an official weekly Build window still wins over the estimate', () => {
  const context = load();
  const acc = buildFreeAccount({
    grok_billing: { weekly: { has_usage: true, usage_percent: 42, reset_at: '' } },
    quota_mode: 'weekly_percent',
    quota_type: 'paid',
    quota_source: 'upstreamBilling',
    quota_confidence: 'confirmed',
    quota_limit_known: true,
  });

  const quota = context.getQuotaStats(acc);
  assert.equal(quota.weeklyPercent, true);
  assert.equal(quota.used, 42);
  assert.equal(quota.estimated, undefined);
  assert.match(context.buildQuotaMarkup(acc), /58%/, 'the reported window is shown as a percentage');
});

test('sidebar and account rows use the same quota even when older counters disagree', () => {
  const sidebar = load({ accountPage: false });
  const page = load();
  const cases = [
    { account: buildFreeAccount({ has_credential: true, quota_remaining: 0,
      grok_billing: { weekly: { has_usage: true, usage_percent: 42 } } }), remaining: 58, exhausted: false },
    { account: buildFreeAccount({ has_credential: true, status_code: '429',
      grok_billing: { weekly: { has_usage: true, usage_percent: 100 } } }), remaining: 0, exhausted: true },
    { account: buildFreeAccount({ has_credential: true, quota_limit: 0, quota_supported: false,
      quota_confidence: '', usage_limit: 100, usage_current: 0 }), remaining: 0, exhausted: false },
    { account: { account_type: 'cline', enabled: true, has_credential: true,
      usage_limit: 100, usage_current: 0 }, remaining: 0, exhausted: false },
    { account: { account_type: 'cline', enabled: true, has_credential: true,
      quota_supported: true, quota_limit: 1000, quota_remaining: 0,
      status_code: 'cline_quota_exhausted' }, remaining: 0, exhausted: true },
    { account: { account_type: 'qoder', enabled: true, has_credential: true,
      quota_supported: true, quota_limit: 0, quota_remaining: 0 }, remaining: 0, exhausted: true },
  ];
  for (const { account, remaining, exhausted } of cases) {
    assert.deepEqual(JSON.parse(JSON.stringify(sidebar.getQuotaStats(account))),
      JSON.parse(JSON.stringify(page.getQuotaStats(account))));
    assert.equal(sidebar.getQuotaStats(account).remaining, remaining);
    assert.equal(sidebar.isQuotaOnlyStatus(account), exhausted);
    assert.equal(Boolean(page.evaluateAccountStatus(account).quotaOnly), exhausted);
    assert.equal(sidebar.isSidebarAccountAbnormal(account), page.isAccountAbnormal(account));
  }
});
