// Portable account backups. The file is kept only until import/close, never stored.
let accountImportData = null;
let accountImportBusy = false;
let accountImportSequence = 0;
function openAccountImport() {
  if (accountImportBusy) return;
  const input = document.getElementById('accountImportFile');
  input.value = ''; input.click();
}
function closeAccountImport() {
  if (accountImportBusy) return;
  accountImportSequence++; accountImportData = null;
  document.getElementById('accountImportFile').value = '';
  document.getElementById('accountImportConfirm').disabled = true;
  ConsoleUI.modal('accountImportModal', false);
}
async function readAccountImport(input) {
  if (accountImportBusy) return;
  const file = input.files?.[0];
  input.value = ''; if (!file) return;
  const current = ++accountImportSequence;
  accountImportData = null;
  ConsoleUI.modal('accountImportModal', true);
  const confirmButton = document.getElementById('accountImportConfirm');
  confirmButton.disabled = true;
  ConsoleUI.setText('accountImportSummary', '正在读取备份…');
  document.getElementById('accountImportResult').replaceChildren();
  try {
    if (file.size > 8 * 1024 * 1024) throw new Error('文件超过 8 MiB 限制');
    const value = JSON.parse((await file.text()).replace(/^\uFEFF/, ''));
    if (current !== accountImportSequence) return;
    if (value?.version !== 1 || !Array.isArray(value.accounts) || !value.accounts.length ||
        value.accounts.some(row => !row || typeof row !== 'object' || Array.isArray(row))) {
      throw new Error('请选择包含账号的 version 1 导出文件');
    }
    const counts = new Map();
    value.accounts.forEach(row => {
      const provider = window.OrchidsProviderRegistry.get(row.account_type);
      const label = provider ? provider.label : '未知渠道（将跳过）';
      counts.set(label, (counts.get(label) || 0) + 1);
    });
    accountImportData = value;
    ConsoleUI.setText('accountImportSummary', '待导入 ' + value.accounts.length + ' 个账号：' + Array.from(counts, ([label, count]) => label + ' ' + count).join('，') + '。确认后执行追加恢复。');
    confirmButton.disabled = false;
  } catch (_) {
    if (current !== accountImportSequence) return;
    ConsoleUI.setText('accountImportSummary', '无法读取备份：请使用不超过 8 MiB、包含账号的 version 1 JSON 导出文件。');
  }
}
async function confirmAccountImport() {
  if (accountImportBusy || !accountImportData) return;
  accountImportBusy = true;
  document.getElementById('accountImportConfirm').disabled = true;
  document.getElementById('accountImportClose').disabled = true;
  document.getElementById('accountImportOpen').disabled = true;
  ConsoleUI.setText('accountImportSummary', '正在导入，请等待结果…');
  try {
    const result = await ConsoleAPI.json('/api/import', ConsoleAPI.jsonOptions('POST', accountImportData));
    if (!result || !['total', 'imported', 'skipped', 'duplicates', 'invalid', 'failed'].every(name => Number.isInteger(result[name]) && result[name] >= 0) || result.imported + result.skipped !== result.total) throw new Error('结果格式无效');
    accountImportData = null;
    ConsoleUI.setText('accountImportSummary', `已处理 ${result.total} 个：导入 ${result.imported}，重复 ${result.duplicates}，无效 ${result.invalid}，写入失败 ${result.failed}。`);
    const reasons = { duplicate_account: '已有相同账号，保留现有记录', unsupported_channel: '不支持的渠道', missing_refresh_token: '缺少可续期的刷新令牌', missing_device_identity: '缺少 Qoder 设备身份', storage_error: '存储写入失败' };
    const container = document.getElementById('accountImportResult');
    container.replaceChildren();
    (Array.isArray(result.issues) ? result.issues : []).slice(0, 50).forEach(issue => {
      const line = document.createElement('p');
      line.textContent = `第 ${Number(issue.index) || '?'} 条：${reasons[issue.reason] || '账号未导入'}`;
      container.appendChild(line);
    });
    if (result.issues?.length > 50) { const line = document.createElement('p'); line.textContent = '仅显示前 50 条明细。'; container.appendChild(line); }
    if (result.imported > 0) loadAccounts();
  } catch (_) {
    // A lost response may follow partial writes. Re-import safely skips restored rows.
    ConsoleUI.setText('accountImportSummary', '未能确认导入结果。可能已有部分账号写入，请检查账号列表；重新选择备份导入会跳过已有账号。');
    accountImportData = null;
    loadAccounts();
  } finally {
    accountImportBusy = false;
    document.getElementById('accountImportClose').disabled = false;
    document.getElementById('accountImportOpen').disabled = false;
  }
}
