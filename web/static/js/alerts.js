// Alert policy editor. It edits the same struct the evaluator reads, so what is
// shown here is the policy in force: no translation layer between the form and
// the engine, and the same endpoint the overview's alert counters come from.
(function () {
  const state = { rules: null, defaults: null, editable: true };

  // FIELDS describes the policy. Each entry states the unit, the range and why the
  // threshold exists, because a number without its reason is a number an operator
  // will guess at.
  const FIELDS = [
    {
      key: 'MinRequests',
      label: '最小请求数',
      hint: '窗口内至少要有这么多次真实请求，成功率规则才允许触发；防止空闲渠道被一次失败打到。',
      kind: 'int',
      min: 0,
    },
    {
      key: 'MinFailures',
      label: '最小失败数',
      hint: '警告级别还需要的失败次数；严重级别（低于严重阈值）不受此限制，因为几乎全部失败本身就是故障。',
      kind: 'int',
      min: 0,
    },
    {
      key: 'SuccessRateWarning',
      label: '告警成功率阈值',
      hint: '成功率低于这个百分比触发警告级别（例如 90%）。',
      kind: 'ratio',
    },
    {
      key: 'SuccessRateCritical',
      label: '严重成功率阈值',
      hint: '低于这个百分比直接判严重；必须低于告警阈值，否则两档描述同一件事。',
      kind: 'ratio',
    },
    {
      key: 'ClearMargin',
      label: '恢复余量',
      hint: '已告警的渠道要回到“阈值 + 余量”以上才判定恢复；这是迟滞带，防止在阈值线上反复告警与恢复。',
      kind: 'ratio',
    },
    {
      key: 'RequireNoAvailableAccounts',
      label: '检查账号池',
      hint: '渠道有启用账号但全部不可用时触发严重告警（冷却、需重新登录都算不可用）。',
      kind: 'bool',
    },
  ];

  function el(id) {
    return document.getElementById(id);
  }

  function fmtRatio(value) {
    return (Number(value || 0) * 100).toFixed(1) + '%';
  }

  // A ratio is stored as a fraction (0.9) and shown as a percentage (90%). The engine's
  // wire format does not change; only the form does, because "0.03 的恢复余量" is not
  // something an operator can read at a glance.
  function ratioToPercent(value) {
    const percent = Number(value) * 100;
    if (!Number.isFinite(percent)) return '';
    return String(Math.round(percent * 100) / 100);
  }

  function percentToRatio(value) {
    const percent = Number(value);
    return Number.isFinite(percent) ? percent / 100 : 0;
  }

  function setState(text, tone) {
    const node = el('alertsState');
    if (!node) return;
    node.textContent = text;
    node.className = 'ops-hint' + (tone ? ' ' + tone : '');
  }

  function renderFields() {
    const container = el('alertsFields');
    if (!container) return;
    container.replaceChildren();
    const rules = state.rules || {};
    FIELDS.forEach((field) => {
      const wrap = document.createElement('label');
      wrap.className = 'alerts-field';
      const label = document.createElement('span');
      label.className = 'alerts-field-label';
      label.textContent = field.label;
      wrap.appendChild(label);

      let input;
      if (field.kind === 'bool') {
        input = document.createElement('input');
        input.type = 'checkbox';
        input.checked = Boolean(rules[field.key]);
        input.className = 'alerts-checkbox';
      } else {
        input = document.createElement('input');
        input.type = 'number';
        input.className = 'form-input';
        input.step = '1';
        if (field.kind === 'ratio') {
          // Percent in the form, fraction on the wire.
          input.min = '0';
          input.max = '100';
          input.value = rules[field.key] === undefined ? '' : ratioToPercent(rules[field.key]);
        } else {
          input.min = String(field.min || 0);
          input.value = String(rules[field.key] === undefined ? '' : rules[field.key]);
        }
      }
      input.id = 'rule' + field.key;
      input.addEventListener('input', renderPreview);
      input.addEventListener('change', renderPreview);
      if (field.kind === 'ratio') {
        const control = document.createElement('span');
        control.className = 'alerts-field-control';
        control.appendChild(input);
        const unit = document.createElement('span');
        unit.className = 'alerts-field-unit';
        unit.textContent = '%';
        control.appendChild(unit);
        wrap.appendChild(control);
      } else {
        wrap.appendChild(input);
      }

      const hint = document.createElement('span');
      hint.className = 'alerts-field-hint';
      hint.textContent = field.hint;
      wrap.appendChild(hint);
      container.appendChild(wrap);
    });
  }

  // readForm turns the form back into the engine's own struct: percentages are stored
  // as the fractions the alert engine evaluates.
  function readForm() {
    const rules = {};
    FIELDS.forEach((field) => {
      const node = el('rule' + field.key);
      if (!node) return;
      if (field.kind === 'bool') {
        rules[field.key] = Boolean(node.checked);
        return;
      }
      if (field.kind === 'ratio') {
        rules[field.key] = percentToRatio(node.value);
        return;
      }
      const value = Number(node.value);
      rules[field.key] = Number.isFinite(value) ? value : 0;
    });
    return rules;
  }

  // validate mirrors the server's rules so the operator sees the problem before the
  // request is sent, not as a 400 after it. The wording is in the unit the form shows.
  function validate(rules) {
    if (rules.SuccessRateWarning <= 0 || rules.SuccessRateWarning > 1) return '告警成功率阈值必须在 0% 与 100% 之间。';
    if (rules.SuccessRateCritical <= 0 || rules.SuccessRateCritical > 1) return '严重成功率阈值必须在 0% 与 100% 之间。';
    if (rules.SuccessRateCritical >= rules.SuccessRateWarning) return '严重阈值必须低于告警阈值。';
    if (rules.SuccessRateWarning + rules.ClearMargin > 1) return '告警阈值与恢复余量之和不能超过 100%。';
    if (rules.ClearMargin < 0 || rules.ClearMargin > 0.5) return '恢复余量必须在 0% 与 50% 之间。';
    if (rules.MinRequests < 0 || rules.MinFailures < 0) return '计数类阈值不能为负。';
    return '';
  }

  function renderPreview() {
    const rules = readForm();
    const note = el('alertsNote');
    const preview = el('alertsPreview');
    const problem = validate(rules);
    if (note) {
      note.textContent = problem || '规则合法，保存后立即用于下一次评估。';
      note.classList.toggle('is-error-text', Boolean(problem));
    }
    if (!preview) return;
    preview.replaceChildren();

    // The plain-language reading of the thresholds: an operator should not have to
    // translate ratios into sentences to know what they just configured.
    const rows = [
      `请求数达到 ${rules.MinRequests} 次且失败 ${rules.MinFailures} 次以上时`,
      `成功率低于 ${fmtRatio(rules.SuccessRateWarning)} 触发警告，低于 ${fmtRatio(rules.SuccessRateCritical)} 触发严重`,
      `恢复线：成功率回到 ${fmtRatio(rules.SuccessRateWarning + rules.ClearMargin)} 以上`,
      rules.RequireNoAvailableAccounts ? '启用账号全部不可用时触发严重告警' : '不检查账号池可用性',
    ];
    rows.forEach((text) => {
      const item = document.createElement('p');
      item.className = 'alerts-preview-line';
      item.textContent = text;
      preview.appendChild(item);
    });
  }

  function renderHelp() {
    const help = el('alertsHelp');
    if (!help) return;
    const items = [
      ['判定依据', '每个渠道的窗口统计（真实请求数、失败数、成功率、账号池可用性）由运维聚合提供，"http" 与 "probe" 这类基础设施聚合不参与告警。'],
      ['何时生效', '保存后写入设置并立刻交给引擎；下一次评估按新阈值判定。正在告警的条目保持原状，避免改阈值造成一次虚假的“恢复 + 再告警”。'],
      ['恢复闭环', '触发与恢复都会写入系统日志（alert_fired / alert_recovered），因此“告警”和“恢复”在日志中心里成对出现。'],
      ['只读部署', '没有 Redis 的部署没有告警引擎，本页会说明原因而不是显示一个空表单。'],
    ];
    help.replaceChildren();
    items.forEach(([title, text]) => {
      const row = document.createElement('div');
      row.className = 'alerts-help-row';
      const strong = document.createElement('strong');
      strong.textContent = title;
      const span = document.createElement('span');
      span.textContent = text;
      row.appendChild(strong);
      row.appendChild(span);
      help.appendChild(row);
    });
  }

  async function loadRules() {
    setState('读取中…');
    try {
      const response = await fetch('/api/ops/alerts/rules', { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      state.rules = payload.rules || {};
      state.defaults = payload.defaults || {};
      state.editable = payload.editable !== false;
      renderFields();
      renderHelp();
      renderPreview();
      const editable = el('alertsSave');
      if (editable) editable.disabled = !state.editable;
      setState(state.editable ? '规则已加载' : '本部署未启用告警引擎（需要 Redis）', state.editable ? '' : 'is-warn');
      const note = el('alertsNote');
      if (note && payload.note) note.textContent = payload.note;
    } catch (error) {
      setState('读取失败：' + (error.message || error), 'is-error');
    }
  }

  async function saveRules() {
    const rules = readForm();
    const problem = validate(rules);
    if (problem) {
      showToast(problem, 'error');
      return;
    }
    setState('保存中…');
    try {
      const response = await fetch('/api/ops/alerts/rules', {
        method: 'PUT',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(rules),
      });
      if (!response.ok) throw new Error((await response.text()) || ('HTTP ' + response.status));
      const payload = await response.json();
      state.rules = payload.rules || rules;
      renderFields();
      renderPreview();
      setState('已保存并生效');
      showToast('告警规则已保存并生效');
    } catch (error) {
      setState('保存失败', 'is-error');
      showToast('保存失败：' + (error.message || error), 'error');
    }
  }

  function resetDefaults() {
    if (!state.defaults) return;
    state.rules = Object.assign({}, state.defaults);
    renderFields();
    renderPreview();
    setState('已填入默认值，尚未保存');
  }

  function eventMessage(body, message) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 5;
    td.className = 'table-empty-cell';
    td.textContent = message;
    tr.appendChild(td);
    body.appendChild(tr);
  }

  async function loadEvents() {
    const table = el('alertsEvents');
    if (!table) return;
    const body = table.querySelector('tbody');
    body.replaceChildren();
    try {
      const params = new URLSearchParams({ kind: 'system', action: 'alert_', limit: '20' });
      const response = await fetch('/api/journal/records?' + params.toString(), { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      const rows = (payload.data || []).filter((record) => {
        const action = (record.event || {}).action;
        return action === 'alert_fired' || action === 'alert_recovered';
      });
      if (!rows.length) {
        eventMessage(body, '保留窗口内没有触发记录。');
        return;
      }
      rows.forEach((record) => {
        const event = record.event || {};
        const tr = document.createElement('tr');
        const fired = event.action === 'alert_fired';
        [
          new Date(event.timestamp).toLocaleString(),
          fired ? '触发' : '恢复',
          (event.metadata && event.metadata.severity) || '—',
          event.channel || '—',
          event.error || event.details || '—',
        ].forEach((value) => {
          const td = document.createElement('td');
          td.textContent = String(value);
          tr.appendChild(td);
        });
        body.appendChild(tr);
      });
    } catch (error) {
      eventMessage(body, '读取触发记录失败：' + (error.message || error));
    }
  }

  function bind() {
    const save = el('alertsSave');
    if (save) save.addEventListener('click', saveRules);
    const reload = el('alertsReload');
    if (reload) reload.addEventListener('click', loadRules);
    const reset = el('alertsResetDefaults');
    if (reset) reset.addEventListener('click', resetDefaults);
    const eventsReload = el('alertsEventsReload');
    if (eventsReload) eventsReload.addEventListener('click', loadEvents);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { bind(); loadRules(); loadEvents(); });
  } else {
    bind();
    loadRules();
    loadEvents();
  }
})();
