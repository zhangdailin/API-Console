// Cline (api.cline.bot) official WorkOS device-authorization login.
//
// The channel is OAuth-only: the console starts a WorkOS device transaction,
// the server exchanges it at the Cline API, and only the resulting account is
// stored. There is no manual credential field anywhere in this flow, and no
// token ever passes through this file.
globalThis.ClineLogin = (() => {
  // Server error codes map to operator-actionable messages. The server never
  // echoes credentials, so these strings are safe to surface verbatim.
  const ERROR_MESSAGES = {
    upstream_unreachable: '服务器无法访问 WorkOS 授权端点（api.workos.com）。请检查服务器的出网/代理设置后重试。',
    upstream_rejected: 'WorkOS 拒绝了本次登录会话，请稍后重试。',
  };

  const driver = globalThis.DeviceAuthLogin.create({
    storageKey: 'cline_login_v1',
    basePath: '/api/cline/login',
    popupName: 'cline-login',
    statusId: 'clineLoginStatus',
    buttonId: 'clineLoginButton',
    linkId: 'clineLoginLink',
    linkTextId: 'clineLoginLinkText',
    label: 'Cline',
    errorMessages: ERROR_MESSAGES,
    statusPrefix: '正在申请 Cline 登录会话…',
    openingStatus: '请在 WorkOS 官方页面完成登录与授权，本窗口会自动接管。',
    authorizedStatus: '正在确认 Cline 授权结果…', popupBlockedMessage: 'Cline 登录弹窗被拦截，请允许本站弹窗后重试。', timeoutMessage: 'Cline 授权已超时，请重新发起登录。' });

  return { start: driver.start, stop: driver.stop };
})();
