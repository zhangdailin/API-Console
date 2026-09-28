// Shared driver for the browser device-authorization flows exposed by the admin
// console (WorkBuddy, Qoder, Cline).
//
// The server owns the whole transaction: it starts the authorization, keeps the
// private verifier, polls the upstream and persists the resulting credential.
// This module only opens the official page in a popup, watches the server-side
// transaction and reports progress. Passwords and tokens never pass through it.
//
// The channel-specific wrappers share polling, resume-after-refresh and error
// handling so their browser flows cannot drift apart.
globalThis.DeviceAuthLogin = (() => {
  const TERMINAL_MESSAGES = {
    complete: '授权完成，账号已保存',
    failed: '授权失败，请重新发起登录。',
    expired: '授权已超时，请重新发起登录。',
  };

  // Kind refers to whether the authorization URL is a different origin (popup)
  // or the option to open the URL in new tab.
  function create(options) {
    const {
      storageKey,
      basePath,
      popupName,
      statusId,
      buttonId,
      linkId,
      linkTextId,
      enabledId = 'enabled',
      label,
      popupWidth = 560,
      popupHeight = 760,
      pollInterval = 2000,
      errorMessages = {},
      statusPrefix = '正在申请登录会话…',
      authorizedStatus = null,
      openingStatus = '',
      popupBlockedMessage = '登录弹窗被拦截，请允许本站弹窗后重试。',
      insecureMessage = '请使用 HTTPS（本地可用 localhost）打开管理页面后再登录。',
      timeoutMessage = '授权已超时，请重新发起登录。',
      terminalMessages = TERMINAL_MESSAGES,
    } = options;

    let active = null;

    const statusNode = () => document.getElementById(statusId);
    const buttonNode = () => document.getElementById(buttonId);
    function linkNode(createIfMissing = false) {
      if (!linkId) return null;
      let anchor = document.getElementById(linkId);
      if (!anchor && createIfMissing) {
        // Older channel modals have no link markup; add the same fallback
        // without requiring changes to their shared modal template.
        const statusElement = statusNode();
        if (!statusElement?.parentNode) return null;
        anchor = document.createElement('div');
        anchor.id = linkId;
        anchor.className = 'account-import-status';
        anchor.hidden = true;
        const text = document.createElement('a');
        text.id = linkTextId;
        text.target = '_blank';
        text.rel = 'noopener noreferrer';
        anchor.appendChild(text);
        statusElement.insertAdjacentElement('afterend', anchor);
      }
      return anchor;
    }
    const linkTextNode = () => linkTextId ? document.getElementById(linkTextId) : null;

    function status(text, kind = 'info') {
      const node = statusNode();
      if (!node) return;
      node.hidden = !text;
      node.classList.toggle('is-error', kind === 'error');
      node.classList.toggle('is-active', kind === 'info' && Boolean(text));
      node.textContent = text || '';
    }

    function setButton(disabled) {
      const button = buttonNode();
      if (button) button.disabled = disabled;
    }

    // The authorization URL is shown next to the popup: a popup blocker, a
    // headless browser or a remote session must not make the flow impossible.
    function setLink(url) {
      const value = String(url || '').trim();
      const anchor = linkNode(Boolean(value));
      if (!anchor) return;
      anchor.hidden = !value;
      const text = linkTextNode();
      if (!value) {
        anchor.removeAttribute('href');
        if (text) text.removeAttribute('href');
        return;
      }
      anchor.href = value;
      if (text) {
        text.href = value;
        text.textContent = value;
      }
    }

    function clearStoredLogin() {
      try {
        window.localStorage.removeItem(storageKey);
      } catch (_) {
        /* storage may be unavailable; the in-page timer still guards the flow */
      }
    }

    function storeLogin(login) {
      try {
        window.localStorage.setItem(storageKey, JSON.stringify({
          loginId: login.loginId,
          expiresAt: login.expiresAt,
        }));
      } catch (_) {
        /* ignore */
      }
    }

    function readStoredLogin() {
      try {
        const raw = window.localStorage.getItem(storageKey);
        if (!raw) return null;
        const parsed = JSON.parse(raw);
        if (!parsed || typeof parsed.loginId !== 'string' || !parsed.loginId) return null;
        if (typeof parsed.expiresAt === 'number' && Date.now() >= parsed.expiresAt) return null;
        return parsed;
      } catch (_) {
        return null;
      }
    }

    function abortActive(silent) {
      if (!active) return;
      const login = active;
      active = null;
      clearInterval(login.timer);
      if (login.controller) login.controller.abort();
      if (login.popup && !login.popup.closed) login.popup.close();
      if (!silent && login.loginId) {
        fetch(`${basePath}/${encodeURIComponent(login.loginId)}`, {
          method: 'DELETE',
          credentials: 'same-origin',
        }).catch(() => {});
      }
      clearStoredLogin();
      setButton(false);
      setLink('');
    }

    function finish(message, kind) {
      abortActive(true);
      status(message, kind || 'info');
      if (kind === 'error') {
        showToast(message, 'error');
        return;
      }
      showToast(message, 'success');
      closeModal();
      loadAccounts();
    }

    function fail(message) {
      if (active) {
        const loginId = active.loginId;
        active.loginId = '';
        if (loginId) {
          fetch(`${basePath}/${encodeURIComponent(loginId)}`, {
            method: 'DELETE',
            credentials: 'same-origin',
          }).catch(() => {});
        }
      }
      finish(message, 'error');
    }

    async function poll(login) {
      if (active !== login || !login.loginId) return;
      try {
        const response = await fetch(`${basePath}/${encodeURIComponent(login.loginId)}`, {
          credentials: 'same-origin',
          signal: login.controller.signal,
        });
        if (active !== login) return;
        if (response.status === 404) {
          fail(`${label} 登录会话已失效，请重新发起登录。`);
          return;
        }
        if (response.status === 409 || response.status === 200) {
          // fall through: 200 is the normal case, 409 is reported body-side
        }
        if (!response.ok) return;
        const result = await response.json();
        if (active !== login) return;
        const state = String(result.status || '');
        if (state === 'complete') {
          finish(result.message || terminalMessages.complete);
          return;
        }
        if (state === 'failed') {
          fail(result.message || terminalMessages.failed);
          return;
        }
        if (state === 'expired') {
          fail(result.message || terminalMessages.expired);
          return;
        }
        if (login.popup && login.popup.closed && !login.resumed) {
          login.closedAt ||= Date.now();
          if (Date.now() - login.closedAt >= 3000) {
            fail(`${label} 登录窗口已关闭，尚未完成授权。`);
          }
        }
      } catch (err) {
        if (err && err.name === 'AbortError') return;
        // Transient network errors are retried on the next tick.
      }
    }

    function messageForError(payload, fallback) {
      const code = String((payload && payload.code) || '').trim();
      if (code && errorMessages[code]) return errorMessages[code];
      const detail = String((payload && (payload.error || payload.message)) || '').trim();
      if (detail) return `${fallback}（${detail}）`;
      return fallback;
    }

    async function readErrorPayload(response) {
      try {
        const text = await response.text();
        if (!text) return null;
        try {
          return JSON.parse(text);
        } catch (_) {
          return { error: text.slice(0, 200) };
        }
      } catch (_) {
        return null;
      }
    }

    function begin(enabled) {
      // The fetch is issued synchronously inside the click gesture so the popup
      // below is still treated as user-initiated by the browser.
      return fetch(basePath, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: enabled !== false }),
      });
    }

    function openPopup(login, authURL) {
      if (!login.popup) return false;
      try {
        login.popup.location.replace(authURL);
        return true;
      } catch (_) {
        try {
          login.popup.location.href = authURL;
          return true;
        } catch (_) {
          return false;
        }
      }
    }

    function start() {
      if (active) return;
      if (!window.isSecureContext) {
        status(insecureMessage, 'error');
        return;
      }
      setButton(true);
      setLink('');
      status(statusPrefix);

      const login = {
        loginId: '',
        expiresAt: 0,
        popup: null,
        timer: null,
        controller: new AbortController(),
        closedAt: 0,
      };
      active = login;

      // Reserve the popup window during the click gesture; it is navigated once
      // the server returns the official authorization URL.
      try {
        login.popup = window.open('about:blank', popupName, `popup,width=${popupWidth},height=${popupHeight}`);
      } catch (_) {
        login.popup = null;
      }

      (async () => {
        try {
          // Re-attach to a session that outlived a page refresh before starting
          // a new one; the server-side transaction is still pollable.
          let session = readStoredLogin();
          login.resumed = Boolean(session);
          if (!session) {
            const response = await begin(document.getElementById(enabledId)?.checked !== false);
            if (!response.ok) {
              const payload = await readErrorPayload(response);
              if (active !== login) return;
              fail(messageForError(payload, `发起 ${label} 登录失败（HTTP ${response.status}）`));
              return;
            }
            session = await response.json();
          }
          if (active !== login) return;
          const loginId = String(session?.id || session?.loginId || '').trim();
          const authURL = String(session?.verification_uri_complete || '').trim();
          if (!loginId) throw new Error('invalid_session');
          login.loginId = loginId;
          login.expiresAt = Date.parse(String(session.expires_at || '')) || Date.now() + 15 * 60 * 1000;
          storeLogin(login);
          if (authURL) {
            setLink(authURL);
            if (!openPopup(login, authURL)) {
              // No popup window: the flow still works through the visible link.
              status(`${label} 官方授权页面未能自动打开，请点击上方链接完成授权。`);
            } else {
              status(openingStatus || `请在 ${label} 官方页面完成登录与授权，本窗口会自动接管。`);
            }
          } else {
            // A resumed transaction cannot re-open the original tab, but the
            // browser step may already be finished.
            status(authorizedStatus || `正在确认 ${label} 授权结果…`);
          }
          login.timer = setInterval(() => {
            if (active !== login) return;
            if (Date.now() >= login.expiresAt) {
              fail(timeoutMessage);
              return;
            }
            poll(login);
          }, pollInterval);
          poll(login);
        } catch (err) {
          if (active !== login) return;
          const offline = err instanceof TypeError;
          fail(offline
            ? '无法连接本服务的登录接口，请确认服务正在运行且页面未被反向代理拦截。'
            : `发起 ${label} 登录时发生异常，请刷新页面后重试。`);
        }
      })();
    }

    function stop() {
      abortActive(false);
      status('');
    }

    window.addEventListener('pagehide', () => {
      // Deliberately keep the server-side session: a refresh must be able to
      // re-attach, and the transaction expires on its own.
      if (!active) return;
      clearInterval(active.timer);
      active = null;
      setButton(false);
    });

    return { start, stop };
  }

  return { create };
})();
