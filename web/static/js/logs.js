// Log centre: three journals (request / operation / system) sharing one
// collapsible filter bar and one drawer. Request details list every upstream
// attempt and offer the request's diagnostic bundle; change details show the
// redacted summary the server produced; a diagnostic entry renders the captured
// chain of the request — the per-request files that used to live under debug-logs/.
(function () {
  const { el, make, attach } = ConsoleUI;

  // loadSeq numbers the page loads. A filter change can be answered out of order —
  // switching to 操作日志 while the request list is still in flight — and the older
  // answer must not overwrite the newer one.
  const state = { kind: 'request', cursor: '', records: [], selected: null, back: '', loadSeq: 0 };

  // RESULT_LABELS names the classes the overview counts with, so the chip the log
  // centre shows and the chart that opened it use the same words.
  const RESULT_LABELS = {
    failed: '失败（全部失败类型）',
    success: '成功',
    rate_limited: '限流 429/529',
    client_error: '客户端错误 4xx',
    server_error: '上游错误 5xx',
    stream_error: '流中断',
    // Two 401/403 rows, because the status alone does not say who refused: the
    // provider refusing our credential is a channel failure, our gate refusing the
    // caller is not.
    upstream_auth: '上游认证失败 401/403',
    rejected: '网关拒绝 401/403',
    quota_exhausted: '额度用尽 402',
  };

  // FILTER_INPUTS is the one list of what the form holds: reading the URL, writing
  // the URL and clearing all use it, so a new filter cannot be wired into two of
  // the three.
  const FILTER_INPUTS = [
    { id: 'filterChannel', param: 'channel' },
    { id: 'filterModel', param: 'model' },
    { id: 'filterStatus', param: 'status' },
    { id: 'filterOutcome', param: 'outcome' },
    { id: 'filterSince', param: 'since' },
    { id: 'filterUntil', param: 'until' },
    { id: 'filterActor', param: 'actor' },
    { id: 'filterAction', param: 'action' },
  ];

  // FILTER_LABELS names each filter for the scope chips.
  const FILTER_LABELS = { channel: '渠道', model: '模型', status: '状态', outcome: '结果', actor: '操作者', action: '动作' };

  // The capture records a request as a fixed sequence of numbered sections. The
  // order is the request's own timeline, so the panel must not sort it.
  const SECTION_LABELS = {
    '1_http_request.json': '1 · 客户端原始请求',
    '5_http_response_metadata.json': '5 · 客户端响应状态与格式',
    '5_http_response.txt': '5 · 实际返回客户端内容',
    '6_http_summary.json': '6 · HTTP 完成状态',
    '6_request_events.jsonl': '6 · 上游尝试与请求事件',
    '1_claude_request.json': '1 · 客户端请求',
    '1_early_exit.json': '1 · 提前返回',
    '2_converted_prompt.md': '2 · 转换后提示词',
    '3_upstream_request.json': '3 · 上游请求',
    '3_upstream_http_error.json': '3 · 上游错误',
    '5_client_sse.jsonl': '5 · 返回客户端 SSE',
    '6_input_token_breakdown.json': '6 · 输入 token 分解',
    '6_summary.json': '6 · 请求摘要',
  };

  function sectionLabel(section) {
    const name = String(section.name || '');
    const attempt = /^upstream_(\d+)_(request\.json|response\.txt|result\.json|error\.json|read_error\.json|body_state\.json|latency\.json)$/.exec(name);
    if (attempt) {
      const labels = { 'request.json': '请求', 'response.txt': '响应内容', 'result.json': 'HTTP 状态', 'error.json': '错误', 'read_error.json': '响应读取错误', 'latency.json': '延迟诊断', 'body_state.json':'响应读取完整性' };
      return '上游尝试 ' + Number(attempt[1]) + ' · ' + labels[attempt[2]];
    }
    return SECTION_LABELS[name] || (section.title || name || '记录');
  }

  // Journal labels. The server sends stable ids; the page shows what they mean.
  const KIND_LABELS = { request: '请求', operation: '操作', system: '系统', debug: '诊断', http: 'HTTP', probe: '探测', grok: 'Grok', workbuddy: 'WorkBuddy' };
  const ACTION_LABELS = {
    debug_bundle: '请求诊断包',
    http_request: '推理请求',
    // The synthetic probe written by the alert engine's own loop. Its channel is the
    // placeholder "probe" and its model is "__probe__": both are machine labels, so
    // the row must not print them as if they described a real request.
    channel_probe: '渠道探测',
    chat_request: '对话请求',
    grok_request: 'Grok 请求',
    grok_upstream_attempt: 'Grok 上游尝试',
    // The line the auth middleware writes for a request it refused: no handler ran,
    // so without this the row would print the raw action.
    gateway_rejected: '网关拒绝',
    gateway_error: '网关校验失败',
    config_update: '更新配置',
    image_generate: '生成图片',
    alert_fired: '告警触发',
    alert_recovered: '告警恢复',
  };

  // Operation actions are built from the request path (see operationAction in
  // middleware/admin_audit.go): "<resource>[.<id>].<verb>", e.g. accounts.120.create.
  // Printing that verbatim is why the operation log read as machine output.
  const OPERATION_VERBS = { create: '创建', update: '更新', delete: '删除', read: '查看', save: '保存', refresh: '刷新', sync: '同步', test: '测试', login: '登录', logout: '登出' };
  const OPERATION_RESOURCES = {
    accounts: '账号',
    models: '模型',
    keys: 'API Key',
    config: '配置',
    settings: '设置',
    session: '登录会话',
    'ops.alerts': '告警规则',
    'ops.alerts.rules': '告警规则',
    ops: '运维设置',
    providers: '渠道',
    journal: '日志',
    audit: '日志',
    'token-cache': 'Token 缓存',
    export: '导出',
    import: '导入',
    grok: 'Grok 账号',
    workbuddy: 'WorkBuddy 账号',
    'v1.admin': '管理接口',
    imagine: '图片生成',
  };

  // actionLabel turns a journal action into something readable. The raw id stays
  // available as a tooltip, because that is what a log grep uses.
  function actionLabel(action) {
    const raw = String(action || '').trim();
    if (!raw) return '—';
    if (ACTION_LABELS[raw]) return ACTION_LABELS[raw];

    const parts = raw.split('.').filter(Boolean);
    if (parts.length < 2) return raw;
    const verb = OPERATION_VERBS[parts[parts.length - 1].toLowerCase()];
    if (!verb) return raw;

    // Everything before the verb is the resource, with an optional object id in the
    // middle ("accounts.120.create") that the 对象 column already carries.
    const resourceParts = parts.slice(0, -1).filter((part) => !/^\d+$/.test(part));
    const resourceKey = resourceParts.join('.').toLowerCase();
    const resource = OPERATION_RESOURCES[resourceKey]
      || OPERATION_RESOURCES[resourceParts[0] ? resourceParts[0].toLowerCase() : '']
      || resourceParts.join(' ');
    if (!resource) return verb;
    // Chinese reads as one word (创建账号); a Latin resource keeps its space
    // (删除 API Key).
    return /^[A-Za-z]/.test(resource) ? verb + ' ' + resource : verb + resource;
  }

  // targetLabel names the object an operation touched: "accounts:120" is an account
  // row, not a machine string.
  function targetLabel(target, action) {
    const raw = String(target || '').trim();
    if (!raw) return '';
    const match = /^([a-z-]+):(\d+)$/i.exec(raw);
    if (!match) return raw;
    const resource = OPERATION_RESOURCES[match[1].toLowerCase()] || match[1];
    return resource + ' #' + match[2];
  }

  // PROBE_MODEL_LABEL is the placeholder model a synthetic probe carries.
  const PROBE_MODEL_LABEL = '__probe__';

  // isProbeRecord reports a synthetic probe: its channel is the reserved "probe"
  // label, which no client can route to.
  function isProbeRecord(event) { return String((event || {}).channel || '').toLowerCase() === 'probe'; }

  // Result semantics. One green pill for success, stop, length and tool_calls
  // made four different outcomes look identical; each one now has its own tone
  // and its own plain-language label.
  const STATUS_META = {
    success: { tone: 'is-ok', label: '成功' },
    ok: { tone: 'is-ok', label: '成功' },
    stop: { tone: 'is-ok', label: '正常结束' },
    recovered: { tone: 'is-ok', label: '已恢复' },
    firing: { tone: 'is-error', label: '告警中' },
    length: { tone: 'is-warn', label: '长度截断' },
    content_filter: { tone: 'is-warn', label: '内容过滤' },
    tool_calls: { tone: 'is-info', label: '工具调用' },
    stream_error: { tone: 'is-error', label: '流中断' },
    error: { tone: 'is-error', label: '失败' },
    '4xx': { tone: 'is-error', label: '客户端错误' },
    '5xx': { tone: 'is-error', label: '上游错误' },
    running: { tone: 'is-idle', label: '进行中' },
    skipped: { tone: 'is-idle', label: '已跳过' },
  };

  const FILTER_HINTS = { request: '请求日志按渠道、模型和状态筛选；带"含诊断"的请求可以在详情里展开它的诊断内容。操作者与动作对请求无意义。', operation: '操作日志按操作者、动作和状态筛选；渠道与模型通常为空。', system: '系统日志按动作和状态筛选；渠道用于标注探测目标。' };

  // The diagnostics journal used to be a fourth tab. Its rows carry only
  // action=debug_bundle and a request id, so every line read 诊断 · 请求诊断包 with
  // no channel or model — it was an index of bundles that the request rows already
  // link to. The index is still written (that is what marks a request as having a
  // bundle), but it is no longer a list of its own.
  const DIAGNOSTIC_INDEX_KIND = 'debug';



  function text(value, fallback) {
    if (value === undefined || value === null || value === '') return fallback || '—';
    return String(value);
  }

  // --- element builders ------------------------------------------------------
  // The detail panel and the row list are built from the same three statements
  // (create, class, text) over and over, so those live here once: a class name is
  // written once and the render functions read as WHAT they draw.




  // note is the muted one-line paragraph used for every "no data", "why this is
  // empty" and "this read failed" message in the page.
  function note(value, className) { return make('p', className || 'ops-empty', value); }

  // emptyRow is the full-width "nothing matched" row. colSpan counts the columns
  // of the table it lands in, so it is passed rather than assumed.
  function emptyRow(body, colSpan, message) {
    const td = make('td', 'table-empty-cell', message);
    td.colSpan = colSpan;
    body.replaceChildren();
    body.appendChild(attach(make('tr'), [td]));
  }

  function formatTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
  }

  function formatBytes(bytes) {
    const value = Number(bytes) || 0;
    if (value >= 1024 * 1024) return (value / (1024 * 1024)).toFixed(1) + ' MiB';
    if (value >= 1024) return (value / 1024).toFixed(1) + ' KiB';
    return value + ' B';
  }

  function statusMeta(status) {
    const normalized = String(status || '').toLowerCase();
    return STATUS_META[normalized] || { tone: 'is-idle', label: normalized || '—' };
  }

  // OUTCOME_META renders the server's result class, which is the same value the
  // drill-down filtered on. A request row therefore cannot read 正常结束 inside a
  // list that was filtered to failures.
  const OUTCOME_META = {
    success: { tone: 'is-ok', label: '成功' },
    rate_limited: { tone: 'is-info', label: '限流' },
    client_error: { tone: 'is-error', label: '客户端错误' },
    server_error: { tone: 'is-error', label: '上游错误' },
    stream_error: { tone: 'is-error', label: '流中断' },
    // Our gate refusing the caller is a warning about the caller; the provider
    // refusing our credential is an error about the channel.
    upstream_auth: { tone: 'is-error', label: '上游认证失败' },
    rejected: { tone: 'is-warn', label: '网关拒绝' },
    quota_exhausted: { tone: 'is-info', label: '额度用尽' },
  };

  function outcomeMeta(record) {
    const event = (record && record.event) || {};
    const class_name = record && record.outcome_class;
    if (!class_name) return null;
    const meta = OUTCOME_META[class_name] || { tone: 'is-idle', label: String(record.outcome_label || class_name) };
    return { ...meta, raw: String(event.status || '') };
  }

  function badge(meta, title) {
    const span = make('span', 'logs-badge ' + meta.tone, meta.label);
    span.title = title;
    return span;
  }

  // addBadge appends an untinted label chip. The badge is the same element the
  // status column uses; only its tone is absent.
  function addBadge(parent, label, tone) { parent.appendChild(make('span', 'logs-badge ' + (tone || ''), label)); }

  function resultBadge(record) {
    const outcome = outcomeMeta(record);
    if (outcome) return badge(outcome, outcome.raw ? outcome.label + '（上游返回 ' + outcome.raw + '）' : outcome.label);
    const status = (record.event || {}).status;
    return badge(statusMeta(status), text(status, '—'));
  }

  function attemptFailed(attempt) {
    const status = String(attempt.status || '').toLowerCase();
    return status !== 'success' && status !== 'ok';
  }

  function filters() {
    const params = new URLSearchParams();
    FILTER_INPUTS.forEach((entry) => {
      const node = el(entry.id);
      const value = node && node.value ? node.value.trim() : '';
      if (value) params.set(entry.param, value);
    });
    return params;
  }

  // readUrlScope applies the filters a drill-down (or a bookmark) arrived with,
  // and remembers where "返回" goes.
  function readUrlScope() {
    const params = new URLSearchParams(window.location.search);
    let applied = 0;
    FILTER_INPUTS.forEach((entry) => {
      const node = el(entry.id);
      if (!node) return;
      const value = params.get(entry.param) || '';
      if (value) applied += 1;
      node.value = value;
    });
    const kind = params.get('kind');
    if (kind && kind !== DIAGNOSTIC_INDEX_KIND) state.kind = kind;
    // A link that still asks for the diagnostics list lands on the request log,
    // where the same bundles are reachable from the requests that produced them.
    // The tab is gone; the addresses that pointed at it keep working.
    state.back = params.get('back') || '';
    return applied;
  }

  // syncUrl keeps the address bar equal to the filters in force. Without it a
  // refresh would silently widen the list back to "everything retained", which is
  // exactly the mismatch the drill-down exists to avoid.
  function syncUrl() {
    if (!window.history || !window.history.replaceState) return;
    const params = filters();
    params.set('tab', 'logs');
    params.set('kind', state.kind);
    if (state.back) params.set('back', state.back);
    window.history.replaceState(null, '', window.location.pathname + '?' + params.toString());
  }

  function renderScope() {
    const bar = el('logsScope');
    const items = el('logsScopeItems');
    const back = el('logsScopeBack');
    if (!bar || !items) return;
    const params = filters();
    // The time range is a drill-down's most important filter and has no obvious
    // place in a form full of text inputs, so it is always stated explicitly.
    const chips = [];
    const since = params.get('since');
    const until = params.get('until');
    if (since || until) { chips.push(scopeChip('时间范围', `${since ? formatTime(since) : '不限'} → ${until ? formatTime(until) : '现在'}`)); }
    FILTER_INPUTS.filter((entry) => entry.param !== 'since' && entry.param !== 'until').forEach((entry) => {
      const value = params.get(entry.param);
      if (!value) return;
      // The chip names the filter in Chinese: a raw query parameter is an
      // implementation detail, not something an operator reads.
      chips.push(scopeChip(FILTER_LABELS[entry.param] || entry.param,
        entry.param === 'outcome' ? (RESULT_LABELS[value] || value) : value));
    });
    items.replaceChildren(...chips);
    bar.hidden = chips.length === 0;
    if (back) back.hidden = !state.back;
  }

  // scopeChip is one label-over-value block in the scope bar.
  function scopeChip(label, value) {
    return attach(make('span', 'logs-scope-chip'), [
      make('span', 'logs-scope-key', label),
      make('span', '', value),
    ]);
  }

  function renderRows(append) {
    const body = el('logsRows');
    if (!body) return;
    if (!append) body.replaceChildren();
    if (state.records.length === 0 && !append) {
      // An empty page and a broken page look alike, so say which one this is and
      // what the window actually covers.
      emptyRow(body, 5, state.kind === 'request'
        ? '保留窗口内没有匹配的请求。带"含诊断"的行可以在详情里展开该请求的诊断内容。'
        : '保留窗口内没有匹配的记录。');
      return;
    }
    if (append) {
      // The "no matching records" placeholder is a row but carries no record. Counting
      // it as one made the FIRST record of the appended page look like an extra row, so
      // it was never drawn: "加载更多" returned data and the list stayed empty.
      body.querySelectorAll('tr').forEach((tr) => {
        if (!tr.dataset || tr.dataset.index === undefined) tr.remove();
      });
    }
    const start = append ? body.querySelectorAll('tr[data-index]').length : 0;
    const rows = state.records.slice(start).map((record, index) => {
      const event = record.event || {};
      const tr = make('tr');
      tr.dataset.index = String(start + index);
      const probe = isProbeRecord(event);
      const channelLabel = KIND_LABELS[event.channel] || event.channel || '';
      const label = text(ACTION_LABELS[event.action] || actionLabel(event.action), '—');
      // An operation has no channel: printing the journal's kind there made every
      // row of the operation log read "操作 · <machine id>".
      const action = make('td', 'logs-cell-action', channelLabel ? channelLabel + ' · ' + label : label);
      action.title = String(event.action || '');
      if (probe) {
        // A probe's model is the placeholder "__probe__" and its own channel is the
        // reserved "probe" label; what an operator needs to see is WHICH channel was
        // probed, so the badge carries the provider instead of the placeholder.
        const target = make('span', 'logs-badge', text(event.provider || '未标注渠道', '未标注渠道'));
        target.title = '被探测渠道（合成探测请求，模型占位为 ' + PROBE_MODEL_LABEL + '）';
        action.appendChild(target);
      } else if (event.model) { addBadge(action, event.model); }
      if (record.diagnostics || String(event.kind || '') === DIAGNOSTIC_INDEX_KIND) { addBadge(action, '含诊断', 'is-info'); }
      // "accounts:120" is an account row; an operation's target is read far more
      // often than it is grepped, so it is named.
      const target = make('td', 'logs-cell-target', targetLabel(event.target, event.action) || text(event.account_id, '—'));
      if (event.target) target.title = event.target;
      const status = make('td', 'logs-cell-status');
      status.appendChild(resultBadge(record));
      attach(tr, [
        make('td', 'logs-cell-time', formatTime(event.timestamp)),
        action,
        target,
        status,
        make('td', 'logs-cell-duration', event.duration_ms ? event.duration_ms + ' ms' : '—'),
      ]);
      tr.addEventListener('click', () => select(start + index));
      return tr;
    });
    rows.forEach((row) => body.appendChild(row));
  }

  // A fact is a small label-over-value block: in a 560px drawer two columns of
  // facts fit where a definition list needed thirteen rows, and the fields an
  // operator actually came for stop being buried under empty ones.
  function fact(label, value, options) {
    const options_ = options || {};
    const detail = make('span', 'logs-fact-value' + (options_.empty ? ' is-empty' : ''), value);
    if (options_.title) detail.title = options_.title;
    return attach(make('div', 'logs-fact' + (options_.wide ? ' is-wide' : '')), [
      make('span', 'logs-fact-label', label),
      detail,
    ]);
  }

  function factsGrid(facts) {
    const grid = make('div', 'logs-facts');
    (facts || []).forEach((node) => grid.appendChild(node));
    return grid;
  }

  function sectionTitle(text_) { return make('div', 'logs-detail-section-title', text_); }

  function detailAction(label, onClick, titleText) {
    const button = make('button', 'btn btn-sm', label);
    button.type = 'button';
    if (titleText) button.title = titleText;
    button.addEventListener('click', onClick);
    return button;
  }

  // summarizeRecord is the one-line description of a log entry: the first thing an
  // operator pastes into a ticket, so the copy button has to produce it verbatim.
  function summarizeRecord(record) {
    const event = (record && record.event) || {};
    const outcome = outcomeMeta(record);
    const bits = [
      formatTime(event.timestamp),
      text(KIND_LABELS[event.kind] || event.kind, '?'),
      text(ACTION_LABELS[event.action] || actionLabel(event.action), event.action || '?'),
    ];
    if (isProbeRecord(event)) {
      bits.push('被探测渠道 ' + text(event.provider, '未标注'));
    } else {
      if (event.channel) bits.push('渠道 ' + event.channel);
      if (event.model) bits.push('模型 ' + event.model);
    }
    bits.push('结果 ' + (outcome ? outcome.label : text(event.status, '—')));
    if (event.duration_ms) bits.push(event.duration_ms + ' ms');
    if (event.request_id) bits.push('请求 ID ' + event.request_id);
    if (event.error) bits.push('错误 ' + event.error);
    return bits.join(' · ');
  }

  function renderDetail(record) {
    const panel = el('logsDetailBody');
    const title = el('logsDetailTitle');
    const sub = el('logsDetailSub');
    if (!panel) return;
    panel.replaceChildren();
    if (!record) {
      if (title) title.textContent = '详情';
      if (sub) sub.textContent = '选择左侧一条记录查看详情';
      panel.appendChild(note('选择左侧一条记录查看详情。'));
      return;
    }
    const event = record.event || {};
    const probe = isProbeRecord(event);
    const outcome = outcomeMeta(record);
    if (title) title.textContent = text(ACTION_LABELS[event.action] || actionLabel(event.action), '详情');
    if (sub) sub.textContent = formatTime(event.timestamp) + ' · ' + text(event.request_id, '无请求 ID');

    // --- outcome first: a failing request is the usual reason this panel is open ---
    const outcomeRow = make('div', 'logs-detail-outcome');
    outcomeRow.appendChild(resultBadge(record));
    // The journal's own status is worth printing when it says something the class
    // does not (an upstream finish reason like "length"). For records written with
    // the class in that field it would just repeat the badge in another language,
    // and an operation's ok/error has nothing to do with an upstream.
    const rawStatus = String(event.status || '');
    const classToken = String(record.outcome_class || '');
    const isInferenceRecord = String(event.kind || '') === 'request';
    if (isInferenceRecord && rawStatus && rawStatus !== classToken && rawStatus !== outcome?.label) { outcomeRow.appendChild(make('span', 'logs-detail-raw', '上游状态：' + rawStatus)); }
    // The HTTP status and the first-token latency are not columns of a journal row:
    // the request middleware keeps them in metadata (http_status, first_token_ms),
    // which is where the attempt list below reads them from too. Reading only the
    // top-level fields meant a recorded 429 and a measured 135 ms were both dropped.
    const meta = event.metadata || {};
    const httpStatus = event.http_status || meta.http_status;
    if (httpStatus) outcomeRow.appendChild(make('span', 'logs-detail-raw', 'HTTP ' + httpStatus));
    panel.appendChild(outcomeRow);

    if (event.error) {
      panel.appendChild(sectionTitle('失败原因'));
      panel.appendChild(make('pre', 'logs-detail-error', event.error));
    }

    // --- actions: what you do next with a record you are looking at ---------------
    const actions = make('div', 'logs-detail-actions');
    if (event.request_id) { actions.appendChild(detailAction('复制请求 ID', () => copyToClipboard(event.request_id), event.request_id)); }
    actions.appendChild(detailAction('复制摘要', () => copyToClipboard(summarizeRecord(record)), summarizeRecord(record)));
    if (!probe && (event.channel || event.model)) {
      // Narrowing the list to this channel/model is the next question after "what
      // happened here" — answer it in place instead of retyping the filters.
      actions.appendChild(detailAction('只看这个渠道/模型', () => applyRecordScope(event), '用该记录的渠道与模型筛选列表'));
    }
    if (actions.childElementCount) panel.appendChild(actions);

    // --- request facts ------------------------------------------------------------
    // Which facts matter depends on the journal: 渠道 / 模型 / Token belong to
    // inference traffic, while an operation is described by what it did and to
    // which object. Printing 模型 未标注 for an account creation is noise.
    const isRequest = String(event.kind || '') === 'request';
    panel.appendChild(sectionTitle(probe ? '探测目标' : (isRequest ? '请求' : '记录')));
    const requestFacts = [];
    if (probe) {
      requestFacts.push(fact('记录渠道', 'probe（合成探测）', { title: '合成流量：不代表某个渠道的用户请求' }));
      requestFacts.push(fact('被探测渠道', text(event.provider, '未标注渠道')));
      requestFacts.push(fact('模型', '探测请求不携带真实模型', { empty: true }));
    } else if (isRequest) {
      requestFacts.push(fact('渠道', text(event.channel, '未标注'), { empty: !event.channel }));
      requestFacts.push(fact('模型', text(event.model, '未标注'), { empty: !event.model }));
    } else {
      requestFacts.push(fact('动作', text(ACTION_LABELS[event.action] || actionLabel(event.action), '—'), { wide: true, title: String(event.action || '') }));
      const objectName = targetLabel(event.target, event.action);
      if (objectName || event.account_id) { requestFacts.push(fact('对象', objectName || ('账号 #' + event.account_id), { title: String(event.target || '') })); }
    }
    // The 对象 row already names the row this operation touched; a second 账号
    // 未指定账号 next to it only contradicts it.
    if (event.account_id) {
      requestFacts.push(fact('账号', String(event.account_id)));
    } else if (isRequest) {
      requestFacts.push(fact('账号', '未指定账号', { empty: true }));
    }
    requestFacts.push(fact('记录类型', text(KIND_LABELS[event.kind] || event.kind, '—')));
    if (event.request_id) requestFacts.push(fact('请求 ID', event.request_id, { title: event.request_id }));
    if (event.actor) requestFacts.push(fact('操作者', event.actor));
    if (event.client_ip) requestFacts.push(fact('来源 IP', event.client_ip));
    panel.appendChild(factsGrid(requestFacts));

    // --- usage and latency --------------------------------------------------------
    const usageFacts = [];
    if (event.duration_ms) usageFacts.push(fact('总耗时', event.duration_ms + ' ms'));
    // Same metadata contract as http_status above: the measured first-token latency
    // lives in metadata, so the row has to look there or the figure is never shown.
    const firstTokenMS = event.first_token_ms || meta.first_token_ms;
    if (firstTokenMS != null) usageFacts.push(fact(meta.first_token_kind === 'body_ttfb' ? '非流式 TTFB' : '首生成（含推理）', firstTokenMS + ' ms'));
    if (meta.first_visible_token_ms != null) usageFacts.push(fact('首字', meta.first_visible_token_ms + ' ms'));
    if (meta.finish_reason) usageFacts.push(fact('结束原因', meta.finish_reason));
    if (meta.output_truncated) usageFacts.push(fact('输出预算', '耗尽 / 输出截断'));
    if (meta.reasoning_only) usageFacts.push(fact('可见答案', '仅推理输出'));
    if (meta.retry_wait_ms != null) usageFacts.push(fact('重试等待', meta.retry_wait_ms + ' ms'));
    if (meta.queue_wait_ms != null) usageFacts.push(fact('上游排队等待', meta.queue_wait_ms + ' ms'));
    if (meta.upstream_attempts != null) usageFacts.push(fact('上游尝试', String(meta.upstream_attempts)));
    if (meta.upstream_http_phase_samples > 0) {
      const labels = { dns_ms: '上游 DNS', tcp_ms: '上游 TCP', tls_ms: '上游 TLS', connection_wait_ms: '上游取连接', response_headers_ms: '上游响应头' };
      Object.entries(labels).forEach(([key,label]) => {
        if (meta.upstream_http_phases_sum?.[key] != null) usageFacts.push(fact(label + '（尝试累计）', meta.upstream_http_phases_sum[key] + ' ms'));
      });
    }
    const reportedTokens = (event.input_tokens || 0) + (event.output_tokens || 0);
    if (reportedTokens > 0) {
      usageFacts.push(fact('Token', (event.input_tokens || 0) + ' in / ' + (event.output_tokens || 0) + ' out'));
    } else if (isRequest && !probe) {
      // Zero tokens and "this channel does not report usage" are different facts;
      // the row used to print "0 in / 0 out" for both. An operation has no tokens
      // at all, so it says nothing rather than something misleading.
      usageFacts.push(fact('Token', '该请求未上报用量', { empty: true }));
    }
    if (usageFacts.length) {
      panel.appendChild(sectionTitle('用量与耗时'));
      panel.appendChild(factsGrid(usageFacts));
    }

    if (event.details) {
      panel.appendChild(sectionTitle(String(event.kind || '') === DIAGNOSTIC_INDEX_KIND ? '诊断内容摘要' : '变更摘要（凭据已脱敏）'));
      panel.appendChild(make('pre', '', event.details));
      if (event.redacted && event.redacted.length) { panel.appendChild(note('已脱敏字段：' + event.redacted.join('、'))); }
    }

    const attempts = record.attempts || [];
    if (attempts.length) {
      const failures = attempts.filter(attemptFailed).length;
      panel.appendChild(sectionTitle(`上游尝试（${attempts.length} 次${failures ? '，其中失败 ' + failures + ' 次' : ''}）`));
      const ul = make('ul', 'logs-attempts');
      attempts.forEach((attempt) => {
        const li = make('li', 'logs-attempt' + (attemptFailed(attempt) ? ' is-failed' : ''));
        const head = make('div', '',
          `第 ${text(attempt.attempt, '?')} 次 · ${text(attempt.provider, '—')} · ${attempt.duration_ms || 0} ms`);
        head.appendChild(badge(statusMeta(attempt.status), text(attempt.status, '—')));
        const detail = attempt.metadata || {};
        const upstream = detail.upstream_url || '';
        const attemptHTTP = detail.http_status || '';
        li.appendChild(head);
        li.appendChild(note([upstream, attemptHTTP ? 'HTTP ' + attemptHTTP : ''].filter(Boolean).join(' · ')));
        // The upstream's own reason. Without it a refused attempt reads "限流 · HTTP 429",
        // which cannot be told apart from a spent daily quota, a challenge, or a real rate
        // limit — the difference the operator needs to act on.
        const upstreamError = detail.response_error || null;
        if (upstreamError) {
          const code = text(upstreamError.code || upstreamError.type, '');
          const message = text(upstreamError.message, '');
          const reason = [code, message].filter(Boolean).join(' — ');
          if (reason) {
            const node = make('div', 'logs-attempt-reason', reason);
            node.title = reason;
            li.appendChild(node);
          }
        }
        ul.appendChild(li);
      });
      panel.appendChild(ul);
    }

    renderDiagnosticsSection(panel, record);
  }

  // applyRecordScope narrows the list to the record's channel and model, which is
  // the question a detail panel usually raises.
  function applyRecordScope(event) {
    const channel = el('filterChannel');
    const model = el('filterModel');
    if (channel) channel.value = String(event.channel || '');
    if (model) model.value = String(event.model || '');
    state.cursor = '';
    syncUrl();
    load(false);
  }

  // renderDiagnosticsSection shows the request's captured chain. The bundle is a
  // separate record (fetched by request id) because the journal entry only indexes
  // it: a request's worth of SSE must not sit in the journal stream.
  function renderDiagnosticsSection(panel, record) {
    const event = record.event || {};
    const index = record.diagnostics;
    const isDiagnosticEntry = String(event.kind || '') === DIAGNOSTIC_INDEX_KIND;
    if (!isDiagnosticEntry && !index) return;

    const container = make('div', 'logs-diagnostics');
    container.appendChild(make('div', '', '请求诊断日志'));

    const requestID = text(event.request_id, '');
    if (index && index.metadata) {
      const indexMeta = index.metadata;
      const parts = [];
      if (Array.isArray(indexMeta.sections) && indexMeta.sections.length) { parts.push(indexMeta.sections.length + ' 段'); }
      if (indexMeta.bytes) parts.push(formatBytes(indexMeta.bytes));
      if (indexMeta.truncated) parts.push('已截断');
      if (indexMeta.retention) parts.push('保留 ' + indexMeta.retention);
      container.appendChild(note(parts.join(' · ')));
    }

    if (!requestID) {
      container.appendChild(note('该记录没有请求 ID，无法定位诊断内容。'));
      panel.appendChild(container);
      return;
    }

    diagnosticDownloadButton(container, '下载完整诊断', async () => {
      const payload = await readDiagnostics(requestID);
      if (!payload.available) throw new Error(payload.note || '没有该请求的诊断记录。');
      downloadDiagnosticText('diagnostics-' + requestID + '.json', JSON.stringify(payload.entry, null, 2), 'application/json;charset=utf-8');
    });
    const body = make('div', 'logs-diagnostic-body');
    const button = make('button', 'btn', '展开诊断内容');
    button.type = 'button';
    button.addEventListener('click', () => loadDiagnostics(requestID, body, button));
    container.appendChild(button);
    container.appendChild(body);
    panel.appendChild(container);
  }

  async function readDiagnostics(requestID) {
    const payload = await ConsoleAPI.json('/api/journal/diagnostics?request_id=' + encodeURIComponent(requestID), {
      credentials: 'same-origin',
    });
    if (!payload || typeof payload.available !== 'boolean' || (payload.available && (!payload.entry || typeof payload.entry !== 'object' || Array.isArray(payload.entry)))) {
      throw new Error('诊断接口返回格式无效');
    }
    return payload;
  }

  function downloadDiagnosticText(name, content, type = 'text/plain;charset=utf-8') {
    const url = URL.createObjectURL(new Blob([content], { type }));
    const link = make('a');
    link.href = url;
    link.download = name.replace(/[\\/:*?"<>|\x00-\x1f]/g, '_');
    link.hidden = true;
    try {
      document.body.appendChild(link);
      link.click();
    } finally {
      link.remove();
      // Keep the URL alive while the browser starts consuming the download.
      setTimeout(() => URL.revokeObjectURL(url), 30000);
    }
  }

  function diagnosticDownloadButton(container, label, download) {
    const button = make('button', 'btn', label);
    button.type = 'button';
    const errorNote = note('');
    errorNote.hidden = true;
    button.addEventListener('click', async () => {
      if (button.disabled) return;
      button.disabled = true;
      button.textContent = '正在准备下载…';
      errorNote.hidden = true;
      errorNote.textContent = '';
      try { await download(); }
      catch (error) {
        errorNote.textContent = '下载诊断失败：' + (error.message || error);
        errorNote.hidden = false;
      } finally {
        button.disabled = false;
        button.textContent = label;
      }
    });
    container.appendChild(button);
    container.appendChild(errorNote);
  }

  async function loadDiagnostics(requestID, body, button) {
    if (button) {
      button.disabled = true;
      button.textContent = '正在读取…';
    }
    body.replaceChildren();
    try {
      const payload = await readDiagnostics(requestID);
      if (!payload.available) body.appendChild(note(payload.note || '没有该请求的诊断记录。'));
      else renderBundle(body, payload.entry || {}, payload.retention || '');
    } catch (error) { body.appendChild(note('读取诊断日志失败：' + (error.message || error))); }
    if (button) { button.disabled = false; button.textContent = '重新读取诊断内容'; }
  }

  function renderBundle(container, entry, retention) {
    const sections = Array.isArray(entry.sections) ? entry.sections : [];
    const parts = [`${sections.length} 段`, formatBytes(entry.bytes || 0)];
    if (entry.duration_ms) parts.push('请求耗时 ' + entry.duration_ms + ' ms');
    if (retention) parts.push('保留 ' + retention);
    if (entry.truncated) parts.push('已截断');
    container.appendChild(note(parts.join(' · ')));
    container.appendChild(note('统一链路：入口请求 → 上游调用与响应 → 客户端响应 → 请求结果。协议转换和渠道专属诊断作为补充记录；旧记录可能缺少阶段。'));
    diagnosticDownloadButton(container, '下载完整诊断', () => downloadDiagnosticText('diagnostics-' + (entry.request_id || 'request') + '.json', JSON.stringify(entry, null, 2), 'application/json;charset=utf-8'));

    if (entry.note) container.appendChild(note(entry.note));
    if (sections.length === 0) {
      container.appendChild(note('该请求没有捕获到内容。'));
      return;
    }

    sections.filter(section => /_latency\.json$/.test(section.name)).forEach(section => {
      try {
        const data = JSON.parse(section.payload);
        const panel = make('div', 'logs-section');
        panel.appendChild(make('strong', '', sectionLabel(section)));
        const labels = {
          account_id:'账号 ID', credential_refresh_expected:'凭据需要刷新', kind:'记录类型', provider:'渠道', model_key:'实际模型路由', model_source:'模型来源', host:'上游主机', proxy:'代理路径',
          resolved_ips:'DNS 解析地址', connected_address:'成功连接目标', remote_address:'实际对端', http_protocol:'HTTP 协议', alpn:'TLS ALPN',
          connection_reused:'复用连接', connection_was_idle:'取自空闲池', httpdns_ip:'HTTPDNS 请求头', body_bytes:'请求体字节',
          messages_count:'历史消息数', tools_count:'工具定义数', conversation_fingerprint:'会话指纹', protocol_profile:'协议配置', http_status:'HTTP 状态',
          before_upstream_ms:'请求进入至本次尝试（含此前等待）', connection_wait_ms:'获取连接（含建连）', dns_ms:'DNS', tls_ms:'TLS',
          request_written_ms:'请求写完', first_response_byte_ms:'首个响应字节', response_headers_ms:'响应头就绪', first_body_ms:'首个响应体字节', first_sse_ms:'首条 SSE',
          first_text_ms:'首个正文', first_reasoning_ms:'首个推理', first_tool_ms:'首个工具调用', total_ms:'本次耗时',
          access_ready_ms:'凭据准备就绪', runtime_ready_ms:'签名身份准备就绪', model_ready_ms:'模型路由就绪', body_ready_ms:'请求体就绪',
          upstream_firstTokenDuration:'上游报告首 Token', upstream_totalDuration:'上游报告总耗时', upstream_serverDuration:'上游报告服务耗时',
          reason:'等待原因', planned_wait_ms:'计划等待', actual_wait_ms:'实际等待', cancelled:'等待被取消', parameters:'生成参数', error:'失败原因'
        };
        Object.entries(data).forEach(([key,value]) => {
          const label = labels[key] || (key.startsWith('tcp:') ? 'TCP '+key.slice(4,-3) : key);
          let rendered = typeof value === 'boolean' ? (value ? '是' : '否') : typeof value === 'object' ? JSON.stringify(value) : String(value);
          if (value === '' || value == null) rendered = '未记录';
          if (key.endsWith('_ms') || key.startsWith('upstream_')) rendered += ' ms';
          panel.appendChild(make('div', '', label + '：' + rendered));
        });
        panel.appendChild(note('除 DNS/TCP/TLS/获取连接外，阶段时间从本次尝试开始累计；缺失表示未观测到。上游报告耗时与本地耗时口径不同。'));
        container.appendChild(panel);
      } catch (_) { /* Raw section below remains available for truncated data. */ }
    });
    const stage = name => name === '1_http_request.json' ? 0 : /^upstream_/.test(name) ? 1 : /^5_http_response/.test(name) ? 2 : /^6_http_summary/.test(name) ? 3 : 4;
    [...sections].sort((a,b) => stage(a.name)-stage(b.name)).forEach((section) => {
      const details = make('details', 'logs-section');
      // A failure artifact is what an operator came for; everything else starts
      // collapsed so a 16 KiB SSE dump does not bury it.
      if (section.name === '3_upstream_http_error.json' || section.name === '1_early_exit.json' || /^upstream_\d+_(error|read_error)\.json$/.test(section.name)) { details.open = true; }
      const summary = make('summary', '', sectionLabel(section));
      summary.appendChild(make('span', 'ops-empty',
        ' ' + formatBytes(section.bytes || 0) + (section.truncated ? ' · 已截断' : '')));
      details.appendChild(summary);
      const pre = make('pre', '', details.open ? (section.payload || '（空）') : '');
      details.appendChild(pre);
      details.addEventListener('toggle', () => { if (details.open && !pre.textContent) pre.textContent=section.payload || '（空）'; });
      diagnosticDownloadButton(details, '下载本段完整内容', () => downloadDiagnosticText(section.name || 'diagnostic.txt', section.payload || ''));
      container.appendChild(details);
    });
  }

  function scopeNote() {
    const hint = el('logsFilterScope');
    if (hint) hint.textContent = FILTER_HINTS[state.kind] || '';
    // The second column holds a channel only for inference traffic; an operation has
    // none, so the header says what the column really contains.
    const header = el('logsActionHeader');
    if (header) header.textContent = state.kind === 'operation' ? '动作' : '渠道 / 动作';
  }

  function openDrawer() {
    const drawer = el('logsDetail');
    const backdrop = el('logsDetailBackdrop');
    if (drawer) {
      drawer.classList.add('is-open');
      drawer.removeAttribute('aria-hidden');
    }
    if (backdrop) backdrop.classList.add('is-open');
    // On a phone the panel covers the list, so the page behind it must not scroll
    // under the finger reading the detail.
    document.body.classList.add('drawer-open');
  }

  function closeDrawer() {
    const drawer = el('logsDetail');
    const backdrop = el('logsDetailBackdrop');
    if (drawer) {
      drawer.classList.remove('is-open');
      // Kept out of the accessibility tree while it is off-screen.
      drawer.setAttribute('aria-hidden', 'true');
    }
    if (backdrop) backdrop.classList.remove('is-open');
    document.body.classList.remove('drawer-open');
  }

  function select(index) {
    const record = state.records[index];
    if (!record) return;
    state.selected = index;
    document.querySelectorAll('#logsRows tr').forEach((tr) => {
      tr.classList.toggle('is-selected', Number(tr.dataset.index) === index);
    });
    renderDetail(record);
    openDrawer();
  }

  async function load(append) {
    const params = filters();
    params.set('kind', state.kind);
    params.set('limit', '50');
    if (append && state.cursor) params.set('before', state.cursor);
    // This load owns the list from here on. Anything still in flight answers an older
    // question (a previous tab, or a previous filter) and is dropped when it lands.
    const seq = ++state.loadSeq;
    try {
      const response = await ConsoleAPI.request('/api/journal/records?' + params.toString(), { credentials: 'same-origin' });
      if (seq !== state.loadSeq) return;
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      if (seq !== state.loadSeq) return;
      state.cursor = payload.next_cursor || '';
      state.records = append ? state.records.concat(payload.data || []) : payload.data || [];
      renderRows(append);
      renderScope();
      const filterUsed = payload.filter_used || {};
      const matched = payload.matched;
      const scanned = payload.scanned;
      const coverage = el('logsCoverage');
      if (coverage) {
        const info = payload.coverage || {};
        // The counts are stated so a drill-down can be checked against the chart:
        // "匹配 N 条 / 扫描 M 条" is what makes an empty page trustworthy.
        const parts = [];
        if (typeof matched === 'number') parts.push(`本次匹配 ${matched} 条`);
        if (typeof scanned === 'number') parts.push(`扫描 ${scanned} 条`);
        parts.push(`保留 ${info.entries || 0} 条审计记录`);
        if (info.oldest) parts.push(`最早 ${info.oldest}`);
        if (info.newest) parts.push(`最新 ${info.newest}`);
        // A window that starts before the oldest retained record cannot be listed in
        // full, and a short list would otherwise read as "there were no failures".
        // The chart above it still has the aggregates, so the two are not comparable
        // for that part of the window.
        if (info.oldest && filterUsed.since) {
          const oldest = new Date(info.oldest);
          const from = new Date(filterUsed.since);
          if (!Number.isNaN(oldest.getTime()) && !Number.isNaN(from.getTime()) && from < oldest) { parts.push('⚠ 起始时间早于日志留存起点：列表中缺少更早的记录，只有图表统计包含那一段'); }
        }
        if (filterUsed.outcome_label) parts.push(`结果口径：${filterUsed.outcome_label}`);
        parts.push('为空的含义是"保留窗口内没有匹配"，不代表从未发生。');
        coverage.textContent = parts.join('；');
      }
      const more = el('logsMore');
      if (more) more.hidden = !state.cursor;
      if (!append) renderDetail(null);
    } catch (error) {
      // A failure that belongs to a superseded load must not replace the newer list
      // with an error row either.
      if (seq !== state.loadSeq) return;
      const body = el('logsRows');
      if (body) {
        body.replaceChildren();
        const tr = document.createElement('tr');
        const td = document.createElement('td');
        td.colSpan = 5;
        td.className = 'table-empty-cell';
        td.textContent = '读取日志失败：' + (error.message || error);
        tr.appendChild(td);
        body.appendChild(tr);
      }
    }
  }

  function toggleFilters(force) {
    const form = el('logsFilters');
    const button = el('logsFiltersToggle');
    if (!form) return;
    const open = force === undefined ? form.hidden : Boolean(force);
    form.hidden = !open;
    if (button) {
      button.setAttribute('aria-expanded', open ? 'true' : 'false');
      button.classList.toggle('is-active', open);
      button.textContent = open ? '收起筛选' : '筛选';
    }
  }

  function clearFilters() {
    FILTER_INPUTS.forEach((entry) => {
      const node = el(entry.id);
      if (node) node.value = '';
    });
    state.cursor = '';
    syncUrl();
    load(false);
  }

  function bind() {
    const applied = readUrlScope();
    // A drill-down arrives with filters in force: show the form open so the scope
    // is editable, and show the scope bar so it is visible.
    toggleFilters(applied > 0);
    scopeNote();
    document.querySelectorAll('.logs-tab').forEach((tab) => {
      tab.addEventListener('click', () => {
        document.querySelectorAll('.logs-tab').forEach((other) => {
          other.classList.remove('is-active');
          other.setAttribute('aria-selected', 'false');
        });
        tab.classList.add('is-active');
        tab.setAttribute('aria-selected', 'true');
        state.kind = tab.getAttribute('data-kind') || 'request';
        state.cursor = '';
        scopeNote();
        closeDrawer();
        syncUrl();
        load(false);
      });
    });
    const toggle = el('logsFiltersToggle');
    if (toggle) toggle.addEventListener('click', () => toggleFilters());
    const form = el('logsFilters');
    if (form) form.addEventListener('submit', (event) => { event.preventDefault(); state.cursor = ''; syncUrl(); load(false); });
    const reload = el('logsReload');
    if (reload) reload.addEventListener('click', () => { state.cursor = ''; load(false); });
    const clear = el('logsClear');
    if (clear) clear.addEventListener('click', clearFilters);
    const scopeClear = el('logsScopeClear');
    if (scopeClear) scopeClear.addEventListener('click', clearFilters);
    const scopeBack = el('logsScopeBack');
    if (scopeBack) {
      scopeBack.addEventListener('click', () => {
        // The overview's own scope travelled with the drill-down, so the way back
        // lands on the chart that was clicked, not on a default view.
        window.location.href = state.back || (window.location.pathname + '?tab=ops');
      });
    }
    const more = el('logsMore');
    if (more) more.addEventListener('click', () => load(true));
    const close = el('logsDetailClose');
    if (close) close.addEventListener('click', closeDrawer);
    const backdrop = el('logsDetailBackdrop');
    if (backdrop) backdrop.addEventListener('click', closeDrawer);
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') closeDrawer();
    });
  }

  async function initDiagnosticToggle() {
    const button = el('logsDiagnosticsToggle');
    if (!button) return;
    let enabled = null;
    async function update(save) {
      button.disabled = true;
      try {
        const response = await ConsoleAPI.request('/api/journal/diagnostics/settings', save ? {
          method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: !enabled }),
        } : { credentials: 'same-origin', cache: 'no-store' });
        if (!response.ok) throw new Error('HTTP ' + response.status);
        const payload = await response.json();
        if (typeof payload.enabled !== 'boolean') throw new Error('Invalid setting');
        enabled = payload.enabled;
        button.setAttribute('aria-pressed', String(enabled));
        button.textContent = enabled ? '诊断采集：已开启（点击关闭）' : '诊断采集：已关闭（点击开启）';
        button.title = '对新请求生效，诊断内容最多保留 24 小时、512 个请求';
      } catch (error) {
        button.textContent = enabled == null ? '诊断采集：读取失败，点击重试' : '诊断采集：保存失败，点击重试';
        button.title = error.message;
      } finally { button.disabled = false; }
    }
    button.addEventListener('click', () => update(enabled != null));
    await update(false);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { bind(); load(false); initDiagnosticToggle(); });
  } else {
    bind();
    load(false);
    initDiagnosticToggle();
  }
})();
