// Qoder (qoder.com) official device-authorization login.
//
// The channel is OAuth-only: the console starts a device transaction and polls
// the upstream for the token pair. There is no personal-access-token field
// anywhere in this flow, and no credential ever passes through this file.
globalThis.QoderLogin = (() => {
  // Server error codes map to operator-actionable messages. The server never
  // echoes credentials, so these strings are safe to surface verbatim.
  const ERROR_MESSAGES = {
    upstream_unreachable: '服务器无法访问 Qoder 授权端点。请检查服务器的出网/代理设置后重试。',
    upstream_rejected: 'Qoder 拒绝了本次登录会话，请稍后重试。',
  };

  const driver = globalThis.DeviceAuthLogin.create({
    storageKey: 'qoder_login_v1',
    basePath: '/api/qoder/login',
    popupName: 'qoder-login',
    statusId: 'qoderLoginStatus',
    buttonId: 'qoderLoginButton',
    linkId: 'qoderLoginLink',
    linkTextId: 'qoderLoginLinkText',
    label: 'Qoder',
    errorMessages: ERROR_MESSAGES,
    statusPrefix: '正在申请 Qoder 登录会话…',
    openingStatus: '请在 Qoder 官方页面完成登录与授权，本窗口会自动接管。',
    authorizedStatus: '正在确认 Qoder 授权结果…', popupBlockedMessage: 'Qoder 登录弹窗被拦截，请允许本站弹窗后重试。', timeoutMessage: 'Qoder 授权已超时，请重新发起登录。' });

  return { start: driver.start, stop: driver.stop };
})();
