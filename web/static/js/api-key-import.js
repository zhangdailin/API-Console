(function () {
 'use strict';
 const base = window.location.origin.replace(/\/$/, '');
  const el = id => document.getElementById(id);
  const form = el('keyImportForm');
  if (!form) return;
  const app = el('keyImportApp'), channel = el('keyImportChannel'), key = { value: '' };
  const model = el('keyImportModel'), load = el('keyLoadModels'), submit = el('keyImportButton');
  const providers = window.OrchidsProviderRegistry;
  let catalog = [], session = 0, selectedID;
  function option(value, label) {
    const node = document.createElement('option');
    node.value = value; node.textContent = label;
    return node;
  }
  providers.providers.forEach(provider => channel.appendChild(option(provider.key, provider.label)));
  channel.value = providers.defaultProviderKey;
  const provider = () => providers.get(channel.value);
  const endpoint = () => base + (app.value === 'claude' ? provider().apiPrefix.replace(/\/v1$/, '') : provider().apiPrefix);
  function update() {
    el('keyImportEndpoint').textContent = endpoint();
    el('keyImportHint').textContent = app.value === 'claude'
      ? '将默认模型及 Haiku / Sonnet / Opus 三个别名都映射到所选模型，避免客户端使用渠道中不存在的官方模型 ID。'
      : 'CC Switch 将生成 wire_api = "responses" 的 Codex 配置，默认推理档位为 high；若模型不支持 high，请在 CC Switch 中按模型能力修改或移除 model_reasoning_effort 后启用。';
    submit.disabled = !key.value.trim() || !catalog.some(item => item.id === model.value);
  }
  const catalogLoads = ConsoleUI.requestGate(), secretLoads = ConsoleUI.requestGate();
  function invalidate() {
    catalogLoads.cancel(); catalog = [];
    model.replaceChildren(option('', '请先读取可用模型')); model.disabled = true;
    load.disabled = false;
    el('keyCatalogStatus').textContent = '尚未读取目录';
    el('keyImportStatus').textContent = '';
    update();
  }
  channel.addEventListener('change', () => { invalidate(); if (key.value) return loadCatalog(); });

  app.addEventListener('change', update);
  model.addEventListener('change', update);
  async function loadCatalog() {
    const token = key.value.trim();
    if (!token || token.includes('*') || /\s/.test(token)) {
      el('keyCatalogStatus').textContent = '正在等待完整密钥，请稍后重试。'; return;
    }
    invalidate();
    const ticket = catalogLoads.begin(); load.disabled = true;
    el('keyCatalogStatus').textContent = '正在读取目录…';
    try {
      const response = await fetch(base + provider().apiPrefix + '/models', {
        credentials: 'omit', cache: 'no-store',
        headers: { Authorization: 'Bearer ' + token }, signal: ticket.signal,
      });
      if (!ticket.isCurrent()) return;
      if (!response.ok || !response.headers.get('content-type')?.includes('application/json')) throw new Error('目录请求失败');
      const value = await response.json();
      if (!ticket.isCurrent()) return;
      if (!value || !Array.isArray(value.data)) throw new Error('目录响应格式无效');
      const seen = new Set();
      catalog = value.data.filter(item => {
        if (!item || typeof item.id !== 'string' || !item.id.trim() || seen.has(item.id)) return false;
        seen.add(item.id); return true;
      });
      model.replaceChildren(option('', catalog.length ? '请选择模型' : '当前 Key 没有可用模型'));
      catalog.forEach(item => model.appendChild(option(item.id, item.id)));
      model.disabled = catalog.length === 0;
      el('keyCatalogStatus').textContent = catalog.length ? '已读取 ' + catalog.length + ' 个模型' : '没有可用模型，请检查账号、模型状态和 Key 权限。';
    } catch (error) {
      if (!ticket.accepts(error)) return;
      // Never expose response text: a misconfigured upstream could echo the Key.
      el('keyCatalogStatus').textContent = '目录读取失败，请检查完整 Key、登录状态与渠道可用性后重试。';
    } finally {
      if (ticket.isCurrent()) { load.disabled = false; update(); }
    }
  }
  load.addEventListener('click', loadCatalog);
  form.addEventListener('submit', event => {
    event.preventDefault();
    const token = key.value.trim();
    const row = apiKeys.find(item => String(item.id) === String(selectedID));
    if (!row || keyStatus(row) !== 'enabled') { invalidate(); el('keyCatalogStatus').textContent = '密钥停用或已过期，无法导入。'; return; }
    if (!['claude', 'codex'].includes(app.value) || !provider() || !token || !catalog.some(item => item.id === model.value)) return;
    const url = new URL('ccswitch://v1/import');
    const params = { resource: 'provider', app: app.value,
      name: 'API-Console · ' + provider().label + ' · ' + (app.value === 'claude' ? 'Claude Code' : 'Codex'),
      endpoint: endpoint(), apiKey: token, model: model.value, homepage: base };
    if (app.value === 'claude') Object.assign(params, { haikuModel: model.value, sonnetModel: model.value, opusModel: model.value });
    Object.entries(params).forEach(([name, value]) => url.searchParams.set(name, value));
    try {
      window.location.href = url.href;
      el('keyImportStatus').textContent = '已请求打开 CC Switch，请在应用中确认导入；未打开时检查安装与浏览器权限。';
    } catch (_) {
      el('keyImportStatus').textContent = '无法打开 CC Switch，请检查安装与浏览器权限。';
    }
  });
  globalThis.closeKeyImport = function () { session++; secretLoads.cancel(); selectedID = null; key.value = ''; invalidate(); closeModal('keyImportModal'); };
  globalThis.openKeyImport = async function (id) {
    const row = apiKeys.find(item => String(item.id) === String(id));
    if (!row || !row.secret_available || keyStatus(row) !== 'enabled') return;
    const current = ++session, ticket = secretLoads.begin(); selectedID = id;
    key.value = ''; channel.value = providers.defaultProviderKey; app.value = 'codex'; invalidate();
    setText('keyImportName', row.name); openModal('keyImportModal');
    setText('keyCatalogStatus', '正在读取密钥…');
    try {
      const secret = await getKeySecret(id, { signal: ticket.signal });
      if (current !== session || !ticket.isCurrent()) return;
      key.value = secret; await loadCatalog();
    } catch (error) { if (current === session && ticket.accepts(error)) setText('keyCatalogStatus', '密钥读取失败，请关闭后重试。'); }
  };
  document.addEventListener('keydown', event => { if (event.key === 'Escape') closeKeyImport(); });
  window.addEventListener('pagehide', () => { key.value = ''; session++; secretLoads.cancel(); catalogLoads.cancel(); });
  update();
})();
