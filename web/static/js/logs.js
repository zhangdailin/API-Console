// Log centre: three journals (request / operation / system) sharing one
// collapsible filter bar and one drawer. Request details list every upstream
// attempt and offer the request's diagnostic bundle; change details show the
// redacted summary the server produced; a diagnostic entry renders the captured
// chain of the request — the per-request files that used to live under debug-logs/.
(function () {
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
  const FILTER_LABELS = {
    channel: '渠道',
    model: '模型',
    status: '状态',
    outcome: '结果',
    actor: '操作者',
    action: '动作',
  };

  // The capture records a request as a fixed sequence of numbered sections. The
  // order is the request's own timeline, so the panel must not sort it.
  const SECTION_LABELS = {
    '1_http_request.json': '1 · 客户端原始请求',
    '5_http_response.txt': '5 · 实际返回客户端内容',
    '6_http_summary.json': '6 · HTTP 完成状态',
    '6_request_events.jsonl': '6 · 上游尝试与请求事件',
    '1_claude_request.json': '1 · 客户端请求',
    '1_early_exit.json': '1 · 提前返回',
    '2_converted_prompt.md': '2 · 转换后提示词',
    '3_upstream_request.json': '3 · 上游请求',
    '3_upstream_http_error.json': '3 · 上游错误',
    '4_upstream_sse.jsonl': '4 · 上游响应（SSE / Protobuf 解码）',
    '5_client_sse.jsonl': '5 · 返回客户端 SSE',
    '6_input_token_breakdown.json': '6 · 输入 token 分解',
    '6_summary.json': '6 · 请求摘要',
  };

  function sectionLabel(section) {
    const name = String(section.name || '');
    const attempt = /^upstream_(\d+)_(request\.json|response\.txt|result\.json|error\.json|read_error\.json)$/.exec(name);
    if (attempt) {
      const labels = { 'request.json': '请求', 'response.txt': '响应内容', 'result.json': 'HTTP 状态', 'error.json': '错误', 'read_error.json': '响应读取错误' };
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
  const OPERATION_VERBS = {
    create: '创建',
    update: '更新',
    delete: '删除',
    read: '查看',
    save: '保存',
    refresh: '刷新',
    sync: '同步',
    test: '测试',
    login: '登录',
    logout: '登出',
  };
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
  function isProbeRecord(event) {
    return String((event || {}).channel || '').toLowerCase() === 'probe';
  }

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

  const FILTER_HINTS = {
    request: '请求日志按渠道、模型和状态筛选；带"含诊断"的请求可以在详情里展开它的诊断内容。操作者与动作对请求无意义。',
    operation: '操作日志按操作者、动作和状态筛选；渠道与模型通常为空。',
    system: '系统日志按动作和状态筛选；渠道用于标注探测目标。',
  };

  // The diagnostics journal used to be a fourth tab. Its rows carry only
  // action=debug_bundle and a request id, so every line read 诊断 · 请求诊断包 with
  // no channel or model — it was an index of bundles that the request rows already
  // link to. The index is still written (that is what marks a request as having a
  // bundle), but it is no longer a list of its own.
  const DIAGNOSTIC_INDEX_KIND = 'debug';

  function el(id) {
    return document.getElementById(id);
  }

  function text(value, fallback) {
    if (value === undefined || value === null || value === '') return fallback || '—';
    return String(value);
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
    const span = document.createElement('span');
    span.className = 'logs-badge ' + meta.tone;
    span.textContent = meta.label;
    span.title = title;
    return span;
  }

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
    const chips = [];
    const addChip = (label, value) => {
      const chip = document.createElement('span');
      chip.className = 'logs-scope-chip';
      const key = document.createElement('span');
      key.className = 'logs-scope-key';
      key.textContent = label;
      const val = document.createElement('span');
      val.textContent = value;
      chip.appendChild(key);
      chip.appendChild(val);
      chips.push(chip);
    };
    const since = params.get('since');
    const until = params.get('until');
    if (since || until) {
      // The time range is a drill-down's most important filter and has no obvious
      // place in a form full of text inputs, so it is always stated explicitly.
      addChip('时间范围', `${since ? formatTime(since) : '不限'} → ${until ? formatTime(until) : '现在'}`);
    }
    FILTER_INPUTS.filter((entry) => entry.param !== 'since' && entry.param !== 'until').forEach((entry) => {
      const value = params.get(entry.param);
      if (!value) return;
      // The chip names the filter in Chinese: a raw query parameter is an
      // implementation detail, not something an operator reads.
      addChip(FILTER_LABELS[entry.param] || entry.param, entry.param === 'outcome' ? (RESULT_LABELS[value] || value) : value);
    });
    items.replaceChildren();
    chips.forEach((chip) => items.appendChild(chip));
    bar.hidden = chips.length === 0;
    if (back) back.hidden = !state.back;
  }

  function renderRows(append) {
    const body = el('logsRows');
    if (!body) return;
    if (!append) body.replaceChildren();
    if (state.records.length === 0 && !append) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 5;
      td.className = 'table-empty-cell';
      // An empty page and a broken page look alike, so say which one this is and
      // what the window actually covers.
      td.textContent = state.kind === 'request'
        ? '保留窗口内没有匹配的请求。带"含诊断"的行可以在详情里展开该请求的诊断内容。'
        : '保留窗口内没有匹配的记录。';
      tr.appendChild(td);
      body.appendChild(tr);
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
    state.records.slice(start).forEach((record, index) => {
      const event = record.event || {};
      const tr = document.createElement('tr');
      tr.dataset.index = String(start + index);

      const time = document.createElement('td');
      time.className = 'logs-cell-time';
      time.textContent = formatTime(event.timestamp);
      tr.appendChild(time);

      const action = document.createElement('td');
      action.className = 'logs-cell-action';
      const probe = isProbeRecord(event);
      const channelLabel = KIND_LABELS[event.channel] || event.channel || '';
      const label = text(ACTION_LABELS[event.action] || actionLabel(event.action), '—');
      if (channelLabel) {
        action.textContent = channelLabel + ' · ' + label;
      } else {
        // An operation has no channel: printing the journal's kind there made every
        // row of the operation log read "操作 · <machine id>".
        action.textContent = label;
      }
      action.title = String(event.action || '');
      if (probe) {
        // A probe's model is the placeholder "__probe__" and its own channel is the
        // reserved "probe" label; what an operator needs to see is WHICH channel was
        // probed, so the badge carries the provider instead of the placeholder.
        const target = document.createElement('span');
        target.className = 'logs-badge';
        target.textContent = text(event.provider || '未标注渠道', '未标注渠道');
        target.title = '被探测渠道（合成探测请求，模型占位为 ' + PROBE_MODEL_LABEL + '）';
        action.appendChild(target);
      } else if (event.model) {
        const badge = document.createElement('span');
        badge.className = 'logs-badge';
        badge.textContent = event.model;
        action.appendChild(badge);
      }
      if (record.diagnostics || String(event.kind || '') === DIAGNOSTIC_INDEX_KIND) {
        const badge = document.createElement('span');
        badge.className = 'logs-badge is-info';
        badge.textContent = '含诊断';
        action.appendChild(badge);
      }
      tr.appendChild(action);

      const target = document.createElement('td');
      target.className = 'logs-cell-target';
      // "accounts:120" is an account row; an operation's target is read far more
      // often than it is grepped, so it is named.
      const targetText = targetLabel(event.target, event.action);
      target.textContent = targetText || text(event.account_id, '—');
      if (event.target) target.title = event.target;
      tr.appendChild(target);

      const status = document.createElement('td');
      status.className = 'logs-cell-status';
      status.appendChild(resultBadge(record));
      tr.appendChild(status);

      const duration = document.createElement('td');
      duration.className = 'logs-cell-duration';
      duration.textContent = event.duration_ms ? event.duration_ms + ' ms' : '—';
      tr.appendChild(duration);

      tr.addEventListener('click', () => select(start + index));
      body.appendChild(tr);
    });
  }

  // A fact is a small label-over-value block: in a 560px drawer two columns of
  // facts fit where a definition list needed thirteen rows, and the fields an
  // operator actually came for stop being buried under empty ones.
  function fact(label, value, options) {
    const wrap = document.createElement('div');
    wrap.className = 'logs-fact' + (options && options.wide ? ' is-wide' : '');
    const term = document.createElement('span');
    term.className = 'logs-fact-label';
    term.textContent = label;
    const detail = document.createElement('span');
    detail.className = 'logs-fact-value';
    if (options && options.empty) detail.classList.add('is-empty');
    detail.textContent = value;
    if (options && options.title) detail.title = options.title;
    wrap.appendChild(term);
    wrap.appendChild(detail);
    return wrap;
  }

  function factsGrid(facts) {
    const grid = document.createElement('div');
    grid.className = 'logs-facts';
    facts.forEach((node) => grid.appendChild(node));
    return grid;
  }

  function sectionTitle(text_) {
    const node = document.createElement('div');
    node.className = 'logs-detail-section-title';
    node.textContent = text_;
    return node;
  }

  function detailAction(label, onClick, titleText) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn btn-sm';
    button.textContent = label;
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
      const empty = document.createElement('p');
      empty.className = 'ops-empty';
      empty.textContent = '选择左侧一条记录查看详情。';
      panel.appendChild(empty);
      return;
    }
    const event = record.event || {};
    const probe = isProbeRecord(event);
    const outcome = outcomeMeta(record);
    if (title) title.textContent = text(ACTION_LABELS[event.action] || actionLabel(event.action), '详情');
    if (sub) sub.textContent = formatTime(event.timestamp) + ' · ' + text(event.request_id, '无请求 ID');

    // --- outcome first: a failing request is the usual reason this panel is open ---
    const outcomeRow = document.createElement('div');
    outcomeRow.className = 'logs-detail-outcome';
    outcomeRow.appendChild(resultBadge(record));
    // The journal's own status is worth printing when it says something the class
    // does not (an upstream finish reason like "length"). For records written with
    // the class in that field it would just repeat the badge in another language,
    // and an operation's ok/error has nothing to do with an upstream.
    const rawStatus = String(event.status || '');
    const classToken = String(record.outcome_class || '');
    const isInferenceRecord = String(event.kind || '') === 'request';
    if (isInferenceRecord && rawStatus && rawStatus !== classToken && rawStatus !== outcome?.label) {
      const raw = document.createElement('span');
      raw.className = 'logs-detail-raw';
      raw.textContent = '上游状态：' + rawStatus;
      outcomeRow.appendChild(raw);
    }
    // The HTTP status and the first-token latency are not columns of a journal row:
    // the request middleware keeps them in metadata (http_status, first_token_ms),
    // which is where the attempt list below reads them from too. Reading only the
    // top-level fields meant a recorded 429 and a measured 135 ms were both dropped.
    const meta = event.metadata || {};
    const httpStatus = event.http_status || meta.http_status;
    if (httpStatus) {
      const http = document.createElement('span');
      http.className = 'logs-detail-raw';
      http.textContent = 'HTTP ' + httpStatus;
      outcomeRow.appendChild(http);
    }
    panel.appendChild(outcomeRow);

    if (event.error) {
      panel.appendChild(sectionTitle('失败原因'));
      const pre = document.createElement('pre');
      pre.className = 'logs-detail-error';
      pre.textContent = event.error;
      panel.appendChild(pre);
    }

    // --- actions: what you do next with a record you are looking at ---------------
    const actions = document.createElement('div');
    actions.className = 'logs-detail-actions';
    if (event.request_id) {
      actions.appendChild(detailAction('复制请求 ID', () => copyToClipboard(event.request_id), event.request_id));
    }
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
      requestFacts.push(fact('动作', text(ACTION_LABELS[event.action] || actionLabel(event.action), '—'), {
        wide: true,
        title: String(event.action || ''),
      }));
      const objectName = targetLabel(event.target, event.action);
      if (objectName || event.account_id) {
        requestFacts.push(fact('对象', objectName || ('账号 #' + event.account_id), { title: String(event.target || '') }));
      }
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
    if (firstTokenMS) usageFacts.push(fact('首 Token', firstTokenMS + ' ms'));
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
      const pre = document.createElement('pre');
      pre.textContent = event.details;
      panel.appendChild(pre);
      if (event.redacted && event.redacted.length) {
        const maskNote = document.createElement('p');
        maskNote.className = 'ops-empty';
        maskNote.textContent = '已脱敏字段：' + event.redacted.join('、');
        panel.appendChild(maskNote);
      }
    }

    const attempts = record.attempts || [];
    if (attempts.length) {
      const failures = attempts.filter(attemptFailed).length;
      panel.appendChild(sectionTitle(`上游尝试（${attempts.length} 次${failures ? '，其中失败 ' + failures + ' 次' : ''}）`));
      const ul = document.createElement('ul');
      ul.className = 'logs-attempts';
      attempts.forEach((attempt) => {
        const li = document.createElement('li');
        li.className = 'logs-attempt';
        if (attemptFailed(attempt)) li.classList.add('is-failed');
        const head = document.createElement('div');
        head.textContent = `第 ${text(attempt.attempt, '?')} 次 · ${text(attempt.provider, '—')} · ${attempt.duration_ms || 0} ms`;
        head.appendChild(badge(statusMeta(attempt.status), text(attempt.status, '—')));
        li.appendChild(head);
        const meta = document.createElement('div');
        meta.className = 'ops-empty';
        const upstream = attempt.metadata && attempt.metadata.upstream_url ? attempt.metadata.upstream_url : '';
        const httpStatus = attempt.metadata && attempt.metadata.http_status ? attempt.metadata.http_status : '';
        meta.textContent = [upstream, httpStatus ? 'HTTP ' + httpStatus : ''].filter(Boolean).join(' · ');
        li.appendChild(meta);
        // The upstream's own reason. Without it a refused attempt reads "限流 · HTTP 429",
        // which cannot be told apart from a spent daily quota, a challenge, or a real rate
        // limit — the difference the operator needs to act on.
        const upstreamError = attempt.metadata && attempt.metadata.response_error ? attempt.metadata.response_error : null;
        if (upstreamError) {
          const reason = document.createElement('div');
          reason.className = 'logs-attempt-reason';
          const code = text(upstreamError.code || upstreamError.type, '');
          const message = text(upstreamError.message, '');
          reason.textContent = [code, message].filter(Boolean).join(' — ');
          reason.title = reason.textContent;
          if (reason.textContent) li.appendChild(reason);
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

    const container = document.createElement('div');
    container.className = 'logs-diagnostics';

    const title = document.createElement('div');
    title.textContent = '请求诊断日志';
    container.appendChild(title);

    const requestID = text(event.request_id, '');
    if (index && index.metadata) {
      const summary = document.createElement('p');
      summary.className = 'ops-empty';
      const parts = [];
      if (Array.isArray(index.metadata.sections) && index.metadata.sections.length) {
        parts.push(index.metadata.sections.length + ' 段');
      }
      if (index.metadata.bytes) parts.push(formatBytes(index.metadata.bytes));
      if (index.metadata.truncated) parts.push('已截断');
      if (index.metadata.retention) parts.push('保留 ' + index.metadata.retention);
      summary.textContent = parts.join(' · ');
      container.appendChild(summary);
    }

    if (!requestID) {
      const empty = document.createElement('p');
      empty.className = 'ops-empty';
      empty.textContent = '该记录没有请求 ID，无法定位诊断内容。';
      container.appendChild(empty);
      panel.appendChild(container);
      return;
    }

    const body = document.createElement('div');
    body.className = 'logs-diagnostic-body';
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn';
    button.textContent = '展开诊断内容';
    button.addEventListener('click', () => loadDiagnostics(requestID, body, button));
    container.appendChild(button);
    container.appendChild(body);
    panel.appendChild(container);
  }

  async function loadDiagnostics(requestID, body, button) {
    if (button) {
      button.disabled = true;
      button.textContent = '正在读取…';
    }
    body.replaceChildren();
    try {
      const response = await fetch('/api/journal/diagnostics?request_id=' + encodeURIComponent(requestID), {
        credentials: 'same-origin',
      });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      if (!payload.available) {
        const empty = document.createElement('p');
        empty.className = 'ops-empty';
        empty.textContent = payload.note || '没有该请求的诊断记录。';
        body.appendChild(empty);
      } else {
        renderBundle(body, payload.entry || {}, payload.retention || '');
      }
    } catch (error) {
      const failed = document.createElement('p');
      failed.className = 'ops-empty';
      failed.textContent = '读取诊断日志失败：' + (error.message || error);
      body.appendChild(failed);
    }
    if (button) { button.disabled = false; button.textContent = '重新读取诊断内容'; }
  }

  function renderBundle(container, entry, retention) {
    const sections = Array.isArray(entry.sections) ? entry.sections : [];
    const meta = document.createElement('p');
    meta.className = 'ops-empty';
    const parts = [`${sections.length} 段`, formatBytes(entry.bytes || 0)];
    if (entry.duration_ms) parts.push('请求耗时 ' + entry.duration_ms + ' ms');
    if (retention) parts.push('保留 ' + retention);
    if (entry.truncated) parts.push('已截断');
    meta.textContent = parts.join(' · ');
    container.appendChild(meta);

    if (entry.note) {
      const note = document.createElement('p');
      note.className = 'ops-empty';
      note.textContent = entry.note;
      container.appendChild(note);
    }
    if (sections.length === 0) {
      const empty = document.createElement('p');
      empty.className = 'ops-empty';
      empty.textContent = '该请求没有捕获到内容。';
      container.appendChild(empty);
      return;
    }

    sections.forEach((section) => {
      const details = document.createElement('details');
      details.className = 'logs-section';
      // A failure artifact is what an operator came for; everything else starts
      // collapsed so a 16 KiB SSE dump does not bury it.
      if (section.name === '3_upstream_http_error.json' || section.name === '1_early_exit.json' || /^upstream_\d+_(error|read_error)\.json$/.test(section.name)) {
        details.open = true;
      }
      const summary = document.createElement('summary');
      summary.textContent = sectionLabel(section);
      const size = document.createElement('span');
      size.className = 'ops-empty';
      size.textContent = ' ' + formatBytes(section.bytes || 0) + (section.truncated ? ' · 已截断' : '');
      summary.appendChild(size);
      details.appendChild(summary);

      const pre = document.createElement('pre');
      pre.textContent = section.payload || '（空）';
      details.appendChild(pre);
      container.appendChild(details);
    });
  }

  function scopeNote() {
    const note = el('logsFilterScope');
    if (note) note.textContent = FILTER_HINTS[state.kind] || '';
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
      const response = await fetch('/api/journal/records?' + params.toString(), { credentials: 'same-origin' });
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
          if (!Number.isNaN(oldest.getTime()) && !Number.isNaN(from.getTime()) && from < oldest) {
            parts.push('⚠ 起始时间早于日志留存起点：列表中缺少更早的记录，只有图表统计包含那一段');
          }
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
        const response = await fetch('/api/journal/diagnostics/settings', save ? {
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
      } finally {
        button.disabled = false;
      }
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
