// Doubao QR login is a server-owned transaction. Only the PNG QR data URL and
// official scan target reach this page; passport cookies remain in memory on
// the server until a completed account is persisted.
globalThis.DoubaoLogin = (() => {
  let active = null;
  const status = (text, error = false) => {
    const node = document.getElementById('doubaoLoginStatus');
    if (!node) return;
    node.hidden = !text; node.textContent = text || '';
    node.classList.toggle('is-error', error); node.classList.toggle('is-active', Boolean(text) && !error);
  };
  const image = () => document.getElementById('doubaoLoginQR');
  async function readJSON(response, action) {
    const raw = await response.text();
    let data = null;
    try { data = raw ? JSON.parse(raw) : {}; }
    catch (_) {
      const kind = /^\s*</.test(raw) ? 'HTML 页面' : '非 JSON 响应';
      throw new Error(`${action}返回了${kind}（HTTP ${response.status}）。请确认网关已升级至 v1.0.180 并重启，且反向代理没有改写 /api/doubao/login。`);
    }
    if (!response.ok) throw new Error(data?.error || data?.message || `${action}失败（HTTP ${response.status}）`);
    return data;
  }
  const clear = () => { if (!active) return; clearInterval(active.timer); active.controller.abort(); active = null; };
  const cancel = () => { const current = active; clear(); if (current?.id) fetch(`/api/doubao/login/${encodeURIComponent(current.id)}`, { method: 'DELETE', credentials: 'same-origin' }).catch(() => {}); };
  const finish = (message, error) => { clear(); status(message, error); if (!error) { showToast(message || '豆包账号已保存', 'success'); closeModal(); loadAccounts(); } };
  async function poll() {
    const current = active; if (!current || current.polling) return; current.polling = true;
    try {
      const response = await fetch(`/api/doubao/login/${encodeURIComponent(current.id)}`, { credentials: 'same-origin', signal: current.controller.signal });
      if (response.status === 404) { finish('扫码会话已失效，请重新开始。', true); return; }
      const state = await readJSON(response, '查询豆包扫码状态'); if (active !== current) return;
      if (state.status === 'complete') return finish(state.message || '豆包账号已保存');
      if (state.status === 'failed' || state.status === 'expired') return finish(state.message || '扫码登录未完成，请重新开始。', true);
      if (state.qr_code && image()) image().src = state.qr_code;
      const labels = { pending: '请使用豆包 App 扫描二维码并在手机确认…', scanned: '已扫码，等待手机确认…' };
      status(labels[state.status] || state.message || '等待扫码确认…');
    } catch (error) { if (error?.name !== 'AbortError') status('正在重试与服务端的扫码连接…'); }
    finally { current.polling = false; }
  }
  async function start() {
    if (active) return;
    const button = document.getElementById('doubaoLoginButton'); if (button) button.disabled = true;
    status('正在生成豆包扫码二维码…');
    try {
      const response = await fetch('/api/doubao/login', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: document.getElementById('enabled')?.checked !== false }) });
      const data = await readJSON(response, '创建豆包扫码会话');
      active = { id: data.id, controller: new AbortController(), polling: false, timer: null };
      if (data.qr_code && image()) image().src = data.qr_code;
      const link = document.getElementById('doubaoLoginLinkText'); if (link && data.verification_uri) { link.href = data.verification_uri; link.textContent = data.verification_uri; document.getElementById('doubaoLoginLink').hidden = false; }
      status('请使用豆包 App 扫描二维码并在手机确认…');
      active.timer = setInterval(poll, 1300); poll();
    } catch (error) { status(error.message || '生成二维码失败，请重试。', true); }
    finally { if (button) button.disabled = false; }
  }
  return { start, stop: cancel, reset: () => { cancel(); status(''); if (image()) image().removeAttribute('src'); } };
})();
