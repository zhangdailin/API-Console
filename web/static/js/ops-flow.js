// Live animation is driven by SSE handler start/finish events, independently of
// the 15-second historical metrics refresh.
(function () {
  const { el, setText, make } = ConsoleUI;
  const upstreams = ['workbuddy', 'cline', 'qoder', 'grok'];
  const protocols = ['chat', 'responses', 'messages'];
  const names = { workbuddy: 'WorkBuddy', cline: 'Cline', qoder: 'Qoder', grok: 'Grok',
    chat: 'OpenAI Chat Completions', responses: 'OpenAI Responses', messages: 'Claude Messages', gateway: 'API Console' };
  let source = null;
  let snapshot = null;
  let previous = null;
  let selected = '';
  let pulseTimer = null;
  let pools = null;
  const pulseUntil = new Map();

  function scope() { return (el('opsChannel') || {}).value || ''; }
  function rowsFor(key) {
    return ((snapshot && snapshot.routes) || []).filter(row => key === 'gateway' || row.channel === key || row.protocol === key);
  }
  function sum(rows, field) { return rows.reduce((total, row) => total + Number(row[field] || 0), 0); }
  function rates(rows) {
    const known = rows.length > 0 && rows.every(row => row.usage_available === true);
    return { qps: sum(rows, 'qps').toFixed(2), input: known ? sum(rows, 'input_tps').toFixed(1) : '—',
      tps: known ? sum(rows, 'output_tps').toFixed(1) : '—' };
  }
  function metrics(key) { return snapshot && snapshot.nodes ? snapshot.nodes[key] : null; }
  function ms(value) {
    if (value == null || !Number.isFinite(Number(value))) return '—';
    return Number(value) < 1000 ? Math.round(value) + ' ms' : (Number(value) / 1000).toFixed(1) + ' s';
  }
  function percent(value) { return value == null ? '—' : (Number(value) * 100).toFixed(1) + '%'; }
  function latencyWindow() {
    return '近 ' + (snapshot?.latency_window_seconds ?? snapshot?.window_seconds ?? 900) / 60 + ' 分钟';
  }
  function distribution(value, missing) {
    return value && value.samples > 0 ? 'P50 ' + ms(value.p50_ms) + ' / P95 ' + ms(value.p95_ms) + ' · ' + value.samples + ' 样本'
      : '— · ' + latencyWindow() + (missing || '无样本');
  }
  function nodeLatency(stats, filtered) {
    if (filtered) return { text: 'P95 范围外', title: '当前渠道筛选范围外' };
    if (!stats) return { text: 'P95 待采集', title: '实时延迟数据尚未就绪' };
    const valid = value => value?.samples > 0 && value.p95_ms != null && Number.isFinite(Number(value.p95_ms));
    const generation = valid(stats.first_generation), body = valid(stats.body_ttfb);
    const details = [];
    if (generation) details.push('首生成 P95 ' + ms(stats.first_generation.p95_ms) + ' · ' + stats.first_generation.samples + ' 个流式样本');
    if (body) details.push('正文 TTFB P95 ' + ms(stats.body_ttfb.p95_ms) + ' · ' + stats.body_ttfb.samples + ' 个非流式样本');
    return {
      text: generation ? '首生成 P95 ' + ms(stats.first_generation.p95_ms)
        : body ? '正文 TTFB P95 ' + ms(stats.body_ttfb.p95_ms) : 'P95 无近期样本',
      title: latencyWindow() + ' · ' + (details.join('；') || '没有有效的流式首生成或非流式正文 TTFB 样本，请求结束后计入') +
        (generation && body ? '；节点优先显示首生成，两类样本独立统计' : ''),
    };
  }
  function poolFact() {
    if (!upstreams.includes(selected) && selected !== 'gateway') return null;
    const rows = (pools || []).filter(row => selected === 'gateway' ? !scope() || row.channel === scope() : row.channel === selected);
    const fresh = rows.length > 0 && rows.every(row => row.capacity_available === true &&
      Number.isFinite(Date.parse(row.capacity_sampled_at)) && Date.now() - Date.parse(row.capacity_sampled_at) < 30000);
    if (!fresh) return ['账号容量', '— · 容量快照未就绪或已过期'];
    const capacity = sum(rows, 'capacity_slots'), occupied = sum(rows, 'occupied_slots');
    return ['账号容量', sum(rows, 'accounts_available') + '/' + sum(rows, 'accounts_enabled') + ' 可用账号 · ' + occupied + '/' + capacity +
      ' 可用池槽位占用' + (capacity > 0 ? '（' + (occupied / capacity * 100).toFixed(1) + '%）' : '') +
      ' · ' + new Date(rows[0].capacity_sampled_at).toLocaleTimeString() + ' 采样'];
  }
  function flowing(key, on) {
    ['opsNode-', 'opsWire-'].forEach(prefix => {
      const node = el(prefix + key);
      if (node) node.classList.toggle('is-flowing', on);
    });
  }
  function renderDetail() {
    if (!selected) return;
    const rows = rowsFor(selected);
    setText('opsFlowDetailTitle', names[selected] + (snapshot ? rows.length ? ' · 当前实例 · 延迟' + latencyWindow() + ' · 错误与等待近 60 秒' : ' · 当前筛选范围外' : ' · 实时连接尚未就绪'));
    const container = el('opsFlowDetailFacts');
    if (!container) return;
    container.replaceChildren();
    if (!snapshot || !rows.length) return;
    const stats = metrics(selected), rate = rates(rows);
    const facts = [
      ['实时速率', 'QPS ' + rate.qps + ' · 输入 ' + rate.input + ' / 输出 ' + rate.tps + ' token/s'],
      ['请求状态', sum(rows, 'active') + ' 进行中' + (stats ? ' · ' + stats.waiting + ' 尚未输出 / ' + stats.responding + ' 已开始输出' : '')],
      ['实例累计', '接收 ' + sum(rows, 'started') + ' · 已结束 ' + sum(rows, 'completed')],
    ];
    if (stats) {
      facts.push(
        ['首生成延迟', distribution(stats.first_generation, '无流式首生成样本')],
        ['可见首字延迟', distribution(stats.first_visible, '无可见文本样本')],
        ['请求总时长', distribution(stats.duration, '无结束请求样本')],
        ['非流式正文 TTFB', distribution(stats.body_ttfb, '无非流式正文样本')],
        ['失败状态', percent(stats.failure_rate) + ' · ' + stats.failed + '/' + stats.completed + ' 失败 · 最终 429/529 ' + stats.rate_limited + ' · 流中断 ' + stats.stream_errors + ' · 超时 ' + stats.timeouts + ' · 取消 ' + stats.canceled],
        ['上游用量覆盖', stats.usage_reported + '/' + stats.completed + ' 个已结束请求'],
        ['重试与切号', stats.observed > 0 ? stats.retried + ' 个请求重试 · ' + stats.attempts + ' 次尝试 · ' + stats.account_switches + ' 次切号' : '— · 未采集'],
        ['平均等待', stats.observed > 0 ? '重试 ' + ms(stats.retry_wait_ms / stats.observed) + ' · 其中排队 ' + ms(stats.queue_wait_ms / stats.observed) + ' · ' + stats.observed + ' 个请求' : '— · 未采集'],
        ['最长进行时长', sum(rows, 'active') > 0 ? ms(stats.longest_active_ms) : '— · 无进行中请求'],
      );
      const labels = { dns_ms: 'DNS', tcp_ms: 'TCP', tls_ms: 'TLS', connection_wait_ms: '取连接', response_headers_ms: '响应头' };
      const phases = Object.entries(labels).flatMap(([key, label]) => {
        const phase = (stats.http_phases || {})[key];
        return phase && phase.samples > 0 ? [label + ' ' + ms(phase.avg_ms) + '（' + phase.samples + ' 次）'] : [];
      });
      facts.push(['上游连接阶段均值', phases.join(' · ') || '— · 未采集']);
    }
    const pool = poolFact();
    if (pool) facts.push(pool);
    if (upstreams.includes(selected)) facts.push(['按下游接口', protocols.map(key => names[key] + ' ' + sum(rows.filter(row => row.protocol === key), 'active')).join(' / ')]);
    facts.forEach(([label, value]) => {
      const fact = make('div', 'ops-flow-fact');
      fact.appendChild(make('span', 'ops-flow-fact-label', label));
      fact.appendChild(make('span', 'ops-flow-fact-value', value));
      container.appendChild(fact);
    });
  }
  function render() {
    const now = Date.now();
    let nextPulse = 0;
    [...upstreams, ...protocols, 'gateway'].forEach(key => {
      const rows = rowsFor(key);
      const active = sum(rows, 'active');
      const until = pulseUntil.get(key) || 0;
      const rate = rates(rows);
      const stats = metrics(key);
      const filtered = upstreams.includes(key) && scope() && scope() !== key;
      const latency = nodeLatency(stats, filtered);
      setText('opsNodeLatency-' + key, latency.text);
      const latencyNode = el('opsNodeLatency-' + key);
      if (latencyNode) latencyNode.title = latency.title;
      setText('opsNodeFailure-' + key, stats && !filtered ? '失败 ' + percent(stats.failure_rate) : '失败 —');
      const health = el('opsNodeFailure-' + key);
      if (health) health.classList.toggle('is-degraded', !!stats && !filtered && stats.failed > 0);
      flowing(key, !!snapshot && (active > 0 || until > now));
      if (until > now) nextPulse = Math.max(nextPulse, until - now);
      if (key === 'gateway') {
        setText('opsFlowActive', snapshot ? active + ' 个进行中' : '— 个进行中');
        setText('opsGatewayQps', snapshot ? rate.qps : '—');
        setText('opsGatewayTps', snapshot ? rate.tps : '—');
      } else {
        setText('opsNodeMetric-' + key, filtered ? '当前筛选范围外' : snapshot
          ? active + ' 个请求进行中' : '等待实时数据');
        setText('opsNodeQps-' + key, snapshot && !filtered ? rate.qps : '—');
        setText('opsNodeTps-' + key, snapshot && !filtered ? rate.tps : '—');
      }
    });
    renderDetail();
    if (pulseTimer) window.clearTimeout(pulseTimer);
    pulseTimer = nextPulse ? window.setTimeout(render, nextPulse + 20) : null;
  }
  function accept(payload) {
    if (!payload || !payload.available || !Array.isArray(payload.routes)) { unavailable('实时数据不可用'); return; }
    const sameInstance = previous && previous.instance_started === payload.instance_started;
    if (!sameInstance) pulseUntil.clear();
    const old = new Map(sameInstance ? previous.routes.map(row => [row.channel + ':' + row.protocol, row.started]) : []);
    payload.routes.forEach(row => {
      // Keep even a sub-frame request visible for one trip along the wire.
      if (sameInstance && row.started > (old.get(row.channel + ':' + row.protocol) || 0)) {
        [row.channel, row.protocol, 'gateway'].forEach(key => pulseUntil.set(key, Date.now() + 1000));
      }
    });
    snapshot = payload;
    previous = payload;
    setText('opsFlowStatus', '实时推送已连接');
    const dot = el('opsFlowDot');
    if (dot) dot.classList.add('is-live');
    render();
  }
  function unavailable(message) {
    snapshot = null;
    previous = null;
    pulseUntil.clear();
    setText('opsFlowStatus', message);
    const dot = el('opsFlowDot');
    if (dot) dot.classList.remove('is-live');
    render();
  }
  function disconnect() {
    if (source) source.close();
    source = null;
    if (pulseTimer) window.clearTimeout(pulseTimer);
    pulseTimer = null;
  }
  function connect() {
    disconnect();
    unavailable('连接实时流量…');
    if (document.hidden) return;
    if (typeof EventSource === 'undefined') { unavailable('浏览器不支持实时推送'); return; }
    const stream = new EventSource('/api/ops/flow' + (scope() ? '?channel=' + encodeURIComponent(scope()) : ''));
    source = stream;
    stream.addEventListener('flow', event => {
      if (source !== stream) return;
      try { accept(JSON.parse(event.data)); } catch (_) { unavailable('实时数据解析失败'); }
    });
    // EventSource reconnects automatically; stale requests stop glowing at once.
    stream.onerror = () => { if (source === stream) unavailable('实时连接中断，正在重连…'); };
  }
  function init() {
    document.querySelectorAll('[data-flow-node]').forEach(button => {
      const key = button.getAttribute('data-flow-node');
      const health = make('span', 'ops-flow-health');
      ['Latency', 'Failure'].forEach(metric => {
        const value = make('span', '', metric === 'Latency' ? 'P95 待采集' : '失败 —');
        value.id = 'opsNode' + metric + '-' + key;
        health.appendChild(value);
      });
      (button.querySelector('.ops-flow-node-body') || button).appendChild(health);
      button.addEventListener('click', () => {
        selected = button.getAttribute('data-flow-node');
        document.querySelectorAll('[data-flow-node]').forEach(node => {
          const on = node.getAttribute('data-flow-node') === selected;
          node.classList.toggle('is-selected', on);
          node.setAttribute('aria-pressed', String(on));
        });
        renderDetail();
      });
    });
    const channel = el('opsChannel');
    if (channel) channel.addEventListener('change', connect);
    window.addEventListener('ops:pool', event => { pools = Array.isArray(event.detail) ? event.detail : null; renderDetail(); });
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) { disconnect(); unavailable('页面在后台，实时推送已暂停'); }
      else connect();
    });
    window.addEventListener('pagehide', disconnect);
    connect();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
