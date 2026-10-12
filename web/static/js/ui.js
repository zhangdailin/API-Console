// Small shared primitives. Pages retain their business state and API contracts.
globalThis.ConsoleAPI = (() => {
  function detail(value, fallback) {
    if (typeof value === 'string') {
      try { return detail(JSON.parse(value), fallback); } catch (_) { return value.trim() || fallback; }
    }
    return value?.error?.message || value?.message || value?.msg ||
      (typeof value?.error === 'string' ? value.error : fallback);
  }
  async function request(url, options = {}) {
    const response = await fetch(url, { credentials: 'same-origin', ...options });
    if (response.status === 401 && !options.signal?.aborted) {
      if (window.location) window.location.href = './login.html';
    }
    return response;
  }
  async function checked(url, options = {}) {
    const response = await request(url, options);
    if (!response.ok) {
      const error = new Error(detail(await response.text(), `HTTP ${response.status}`));
      error.status = response.status;
      throw error;
    }
    return response;
  }
  function jsonOptions(method, value, options = {}) {
    return { ...options, method, headers: { ...options.headers, 'Content-Type': 'application/json' }, body: JSON.stringify(value) };
  }
  async function json(url, options = {}, contract = {}) {
    const response = await checked(url, options);
    const contentType = response.headers?.get?.('content-type');
    if (contentType && !contentType.toLowerCase().includes('application/json')) { throw new Error('接口未返回 JSON，登录状态可能已失效'); }
    let value = await response.json();
    if (contract.envelope) {
      if (!value || value.code !== 0) throw new Error(detail(value, '请求失败'));
      value = value.data ?? value;
    }
    if (contract.array && !Array.isArray(value)) throw new Error('接口列表格式无效');
    return value;
  }
  async function batch(items, operation) {
    const succeeded = [], failed = [];
    for (const item of items) {
      try { await operation(item); succeeded.push(item); }
      catch (error) { failed.push({ item, error }); }
    }
    return { succeeded, failed };
  }
  async function journal(params, options = {}) {
    const payload = await json('/api/journal/records?' + params.toString(), options);
    if (!payload || (payload.data != null && !Array.isArray(payload.data))) throw new Error('日志列表格式无效');
    return { ...payload, data: payload.data || [] };
  }
  return Object.freeze({ request, checked, json, jsonOptions, batch, journal, detail });
})();

globalThis.ConsoleUI = (() => {
  const el = (id) => document.getElementById(id);
  // A ticket owns updates until another request starts or the scope closes.
  function requestGate() {
    let sequence = 0, controller;
    function cancel() { sequence++; controller?.abort(); controller = null; }
    function begin() {
      cancel();
      controller = new AbortController();
      const current = sequence, signal = controller.signal;
      const isCurrent = () => current === sequence && !signal.aborted;
      return Object.freeze({ signal, isCurrent, accepts: error => isCurrent() && error?.name !== 'AbortError' });
    }
    return Object.freeze({ begin, cancel });
  }
  function submission() {
    let busy = false;
    async function run(operation, buttons = []) {
      if (busy) return;
      busy = true;
      const controls = buttons.filter(Boolean).map(node => ({ node, disabled: node.disabled }));
      controls.forEach(({ node }) => { node.disabled = true; });
      try { return await operation(); }
      finally {
        busy = false;
        controls.forEach(({ node, disabled }) => { node.disabled = disabled; });
      }
    }
    return Object.freeze({ run, get busy() { return busy; } });
  }
  function keyedSubmission() {
    const active = new Set();
    const busy = id => active.has(String(id));
    async function run(id, operation) {
      if (busy(id)) return;
      const key = String(id);
      active.add(key);
      try { return await operation(); }
      finally { active.delete(key); }
    }
    return Object.freeze({ run, busy });
  }
  function make(tag, className, value) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (value != null) node.textContent = value;
    return node;
  }
  function attach(parent, children) {
    (children || []).forEach((child) => { if (child) parent.appendChild(child); });
    return parent;
  }
  function setText(id, value) {
    const node = el(id);
    if (node) node.textContent = value == null ? '' : String(value);
  }
  function modal(id, open) {
    const node = el(id);
    if (!node) return;
    node.classList.toggle('active', open);
    node.style.display = open ? 'flex' : 'none';
    node.setAttribute('aria-hidden', String(!open));
  }
  function pagination(container, current, total, select) {
    if (!container) return;
    container.replaceChildren();
    const button = (label, page, disabled = false) => {
      const node = make('button', 'btn btn-page ' + (page === current && /^\d+$/.test(label) ? 'btn-primary' : 'btn-outline'), label);
      node.type = 'button';
      node.dataset.page = String(page);
      node.disabled = disabled;
      container.appendChild(node);
    };
    button('首页', 1, current === 1);
    button('上一页', current - 1, current === 1);
    const start = Math.max(1, Math.min(current - 2, total - 4));
    for (let page = start; page <= Math.min(total, start + 4); page++) button(String(page), page);
    button('下一页', current + 1, current === total);
    button('末页', total, current === total);
    container.onclick = (event) => {
      const node = event.target.closest('button[data-page]');
      if (node && container.contains(node) && !node.disabled) select(Number(node.dataset.page));
    };
  }
  // One row DOM at every viewport width. CSS presents cells as labelled cards.
  function responsiveTable(table) {
    if (!table) return table;
    table.classList.add('responsive-table');
    const headers = Array.from(table.querySelectorAll('thead th'));
    table.querySelectorAll('tbody tr').forEach((row) => {
      Array.from(row.children).forEach((cell, index) => {
        cell.dataset.label = headers[index]?.textContent || '';
      });
    });
    return table;
  }
  return Object.freeze({ el, make, attach, setText, modal, pagination, responsiveTable, requestGate, submission, keyedSubmission });
})();
