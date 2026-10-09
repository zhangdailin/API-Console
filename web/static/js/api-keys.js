// Key management shares the configuration page's modal and DOM primitives.
let keyPage = 1, keysLoading = false, keysLoaded = false, keysLoadSequence = 0;
const keyBusy = new Set();
function keyStatus(key) {
  if (!key.enabled) return 'disabled';
  return key.expires_at && new Date(key.expires_at) <= new Date() ? 'expired' : 'enabled';
}
function filteredKeys() {
  const search = (document.getElementById('keySearch')?.value || '').trim().toLowerCase();
  const status = document.getElementById('keyStatusFilter')?.value || '';
  return apiKeys.filter(key => (!search || [key.name, key.key_suffix, key.key_prefix].some(value => String(value || '').toLowerCase().includes(search))) && (!status || keyStatus(key) === status))
    .sort((a, b) => b.id - a.id);
}
function filterApiKeys() { keyPage = 1; renderApiKeys(); }
async function loadApiKeys() {
  const sequence = ++keysLoadSequence;
  keysLoading = true;
  setText('keyLoadStatus', '正在刷新…');
  const refresh = document.getElementById('keyRefresh');
  if (refresh) refresh.disabled = true;
  if (!keysLoaded) document.getElementById('keysList').replaceChildren(make('div', 'key-loading', '正在加载密钥…'));
  try {
    const result = await ConsoleAPI.json('/api/keys');
    if (sequence !== keysLoadSequence) return;
    if (result !== null && !Array.isArray(result)) throw new Error('密钥列表格式无效');
    apiKeys = result || []; keysLoaded = true; renderApiKeys();
    setText('keyLoadStatus', '');
  } catch (err) {
    if (sequence === keysLoadSequence) {
      setText('keyLoadStatus', '加载失败，请点击刷新重试');
      if (!keysLoaded) document.getElementById('keysList').replaceChildren(make('div', 'empty-state', '密钥加载失败'));
    }
  } finally {
    if (sequence === keysLoadSequence) { keysLoading = false; if (refresh) refresh.disabled = false; }
  }
}
function keyButton(label, action, id, danger = false) {
  const button = make('button', danger ? 'btn btn-danger-outline' : 'btn btn-outline', label);
  button.type = 'button'; button.dataset.action = action; button.dataset.id = String(id);
  button.disabled = keyBusy.has(String(id));
  return button;
}
function bindApiKeyActions(container) {
  container.onclick = event => {
    const button = event.target.closest('[data-action]');
    if (!button || !container.contains(button) || button.disabled) return;
    const id = button.dataset.id, key = apiKeys.find(item => String(item.id) === id);
    const handlers = {
      'copy-key': () => copyApiKey(id), 'import-key': () => openKeyImport(id),
      'edit-key': () => openEditKeyModal(id), 'rotate-key': () => rotateApiKey(id),
      'toggle-key': () => toggleKeyStatus(id, !key.enabled),
      'delete-key': () => openDeleteKeyModal(id, key?.name || ''),
    };
    handlers[button.dataset.action]?.();
  };
}
function renderApiKeys() {
  const container = document.getElementById('keysList');
  if (!container) return;
  const rows = filteredKeys(), total = Math.max(1, Math.ceil(rows.length / 20));
  keyPage = Math.min(keyPage, total);
  setText('keyCount', apiKeys.length + ' 个密钥');
  setText('keyPageSummary', rows.length + ' 条结果 · 第 ' + keyPage + '/' + total + ' 页');
  ConsoleUI.pagination(document.getElementById('keyPagination'), keyPage, total, page => { keyPage = page; renderApiKeys(); });
  if (!rows.length) {
    container.replaceChildren(make('div', 'empty-state', apiKeys.length ? '没有匹配的密钥，请调整筛选条件。' : '暂无 API 密钥，点击“创建 API Key”开始。'));
    return;
  }
  const labels = ['名称 / 密钥', '状态', '模型权限', '请求限制', '有效期 / 最近使用', '操作'];
  const table = attach(make('table', 'key-table'), [attach(make('thead'), [attach(make('tr'), labels.map(label => make('th', '', label)))])]);
  const body = make('tbody');
  rows.slice((keyPage - 1) * 20, keyPage * 20).forEach(key => {
    const status = keyStatus(key), names = { enabled: '启用', disabled: '停用', expired: '已过期' };
    const identity = attach(make('td'), [make('div', 'key-name', key.name), make('code', 'key-masked', (key.key_prefix || 'sk-') + '••••' + key.key_suffix)]);
    const models = key.allowed_models?.length ? key.allowed_models : ['全部模型'];
    const permissions = attach(make('div', 'key-models'), models.slice(0, 2).map(model => make('span', 'tag', model)));
    if (models.length > 2) permissions.appendChild(make('span', 'key-note', '+' + (models.length - 2)));
    permissions.title = models.join(', ');
    const limits = attach(make('td'), [make('div', '', key.rpm_limit > 0 ? key.rpm_limit + ' RPM' : 'RPM 不限'), make('small', 'key-note', key.max_concurrent > 0 ? '并发上限 ' + key.max_concurrent : '并发不限')]);
    const dates = attach(make('td'), [make('div', '', key.expires_at ? formatTime(key.expires_at) : '永不过期'), make('small', 'key-note', key.last_used_at ? '最近 ' + formatTime(key.last_used_at) : '从未使用')]);
    const actions = attach(make('div', 'key-actions'), [
      keyButton('复制', 'copy-key', key.id), keyButton('导入 CC Switch', 'import-key', key.id),
      keyButton('编辑', 'edit-key', key.id), keyButton(key.enabled ? '停用' : '启用', 'toggle-key', key.id),
      keyButton('轮换', 'rotate-key', key.id), keyButton('删除', 'delete-key', key.id, true),
    ]);
    for (const button of actions.children) {
      if (['copy-key', 'import-key'].includes(button.dataset.action) && !key.secret_available) { button.disabled = true; button.title = '密钥内容不可用'; }
      if (button.dataset.action === 'import-key' && status !== 'enabled') { button.disabled = true; button.title = '密钥停用或已过期'; }
    }
    body.appendChild(attach(make('tr'), [identity, attach(make('td'), [make('span', 'key-status key-status-' + status, names[status])]), attach(make('td'), [permissions]), limits, dates, attach(make('td'), [actions])]));
  });
  table.appendChild(body); container.replaceChildren(ConsoleUI.responsiveTable(table)); bindApiKeyActions(container);
}
async function getKeySecret(id) {
  const result = await ConsoleAPI.json('/api/keys/' + id + '/secret', { cache: 'no-store' });
  if (!result || typeof result.key !== 'string' || !result.key.startsWith('sk-')) throw new Error('无法读取完整密钥');
  return result.key;
}
async function keyOperation(id, operation) {
  const identity = String(id);
  if (keyBusy.has(identity)) return;
  keyBusy.add(identity); renderApiKeys();
  try { await operation(); } catch (err) { showToast(err.message || '操作失败', 'error'); }
  finally { keyBusy.delete(identity); renderApiKeys(); }
}
async function copyApiKey(id) {
  return keyOperation(id, async () => {
    const secret = await getKeySecret(id);
    // Fetching a secret can consume browser activation; the shared helper supplies a fallback.
    await copyToClipboard(secret);
  });
}
async function toggleKeyStatus(id, enabled) {
  return keyOperation(id, async () => {
    await ConsoleAPI.json('/api/keys/' + id, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) });
    await loadApiKeys();
  });
}
function parseAllowedModels(value) { return String(value || '').split(/[\n,]/).map(item => item.trim()).filter(Boolean); }
function toDatetimeLocal(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}
function keyPolicy(prefix) {
  const number = (suffix, max) => {
    const value = Number(document.getElementById(prefix + suffix).value || 0);
    if (!Number.isSafeInteger(value) || value < 0 || (max && value > max)) throw new Error('请求限制必须是有效的非负整数，并发上限最多 1024');
    return value;
  };
  const expiry = document.getElementById(prefix + 'ExpiresAt').value;
  if (expiry && (!Number.isFinite(new Date(expiry).getTime()) || new Date(expiry) <= new Date())) throw new Error('到期时间必须晚于当前时间');
  return { allowed_models: parseAllowedModels(document.getElementById(prefix + 'AllowedModels').value),
    rpm_limit: number('RPMLimit'), max_concurrent: number('MaxConcurrent', 1024), expires_at: expiry ? new Date(expiry).toISOString() : null };
}
function openCreateKeyModal() {
  for (const suffix of ['Name', 'AllowedModels', 'ExpiresAt']) setValue('key' + suffix, '');
  for (const suffix of ['RPMLimit', 'MaxConcurrent']) setValue('key' + suffix, '0');
  setText('createKeyError', ''); openModal('createKeyModal');
}
function closeCreateKeyModal() { if (!document.getElementById('createKeySubmit').disabled) closeModal('createKeyModal'); }
async function createApiKey(event) {
  event.preventDefault();
  const button = document.getElementById('createKeySubmit'); if (button.disabled) return;
  let policy, names;
  try { policy = keyPolicy('key'); names = document.getElementById('keyName').value.split('\n').map(name => name.trim()).filter(Boolean); if (!names.length) throw new Error('请填写名称'); }
  catch (err) { setText('createKeyError', err.message); return; }
  button.disabled = true; createdKeys = []; setText('createKeyError', '');
  try {
    for (const name of names) {
      try { const result = await ConsoleAPI.json('/api/keys', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, ...policy }) }); createdKeys.push({ name, key: result.key }); }
      catch (err) { createdKeys.push({ name, error: err.message }); }
    }
    if (!createdKeys.some(item => item.key)) { setText('createKeyError', createdKeys.map(item => item.name + ': ' + item.error).join('\n')); createdKeys = []; return; }
    closeModal('createKeyModal'); renderCreatedKeys(); openModal('showKeyModal'); await loadApiKeys();
  } finally { button.disabled = false; }
}
function openEditKeyModal(id) {
  const key = apiKeys.find(item => String(item.id) === String(id)); if (!key) return;
  setValue('editKeyId', id); setText('editKeyName', key.name);
  setValue('editKeyAllowedModels', (key.allowed_models || []).join('\n'));
  setValue('editKeyRPMLimit', key.rpm_limit || 0); setValue('editKeyMaxConcurrent', key.max_concurrent || 0);
  setValue('editKeyExpiresAt', toDatetimeLocal(key.expires_at)); setText('editKeyError', ''); openModal('editKeyModal');
}
function closeEditKeyModal() { if (!document.getElementById('editKeySubmit').disabled) closeModal('editKeyModal'); }
async function saveKeyPolicy(event) {
  event.preventDefault(); const button = document.getElementById('editKeySubmit'); if (button.disabled) return;
  let policy; try { policy = keyPolicy('editKey'); } catch (err) { setText('editKeyError', err.message); return; }
  button.disabled = true; setText('editKeyError', '');
  try { await ConsoleAPI.json('/api/keys/' + document.getElementById('editKeyId').value, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(policy) }); closeModal('editKeyModal'); await loadApiKeys(); }
  catch (err) { setText('editKeyError', err.message); } finally { button.disabled = false; }
}
function renderCreatedKeys() {
  const rows = createdKeys.map(item => {
    const row = attach(make('div', 'key-secret-result'), [make('strong', '', item.name), make(item.key ? 'code' : 'p', item.key ? 'key-secret' : 'warning-text', item.key || item.error)]);
    if (item.key) { const button = make('button', 'btn btn-outline', '复制'); button.type = 'button'; button.onclick = () => copyToClipboard(item.key); row.appendChild(button); }
    return row;
  });
  document.getElementById('fullKeyDisplay').replaceChildren(...rows);
}
function copyAllKeys() { copyToClipboard(createdKeys.filter(item => item.key).map(item => item.name + ': ' + item.key).join('\n')); }
function closeShowKeyModal() { closeModal('showKeyModal'); createdKeys = []; document.getElementById('fullKeyDisplay').replaceChildren(); }
let keyConfirmation = null;
function rotateApiKey(id) {
  const key = apiKeys.find(item => String(item.id) === String(id)); if (!key) return;
  keyConfirmation = { id, action: 'rotate' };
  setText('deleteKeyModalTitle', '轮换 API 密钥'); setText('deleteKeyModalDescription', '为“' + key.name + '”生成新密钥？旧密钥会立即失效，客户端需要更新。');
  setText('deleteKeyConfirm', '确认轮换'); setText('deleteKeyError', ''); openModal('deleteKeyModal');
}
function openDeleteKeyModal(id, name) {
  keyConfirmation = { id, action: 'delete' };
  setText('deleteKeyModalTitle', '删除 API 密钥'); setText('deleteKeyModalDescription', '确定删除“' + name + '”？使用它的客户端将无法访问，此操作不可撤销。');
  setText('deleteKeyConfirm', '确认删除'); setText('deleteKeyError', ''); openModal('deleteKeyModal');
}
function closeDeleteKeyModal() { if (!document.getElementById('deleteKeyConfirm').disabled) { closeModal('deleteKeyModal'); keyConfirmation = null; } }
async function confirmDeleteKey() {
  const button = document.getElementById('deleteKeyConfirm'); if (button.disabled || !keyConfirmation) return;
  const { id, action } = keyConfirmation; button.disabled = true;
  try {
    if (action === 'rotate') {
      const result = await ConsoleAPI.json('/api/keys/' + id + '/rotate', { method: 'POST' });
      createdKeys = [{ name: result.name, key: result.key }]; renderCreatedKeys(); openModal('showKeyModal');
    } else {
      const response = await ConsoleAPI.request('/api/keys/' + id, { method: 'DELETE' });
      if (!response.ok) throw new Error(ConsoleAPI.detail(await response.text(), '删除失败'));
    }
    closeModal('deleteKeyModal'); keyConfirmation = null; await loadApiKeys();
  } catch (err) { setText('deleteKeyError', err.message); } finally { button.disabled = false; }
}
