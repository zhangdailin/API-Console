// Operations monitoring dashboard: header status, health gauge, six KPI cards,
// host/runtime resources, per-platform concurrency, throughput and switch trends,
// latency distribution, error mix/trend, alert events and the channel × model
// matrix. Every figure that has no sample says so instead of rendering a healthy
// zero.
(function () {
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
  const ALERT_SEVERITY_LABELS = {
    critical: '严重',
    warning: '警告',
    info: '提示',
  };

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

  function outcomeTab(key) {
    return OUTCOME_TABS.filter((tab) => tab.key === key)[0] || OUTCOME_TABS[0];
  }

  // The page state lives in the URL, so these two accessors are the only places
  // that touch window.location. Keeping them here means the render tests (which
  // stub a DOM without a location) exercise the same code as a browser.
  function pagePath() {
    return (window.location && window.location.pathname) || '';
  }

  function pageSearch() {
    return (window.location && window.location.search) || '';
  }

  // windowStart/untilISO turn the selected window into the absolute range a
  // drill-down hands to the log centre. The chart and the list must cover the same
  // minutes, so the range is derived once here instead of in each click handler.
  function windowRange() {
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
    const params = new URLSearchParams({ tab: 'logs', kind: 'request' });
    params.set('since', range.since.toISOString());
    params.set('until', range.until.toISOString());
    if (state.channel) params.set('channel', state.channel);
    if (state.model) params.set('model', state.model);
    Object.keys(extra || {}).forEach((key) => {
      const value = extra[key];
      if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
    });
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
    const params = new URLSearchParams({ tab: 'ops', window: String(state.window) });
    if (state.channel) params.set('channel', state.channel);
    if (state.model) params.set('model', state.model);
    if (state.outcome !== 'all') params.set('outcome', state.outcome);
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

  function el(id) {
    return document.getElementById(id);
  }

  function setText(id, value) {
    const node = el(id);
    if (node) node.textContent = value;
  }

  function fmtInt(value) {
    return Number(value || 0).toLocaleString('zh-CN');
  }

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

  function fmtClock(date) {
    return date.toLocaleTimeString('zh-CN', { hour12: false });
  }

  function fmtMinute(iso) {
    if (!iso) return '';
    const date = new Date(iso);
    if (Number.isNaN(date.getTime())) return String(iso).slice(11, 16);
    return String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
  }

  function emptyChart(container, text) {
    container.replaceChildren();
    const note = document.createElement('p');
    note.className = 'ops-chart-empty';
    note.textContent = text;
    container.appendChild(note);
  }

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

  function svgEl(name, attrs) {
    const node = document.createElementNS(SVG_NS, name);
    Object.keys(attrs || {}).forEach((key) => node.setAttribute(key, String(attrs[key])));
    return node;
  }

  // lineChart draws one or two series with a left axis, a grid and time labels.
  function lineChart(container, options) {
    container.replaceChildren();
    const points = options.points || [];
    if (!points.length || !points.some(p => p.value != null)) {
      emptyChart(container, options.emptyText || '这段时间没有样本。');
      return;
    }
    const box = chartBox(container, options.height || 150);
    const width = box.width;
    const height = box.height;
    const padLeft = 34;
    const padRight = options.rightAxis ? 34 : 10;
    const padTop = 8;
    const padBottom = 20;
    const plotW = width - padLeft - padRight;
    const plotH = height - padTop - padBottom;
    const maxValue = Math.max(options.maxValue || 0, ...points.map((p) => p.value), 0.0001);

    const svg = svgEl('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none' });

    for (let i = 0; i <= 3; i += 1) {
      const y = padTop + (plotH / 3) * i;
      svg.appendChild(svgEl('line', { x1: padLeft, y1: y, x2: width - padRight, y2: y, class: 'grid-line' }));
      const label = svgEl('text', { x: 4, y: y + 3, class: 'axis-text' });
      label.textContent = (maxValue * (1 - i / 3)).toFixed(maxValue < 10 ? 2 : 0);
      svg.appendChild(label);
    }

    const step = plotW / Math.max(points.length - 1, 1);
    const coords = points.map((point, index) => {
      const x = padLeft + step * index;
      const y = padTop + plotH - (point.value / maxValue) * plotH;
      return { x, y };
    });

    if (options.area) {
      const area = coords.map((c, i) => `${i === 0 ? 'M' : 'L'}${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' ');
      svg.appendChild(svgEl('path', {
        d: `${area} L${coords[coords.length - 1].x.toFixed(1)},${(padTop + plotH).toFixed(1)} L${coords[0].x.toFixed(1)},${(padTop + plotH).toFixed(1)} Z`,
        class: 'series-area',
      }));
    }

    const line = coords.map((c, i) => points[i].value == null ? '' : `${i === 0 || points[i - 1].value == null ? 'M' : 'L'}${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' ');
    svg.appendChild(svgEl('path', { d: line, class: options.className || 'series-qps' }));
    coords.forEach((c, i) => {
      if (points[i].value != null && (i === 0 || points[i - 1].value == null) &&
          (i === points.length - 1 || points[i + 1].value == null)) {
        svg.appendChild(svgEl('circle', { cx: c.x, cy: c.y, r: 3, fill: 'var(--accent)' }));
      }
    });

    // Second series on its own scale: QPS and TPS differ by orders of magnitude,
    // so sharing one axis would flatten one of them into the baseline.
    if (options.secondaryPoints && options.secondaryPoints.length === points.length) {
      const secondaryMax = Math.max(options.secondaryMax || 0, ...options.secondaryPoints.map((p) => p.value), 0.0001);
      const secondaryCoords = options.secondaryPoints.map((point, index) => ({
        x: padLeft + step * index,
        y: padTop + plotH - (point.value / secondaryMax) * plotH,
      }));
      svg.appendChild(svgEl('path', {
        d: secondaryCoords.map((c, i) => `${i === 0 ? 'M' : 'L'}${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' '),
        class: 'series-tps',
      }));
      for (let i = 0; i <= 3; i += 1) {
        const y = padTop + (plotH / 3) * i;
        const label = svgEl('text', { x: width - padRight + 4, y: y + 3, class: 'axis-text' });
        label.textContent = (secondaryMax * (1 - i / 3)).toFixed(secondaryMax < 10 ? 2 : 0);
        svg.appendChild(label);
      }
    }

    const labelEvery = Math.max(1, Math.ceil(points.length / 6));
    points.forEach((point, index) => {
      if (index % labelEvery !== 0 && index !== points.length - 1) return;
      const text = svgEl('text', { x: coords[index].x, y: height - 5, class: 'axis-text', 'text-anchor': 'middle' });
      text.textContent = point.label;
      svg.appendChild(text);
    });

    container.appendChild(svg);
  }

  // bars renders a per-minute bar series (errors, sparkline ticks). An optional
  // overlay draws a second series on the same scale, and onSelect makes each bar
  // open the log centre scoped to that minute.
  function barChart(container, points, options) {
    container.replaceChildren();
    if (!points.length) {
      emptyChart(container, (options && options.emptyText) || '这段时间没有样本。');
      return;
    }
    const box = chartBox(container, (options && options.height) || 150);
    const width = box.width;
    const height = box.height;
    const padLeft = 30;
    const padBottom = 20;
    const plotW = width - padLeft - 8;
    const plotH = height - padBottom - 8;
    const overlay = (options && options.overlay) || [];
    const maxValue = Math.max(1, ...points.map((p) => p.value), ...overlay.map((p) => p.value));
    const svg = svgEl('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none' });
    svg.appendChild(svgEl('line', { x1: padLeft, y1: 8, x2: padLeft, y2: 8 + plotH, class: 'grid-line' }));
    const barStep = plotW / points.length;
    const barW = Math.max(0.2, barStep - Math.min(2, barStep * 0.2));
    points.forEach((point, index) => {
      const barH = Math.max(point.value > 0 ? 3 : 0.6, (point.value / maxValue) * plotH);
      const x = padLeft + (plotW / points.length) * index;
      const bar = svgEl('rect', {
        x, y: 8 + plotH - barH, width: barW, height: barH,
        class: (point.value > 0 || !(options && options.markZero)) && options && options.errorBars ? 'bar is-error' : 'bar',
      });
      bar.appendChild(svgEl('title', {})).textContent = `${point.label}：${point.value}`;
      if (options && options.onSelect) {
        // A chart that cannot be interrogated is a poster: the bar carries the
        // minute it belongs to into the log centre.
        bar.setAttribute('tabindex', '0');
        bar.setAttribute('role', 'button');
        bar.classList.add('is-clickable');
        const activate = () => options.onSelect(index);
        bar.addEventListener('click', activate);
        bar.addEventListener('keydown', (event) => {
          if (event.key === 'Enter' || event.key === ' ') {
            event.preventDefault();
            activate();
          }
        });
      }
      svg.appendChild(bar);
    });
    if (overlay.length === points.length) {
      // The attempt-failure series shares the axis (both are request counts), so it
      // is drawn as a line over the bars rather than as a second scale.
      const step = plotW / points.length;
      const coords = overlay.map((point, index) => ({
        x: padLeft + step * index + barW / 2,
        y: 8 + plotH - (point.value / maxValue) * plotH,
      }));
      svg.appendChild(svgEl('polyline', {
        points: coords.map((c) => `${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' '),
        class: 'series-alt',
      }));
    }
    const labelEvery = Math.max(1, Math.ceil(points.length / 6));
    points.forEach((point, index) => {
      if (index % labelEvery !== 0 && index !== points.length - 1) return;
      const text = svgEl('text', {
        x: padLeft + (plotW / points.length) * index + barW / 2,
        y: height - 5, class: 'axis-text', 'text-anchor': 'middle',
      });
      text.textContent = point.label;
      svg.appendChild(text);
    });
    container.appendChild(svg);
  }

  function sparkline(points) {
    const container = el('opsSpark');
    if (!container) return;
    const width = 320;
    const height = 72;
    container.replaceChildren();
    if (!points.length) {
      const note = document.createElement('p');
      note.className = 'ops-chart-empty';
      note.textContent = '指标未采集';
      container.appendChild(note);
      return;
    }
    if (points.length === 1) {
      const svg = svgEl('svg', { viewBox: `0 0 ${width} ${height}` });
      svg.appendChild(svgEl('circle', { cx: width / 2, cy: height / 2, r: 4, fill: 'var(--accent)' }));
      const label = svgEl('text', { x: width / 2, y: height - 8, 'text-anchor': 'middle', class: 'axis-text' });
      label.textContent = points[0].value > 0 ? '当前分钟已有请求' : '当前分钟无请求';
      svg.appendChild(label);
      container.appendChild(svg);
      return;
    }
    const max = Math.max(1, ...points.map((p) => p.value));
    const svg = svgEl('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none' });
    const coords = points.map((point, index) => ({
      x: (width / Math.max(points.length - 1, 1)) * index,
      y: height - 6 - (point.value / max) * (height - 16),
    }));
    svg.appendChild(svgEl('path', {
      d: coords.map((c, i) => `${i === 0 ? 'M' : 'L'}${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' '),
      class: 'series-tps',
    }));
    container.appendChild(svg);
  }

  // --- KPI cards -------------------------------------------------------------

  function kpiCard(title, tools) {
    const card = document.createElement('article');
    card.className = 'ops-kpi-card';
    const head = document.createElement('div');
    head.className = 'ops-kpi-head';
    const label = document.createElement('span');
    label.textContent = title;
    head.appendChild(label);
    if (tools) {
      const tool = document.createElement('span');
      tool.className = 'ops-hint';
      tool.textContent = tools;
      head.appendChild(tool);
    }
    card.appendChild(head);
    return card;
  }

  function bigValue(card, value, unit, tone) {
    const node = document.createElement('div');
    node.className = 'ops-kpi-big' + (tone ? ' ' + tone : '');
    node.textContent = value;
    if (unit) {
      const unitNode = document.createElement('span');
      unitNode.className = 'ops-kpi-unit';
      unitNode.textContent = unit;
      node.appendChild(unitNode);
    }
    card.appendChild(node);
    return node;
  }

  function rowList(card, rows) {
    const list = document.createElement('div');
    list.className = 'ops-kpi-rows';
    rows.forEach((entry) => {
      const row = document.createElement('div');
      row.className = 'ops-kpi-row' + (entry.strong ? ' is-strong' : '');
      const label = document.createElement('span');
      label.textContent = entry.label;
      const value = document.createElement('span');
      value.textContent = entry.value;
      row.appendChild(label);
      row.appendChild(value);
      list.appendChild(row);
    });
    card.appendChild(list);
    return list;
  }

  function meter(card, ratio, tone) {
    const wrap = document.createElement('div');
    wrap.className = 'ops-kpi-meter';
    const fill = document.createElement('span');
    if (tone) fill.className = tone;
    fill.style.width = Math.max(0, Math.min(100, ratio * 100)).toFixed(1) + '%';
    wrap.appendChild(fill);
    card.appendChild(wrap);
  }

  function percentileRows(set) {
    const p = set || {};
    return [
      { label: 'P95', value: fmtMs(p.p95_ms, p.samples) },
      { label: 'P90', value: fmtMs(p.p90_ms, p.samples) },
      { label: 'P50', value: fmtMs(p.p50_ms, p.samples) },
      { label: 'Avg', value: fmtMs(p.avg_ms, p.samples) },
      { label: 'Max', value: fmtMs(p.max_ms, p.samples) },
    ];
  }

  function renderSkeletons() {
    const container = el('opsKpis');
    if (!container || container.childElementCount) return;
    for (let i = 0; i < 6; i += 1) {
      const card = document.createElement('article');
      card.className = 'ops-kpi-card is-loading';
      for (const width of ['w-40', 'w-70', 'w-40']) {
        const line = document.createElement('div');
        line.className = 'skeleton-line ' + width;
        card.appendChild(line);
      }
      container.appendChild(card);
    }
  }

  // outcomeTabs builds the 口径 selector. It lives in the latency card and drives
  // that card, the TTFT card and the distribution together, which is why it is
  // labelled with what it affects instead of repeating itself in four cards.
  function outcomeTabs(onChange) {
    const wrap = document.createElement('span');
    wrap.className = 'ops-tabs';
    wrap.id = 'opsOutcomeTabs';
    OUTCOME_TABS.forEach((tab) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'ops-chip' + (state.outcome === tab.key ? ' is-active' : '');
      button.setAttribute('data-outcome', tab.key);
      button.setAttribute('aria-pressed', state.outcome === tab.key ? 'true' : 'false');
      button.textContent = tab.label;
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
    const duration = totals[tab.duration] || (state.outcome === 'all' && totals.duration_p95_ms ? {
      p95_ms: totals.duration_p95_ms,
      samples: totals.samples || 0,
    } : {});
    const durationCard = kpiCard('请求时长', (duration.samples || 0) + ' 个样本');
    durationCard.querySelector('.ops-kpi-head').appendChild(outcomeTabs(() => renderKpis(payload)));
    bigValue(durationCard, fmtMs(duration.p99_ms ?? duration.p95_ms, duration.samples), duration.p99_ms == null ? 'P95' : 'P99', duration.samples ? '' : 'is-muted');
    rowList(durationCard, percentileRows(duration).filter((row) => row.label !== 'P99'));
    container.appendChild(durationCard);

    // Time to first token, same cohort
    const firstToken = totals[tab.firstToken] || (state.outcome === 'all' && totals.first_token_p95_ms ? {
      p95_ms: totals.first_token_p95_ms,
      samples: totals.samples || 0,
    } : {});
    const ttftCard = kpiCard('TTFT', '口径：' + tab.label);
    ttftCard.title = '流式：首次文本、思考或工具内容；非流式：首响应字节。未产生流内容的请求不计入 TTFT 样本。';
    bigValue(ttftCard, fmtMs(firstToken.p99_ms ?? firstToken.p95_ms, firstToken.samples), firstToken.p99_ms == null ? 'P95' : 'P99', firstToken.samples ? '' : 'is-muted');
    rowList(ttftCard, percentileRows(firstToken).filter((row) => row.label !== 'P99'));
    container.appendChild(ttftCard);

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
    const totals = payload.totals || {};
    state.overview = payload;
    const buckets = liveBuckets(state.liveWindow);
    const qpsPoints = buckets.map(point => ({ label: fmtMinute(point.minute), value: point.requests / point.seconds }));
    const tpsPoints = buckets.map(point => ({ label: fmtMinute(point.minute), value: ((point.input_tokens || 0) + (point.output_tokens || 0)) / point.seconds }));
    const seconds = buckets.reduce((sum, point) => sum + point.seconds, 0);
    const requests = buckets.reduce((sum, point) => sum + point.requests, 0);
    const tokens = buckets.reduce((sum, point) => sum + (point.input_tokens || 0) + (point.output_tokens || 0), 0);
    const hasUsage = requests === 0 || tokens > 0 || buckets.some(point => point.usage_samples > 0);
    const current = qpsPoints.length ? qpsPoints[qpsPoints.length - 1].value : 0;
    const peak = qpsPoints.reduce((max, point) => Math.max(max, point.value), 0);
    const avg = seconds ? requests / seconds : 0;
    const tpsCurrent = tpsPoints.length ? tpsPoints[tpsPoints.length - 1].value : 0;
    const tpsPeak = tpsPoints.reduce((max, point) => Math.max(max, point.value), 0);
    const tpsAvg = seconds ? tokens / seconds : 0;
    setText('opsLiveQpsNow', buckets.length ? current.toFixed(2) : '未采集');
    setText('opsLiveQpsPeak', buckets.length ? peak.toFixed(2) : '未采集');
    setText('opsLiveQpsAvg', buckets.length ? avg.toFixed(2) : '未采集');
    setText('opsLiveTpsNow', buckets.length && hasUsage ? tpsCurrent.toFixed(1) : '未采集');
    setText('opsLiveTpsPeak', buckets.length && hasUsage ? tpsPeak.toFixed(1) : '未采集');
    setText('opsLiveTpsAvg', buckets.length && hasUsage ? tpsAvg.toFixed(1) : '未采集');
    setText('opsHeroHint', `最近 ${state.liveWindow} 个分钟桶 · 当前分钟按已过时间计算 · 15 秒刷新`);

    // Health: the SLA of the window, with the traffic level deciding whether the
    // console is "serving" or "standby".
    const real = Math.max(totals.requests || 0, 0);
    const rate = real > 0 ? Math.min((totals.success || 0) / real, 1) : 0;
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
      const parts = [
        `${payload.window_minutes} 分钟 ${fmtInt(real)} 次请求`,
        `最终失败 ${fmtInt(totals.failed || 0)}`,
        `上游尝试失败 ${fmtInt(totals.attempt_failures || 0)}`,
      ];
      parts.forEach((part) => {
        const span = document.createElement('span');
        span.textContent = part;
        gaugeSub.appendChild(span);
      });
      if (totals.failed > 0) {
        // A click target on the number an operator would reach for anyway.
        const link = document.createElement('button');
        link.type = 'button';
        link.className = 'ops-inline-link';
        link.id = 'opsHeroUpstreamErrors';
        link.textContent = '查看失败请求 →';
        link.addEventListener('click', () => drilldownToLogs({ outcome: 'failed' }));
        gaugeSub.appendChild(link);
      }
    }

    sparkline(qpsPoints.slice(-30));
  }

  // --- resource row ----------------------------------------------------------

  function renderResources(payload) {
    const container = el('opsResources');
    if (!container) return;
    container.replaceChildren();
    const metrics = payload && payload.available !== false ? (payload.metrics || []) : [];
    if (!metrics.length) {
      const note = document.createElement('p');
      note.className = 'ops-empty';
      note.textContent = (payload && payload.note) || '运行时指标不可用。';
      container.appendChild(note);
      return;
    }
    metrics.forEach((metric) => {
      const card = document.createElement('article');
      card.className = 'ops-resource is-' + (metric.status || 'unknown');
      const head = document.createElement('div');
      head.className = 'ops-resource-head';
      head.textContent = metric.available ? metric.label : metric.label + '（不可用）';
      const value = document.createElement('div');
      value.className = 'ops-resource-value';
      value.textContent = metric.available ? metric.value : '不可用';
      const detail = document.createElement('div');
      detail.className = 'ops-resource-detail';
      detail.textContent = [metric.detail, metric.thresholds].filter(Boolean).join(' · ');
      card.appendChild(head);
      card.appendChild(value);
      card.appendChild(detail);
      container.appendChild(card);
    });
  }

  // --- concurrency -----------------------------------------------------------

  function renderConcurrency(payload) {
    const container = el('opsConcurrency');
    if (!container) return;
    container.replaceChildren();
    const rows = (payload.matrix || []).filter((row) => IsProvider(row.channel));
    const head = document.createElement('div');
    head.className = 'ops-platform-head';
    head.textContent = '按平台';
    const count = document.createElement('span');
    count.textContent = `共 ${rows.length} 项`;
    head.appendChild(count);
    container.appendChild(head);

    if (!rows.length) {
      const note = document.createElement('p');
      note.className = 'ops-platform-empty';
      note.textContent = '还没有渠道数据。';
      container.appendChild(note);
      return;
    }
    rows.forEach((row) => {
      const enabled = row.accounts_enabled || 0;
      const available = row.accounts_available || 0;
      const ratio = enabled > 0 ? available / enabled : 0;
      const card = document.createElement('div');
      card.className = 'ops-platform';
      const top = document.createElement('div');
      top.className = 'ops-platform-top';
      const name = document.createElement('span');
      name.className = 'ops-platform-name';
      name.textContent = row.channel;
      const rate = document.createElement('span');
      rate.className = 'ops-platform-rate';
      rate.textContent = row.concurrency_available ? `${row.active_requests || 0} 活跃请求 · ${available}/${enabled} 可用账号` : `${available}/${enabled} 可用账号 · 并发未采集`;
      top.appendChild(name);
      top.appendChild(rate);
      card.appendChild(top);
      meter(card, ratio, ratio >= 0.99 ? '' : ratio > 0 ? 'is-warn' : 'is-error');
      const badges = document.createElement('div');
      badges.className = 'ops-platform-badges';
      if (row.accounts_needing_login) addBadge(badges, `需登录 ${row.accounts_needing_login}`, 'is-error');
      if (row.model_cooldowns) addBadge(badges, `限流 ${row.model_cooldowns}`, 'is-warn');
      if (!row.accounts_needing_login && !row.model_cooldowns) addBadge(badges, '无限制', 'is-ok');
      card.appendChild(badges);
      container.appendChild(card);
    });
  }

  function addBadge(parent, text, tone) {
    const badge = document.createElement('span');
    badge.className = 'logs-badge ' + (tone || '');
    badge.style.marginLeft = '0';
    badge.textContent = text;
    parent.appendChild(badge);
  }

  function IsProvider(channel) {
    const name = String(channel || '').toLowerCase();
    // The infrastructure aggregates are counted but are not provider channels.
    return name !== '' && name !== 'http' && name !== 'probe';
  }

  // --- trends ----------------------------------------------------------------

  function renderTrends(payload) {
    const throughput = el('opsThroughput');
    const switchTrend = el('opsSwitchTrend');
    const errorTrend = el('opsErrorTrend');
    const points = trendBuckets();

    if (throughput) {
      const qps = points.map((p) => ({ label: fmtMinute(p.minute), value: (p.requests || 0) / p.seconds }));
      const tps = points.map((p) => ({
        label: fmtMinute(p.minute),
        value: ((p.input_tokens || 0) + (p.output_tokens || 0)) / p.seconds,
      }));
      const hasTokens = tps.some((point) => point.value > 0);
      lineChart(throughput, {
        points: qps,
        secondaryPoints: hasTokens ? tps : null,
        area: true,
        className: 'series-qps',
        height: 190,
        rightAxis: hasTokens,
        emptyText: '这段时间没有流量样本。',
      });
      setText('opsThroughputHint', hasTokens
        ? '左轴 QPS（次/秒） · 右轴 TPS（token/秒） · 当前分钟按已过时间计算'
        : '左轴 QPS（次/秒） · 窗口内请求未上报用量，TPS 暂不绘制');
    }
    if (switchTrend) {
      const average = points.map((p) => ({
        label: fmtMinute(p.minute),
        value: p.account_switch_count ? (p.account_switch_sum || 0) / p.account_switch_count : null,
      }));
      lineChart(switchTrend, {
        points: average,
        className: 'series-switch',
        height: 150,
        emptyText: '这段时间没有账号切换样本。',
      });
    }
    if (errorTrend) {
      // Two series, not one: 最终失败 is what the caller saw, 上游尝试失败 counts
      // attempts that died before a retry rescued the request. Plotting only the
      // first hid exactly the upstream trouble an operator is looking for.
      const failures = points.map((p) => ({ label: fmtMinute(p.minute), value: p.failed || 0 }));
      const attempts = points.map((p) => ({ label: fmtMinute(p.minute), value: p.attempt_failures || 0 }));
      const legend = el('opsErrorTrendLegend');
      if (legend) {
        legend.replaceChildren();
        [['最终失败', 'series-error'], ['上游尝试失败', 'series-alt']].forEach(([label, className]) => {
          const item = document.createElement('span');
          item.className = 'ops-legend-item';
          const swatch = document.createElement('span');
          swatch.className = 'ops-legend-swatch ' + className;
          const text = document.createElement('span');
          text.textContent = label;
          item.appendChild(swatch);
          item.appendChild(text);
          legend.appendChild(item);
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
          drilldownToLogs({
            outcome: 'failed',
            since: minute.toISOString(),
            until: new Date(minute.getTime() + 60 * 1000).toISOString(),
          });
        },
      });
    }
  }

  function renderDistributions(payload) {
    const totals = payload.totals || {};
    const histogram = el('opsHistogram');
    const errorMix = el('opsErrorMix');
    const tab = outcomeTab(state.outcome);
    const bins = totals[tab.histogram] || [];

    if (histogram) {
      histogram.replaceChildren();
      if (!bins.length) {
        const note = document.createElement('p');
        note.className = 'ops-empty';
        // An empty chart is indistinguishable from a broken one, so say which of
        // the two it is: no samples in this window, or no distribution collected.
        note.textContent = (totals[tab.duration] && totals[tab.duration].samples)
          ? '该窗口没有分布样本。'
          : '该时间窗口内暂无延迟样本（口径：' + tab.label + '）。';
        histogram.appendChild(note);
      }
      const maxCount = Math.max(1, ...bins.map((bin) => bin.count || 0));
      bins.forEach((bin) => {
        const col = document.createElement('div');
        col.className = 'ops-histogram-col';
        const count = document.createElement('span');
        count.className = 'ops-histogram-count';
        count.textContent = String(bin.count || 0);
        const bar = document.createElement('div');
        bar.className = 'ops-histogram-bar';
        bar.style.height = ((bin.count || 0) / maxCount * 100).toFixed(1) + '%';
        bar.title = `${bin.label}: ${bin.count || 0}`;
        const label = document.createElement('span');
        label.className = 'ops-histogram-label';
        label.textContent = bin.label;
        col.appendChild(count);
        col.appendChild(bar);
        col.appendChild(label);
        histogram.appendChild(col);
      });
      const cohort = totals[tab.duration] || {};
      setText('opsHistogramHint', (cohort.samples || 0) ? `${cohort.samples} 个样本 · 口径：${tab.label}` : '暂无样本');
    }

    if (errorMix) {
      errorMix.replaceChildren();
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
      groups.forEach((group) => {
        const row = document.createElement('button');
        row.type = 'button';
        row.className = 'ops-error-row is-clickable';
        // Clicking a class opens the log centre filtered to exactly that class.
        row.addEventListener('click', () => drilldownToLogs({ outcome: group.outcome }));
        const label = document.createElement('span');
        label.textContent = group.label;
        const bar = document.createElement('div');
        bar.className = 'ops-error-meter';
        const fill = document.createElement('span');
        if (group.tone) fill.className = group.tone;
        fill.style.width = (group.value / maxValue * 100).toFixed(1) + '%';
        bar.appendChild(fill);
        const value = document.createElement('span');
        value.textContent = (totals.requests > 0 && !totals.detailed_requests) ? '未采集' : fmtInt(group.value);
        row.appendChild(label);
        row.appendChild(bar);
        row.appendChild(value);
        errorMix.appendChild(row);
      });
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
    const channel = (el('opsAlertChannel') || {}).value || '';
    body.replaceChildren();
    try {
      const params = new URLSearchParams({ kind: 'system', action: 'alert_', limit: '50' });
      if (channel) params.set('channel', channel);
      const response = await fetch('/api/journal/records?' + params.toString(), { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      const rows = (payload.data || []).filter((record) => {
        const event = record.event || {};
        if (event.action !== 'alert_fired' && event.action !== 'alert_recovered') return false;
        const level = (event.metadata && event.metadata.severity) || '';
        return !severity || level === severity;
      });
      if (!rows.length) {
        const tr = document.createElement('tr');
        const td = document.createElement('td');
        td.colSpan = 6;
        td.className = 'table-empty-cell';
        td.textContent = '暂无告警事件（保留窗口内）。';
        tr.appendChild(td);
        body.appendChild(tr);
        return;
      }
      rows.forEach((record) => {
        const event = record.event || {};
        const tr = document.createElement('tr');
        const severity = String((event.metadata && event.metadata.severity) || '').toLowerCase();
        const cells = [
          fmtClock(new Date(event.timestamp)),
          event.action === 'alert_fired' ? '触发' : '恢复',
          // The level is a word an operator reads, not the key the engine stores: the
          // table printed "warning" next to fully Chinese text.
          ALERT_SEVERITY_LABELS[severity] || (severity ? severity : '—'),
          event.channel || '—',
          event.model || '—',
          event.error || event.details || '—',
        ];
        cells.forEach((value, index) => {
          const cell = ALERT_CELLS[index];
          const td = document.createElement('td');
          td.textContent = value;
          // Named so the phone layout can place each cell: six columns cannot fit a
          // 360px screen, and the card view in ops.css positions these by class.
          td.className = cell.className;
          td.dataset.label = cell.label;
          if (index === 2) {
            td.className += ' ops-alert-severity ' + (severity === 'critical' ? 'is-critical' : severity === 'warning' ? 'is-warning' : '');
          }
          tr.appendChild(td);
        });
        body.appendChild(tr);
      });
    } catch (error) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 6;
      td.className = 'table-empty-cell';
      td.textContent = '读取告警事件失败：' + (error.message || error);
      tr.appendChild(td);
      body.appendChild(tr);
    }
  }

  // --- matrix ----------------------------------------------------------------

  // historyCells renders per-minute blocks. A model row uses its own history, so
  // "when did this model start failing" is answerable per model instead of only
  // per channel; a channel row uses the channel's series.
  function historyCells(series) {
    const cell = document.createElement('td');
    cell.className = 'ops-history-cells';
    const buckets = (series || []).slice(-40);
    if (!buckets.length) {
      cell.textContent = '暂无样本';
      cell.classList.add('ops-empty');
      return cell;
    }
    buckets.forEach((point) => {
      const block = document.createElement('span');
      block.className = 'ops-block';
      const requests = point.requests || 0;
      const failed = point.failed || 0;
      if (!requests) {
        block.classList.add('is-idle');
      } else if (failed >= requests) {
        block.classList.add('is-bad');
      } else if (failed > 0) {
        block.classList.add('is-warn');
      } else {
        block.classList.add('is-ok');
      }
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

  function matrixRow(label, row, options) {
    const tr = document.createElement('tr');
    tr.className = options.isModel ? 'is-model' : 'is-channel';

    const name = document.createElement('td');
    name.className = 'ops-matrix-name';
    name.textContent = label;
    // No data-label here on purpose: the card prints the cell's label above its value,
    // and this cell's value is the channel or model name — "渠道 / 模型 / 名称"
    // labelled the heading twice. The other seven cells carry theirs.
    // The row name is the drill-down: a channel opens its own traffic, a model its
    // channel-and-model traffic, both over the window the page is showing.
    name.classList.add('is-clickable');
    name.title = '点击查看该' + (options.isModel ? '模型的请求日志' : '渠道的请求日志');
    name.setAttribute('role', 'button');
    name.setAttribute('tabindex', '0');
    const open = () => drilldownToLogs(options.isModel
      ? { channel: options.channel || state.channel, model: label }
      : { channel: label });
    name.addEventListener('click', open);
    name.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        open();
      }
    });
    tr.appendChild(name);

    const accounts = document.createElement('td');
    if (options.isModel) accounts.textContent = '—';
    else if (row.accounts_enabled === 0) accounts.textContent = '未配置';
    else accounts.textContent = `${row.accounts_available} / ${row.accounts_enabled}` + (row.accounts_needing_login ? `（需登录 ${row.accounts_needing_login}）` : '');
    tr.appendChild(accounts);

    const requests = document.createElement('td');
    requests.textContent = options.isModel ? String(row.requests || 0) : String(Math.max((row.summary && row.summary.requests) || 0, 0));
    tr.appendChild(requests);

    const rate = document.createElement('td');
    const samples = options.isModel ? row.samples || 0 : (row.summary && row.summary.samples) || 0;
    const r = options.isModel ? row.success_rate || 0 : (row.summary && row.summary.success_rate) || 0;
    rate.textContent = formatRate(r, samples);
    if (!samples) rate.className = 'ops-empty';
    tr.appendChild(rate);

    const ttft = document.createElement('td');
    if (options.isModel) {
      // A model's first-token figure needs its own sample count: "no sample" and
      // "0 ms" must not look the same, and a rate limit produces no token at all.
      const ttftSamples = row.first_token_samples || 0;
      ttft.textContent = fmtMs(row.first_token_p95_ms, ttftSamples);
      if (!ttftSamples) ttft.className = 'ops-empty';
      else ttft.title = `P95 · ${ttftSamples} 个样本`;
    } else {
      ttft.textContent = fmtMs(row.summary && row.summary.first_token_p95_ms, samples);
    }
    tr.appendChild(ttft);

    const duration = document.createElement('td');
    duration.textContent = options.isModel ? fmtMs(row.duration_p95_ms, samples) : fmtMs(row.summary && row.summary.duration_p95_ms, samples);
    tr.appendChild(duration);

    const throttled = document.createElement('td');
    throttled.textContent = options.isModel ? '—' : String(row.model_cooldowns || 0);
    tr.appendChild(throttled);

    tr.appendChild(historyCells(options.isModel ? row.history : row.series));
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

  function formatRate(rate, samples) {
    if (!samples) return '暂无样本';
    return (rate * 100).toFixed(1) + '%';
  }

  function renderMatrix(rows) {
    const table = el('opsMatrix');
    if (!table) return;
    const body = table.querySelector('tbody');
    body.replaceChildren();
    const visible = (rows || []).filter((row) => !state.model || (row.models || []).some((model) => model.model === state.model));
    if (!visible.length) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 8;
      td.className = 'table-empty-cell';
      td.textContent = '还没有渠道数据。添加账号或等待流量后这里会出现状态矩阵。';
      tr.appendChild(td);
      body.appendChild(tr);
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

  function renderCoverage(payload) {
    const node = el('opsCoverage');
    if (!node) return;
    const parts = [];
    const coverage = payload.coverage || {};
    if (typeof coverage.entries === 'number') {
      parts.push(`审计日志保留 ${coverage.entries} 条`);
      if (coverage.oldest) parts.push(`最早 ${coverage.oldest}`);
      if (coverage.newest) parts.push(`最新 ${coverage.newest}`);
    } else {
      parts.push('审计日志覆盖范围未能读取（接口未返回 coverage）');
    }
    if (coverage.counts) {
      const counts = Object.keys(coverage.counts).map((key) => `${key}=${coverage.counts[key]}`).join('、');
      if (counts) parts.push(`采样计数：${counts}`);
    }
    parts.push('因此页面只承诺“保留窗口内”的结论，不承诺固定天数。');
    const excluded = payload.excluded_aggregates || [];
    if (excluded.length) {
      const labels = excluded.map((name) => {
        if (name === 'http') return 'http（非推理路径：管理页、健康检查、公网扫描）';
        if (name === 'probe') return 'probe（旧版本探测流量的历史聚合，已不再产生）';
        return name;
      });
      parts.push('已计数但不在渠道矩阵中显示：' + labels.join('；'));
    }
    parts.push('Token / TPS 只统计上报了用量的请求；未上报用量的渠道其 TPS 会偏低。');
    node.textContent = parts.join('；');
    // When the aggregation itself is disabled the reason matters more than the
    // coverage caveat: the whole page is showing nothing because of it.
    if (payload.available === false && payload.note) {
      node.textContent = payload.note + '；' + node.textContent;
    }
  }

  function updateChannelOptions(channels, current) {
    const select = el('opsChannel');
    if (!select) return;
    select.innerHTML = ['<option value="">全部渠道</option>']
      .concat((channels || []).map((channel) => `<option value="${channel}">${channel}</option>`))
      .join('');
    select.value = current || '';
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
    const names = [];
    (rows || []).forEach((row) => (row.models || []).forEach((model) => {
      if (model.model && names.indexOf(model.model) === -1) names.push(model.model);
    }));
    names.sort();
    select.innerHTML = ['<option value="">全部模型</option>']
      .concat(names.map((name) => `<option value="${name}">${name}</option>`))
      .join('');
    select.value = current || '';
  }

  // --- load ------------------------------------------------------------------

  function setStatus(text, tone) {
    setText('opsStatusText', text);
    const dot = el('opsStatusDot');
    if (dot) dot.className = 'ops-dot' + (tone ? ' ' + tone : '');
  }

  async function load() {
    if (state.loading) {
      state.refreshPending = true;
      return;
    }
    state.loading = true;
    if (!state.overview) renderSkeletons();
    setStatus('读取中…', 'is-warn');
    const windowMinutes = state.window;
    const params = new URLSearchParams({ window: String(windowMinutes) });
    if (state.channel) params.set('channel', state.channel);
    syncUrl();
    try {
      const [overviewResponse, runtimeResponse] = await Promise.all([
        fetch('/api/ops/overview?' + params.toString(), { credentials: 'same-origin' }),
        fetch('/api/ops/runtime', { credentials: 'same-origin' }).catch(() => null),
      ]);
      if (!overviewResponse.ok) throw new Error('HTTP ' + overviewResponse.status);
      const payload = await overviewResponse.json();
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
      if (runtimeResponse && runtimeResponse.ok) renderResources(await runtimeResponse.json());
      else renderResources({ available: false, note: '运行时指标读取失败。' });
      setText('opsRefreshedAt', fmtClock(new Date()));
      setStatus('就绪', '');
      state.countdown = state.refreshSeconds;
    } catch (error) {
      const container = el('opsKpis');
      if (container) {
        container.replaceChildren();
        const card = kpiCard('读取失败');
        bigValue(card, '—', '', 'is-muted');
        rowList(card, [{ label: '原因', value: String(error.message || error) }]);
        container.appendChild(card);
      }
      renderCoverage({ window_minutes: windowMinutes, coverage: {}, excluded_aggregates: [] });
      const coverage = el('opsCoverage');
      if (coverage) coverage.textContent = `指标读取失败：${String(error.message || error)}。会话可能已过期，请重新登录后刷新。`;
      setStatus('读取失败', 'is-error');
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

    const windowSelect = el('opsWindow');
    if (windowSelect) {
      windowSelect.addEventListener('change', () => {
        state.window = Number(windowSelect.value) || 180;
        load();
      });
    }
    const channelSelect = el('opsChannel');
    if (channelSelect) {
      channelSelect.addEventListener('change', () => {
        state.channel = channelSelect.value;
        load();
      });
    }
    const modelSelect = el('opsModel');
    if (modelSelect) {
      modelSelect.addEventListener('change', () => {
        state.model = modelSelect.value;
        syncUrl();
        if (state.overview) renderMatrix(state.overview.matrix);
      });
    }
    const refresh = el('opsRefresh');
    if (refresh) refresh.addEventListener('click', load);

    // The hero's failure link is created with the gauge sub-line (it only exists
    // when there is a failure to look at), so its listener is attached there.
    const alertsLink = el('opsAlertsLink');
    if (alertsLink) {
      alertsLink.addEventListener('click', () => {
        if (window.location) window.location.href = pagePath() + '?tab=alerts';
      });
    }

    // One place marks the selected window, so the chips and the state cannot drift:
    // 重置 used to set the state to 1 minute and leave "1h" lit as the selection.
    function selectLiveWindow(minutes) {
      document.querySelectorAll('#opsLiveTabs .ops-chip').forEach((chip) => {
        const value = Number(chip.getAttribute('data-live')) || 1;
        chip.classList.toggle('is-active', value === minutes);
      });
      state.liveWindow = minutes;
      if (state.overview) renderHero(state.overview);
    }

    document.querySelectorAll('#opsLiveTabs .ops-chip').forEach((button) => {
      button.addEventListener('click', () => {
        selectLiveWindow(Number(button.getAttribute('data-live')) || 1);
      });
    });

    const reset = el('opsTrendReset');
    if (reset) {
      reset.addEventListener('click', () => {
        selectLiveWindow(1);
        if (state.overview) {
          renderTrends(state.overview);
        }
        showToast('已重置实时与趋势视图');
      });
    }
    const download = el('opsTrendDownload');
    if (download) {
      download.addEventListener('click', () => {
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
    }

    const alertReload = el('opsAlertReload');
    if (alertReload) alertReload.addEventListener('click', loadAlertEvents);
    const alertSeverity = el('opsAlertSeverity');
    if (alertSeverity) alertSeverity.addEventListener('change', loadAlertEvents);
    const alertChannel = el('opsAlertChannel');
    if (alertChannel) alertChannel.addEventListener('change', loadAlertEvents);

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
    state.refreshPending = false;
    load();
    loadAlertEvents();
  }

  // Returning to the tab has to catch up immediately: without this the operator
  // can sit down to a dashboard that is up to a full refresh interval stale.
  function flushPendingRefresh() {
    if (typeof document === 'undefined' || document.hidden || !state.refreshPending) return;
    state.refreshPending = false;
    state.countdown = state.refreshSeconds;
    load();
    loadAlertEvents();
  }

  // applyUrlState puts the page back where the URL says it should be: the filters
  // a drill-down returned to, or a bookmarked scope.
  function applyUrlState() {
    readUrlState();
    const windowSelect = el('opsWindow');
    if (windowSelect) windowSelect.value = String(state.window);
    const channelSelect = el('opsChannel');
    if (channelSelect && state.channel) channelSelect.value = state.channel;
    const modelSelect = el('opsModel');
    if (modelSelect && state.model) modelSelect.value = state.model;
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { applyUrlState(); bind(); load(); loadAlertEvents(); });
  } else {
    applyUrlState();
    bind();
    load();
    loadAlertEvents();
  }
  if (typeof document.addEventListener === 'function') {
    document.addEventListener('visibilitychange', flushPendingRefresh);
  }
  state.timer = setInterval(tick, 1000);
})();
