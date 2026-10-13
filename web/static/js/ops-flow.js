// Live animation is driven by SSE handler start/finish events, independently of
// the 15-second historical metrics refresh.
(function () {
  const { el, setText } = ConsoleUI;
  const upstreams = ['workbuddy', 'cline', 'qoder', 'grok'];
  const protocols = ['chat', 'responses', 'messages'];
  const names = { workbuddy: 'WorkBuddy', cline: 'Cline', qoder: 'Qoder', grok: 'Grok',
    chat: 'OpenAI Chat Completions', responses: 'OpenAI Responses', messages: 'Claude Messages', gateway: 'API Console' };
  let source = null;
  let snapshot = null;
  let previous = null;
  let selected = '';
  let pulseTimer = null;
  const pulseUntil = new Map();

  function scope() { return (el('opsChannel') || {}).value || ''; }
  function rowsFor(key) {
    return ((snapshot && snapshot.routes) || []).filter(row => key === 'gateway' || row.channel === key || row.protocol === key);
  }
  function sum(rows, field) { return rows.reduce((total, row) => total + Number(row[field] || 0), 0); }
  function rates(rows) {
    return { qps: sum(rows, "qps").toFixed(2), tps: rows.every(row => row.usage_available === true) ? sum(rows, "tps").toFixed(1) : "—" };
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
    setText('opsFlowDetail', snapshot
      ? names[selected] + ' · QPS ' + rates(rows).qps + ' · TPS ' + rates(rows).tps + ' · 当前进行中 ' + sum(rows, 'active') + ' · 本实例启动以来接收 ' + sum(rows, 'started') + ' · 已结束 ' + sum(rows, 'completed') +
        (upstreams.includes(selected) ? ' · 按下游接口：' + protocols.map(key => names[key] + ' ' + sum(rows.filter(row => row.protocol === key), 'active')).join(' / ') : '')
      : names[selected] + ' · 实时连接尚未就绪');
  }
  function render() {
    const now = Date.now();
    let nextPulse = 0;
    [...upstreams, ...protocols, 'gateway'].forEach(key => {
      const rows = rowsFor(key);
      const active = sum(rows, 'active');
      const until = pulseUntil.get(key) || 0;
      const rate = rates(rows);
      flowing(key, !!snapshot && (active > 0 || until > now));
      if (until > now) nextPulse = Math.max(nextPulse, until - now);
      if (key === 'gateway') {
        setText('opsFlowActive', snapshot ? active + ' 个进行中' : '— 个进行中');
        setText('opsGatewayQps', snapshot ? rate.qps : '—');
        setText('opsGatewayTps', snapshot ? rate.tps : '—');
      } else {
        const filtered = upstreams.includes(key) && scope() && scope() !== key;
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
    if (!payload || !payload.available || !Array.isArray(payload.routes)) return;
    const sameInstance = previous && previous.instance_started === payload.instance_started;
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
