// WorkBuddy international (www.workbuddy.ai) official browser login.
// The server owns the transaction and tokens; the shared browser driver handles
// popup opening, authorization progress and resume-after-refresh.
globalThis.WorkBuddyLogin = (() => {
  const ERROR_MESSAGES = {
    upstream_unreachable: '服务器无法访问 www.workbuddy.ai。请检查服务器的出网/代理设置后重试。',
    upstream_rejected: 'www.workbuddy.ai 拒绝了本次登录会话，请稍后重试。',
  };

  const driver = globalThis.DeviceAuthLogin.create({
    storageKey: 'workbuddy_login_v1',
    basePath: '/api/workbuddy/login',
    popupName: 'workbuddy-login',
    statusId: 'workbuddyLoginStatus',
    buttonId: 'workbuddyLoginButton',
    linkId: 'workbuddyLoginLink',
    linkTextId: 'workbuddyLoginLinkText',
    label: 'WorkBuddy',
    errorMessages: ERROR_MESSAGES,
    statusPrefix: '正在申请 WorkBuddy 登录会话…',
    openingStatus: '请在 WorkBuddy 官方页面完成登录与授权，本窗口会自动接管。',
    authorizedStatus: '正在确认 WorkBuddy 授权结果…',
    popupBlockedMessage: 'WorkBuddy 登录弹窗被拦截，请允许本站弹窗后重试。',
    timeoutMessage: 'WorkBuddy 授权已超时，请重新发起登录。',
    terminalMessages: { complete: 'WorkBuddy 官方登录完成，账号已保存', failed: 'WorkBuddy 授权失败，请重新发起登录。', expired: 'WorkBuddy 授权已超时，请重新发起登录。' },
  });

  return { start: driver.start, stop: driver.stop };
})();
