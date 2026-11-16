// Warp official browser login uses the shared server-owned device transaction.
// Only the transaction ID and expiry are persisted by the driver; account
// credentials remain on the server and are never read or copied by this page.
globalThis.WarpLogin = (() => {
  const driver = globalThis.DeviceAuthLogin.create({
    storageKey: 'warp_login_v1',
    basePath: '/api/warp/login',
    popupName: 'warp-login',
    statusId: 'warpLoginStatus',
    buttonId: 'warpLoginButton',
    linkId: 'warpLoginLink',
    linkTextId: 'warpLoginLinkText',
    codeId: 'warpLoginCode',
    label: 'Warp',
    statusPrefix: 'Starting Warp browser login…',
    openingStatus: 'Complete login on the official Warp page. This window will save the account automatically.',
    authorizedStatus: 'Checking the Warp authorization result…',
    timeoutMessage: 'Warp authorization expired. Start a new login.',
    errorMessages: {
      upstream_unreachable: 'The server cannot reach the official Warp login service. Check server connectivity and retry.',
      upstream_rejected: 'The official Warp login service rejected this session. Start a new login.',
    },
    // Compare exact HTTPS origins, not hostname suffixes. Alternate ports,
    // lookalike hosts and embedded user information are never navigable.
    isAllowedURL: (url) => !url.username && !url.password &&
      (url.origin === 'https://app.warp.dev' || url.origin === 'https://warp.dev'),
  });

  return { start: driver.start, stop: driver.stop, reset: driver.reset };
})();
