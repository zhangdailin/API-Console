(() => {
  'use strict';
  let data = null;
  let pending = false;
  let pollTimer = null;
  const busyPhases = new Set(['queued', 'downloading', 'verifying', 'staging', 'restart_pending', 'restarting', 'rolling_back']);
  const terminalPhases = new Set(['complete', 'failed', 'rolled_back', 'recovery_failed']);
  const text = ConsoleUI.setText;
  const node = ConsoleUI.el;
  const checkLoads = ConsoleUI.requestGate();
  const operationLoads = ConsoleUI.requestGate();
  function render() {
    if (!data) return;
    const op = data.operation;
    const busy = pending || busyPhases.has(op?.phase);
    text('versionBadge', data.current.version + (data.has_update ? ' · 有更新' : ''));
    document.querySelectorAll('[data-system-open]').forEach(button => {button.classList.toggle('has-update', data.has_update);button.title = data.has_update ? '发现新版本：' + data.release.tag_name : '版本与升级';});
    text('systemCurrent', `${data.current.version} · ${data.current.commit} · ${data.current.build_type}`);
    text('systemLatest', data.available ? data.release?.tag_name || '暂无发行版' : '未能检查');
    text('systemCheckState', data.warning || (data.has_update ? '发现新版本' : data.available ? '当前没有更高的稳定发行版' : '正在检查…'));
    text('systemCapability', data.reason || '升级会短暂重启服务；配置和账号数据保留。失败时自动恢复旧程序。');
    text('systemProgress', op ? op.message : '暂无升级操作');
    node('systemProgress').classList.toggle('is-error', ['failed', 'rolled_back', 'recovery_failed'].includes(op?.phase));
    node('systemUpdate').disabled = busy || !data.available || !data.can_update || !data.has_update;
    node('systemRollback').disabled = busy || !data.can_rollback;
    node('systemCheck').disabled = pending;
    const link = node('systemReleaseLink');
    link.hidden = !data.release;
    if (data.release) link.href = `https://github.com/${data.current.repository}/releases/tag/${encodeURIComponent(data.release.tag_name)}`;
    const notes = String(data.release?.body || '').replace(/^\s*(?:\*\*)?Full Changelog(?:\*\*)?\s*:.*$/gmi, '').trim();
    text('systemReleaseNotes', notes);
    node('systemReleaseNotes').hidden = !notes;
    if (busy && !pollTimer) pollTimer = setTimeout(poll, 1500);
  }
  async function check(force = false) {
    if (pending) return;
    const ticket = checkLoads.begin();
    const controller = new AbortController();
    ticket.signal.addEventListener('abort', () => controller.abort(), { once: true });
    const timer = setTimeout(() => controller.abort(), 25000);
    try {
      const result = await ConsoleAPI.json('/api/system/check-updates' + (force ? '?force=true' : ''), { signal: controller.signal });
      if (!ticket.isCurrent()) return;
      data = result;
      if (terminalPhases.has(data.operation?.phase)) { try { localStorage.removeItem('api-console-update-key'); } catch (_) {} }
      render();
    } catch (err) {
      if (!ticket.isCurrent()) return;
      text('systemCheckState', '检查失败：' + err.message);
      if (data) { data.available = false; data.warning = '检查失败：' + err.message; render(); }
    } finally { clearTimeout(timer); }
  }
  async function poll() {
    pollTimer = null;
    if (pending) { render(); return; }
    const ticket = operationLoads.begin();
    try {
      const result = await ConsoleAPI.json('/api/system/operation', { signal: ticket.signal });
      if (!ticket.isCurrent()) return;
      if (data) data.operation = result.operation;
      if (terminalPhases.has(result.operation?.phase)) {
        try { localStorage.removeItem('api-console-update-key'); } catch (_) {}
        await check();
        return;
      }
      render();
    } catch (err) {
      if (!ticket.accepts(err)) return;
      text('systemProgress', '服务正在重启或暂时不可达，继续等待操作状态…');
      pollTimer = setTimeout(poll, 2500);
    }
  }
  async function act(kind) {
    if (!data || pending || busyPhases.has(data.operation?.phase)) return;
    if (kind === 'rollback' && !confirm('恢复上一次升级前的程序并重启？这不会回退账号或配置数据。')) return;
    checkLoads.cancel();
    operationLoads.cancel();
    pending = true; render();
    let key;
    try { key = localStorage.getItem('api-console-update-key'); } catch (_) {}
    key ||= crypto.randomUUID();
    try { localStorage.setItem('api-console-update-key', key); } catch (_) {}
    try {
      const result = await ConsoleAPI.json('/api/system/' + kind, ConsoleAPI.jsonOptions('POST',
        { version: kind === 'update' ? data.release.tag_name : '' }, { headers: { 'Idempotency-Key': key } }));
      data.operation = result.operation;
    } catch (err) {
      text('systemProgress', '提交结果待确认：' + err.message + '。正在读取后台状态，刷新页面不会取消已提交的任务。');
    } finally {
      pending = false;
      operationLoads.cancel();
      if (!pollTimer) pollTimer = setTimeout(poll, 1000);
    }
  }
  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-system-open]').forEach(button => button.addEventListener('click', () => {
      ConsoleUI.modal('systemModal', true); check();
    }));
    node('systemClose')?.addEventListener('click', () => ConsoleUI.modal('systemModal', false));
    node('systemCheck')?.addEventListener('click', () => check(true));
    node('systemUpdate')?.addEventListener('click', () => act('update'));
    node('systemRollback')?.addEventListener('click', () => act('rollback'));
    check();
    setInterval(() => check(), 20 * 60 * 1000);
  });
})();
