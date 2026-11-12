// Doubao human verification.
//
// The upstream parks an account behind a captcha only a person can answer. The
// console answers it with the upstream's own captcha SDK, handed the recorded
// challenge and the device identity the refused request presented.
//
// The SDK is third-party script, so it never runs in the console's origin:
// it lives in a sandboxed srcdoc frame (allow-scripts only), whose origin is
// opaque. It cannot read the console's cookies or storage, nor call its API
// with the admin session. The frame and this page talk only by postMessage,
// and each side checks the other's window.
(() => {
  const SDK_SOURCES = [
    'https://lf-rc1.yhgfb-cn-static.com/obj/rc-verifycenter/rmc-captcha/1.0.0.739/captcha.js',
    'https://lf-rc2.yhgfb-cn-static.com/obj/rc-verifycenter/rmc-captcha/1.0.0.739/captcha.js',
    'https://lf-cdn-tos.bytescm.com/obj/rc-verifycenter/rmc-captcha/1.0.0.739/captcha.js',
  ];

  // frameDocument is the sandboxed page. It loads the SDK once it receives the
  // challenge and reports only what happened, never the SDK's result payload.
  function frameDocument() {
    return `<!doctype html><meta charset="utf-8">
<style>html,body{margin:0;background:transparent;font:14px system-ui,sans-serif}#c{min-height:380px;display:flex;justify-content:center}</style>
<div id="c"></div>
<script>
(function () {
  var sources = ${JSON.stringify(SDK_SOURCES)};
  var started = false;
  function send(type, detail) { parent.postMessage({ source: 'doubao-verify', type: type, detail: detail || '' }, '*'); }
  function load(index, done) {
    if (index >= sources.length) { send('error', '官方验证组件加载失败'); return; }
    var script = document.createElement('script');
    script.src = sources[index];
    script.onload = done;
    script.onerror = function () { script.remove(); load(index + 1, done); };
    document.head.appendChild(script);
  }
  function start(params) {
    load(0, function () {
      try {
        var instance = new window.bdCaptcha.CaptchaVerify({
          info: params.info, ele: 'c', host: params.host, env: params.env,
          successCb: function () { send('success'); },
          closeCb: function () { send('closed'); },
          feedbackSubmitCb: function () {}
        });
        instance.init();
        instance.render(params.verify_data);
        send('rendered');
      } catch (err) { send('error', String(err && err.message || err)); }
    });
  }
  window.addEventListener('message', function (event) {
    if (event.source !== parent || started || !event.data || event.data.source !== 'doubao-verify-host') return;
    started = true;
    start(event.data.params || {});
  });
  send('ready');
})();
<\/script>`;
  }

  let active = null;

  function setStatus(text, tone) {
    const node = document.getElementById('verificationStatus');
    if (!node) return;
    node.textContent = text;
    node.dataset.tone = tone || '';
  }

  function close() {
    if (active) window.removeEventListener('message', active.listener);
    active = null;
    const frame = document.getElementById('verificationFrame');
    if (frame) frame.removeAttribute('srcdoc');
    ConsoleUI.modal('verificationModal', false);
  }

  async function solved(id) {
    setStatus('验证已完成，正在恢复账号调度…', 'info');
    try {
      await ConsoleAPI.json(`/api/accounts/${id}/verification`, ConsoleAPI.jsonOptions('POST', {}));
      close();
      showToast('验证已完成，账号已恢复调度；下一次请求会确认上游是否放行', 'success');
      if (typeof loadAccounts === 'function') loadAccounts();
    } catch (err) {
      setStatus('验证已完成，但恢复账号失败：' + (err && err.message || err), 'error');
    }
  }

  async function open(id) {
    let challenge;
    try {
      challenge = await ConsoleAPI.json(`/api/accounts/${id}/verification`);
    } catch (err) {
      showToast(String(err && err.message || err).includes('no recent verification')
        ? '没有该账号最近的验证记录：先让它处理一次请求，再点验证'
        : '获取验证信息失败', 'error');
      return;
    }
    if (!challenge || challenge.sdk !== 'bytedance_captcha' || !challenge.params) {
      showToast('该账号的验证类型暂不支持', 'error');
      return;
    }
    close();
    const frame = document.getElementById('verificationFrame');
    const listener = (event) => {
      if (!active || event.source !== frame.contentWindow || !event.data || event.data.source !== 'doubao-verify') return;
      switch (event.data.type) {
        case 'ready':
          frame.contentWindow.postMessage({ source: 'doubao-verify-host', params: challenge.params }, '*');
          break;
        case 'rendered':
          setStatus('请完成下方的豆包官方验证', '');
          break;
        case 'success':
          solved(id);
          break;
        case 'closed':
          setStatus('验证已关闭，可重新打开再试', 'warn');
          break;
        case 'error':
          setStatus('验证组件出错：' + event.data.detail, 'error');
          break;
      }
    };
    active = { id, listener };
    window.addEventListener('message', listener);
    setStatus('正在加载豆包官方验证组件…', 'info');
    ConsoleUI.modal('verificationModal', true);
    frame.srcdoc = frameDocument();
  }

  globalThis.AccountVerification = { open, close, frameDocument };
})();
