// Operations monitoring dashboard: header status, health gauge, six KPI cards,
// host/runtime resources, per-platform concurrency, throughput and switch trends,
// latency distribution, error mix/trend, alert events and the channel × model
// matrix. Every figure that has no sample says so instead of rendering a healthy
// zero.
(function () {
  const { el, make, attach, setText } = ConsoleUI;

  const state = {
    window: 180,
    channel: '',
    model: '',
    liveWindow: 1,
    refreshSeconds: 15,
    countdown: 15,
    timer: null,
    // Set when a refresh interval elapses while the tab is hidden; the work is
    // deferred to the moment the operator looks at the dashboard again.
    refreshPending: false,
    // Prevent slow overview/runtime requests from overlapping on each refresh
    // tick and multiplying server aggregation plus SVG rendering work.
    loading: false,
    overview: null,
    // outcome selects which cohort the latency cards describe: every request, the
    // ones that ended in a failure, or the ones where an upstream attempt failed
    // before a retry rescued them.
    outcome: 'all',
  };

  // OUTCOME_TABS is the 口径 selector shared by the latency cards and the
  // distribution: one control, one meaning, so "失败请求的 P95" cannot be read with
  // a different cohort in two different cards.
  const OUTCOME_TABS = [
    { key: 'all', label: '全部请求', duration: 'duration', firstToken: 'first_token', histogram: 'duration_histogram' },
    { key: 'failed', label: '最终失败', duration: 'duration_failed', firstToken: 'first_token_failed', histogram: 'duration_histogram_failed' },
    { key: 'attempt', label: '上游尝试失败', duration: 'duration_attempt', firstToken: 'first_token_attempt', histogram: 'duration_histogram_attempt' },
  ];

  // ALERT_SEVERITY_LABELS names the levels the alert engine stores. The table prints the
  // name an operator reads: it used to show the raw key, so a row read "warning" in the
  // middle of Chinese text.
  const ALERT_SEVERITY_LABELS = { critical: '严重', warning: '警告', info: '提示' };

  // ALERT_CELLS names each alert column, in the order the row builds its cells. Desktop
  // hides the distinction inside a table; a phone turns the row into a card and places
  // the cells by these classes (see the alert card rules in ops.css). The column header
  // travels with the cell as data-label so the card can print it: the phone view has no
  // header row, and two unlabelled values in a card are unreadable.
  const ALERT_CELLS = [
    { className: 'ops-alert-time', label: '时间' },
    { className: 'ops-alert-status', label: '状态' },
    { className: 'ops-alert-level', label: '严重级别' },
    { className: 'ops-alert-channel', label: '渠道' },
    { className: 'ops-alert-target', label: '对象' },
    { className: 'ops-alert-detail', label: '说明' },
  ];

  function outcomeTab(key) { return OUTCOME_TABS.filter((tab) => tab.key === key)[0] || OUTCOME_TABS[0]; }

  // The page state lives in the URL, so these two accessors are the only places
  // that touch window.location. Keeping them here means the render tests (which
  // stub a DOM without a location) exercise the same code as a browser.
  function pagePath() { return (window.location && window.location.pathname) || ''; }

  function pageSearch() { return (window.location && window.location.search) || ''; }

  // windowStart/untilISO turn the selected window into the absolute range a
  // drill-down hands to the log centre. The chart and the list must cover the same
  // minutes, so the range is derived once here instead of in each click handler.
  function windowRange() {
    const overview = state.overview;
    if (overview && Number(overview.window_minutes) === state.window) {
      const since = new Date(overview.since);
      const until = new Date(overview.until);
      if (Number.isFinite(since.getTime()) && Number.isFinite(until.getTime()) && since <= until) return { since, until };
    }
    const until = new Date();
    const since = new Date(until.getTime() - state.window * 60 * 1000);
    return { since, until };
  }

  function drilldownToLogs(extra) {
    const range = windowRange();
    // The chart covers its whole window from the aggregates; the list covers only what
    // the journal still holds. When the window starts before the oldest retained
    // record, say so at the entry point: otherwise a short list reads as "there were
    // no failures" for a stretch that was never in it.
    const gap = journalCoverageGap(range.since);
    if (gap) showToast(gap, 'info');
    const params = assignParams(new URLSearchParams({ tab: 'logs', kind: 'request' }), {
      since: range.since.toISOString(), until: range.until.toISOString(), channel: state.channel, model: state.model });
    assignParams(params, extra);
    // The overview's own scope travels with the link, so the log centre can offer
    // "返回" and land on exactly the view that was left.
    params.set('back', currentOpsQuery());
    if (window.location) window.location.href = pagePath() + '?' + params.toString();
  }

  // journalCoverageGap describes the part of the window the journal cannot list, or ""
  // when it covers the whole window. The overview payload carries the coverage, so the
  // warning costs no extra request.
  function journalCoverageGap(since) {
    const coverage = (state.overview && state.overview.coverage) || null;
    if (!coverage || !coverage.oldest || !since) return '';
    const oldest = new Date(coverage.oldest);
    if (Number.isNaN(oldest.getTime()) || since >= oldest) return '';
    return '日志只保留到 ' + fmtMinute(oldest.toISOString()) + ' 之后：更早的部分只在图表统计里，下钻列表不会包含。';
  }

  // currentOpsQuery is the overview's state as a query string. It is the return
  // address: the console navigates between tabs with a full page load, so an
  // in-memory "previous view" would not survive the trip.
  function currentOpsQuery() {
    const params = assignParams(new URLSearchParams({ tab: 'ops', window: String(state.window) }), {
      channel: state.channel,
      model: state.model,
      // The default cohort is absent from the URL: a bookmark without it must read
      // as "all requests", not as a stuck selection.
      outcome: state.outcome === 'all' ? '' : state.outcome,
    });
    return pagePath() + '?' + params.toString();
  }

  // Keep bookmarks and the log-centre return link on the same scope.
  function syncUrl() {
    if (window.history && window.history.replaceState) window.history.replaceState(null, '', currentOpsQuery());
  }

  function readUrlState() {
    const params = new URLSearchParams(pageSearch());
    const minutes = Number(params.get('window'));
    if (Number.isFinite(minutes) && minutes > 0) state.window = minutes;
    state.channel = params.get('channel') || '';
    state.model = params.get('model') || '';
    const outcome = params.get('outcome');
    if (outcome && OUTCOME_TABS.some((tab) => tab.key === outcome)) state.outcome = outcome;
  }





  // --- element builders ------------------------------------------------------
  // Nearly every element this page draws is the same three statements (create,
  // class, text). These own that shape so the render functions read as WHAT they
  // draw instead of how, and so a class name is written once.




  // An empty box and a broken one look alike, so both say which they are. Every
  // empty chart, table and resource row goes through these two.
  function emptyNote(container, text, className) {
    container.replaceChildren();
    container.appendChild(make('p', className || 'ops-empty', text));
    return container;
  }

  function emptyRow(body, colSpan, text) {
    const td = make('td', 'table-empty-cell', text);
    td.colSpan = colSpan;
    body.replaceChildren();
    body.appendChild(attach(make('tr'), [td]));
  }

  // attachHandler wires one control to one handler. Every control is optional
  // (the markup is shared with the smaller pages), so the "does it exist" guard
  // lives here instead of at each of the twenty call sites.
  function attachHandler(id, event, handler) {
    const node = el(id);
    if (node) node.addEventListener(event, handler);
  }

  // makeActivatable turns a node into a control: click, Enter and Space all run
  // the same action. A chart bar and a matrix cell look nothing alike but must be
  // reachable the same way, so the keyboard contract lives in one place.
  function makeActivatable(node, action) {
    node.setAttribute('tabindex', '0');
    node.setAttribute('role', 'button');
    node.classList.add('is-clickable');
    node.addEventListener('click', action);
    node.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        action();
      }
    });
    return node;
  }

  // assignParams copies the scope fields that are set into a query string. A
  // filter that is unset must stay absent: the log centre treats "" and "missing"
  // the same way only because nothing ever sends an empty value.
  function assignParams(params, source) {
    Object.keys(source || {}).forEach((key) => {
      const value = source[key];
      if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
    });
    return params;
  }

  function fmtInt(value) { return Number(value || 0).toLocaleString('zh-CN'); }

  function fmtAmount(value) {
    const num = Number(value || 0);
    if (num >= 1e8) return (num / 1e8).toFixed(2) + ' 亿';
    if (num >= 1e4) return (num / 1e4).toFixed(1) + ' 万';
    return fmtInt(num);
  }

  function fmtRate(rate, samples) {
    if (!samples) return '暂无样本';
    return (rate * 100).toFixed(3) + '%';
  }

  function fmtMs(value, samples) {
    if (!samples || !value) return '暂无样本';
    return value + ' ms';
  }

  function fmtClock(date) { return date.toLocaleTimeString('zh-CN', { hour12: false }); }

  function fmtMinute(iso) {
    if (!iso) return '';
    const date = new Date(iso);
    if (Number.isNaN(date.getTime())) return String(iso).slice(11, 16);
    return String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
  }

  function emptyChart(container, text) { emptyNote(container, text, 'ops-chart-empty'); }

  // --- charts (inline SVG; no chart library) --------------------------------

  const SVG_NS = 'http://www.w3.org/2000/svg';

  // The chart boxes are sized by CSS (aspect-ratio with a min/max clamp — see
  // .ops-chart in ops.css), so the drawing has to measure the box it landed in
  // instead of assuming a height. Drawing at the measured pixel size is what
  // keeps the 9px axis labels 9px: the old fixed `width = 640` viewBox blown up
  // to 1100 CSS pixels stretched every glyph and every stroke by 1.7x.
  function chartBox(container, fallbackHeight) {
    const rect = container && typeof container.getBoundingClientRect === 'function'
      ? container.getBoundingClientRect()
      : null;
    const width = Math.max(240, Math.round((rect && rect.width) || 0) || 640);
    const height = Math.max(120, Math.round((rect && rect.height) || 0) || fallbackHeight);
    return { width, height };
  }

  // plotBox is the drawing area left inside a measured box after the axis gutters
  // are taken off. Every chart below works in these five numbers, so none of them
  // re-derives "where does the plot start" from its own padding constants.
  function plotBox(container, fallbackHeight, pad) {
    const box = chartBox(container, fallbackHeight);
    return { width: box.width, height: box.height, left: pad.left, right: box.width - pad.right, top: pad.top, plotW: box.width - pad.left - pad.right, plotH: box.height - pad.top - pad.bottom };
  }

  function svgEl(name, attrs) {
    const node = document.createElementNS(SVG_NS, name);
    Object.keys(attrs || {}).forEach((key) => node.setAttribute(key, String(attrs[key])));
    return node;
  }

  // gridLines draws the four horizontal guides, and axisLabels the numbers beside
  // them. They are separate because a second series adds its own labels to the
  // right without redrawing the guides over them.
  function gridLines(svg, box) {
    for (let i = 0; i <= 3; i += 1) {
      const y = box.top + (box.plotH / 3) * i;
      svg.appendChild(svgEl('line', { x1: box.left, y1: y, x2: box.right, y2: y, class: 'grid-line' }));
    }
  }

  // A peak under 10 (a quiet window's QPS) would round every tick to the same
  // integer, so the decimals follow the peak rather than being fixed.
  function axisLabels(svg, box, max, x, format) {
    for (let i = 0; i <= 3; i += 1) {
      const label = svgEl('text', { x, y: box.top + (box.plotH / 3) * i + 3, class: 'axis-text' });
      const value = max * (1 - i / 3);
      label.textContent = format ? format(value) : value.toFixed(max < 10 ? 2 : 0);
      svg.appendChild(label);
    }
  }

  // polyPath turns coordinates into a path. A gap — a minute with no sample —
  // breaks the line instead of bridging it with a straight segment that reads as
  // a measured value: every null restarts the path with M.
  function polyPath(coords, values) {
    const parts = [];
    coords.forEach((coord, index) => {
      const value = values ? values[index] : coord.value;
      if (value === null || value === undefined) return;
      const command = parts.length === 0 || (values && values[index - 1] == null) ? 'M' : 'L';
      parts.push(`${command}${coord.x.toFixed(1)},${coord.y.toFixed(1)}`);
    });
    return parts.join(' ');
  }

  // timeAxis prints at most six minute labels plus the last one: forty labels for
  // forty buckets is a grey band, not an axis.
  function timeAxis(svg, box, points, xOf) {
    const every = Math.max(1, Math.ceil(points.length / 6));
    points.forEach((point, index) => {
      if (index % every !== 0 && index !== points.length - 1) return;
      const text = svgEl('text', { x: xOf(index), y: box.height - 5, class: 'axis-text', 'text-anchor': 'middle' });
      text.textContent = point.label;
      svg.appendChild(text);
    });
  }

  function chartSvg(box, stretch) {
    const attrs = { viewBox: `0 0 ${box.width} ${box.height}` };
    if (stretch) attrs.preserveAspectRatio = 'none';
    return svgEl('svg', attrs);
  }

  // lineChart draws one or two series with a left axis, a grid and time labels.
  function lineChart(container, options) {
    const points = options.points || [];
    if (!points.length || !points.some((p) => p.value != null)) {
      emptyChart(container, options.emptyText || '这段时间没有样本。');
      return;
    }
    const box = plotBox(container, options.height || 150, {
      left: 34, right: options.rightAxis ? 34 : 10, top: 8, bottom: 20,
    });
    const maxValue = Math.max(options.maxValue || 0, ...points.map((p) => p.value), 0.0001);
    const svg = chartSvg(box, true);
    gridLines(svg, box);
    axisLabels(svg, box, maxValue, 4, options.axisFormat);

    const step = box.plotW / Math.max(points.length - 1, 1);
    const coords = points.map((point, index) => ({ x: points.length === 1 ? box.left + box.plotW / 2 : box.left + step * index, y: box.top + box.plotH - (point.value / maxValue) * box.plotH }));

    if (options.area) {
      const baseline = (box.top + box.plotH).toFixed(1);
      svg.appendChild(svgEl('path', {
        d: `${polyPath(coords, points.map((point) => point.value))} L${coords[coords.length - 1].x.toFixed(1)},${baseline} L${coords[0].x.toFixed(1)},${baseline} Z`,
        class: 'series-area',
      }));
    }

    svg.appendChild(svgEl('path', { d: polyPath(coords, points.map((point) => point.value)), class: options.className || 'series-qps' }));
    // A sample alone in a gap would be invisible without a dot: a one-point path
    // has no segment to draw.
    coords.forEach((coord, index) => {
      const value = points[index].value;
      if (value == null) return;
      if (!options.tooltips && (index > 0 && points[index - 1].value != null) && (index < points.length - 1 && points[index + 1].value != null)) return;
      const dot = svgEl('circle', { cx: coord.x, cy: coord.y, r: 3, fill: options.pointColor || 'var(--accent)' });
      if (options.tooltips) { const title = svgEl('title'); title.textContent = points[index].tooltip || points[index].label; dot.appendChild(title); }
      svg.appendChild(dot);
    });

    // Second series on its own scale: QPS and TPS differ by orders of magnitude,
    // so sharing one axis would flatten one of them into the baseline.
    if (options.secondaryPoints && options.secondaryPoints.length === points.length) {
      const secondaryMax = Math.max(options.secondaryMax || 0, ...options.secondaryPoints.map((p) => p.value), 0.0001);
      const secondaryCoords = options.secondaryPoints.map((point, index) => ({
        x: points.length === 1 ? box.left + box.plotW / 2 : box.left + step * index,
        y: box.top + box.plotH - (point.value / secondaryMax) * box.plotH,
      }));
      svg.appendChild(svgEl('path', { d: polyPath(secondaryCoords, options.secondaryPoints.map((point) => point.value)), class: 'series-tps' }));
      axisLabels(svg, box, secondaryMax, box.right + 4);
    }

    timeAxis(svg, box, points, (index) => coords[index].x);
    container.replaceChildren();
    container.appendChild(svg);
  }

  // barChart renders a per-minute bar series (errors, sparkline ticks). An optional
  // overlay draws a second series on the same scale, and onSelect makes each bar
  // open the log centre scoped to that minute.
  function barChart(container, points, options) {
    if (!points.length) {
      emptyChart(container, (options && options.emptyText) || '这段时间没有样本。');
      return;
    }
    const opts = options || {};
    const box = plotBox(container, opts.height || 150, { left: 30, right: 8, top: 8, bottom: 20 });
    const overlay = opts.overlay || [];
    const maxValue = Math.max(1, ...points.map((p) => p.value), ...overlay.map((p) => p.value));
    const svg = chartSvg(box, true);
    svg.appendChild(svgEl('line', { x1: box.left, y1: box.top, x2: box.left, y2: box.top + box.plotH, class: 'grid-line' }));
    const barStep = box.plotW / points.length;
    const barW = Math.max(0.2, barStep - Math.min(2, barStep * 0.2));
    points.forEach((point, index) => {
      const barH = Math.max(point.value > 0 ? 3 : 0.6, (point.value / maxValue) * box.plotH);
      const bar = svgEl('rect', {
        x: box.left + barStep * index,
        y: box.top + box.plotH - barH, width: barW, height: barH, class: (point.value > 0 || !opts.markZero) && opts.errorBars ? 'bar is-error' : 'bar' });
      bar.appendChild(svgEl('title', {})).textContent = `${point.label}：${point.value}`;
      if (opts.onSelect) {
        // A chart that cannot be interrogated is a poster: the bar carries the
        // minute it belongs to into the log centre.
        makeActivatable(bar, () => opts.onSelect(index));
      }
      svg.appendChild(bar);
    });
    if (overlay.length === points.length) {
      // The attempt-failure series shares the axis (both are request counts), so it
      // is drawn as a line over the bars rather than as a second scale.
      const coords = overlay.map((point, index) => ({ x: box.left + barStep * index + barW / 2, y: box.top + box.plotH - (point.value / maxValue) * box.plotH }));
      svg.appendChild(svgEl('polyline', { points: coords.map((c) => `${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' '), class: 'series-alt' }));
    }
    timeAxis(svg, box, points, (index) => box.left + barStep * index + barW / 2);
    container.replaceChildren();
    container.appendChild(svg);
  }

  // --- KPI cards -------------------------------------------------------------

  function kpiCard(title, tools) {
    const card = make('article', 'ops-kpi-card');
    const head = attach(make('div', 'ops-kpi-head'), [make('span', '', title), tools ? make('span', 'ops-hint', tools) : null]);
    card.appendChild(head);
    return card;
  }

  function bigValue(card, value, unit, tone) {
    const node = make('div', 'ops-kpi-big' + (tone ? ' ' + tone : ''), value);
    if (unit) node.appendChild(make('span', 'ops-kpi-unit', unit));
    card.appendChild(node);
    return node;
  }

  function rowList(card, rows) {
    const list = make('div', 'ops-kpi-rows');
    rows.forEach((entry) => {
      const row = make('div', 'ops-kpi-row' + (entry.strong ? ' is-strong' : ''));
      attach(row, [make('span', '', entry.label), make('span', '', entry.value)]);
      list.appendChild(row);
    });
    card.appendChild(list);
    return list;
  }

  // meter draws a fill bar clamped to 0–100%. A ratio above 1 would otherwise
  // push the fill out of its track and look like a broken card.
  function meter(card, ratio, tone) {
    const wrap = make('div', 'ops-kpi-meter');
    const fill = make('span', tone || '');
    fill.style.width = Math.max(0, Math.min(100, ratio * 100)).toFixed(1) + '%';
    wrap.appendChild(fill);
    card.appendChild(wrap);
  }

  function percentileRows(set) {
    const p = set || {};
    return ['P95', 'P90', 'P50', 'Avg', 'Max'].map((label) => ({
      label,
      value: fmtMs(p[label === 'Avg' ? 'avg_ms' : label.toLowerCase() + '_ms'], p.samples),
    }));
  }

  function renderSkeletons() {
    const container = el('opsKpis');
    if (!container || container.childElementCount) return;
    for (let i = 0; i < 6; i += 1) {
      const card = make('article', 'ops-kpi-card is-loading');
      ['w-40', 'w-70', 'w-40'].forEach((width) => card.appendChild(make('div', 'skeleton-line ' + width)));
      container.appendChild(card);
    }
  }

  // outcomeTabs builds the 口径 selector. It lives in the latency card and drives
  // that card, the TTFT card and the distribution together, which is why it is
  // labelled with what it affects instead of repeating itself in four cards.
  function outcomeTabs(onChange) {
    const wrap = make('span', 'ops-tabs');
    wrap.id = 'opsOutcomeTabs';
    OUTCOME_TABS.forEach((tab) => {
      const active = state.outcome === tab.key;
      const button = make('button', 'ops-chip' + (active ? ' is-active' : ''), tab.label);
      button.type = 'button';
      button.setAttribute('data-outcome', tab.key);
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
      button.addEventListener('click', () => {
        state.outcome = tab.key;
        syncUrl();
        onChange();
      });
      wrap.appendChild(button);
    });
    return wrap;
  }

  function renderKpis(payload) {
    const container = el('opsKpis');
    if (!container) return;
    container.replaceChildren();
    const totals = payload.totals || {};
    const real = Math.max(totals.requests || 0, 0);

    if (payload.available === false) {
      const card = kpiCard('指标聚合');
      bigValue(card, '未启用', '', 'is-muted');
      rowList(card, [{ label: '原因', value: payload.note || '需要 Redis' }]);
      container.appendChild(card);
      return;
    }

    // 请求
    const requests = kpiCard('请求', payload.window_minutes + ' 分钟窗口');
    bigValue(requests, fmtInt(real));
    rowList(requests, [
      { label: 'Token 数', value: fmtAmount((totals.input_tokens || 0) + (totals.output_tokens || 0)) },
      { label: '平均 QPS', value: (totals.qps != null ? Number(totals.qps) : Number(totals.rpm || 0) / 60).toFixed(2) + ' 次/秒' },
      { label: '平均 TPS', value: totals.usage_samples > 0 || totals.input_tokens > 0 || totals.output_tokens > 0 || real === 0 ? Number(totals.tps || 0).toFixed(1) + ' token/秒' : '未采集' },
    ]);
    if (real > 0) rowList(requests, [{ label: '用量覆盖', value: fmtInt(totals.usage_samples || 0) + ' / ' + fmtInt(real) + ' 次请求' }]);
    if (real > 0 && Number(totals.detailed_requests || 0) < real) rowList(requests, [{ label: '数据覆盖', value: '窗口含旧数据，部分用量和错误分类未采集' }]);
    container.appendChild(requests);

    // SLA. Throttling (429/529), a credential our own gate refused (401/403) and an
    // exhausted quota (402) are all business limits: none of them is evidence that
    // the provider cannot serve. The server computes the ratio AND the denominator
    // it used, so the card prints both instead of recomputing and drifting.
    const limited = (totals.rate_limited || 0) + (totals.rejected || 0) + (totals.quota_exhausted || 0);
    // Older metric buckets predate the outcome breakdown. Their success rate
    // was calculated over all real requests, which is the compatible fallback.
    const attributable = totals.attributable == null ? real : Number(totals.attributable || 0);
    const slaSuccess = Number(totals.success || 0);
    const slaRate = attributable > 0 ? Number(totals.sla_success_rate ?? totals.success_rate ?? 0) : 0;
    const sla = kpiCard('SLA（排除业务限制）', Number(totals.detailed_requests || 0) < real ? '旧数据未分类，按全部请求计' : '成功率口径');
    bigValue(sla, fmtRate(slaRate, attributable), '', attributable === 0 ? 'is-muted' : (slaRate >= 0.95 ? 'is-ok' : slaRate >= 0.8 ? 'is-warn' : 'is-error'));
    meter(sla, slaRate, slaRate >= 0.95 ? '' : slaRate >= 0.8 ? 'is-warn' : 'is-error');
    rowList(sla, [
      { label: '成功数', value: fmtInt(slaSuccess) },
      { label: '输出证据样本', value: fmtInt(totals.output_evidence_samples || 0) },
      { label: '可见答案 / 仅推理', value: fmtInt(totals.visible_answers || 0) + ' / ' + fmtInt(totals.reasoning_only || 0) },
      { label: '输出预算截断', value: fmtInt(totals.output_truncated || 0) },
      // This row counts failed REQUESTS in the selected window, not accounts.
      // It used to read 异常数, the same word the sidebar uses for the account
      // counter, so an operator comparing the two saw 11 against 5 and could not
      // tell that they were never the same metric.
      { label: '失败请求数', value: fmtInt(totals.failed || 0) },
      { label: '业务限制', value: fmtInt(limited) + '（限流 ' + fmtInt(totals.rate_limited || 0) + ' / 额度 ' + fmtInt(totals.quota_exhausted || 0) + ' / 网关拒绝 ' + fmtInt(totals.rejected || 0) + '）' },
    ]);
    container.appendChild(sla);

    // Request errors
    const errorRate = real > 0 ? (totals.failed || 0) / real : 0;
    const errors = kpiCard('请求错误', '包含业务限制');
    bigValue(errors, real === 0 ? '暂无样本' : (errorRate * 100).toFixed(2) + '%', '', real === 0 ? 'is-muted' : errorRate > 0.1 ? 'is-error' : errorRate > 0.01 ? 'is-warn' : 'is-ok');
    rowList(errors, [
      { label: '最终失败', value: fmtInt(totals.failed || 0) },
      { label: '上游尝试失败', value: fmtInt(totals.attempt_failures || 0) },
      { label: '流中断', value: fmtInt(totals.stream_errors || 0) },
      { label: '业务限制', value: fmtInt(limited) },
    ]);    container.appendChild(errors);

    // Request duration, with the cohort selector
    const tab = outcomeTab(state.outcome);
    // cohortSet reads the percentile set for the selected cohort. Payloads that
    // predate the outcome breakdown carry only the all-requests P95, so it is the
    // fallback for that cohort only — never for "failed", where it would answer a
    // question nobody asked with a number about a different set of requests.
    function cohortSet(key, legacyKey) {
      return totals[key] || (state.outcome === 'all' && totals[legacyKey]
        ? { p95_ms: totals[legacyKey], samples: totals.samples || 0 }
        : {});
    }
    // latencyCard draws one percentile card. The duration and TTFT cards differ
    // only in their headline, their sample note and which of them carries the 口径
    // selector; one builder is what keeps "P95 of failed requests" meaning the
    // same thing in both.
    function latencyCard(title, tools, set, options) {
      const card = kpiCard(title, tools);
      if (options.tabs) card.querySelector('.ops-kpi-head').appendChild(outcomeTabs(() => renderKpis(payload)));
      if (options.note) card.title = options.note;
      bigValue(card, fmtMs(set.p99_ms ?? set.p95_ms, set.samples), set.p99_ms == null ? 'P95' : 'P99', set.samples ? '' : 'is-muted');
      rowList(card, percentileRows(set));
      return card;
    }
    const duration = cohortSet(tab.duration, 'duration_p95_ms');
    container.appendChild(latencyCard('请求时长', (duration.samples || 0) + ' 个样本', duration, { tabs: true }));

    // Time to first token, same cohort
    const firstToken = cohortSet(tab.firstToken, 'first_token_p95_ms');
    container.appendChild(latencyCard('TTFT', '口径：' + tab.label, firstToken, {
      note: '流式：首次文本、思考或工具内容；非流式：首响应字节。未产生流内容的请求不计入 TTFT 样本。',
    }));

    // Upstream errors: the two classes that are the provider's side of the
    // failure. A 4xx is the caller's fault and a rate limit is a business limit,
    // so both are shown but neither is added to the headline number.
    const upstream = kpiCard('上游错误', '排除 4xx 与 429 / 529');
    const upstreamCount = (totals.server_errors || 0) + (totals.stream_errors || 0);
    bigValue(upstream, String(upstreamCount), '次', upstreamCount === 0 ? 'is-ok' : 'is-error');
    rowList(upstream, [
      { label: '5xx（上游错误）', value: fmtInt(totals.server_errors || 0) },
      { label: '流中断（已提交 2xx）', value: fmtInt(totals.stream_errors || 0) },
      { label: '上游认证失败（账号被上游拒绝）', value: fmtInt(totals.upstream_auth || 0) },
      { label: '4xx（客户端，非限流）', value: fmtInt(totals.client_errors || 0) },
      { label: '429 / 529 限流', value: fmtInt(totals.rate_limited || 0) },
      { label: '网关拒绝 401/403（我方拒绝，非上游故障）', value: fmtInt(totals.rejected || 0) },
      { label: '额度用尽 402', value: fmtInt(totals.quota_exhausted || 0) },
    ]);
    container.appendChild(upstream);
  }

  // --- hero gauge and live figures ------------------------------------------

  function liveBuckets(minutes) {
    if (!state.overview || state.overview.available === false) return [];
    const serverTime = Date.parse(state.overview.until || '');
    const until = Number.isFinite(serverTime) ? serverTime : Date.now();
    const lastMinute = Math.floor(until / 60000) * 60000;
    const byMinute = new Map((state.overview.series || []).map(point => [Date.parse(point.minute), point]));
    return Array.from({ length: minutes }, (_, index) => {
      const minute = lastMinute - (minutes - 1 - index) * 60000;
      return { minute: new Date(minute).toISOString(), requests: 0, input_tokens: 0, output_tokens: 0,
        ...(byMinute.get(minute) || {}), seconds: minute === lastMinute ? Math.max(1, (until - minute) / 1000) : 60 };
    });
  }

  function trendBuckets() {
    const payload = state.overview;
    if (!payload || payload.available === false) return [];
    const until = Date.parse(payload.until || '');
    const since = Date.parse(payload.since || '');
    const minutes = Number.isFinite(since) && Number.isFinite(until)
      ? Math.floor(until / 60000) - Math.floor(since / 60000) + 1
      : Number(payload.window_minutes || state.window) + 1;
    return liveBuckets(Math.max(1, Math.min(1440, minutes)));
  }

  function renderHero(payload) {
    state.overview = payload;
    if (payload.available === false) {
      ['Qps', 'Tps'].forEach((metric) => ['Now', 'Peak', 'Avg'].forEach((period) => setText('opsLive' + metric + period, '未采集')));
      setText('opsGaugeValue', '—');
      setText('opsGaugeLabel', '未采集');
      setText('opsGaugeState', '指标不可用');
      setText('opsGaugeSub', '等待有效数据');
      setText('opsHeroHint', '当前未获得指标数据');
      setText('opsSuccessTrendHint', '成功率趋势 · 未采集');
      const gauge = el('opsGaugeRing');
      if (gauge) gauge.style.background = 'var(--surface-3)';
      const spark = el('opsSpark');
      if (spark) emptyNote(spark, '指标未采集');
      return;
    }
    const buckets = liveBuckets(state.liveWindow);
    // rateSeries turns the minute buckets into one line of points: per-second
    // rates, because the bucket still in progress covers fewer than 60 seconds.
    function rateSeries(tokenSum) {
      return buckets.map((point) => ({ label: fmtMinute(point.minute), value: tokenSum(point) / point.seconds }));
    }
    const qpsPoints = rateSeries((point) => point.requests);
    const tpsPoints = rateSeries((point) => (point.input_tokens || 0) + (point.output_tokens || 0));
    const seconds = buckets.reduce((sum, point) => sum + point.seconds, 0);
    const requests = buckets.reduce((sum, point) => sum + point.requests, 0);
    const tokens = buckets.reduce((sum, point) => sum + (point.input_tokens || 0) + (point.output_tokens || 0), 0);
    const hasUsage = requests === 0 || tokens > 0 || buckets.some((point) => point.usage_samples > 0);
    // Three live figures per metric: the bucket in progress, the busiest bucket,
    // and the mean over the elapsed seconds. All three read 未采集 rather than 0.00
    // when no bucket was collected: an idle console is not a zero-rate console.
    function liveFigures(prefix, points, total, digits) {
      const peak = points.reduce((max, point) => Math.max(max, point.value), 0);
      const latest = points.length ? points[points.length - 1].value : 0;
      const avg = seconds ? total / seconds : 0;
      const shown = (value) => (points.length ? value.toFixed(digits) : '未采集');
      setText(prefix + 'Now', shown(latest));
      setText(prefix + 'Peak', shown(peak));
      setText(prefix + 'Avg', shown(avg));
    }
    liveFigures('opsLiveQps', qpsPoints, requests, 2);
    // The token rate is withheld entirely when nothing reported usage: a partial
    // TPS would read as the whole window's and quietly understate the traffic.
    liveFigures('opsLiveTps', hasUsage ? tpsPoints : [], tokens, 1);
    setText('opsHeroHint', `最近 ${state.liveWindow} 个分钟桶 · 当前分钟按已过时间计算 · 15 秒刷新`);

    // Health: the SLA of the window, with the traffic level deciding whether the
    // console is "serving" or "standby".
    const real = requests;
    const success = buckets.reduce((sum, point) => sum + (point.success || 0), 0);
    const failed = buckets.reduce((sum, point) => sum + (point.failed || 0), 0);
    const attemptFailures = buckets.reduce((sum, point) => sum + (point.attempt_failures || 0), 0);
    const rate = real > 0 ? Math.min(success / real, 1) : 0;
    const gauge = el('opsGaugeRing');
    const score = real > 0 ? rate * 100 : 0;
    const tone = real === 0 ? 'var(--idle)' : rate >= 0.95 ? 'var(--ok)' : rate >= 0.8 ? 'var(--warn)' : 'var(--bad)';
    if (gauge) {
      // Painted inline: the ring's fill angle is data, not a style rule.
      gauge.style.background = `conic-gradient(${tone} ${score.toFixed(1)}%, var(--surface-3) 0)`;
    }
    setText('opsGaugeValue', real === 0 ? '待机' : (rate * 100).toFixed(1) + '%');
    setText('opsGaugeLabel', real === 0 ? '无流量' : rate >= 0.95 ? '健康' : rate >= 0.8 ? '降级' : '异常');
    setText('opsGaugeState', real === 0 ? '待机' : '服务中');
    // The gauge's sub-line carries the two failure figures, so
    // the health number can be reconciled with the error cards at a glance.
    const gaugeSub = el('opsGaugeSub');
    if (gaugeSub) {
      gaugeSub.replaceChildren();
      [
        `${state.liveWindow} 分钟桶 ${fmtInt(real)} 次请求`,
        `最终失败 ${fmtInt(failed)}`,
        `上游尝试失败 ${fmtInt(attemptFailures)}`,
      ].forEach((part) => gaugeSub.appendChild(make('span', '', part)));
      if (failed > 0) {
        // A click target on the number an operator would reach for anyway.
        const link = make('button', 'ops-inline-link', '查看失败请求 →');
        link.type = 'button';
        link.id = 'opsHeroUpstreamErrors';
        link.addEventListener('click', () => drilldownToLogs({ outcome: 'failed', since: buckets[0].minute, until: payload.until }));
        gaugeSub.appendChild(link);
      }
    }

    setText('opsSuccessTrendHint', `成功率趋势 · ${state.liveWindow} 分钟桶 · 无请求的分钟留空`);
    const trend = el('opsSpark');
    if (trend) lineChart(trend, {
      points: buckets.map(point => ({
        label: fmtMinute(point.minute),
        value: point.requests > 0 ? Math.min((point.success || 0) / point.requests, 1) * 100 : null,
        tooltip: `${fmtMinute(point.minute)} · ${point.requests > 0 ? (((point.success || 0) / point.requests) * 100).toFixed(1) : '—'}% · ${point.success || 0} / ${point.requests} 次成功`,
      })),
      maxValue: 100, axisFormat: value => value.toFixed(0) + '%',
      className: 'series-success', pointColor: 'var(--ok)', tooltips: true,
      height: 140, emptyText: '此时间范围没有请求，暂无成功率样本。',
    });
  }

  // --- resource row ----------------------------------------------------------

  function renderResources(payload) {
    const container = el('opsResources');
    if (!container) return;
    const metrics = payload && payload.available !== false ? (payload.metrics || []) : [];
    if (!metrics.length) {
      emptyNote(container, (payload && payload.note) || '运行时指标不可用。');
      return;
    }
    container.replaceChildren();
    metrics.forEach((metric) => {
      // A metric whose collector stopped keeps its row and says 不可用: a row that
      // silently disappeared reads as "this host has no such metric".
      const card = make('article', 'ops-resource is-' + (metric.status || 'unknown'));
      attach(card, [
        make('div', 'ops-resource-head', metric.available ? metric.label : metric.label + '（不可用）'),
        make('div', 'ops-resource-value', metric.available ? metric.value : '不可用'),
        make('div', 'ops-resource-detail', [metric.detail, metric.thresholds].filter(Boolean).join(' · ')),
      ]);
      container.appendChild(card);
    });
  }

  // --- concurrency -----------------------------------------------------------

  function renderConcurrency(payload) {
    const container = el('opsConcurrency');
    if (!container) return;
    const rows = (payload.matrix || []).filter((row) => IsProvider(row.channel));
    const head = attach(make('div', 'ops-platform-head', '按平台'), [make('span', '', `共 ${rows.length} 项`)]);
    container.replaceChildren();
    container.appendChild(head);

    if (!rows.length) {
      emptyNote(container, '还没有渠道数据。', 'ops-platform-empty');
      return;
    }
    rows.forEach((row) => {
      const enabled = row.accounts_enabled || 0;
      const available = row.accounts_available || 0;
      const ratio = enabled > 0 ? available / enabled : 0;
      const card = make('div', 'ops-platform');
      const top = attach(make('div', 'ops-platform-top'), [
        make('span', 'ops-platform-name', row.channel),
        make('span', 'ops-platform-rate', row.concurrency_available
          ? `${row.active_requests || 0} 活跃请求 · ${available}/${enabled} 可用账号`
          : `${available}/${enabled} 可用账号 · 并发未采集`),
      ]);
      card.appendChild(top);
      meter(card, ratio, ratio >= 0.99 ? '' : ratio > 0 ? 'is-warn' : 'is-error');
      // The badges state what is holding the channel back, or that nothing is:
      // an empty badge row reads as "no data" rather than "no limits".
      const badges = [
        row.accounts_needing_login ? [`需登录 ${row.accounts_needing_login}`, 'is-error'] : null,
        row.model_cooldowns ? [`限流 ${row.model_cooldowns}`, 'is-warn'] : null,
        (!row.accounts_needing_login && !row.model_cooldowns) ? ['无限制', 'is-ok'] : null,
      ].filter(Boolean);
      card.appendChild(attach(make('div', 'ops-platform-badges'), badges.map(([label, tone]) => {
        const badge = make('span', 'logs-badge ' + tone, label);
        badge.style.marginLeft = '0';
        return badge;
      })));
      container.appendChild(card);
    });
  }

  function IsProvider(channel) {
    const name = String(channel || '').toLowerCase();
    // The infrastructure aggregates are counted but are not provider channels.
    return name !== '' && name !== 'http' && name !== 'probe';
  }

  // --- trends ----------------------------------------------------------------

  // bucketSeries turns the minute buckets into chart points. The value is taken
  // per point because every trend plots a different field of the same buckets.
  function bucketSeries(points, valueOf) {
    return points.map((point) => ({ label: fmtMinute(point.minute), value: valueOf(point) }));
  }

  function renderTrends(payload) {
    if (payload.available === false) {
      ['opsSwitchTrend', 'opsThroughput', 'opsErrorTrend'].forEach((id) => {
        const node = el(id);
        if (node) emptyNote(node, '指标未采集');
      });
      return;
    }
    const points = trendBuckets();
    // rateSeries is the per-second rate of a bucket count: the bucket still in
    // progress covers fewer seconds, so an idle minute must not read as 0/min.
    const rateSeries = (valueOf) => bucketSeries(points, (point) => valueOf(point) / point.seconds);

    const throughput = el('opsThroughput');
    if (throughput) {
      const qps = rateSeries((point) => point.requests || 0);
      const tps = rateSeries((point) => (point.input_tokens || 0) + (point.output_tokens || 0));
      const hasTokens = tps.some((point) => point.value > 0);
      lineChart(throughput, {
        points: qps, secondaryPoints: hasTokens ? tps : null, area: true, className: 'series-qps', height: 190, rightAxis: hasTokens, emptyText: '这段时间没有流量样本。' });
      setText('opsThroughputHint', hasTokens
        ? '左轴 QPS（次/秒） · 右轴 TPS（token/秒） · 当前分钟按已过时间计算'
        : '左轴 QPS（次/秒） · 窗口内请求未上报用量，TPS 暂不绘制');
    }
    const switchTrend = el('opsSwitchTrend');
    if (switchTrend) {
      // A minute with no switch has no average to plot. It is null rather than 0:
      // a zero would draw a line along the baseline and read as "measured".
      lineChart(switchTrend, {
        points: bucketSeries(points, (point) => (point.account_switch_count
          ? (point.account_switch_sum || 0) / point.account_switch_count
          : null)), className: 'series-switch', height: 150, emptyText: '这段时间没有账号切换样本。' });
    }
    const errorTrend = el('opsErrorTrend');
    if (errorTrend) {
      // Two series, not one: 最终失败 is what the caller saw, 上游尝试失败 counts
      // attempts that died before a retry rescued the request. Plotting only the
      // first hid exactly the upstream trouble an operator is looking for.
      const failures = bucketSeries(points, (point) => point.failed || 0);
      const attempts = bucketSeries(points, (point) => point.attempt_failures || 0);
      const legend = el('opsErrorTrendLegend');
      if (legend) {
        legend.replaceChildren();
        [['最终失败', 'series-error'], ['上游尝试失败', 'series-alt']].forEach(([label, className]) => {
          legend.appendChild(attach(make('span', 'ops-legend-item'), [
            make('span', 'ops-legend-swatch ' + className),
            make('span', '', label),
          ]));
        });
      }
      barChart(errorTrend, failures, {
        height: 150,
        errorBars: true,
        emptyText: '这段时间没有失败。',
        overlay: attempts,
        onSelect: (index) => {
          const point = points[index];
          if (!point) return;
          const minute = new Date(point.minute);
          drilldownToLogs({ outcome: 'failed', since: minute.toISOString(), until: new Date(minute.getTime() + 60 * 1000).toISOString() });
        },
      });
    }
  }

  function renderDistributions(payload) {
    if (payload.available === false) {
      ['opsHistogram', 'opsErrorMix'].forEach((id) => {
        const node = el(id);
        if (node) emptyNote(node, '指标未采集');
      });
      setText('opsHistogramHint', '未采集');
      setText('opsErrorMixHint', '未采集');
      return;
    }
    const totals = payload.totals || {};
    const histogram = el('opsHistogram');
    const errorMix = el('opsErrorMix');
    const tab = outcomeTab(state.outcome);
    const bins = totals[tab.histogram] || [];

    if (histogram) {
      histogram.replaceChildren();
      // An empty chart is indistinguishable from a broken one, so say which of
      // the two it is: no samples in this window, or no distribution collected.
      if (!bins.length) emptyNote(histogram, (totals[tab.duration] && totals[tab.duration].samples)
        ? '该窗口没有分布样本。'
        : '该时间窗口内暂无延迟样本（口径：' + tab.label + '）。');
      const maxCount = Math.max(1, ...bins.map((bin) => bin.count || 0));
      bins.forEach((bin) => {
        const bar = make('div', 'ops-histogram-bar');
        // The column heights are relative to the tallest bin, so a window of one
        // slow request is not flattened against a 10k-request peak.
        bar.style.height = ((bin.count || 0) / maxCount * 100).toFixed(1) + '%';
        bar.title = `${bin.label}: ${bin.count || 0}`;
        histogram.appendChild(attach(make('div', 'ops-histogram-col'), [
          make('span', 'ops-histogram-count', String(bin.count || 0)),
          bar,
          make('span', 'ops-histogram-label', bin.label),
        ]));
      });
      const cohort = totals[tab.duration] || {};
      setText('opsHistogramHint', (cohort.samples || 0) ? `${cohort.samples} 个样本 · 口径：${tab.label}` : '暂无样本');
    }

    if (errorMix) {
      // Each row is one class of the shared classifier, so these numbers add up to
      // the failure count the drill-down lists: 4xx + 5xx + 流中断 = 最终失败, and
      // 限流 is shown apart because it is a business limit, not a failure.
      const groups = [
        { label: '4xx（非限流）', value: totals.client_errors || 0, tone: 'is-warn', outcome: 'client_error' },
        { label: '5xx 上游错误', value: totals.server_errors || 0, tone: '', outcome: 'server_error' },
        { label: '流中断（已提交 2xx）', value: totals.stream_errors || 0, tone: '', outcome: 'stream_error' },
        // A 401/403 from the provider is the channel's own credential failing, and it
        // is a failure. Our gate's 401/403 is the row below, and the two are never
        // added together: they are opposite statements about the channel.
        { label: '上游认证失败 401/403（账号被上游拒绝）', value: totals.upstream_auth || 0, tone: '', outcome: 'upstream_auth' },
        { label: '429 / 529 限流', value: totals.rate_limited || 0, tone: 'is-info', outcome: 'rate_limited' },
        { label: '402 额度用尽', value: totals.quota_exhausted || 0, tone: 'is-info', outcome: 'quota_exhausted' },
        { label: '网关拒绝 401/403（我方拒绝，非上游故障）', value: totals.rejected || 0, tone: 'is-info', outcome: 'rejected' },
        { label: '上游尝试失败（含已重试成功）', value: totals.attempt_failures || 0, tone: 'is-warn', outcome: 'failed' },
      ];
      const maxValue = Math.max(1, ...groups.map((group) => group.value));
      // 未采集 when the window predates the classifier: a 0 next to seven other 0s
      // reads as "no such failures", which is a different and misleading fact.
      const collected = !(totals.requests > 0 && !totals.detailed_requests);
      const rows = groups.map((group) => {
        const row = make('button', 'ops-error-row is-clickable');
        row.type = 'button';
        // Clicking a class opens the log centre filtered to exactly that class.
        row.addEventListener('click', () => drilldownToLogs({ outcome: group.outcome }));
        const fill = make('span', group.tone || '');
        fill.style.width = (group.value / maxValue * 100).toFixed(1) + '%';
        return attach(row, [
          make('span', '', group.label),
          attach(make('div', 'ops-error-meter'), [fill]),
          make('span', '', collected ? fmtInt(group.value) : '未采集'),
        ]);
      });
      errorMix.replaceChildren(...rows);
      const total = totals.failed || 0;
      setText('opsErrorMixHint', Number(totals.detailed_requests || 0) < Math.max(0, totals.requests || 0) ? '窗口含旧数据，错误分类未完整采集' : total === 0 ? '该时间窗口内暂无最终失败。' : `最终失败 ${total} 次 · 点击分类可下钻`);
    }
  }

  // --- alerts ----------------------------------------------------------------

  async function loadAlertEvents() {
    const table = el('opsAlertTable');
    if (!table) return;
    const body = table.querySelector('tbody');
    const severity = (el('opsAlertSeverity') || {}).value || '';
    const localChannel = (el('opsAlertChannel') || {}).value || '';
    const channel = state.channel || localChannel;
    const scope = { window: state.window, channel: state.channel };
    const ticket = alertLoads.begin();
    const current = () => ticket.isCurrent() && scope.window === state.window && scope.channel === state.channel;
    const range = windowRange();
    body.replaceChildren();
    try {
      const params = assignParams(new URLSearchParams({ kind: 'system', action: 'alert_', limit: '50' }), {
        channel, since: range.since.toISOString(), until: range.until.toISOString(),
      });
      const response = await ConsoleAPI.request('/api/journal/records?' + params.toString(), { signal: ticket.signal });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      if (!current()) return;
      const rows = (payload.data || []).filter((record) => {
        const event = record.event || {};
        if (event.action !== 'alert_fired' && event.action !== 'alert_recovered') return false;
        const level = (event.metadata && event.metadata.severity) || '';
        return (!severity || level === severity) && (!localChannel || event.channel === localChannel);
      });
      if (!rows.length) {
        emptyRow(body, 6, '所选时间范围及渠道内暂无告警事件。');
        return;
      }
      rows.forEach((record) => {
        const event = record.event || {};
        const tr = make('tr');
        const level = String((event.metadata && event.metadata.severity) || '').toLowerCase();
        // The level is a word an operator reads, not the key the engine stores: the
        // table printed "warning" next to fully Chinese text.
        const cells = [
          fmtClock(new Date(event.timestamp)),
          event.action === 'alert_fired' ? '触发' : '恢复',
          ALERT_SEVERITY_LABELS[level] || (level ? level : '—'),
          event.channel || '—',
          event.model || '—',
          event.error || event.details || '—',
        ];
        cells.forEach((value, index) => {
          const cell = ALERT_CELLS[index];
          // Named so the phone layout can place each cell: six columns cannot fit a
          // 360px screen, and the card view in ops.css positions these by class.
          // The severity column carries its tone in the same class list.
          const className = index === 2
            ? cell.className + ' ops-alert-severity ' + (level === 'critical' ? 'is-critical' : level === 'warning' ? 'is-warning' : '')
            : cell.className;
          const td = make('td', className, value);
          td.dataset.label = cell.label;
          tr.appendChild(td);
        });
        body.appendChild(tr);
      });
    } catch (error) {
      if (!current()) return;
      emptyRow(body, 6, '读取告警事件失败：' + (error.message || error));
    }
  }

  // --- matrix ----------------------------------------------------------------

  // historyCells renders per-minute blocks. A model row uses its own history, so
  // "when did this model start failing" is answerable per model instead of only
  // per channel; a channel row uses the channel's series.
  function historyCells(series) {
    const cell = make('td', 'ops-history-cells');
    const buckets = (series || []).slice(-40);
    if (!buckets.length) {
      cell.textContent = '暂无样本';
      cell.classList.add('ops-empty');
      return cell;
    }
    buckets.forEach((point) => {
      const block = make('span', 'ops-block');
      const requests = point.requests || 0;
      const failed = point.failed || 0;
      // Idle, wholly failed, partly failed, clean: four states, because a minute
      // with no traffic and a minute with no failures must not share a colour.
      block.classList.add(!requests ? 'is-idle' : failed >= requests ? 'is-bad' : failed > 0 ? 'is-warn' : 'is-ok');
      block.title = `${fmtMinute(point.minute)}：${requests} 请求 / ${failed} 失败`;
      cell.appendChild(block);
    });
    return cell;
  }

  // MATRIX_CELLS names the matrix columns after the row label, in build order. The
  // channel/model card on a phone prints these as the label of each value: the header
  // row is hidden there, and a column of bare numbers has no owner.
  const MATRIX_CELLS = [
    { className: 'ops-mx-accounts', label: '可用账号' },
    { className: 'ops-mx-requests', label: '请求' },
    { className: 'ops-mx-rate', label: '成功率' },
    { className: 'ops-mx-ttft', label: '首 Token P95' },
    { className: 'ops-mx-duration', label: '总耗时 P95' },
    { className: 'ops-mx-throttled', label: '限流' },
  ];

  function formatRate(rate, samples) {
    if (!samples) return '暂无样本';
    return (rate * 100).toFixed(1) + '%';
  }

  function matrixRow(label, row, options) {
    const tr = make('tr', options.isModel ? 'is-model' : 'is-channel');

    const name = make('td', 'ops-matrix-name', label);
    // No data-label here on purpose: the card prints the cell's label above its value,
    // and this cell's value is the channel or model name — "渠道 / 模型 / 名称"
    // labelled the heading twice. The other seven cells carry theirs.
    // The row name is the drill-down: a channel opens its own traffic, a model its
    // channel-and-model traffic, both over the window the page is showing.
    name.classList.add('is-clickable');
    name.title = '点击查看该' + (options.isModel ? '模型的请求日志' : '渠道的请求日志');
    makeActivatable(name, () => drilldownToLogs(options.isModel
      ? { channel: options.channel || state.channel, model: label }
      : { channel: label }));
    tr.appendChild(name);

    // A model row inherits its channel's account pool and cooldowns, so those two
    // columns are the channel's to fill: dash, not zero.
    const accounts = make('td', '', options.isModel
      ? '—'
      : row.accounts_enabled === 0
        ? '未配置'
        : `${row.accounts_available} / ${row.accounts_enabled}` + (row.accounts_needing_login ? `（需登录 ${row.accounts_needing_login}）` : ''));
    const samples = options.isModel ? row.samples || 0 : (row.summary && row.summary.samples) || 0;
    const r = options.isModel ? row.success_rate || 0 : (row.summary && row.summary.success_rate) || 0;
    const rate = make('td', samples ? '' : 'ops-empty', formatRate(r, samples));
    // A model's first-token figure needs its own sample count: "no sample" and
    // "0 ms" must not look the same, and a rate limit produces no token at all.
    const ttftSamples = row.first_token_samples || 0;
    const ttft = options.isModel
      ? make('td', ttftSamples ? '' : 'ops-empty', fmtMs(row.first_token_p95_ms, ttftSamples))
      : make('td', '', fmtMs(row.summary && row.summary.first_token_p95_ms, samples));
    if (options.isModel && ttftSamples) ttft.title = `P95 · ${ttftSamples} 个样本`;
    const cells = [
      accounts,
      make('td', '', options.isModel ? String(row.requests || 0) : String(Math.max((row.summary && row.summary.requests) || 0, 0))),
      rate,
      ttft,
      make('td', '', fmtMs(options.isModel ? row.duration_p95_ms : row.summary && row.summary.duration_p95_ms, samples)),
      make('td', '', options.isModel ? '—' : String(row.model_cooldowns || 0)),
      historyCells(options.isModel ? row.history : row.series),
    ];
    cells.forEach((cell) => tr.appendChild(cell));
    // The seven value cells were named as they were built; stamp the header label on
    // each one now. Both card layouts (in ops.css) read it, and the desktop table
    // ignores it because its own <thead> is visible.
    MATRIX_CELLS.forEach((cell, index) => {
      const td = tr.children[index + 1];
      td.classList.add(cell.className);
      td.dataset.label = cell.label;
    });
    return tr;
  }

  function renderMatrix(rows) {
    const table = el('opsMatrix');
    if (!table) return;
    const body = table.querySelector('tbody');
    body.replaceChildren();
    const visible = (rows || []).filter((row) => !state.model || (row.models || []).some((model) => model.model === state.model));
    if (!visible.length) {
      emptyRow(body, 8, '还没有渠道数据。添加账号或等待流量后这里会出现状态矩阵。');
      return;
    }
    visible.forEach((row) => {
      body.appendChild(matrixRow(row.channel, row, { isModel: false }));
      (row.models || [])
        .filter((model) => !state.model || model.model === state.model)
        .forEach((model) => {
          body.appendChild(matrixRow(model.model, model, { isModel: true, channel: row.channel }));
        });
    });
  }

  // --- coverage --------------------------------------------------------------

  // excludedLabel explains an aggregate the matrix leaves out. These are counted
  // but they are not channels, so a matrix that omits them has to say why.
  function excludedLabel(name) {
    if (name === 'http') return 'http（非推理路径：管理页、健康检查、公网扫描）';
    if (name === 'probe') return 'probe（旧版本探测流量的历史聚合，已不再产生）';
    return name;
  }

  function renderCoverage(payload) {
    const node = el('opsCoverage');
    if (!node) return;
    const health = payload.ingestion;
    const aggregationHealth = payload.aggregation_health;
    setText("opsIngestion", health
      ? `本实例日志采集：队列 ${health.queue} 条 / ${health.queued_bytes} 字节；已写入 ${health.written}；丢弃 ${health.dropped}；写入失败 ${health.write_failed}` + (health.last_success ? `；最近成功 ${health.last_success}` : "；尚无成功写入") + ((health.dropped || health.write_failed) ? "；日志可能不完整，请检查 Redis 和流量压力。" : "")
      : "本实例日志采集健康状态不可用");
    if (aggregationHealth && aggregationHealth.write_failed) {
 const node = el("opsIngestion");
 if (node) node.textContent += `；本实例指标聚合写入失败 ${aggregationHealth.write_failed} 次，统计可能不完整。`;
 }
    const coverage = payload.coverage || {};
    const parts = coverage.available !== false && typeof coverage.entries === 'number'
      ? [`审计日志保留 ${coverage.entries} 条`]
        .concat(coverage.oldest ? [`最早 ${coverage.oldest}`] : [])
        .concat(coverage.newest ? [`最新 ${coverage.newest}`] : [])
      : ['审计日志覆盖范围未能读取（接口未返回 coverage）'];
    if (coverage.counts) {
      const counts = Object.keys(coverage.counts).map((key) => `${key}=${coverage.counts[key]}`).join('、');
      if (counts) parts.push(`采样计数：${counts}`);
    }
    parts.push('因此页面只承诺“保留窗口内”的结论，不承诺固定天数。');
    const excluded = (payload.excluded_aggregates || []).map(excludedLabel);
    if (excluded.length) parts.push('已计数但不在渠道矩阵中显示：' + excluded.join('；'));
    if (payload.latency_precision) parts.push(payload.latency_precision);
    parts.push('Token / TPS 只统计上报了用量的请求；未上报用量的渠道其 TPS 会偏低。');
    // When the aggregation itself is disabled the reason matters more than the
    // coverage caveat: the whole page is showing nothing because of it.
    node.textContent = (payload.available === false && payload.note ? payload.note + '；' : '') + parts.join('；');
  }

  // fillOptions rebuilds a <select> of one "all" entry plus the given values. The
  // options come from the payload, so they are rewritten on every refresh; the
  // selection is restored afterwards because rewriting clears it.
  function fillOptions(select, placeholder, values) {
    select.replaceChildren();
    const option = (value, label) => {
      const node = document.createElement('option');
      node.value = value;
      node.textContent = label;
      return node;
    };
    select.appendChild(option('', placeholder));
    values.forEach((value) => select.appendChild(option(value, value)));
    return select;
  }

  function updateChannelOptions(channels, current) {
    const select = el('opsChannel');
    if (!select) return;
    fillOptions(select, '全部渠道', channels || []).value = current || '';
    // The alert table's channel filter is filled once: it is a filter, not a mirror
    // of the page scope, so a refresh must not keep appending to it.
    const alertChannel = el('opsAlertChannel');
    if (alertChannel && alertChannel.childElementCount <= 1) {
      (channels || []).forEach((channel) => {
        const option = document.createElement('option');
        option.value = channel;
        option.textContent = channel;
        alertChannel.appendChild(option);
      });
    }
  }

  function updateModelOptions(rows, current) {
    const select = el('opsModel');
    if (!select) return;
    const names = new Set();
    (rows || []).forEach((row) => (row.models || []).forEach((model) => {
      if (model.model) names.add(model.model);
    }));
    fillOptions(select, '全部模型', Array.from(names).sort()).value = current || '';
  }

  // --- load ------------------------------------------------------------------

  function setStatus(text, tone) {
    setText('opsStatusText', text);
    const dot = el('opsStatusDot');
    if (dot) dot.className = 'ops-dot' + (tone ? ' ' + tone : '');
  }

  const overviewLoads = ConsoleUI.requestGate();
  const alertLoads = ConsoleUI.requestGate();
  async function load() {
    if (state.loading) {
      state.refreshPending = true;
      overviewLoads.cancel();
      alertLoads.cancel();
      return;
    }
    state.loading = true;
    const ticket = overviewLoads.begin();
    const scope = { window: state.window, channel: state.channel };
    if (!state.overview) renderSkeletons();
    setStatus('读取中…', 'is-warn');
    syncUrl();
    try {
      const params = assignParams(new URLSearchParams({ window: String(state.window) }), { channel: state.channel });
      const [overviewResponse, runtimeResponse] = await Promise.all([
        ConsoleAPI.request('/api/ops/overview?' + params.toString(), { signal: ticket.signal }),
        ConsoleAPI.request('/api/ops/runtime', { signal: ticket.signal }).catch(() => null),
      ]);
      if (!overviewResponse.ok) throw new Error('HTTP ' + overviewResponse.status);
      const payload = await overviewResponse.json();
      const runtimePayload = runtimeResponse && runtimeResponse.ok
        ? await runtimeResponse.json().catch(() => null) : null;
      // A queued filter change owns the screen; never paint the previous scope.
      if (!ticket.isCurrent() || scope.window !== state.window || scope.channel !== state.channel) return;
      state.overview = payload;
      renderKpis(payload);
      renderHero(payload);
      renderTrends(payload);
      renderDistributions(payload);
      renderConcurrency(payload);
      renderMatrix(payload.matrix);
      renderCoverage(payload);
      updateChannelOptions(payload.channels, state.channel);
      updateModelOptions(payload.matrix, state.model);
      if (runtimePayload) renderResources(runtimePayload);
      else renderResources({ available: false, note: '运行时指标读取失败。' });
      setText('opsRefreshedAt', fmtClock(new Date()));
      const collectionLoss = payload.ingestion && (payload.ingestion.dropped || payload.ingestion.write_failed);
      const aggregationLoss = payload.aggregation_health && payload.aggregation_health.write_failed;
      setStatus(collectionLoss ? '日志采集存在丢失' : aggregationLoss ? '指标采集存在丢失' : '就绪', collectionLoss || aggregationLoss ? 'is-warn' : '');
      state.countdown = state.refreshSeconds;
      await loadAlertEvents();
    } catch (error) {
      if (!ticket.accepts(error) || scope.window !== state.window || scope.channel !== state.channel) return;
      // A failed filter change has no valid historical scope to display.
      state.overview = null;
      if (!state.overview) {
        renderHero({ available: false });
        renderResources({ available: false, note: '运行时指标尚未读取成功。' });
        ['opsConcurrency', 'opsErrorMix', 'opsHistogram', 'opsSwitchTrend', 'opsThroughput', 'opsErrorTrend'].forEach((id) => {
          const node = el(id);
          if (node) emptyNote(node, '指标读取失败，请刷新重试。');
        });
      }
      const container = el('opsKpis');
      if (container) {
        container.replaceChildren();
        const card = kpiCard('读取失败');
        bigValue(card, '—', '', 'is-muted');
        rowList(card, [{ label: '原因', value: String(error.message || error) }]);
        container.appendChild(card);
      }
      // A failed read has no window to describe, so the coverage line states the
      // failure instead of the retention it could not read.
      setText('opsIngestion', '采集健康状态未能更新，请重试。');
      setText('opsCoverage', `指标读取失败：${String(error.message || error)}。会话可能已过期，请重新登录后刷新。`);
      setStatus('读取失败', 'is-error');
      renderMatrix([]);
      await loadAlertEvents();
    } finally {
      state.loading = false;
      // Coalesce every refresh requested while this one was in flight into one
      // follow-up pass, rather than losing a filter change or starting N passes.
      if (state.refreshPending && (typeof document === 'undefined' || !document.hidden)) {
        state.refreshPending = false;
        window.setTimeout(load, 0);
      }
    }
  }

  function bind() {
    // The charts are drawn at their measured pixel size, so a window resize (or a
    // rotate, or the sidebar collapsing) invalidates every one of them: the SVG
    // keeps its old viewBox and stretches. Debounced, because a drag-resize fires
    // continuously and each redraw rebuilds three SVGs and an histogram.
    if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
      let resizeTimer = 0;
      window.addEventListener('resize', () => {
        if (resizeTimer && typeof clearTimeout === 'function') clearTimeout(resizeTimer);
        resizeTimer = setTimeout(() => {
          resizeTimer = 0;
          if (state.overview) {
            renderTrends(state.overview);
            renderDistributions(state.overview);
          }
        }, 160);
      });
    }

    // A filter change reloads; a model change only re-filters the matrix, because
    // the payload already carries every channel's models.
    attachHandler('opsWindow', 'change', () => {
      state.window = Number(el('opsWindow').value) || 180;
      load();
    });
    attachHandler('opsChannel', 'change', () => {
      state.channel = el('opsChannel').value;
      load();
    });
    attachHandler('opsModel', 'change', () => {
      state.model = el('opsModel').value;
      syncUrl();
      if (state.overview) renderMatrix(state.overview.matrix);
    });
    attachHandler('opsRefresh', 'click', load);
    // The hero's failure link is created with the gauge sub-line (it only exists
    // when there is a failure to look at), so its listener is attached there.
    attachHandler('opsAlertsLink', 'click', () => {
      if (window.location) window.location.href = pagePath() + '?tab=alerts';
    });

    // One place marks the selected window, so the chips and the state cannot drift:
    // 重置 used to set the state to 1 minute and leave "1h" lit as the selection.
    function selectLiveWindow(minutes) {
      document.querySelectorAll('#opsLiveTabs .ops-chip').forEach((chip) => {
        chip.classList.toggle('is-active', (Number(chip.getAttribute('data-live')) || 1) === minutes);
      });
      state.liveWindow = minutes;
      if (state.overview) renderHero(state.overview);
    }

    document.querySelectorAll('#opsLiveTabs .ops-chip').forEach((button) => {
      button.addEventListener('click', () => {
        selectLiveWindow(Number(button.getAttribute('data-live')) || 1);
      });
    });

    attachHandler('opsTrendReset', 'click', () => {
      selectLiveWindow(1);
      if (state.overview) renderTrends(state.overview);
      showToast('已重置实时与趋势视图');
    });
    attachHandler('opsTrendDownload', 'click', () => {
      const header = 'minute,requests,success,failed,input_tokens,output_tokens\n';
      const rows = trendBuckets().map((point) => [
        point.minute, point.requests || 0, point.success || 0, point.failed || 0,
        point.input_tokens || 0, point.output_tokens || 0,
      ].join(',')).join('\n');
      const blob = new Blob([header + rows], { type: 'text/csv;charset=utf-8' });
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = `orchids-ops-${state.window}min.csv`;
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      URL.revokeObjectURL(url);
    });

    attachHandler('opsAlertReload', 'click', loadAlertEvents);
    attachHandler('opsAlertSeverity', 'change', loadAlertEvents);
    attachHandler('opsAlertChannel', 'change', loadAlertEvents);

    document.querySelectorAll('[data-scroll-to]').forEach((button) => {
      button.addEventListener('click', () => {
        const target = el(button.getAttribute('data-scroll-to'));
        if (!target) return;
        if (target.tagName === 'DETAILS') target.open = true;
        target.scrollIntoView({ behavior: 'smooth', block: 'start' });
      });
    });
  }

  function tick() {
    state.countdown -= 1;
    if (state.countdown > 0) {
      setText('opsCountdown', String(state.countdown));
      return;
    }
    state.countdown = state.refreshSeconds;
    // A background tab keeps no one informed: the dashboard it refreshes is not
    // on screen, so the poll only burns the operator's quota and the server's
    // aggregation budget. The backlog is fetched as soon as the tab is shown.
    if (typeof document !== 'undefined' && document.hidden) {
      state.refreshPending = true;
      return;
    }
    flushPendingRefresh();
  }

  // Returning to the tab has to catch up immediately: without this the operator
  // can sit down to a dashboard that is up to a full refresh interval stale.
  function flushPendingRefresh() {
    if (typeof document !== 'undefined' && document.hidden) return;
    state.refreshPending = false;
    state.countdown = state.refreshSeconds;
    load();
  }

  // applyUrlState puts the page back where the URL says it should be: the filters
  // a drill-down returned to, or a bookmarked scope.
  function applyUrlState() {
    readUrlState();
    // The two selects are only corrected when the scope has a value: an absent
    // filter must leave "全部渠道" selected rather than forcing a choice.
    if (state.window) {
      const windowSelect = el('opsWindow');
      if (windowSelect) windowSelect.value = String(state.window);
    }
    if (state.channel) {
      const channelSelect = el('opsChannel');
      if (channelSelect) channelSelect.value = state.channel;
    }
    if (state.model) {
      const modelSelect = el('opsModel');
      if (modelSelect) modelSelect.value = state.model;
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { applyUrlState(); bind(); load(); });
  } else {
    applyUrlState();
    bind();
    load();
  }
  if (typeof document.addEventListener === 'function') { document.addEventListener('visibilitychange', flushPendingRefresh); }
  state.timer = setInterval(tick, 1000);
})();
