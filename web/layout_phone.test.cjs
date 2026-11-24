const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const DRAWER_BREAKPOINT = 900;

// Every `@media (max-width: Npx) { ... }` block in a stylesheet, with its width and
// body, by source order. These stylesheets nest no media queries, so brace counting
// is enough.
function mediaBlocks(source) {
  const blocks = [];
  const opener = /@media[^{]*max-width:\s*(\d+)px[^{]*\{/g;
  let match;
  while ((match = opener.exec(source)) !== null) {
    let depth = 1;
    let i = opener.lastIndex;
    while (i < source.length && depth > 0) {
      if (source[i] === '{') depth++;
      else if (source[i] === '}') depth--;
      i++;
    }
    blocks.push({ width: Number(match[1]), source: match[0], body: source.slice(opener.lastIndex, i - 1) });
  }
  return blocks;
}

test('every admin page links a cache-busted main.css', () => {
  const pageDir = path.join(__dirname, 'templates', 'pages');
  const pages = fs.readdirSync(pageDir).filter((f) => f.endsWith('.html'));
  assert.ok(pages.length > 0, 'page templates exist');

  // main.css is served with `immutable` when the URL carries a version, so a
  // stylesheet fix that does not change the query string never reaches a browser
  // that already fetched the broken bytes. The version is therefore never written
  // by hand: page templates read the content hash from PageData, and the static
  // login page carries the placeholder web.LoginPage resolves at serve time.
  // A literal here is the bug this test exists to catch, so assert on the
  // mechanism rather than on a value.
  for (const page of pages.map((p) => path.join(pageDir, p))) {
    const html = fs.readFileSync(page, 'utf8').replace('{{template "page-styles.html" .}}', fs.readFileSync(path.join(__dirname,'templates/partials/page-styles.html'),'utf8'));
    assert.match(
      html,
      /main\.css\?v=\{\{\.AssetVersion\}\}/,
      `${path.basename(page)} links main.css through the generated asset version`
    );
    assert.doesNotMatch(
      html,
      /(?:main|accounts|models|ops|tutorial|logs|config|alerts)\.css\?v=[0-9A-Za-z._-]+"/,
      `${path.basename(page)} must not pin a hand-written asset version`
    );
  }

  const login = fs.readFileSync(path.join(__dirname, 'static', 'login.html'), 'utf8');
  assert.match(
    login,
    /main\.css\?v=__ASSET_VERSION__/,
    'the static login page carries the placeholder resolved by web.LoginPage'
  );
});

// ---------------------------------------------------------------------------
// Two boxes were sized by their content instead of by their card. Both escaped,
// and the shell's `overflow-x: hidden` (the phone rule that stops one wide row
// from zooming the whole console out) meant the extra width was silently cut
// off rather than scrolled to. Neither is visible in a DOM assertion, so these
// pin the declarations that keep the boxes inside their parents.
// ---------------------------------------------------------------------------

const opsCss = fs.readFileSync(path.join(__dirname, 'static', 'css', 'ops.css'), 'utf8');
const opsJs = fs.readFileSync(path.join(__dirname, 'static', 'js', 'ops.js'), 'utf8');
const modelsCss = fs.readFileSync(path.join(__dirname, 'static', 'css', 'models.css'), 'utf8');

test('the trend charts are width-constrained instead of sized by their aspect ratio', () => {
  const rule = opsCss.match(/\.ops-chart\s*\{([^}]*)\}/);
  assert.ok(rule, '.ops-chart is declared in ops.css');
  // Comments are stripped first: the rule documents itself, and prose about width
  // must not be mistaken for the declaration.
  const body = rule[1].replace(/\/\*[\s\S]*?\*\//g, ' ');
  // With width:auto, `aspect-ratio` + `min-height` let Chrome derive the width:
  // 2.6/1 and min-height 190px produced a 494px box inside a 336px card.
  assert.ok(
    /(^|;)\s*width:\s*100%/.test(body),
    '.ops-chart must state width:100%; otherwise min-height and aspect-ratio ' +
      'size it from the ratio (measured: 494px wide inside a 336px card)',
  );
  assert.match(body, /aspect-ratio:/, '.ops-chart still sizes its height from the ratio');
});

function renderOpsPhoneRows() {
  const make = (tag) => {
    const classes = new Set();
    return {
      tagName: tag, children: [], dataset: {}, style: {}, textContent: '', value: '',
      classList: { add: (name) => classes.add(name), remove: (name) => classes.delete(name),
        toggle: (name, on) => on ? classes.add(name) : classes.delete(name), contains: (name) => classes.has(name) },
      appendChild(child) { this.children.push(child); return child; },
      replaceChildren(...children) { this.children = children; },
      setAttribute(name, value) { this[name] = String(value); },
      getAttribute(name) { return this[name]; },
      addEventListener() {},
      querySelector(selector) {
        if (selector === 'tbody') return this.tbody ||= make('tbody');
        const descendants = (parent) => parent.children.flatMap((child) => [child, ...descendants(child)]);
        return descendants(this).find((child) => selector.startsWith('.')
          ? (child.className || '').split(' ').includes(selector.slice(1)) : child.tagName === selector) || null;
      },
    };
  };
  const nodes = new Map();
  const node = (id) => { if (!nodes.has(id)) nodes.set(id, make(id)); return nodes.get(id); };
  const overview = {
    available: true, window_minutes: 180, since: '2026-09-12T11:48:00Z', until: '2026-09-12T14:48:00Z',
    totals: {}, coverage: {}, series: [], matrix: [{
      channel: 'grok', accounts_enabled: 2, accounts_available: 1, model_cooldowns: 3,
      summary: { requests: 8, samples: 8, success_rate: 0.75 }, series: [],
      models: [{ model: 'fixture-model', requests: 8, samples: 8, success_rate: 0.75 }],
    }],
  };
  const context = vm.createContext({
    document: { readyState: 'complete', getElementById: node, querySelector: node,
      querySelectorAll: () => [], createElement: make, createElementNS: (_ns, tag) => make(tag), addEventListener() {} },
    window: { innerWidth: 390, addEventListener() {}, setTimeout },
    setInterval: () => 0, setTimeout, URLSearchParams,
    fetch: async (url) => ({ ok: true, status: 200, json: async () => url.startsWith('/api/journal/records')
      ? { data: [{ event: { action: 'alert_fired', timestamp: overview.until, channel: 'grok',
        model: 'fixture-model', error: 'fixture alert', metadata: { severity: 'warning' } } }] }
      : url.startsWith('/api/ops/overview') ? overview : {} }),
  });
  vm.runInContext(opsJs, context);
  return node;
}

test('the phone card layouts are driven by the labels the rows actually carry', async () => {
  // Execute both production branches: a matrix label elsewhere in the source
  // cannot mask a missing alert label, or vice versa.
  const node = renderOpsPhoneRows();
  for (let i = 0; i < 5; i++) await new Promise((resolve) => setImmediate(resolve));
  const matrix = node('opsMatrix').querySelector('tbody').children;
  assert.equal(matrix.length, 2, `both channel and model matrix branches render: ${node('opsStatusText').textContent}`);
  const matrixCells = [
    ['ops-mx-accounts', '可用账号'], ['ops-mx-requests', '请求'], ['ops-mx-rate', '成功率'],
    ['ops-mx-ttft', '首 Token P95'], ['ops-mx-duration', '总耗时 P95'], ['ops-mx-throttled', '限流'],
  ];
  for (const row of matrix) {
    for (const [index, [className, label]] of matrixCells.entries()) {
      assert.equal(row.children[index + 1].dataset.label, label, `matrix ${className}`);
      assert.equal(row.children[index + 1].classList.contains(className), true);
    }
  }
  const alerts = node('opsAlertTable').querySelector('tbody').children;
  assert.equal(alerts.length, 1);
  const alertCells = [
    ['ops-alert-time', '时间'], ['ops-alert-status', '状态'], ['ops-alert-level', '严重级别'],
    ['ops-alert-channel', '渠道'], ['ops-alert-target', '对象'], ['ops-alert-detail', '说明'],
  ];
  for (const [index, [className, label]] of alertCells.entries()) {
    assert.equal(alerts[0].children[index].dataset.label, label, `alert ${className}`);
    assert.ok(alerts[0].children[index].className.split(' ').includes(className));
  }
  assert.equal(alerts[0].children[5].textContent, 'fixture alert');

  const cards = mediaBlocks(opsCss).filter((b) => /\.ops-table tbody tr\s*\{[^}]*display:\s*grid/.test(b.body));
  assert.equal(cards.length, 1, 'exactly one breakpoint turns a matrix/alert row into a card');
  const card = cards[0];
  assert.match(card.body, /content:\s*attr\(data-label\)/, 'the card prints the cell label');
  assert.match(card.body, /\.ops-table thead\s*\{\s*display:\s*none/, 'the header row goes away');
  assert.match(card.body, /\.ops-table,\s*[\s\S]*?\.ops-table td\s*\{\s*display:\s*block/, 'cells stop being table cells');
  for (const cls of ['ops-alert-time', 'ops-alert-status', 'ops-alert-level', 'ops-alert-channel', 'ops-alert-target', 'ops-alert-detail']) {
    assert.ok(card.body.includes(`.${cls}`), `the phone card styles the ${cls} cell`);
  }

  // The matrix does not degrade gracefully below this width: at 700px its 40 history
  // blocks claim the room the columns need and every cell computes to 0px wide. That
  // makes the card breakpoint load bearing, so pin it to the drawer breakpoint the
  // shell already changes shape at.
  assert.equal(
    card.width,
    DRAWER_BREAKPOINT,
    `the card layout must start at the ${DRAWER_BREAKPOINT}px drawer breakpoint (got ${card.width}px); ` +
      'below it the matrix columns collapse to zero width',
  );
});

test('model rows become compact selectable cards on phones', () => {
  const phone = mediaBlocks(modelsCss).find((block) => block.width === 640);
  assert.ok(phone, 'models.css has a 640px phone breakpoint');
  assert.match(phone.body, /\.models-table tbody tr\s*\{[^}]*position:\s*relative/, 'the model card anchors its selection control');
  assert.match(phone.body, /\.models-table tbody td\.col-select\s*\{[^}]*position:\s*absolute/, 'selection no longer consumes an empty grid row');
  assert.match(phone.body, /\.models-table tbody td\.col-model\s*\{[^}]*grid-column:\s*1\s*\/\s*-1/, 'the model identity spans the card');
  assert.match(phone.body, /\.models-actions\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/, 'edit and delete share one full-width action row');
  assert.match(phone.body, /\.models-table-wrap\s*\{[^}]*max-height:\s*none/, 'the phone page scrolls as one document instead of nesting the model list');
});

