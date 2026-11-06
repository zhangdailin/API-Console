(() => {
  const LIVEKIT_CLIENT_VERSIONS = {
    stable: {
      label: "2.7.3",
      integrity: "sha384-Ci1eiIh2+SkRFqqfxfXgWEo0r6h30tVLpq9wIjI3lkG2VpU9MN/aNZdmQy0t3+9X",
      url: "https://cdn.jsdelivr.net/npm/livekit-client@2.7.3/dist/livekit-client.umd.min.js",
    },
  };


  const toolAuthState = { mode: "admin", apiKey: "" };
  function toolAuthHeaders(headers = {}) {
    return toolAuthState.mode === "client" && toolAuthState.apiKey
      ? { ...headers, Authorization: `Bearer ${toolAuthState.apiKey}` }
      : headers;
  }
  function toolInferencePrefix() { return toolAuthState.mode === "client" ? "/grok/v1" : "/api/grok/tools/v1"; }
  function toolHistoryScope() {
    if (!toolAuthState.apiKey) return "admin";
    let hash = 2166136261;
    for (const char of toolAuthState.apiKey) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619);
    return `key-${(hash >>> 0).toString(16)}`;
  }

  if (typeof window !== "undefined") {
    window.GrokToolRequest = Object.freeze({ headers: (headers = {}) => toolAuthHeaders(headers), prefix: () => toolInferencePrefix() });
  }

  const cacheOnlineState = {
    selectedTokens: new Set(),
    accounts: [],
    details: [],
    online: {},
    onlineScope: "none",
    accountMap: new Map(),
    detailMap: new Map(),
  };

  const cacheBatchState = {
    running: false,
    action: "",
    taskID: "",
    total: 0,
    processed: 0,
    statusText: "空闲",
    eventSource: null,
  };

  const videoState = {
    taskID: "",
    running: false,
    imageFileID: "",
    referenceFileID: "",
    sourceFileID: "",
    startAt: 0,
    elapsedTimer: null,
    lastProgress: 0,
    currentPreviewItem: null,
    previewCount: 0,
    pollTimer: null,
    objectURLs: new Set(),
  };

  const voiceState = {
    running: false,
    stopping: false,
    room: null,
    visualizerTimer: null,
    outputMuted: false,
    reconnecting: false,
  };
  const grokLazyState = {
    imagineReady: false,
    cacheReady: false,
    livekitPromise: null,
  };

  const chatState = {
    sessions: [],
    activeId: "",
    sending: false,
    abortController: null,
    renderFrame: 0,
    sidebarOpen: false,
    model: "",
    models: [],
    capabilities: { chat: false, imagine: false, video: false, voice: false },
    modelsLoaded: false,
    requestGeneration: 0,
    persistenceTimer: null,
  };
  // The cache endpoint reports which accounts its online figures cover.
  const ONLINE_SCOPE_LABELS = { none: "未加载", single: "单个账号", selected: "选中账号", all: "全部账号" };
  const grokCapabilityState = {
    loaded: false,
    failed: false,
    counts: { build: 0, web: 0 },
  };
  const chatStorageKey = "grok_tools_chat_sessions_v1";
  const chatSidebarStateKey = "grok_tools_chat_sidebar_collapsed";
  const grokToolsUIStorageKey = "grok_tools_ui_v1";
  const i18nMap = {
    "common.notConnected": "未连接",
    // A generator that is not running is idle; 未连接 is for live sessions.
    "common.idle": "空闲",
    "common.connecting": "连接中...",
    "common.generating": "生成中",
    "common.done": "已完成",
    "common.connectionError": "连接中断",
    "common.createTaskFailed": "创建任务失败",
    "common.enterPrompt": "请输入 Prompt",
    "video.alreadyRunning": "Video 任务已在运行中",
    "video.enterPrompt": "请输入 Video Prompt",
    "video.superResolution": "超分辨率处理中...",
    "video.downloadFailed": "视频下载失败",
    "video.taskEmpty": "创建任务失败：空 task_id",
    "video.startFailed": "启动失败",
    "video.generatingPlaceholder": "视频生成中...",
    "voice.alreadyRunning": "Voice 会话已在运行中",
    "voice.tokenUnavailable": "voice token unavailable",
    "voice.connected": "已连接",
    "voice.stopped": "未连接",
    "voice.connectFailed": "连接失败",
  };

  function t(key, vars) {
    let text = i18nMap[key] || key;
    if (vars && typeof vars === "object") {
      Object.keys(vars).forEach((name) => {
        text = text.replace(new RegExp(`\\{${name}\\}`, "g"), String(vars[name]));
      });
    }
    return text;
  }

  function handleUnauthorized(res) {
    if (!res || res.status !== 401) return false;
    // Only the admin session middleware answers a bare plain-text 401. A JSON
    // 401 comes from a Grok handler — for example every upstream account needing
    // re-login, or a rejected client key — and is a tool failure, not an expired
    // admin session. Redirecting on that logged the operator out of a page
    // that was still authenticated, which made the tools look unusable.
    const contentType = String(res.headers?.get?.("Content-Type") || "").toLowerCase();
    const sessionExpired = contentType.includes("text/plain");
    if (sessionExpired) {
      window.location.href = "/admin/login.html?next=" + encodeURIComponent("/admin/?tab=grok-tools");
    }
    return true;
  }

  function currentGrokToolTab() {
    return String(document.querySelector("#grokToolsTabs .tab-item.active")?.dataset.tab || "imagine").toLowerCase();
  }

  function hasGrokCapability(tab) {
    if (!grokCapabilityState.loaded || grokCapabilityState.failed) return true;
    const counts = grokCapabilityState.counts;
    if (tab === "cache") return true;
    // LiveKit uses the Web account's token endpoint, independently of the
    // inference model catalog and its realtime/TTS/STT capabilities.
    if (tab === "voice") return counts.web > 0;
    if (chatState.modelsLoaded) return chatState.capabilities[tab] === true;
    if (tab === "chat") return counts.build + counts.web > 0;
    return chatState.capabilities[tab] || counts.web > 0;
  }

  function grokAccountSummary() {
    const counts = grokCapabilityState.counts;
    const parts = [];
    if (counts.build) parts.push(`${counts.build} 个 Build`);
    if (counts.web) parts.push(`${counts.web} 个 Web`);
    return parts.length ? parts.join("、") : "没有可用的 Grok 账号";
  }

  function applyGrokCapabilityControls() {
    if (!grokCapabilityState.loaded || grokCapabilityState.failed) return;
    const controls = {
      chat: document.getElementById("grokSendBtn"),
      imagine: document.getElementById("imagineStartBtn"),
      video: document.getElementById("videoStartBtn"),
      voice: document.getElementById("voiceStartBtn"),
    };
    Object.entries(controls).forEach(([tab, control]) => {
      if (!control) return;
      const blocked = !hasGrokCapability(tab);
      if (!(tab === "chat" && chatState.sending) && !(tab === "video" && videoState.running) && !(tab === "voice" && voiceState.running)) {
        control.disabled = blocked;
      }
      control.dataset.capabilityBlocked = blocked ? "true" : "false";
      if (blocked) control.title = `${tab === "chat" ? "对话" : tab === "imagine" ? "图片生成" : tab === "video" ? "视频生成" : "语音对话"}缺少可用账号`;
      else control.removeAttribute("title");
    });
  }

  function updateGrokCapabilityPresentation(tab) {
    const banner = document.getElementById("grokCapabilityBanner");
    const title = document.getElementById("grokCapabilityTitle");
    const text = document.getElementById("grokCapabilityText");
    const action = document.getElementById("grokCapabilityAction");
    if (!banner || !title || !text || !action) return;
    const labels = { chat: "对话", imagine: "图片生成", video: "视频生成", voice: "语音对话", cache: "缓存管理" };
    banner.classList.remove("is-loading", "is-ready", "is-warning");
    action.classList.add("hidden");
    if (!grokCapabilityState.loaded) {
      banner.classList.add("is-loading");
      title.textContent = "正在检查可用能力";
      text.textContent = "读取账号和模型状态…";
      return;
    }
    if (grokCapabilityState.failed) {
      banner.classList.add("is-warning");
      title.textContent = "暂时无法读取能力状态";
      text.textContent = "你仍可继续操作；如请求失败，请检查账号管理页。";
      action.classList.remove("hidden");
      return;
    }
    const summary = grokAccountSummary();
    if (tab === "cache") {
      banner.classList.add("is-ready");
      title.textContent = "本地缓存可用";
      text.textContent = grokCapabilityState.counts.web > 0 ? `${summary}；可同时管理在线资产。` : `${summary}；在线资产需要 Grok Web 账号。`;
      return;
    }
    if (hasGrokCapability(tab)) {
      banner.classList.add("is-ready");
      title.textContent = `${labels[tab] || "当前功能"}已就绪`;
      text.textContent = `可用账号：${summary}。`;
      return;
    }
    banner.classList.add("is-warning");
    if (grokCapabilityState.counts.web > 0 && !grokCapabilityState.webSynced) {
      // The account exists; what is missing is a successful grok.com quota
      // read, which is what publishes the Web models.
      title.textContent = "Grok Web 账号尚未同步成功";
      const reason = grokCapabilityState.webSyncError ? `原因：${grokCapabilityState.webSyncError}。` : "";
      text.textContent = `已有 ${grokCapabilityState.counts.web} 个 Web 账号，但还没有成功读取 grok.com 额度，Web 模型暂未发布。${reason}在账号管理点「同步 SSO 额度」重试，完成后刷新本页。`;
      action.textContent = "去同步账号";
      action.classList.remove("hidden");
      return;
    }
    title.textContent = tab === "chat" ? "对话需要可用的 Grok 账号" : `${labels[tab]}需要 Grok Web 账号`;
    text.textContent = `当前状态：${summary}。添加匹配账号后即可使用此功能。`;
    action.textContent = "添加所需账号";
    action.classList.remove("hidden");
  }

  async function loadGrokCapabilities() {
    try {
      const res = await fetch("/api/grok/availability");
      if (handleUnauthorized(res)) return;
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const payload = await res.json();
      const rawCounts = payload && payload.counts;
      const counts = {};
      for (const provider of ["build", "web"]) {
        const count = Number(rawCounts && rawCounts[provider]);
        if (!Number.isFinite(count) || count < 0 || !Number.isInteger(count)) {
          throw new Error(`invalid ${provider} account count`);
        }
        counts[provider] = count;
      }
      grokCapabilityState.counts = counts;
      grokCapabilityState.webSynced = Number(payload?.web_synced || 0);
      grokCapabilityState.webSyncError = String(payload?.web_sync_error || "");
      grokCapabilityState.failed = false;
    } catch (err) {
      grokCapabilityState.failed = true;
      console.debug("Failed to load Grok capabilities:", err);
    }
    grokCapabilityState.loaded = true;
    ["chat", "imagine", "video", "voice", "cache"].forEach((tab) => {
      const badge = document.querySelector(`[data-capability="${tab}"]`);
      if (!badge) return;
      const ready = tab === "cache" || hasGrokCapability(tab);
      badge.textContent = grokCapabilityState.failed ? "未知" : tab === "cache" ? "本地" : ready ? "可用" : "需账号";
      badge.classList.toggle("is-ready", ready && !grokCapabilityState.failed);
      badge.classList.toggle("is-unavailable", !ready || grokCapabilityState.failed);
    });
    applyGrokCapabilityControls();
    updateGrokCapabilityPresentation(currentGrokToolTab());
  }

  function formatBytes(bytes) {
    const num = Number(bytes || 0);
    if (!Number.isFinite(num) || num <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let value = num;
    let idx = 0;
    while (value >= 1024 && idx < units.length - 1) {
      value /= 1024;
      idx++;
    }
    return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[idx]}`;
  }

  function formatDateTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    return new Date(num).toLocaleString();
  }

  function relativeTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    const diff = Math.max(0, Math.round((Date.now() - num) / 1000));
    if (diff < 60) return "刚刚";
    if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
    if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
    return `${Math.floor(diff / 86400)} 天前`;
  }

  function loadGrokToolsUIState() {
    try {
      const raw = localStorage.getItem(grokToolsUIStorageKey);
      if (!raw) return {};
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (err) {
      return {};
    }
  }

  function saveGrokToolsUIState(patch) {
    try {
      const current = loadGrokToolsUIState();
      localStorage.setItem(grokToolsUIStorageKey, JSON.stringify({ ...current, ...patch }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function setChatSendButtonState(sending) {
    const btn = document.getElementById("grokSendBtn");
    if (!btn) return;
    btn.disabled = false;
    if (sending) {
      btn.title = "停止生成";
      btn.innerHTML = `
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <rect x="6" y="6" width="12" height="12" rx="2"></rect>
        </svg>
      `;
      return;
    }
    btn.title = "发送";
    btn.innerHTML = `
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
        <path d="M22 2L11 13"></path>
        <path d="M22 2L15 22L11 13L2 9L22 2Z"></path>
      </svg>
    `;
  }

  function setChatSidebarCollapsed(collapsed) {
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    layout.classList.toggle("collapsed", collapsed);
    try {
      localStorage.setItem(chatSidebarStateKey, collapsed ? "1" : "0");
    } catch (err) {
      // ignore storage failures
    }
  }

  function closeChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = false;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.remove("open");
      sidebar?.classList.remove("show");
      overlay?.classList.remove("open");
      overlay?.classList.remove("show");
      return;
    }
    setChatSidebarCollapsed(true);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "inline-flex";
  }

  function openChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = true;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.add("open");
      sidebar?.classList.add("show");
      overlay?.classList.add("open");
      overlay?.classList.add("show");
      return;
    }
    setChatSidebarCollapsed(false);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "none";
  }

  function toggleChatSidebar() {
    if (isMobileChatSidebar()) {
      const sidebar = document.getElementById("grokChatSidebar");
      if (sidebar?.classList.contains("open") || sidebar?.classList.contains("show")) {
        closeChatSidebar();
      } else {
        openChatSidebar();
      }
      return;
    }
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    if (layout.classList.contains("collapsed")) {
      openChatSidebar();
    } else {
      closeChatSidebar();
    }
  }

  function isMobileChatSidebar() {
    return window.matchMedia("(max-width: 1024px)").matches;
  }

  function syncChatSidebarState() {
    if (isMobileChatSidebar()) {
      if (chatState.sidebarOpen) {
        openChatSidebar();
      } else {
        closeChatSidebar();
      }
      return;
    }
    const sidebar = document.getElementById("grokChatSidebar");
    const overlay = document.getElementById("grokChatSidebarOverlay");
    sidebar?.classList.remove("open");
    sidebar?.classList.remove("show");
    overlay?.classList.remove("open");
    overlay?.classList.remove("show");
    let collapsed = false;
    try {
      collapsed = localStorage.getItem(chatSidebarStateKey) === "1";
    } catch (err) {
      collapsed = false;
    }
    setChatSidebarCollapsed(collapsed);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = collapsed ? "inline-flex" : "none";
  }

  function resolveOnlineStatusText(status) {
    const raw = String(status || "").trim();
    if (raw === "ok") return "连接正常";
    if (raw === "not_loaded") return "未加载";
    if (raw === "no_token") return "无可用 Token";
    if (!raw) return "未知";
    return raw;
  }

  function normalizeOnlineToken(raw) {
    const token = String(raw || "").trim();
    if (!token) return "";
    if (!token.includes("sso=")) return token;
    const idx = token.indexOf("sso=");
    const tail = token.slice(idx + 4);
    const semi = tail.indexOf(";");
    return (semi >= 0 ? tail.slice(0, semi) : tail).trim();
  }

  function formatTokenMask(token) {
    const raw = String(token || "").trim();
    if (!raw) return "";
    if (raw.length <= 24) return raw;
    return `${raw.slice(0, 8)}...${raw.slice(-16)}`;
  }

  function toNumberOrZero(value) {
    const n = Number(value || 0);
    return Number.isFinite(n) ? n : 0;
  }

  function closeCacheBatchStream() {
    const es = cacheBatchState.eventSource;
    cacheBatchState.eventSource = null;
    if (!es) return;
    try {
      es.close();
    } catch (err) {
      // ignore
    }
  }

  function updateCacheBatchUI() {
    const statusEl = document.getElementById("cacheOnlineBatchStatus");
    const progressEl = document.getElementById("cacheOnlineBatchProgress");
    const barEl = document.getElementById("cacheOnlineBatchBar");
    const cancelBtn = document.getElementById("cacheOnlineBatchCancelBtn");
    const loadSelectedBtn = document.getElementById("cacheOnlineLoadSelectedBtn");
    const loadAllBtn = document.getElementById("cacheOnlineLoadAllBtn");
    const clearSelectedBtn = document.getElementById("cacheOnlineClearSelectedBtn");

    const total = Math.max(0, Math.floor(toNumberOrZero(cacheBatchState.total)));
    const processed = Math.max(0, Math.floor(toNumberOrZero(cacheBatchState.processed)));
    const safeTotal = total > 0 ? total : 0;
    const safeProcessed = total > 0 ? Math.min(processed, total) : 0;
    const percent = safeTotal > 0 ? Math.floor((safeProcessed / safeTotal) * 100) : 0;

    if (statusEl) {
      const text = String(cacheBatchState.statusText || "").trim();
      statusEl.textContent = text || (cacheBatchState.running ? "运行中" : "空闲");
    }
    if (progressEl) progressEl.textContent = `${safeProcessed}/${safeTotal}`;
    if (barEl) barEl.value = percent;
    if (cancelBtn) {
      cancelBtn.style.display = cacheBatchState.running ? "inline-flex" : "none";
      cancelBtn.disabled = !cacheBatchState.running;
    }
    if (loadSelectedBtn) loadSelectedBtn.disabled = cacheBatchState.running;
    if (loadAllBtn) loadAllBtn.disabled = cacheBatchState.running;
    if (clearSelectedBtn) clearSelectedBtn.disabled = cacheBatchState.running;
  }

  function applyCacheBatchProgress(msg) {
    if (!msg || typeof msg !== "object") return;
    if (typeof msg.total === "number" && Number.isFinite(msg.total)) {
      cacheBatchState.total = Math.max(0, Math.floor(msg.total));
    }
    if (typeof msg.processed === "number" && Number.isFinite(msg.processed)) {
      cacheBatchState.processed = Math.max(0, Math.floor(msg.processed));
    } else if (typeof msg.done === "number" && Number.isFinite(msg.done)) {
      cacheBatchState.processed = Math.max(0, Math.floor(msg.done));
    }
    if (cacheBatchState.total > 0 && cacheBatchState.processed > cacheBatchState.total) {
      cacheBatchState.total = cacheBatchState.processed;
    }
    updateCacheBatchUI();
  }

  function beginCacheBatch(action, taskID, total, statusText) {
    closeCacheBatchStream();
    cacheBatchState.running = true;
    cacheBatchState.action = String(action || "").trim();
    cacheBatchState.taskID = String(taskID || "").trim();
    cacheBatchState.total = Math.max(0, Math.floor(toNumberOrZero(total)));
    cacheBatchState.processed = 0;
    cacheBatchState.statusText = String(statusText || "运行中");
    updateCacheBatchUI();
  }

  function finishCacheBatch(statusText) {
    cacheBatchState.running = false;
    cacheBatchState.action = "";
    cacheBatchState.taskID = "";
    cacheBatchState.statusText = String(statusText || "空闲");
    closeCacheBatchStream();
    updateCacheBatchUI();
  }

  function openCacheBatchStream(taskID, handlers = {}) {
    const cleanTaskID = String(taskID || "").trim();
    if (!cleanTaskID) throw new Error("empty task_id");

    const url = `/api/grok/admin/batch/${encodeURIComponent(cleanTaskID)}/stream?t=${Date.now()}`;
    const es = new EventSource(url);
    cacheBatchState.eventSource = es;
    let ended = false;

    const doneOnce = (fn) => {
      if (ended) return;
      ended = true;
      closeCacheBatchStream();
      if (typeof fn === "function") {
        Promise.resolve()
          .then(() => fn())
          .catch((err) => {
            showToast(err?.message || String(err || "批量任务处理失败"), "error");
          });
      }
    };

    es.onmessage = (event) => {
      let msg = null;
      try {
        msg = JSON.parse(event.data);
      } catch (err) {
        return;
      }
      if (!msg || typeof msg !== "object") return;
      const msgTaskID = String(msg.task_id || "").trim();
      if (msgTaskID && msgTaskID !== cleanTaskID) return;

      applyCacheBatchProgress(msg);
      const type = String(msg.type || "").trim().toLowerCase();
      if (type === "snapshot" || type === "progress") {
        return;
      }
      if (type === "done") {
        doneOnce(() => {
          if (typeof handlers.onDone === "function") {
            handlers.onDone(msg);
          }
        });
        return;
      }
      if (type === "cancelled") {
        doneOnce(() => {
          if (typeof handlers.onCancelled === "function") {
            handlers.onCancelled(msg);
          }
        });
        return;
      }
      if (type === "error") {
        doneOnce(() => {
          if (typeof handlers.onError === "function") {
            handlers.onError(String(msg.error || "unknown error"), msg);
          }
        });
      }
    };

    es.onerror = () => {
      doneOnce(() => {
        if (typeof handlers.onError === "function") {
          handlers.onError("连接中断", null);
        }
      });
    };
  }


  function createChatSession() {
    return {
      id: `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`,
      title: "新会话",
      isDefaultTitle: true,
      createdAt: Date.now(),
      updatedAt: Date.now(),
      messages: [],
      model: chatState.model,
      promptCacheKey: createPromptCacheKey(),
      reasoningEffort: "",
      webSearch: false,
      xSearch: false,
    };
  }

  function createPromptCacheKey() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === "function") return `grok-tools-${globalThis.crypto.randomUUID()}`;
    return `grok-tools-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }

  function normalizeAssistantMessage(message) {
    if (message.role !== "assistant" || !/<think>/i.test(String(message.content || ""))) return message;
    // New-format messages store reasoning separately; content is the pure answer and must never be re-scanned.
    if (String(message.reasoning || "").trim()) return message;
    // Legacy history always led with a reasoning block. Mid-body think tags are real content (e.g. code examples).
    const raw = String(message.content || "");
    if (!/^\s*<think>/i.test(raw)) return message;
    const thoughts = [];
    const content = raw.replace(/<think>([\s\S]*?)(?:<\/think>|$)/gi, (_, text) => {
      thoughts.push(text.trim());
      return "";
    });
    return { ...message, content: content.trim(), reasoning: [message.reasoning || "", ...thoughts].filter(Boolean).join("\n\n") };
  }

  function assistantDisplay(message) {
    const reasoning = String(message.reasoning || "");
    return (reasoning ? `<think>${reasoning}</think>` : "") + String(message.content || "");
  }

  function renderToolActivity(tools) {
    return (tools || []).map((tool) => `<div class="tool-activity">${escapeHtml(tool.name || tool.type || "工具")} · ${escapeHtml(tool.status || "in_progress")}${tool.detail ? `<pre>${escapeHtml(tool.detail)}</pre>` : ""}</div>`).join("");
  }

  const chatSessionLimit = 50;
  const chatStorageByteLimit = 4 * 1024 * 1024;
  const chatPersistenceDelay = 250;

  function scopedChatStorageKey() {
    return `${chatStorageKey}:${toolHistoryScope()}`;
  }

  function persistedChatSession(session) {
    return {
      ...session,
      messages: Array.isArray(session.messages)
        ? session.messages.map((msg) => ({
            ...msg,
            attachment: msg?.attachment
              ? {
                  name: String(msg.attachment.name || ""),
                  type: String(msg.attachment.type || ""),
                  dataUrl: msg.attachment.dataUrl,
                }
              : undefined,
          }))
        : [],
    };
  }

  function chatPayloadJSON(sessions) {
    return JSON.stringify({ activeId: chatState.activeId, model: chatState.model, sessions });
  }

  function storageByteLength(value) {
    if (typeof TextEncoder === "function") return new TextEncoder().encode(value).byteLength;
    return unescape(encodeURIComponent(value)).length;
  }

  function boundedChatPayload() {
    const sessions = chatState.sessions.filter(Boolean).map(persistedChatSession);
    sessions.sort((a, b) => Number(b.updatedAt || b.createdAt || 0) - Number(a.updatedAt || a.createdAt || 0));
    if (sessions.length > chatSessionLimit) sessions.length = chatSessionLimit;
    let payload = chatPayloadJSON(sessions);
    while (sessions.length && storageByteLength(payload) > chatStorageByteLimit) {
      sessions.pop();
      payload = chatPayloadJSON(sessions);
    }
    return { payload, sessions };
  }

  function persistChatSessionsNow() {
    if (chatState.persistenceTimer) {
      clearTimeout(chatState.persistenceTimer);
      chatState.persistenceTimer = null;
    }
    try {
      const bounded = boundedChatPayload();
      // Keep memory and disk subject to the same limits, so an evicted session
      // cannot reappear during the next save in this tab.
      chatState.sessions = bounded.sessions;
      if (!chatState.sessions.some((item) => item.id === chatState.activeId)) {
        chatState.activeId = chatState.sessions[0]?.id || "";
      }
      localStorage.setItem(scopedChatStorageKey(), chatPayloadJSON(chatState.sessions));
    } catch (err) {
      updateChatStatus("浏览器存储空间不足，会话尚未保存；请释放存储空间后重试", "error");
    }
  }

  function saveChatSessions() {
    if (chatState.persistenceTimer) clearTimeout(chatState.persistenceTimer);
    chatState.persistenceTimer = setTimeout(persistChatSessionsNow, chatPersistenceDelay);
  }

  function loadChatSessions() {
    try {
      const storageKey = scopedChatStorageKey();
      // Migrate the original unscoped history only into the admin scope. A
      // selected client key receives a distinct non-secret fingerprint scope.
      const raw = localStorage.getItem(storageKey)
        || (toolHistoryScope() === "admin" ? localStorage.getItem(chatStorageKey) : null);
      if (raw) {
        const parsed = JSON.parse(raw);
        if (parsed && Array.isArray(parsed.sessions)) {
          chatState.sessions = parsed.sessions;
          chatState.activeId = String(parsed.activeId || "");
          if (typeof parsed.model === "string" && parsed.model.trim()) {
            chatState.model = parsed.model.trim();
          }
        }
      }
    } catch (err) {
      chatState.sessions = [];
      chatState.activeId = "";
    }
    if (!Array.isArray(chatState.sessions) || chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    }
    if (!chatState.sessions.find((item) => item && item.id === chatState.activeId)) {
      chatState.activeId = chatState.sessions[0].id;
    }
    chatState.sessions.forEach((session) => {
      if (session && typeof session.isDefaultTitle === "undefined") {
        session.isDefaultTitle = !session.title || session.title === "新会话";
      }
      if (session && !session.promptCacheKey) session.promptCacheKey = createPromptCacheKey();
      if (session && typeof session.reasoningEffort !== "string") session.reasoningEffort = "";
      if (session) session.webSearch = session.webSearch === true;
      if (session) session.xSearch = session.xSearch === true;
      if (Array.isArray(session?.messages)) session.messages = session.messages.map(normalizeAssistantMessage);
    });
    // Loading old/unbounded data also repairs it in the scoped store.
    saveChatSessions();
  }

  function activeChatSession() {
    return chatState.sessions.find((item) => item && item.id === chatState.activeId) || null;
  }

  function updateChatStatus(text, type) {
    const el = document.getElementById("grokChatStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  const escapeHTML = escapeHtml;

  function setRenderedHTML(el, html) {
    el.innerHTML = html;
  }

  function isSafeLinkURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|mailto:|tel:|\/(?!\/)|\.\.?\/|#)/.test(value);
  }

  function isSafeImageURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|data:image\/(?:png|jpe?g|gif|webp|bmp|ico);base64,|\/(?!\/)|\.\.?\/)/.test(value);
  }

  function renderBasicMarkdown(rawText) {
    const text = String(rawText || "").replace(/\\n/g, "\n");
    const escaped = escapeHTML(text);
    const codeBlocks = [];
    const fenced = escaped.replace(/```([a-zA-Z0-9_-]+)?\n([\s\S]*?)```/g, (_match, lang, code) => {
      const safeLang = lang ? escapeHTML(lang) : "";
      // The header names the language and carries a copy button; the click is
      // handled once on the chat log (see bindChatEvents).
      const html = `<div class="code-block-wrap"><div class="code-block-head"><span>${safeLang || "代码"}</span>`
        + `<button type="button" class="code-copy-btn" data-code-copy>复制</button></div>`
        + `<pre class="code-block"><code${safeLang ? ` class="language-${safeLang}"` : ""}>${code}</code></pre></div>`;
      const token = `@@CODEBLOCK_${codeBlocks.length}@@`;
      codeBlocks.push(html);
      return token;
    });

    const renderInline = (value) => {
      const inlineCodes = [];
      let output = value.replace(/`([^`]+)`/g, (_m, code) => {
        const token = `@@INLINE_${inlineCodes.length}@@`;
        inlineCodes.push(`<code class="inline-code">${code}</code>`);
        return token;
      });
      output = output
        .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/\*([^*]+)\*/g, "<em>$1</em>")
        .replace(/~~([^~]+)~~/g, "<del>$1</del>");
      output = output.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_m, alt, url) => {
        const safeAlt = escapeHTML(alt || "image");
        if (!isSafeImageURL(url)) return safeAlt;
        return `<img src="${escapeHTML(url || "")}" alt="${safeAlt}" loading="lazy">`;
      });
      output = output.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, label, url) => {
        const safeLabel = escapeHTML(label || "");
        if (!isSafeLinkURL(url)) return safeLabel;
        return `<a href="${escapeHTML(url || "")}" target="_blank" rel="noopener">${safeLabel}</a>`;
      });
      output = output.replace(/(data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+)/g, (uri) => {
        if (!isSafeImageURL(uri)) return "";
        return `<img src="${escapeHTML(uri)}" alt="image" loading="lazy">`;
      });
      inlineCodes.forEach((html, index) => {
        output = output.replace(new RegExp(`@@INLINE_${index}@@`, "g"), html);
      });
      return output;
    };

    const lines = fenced.split(/\r?\n/);
    const htmlParts = [];
    let inUl = false;
    let inTaskUl = false;
    let inOl = false;
    let inTable = false;
    let paragraphLines = [];

    const closeLists = () => {
      if (inUl) {
        htmlParts.push("</ul>");
        inUl = false;
        inTaskUl = false;
      }
      if (inOl) {
        htmlParts.push("</ol>");
        inOl = false;
      }
    };
    const closeTable = () => {
      if (inTable) {
        htmlParts.push("</tbody></table></div>");
        inTable = false;
      }
    };
    const flushParagraph = () => {
      if (!paragraphLines.length) return;
      htmlParts.push(`<p>${renderInline(paragraphLines.join("<br>"))}</p>`);
      paragraphLines = [];
    };
    const isTableSeparator = (line) => /^\s*\|?(?:\s*:?-+:?\s*\|)+\s*$/.test(line);
    const splitTableRow = (line) => line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim());

    for (let i = 0; i < lines.length; i += 1) {
      const line = lines[i];
      const trimmed = line.trim();
      if (!trimmed) {
        flushParagraph();
        closeLists();
        closeTable();
        continue;
      }

      const codeTokenMatch = trimmed.match(/^@@CODEBLOCK_(\d+)@@$/);
      if (codeTokenMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push(trimmed);
        continue;
      }

      const headingMatch = trimmed.match(/^(#{1,6})\s+(.*)$/);
      if (headingMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        const level = headingMatch[1].length;
        htmlParts.push(`<h${level}>${renderInline(headingMatch[2])}</h${level}>`);
        continue;
      }

      if (/^(-{3,}|\*{3,}|_{3,})$/.test(trimmed)) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push("<hr>");
        continue;
      }

      if (/^\s*>/.test(line)) {
        flushParagraph();
        closeLists();
        closeTable();
        const quoteLines = [];
        let j = i;
        for (; j < lines.length; j += 1) {
          const currentLine = lines[j];
          if (!/^\s*>/.test(currentLine)) break;
          quoteLines.push(currentLine.replace(/^\s*>\s?/, ""));
        }
        i = j - 1;
        htmlParts.push(`<blockquote>${renderBasicMarkdown(quoteLines.join("\n"))}</blockquote>`);
        continue;
      }

      if (trimmed.includes("|")) {
        const nextLine = lines[i + 1] || "";
        if (!inTable && isTableSeparator(nextLine.trim())) {
          flushParagraph();
          closeLists();
          const headers = splitTableRow(trimmed);
          htmlParts.push('<div class="table-wrap"><table><thead><tr>');
          headers.forEach((cell) => htmlParts.push(`<th>${renderInline(cell)}</th>`));
          htmlParts.push("</tr></thead><tbody>");
          inTable = true;
          i += 1;
          continue;
        }
        if (inTable && !isTableSeparator(trimmed)) {
          const cells = splitTableRow(trimmed);
          htmlParts.push("<tr>");
          cells.forEach((cell) => htmlParts.push(`<td>${renderInline(cell)}</td>`));
          htmlParts.push("</tr>");
          continue;
        }
      }

      const taskMatch = trimmed.match(/^[-*+•·]\s+\[([ xX])\]\s+(.*)$/);
      if (taskMatch) {
        flushParagraph();
        if (inUl && !inTaskUl) closeLists();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push('<ul class="task-list">');
          inUl = true;
          inTaskUl = true;
        }
        const checked = taskMatch[1].toLowerCase() === "x";
        htmlParts.push(`<li class="task-item"><input type="checkbox" disabled${checked ? " checked" : ""}>${renderInline(taskMatch[2])}</li>`);
        continue;
      }

      const ulMatch = trimmed.match(/^[-*+•·]\s+(.*)$/);
      if (ulMatch) {
        flushParagraph();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push("<ul>");
          inUl = true;
          inTaskUl = false;
        }
        htmlParts.push(`<li>${renderInline(ulMatch[1])}</li>`);
        continue;
      }

      const olMatch = trimmed.match(/^\d+[.)、]\s+(.*)$/);
      if (olMatch) {
        flushParagraph();
        if (!inOl) {
          closeLists();
          closeTable();
          htmlParts.push("<ol>");
          inOl = true;
        }
        htmlParts.push(`<li>${renderInline(olMatch[1])}</li>`);
        continue;
      }

      paragraphLines.push(trimmed);
    }

    flushParagraph();
    closeLists();
    closeTable();

    let output = htmlParts.join("");
    codeBlocks.forEach((html, index) => {
      output = output.replace(`@@CODEBLOCK_${index}@@`, html);
    });
    return output;
  }

  function parseThinkSections(raw) {
    const input = String(raw || "");
    const parts = [];
    let cursor = 0;
    while (cursor < input.length) {
      const start = input.indexOf("<think>", cursor);
      if (start === -1) {
        parts.push({ type: "text", value: input.slice(cursor) });
        break;
      }
      if (start > cursor) {
        parts.push({ type: "text", value: input.slice(cursor, start) });
      }
      const thinkStart = start + 7;
      const end = input.indexOf("</think>", thinkStart);
      if (end === -1) {
        parts.push({ type: "think", value: input.slice(thinkStart), open: true });
        cursor = input.length;
      } else {
        parts.push({ type: "think", value: input.slice(thinkStart, end), open: false });
        cursor = end + 8;
      }
    }
    return parts;
  }

  function parseRolloutBlocks(text) {
    const lines = String(text || "").split(/\r?\n/);
    const blocks = [];
    let current = null;
    for (const line of lines) {
      const match = line.match(/^\s*\[([^\]]+)\]\[([^\]]+)\]\s*(.*)$/);
      if (match) {
        if (current) blocks.push(current);
        current = { id: match[1], type: match[2], lines: [] };
        if (match[3]) current.lines.push(match[3]);
        continue;
      }
      if (current) {
        current.lines.push(line);
      }
    }
    if (current) blocks.push(current);
    return blocks;
  }

  function parseAgentSections(text) {
    const lines = String(text || "").split(/\r?\n/);
    const sections = [];
    let current = { title: null, lines: [] };
    let hasAgentHeading = false;
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) {
        current.lines.push(line);
        continue;
      }
      const agentMatch = trimmed.match(/^(Grok\s+Leader|(?:Grok\s+)?Agent\s*\d+)$/i);
      if (agentMatch) {
        hasAgentHeading = true;
        if (current.lines.length) {
          sections.push(current);
        }
        current = { title: agentMatch[1], lines: [] };
        continue;
      }
      current.lines.push(line);
    }
    if (current.lines.length) {
      sections.push(current);
    }
    if (!hasAgentHeading) {
      return [{ title: null, lines }];
    }
    return sections;
  }

  const toolTypeMap = {
    websearch: { icon: "", label: "网页搜索" },
    searchimage: { icon: "", label: "图片搜索" },
    agentthink: { icon: "", label: "Agent Think" },
  };
  const defaultToolType = { icon: "", label: "工具" };

  function getToolMeta(typeStr) {
    const key = String(typeStr || "").trim().toLowerCase().replace(/\s+/g, "");
    return toolTypeMap[key] || defaultToolType;
  }

  function renderThinkContent(text, openAll) {
    const sections = parseAgentSections(text);
    if (!sections.length) {
      return renderBasicMarkdown(text);
    }
    const renderGroups = (blocks, openAllGroups) => {
      const groups = [];
      const map = new Map();
      for (const block of blocks) {
        const key = block.id;
        let group = map.get(key);
        if (!group) {
          group = { id: key, items: [] };
          map.set(key, group);
          groups.push(group);
        }
        group.items.push(block);
      }
      return groups.map((group) => {
        const items = group.items.map((item) => {
          const body = renderBasicMarkdown(item.lines.join("\n").trim());
          const typeKey = String(item.type || "").trim().toLowerCase().replace(/\s+/g, "");
          const typeAttr = escapeHTML(typeKey);
          const meta = getToolMeta(item.type);
          const iconHtml = meta.icon ? `<span class="think-tool-icon">${meta.icon}</span>` : "";
          const typeLabel = `${iconHtml}${escapeHTML(meta.label)}`;
          return `<div class="think-item-row think-tool-card" data-tool-type="${typeAttr}"><div class="think-item-type" data-type="${typeAttr}">${typeLabel}</div><div class="think-item-body">${body || "<em>空</em>"}</div></div>`;
        }).join("");
        const title = escapeHTML(group.id);
        const openAttr = openAllGroups ? " open" : "";
        return `<details class="think-rollout-group"${openAttr}><summary><span class="think-rollout-title">${title}</span></summary><div class="think-rollout-body">${items}</div></details>`;
      }).join("");
    };

    const agentBlocks = sections.map((section, idx) => {
      const blocks = parseRolloutBlocks(section.lines.join("\n"));
      const inner = blocks.length
        ? renderGroups(blocks, openAll)
        : `<div class="think-rollout-body">${renderBasicMarkdown(section.lines.join("\n").trim())}</div>`;
      if (!section.title) {
        return `<div class="think-agent-items">${inner}</div>`;
      }
      const title = escapeHTML(section.title);
      const openAttr = openAll ? " open" : (idx === 0 ? " open" : "");
      return `<details class="think-agent"${openAttr}><summary>${title}</summary><div class="think-agent-items">${inner}</div></details>`;
    });
    return `<div class="think-agents">${agentBlocks.join("")}</div>`;
  }

  function renderAssistantContent(raw) {
    const parts = parseThinkSections(String(raw || ""));
    return parts.map((part) => {
      if (part.type === "think") {
        const body = renderThinkContent(part.value.trim(), part.open);
        const openAttr = part.open ? " open" : "";
        return `<details class="think-block" data-think="true"${openAttr}><summary class="think-summary">思考过程</summary><div class="think-content">${body || "<em>空</em>"}</div></details>`;
      }
      return renderBasicMarkdown(part.value);
    }).join("");
  }

  function applyImageGrid(root) {
    if (!root) return;
    const isIgnorable = (node) => {
      if (node.nodeType === Node.TEXT_NODE) {
        return !node.textContent.trim();
      }
      return node.nodeType === Node.ELEMENT_NODE && node.tagName === "BR";
    };

    const isImageLink = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE || node.tagName !== "A") return false;
      const children = Array.from(node.childNodes);
      if (!children.length) return false;
      return children.every((child) => {
        if (child.nodeType === Node.TEXT_NODE) {
          return !child.textContent.trim();
        }
        return child.nodeType === Node.ELEMENT_NODE && child.tagName === "IMG";
      });
    };

    const extractImageItems = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE) return null;
      if (node.classList && node.classList.contains("img-grid")) return null;
      if (node.tagName === "IMG") {
        return { items: [node], removeNode: null };
      }
      if (isImageLink(node)) {
        return { items: [node], removeNode: null };
      }
      if (node.tagName === "P") {
        const items = [];
        const children = Array.from(node.childNodes);
        if (!children.length) return null;
        for (const child of children) {
          if (child.nodeType === Node.TEXT_NODE) {
            if (!child.textContent.trim()) continue;
            return null;
          }
          if (child.nodeType === Node.ELEMENT_NODE) {
            if (child.tagName === "IMG" || isImageLink(child)) {
              items.push(child);
              continue;
            }
            if (child.tagName === "BR") continue;
            return null;
          }
          return null;
        }
        if (!items.length) return null;
        return { items, removeNode: node };
      }
      return null;
    };

    const wrapImagesInContainer = (container) => {
      const children = Array.from(container.childNodes);
      let group = [];
      let groupStart = null;
      let removeNodes = [];

      const flush = () => {
        if (group.length < 2) {
          group = [];
          groupStart = null;
          removeNodes = [];
          return;
        }
        const wrapper = document.createElement("div");
        wrapper.className = "img-grid";
        const cols = Math.min(4, group.length);
        wrapper.style.setProperty("--cols", String(cols));
        if (groupStart) {
          container.insertBefore(wrapper, groupStart);
        } else {
          container.appendChild(wrapper);
        }
        group.forEach((img) => wrapper.appendChild(img));
        removeNodes.forEach((n) => n.parentNode && n.parentNode.removeChild(n));
        group = [];
        groupStart = null;
        removeNodes = [];
      };

      children.forEach((node) => {
        if (group.length && isIgnorable(node)) {
          removeNodes.push(node);
          return;
        }
        const extracted = extractImageItems(node);
        if (extracted && extracted.items.length) {
          if (!groupStart) groupStart = node;
          group.push(...extracted.items);
          if (extracted.removeNode) {
            removeNodes.push(extracted.removeNode);
          }
          return;
        }
        flush();
      });
      flush();
    };

    const containers = [root, ...root.querySelectorAll(".think-content, .think-item-body, .think-rollout-body, .think-agent-items")];
    containers.forEach((container) => {
      if (!container || container.closest(".img-grid")) return;
      if (!container.querySelector || !container.querySelector("img")) return;
      wrapImagesInContainer(container);
    });
  }

  function enhanceBrokenImages(root) {
    if (!root) return;
    const images = root.querySelectorAll("img");
    images.forEach((img) => {
      if (img.dataset.retryBound) return;
      img.dataset.retryBound = "1";
      img.addEventListener("error", () => {
        if (img.dataset.failed) return;
        img.dataset.failed = "1";
        const wrapper = document.createElement("button");
        wrapper.type = "button";
        wrapper.className = "img-retry";
        wrapper.textContent = "点击重试";
        wrapper.addEventListener("click", () => {
          wrapper.classList.add("loading");
          const original = img.getAttribute("src") || "";
          const cacheBust = original.includes("?") ? "&" : "?";
          img.dataset.failed = "";
          img.src = `${original}${cacheBust}t=${Date.now()}`;
        });
        img.replaceWith(wrapper);
      });
      img.addEventListener("load", () => {
        if (img.dataset.failed) {
          img.dataset.failed = "";
        }
      });
    });
  }

  function updateThinkSummary(root, elapsedSec) {
    if (!root) return;
    const summaries = root.querySelectorAll(".think-summary");
    if (!summaries.length) return;
    let text = "思考过程";
    if (typeof elapsedSec === "number") {
      text = elapsedSec > 0 ? `思考 ${elapsedSec}s` : "思考完成";
    } else {
      text = "思考中";
    }
    summaries.forEach((node) => {
      node.textContent = text;
      const block = node.closest(".think-block");
      if (!block) return;
      if (typeof elapsedSec === "number") {
        block.removeAttribute("data-thinking");
      } else {
        block.setAttribute("data-thinking", "true");
      }
    });
  }

  function renderUserContent(content, attachment) {
    const wrapper = document.createElement("div");
    const text = document.createElement("div");
    text.className = "message-content";
    text.textContent = String(content || "");
    wrapper.appendChild(text);
    if (attachment && attachment.name) {
      const badge = document.createElement("div");
      badge.className = "message-attachment";
      badge.textContent = `附件: ${attachment.name}`;
      wrapper.appendChild(badge);
    }
    return wrapper;
  }

  function appendChatMessage(role, content, attachment, message) {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return null;
    if (empty) empty.style.display = "none";
    const row = document.createElement("div");
    row.className = `message-row ${role}`;
    const bubble = document.createElement("div");
    bubble.className = "message-bubble";
    let contentEl = document.createElement("div");
    contentEl.className = role === "assistant" ? "message-content rendered" : "message-content";
    if (role === "assistant") {
      setRenderedHTML(contentEl, renderAssistantContent(message ? assistantDisplay(message) : content || "") + renderToolActivity(message?.tools));
      applyImageGrid(contentEl);
      if (String(content || "").includes("<think>")) {
        updateThinkSummary(contentEl, 0);
      }
      enhanceBrokenImages(contentEl);
      bubble.appendChild(contentEl);
    } else {
      contentEl = renderUserContent(content, attachment);
      bubble.appendChild(contentEl);
    }
    row.appendChild(bubble);
    const actions = document.createElement("div");
    actions.className = "message-actions";
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "action-btn";
    copyBtn.textContent = "复制";
    copyBtn.title = "复制这条消息";
    copyBtn.addEventListener("click", () => {
      const text = role === "assistant"
        ? String(bubble.innerText || "").trim()
        : String(content || "");
      copyToClipboard(text);
    });
    actions.appendChild(copyBtn);
    if (role === "user") {
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditChatMessage(row, content, attachment));
      actions.appendChild(editBtn);
    } else {
      const retryBtn = document.createElement("button");
      retryBtn.type = "button";
      retryBtn.className = "action-btn";
      retryBtn.textContent = "重试";
      retryBtn.addEventListener("click", () => retryAssistantMessage(row));
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditAssistantMessage(row));
      actions.appendChild(retryBtn);
      actions.appendChild(editBtn);
    }
    const deleteBtn = document.createElement("button");
    deleteBtn.type = "button";
    deleteBtn.className = "action-btn action-btn-danger";
    deleteBtn.textContent = "删除";
    deleteBtn.addEventListener("click", () => deleteChatMessage(row));
    actions.appendChild(deleteBtn);
    row.appendChild(actions);
    log.appendChild(row);
    log.scrollTop = log.scrollHeight;
    return contentEl;
  }

  function chatMessageIndex(row) {
    if (!row) return -1;
    const log = row.closest?.("#grokChatLog") || row.parentElement;
    return Array.from(log?.querySelectorAll?.(".message-row") || []).indexOf(row);
  }

  function confirmTrailingMessages(action, trailingCount) {
    if (trailingCount <= 0) return true;
    return window.confirm(`${action}会同时删除后续 ${trailingCount} 条消息，是否继续？`);
  }

  function truncateChatBranch(session, keepCount) {
    session.messages = (Array.isArray(session.messages) ? session.messages : []).slice(0, keepCount);
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
  }

  function deleteChatMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const targetIndex = chatMessageIndex(row);
    if (targetIndex < 0 || targetIndex >= messages.length) return;
    const trailingCount = messages.length - targetIndex - 1;
    const prompt = trailingCount > 0
      ? `删除这条消息会同时删除后续 ${trailingCount} 条消息，是否继续？`
      : "确认删除这条消息？";
    if (!window.confirm(prompt)) return;
    truncateChatBranch(session, targetIndex);
  }

  function startEditChatMessage(row, content, attachment) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const targetIndex = chatMessageIndex(row);
    if (targetIndex < 0) return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.innerHTML = "";

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";

    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", async () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      const trailingCount = messages.length - targetIndex - 1;
      if (!confirmTrailingMessages("编辑这条消息", trailingCount)) return;
      const edited = { ...messages[targetIndex], content: next };
      if (attachment) edited.attachment = attachment;
      session.messages = messages.slice(0, targetIndex).concat(edited);
      session.promptCacheKey = createPromptCacheKey();
      session.updatedAt = Date.now();
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
      const contentEl = appendChatMessage("assistant", "");
      await requestChatCompletion(session, contentEl);
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });

    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  function startEditAssistantMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const rowIndex = chatMessageIndex(row);
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    const msg = messages[rowIndex];
    if (!msg || msg.role !== "assistant") return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.classList.add("hidden");

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(msg.content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";
    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      // Editing a historical answer changes the branch's premise, so the
      // descendants that answered the old text must not survive it. Only the
      // edited message itself is kept.
      const trailing = messages.length - (rowIndex + 1);
      if (!confirmTrailingMessages("编辑这条回复", trailing)) {
        return;
      }
      msg.content = next;
      msg.reasoning = "";
      msg.tools = [];
      session.promptCacheKey = createPromptCacheKey();
      session.updatedAt = Date.now();
      if (trailing > 0) {
        session.messages = messages.slice(0, rowIndex + 1);
      }
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });
    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  async function requestChatCompletion(session, contentEl) {
    if (!chatState.modelsLoaded || !chatState.model) {
      updateChatStatus("模型目录尚未加载，无法发送", "error");
      return;
    }
    const requestGeneration = Number(chatState.requestGeneration || 0) + 1;
    chatState.requestGeneration = requestGeneration;
    const abortController = new AbortController();
    const isCurrentRequest = () => chatState.requestGeneration === requestGeneration;
    if (chatState.abortController) chatState.abortController.abort();
    let assistantText = "";
    let answerText = "";
    let reasoningText = "";
    const toolActivities = new Map();
    let saved = false;
    let streamCompleted = false;
    const persistAssistant = () => {
      if (!isCurrentRequest() || saved || (!answerText.trim() && !reasoningText.trim() && !toolActivities.size)) return;
      saved = true;
      session.messages.push({ role: "assistant", content: answerText, reasoning: reasoningText, tools: Array.from(toolActivities.values()) });
      session.updatedAt = Date.now();
    };
    let reasoningOpen = false;
    let hasThink = false;
    let thinkStartAt = null;
    let thinkElapsed = null;
    let thinkAutoCollapsed = false;
    chatState.sending = true;
    chatState.abortController = abortController;
    setChatSendButtonState(true);
    updateChatStatus("连接中...", "connecting");

    const updateAssistantView = () => {
      if (!isCurrentRequest() || !contentEl) return;
      let savedThinkStates = null;
      if (hasThink && thinkAutoCollapsed) {
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        if (blocks.length) {
          savedThinkStates = Array.from(blocks).map((b) => b.hasAttribute("open"));
        }
      }
      setRenderedHTML(contentEl, renderAssistantContent(assistantText) + renderToolActivity(Array.from(toolActivities.values())));
      if (hasThink) {
        updateThinkSummary(contentEl, typeof thinkElapsed === "number" ? thinkElapsed : null);
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        blocks.forEach((block, index) => {
          if (savedThinkStates && index < savedThinkStates.length) {
            if (savedThinkStates[index]) {
              block.setAttribute("open", "");
            } else {
              block.removeAttribute("open");
            }
          } else if (thinkElapsed === null || thinkElapsed === undefined) {
            block.setAttribute("open", "");
          } else if (!thinkAutoCollapsed) {
            block.removeAttribute("open");
            thinkAutoCollapsed = true;
          }
        });
      }
      applyImageGrid(contentEl);
      enhanceBrokenImages(contentEl);
      if (hasThink) {
        const thinkNodes = contentEl.querySelectorAll(".think-content");
        thinkNodes.forEach((node) => {
          node.scrollTop = node.scrollHeight;
        });
      }
      const log = document.getElementById("grokChatLog");
      if (log) log.scrollTop = log.scrollHeight;
    };
    const scheduleAssistantView = () => {
      if (!isCurrentRequest() || chatState.renderFrame) return;
      chatState.renderFrame = requestAnimationFrame(() => {
        chatState.renderFrame = 0;
        updateAssistantView();
      });
    };

    try {
      const payload = buildResponsesPayload();
      const res = await fetch(`${toolInferencePrefix()}/responses`, {
        method: "POST",
        headers: toolAuthHeaders({ "Content-Type": "application/json" }),
        body: JSON.stringify(payload),
        signal: abortController.signal,
      });
      if (handleUnauthorized(res)) return { aborted: false, text: "" };
      if (!res.ok || !res.body) {
        throw new Error(await res.text());
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      // Line-oriented SSE scanner: tolerates CRLF line endings across chunk
      // boundaries and processes a trailing frame not followed by a blank line.
      // Bare CR line endings are not supported.
      let carry = "";
      const pendingData = [];
      let finishReason = "";
      const finishReasonSweep = (finalFinish) => {
        finishReason = finalFinish;
        streamCompleted = true;
        for (const tool of toolActivities.values()) {
          if (tool.status !== "failed") tool.status = finalFinish === "tool_calls" ? "awaiting_result" : "completed";
        }
      };
      const dispatchData = (data) => {
        if (!isCurrentRequest()) return;
        if (!data || data === "[DONE]") {
          if (data === "[DONE]") streamCompleted = true;
          return;
        }
        let event = null;
        try {
          event = JSON.parse(data);
        } catch (err) {
          throw new Error("服务端返回了无效的流数据");
        }
        if (event?.error) {
          throw new Error(event.error.message || String(event.error));
        }
        const kind = String(event?.type || "");
        if (kind === "response.failed" || kind === "error") {
          throw new Error(event?.response?.error?.message || event?.message || "上游返回失败");
        }
        if (kind === "response.completed" || kind === "response.incomplete") {
          const hasCalls = Array.from(toolActivities.values()).some((tool) => tool.status !== "failed");
          finishReasonSweep(kind === "response.incomplete" ? "length" : hasCalls ? "tool_calls" : "stop");
          scheduleAssistantView();
          return;
        }
        const item = event?.item;
        if (item && (kind === "response.output_item.added" || kind === "response.output_item.done")) {
          const itemType = String(item.type || "");
          const done = kind === "response.output_item.done";
          if (itemType === "web_search_call" || itemType === "x_search_call") {
            const id = String(item.id || item.call_id || itemType);
            const itemStatus = String(item.status || "").toLowerCase();
            const status = itemStatus === "failed" || itemStatus === "incomplete" ? "failed" : done ? "completed" : "in_progress";
            toolActivities.set(id, { id, name: itemType, status, detail: item.action?.query || "" });
          } else if (itemType === "function_call") {
            const id = String(item.call_id || item.id || "tool");
            const previous = toolActivities.get(id) || { id, name: "工具", detail: "", status: "in_progress" };
            if (item.name) previous.name = item.name;
            if (typeof item.arguments === "string" && item.arguments) previous.detail = item.arguments;
            if (done) previous.status = "awaiting_result";
            toolActivities.set(id, previous);
          }
          scheduleAssistantView();
          return;
        }
        if (kind === "response.function_call_arguments.delta" && typeof event.delta === "string") {
          const id = String(event.item_id || "tool");
          const previous = toolActivities.get(id) || { id, name: "工具", detail: "", status: "in_progress" };
          previous.detail += event.delta;
          toolActivities.set(id, previous);
          scheduleAssistantView();
          return;
        }
        const delta = typeof event?.delta === "string" ? event.delta : "";
        if (!delta) return;
        if (kind === "response.reasoning_summary_text.delta" || kind === "response.reasoning_text.delta") {
          reasoningText += delta;
          if (!reasoningOpen) {
            assistantText += "<think>";
            reasoningOpen = true;
          }
          assistantText += delta;
          updateChatStatus("思考中...", "connecting");
        } else if (kind === "response.output_text.delta" || kind === "response.refusal.delta") {
          if (reasoningOpen) {
            assistantText += "</think>";
            reasoningOpen = false;
          }
          answerText += delta;
          assistantText += delta;
          updateChatStatus("生成中...", "connecting");
        } else {
          return;
        }
        if (!hasThink && assistantText.includes("<think>")) {
          hasThink = true;
          thinkStartAt = Date.now();
          thinkElapsed = null;
        }
        if (hasThink && thinkStartAt && thinkElapsed === null && assistantText.includes("</think>")) {
          thinkElapsed = Math.max(1, Math.round((Date.now() - thinkStartAt) / 1000));
        }
        scheduleAssistantView();
      };
      const handleLine = (line) => {
        if (line === "") {
          const data = pendingData.join("");
          pendingData.length = 0;
          dispatchData(data);
          return;
        }
        if (line.startsWith("data:")) {
          pendingData.push(line.slice(5).trimStart());
        }
      };
      const feedChunk = (text) => {
        const parts = (carry + text).split("\n");
        carry = parts.pop();
        for (let line of parts) {
          if (line.endsWith("\r")) line = line.slice(0, -1);
          handleLine(line);
        }
      };
      while (isCurrentRequest()) {
        const { value, done } = await reader.read();
        if (done) break;
        feedChunk(decoder.decode(value, { stream: true }));
      }
      if (!isCurrentRequest()) return { aborted: true, stale: true, text: "" };
      feedChunk(decoder.decode());
      if (carry) handleLine(carry.endsWith("\r") ? carry.slice(0, -1) : carry);
      handleLine("");
      if (!streamCompleted) throw new Error("连接中断，已保留收到的部分回答");
      if (chatState.renderFrame) cancelAnimationFrame(chatState.renderFrame);
      chatState.renderFrame = 0;
      updateAssistantView();
      if (!assistantText.trim() && !toolActivities.size) {
        throw new Error("服务端未返回可显示的内容，请重试或检查上游日志");
      }
      persistAssistant();
      session.updatedAt = Date.now();
      saveChatSessions();
      if (finishReason === "length") {
        updateChatStatus("回复因达到长度上限被截断", "error");
      } else {
        updateChatStatus("完成", "ok");
      }
      return { aborted: false, text: assistantText.trim() };
    } catch (err) {
      if (!isCurrentRequest()) return { aborted: true, stale: true, text: "" };
      persistAssistant();
      if (err && err.name === "AbortError") {
        if (contentEl && !assistantText.trim()) {
          assistantText = "[stopped]";
          updateAssistantView();
        }
        updateChatStatus("已停止", "error");
        return { aborted: true, text: assistantText.trim() };
      }
      if (contentEl) {
        if (!assistantText.trim()) assistantText = `[error] ${err.message || err}`;
        updateAssistantView();
      }
      updateChatStatus(err.message || "发送失败", "error");
      return { aborted: false, text: "" };
    } finally {
      if (isCurrentRequest()) {
        if (chatState.renderFrame) cancelAnimationFrame(chatState.renderFrame);
        chatState.renderFrame = 0;
        chatState.sending = false;
        chatState.abortController = null;
        setChatSendButtonState(false);
        renderChatSessions();
        saveChatSessions();
      }
    }
  }

  async function retryAssistantMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const rowIndex = chatMessageIndex(row);
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    if (messages[rowIndex]?.role !== "assistant") return;

    let lastUserIndex = -1;
    for (let i = rowIndex - 1; i >= 0; i -= 1) {
      if (messages[i]?.role === "user") {
        lastUserIndex = i;
        break;
      }
    }
    if (lastUserIndex < 0) {
      showToast("没有可重试的上文", "info");
      return;
    }

    const trailingCount = messages.length - rowIndex - 1;
    if (!confirmTrailingMessages("重试这条回复", trailingCount)) return;
    session.messages = messages.slice(0, lastUserIndex + 1);
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  function rerenderChatThread() {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return;
    log.innerHTML = "";
    const session = activeChatSession();
    const messages = Array.isArray(session?.messages) ? session.messages : [];
    if (messages.length === 0) {
      if (empty) {
        empty.style.display = "block";
        log.appendChild(empty);
      }
      return;
    }
    messages.forEach((msg) => appendChatMessage(msg.role, msg.content, msg.attachment, msg));
  }

  function renderChatSessions() {
    const list = document.getElementById("grokSessionList");
    if (!list) return;
    list.innerHTML = "";
    chatState.sessions.forEach((session) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = `session-item${session.id === chatState.activeId ? " active" : ""}`;
      item.dataset.id = session.id;

      const title = document.createElement("span");
      title.className = "session-title";
      title.textContent = session.title || "新会话";
      title.addEventListener("dblclick", (event) => {
        event.stopPropagation();
        startRenameChatSession(session.id, title);
      });

      const meta = document.createElement("span");
      meta.className = "session-meta";
      meta.textContent = relativeTime(session.updatedAt);

      const delBtn = document.createElement("button");
      delBtn.type = "button";
      delBtn.className = "session-delete";
      delBtn.title = "删除";
      delBtn.textContent = "×";
      delBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        deleteChatSession(session.id);
      });

      item.addEventListener("click", () => {
        switchChatSession(session.id);
        if (isMobileChatSidebar()) closeChatSidebar();
      });

      item.appendChild(title);
      item.appendChild(meta);
      item.appendChild(delBtn);
      list.appendChild(item);
    });
  }

  function syncChatModelUI() {
    const route = chatState.routes?.find((item) => item.id === chatState.model);
    const effortControl = document.getElementById("grokReasoningEffort");
    const session = activeChatSession();
    if (effortControl) {
      const labels = { none: "关闭", low: "低", medium: "中", high: "高", xhigh: "极高" };
      const advertised = Array.isArray(route?.reasoning_efforts)
        ? route.reasoning_efforts.map((value) => String(value || "").trim().toLowerCase()).filter(Boolean)
        : null;
      const efforts = advertised || ["none", "low", "medium", "high", "xhigh"];
      const previous = String(session?.reasoningEffort || effortControl.value || "");
      effortControl.replaceChildren();
      const automatic = document.createElement("option");
      automatic.value = "";
      automatic.textContent = "自动";
      effortControl.appendChild(automatic);
      efforts.forEach((value) => {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = labels[value] || value;
        effortControl.appendChild(option);
      });
      effortControl.disabled = route?.supports_reasoning_effort === false;
      const fallback = efforts.includes(String(route?.default_reasoning_effort || "")) ? String(route.default_reasoning_effort) : "";
      effortControl.value = efforts.includes(previous) ? previous : fallback;
      if (session) session.reasoningEffort = effortControl.value;
    }
    const webSearch = document.getElementById("grokWebSearch");
    if (webSearch) {
      const unsupported = route?.supports_backend_search === false;
      webSearch.disabled = unsupported;
      if (unsupported) {
        webSearch.checked = false;
        if (session) session.webSearch = false;
      }
    }
    const label = document.getElementById("grokModelLabel");
    if (label) label.textContent = chatState.model;
    if (session) {
      session.model = chatState.model;
    }
  }

  function renderChatModelDropdown() {
    const dropdown = document.getElementById("grokModelDropdown");
    if (!dropdown) return;
    dropdown.innerHTML = "";
    chatState.models.forEach((model) => {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = `model-option${model === chatState.model ? " active" : ""}`;
      btn.textContent = model;
      btn.dataset.model = model;
      dropdown.appendChild(btn);
    });
  }

  function preferredAppChatModel(models) {
    const list = Array.isArray(models) ? models : [];
    return list[0] || "";
  }

  async function loadChatModels() {
    try {
      let routes;
      if (toolAuthState.mode === "client") {
        const res = await fetch("/grok/v1/models", { headers: toolAuthHeaders() });
        if (!res.ok) throw new Error(`模型目录加载失败 (HTTP ${res.status})`);
        const payload = await res.json();
        routes = (Array.isArray(payload?.data) ? payload.data : [])
          .map((item) => typeof item === "string" ? { id: item } : item)
          .filter((item) => String(item?.id || "").trim())
          .map((item) => {
            const route = { ...item, id: String(item.id).trim() };
            // /v1/models is authoritative when it declares capabilities. Only
            // old servers that omit the field get the compatibility chat route.
            if (!Array.isArray(route.capabilities)) route.capabilities = ["chat"];
            return route;
          });
      } else {
        const catalogPromise = window.GrokModelCatalogPromise || (window.GrokModelCatalogPromise = fetch("/api/grok/tools/v1/models").then(async (res) => {
          if (handleUnauthorized(res)) throw new Error("登录已失效");
          if (!res.ok) throw new Error("模型目录加载失败");
          const payload = await res.json();
          return Array.isArray(payload?.data) ? payload.data : [];
        }));
        routes = await catalogPromise;
      }
      const supports = (item, capability) => {
        const capabilities = Array.isArray(item?.capabilities) ? item.capabilities : [];
        // Match the backend's legacy compatibility rule: an old row with no
        // capability metadata is a conversation route, not an invisible model.
        return capabilities.length === 0 ? capability === "chat" : capabilities.includes(capability);
      };
      chatState.routes = routes;
      const videoSelect = document.getElementById("videoModel");
      if (videoSelect) {
        videoSelect.replaceChildren();
        routes.filter((item) => supports(item, "video")).forEach((item) => {
          const option = document.createElement("option"); option.value = item.id; option.textContent = item.id; videoSelect.appendChild(option);
        });
        videoSelect.addEventListener("change", syncVideoRouteControls);
        ["videoAction", "videoReferenceURL", "videoReferenceVoice"].forEach((id) => document.getElementById(id)?.addEventListener("change", syncVideoRouteControls));
        syncVideoRouteControls();
      }
      window.dispatchEvent(new CustomEvent("grok-models-loaded", { detail: routes }));
      chatState.capabilities = {
        chat: routes.some((item) => supports(item, "chat")),
        imagine: routes.some((item) => supports(item, "image")),
        video: routes.some((item) => supports(item, "video")),
        voice: routes.some((item) => supports(item, "realtime") || supports(item, "tts") || supports(item, "stt")),
      };
      chatState.modelsLoaded = true;
      const models = routes
        .filter((item) => supports(item, "chat"))
        .map((item) => String(item?.id || "").trim())
        .filter(Boolean);
      if (models.length === 0) throw new Error("模型目录为空");
      chatState.models = models;
      if (!models.includes(chatState.model)) {
        chatState.model = preferredAppChatModel(models);
      }
    } catch (err) {
      chatState.modelsLoaded = false;
      chatState.models = [];
      chatState.model = "";
      updateChatStatus(`模型目录不可用：${err?.message || "加载失败"}`, "error");
    }
  }

  function ensureChatTitle(session) {
    if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return;
    if (session.isDefaultTitle === false) return;
    const firstUser = session.messages.find((msg) => msg && msg.role === "user" && String(msg.content || "").trim());
    if (!firstUser) return;
    session.title = String(firstUser.content || "").replace(/\s+/g, " ").trim().slice(0, 20) || "新会话";
    session.isDefaultTitle = false;
  }

  function renameChatSession(id, newTitle) {
    const session = chatState.sessions.find((item) => item && item.id === id);
    if (!session) return;
    const trimmed = String(newTitle || "").trim();
    session.title = trimmed || "新会话";
    session.isDefaultTitle = !trimmed;
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
  }

  function deleteChatSession(id) {
    if (chatState.sending) return;
    const idx = chatState.sessions.findIndex((item) => item && item.id === id);
    if (idx < 0) return;
    const target = chatState.sessions[idx];
    const count = Array.isArray(target?.messages) ? target.messages.length : 0;
    // Deleting a conversation is irreversible in the local store, so it needs
    // the same explicit confirmation as clearing one.
    if (!window.confirm(count > 0 ? `确认删除该会话及其 ${count} 条消息？` : "确认删除该会话？")) return;
    chatState.sessions.splice(idx, 1);
    if (chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    } else if (chatState.activeId === id) {
      chatState.activeId = chatState.sessions[Math.max(0, idx - 1)].id;
    }
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function startRenameChatSession(sessionId, titleEl) {
    const session = chatState.sessions.find((item) => item && item.id === sessionId);
    if (!session || !titleEl || !titleEl.parentNode) return;
    const input = document.createElement("input");
    input.type = "text";
    input.className = "session-rename-input";
    input.value = String(session.title || "");
    input.maxLength = 40;
    titleEl.replaceWith(input);
    input.focus();
    input.select();
    const commit = () => renameChatSession(sessionId, input.value);
    input.addEventListener("blur", commit);
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        input.blur();
      }
      if (event.key === "Escape") {
        input.value = session.title || "新会话";
        input.blur();
      }
    });
  }

  function switchChatSession(id) {
    if (chatState.sending) return;
    if (!id || id === chatState.activeId) return;
    chatState.activeId = id;
    const session = activeChatSession();
    if (session && session.model) {
      chatState.model = session.model;
    }
    syncChatSessionSettings(session);
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function clearCurrentChatSession() {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return;
    if (!window.confirm(`确认清空当前会话的 ${session.messages.length} 条消息？`)) return;
    session.messages = [];
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
    updateChatStatus("当前会话已清空", "ok");
  }

  function newChatSession() {
    if (chatState.sending) return;
    const session = createChatSession();
    chatState.sessions.unshift(session);
    chatState.activeId = session.id;
    syncChatSessionSettings(session);
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
    updateChatStatus("就绪");
  }

  function buildResponsesPayload() {
    const session = activeChatSession();
    if (!session) {
      throw new Error("missing chat session");
    }
    const input = [];
    session.messages.forEach((msg) => {
      const role = msg.role === "assistant" ? "assistant" : "user";
      const parts = [];
      if (msg.content) {
        parts.push({ type: role === "assistant" ? "output_text" : "input_text", text: String(msg.content || "") });
      }
      if (role === "user" && msg.attachment?.dataUrl) {
        parts.push({ type: "input_file", file: { data: msg.attachment.dataUrl } });
      }
      if (parts.length === 0) return;
      input.push({ type: "message", role, content: parts });
    });
    const payload = {
      model: chatState.model,
      input,
      stream: true,
      store: false,
    };
    const systemPrompt = String(document.getElementById("grokSystemInput")?.value || "").trim();
    if (systemPrompt) payload.instructions = systemPrompt;
    const route = chatState.routes?.find((item) => item.id === chatState.model);
    // The Responses API defines no sampling controls, so temperature and top_p
    // are never sent: Build would forward them upstream verbatim.
    const effort = String(session.reasoningEffort || "").trim();
    const reasoning = {};
    if (effort) reasoning.effort = effort;
    // Every reasoning request is paired with a summary; only "none" must not ask for one.
    if (effort !== "none") reasoning.summary = "auto";
    if (Object.keys(reasoning).length) payload.reasoning = reasoning;
    if (session.promptCacheKey) payload.prompt_cache_key = session.promptCacheKey;
    const tools = [];
    if (session.webSearch && route?.supports_backend_search !== false) tools.push({ type: "web_search" });
    if (session.xSearch) tools.push({ type: "x_search" });
    if (tools.length) payload.tools = tools;
    return payload;
  }

  function syncChatSessionSettings(session) {
    const effort = document.getElementById("grokReasoningEffort");
    const webSearch = document.getElementById("grokWebSearch");
    const xSearch = document.getElementById("grokXSearch");
    if (effort) effort.value = String(session?.reasoningEffort || "");
    if (webSearch) webSearch.checked = session?.webSearch === true;
    if (xSearch) xSearch.checked = session?.xSearch === true;
  }

  async function sendChatMessage() {
    if (chatState.sending) return;
    const input = document.getElementById("grokPromptInput");
    const prompt = String(input?.value || "").trim();
    if (!prompt) return;
    const session = activeChatSession();
    if (!session) return;
    session.messages.push({ role: "user", content: prompt });
    session.updatedAt = Date.now();
    ensureChatTitle(session);
    renderChatSessions();
    rerenderChatThread();
    if (input) {
      input.value = "";
      input.style.height = "40px";
    }

    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  // Code-block copy buttons live in rendered markup that is replaced while a
  // reply streams, so one delegated listener serves every block.
  function bindCodeCopy() {
    const log = document.getElementById("grokChatLog");
    if (!log || log.dataset.codeCopyBound) return;
    log.dataset.codeCopyBound = "1";
    log.addEventListener("click", (event) => {
      const button = event.target && event.target.closest ? event.target.closest("[data-code-copy]") : null;
      if (!button) return;
      const code = button.closest(".code-block-wrap")?.querySelector("pre");
      if (!code) return;
      copyToClipboard(code.innerText || "");
      button.textContent = "已复制";
      setTimeout(() => { button.textContent = "复制"; }, 1500);
    });
  }

  function bindChatEvents() {
    bindCodeCopy();
    const newBtn = document.getElementById("grokChatNewBtn");
    const clearBtn = document.getElementById("grokChatClearBtn");
    const sendBtn = document.getElementById("grokSendBtn");
    const input = document.getElementById("grokPromptInput");
    const modelChip = document.getElementById("grokModelChip");
    const modelDropdown = document.getElementById("grokModelDropdown");
    const settingsToggle = document.getElementById("grokSettingsToggle");
    const settingsPanel = document.getElementById("grokSettingsPanel");
    const settingsCloseBtn = document.getElementById("grokSettingsCloseBtn");
    const sidebarToggle = document.getElementById("grokChatSidebarToggle");
    const sidebarOverlay = document.getElementById("grokChatSidebarOverlay");
    const collapseBtn = document.getElementById("grokChatCollapseBtn");
    const expandBtn = document.getElementById("grokChatExpandBtn");
    const reasoningEffort = document.getElementById("grokReasoningEffort");
    const webSearch = document.getElementById("grokWebSearch");
    const xSearch = document.getElementById("grokXSearch");

    if (newBtn) newBtn.addEventListener("click", () => {
      newChatSession();
      if (isMobileChatSidebar()) {
        closeChatSidebar();
      }
    });
    if (clearBtn) clearBtn.addEventListener("click", clearCurrentChatSession);
    if (sendBtn) {
      sendBtn.addEventListener("click", () => {
        if (chatState.sending && chatState.abortController) {
          chatState.abortController.abort();
          return;
        }
        sendChatMessage().catch(() => {});
      });
    }
    if (input) {
      let composing = false;
      input.addEventListener("compositionstart", () => {
        composing = true;
      });
      input.addEventListener("compositionend", () => {
        composing = false;
      });
      input.addEventListener("input", () => {
        input.style.height = "40px";
        input.style.height = `${Math.min(input.scrollHeight, 160)}px`;
      });
      input.addEventListener("keydown", (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          if (composing || event.isComposing) return;
          event.preventDefault();
          sendChatMessage().catch(() => {});
        }
      });
    }
    if (sidebarToggle) sidebarToggle.addEventListener("click", () => toggleChatSidebar());
    if (expandBtn) expandBtn.addEventListener("click", () => openChatSidebar());
    if (collapseBtn) collapseBtn.addEventListener("click", () => closeChatSidebar());
    if (sidebarOverlay) sidebarOverlay.addEventListener("click", () => closeChatSidebar());
    window.addEventListener("resize", syncChatSidebarState);
    if (modelChip && modelDropdown) {
      modelChip.addEventListener("click", (event) => {
        event.stopPropagation();
        closeChatSettingsPanel(settingsToggle, settingsPanel);
        modelDropdown.classList.toggle("show");
      });
      modelDropdown.addEventListener("click", (event) => {
        const btn = event.target.closest(".model-option");
        if (!btn || !modelDropdown.contains(btn)) return;
        chatState.model = String(btn.dataset.model || chatState.model);
        const session = activeChatSession();
        if (session) {
          session.model = chatState.model;
          session.promptCacheKey = createPromptCacheKey();
        }
        syncChatModelUI();
        renderChatModelDropdown();
        saveChatSessions();
        modelDropdown.classList.remove("show");
      });
    }
    if (settingsToggle && settingsPanel) {
      settingsToggle.addEventListener("click", (event) => {
        event.stopPropagation();
        modelDropdown?.classList.remove("show");
        toggleChatSettingsPanel(settingsToggle, settingsPanel);
      });
      settingsPanel.addEventListener("click", (event) => {
        event.stopPropagation();
      });
    }
    if (settingsCloseBtn) {
      settingsCloseBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        closeChatSettingsPanel(settingsToggle, settingsPanel);
        settingsToggle?.focus();
      });
    }
    document.addEventListener("click", () => {
      modelDropdown?.classList.remove("show");
      closeChatSettingsPanel(settingsToggle, settingsPanel);
    });
    document.addEventListener("keydown", (event) => {
      if (event.key !== "Escape") return;
      modelDropdown?.classList.remove("show");
      closeChatSettingsPanel(settingsToggle, settingsPanel);
    });
    [reasoningEffort, webSearch, xSearch].forEach((control) => {
      if (!control) return;
      control.addEventListener("change", () => {
        const session = activeChatSession();
        if (!session) return;
        session.reasoningEffort = String(reasoningEffort?.value || "");
        session.webSearch = webSearch?.checked === true;
        session.xSearch = xSearch?.checked === true;
        session.updatedAt = Date.now();
        saveChatSessions();
      });
    });
    const systemInput = document.getElementById("grokSystemInput");
    if (systemInput) {
      systemInput.addEventListener("input", () => {
        saveGrokToolsUIState({ chatSystemPrompt: String(systemInput.value || "") });
      });
    }
  }

  function toggleChatSettingsPanel(toggle, panel) {
    if (!panel) return;
    const willShow = !panel.classList.contains("show");
    panel.classList.toggle("show", willShow);
    panel.setAttribute("aria-hidden", willShow ? "false" : "true");
    if (toggle) {
      toggle.setAttribute("aria-expanded", willShow ? "true" : "false");
    }
  }

  function closeChatSettingsPanel(toggle, panel) {
    if (!panel) return;
    panel.classList.remove("show");
    panel.setAttribute("aria-hidden", "true");
    if (toggle) {
      toggle.setAttribute("aria-expanded", "false");
    }
  }

  async function applyToolRequestMode() {
    const mode = document.getElementById("grokRequestMode");
    const keyInput = document.getElementById("grokClientKey");
    const status = document.getElementById("grokClientKeyStatus");
    const nextMode = mode?.value === "client" ? "client" : "admin";
    const nextKey = nextMode === "client" ? String(keyInput?.value || "").trim() : "";
    if (nextMode === "client" && !nextKey) {
      if (status) status.textContent = "请输入 Client Key";
      return;
    }
    persistChatSessionsNow();
    if (chatState.abortController) chatState.abortController.abort();
    chatState.requestGeneration += 1;
    toolAuthState.mode = nextMode;
    toolAuthState.apiKey = nextKey;
    chatState.sessions = [];
    chatState.activeId = "";
    chatState.model = "";
    chatState.models = [];
    chatState.modelsLoaded = false;
    await loadChatModels();
    loadChatSessions();
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    if (status) status.textContent = nextMode === "client" ? "Client Key 已应用（仅当前标签页）" : "管理员会话";
  }

  function bindToolRequestMode() {
    const mode = document.getElementById("grokRequestMode");
    const keyInput = document.getElementById("grokClientKey");
    const apply = document.getElementById("grokClientKeyApply");
    if (!mode) return;
    const sync = () => {
      const client = mode.value === "client";
      keyInput?.classList.toggle("hidden", !client);
      apply?.classList.toggle("hidden", !client);
    };
    mode.addEventListener("change", () => {
      sync();
      if (mode.value !== "client") applyToolRequestMode().catch(() => {});
    });
    apply?.addEventListener("click", () => applyToolRequestMode().catch(() => {}));
    keyInput?.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        applyToolRequestMode().catch(() => {});
      }
    });
    sync();
  }

  async function initChat() {
    bindToolRequestMode();
    await loadChatModels();
    loadChatSessions();
    const uiState = loadGrokToolsUIState();
    if (!chatState.models.includes(chatState.model)) {
      chatState.model = preferredAppChatModel(chatState.models);
    }
    const systemInput = document.getElementById("grokSystemInput");
    if (systemInput && typeof uiState.chatSystemPrompt === "string") {
      systemInput.value = uiState.chatSystemPrompt;
    }
    syncChatSessionSettings(activeChatSession());
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    setChatSendButtonState(false);
    bindChatEvents();
    closeChatSidebar();
    syncChatSidebarState();
  }

  function setVoiceStatus(text, type) {
    const el = document.getElementById("voiceStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "warn" || type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  function appendVoiceLog(line) {
    const el = document.getElementById("voiceLogOutput");
    if (!el) return;
    const time = new Date().toLocaleTimeString();
    el.textContent = `[${time}] ${String(line || "")}\n` + el.textContent;
    el.scrollTop = 0;
  }

  function clearVoiceLog() {
    const el = document.getElementById("voiceLogOutput");
    if (el) el.textContent = "";
  }

  function setVoiceButtons(running) {
    const start = document.getElementById("voiceStartBtn");
    const stop = document.getElementById("voiceStopBtn");
    if (start) {
      start.disabled = false;
      start.classList.toggle("hidden", !!running);
    }
    if (stop) {
      stop.disabled = false;
      stop.classList.toggle("hidden", !running);
    }
  }

  function updateVoiceMeta() {
    const voiceSelect = String(document.getElementById("voiceName")?.value || "ara").trim() || "ara";
    const customVoice = String(document.getElementById("voiceCustomID")?.value || "").trim();
    const voice = voiceSelect === "custom" ? (customVoice || "custom") : voiceSelect;
    const personality = String(document.getElementById("voicePersonality")?.value || "assistant").trim() || "assistant";
    const speed = Math.max(0.1, Number(document.getElementById("voiceSpeed")?.value || 1));
    const customBlock = document.getElementById("voiceCustomBlock");
    const statusVoice = document.getElementById("voiceStatusVoice");
    const statusPersonality = document.getElementById("voiceStatusPersonality");
    const statusSpeed = document.getElementById("voiceStatusSpeed");
    const speedValue = document.getElementById("voiceSpeedValue");
    if (customBlock) customBlock.classList.toggle("hidden", voiceSelect !== "custom");
    if (statusVoice) statusVoice.textContent = voice;
    if (statusPersonality) statusPersonality.textContent = personality;
    if (statusSpeed) statusSpeed.textContent = `${speed}x`;
    if (speedValue) speedValue.textContent = speed.toFixed(1);
    updateVoiceRangeProgress();
  }

  function updateVoiceRangeProgress() {
    const speedRange = document.getElementById("voiceSpeed");
    if (!speedRange) return;
    const min = Number(speedRange.min || 0);
    const max = Number(speedRange.max || 100);
    const val = Number(speedRange.value || 0);
    const pct = max === min ? 0 : ((val - min) / (max - min)) * 100;
    speedRange.style.setProperty("--range-progress", `${pct}%`);
  }

  function buildVoiceVisualizerBars() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    root.innerHTML = "";
    const targetCount = Math.max(36, Math.floor(root.offsetWidth / 7));
    for (let i = 0; i < targetCount; i += 1) {
      const bar = document.createElement("div");
      bar.className = "bar";
      root.appendChild(bar);
    }
  }

  function stopVoiceVisualizer() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    const bars = root.querySelectorAll(".bar");
    bars.forEach((bar) => {
      bar.style.height = "6px";
    });
  }

  function startVoiceVisualizer() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    buildVoiceVisualizerBars();
    if (voiceState.visualizerTimer) return;
    voiceState.visualizerTimer = window.setInterval(() => {
      const bars = root.querySelectorAll(".bar");
      const status = document.getElementById("voiceStatus");
      const connected = status && status.classList.contains("connected");
      bars.forEach((bar) => {
        bar.style.height = connected ? `${Math.random() * 32 + 6}px` : "6px";
      });
    }, 150);
  }

  function resetVoiceAudio() {
    const root = document.getElementById("voiceAudioRoot");
    if (root) root.innerHTML = "";
  }

  function syncVoiceOutputMute() {
    const root = document.getElementById("voiceAudioRoot");
    if (root) {
      root.querySelectorAll("audio").forEach((audio) => {
        audio.muted = !!voiceState.outputMuted;
      });
    }
    const btn = document.getElementById("voiceMuteOutputBtn");
    if (btn) btn.textContent = voiceState.outputMuted ? "恢复输出" : "静音输出";
  }

  async function resetVoiceSession(reason, opts) {
    const options = opts || {};
    const skipDisconnect = !!options.skipDisconnect;
    const statusText = options.statusText === undefined ? t("common.notConnected") : options.statusText;
    const statusType = options.statusType || "";
    const logLine = options.logLine === undefined
      ? (reason ? `Voice session stopped: ${reason}` : "Voice session stopped")
      : options.logLine;

    if (!skipDisconnect && voiceState.room && typeof voiceState.room.disconnect === "function") {
      try {
        await voiceState.room.disconnect();
      } catch (err) {
        // ignore disconnect failures during reset
      }
    }
    voiceState.room = null;
    voiceState.running = false;
    voiceState.stopping = false;
    voiceState.reconnecting = false;
    setVoiceButtons(false);
    stopVoiceVisualizer();
    resetVoiceAudio();
    setVoiceStatus(statusText, statusType);
    if (logLine) appendVoiceLog(logLine);
  }

  function resolveLiveKitClient() {
    return window.LivekitClient || window.LiveKitClient || null;
  }

  function isActiveVoiceRoom(room) {
    return voiceState.room === room && !voiceState.stopping;
  }

  async function ensureLiveKitClient() {
    const cfg = LIVEKIT_CLIENT_VERSIONS.stable;
    const sdk = resolveLiveKitClient();
    if (sdk) {
      appendVoiceLog(`LiveKit SDK 已加载: ${window.__warpLiveKitVersion || "unknown"}`);
      return sdk;
    }
    if (!grokLazyState.livekitPromise) {
      grokLazyState.livekitPromise = new Promise((resolve, reject) => {
        const existing = document.querySelector('script[data-livekit-client="1"]');
        if (existing) {
          existing.addEventListener("load", () => resolve(resolveLiveKitClient()), { once: true });
          existing.addEventListener("error", () => reject(new Error("LiveKit SDK 加载失败")), { once: true });
          return;
        }
        const script = document.createElement("script");
        script.integrity = cfg.integrity;
        script.crossOrigin = "anonymous";
        script.src = cfg.url;
        script.async = true;
        script.dataset.livekitClient = "1";
        script.dataset.livekitVersion = cfg.label;
        script.onload = () => {
          const loaded = resolveLiveKitClient();
          if (loaded) {
            window.__warpLiveKitVersion = cfg.label;
            appendVoiceLog(`LiveKit SDK 已按需加载: ${cfg.label}`);
            resolve(loaded);
            return;
          }
          reject(new Error("LiveKit SDK 未挂载到 window"));
        };
        script.onerror = () => reject(new Error("LiveKit SDK 加载失败"));
        document.head.appendChild(script);
      }).catch((err) => {
        grokLazyState.livekitPromise = null;
        throw err;
      });
    }
    const loaded = await grokLazyState.livekitPromise;
    if (loaded) return loaded;
    appendVoiceLog("LiveKit SDK 未加载");
    showToast("LiveKit SDK 未加载", "error");
    throw new Error("LiveKit SDK 未加载");
  }

  function ensureVoiceMicSupport() {
    const hasMediaDevices = typeof navigator !== "undefined" && navigator.mediaDevices;
    const hasGetUserMedia = !!(hasMediaDevices && typeof navigator.mediaDevices.getUserMedia === "function");
    if (hasGetUserMedia) return;
    const isLocalhost = typeof window !== "undefined" && ["localhost", "127.0.0.1"].includes(window.location.hostname);
    const secureHint = (typeof window !== "undefined" && window.isSecureContext) || isLocalhost
      ? "当前浏览器未暴露麦克风接口"
      : "当前页面不是 HTTPS 或 localhost，浏览器不会开放麦克风";
    throw new Error(secureHint);
  }

  function formatVoiceMicError(err) {
    const raw = String(err?.message || err || "");
    if (/permission denied|notallowederror/i.test(raw)) {
      return "麦克风权限被拒绝，请在浏览器设置中允许麦克风后重试";
    }
    if (/notfounderror|devicesnotfounderror/i.test(raw)) {
      return "未检测到麦克风设备";
    }
    if (/notreadableerror|trackstarterror/i.test(raw)) {
      return "麦克风被占用或不可用";
    }
    return raw || "麦克风不可用";
  }

  function isVoiceMicLikeError(err) {
    const raw = String(err?.message || err || "");
    return /permission denied|notallowederror|notfounderror|devicesnotfounderror|notreadableerror|trackstarterror|microphone/i.test(raw);
  }

  async function enableVoiceMicrophone(LiveKitSDK, room) {
    if (!room?.localParticipant) {
      throw new Error("LiveKit local participant is unavailable");
    }
    if (typeof LiveKitSDK.createLocalTracks === "function" && typeof room.localParticipant.publishTrack === "function") {
      const tracks = await LiveKitSDK.createLocalTracks({ audio: true, video: false });
      for (const track of tracks) {
        if (!isActiveVoiceRoom(room)) return;
        await room.localParticipant.publishTrack(track);
      }
      if (!isActiveVoiceRoom(room)) return;
      appendVoiceLog("Microphone published with createLocalTracks");
      return;
    }
    if (typeof room.localParticipant.setMicrophoneEnabled === "function") {
      if (!isActiveVoiceRoom(room)) return;
      await room.localParticipant.setMicrophoneEnabled(true);
      if (!isActiveVoiceRoom(room)) return;
      appendVoiceLog("Microphone enabled with setMicrophoneEnabled");
      return;
    }
    throw new Error("LiveKit microphone API is unavailable");
  }

  async function fetchVoiceToken() {
    updateVoiceMeta();
    const voiceSelect = String(document.getElementById("voiceName")?.value || "ara").trim() || "ara";
    const customVoice = String(document.getElementById("voiceCustomID")?.value || "").trim();
    const voice = voiceSelect === "custom" ? customVoice : voiceSelect;
    const personality = String(document.getElementById("voicePersonality")?.value || "assistant").trim() || "assistant";
    const speed = Number(document.getElementById("voiceSpeed")?.value || 1);
    if (!voice) {
      throw new Error("请选择声音，或填写自定义 voice_id");
    }

    const res = await fetch("/api/grok/admin/voice/token", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        voice,
        personality,
        speed: speed > 0 ? speed : 1,
      }),
    });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    const token = String(data.token || "");
    const livekitURL = String(data.url || "");
    const urlStatus = document.getElementById("voiceStatusURL");
    if (urlStatus) urlStatus.textContent = livekitURL || "-";
    appendVoiceLog("Fetched voice token");
    return { token, url: livekitURL };
  }

  async function stopVoiceSession(reason) {
    if (voiceState.stopping) return;
    voiceState.stopping = true;
    await resetVoiceSession(reason, {
      skipDisconnect: false,
      statusText: t("common.notConnected"),
      statusType: "",
    });
  }

  async function startVoiceSession() {
    if (voiceState.running) {
      showToast(t("voice.alreadyRunning"), "info");
      return;
    }
    const startBtn = document.getElementById("voiceStartBtn");
    if (startBtn) startBtn.disabled = true;
    setVoiceStatus(t("common.connecting"), "connecting");
    updateVoiceMeta();

    const LiveKitSDK = await ensureLiveKitClient();
    const payload = await fetchVoiceToken();
    if (!payload || !payload.token || !payload.url) {
      throw new Error(t("voice.tokenUnavailable"));
    }

    const room = new LiveKitSDK.Room({
      adaptiveStream: true,
      dynacast: true,
      audioCaptureDefaults: {
        autoGainControl: true,
        echoCancellation: true,
        noiseSuppression: true,
      },
    });
    voiceState.room = room;
    voiceState.running = true;
    voiceState.reconnecting = false;
    appendVoiceLog(`LiveKit session using SDK: ${window.__warpLiveKitVersion || LIVEKIT_CLIENT_VERSIONS.stable.label}`);
    room.on(LiveKitSDK.RoomEvent.ParticipantConnected, (participant) => {
      appendVoiceLog(`Participant connected: ${participant?.identity || "unknown"} sid=${participant?.sid || "-"}`);
    });
    room.on(LiveKitSDK.RoomEvent.ParticipantDisconnected, (participant) => {
      appendVoiceLog(`Participant disconnected: ${participant?.identity || "unknown"} sid=${participant?.sid || "-"}`);
    });
    room.on(LiveKitSDK.RoomEvent.TrackSubscribed, (track) => {
      if (!track || track.kind !== "audio") return;
      const root = document.getElementById("voiceAudioRoot");
      if (!root) return;
      root.innerHTML = "";
      try {
        const el = track.attach();
        el.autoplay = true;
        el.controls = true;
        el.muted = !!voiceState.outputMuted;
        root.appendChild(el);
        syncVoiceOutputMute();
      } catch (err) {
        appendVoiceLog(`Attach audio failed: ${err.message || err}`);
      }
    });
    room.on(LiveKitSDK.RoomEvent.Reconnecting, () => {
      if (!isActiveVoiceRoom(room)) return;
      voiceState.reconnecting = true;
      appendVoiceLog("Voice reconnecting");
      setVoiceStatus("重连中...", "warn");
    });
    room.on(LiveKitSDK.RoomEvent.Reconnected, () => {
      if (!isActiveVoiceRoom(room)) return;
      voiceState.reconnecting = false;
      appendVoiceLog("Voice reconnected");
      setVoiceStatus("已连接", "ok");
    });
    room.on(LiveKitSDK.RoomEvent.ConnectionStateChanged, (state) => {
      const stateText = String(state || "unknown");
      if (!isActiveVoiceRoom(room) && stateText !== "disconnected") return;
      appendVoiceLog(`Connection state: ${stateText}`);
    });
    if (LiveKitSDK.RoomEvent.MediaDevicesError) {
      room.on(LiveKitSDK.RoomEvent.MediaDevicesError, (err) => {
        appendVoiceLog(`Media devices error: ${err?.message || err}`);
      });
    }
    room.on(LiveKitSDK.RoomEvent.Disconnected, (reason) => {
      const text = String(reason || "disconnected");
      if (voiceState.room !== room) return;
      if (voiceState.stopping) {
        appendVoiceLog(`Voice disconnected after stop: ${text}`);
        return;
      }
      appendVoiceLog(`Voice disconnected: ${text}`);
      if (voiceState.reconnecting) {
        setVoiceStatus("重连中...", "warn");
        return;
      }
      resetVoiceSession(reason || "disconnected", {
        skipDisconnect: true,
        statusText: t("common.notConnected"),
        statusType: "",
      }).catch(() => {});
    });

    try {
      await room.connect(payload.url, payload.token);
      appendVoiceLog("Connected to LiveKit signaling server");
      ensureVoiceMicSupport();
      await enableVoiceMicrophone(LiveKitSDK, room);
      if (!isActiveVoiceRoom(room)) return;
      setVoiceStatus(t("voice.connected"), "ok");
      setVoiceButtons(true);
      appendVoiceLog("Voice session connected");
      startVoiceVisualizer();
    } catch (err) {
      appendVoiceLog(`Voice connect failed: ${err?.message || err}`);
      await resetVoiceSession("connect failed", {
        skipDisconnect: false,
        statusText: t("voice.connectFailed"),
        statusType: "error",
        logLine: null,
      });
      if (!err?._voiceLogged) {
        const message = isVoiceMicLikeError(err) ? formatVoiceMicError(err) : String(err?.message || err || "连接失败");
        appendVoiceLog(message);
        setVoiceStatus(message || t("voice.connectFailed"), "error");
        throw Object.assign(new Error(message), { _voiceLogged: true });
      }
      setVoiceStatus(err.message || t("voice.connectFailed"), "error");
      throw err;
    }
  }

  function setVideoStatus(text, type) {
    const el = document.getElementById("videoStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "warn" || type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  function setVideoButtons(running) {
    const start = document.getElementById("videoStartBtn");
    const stop = document.getElementById("videoStopBtn");
    if (start) {
      start.disabled = false;
      start.classList.toggle("hidden", !!running);
    }
    if (stop) {
      stop.disabled = false;
      stop.classList.toggle("hidden", !running);
    }
  }

  function updateVideoMeta() {
    const aspectValue = document.getElementById("videoAspectValue");
    const lengthValue = document.getElementById("videoLengthValue");
    const resolutionValue = document.getElementById("videoResolutionValue");
    const presetValue = document.getElementById("videoPresetValue");
    const ratioInput = document.getElementById("videoRatio");
    const lengthInput = document.getElementById("videoLength");
    const resolutionInput = document.getElementById("videoResolution");
    const presetInput = document.getElementById("videoPreset");
    if (aspectValue && ratioInput) aspectValue.textContent = ratioInput.value || "-";
    if (lengthValue && lengthInput) lengthValue.textContent = `${lengthInput.value || "-"}s`;
    if (resolutionValue && resolutionInput) resolutionValue.textContent = resolutionInput.value || "-";
    // The tile repeats what the select shows, so it reads the option's label
    // ("自定义"), not its wire value ("custom").
    if (presetValue && presetInput) presetValue.textContent = presetInput.selectedOptions?.[0]?.textContent || presetInput.value || "-";
  }

  function setVideoProgress(value) {
    const safe = Math.max(0, Math.min(100, Number(value) || 0));
    videoState.lastProgress = safe;
    const fill = document.getElementById("videoProgressFill");
    const text = document.getElementById("videoProgressText");
    if (fill) fill.style.width = `${safe}%`;
    if (text) text.textContent = `${safe}%`;
  }

  function setVideoIndeterminate(active) {
    const bar = document.getElementById("videoProgressBar");
    if (!bar) return;
    bar.classList.toggle("indeterminate", !!active);
  }

  function stopVideoElapsedTimer() {
    if (videoState.elapsedTimer) {
      clearInterval(videoState.elapsedTimer);
      videoState.elapsedTimer = null;
    }
  }

  function startVideoElapsedTimer() {
    stopVideoElapsedTimer();
    const duration = document.getElementById("videoDurationValue");
    videoState.elapsedTimer = window.setInterval(() => {
      if (!videoState.startAt || !duration) return;
      const seconds = Math.max(0, Math.round((Date.now() - videoState.startAt) / 1000));
      duration.textContent = `${seconds}s`;
    }, 1000);
  }

  function revokeVideoObjectURL(url) {
    if (!url || !videoState.objectURLs.has(url)) return;
    URL.revokeObjectURL(url);
    videoState.objectURLs.delete(url);
  }

  function revokeVideoObjectURLs() {
    for (const url of videoState.objectURLs) URL.revokeObjectURL(url);
    videoState.objectURLs.clear();
  }

  function resetVideoOutput(keepPreview) {
    const stage = document.getElementById("videoStage");
    const empty = document.getElementById("videoEmpty");
    videoState.lastProgress = 0;
    videoState.currentPreviewItem = null;
    setVideoProgress(0);
    setVideoIndeterminate(false);
    if (!keepPreview) {
      revokeVideoObjectURLs();
      if (stage) {
        stage.innerHTML = "";
        stage.classList.add("hidden");
      }
      if (empty) {
        empty.classList.remove("hidden");
      }
      videoState.previewCount = 0;
    }
    const duration = document.getElementById("videoDurationValue");
    if (duration) duration.textContent = "-";
  }

  function normalizeVideoURL(raw) {
    let url = String(raw || "").trim();
    if (!url) return "";
    url = url.replace(/^["'`(<\[]+/, "").replace(/["'`)>\\],.;:]+$/g, "");
    try {
      return new URL(url, window.location.origin).toString();
    } catch (err) {
      return url;
    }
  }

  function videoSizeForRatio(value) {
    switch (String(value || "").trim()) {
      case "16:9":
      case "3:2":
      case "1792x1024":
      case "1280x720":
        return "1792x1024";
      case "9:16":
      case "2:3":
      case "1024x1792":
      case "720x1280":
        return "1024x1792";
      case "1:1":
      case "1024x1024":
        return "1024x1024";
      default:
        return "720x1280";
    }
  }

  function initVideoPreviewSlot() {
    const stage = document.getElementById("videoStage");
    if (!stage) return;
    videoState.previewCount += 1;
    const item = document.createElement("div");
    item.className = "video-item is-pending";
    item.dataset.index = String(videoState.previewCount);

    const header = document.createElement("div");
    header.className = "video-item-bar";

    const title = document.createElement("div");
    title.className = "video-item-title";
    title.textContent = `Video #${videoState.previewCount}`;

    const actions = document.createElement("div");
    actions.className = "video-item-actions";

    const openBtn = document.createElement("a");
    openBtn.className = "btn btn-outline video-open hidden";
    openBtn.target = "_blank";
    openBtn.rel = "noopener";
    openBtn.textContent = "打开";

    const downloadBtn = document.createElement("button");
    downloadBtn.type = "button";
    downloadBtn.className = "btn btn-outline video-download";
    downloadBtn.textContent = "下载";
    downloadBtn.disabled = true;

    actions.appendChild(openBtn);
    actions.appendChild(downloadBtn);
    header.appendChild(title);
    header.appendChild(actions);

    const body = document.createElement("div");
    body.className = "video-item-body";
    body.innerHTML = `<div class="video-item-placeholder">${t("video.generatingPlaceholder")}</div>`;

    const link = document.createElement("div");
    link.className = "video-item-link";

    item.appendChild(header);
    item.appendChild(body);
    item.appendChild(link);
    stage.appendChild(item);
    stage.classList.remove("hidden");
    const empty = document.getElementById("videoEmpty");
    if (empty) empty.classList.add("hidden");
    videoState.currentPreviewItem = item;
  }

  function ensureVideoPreviewSlot() {
    if (!videoState.currentPreviewItem) {
      initVideoPreviewSlot();
    }
    return videoState.currentPreviewItem;
  }

  function updateVideoItemLinks(item, url) {
    if (!item) return;
    const openBtn = item.querySelector(".video-open");
    const downloadBtn = item.querySelector(".video-download");
    const link = item.querySelector(".video-item-link");
    const safeUrl = normalizeVideoURL(url);
    item.dataset.url = safeUrl;
    if (link) {
      link.textContent = safeUrl;
      link.classList.toggle("has-url", !!safeUrl);
    }
    if (openBtn) {
      if (safeUrl) {
        openBtn.href = safeUrl;
        openBtn.classList.remove("hidden");
      } else {
        openBtn.classList.add("hidden");
        openBtn.removeAttribute("href");
      }
    }
    if (downloadBtn) {
      downloadBtn.dataset.url = safeUrl;
      downloadBtn.disabled = !safeUrl;
    }
    if (safeUrl) {
      item.classList.remove("is-pending");
    }
  }

  async function fetchVideoBlobURL(url) {
    const response = await fetch(normalizeVideoURL(url), {
      headers: window.GrokToolRequest?.headers?.({ Accept: "video/*" }) || toolAuthHeaders({ Accept: "video/*" }),
    });
    if (handleUnauthorized(response)) throw new Error("登录已失效");
    if (!response.ok) throw new Error(await response.text() || `HTTP ${response.status}`);
    const objectURL = URL.createObjectURL(await response.blob());
    videoState.objectURLs.add(objectURL);
    return objectURL;
  }

  async function renderVideoFromUrl(url) {
    const container = ensureVideoPreviewSlot();
    if (!container) return;
    const body = container.querySelector(".video-item-body");
    if (!body) return;
    const sourceUrl = normalizeVideoURL(url);
    const previousObjectURL = container.dataset.objectUrl || "";
    const objectURL = await fetchVideoBlobURL(sourceUrl);
    revokeVideoObjectURL(previousObjectURL);
    container.dataset.objectUrl = objectURL;
    const video = document.createElement("video");
    video.controls = true;
    video.preload = "metadata";
    video.src = objectURL;
    body.replaceChildren(video);
    updateVideoItemLinks(container, sourceUrl);
  }

  async function stageMediaFile(file, expectedKind) {
    if (!file) throw new Error("请选择媒体文件");
    const clientMode = toolAuthState.mode === "client";
    const form = new FormData();
    form.set("file", file);
    const res = await fetch(clientMode ? "/grok/v1/media/inputs" : "/api/grok/admin/media/inputs/upload", {
      method: "POST", headers: toolAuthHeaders(), body: form,
    });
    if (!clientMode && handleUnauthorized(res)) throw new Error("登录已失效");
    if (!res.ok) throw new Error(await res.text());
    const payload = await res.json();
    const data = clientMode ? payload : (payload?.data || {});
    if (expectedKind && data.kind !== expectedKind) throw new Error(`请选择${expectedKind === "image" ? "图片" : "视频"}文件`);
    const fileID = String(data.file_id || data.fileId || data.id || "").trim();
    if (!fileID) throw new Error("暂存响应缺少 file_id");
    return fileID;
  }

  async function importMediaURL(url, expectedKind) {
    const clientMode = toolAuthState.mode === "client";
    const res = await fetch(clientMode ? "/grok/v1/media/inputs/import" : "/api/grok/admin/media/inputs/import", {
      method: "POST",
      headers: toolAuthHeaders({ "Content-Type": "application/json" }),
      body: JSON.stringify({ url }),
    });
    if (!clientMode && handleUnauthorized(res)) throw new Error("登录已失效");
    if (!res.ok) throw new Error(await res.text());
    const payload = await res.json();
    const data = clientMode ? payload : (payload?.data || {});
    if (expectedKind && data.kind !== expectedKind) throw new Error(`URL 必须指向${expectedKind === "image" ? "图片" : "视频"}`);
    const fileID = String(data.file_id || data.fileId || data.id || "").trim();
    if (!fileID) throw new Error("暂存响应缺少 file_id");
    return fileID;
  }

  async function stagedVideoInput(fileID, url, expectedKind, importURL) {
    if (fileID) return { file_id: fileID };
    const value = String(url || "").trim();
    if (!value) return null;
    if (importURL && /^https?:\/\//i.test(value)) return { file_id: await importMediaURL(value, expectedKind) };
    return { url: value };
  }

  window.GrokMediaStaging = Object.freeze({ upload: stageMediaFile, importURL: importMediaURL });

  function videoRouteActions(route) {
    const explicit = Array.isArray(route?.video_actions)
      ? route.video_actions.map((value) => String(value || "").trim().toLowerCase()).filter(Boolean)
      : [];
    if (explicit.length) return new Set(explicit);
    // A route that advertises no actions generates only.
    return new Set(["generate"]);
  }

  function syncVideoRouteControls() {
    const route = chatState.routes?.find((item) => item.id === document.getElementById("videoModel")?.value);
    const actions = videoRouteActions(route);
    const constraints = route?.video_constraints || {};
    const action = document.getElementById("videoAction");
    if (!action) return;
    for (const option of action.options) option.disabled = !actions.has(option.value);
    if (action.selectedOptions[0]?.disabled) action.value = actions.has("generate") ? "generate" : [...actions][0] || "generate";
    const generate = action.value === "generate";
    const canReferenceImages = constraints.reference_images === true;
    const canReferenceAudio = constraints.reference_audio === true;
    document.getElementById("videoReferenceURL").disabled = !canReferenceImages || !generate;
    document.getElementById("videoReferenceVoice").disabled = !canReferenceAudio || !generate;
    document.getElementById("videoSourceURL").disabled = generate;
    const resolution = document.getElementById("videoResolution");
    const references = document.getElementById("videoReferenceURL").value || videoState.referenceFileID || document.getElementById("videoReferenceVoice").value;
    const resolutions = Array.isArray(constraints.resolutions) ? new Set(constraints.resolutions) : null;
    for (const option of resolution.options) {
      const unsupported = resolutions ? !resolutions.has(option.value) : option.value === "1080p";
      const blockedByReferences = !!references && option.value === "1080p" && constraints.max_resolution_with_references === "720p";
      option.disabled = unsupported || blockedByReferences;
    }
    if (resolution.selectedOptions[0]?.disabled) resolution.value = "720p";
    resolution.disabled = !generate;
    const length = document.getElementById("videoLength");
    const range = constraints.lengths?.[action.value];
    // Grok Web accepts 1–15 seconds (Free accounts are capped at 6 server-side).
    length.min = Number(range?.min ?? 1);
    length.max = Number(range?.max ?? 15);
    length.disabled = action.value === "edit";
  }

  async function createVideoTask(payload) {
    const route = chatState.routes?.find((item) => item.id === payload.model);
    const action = document.getElementById("videoAction")?.value || "generate";
    let path = `${toolInferencePrefix()}/videos`;
    if (action !== "generate") {
      if (!videoRouteActions(route).has(action)) throw new Error("所选模型不支持此操作");
      path += action === "edit" ? "/edits" : "/extensions";
      const body = { model: payload.model, prompt: payload.prompt };
      const video = await stagedVideoInput(videoState.sourceFileID, document.getElementById("videoSourceURL").value, "video", true);
      if (!video) throw new Error("请填写原视频 URL 或上传原视频");
      body.video = video;
      if (action === "extend") body.duration = payload.seconds;
      payload = body;
    }
    const res = await fetch(path, {
      method: "POST",
      headers: toolAuthHeaders({ "Content-Type": "application/json" }),
      body: JSON.stringify(payload),
    });
    if (handleUnauthorized(res)) return "";
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    return String(data.id || data.task_id || data.request_id || "").trim();
  }

  function stopVideoPoll() {
    if (videoState.pollTimer) {
      clearTimeout(videoState.pollTimer);
      videoState.pollTimer = null;
    }
  }

  function finishVideoRun(hasError) {
    if (!videoState.running) return;
    stopVideoPoll();
    videoState.running = false;
    setVideoButtons(false);
    stopVideoElapsedTimer();
    setVideoIndeterminate(false);
    if (!hasError) {
      setVideoStatus(t("common.done"), "ok");
      setVideoProgress(100);
    }
    const duration = document.getElementById("videoDurationValue");
    if (duration && videoState.startAt) {
      const seconds = Math.max(0, Math.round((Date.now() - videoState.startAt) / 1000));
      duration.textContent = `${seconds}s`;
    }
  }

  async function fetchVideoJob(taskID) {
    const res = await fetch(`${toolInferencePrefix()}/videos/${encodeURIComponent(taskID)}?t=${Date.now()}`, { cache: "no-store", headers: toolAuthHeaders() });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    return await res.json();
  }

  function videoContentURL(taskID) {
    return normalizeVideoURL(`${toolInferencePrefix()}/videos/${encodeURIComponent(taskID)}/content`);
  }

  function videoJobURL(job, taskID) {
    return job?.video?.url || job?.content_url || job?.video_url || videoContentURL(taskID);
  }

  function pollVideoTask(taskID) {
    stopVideoPoll();
    videoState.pollTimer = window.setTimeout(async () => {
      if (!videoState.running || videoState.taskID !== taskID) return;
      try {
        const job = await fetchVideoJob(taskID);
        if (!job) return;
        const progress = Number(job.progress || 0);
        setVideoIndeterminate(false);
        setVideoProgress(progress);
        const status = String(job.status || "").toLowerCase();
        if (status === "completed" || status === "done") {
          await renderVideoFromUrl(videoJobURL(job, taskID));
          finishVideoRun(false);
          return;
        }
        if (status === "failed") {
          const message = job.error?.message || "视频生成失败";
          setVideoStatus(String(message), "error");
          finishVideoRun(true);
          return;
        }
        setVideoStatus(t("common.generating"), "ok");
        pollVideoTask(taskID);
      } catch (err) {
        if (!videoState.running) return;
        setVideoStatus(err.message || t("common.connectionError"), "error");
        finishVideoRun(true);
      }
    }, 1500);
  }

  async function startVideo() {
    if (videoState.running) {
      showToast(t("video.alreadyRunning"), "info");
      return;
    }
    const prompt = String(document.getElementById("videoPrompt")?.value || "").trim();
    if (!prompt) {
      showToast(t("video.enterPrompt"), "error");
      return;
    }
    const effort = String(document.getElementById("videoEffort")?.value || "").trim();
    const payload = {
      model: document.getElementById("videoModel")?.value || "grok-imagine-video",
      prompt,
      size: videoSizeForRatio(document.getElementById("videoRatio")?.value || "3:2"),
      seconds: Number(document.getElementById("videoLength")?.value || 6),
      resolution_name: String(document.getElementById("videoResolution")?.value || "480p"),
      preset: String(document.getElementById("videoPreset")?.value || "custom"),
      input_references: [],
    };
    const imageURL = String(document.getElementById("videoImageUrl")?.value || "").trim();
    const imageRef = videoState.imageFileID ? { file_id: videoState.imageFileID } : (imageURL ? { url: imageURL } : null);
    if (imageRef) payload.input_references = [imageRef];
    if (effort) payload.reasoning_effort = effort;
    updateVideoMeta();
    resetVideoOutput(true);
    initVideoPreviewSlot();
    setVideoButtons(true);
    const startBtn = document.getElementById("videoStartBtn");
    if (startBtn) startBtn.disabled = true;
    setVideoStatus(t("common.connecting"), "connecting");
    videoState.running = true;
    try {
      const taskID = await createVideoTask(payload);
      if (!taskID) {
        throw new Error(t("video.taskEmpty"));
      }
      videoState.taskID = taskID;
      videoState.startAt = Date.now();
      startVideoElapsedTimer();
      setVideoStatus(t("common.generating"), "ok");
      setVideoIndeterminate(true);
      pollVideoTask(taskID);
    } catch (err) {
      videoState.running = false;
      setVideoButtons(false);
      setVideoStatus(err.message || t("video.startFailed"), "error");
      throw err;
    }
  }

  async function stopVideo() {
    videoState.running = false;
    stopVideoPoll();
    stopVideoElapsedTimer();
    setVideoButtons(false);
    setVideoStatus(t("common.idle"));
    setVideoIndeterminate(false);
    videoState.taskID = "";
  }

  function normalizeOnlineAccounts(rawAccounts) {
    const list = Array.isArray(rawAccounts) ? rawAccounts : [];
    const out = [];
    for (const item of list) {
      const token = normalizeOnlineToken(item?.token);
      if (!token) continue;
      out.push({
        ...item,
        token,
        token_masked: String(item?.token_masked || formatTokenMask(token)),
      });
    }
    return out;
  }

  function normalizeOnlineDetails(rawDetails) {
    const list = Array.isArray(rawDetails) ? rawDetails : [];
    const out = [];
    for (const item of list) {
      const token = normalizeOnlineToken(item?.token);
      if (!token) continue;
      out.push({
        ...item,
        token,
        token_masked: String(item?.token_masked || formatTokenMask(token)),
      });
    }
    return out;
  }

  function currentOnlineRows() {
    const rows = [];
    const online = cacheOnlineState.online || {};
    const detailsMap = cacheOnlineState.detailMap;
    if (cacheOnlineState.accounts.length > 0) {
      for (const acc of cacheOnlineState.accounts) {
        const token = normalizeOnlineToken(acc.token);
        if (!token) continue;
        const detail = detailsMap.get(token);
        const isOnlineToken = normalizeOnlineToken(online.token) === token;
        const count = detail ? toNumberOrZero(detail.count) : (isOnlineToken ? toNumberOrZero(online.count) : null);
        const status = String(detail?.status || (isOnlineToken ? online.status : "not_loaded") || "not_loaded");
        const lastClear = detail?.last_asset_clear_at ?? (isOnlineToken ? online.last_asset_clear_at : acc.last_asset_clear_at);
        rows.push({
          token,
          token_masked: String(acc.token_masked || detail?.token_masked || formatTokenMask(token)),
          pool: String(acc.pool || "-"),
          count,
          status,
          account_status: String(acc.status || ""),
          last_asset_clear_at: lastClear,
        });
      }
      return rows;
    }
    for (const detail of cacheOnlineState.details) {
      rows.push({
        token: detail.token,
        token_masked: String(detail.token_masked || formatTokenMask(detail.token)),
        pool: "-",
        count: toNumberOrZero(detail.count),
        status: String(detail.status || "not_loaded"),
        last_asset_clear_at: detail.last_asset_clear_at,
      });
    }
    return rows;
  }

  function syncCacheOnlineSelectAll() {
    const selectAll = document.getElementById("cacheOnlineSelectAll");
    const body = document.getElementById("cacheOnlineBody");
    if (!selectAll || !body) return;
    const checkboxes = Array.from(body.querySelectorAll("input.cache-online-check"));
    if (checkboxes.length === 0) {
      selectAll.checked = false;
      selectAll.indeterminate = false;
      return;
    }
    const selected = checkboxes.filter((item) => item.checked).length;
    selectAll.checked = selected > 0 && selected === checkboxes.length;
    selectAll.indeterminate = selected > 0 && selected < checkboxes.length;
  }

  function renderCacheOnlineTable() {
    const body = document.getElementById("cacheOnlineBody");
    if (!body) return;
    const rows = currentOnlineRows();
    if (rows.length === 0) {
      body.innerHTML = `<tr><td colspan="7" style="text-align:center;color:var(--text-secondary);padding:24px;">暂无在线账号</td></tr>`;
      syncCacheOnlineSelectAll();
      return;
    }

    body.innerHTML = rows.map((row) => {
      const checked = cacheOnlineState.selectedTokens.has(row.token) ? "checked" : "";
      const countText = row.count === null ? "-" : String(row.count);
      const statusText = resolveOnlineStatusText(row.status);
      const accountStatus = String(row.account_status || "").trim();
      const accountStatusText = accountStatus && accountStatus !== "active" ? `账号: ${accountStatus}` : "";
      const lastClear = formatDateTime(row.last_asset_clear_at);
      return `
        <tr>
          <td style="text-align:center;">
            <input type="checkbox" class="cache-online-check" data-token="${encodeURIComponent(row.token)}" ${checked} />
          </td>
          <td><code>${escapeHTML(row.token_masked || formatTokenMask(row.token))}</code></td>
          <td><span class="tag">${escapeHTML(row.pool || "-")}</span></td>
          <td>${escapeHTML(countText)}</td>
          <td>${escapeHTML(statusText)}${accountStatusText ? `<br><small>${escapeHTML(accountStatusText)}</small>` : ""}</td>
          <td>${escapeHTML(lastClear)}</td>
          <td>
            <button class="btn btn-danger-outline cache-online-clear-btn" data-token="${encodeURIComponent(row.token)}" style="padding:4px 8px;">清理</button>
          </td>
        </tr>
      `;
    }).join("");
    syncCacheOnlineSelectAll();
  }

  function applyCacheOnlineData(data) {
    const rawOnline = (data && typeof data === "object") ? (data.online || {}) : {};
    const online = {
      ...rawOnline,
      token: normalizeOnlineToken(rawOnline.token),
    };
    const onlineScope = String(data?.online_scope || "none");
    const accounts = normalizeOnlineAccounts(data?.online_accounts);
    const details = normalizeOnlineDetails(data?.online_details);

    cacheOnlineState.accounts = accounts;
    cacheOnlineState.details = details;
    cacheOnlineState.online = online;
    cacheOnlineState.onlineScope = onlineScope;
    cacheOnlineState.accountMap = new Map();
    cacheOnlineState.detailMap = new Map();
    accounts.forEach((item) => cacheOnlineState.accountMap.set(item.token, item));
    details.forEach((item) => cacheOnlineState.detailMap.set(item.token, item));

    const available = new Set();
    accounts.forEach((item) => available.add(item.token));
    details.forEach((item) => available.add(item.token));
    Array.from(cacheOnlineState.selectedTokens).forEach((token) => {
      if (!available.has(token)) {
        cacheOnlineState.selectedTokens.delete(token);
      }
    });

    const onlineCountEl = document.getElementById("cacheOnlineCount");
    const onlineStatusEl = document.getElementById("cacheOnlineStatus");
    const onlineScopeEl = document.getElementById("cacheOnlineScope");
    const onlineLastClearEl = document.getElementById("cacheOnlineLastClear");
    if (onlineCountEl) onlineCountEl.textContent = String(toNumberOrZero(online.count));
    if (onlineStatusEl) onlineStatusEl.textContent = resolveOnlineStatusText(online.status);
    if (onlineScopeEl) onlineScopeEl.textContent = ONLINE_SCOPE_LABELS[onlineScope] || onlineScope;
    if (onlineLastClearEl) onlineLastClearEl.textContent = formatDateTime(online.last_asset_clear_at);

    renderCacheOnlineTable();
  }

  async function loadCacheSummary(options = {}) {
    const params = new URLSearchParams();
    const tokens = Array.isArray(options.tokens) ? options.tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    const scope = String(options.scope || "").trim().toLowerCase();
    const token = normalizeOnlineToken(options.token);
    if (tokens.length > 0) {
      params.set("tokens", tokens.join(","));
    } else if (scope === "all") {
      params.set("scope", "all");
    } else if (token) {
      params.set("token", token);
    }

    const url = params.toString() ? `/api/grok/admin/cache?${params.toString()}` : "/api/grok/admin/cache";
    const res = await fetch(url);
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    const imageText = `${data?.image?.count || 0} / ${formatBytes(data?.image?.bytes || 0)}`;
    const videoText = `${data?.video?.count || 0} / ${formatBytes(data?.video?.bytes || 0)}`;
    const totalText = `${data?.total?.count || 0} / ${formatBytes(data?.total?.bytes || 0)}`;

    const imageEl = document.getElementById("cacheImageSummary");
    const videoEl = document.getElementById("cacheVideoSummary");
    const totalEl = document.getElementById("cacheTotalSummary");
    const baseEl = document.getElementById("cacheBaseDir");
    if (imageEl) imageEl.textContent = imageText;
    if (videoEl) videoEl.textContent = videoText;
    if (totalEl) totalEl.textContent = totalText;
    if (baseEl) baseEl.textContent = String(data.base_dir || "-");

    applyCacheOnlineData(data);
    return data;
  }

  function renderCacheList(items) {
    const body = document.getElementById("cacheListBody");
    if (!body) return;
    const list = Array.isArray(items) ? items : [];
    if (list.length === 0) {
      body.innerHTML = `<tr><td colspan="5" style="text-align:center;color:var(--text-secondary);padding:24px;">暂无缓存数据</td></tr>`;
      return;
    }

    body.innerHTML = list.map((item) => {
      const mediaType = String(item.media_type || "");
      const name = String(item.name || "");
      const url = String(item.view_url || item.url || "");
      const size = formatBytes(item.size_bytes || item.size || 0);
      const updatedAt = formatDateTime(item.mtime_ms || item.updated_at || 0);
      return `
        <tr>
          <td><span class="tag">${escapeHTML(mediaType)}</span></td>
          <td>${isSafeLinkURL(url) ? `<a href="${escapeHTML(url)}" target="_blank" rel="noopener"><code>${escapeHTML(name)}</code></a>` : `<code>${escapeHTML(name)}</code>`}</td>
          <td>${escapeHTML(size)}</td>
          <td>${escapeHTML(updatedAt)}</td>
          <td>
            <button class="btn btn-danger-outline cache-delete-btn" data-media-type="${encodeURIComponent(mediaType)}" data-name="${encodeURIComponent(name)}" style="padding:4px 8px;">删除</button>
          </td>
        </tr>
      `;
    }).join("");
  }

  function selectedOnlineTokens() {
    return Array.from(cacheOnlineState.selectedTokens);
  }

  async function cancelCacheBatchTask() {
    const taskID = String(cacheBatchState.taskID || "").trim();
    if (!cacheBatchState.running || !taskID) return;
    const res = await fetch(`/api/grok/admin/batch/${encodeURIComponent(taskID)}/cancel`, {
      method: "POST",
    });
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    showToast("已发送取消请求", "info");
  }

  async function startOnlineLoadBatch(payload, label) {
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    const res = await fetch("/api/grok/admin/cache/online/load/async", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload || {}),
    });
    if (handleUnauthorized(res)) return;
    let data = {};
    try {
      data = await res.json();
    } catch (err) {
      // ignore
    }
    if (!res.ok || String(data.status || "") !== "success") {
      throw new Error(data.detail || data.error || (await res.text()) || "请求失败");
    }

    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("创建任务失败：空 task_id");
    }
    const total = toNumberOrZero(data.total);
    beginCacheBatch("load", taskID, total, "在线统计加载中");
    showToast(`开始加载 ${label || "账号"} (${total})`, "info");

    openCacheBatchStream(taskID, {
      onDone: async (msg) => {
        try {
          const result = (msg && typeof msg.result === "object") ? msg.result : null;
          if (result) {
            applyCacheOnlineData(result);
          } else {
            await loadCacheSummary(payload || {});
          }

          let ok = 0;
          let fail = 0;
          const details = Array.isArray(result?.online_details) ? result.online_details : [];
          if (details.length > 0) {
            details.forEach((item) => {
              const status = String(item?.status || "").trim().toLowerCase();
              if (status === "ok") ok++;
              else fail++;
            });
          } else {
            const totalDone = Math.max(toNumberOrZero(msg?.total), toNumberOrZero(data.total));
            ok = totalDone;
          }

          cacheBatchState.processed = Math.max(toNumberOrZero(msg?.total), toNumberOrZero(data.total));
          cacheBatchState.total = cacheBatchState.processed;
          finishCacheBatch("空闲");
          showToast(`在线统计加载完成：成功 ${ok}，失败 ${fail}`, fail > 0 ? "info" : "success");
        } catch (err) {
          finishCacheBatch("失败");
          throw err;
        }
      },
      onCancelled: () => {
        finishCacheBatch("已取消");
        showToast("已终止加载", "info");
      },
      onError: (message) => {
        finishCacheBatch("失败");
        showToast(`加载失败: ${message || "未知错误"}`, "error");
      },
    });
  }

  async function startOnlineClearBatch(tokens) {
    const cleanTokens = Array.isArray(tokens) ? tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    if (cleanTokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }

    const res = await fetch("/api/grok/admin/cache/online/clear/async", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tokens: cleanTokens }),
    });
    if (handleUnauthorized(res)) return;
    let data = {};
    try {
      data = await res.json();
    } catch (err) {
      // ignore
    }
    if (!res.ok || String(data.status || "") !== "success") {
      throw new Error(data.detail || data.error || (await res.text()) || "请求失败");
    }

    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("创建任务失败：空 task_id");
    }
    const total = toNumberOrZero(data.total);
    beginCacheBatch("clear", taskID, total, "在线资产清理中");
    showToast(`开始清理 ${cleanTokens.length} 个账号`, "info");

    openCacheBatchStream(taskID, {
      onDone: async (msg) => {
        try {
          const result = (msg && typeof msg.result === "object") ? msg.result : {};
          const summary = (result && typeof result.summary === "object") ? result.summary : {};
          const ok = toNumberOrZero(summary.ok);
          const fail = toNumberOrZero(summary.fail);
          const doneTotal = Math.max(toNumberOrZero(summary.total), toNumberOrZero(msg?.total), cleanTokens.length);
          cacheBatchState.processed = doneTotal;
          cacheBatchState.total = doneTotal;
          finishCacheBatch("空闲");
          showToast(`在线清理完成：成功 ${ok}，失败 ${fail}`, fail > 0 ? "info" : "success");
          await loadCacheSummary({ tokens: cleanTokens });
        } catch (err) {
          finishCacheBatch("失败");
          throw err;
        }
      },
      onCancelled: () => {
        finishCacheBatch("已取消");
        showToast("已终止清理", "info");
      },
      onError: (message) => {
        finishCacheBatch("失败");
        showToast(`清理失败: ${message || "未知错误"}`, "error");
      },
    });
  }

  async function clearOnlineAssets(tokens) {
    const cleanTokens = Array.isArray(tokens) ? tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    if (cleanTokens.length === 0) {
      throw new Error("no tokens selected");
    }
    const body = cleanTokens.length === 1 ? { token: cleanTokens[0] } : { tokens: cleanTokens };
    const res = await fetch("/api/grok/admin/cache/online/clear", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    return res.json();
  }

  function summarizeOnlineClear(data) {
    if (!data || typeof data !== "object") {
      return { total: 0, success: 0, failed: 0 };
    }
    const result = data.result || {};
    let total = toNumberOrZero(result.total);
    let success = toNumberOrZero(result.success);
    let failed = toNumberOrZero(result.failed);
    if (total > 0 || success > 0 || failed > 0) {
      return { total, success, failed };
    }
    const results = data.results || {};
    Object.values(results).forEach((item) => {
      const sub = item?.result || {};
      total += toNumberOrZero(sub.total);
      success += toNumberOrZero(sub.success);
      failed += toNumberOrZero(sub.failed);
    });
    return { total, success, failed };
  }

  async function loadCacheList() {
    const filter = String(document.getElementById("cacheTypeFilter")?.value || "").trim();
    const fetchByType = async (mediaType) => {
      const url = `/api/grok/admin/cache/list?media_type=${encodeURIComponent(mediaType)}`;
      const res = await fetch(url);
      if (handleUnauthorized(res)) return null;
      if (!res.ok) {
        throw new Error(await res.text());
      }
      const data = await res.json();
      return Array.isArray(data?.items) ? data.items : [];
    };

    if (filter) {
      const items = await fetchByType(filter);
      if (items === null) return;
      renderCacheList(items);
      return;
    }

    const [imageItems, videoItems] = await Promise.all([fetchByType("image"), fetchByType("video")]);
    if (imageItems === null || videoItems === null) return;
    const merged = imageItems.concat(videoItems);
    merged.sort((a, b) => {
      const left = toNumberOrZero(a?.mtime_ms ?? a?.updated_at);
      const right = toNumberOrZero(b?.mtime_ms ?? b?.updated_at);
      return right - left;
    });
    renderCacheList(merged);
  }

  async function refreshCacheView(options = {}) {
    await loadCacheSummary(options);
    await loadCacheList();
  }

  async function loadSelectedOnlineStats() {
    const tokens = selectedOnlineTokens();
    if (tokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    await startOnlineLoadBatch({ tokens }, "选中账号");
  }

  async function loadAllOnlineStats() {
    const allTokens = cacheOnlineState.accounts.map((item) => normalizeOnlineToken(item.token)).filter(Boolean);
    if (allTokens.length === 0) {
      showToast("暂无在线账号", "info");
      return;
    }
    await startOnlineLoadBatch({ scope: "all" }, "全部账号");
  }

  async function clearSelectedOnlineAssets() {
    const tokens = selectedOnlineTokens();
    if (tokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    if (!window.confirm(`确认清理选中的 ${tokens.length} 个账号在线资产？`)) return;
    await startOnlineClearBatch(tokens);
  }

  async function clearSingleOnlineAssets(token) {
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    const cleanToken = normalizeOnlineToken(token);
    if (!cleanToken) return;
    const display = cacheOnlineState.accountMap.get(cleanToken)?.token_masked || formatTokenMask(cleanToken);
    if (!window.confirm(`确认清理账号 ${display} 的在线资产？`)) return;
    const data = await clearOnlineAssets([cleanToken]);
    if (!data) return;
    const summary = summarizeOnlineClear(data);
    showToast(`在线清理完成：成功 ${summary.success}，失败 ${summary.failed}`, "success");
    await loadCacheSummary({ token: cleanToken });
  }

  async function clearCache() {
    const filter = String(document.getElementById("cacheTypeFilter")?.value || "").trim();
    const confirmText = filter ? `确认清空 ${filter} 缓存？` : "确认清空全部缓存？";
    if (!window.confirm(confirmText)) return;

    const requestClear = async (mediaType) => {
      const res = await fetch("/api/grok/admin/cache/clear", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ media_type: mediaType }),
      });
      if (handleUnauthorized(res)) return null;
      if (!res.ok) {
        throw new Error(await res.text());
      }
      return res.json();
    };

    if (filter) {
      const single = await requestClear(filter);
      if (single === null) return;
    } else {
      const result = await Promise.all([requestClear("image"), requestClear("video")]);
      if (result[0] === null || result[1] === null) return;
    }
    showToast("缓存已清空", "success");
    await refreshCacheView();
  }

  async function deleteCacheItem(mediaType, name) {
    const res = await fetch("/api/grok/admin/cache/item/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        media_type: mediaType,
        name,
      }),
    });
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    await refreshCacheView();
  }

  async function ensureGrokTabReady(tab) {
    const nextTab = String(tab || "imagine").toLowerCase();
    if (nextTab === "imagine" && !grokLazyState.imagineReady) {
      if (window.GrokImagine && typeof window.GrokImagine.ensureReady === "function") {
        await window.GrokImagine.ensureReady();
      }
      grokLazyState.imagineReady = true;
      return;
    }
    if (nextTab === "cache" && !grokLazyState.cacheReady) {
      await refreshCacheView();
      grokLazyState.cacheReady = true;
    }
  }

  async function switchGrokToolTab(tab) {
    const nextTab = String(tab || "").toLowerCase();
    try {
      await ensureGrokTabReady(nextTab);
    } catch (err) {
      showToast(`加载 ${nextTab} 失败: ${err.message || err}`, "error");
    }
    if (nextTab !== "chat") {
      closeChatSidebar();
    } else if (!isMobileChatSidebar()) {
      openChatSidebar();
    }
    const sections = {
      chat: document.getElementById("grokChatSection"),
      imagine: document.getElementById("grokImagineSection"),
      video: document.getElementById("grokVideoSection"),
      voice: document.getElementById("grokVoiceSection"),
      cache: document.getElementById("grokCacheSection"),
    };
    Object.keys(sections).forEach((key) => {
      const section = sections[key];
      if (!section) return;
      if (key !== nextTab) {
        section.classList.add("section-hidden");
        section.style.display = "none";
        return;
      }
      section.classList.remove("section-hidden");
      section.style.display = "";
    });

    const tabs = document.querySelectorAll("#grokToolsTabs .tab-item");
    tabs.forEach((btn) => {
      const active = String(btn.dataset.tab || "").toLowerCase() === nextTab;
      btn.classList.toggle("active", active);
      btn.setAttribute("aria-selected", active ? "true" : "false");
      btn.setAttribute("tabindex", active ? "0" : "-1");
      if (active && typeof btn.scrollIntoView === "function") {
        btn.scrollIntoView({ block: "nearest", inline: "nearest", behavior: "smooth" });
      }
    });
    updateGrokCapabilityPresentation(nextTab);
    applyGrokCapabilityControls();
    saveGrokToolsUIState({ activeToolTab: nextTab });
  }

  window.switchGrokToolTab = switchGrokToolTab;

  function bindGrokTabs() {
    const tablist = document.getElementById("grokToolsTabs");
    if (!tablist) return;
    tablist.setAttribute("role", "tablist");
    tablist.querySelectorAll(".tab-item").forEach((btn) => {
      btn.setAttribute("role", "tab");
      btn.setAttribute("type", "button");
    });
    tablist.addEventListener("keydown", (event) => {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      const tabs = Array.from(tablist.querySelectorAll(".tab-item"));
      if (!tabs.length) return;
      event.preventDefault();
      const current = Math.max(0, tabs.findIndex((btn) => btn.classList.contains("active")));
      const step = event.key === "ArrowRight" ? 1 : -1;
      const next = tabs[(current + step + tabs.length) % tabs.length];
      next.focus();
      switchGrokToolTab(next.dataset.tab).catch(() => {});
    });
  }

  function bindEvents() {
    bindGrokTabs();

    const videoStartBtn = document.getElementById("videoStartBtn");
    if (videoStartBtn) {
      videoStartBtn.addEventListener("click", async () => {
        try {
          await startVideo();
        } catch (err) {
          showToast(`启动失败: ${err.message || err}`, "error");
        }
      });
    }
    const videoStopBtn = document.getElementById("videoStopBtn");
    if (videoStopBtn) {
      videoStopBtn.addEventListener("click", async () => {
        try {
          await stopVideo();
        } catch (err) {
          showToast(`停止失败: ${err.message || err}`, "error");
        }
      });
    }
    const videoClearBtn = document.getElementById("videoClearBtn");
    if (videoClearBtn) {
      videoClearBtn.addEventListener("click", () => {
        resetVideoOutput();
        setVideoStatus(t("common.idle"));
      });
    }
    const videoStage = document.getElementById("videoStage");
    if (videoStage) {
      videoStage.addEventListener("click", async (event) => {
        const target = event.target.closest(".video-download");
        if (!target || !videoStage.contains(target)) return;
        event.preventDefault();
        const item = target.closest(".video-item");
        if (!item) return;
        const url = item.dataset.url || target.dataset.url || "";
        if (!url) return;
        try {
          const blobUrl = await fetchVideoBlobURL(url);
          const anchor = document.createElement("a");
          anchor.href = blobUrl;
          const index = item.dataset.index || "";
          anchor.download = index ? `grok_video_${index}.mp4` : "grok_video.mp4";
          document.body.appendChild(anchor);
          anchor.click();
          anchor.remove();
          revokeVideoObjectURL(blobUrl);
        } catch (err) {
          showToast(t("video.downloadFailed"), "error");
        }
      });
    }
    function bindStagedMediaInput({ buttonID, inputID, clearID, urlID, labelID, stateKey, kind }) {
      const button = document.getElementById(buttonID);
      const input = document.getElementById(inputID);
      const clear = document.getElementById(clearID);
      const url = document.getElementById(urlID);
      const label = document.getElementById(labelID);
      button?.addEventListener("click", () => input?.click());
      input?.addEventListener("change", async () => {
        const file = input.files?.[0];
        if (!file) return;
        videoState[stateKey] = "";
        if (label) label.textContent = `${file.name} · 暂存中…`;
        button.disabled = true;
        try {
          videoState[stateKey] = await stageMediaFile(file, kind);
          if (url) url.value = "";
          if (label) label.textContent = `${file.name} · 已暂存`;
          syncVideoRouteControls();
        } catch (err) {
          input.value = "";
          if (label) label.textContent = "暂存失败";
          showToast(err.message || "媒体暂存失败", "error");
        } finally { button.disabled = false; }
      });
      clear?.addEventListener("click", () => {
        videoState[stateKey] = "";
        if (input) input.value = "";
        if (label) label.textContent = "未选择文件";
        syncVideoRouteControls();
      });
      url?.addEventListener("input", () => {
        if (!url.value.trim()) return;
        videoState[stateKey] = "";
        if (input) input.value = "";
        if (label) label.textContent = "使用 URL";
        syncVideoRouteControls();
      });
    }
    bindStagedMediaInput({ buttonID: "videoSelectImageBtn", inputID: "videoImageFileInput", clearID: "videoClearImageBtn", urlID: "videoImageUrl", labelID: "videoImageFileName", stateKey: "imageFileID", kind: "image" });
    bindStagedMediaInput({ buttonID: "videoSelectReferenceBtn", inputID: "videoReferenceFileInput", clearID: "videoClearReferenceBtn", urlID: "videoReferenceURL", labelID: "videoReferenceFileName", stateKey: "referenceFileID", kind: "image" });
    bindStagedMediaInput({ buttonID: "videoSelectSourceBtn", inputID: "videoSourceFileInput", clearID: "videoClearSourceBtn", urlID: "videoSourceURL", labelID: "videoSourceFileName", stateKey: "sourceFileID", kind: "video" });
    const videoPrompt = document.getElementById("videoPrompt");
    if (videoPrompt) {
      videoPrompt.addEventListener("keydown", async (event) => {
        if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
          event.preventDefault();
          try {
            await startVideo();
          } catch (err) {
            showToast(`启动失败: ${err.message || err}`, "error");
          }
        }
      });
    }
    [
      ["videoRatio", "videoRatio"],
      ["videoLength", "videoLength"],
      ["videoResolution", "videoResolution"],
      ["videoPreset", "videoPreset"],
      ["videoEffort", "videoEffort"],
    ].forEach(([id, key]) => {
      const input = document.getElementById(id);
      if (!input) return;
      const sync = () => {
        saveGrokToolsUIState({ [key]: String(input.value || "") });
        if (id !== "videoEffort") updateVideoMeta();
      };
      input.addEventListener("change", sync);
      input.addEventListener("input", sync);
    });

    const voiceStartBtn = document.getElementById("voiceStartBtn");
    if (voiceStartBtn) {
      voiceStartBtn.addEventListener("click", async () => {
        try {
          await startVoiceSession();
        } catch (err) {
          if (!err?._voiceLogged) {
            appendVoiceLog(err.message || "Voice start failed");
          }
          showToast(`Voice 启动失败: ${err.message || err}`, "error");
        }
      });
    }
    const voiceStopBtn = document.getElementById("voiceStopBtn");
    if (voiceStopBtn) {
      voiceStopBtn.addEventListener("click", async () => {
        try {
          await stopVoiceSession();
        } catch (err) {
          showToast(`Voice 停止失败: ${err.message || err}`, "error");
        }
      });
    }
    const voiceClearLogBtn = document.getElementById("voiceClearLogBtn");
    if (voiceClearLogBtn) {
      voiceClearLogBtn.addEventListener("click", () => clearVoiceLog());
    }
    const voiceCopyLogBtn = document.getElementById("voiceCopyLogBtn");
    if (voiceCopyLogBtn) {
      voiceCopyLogBtn.addEventListener("click", () => {
        const content = String(document.getElementById("voiceLogOutput")?.textContent || "").trim();
        if (!content) {
          showToast("暂无可复制日志", "info");
          return;
        }
        copyToClipboard(content);
      });
    }
    const voiceName = document.getElementById("voiceName");
    const voiceCustomID = document.getElementById("voiceCustomID");
    const voicePersonality = document.getElementById("voicePersonality");
    const voiceSpeed = document.getElementById("voiceSpeed");
    const voiceInstruction = document.getElementById("voiceInstruction");
    [voiceName, voiceCustomID, voicePersonality, voiceSpeed, voiceInstruction].forEach((input) => {
      if (!input) return;
      const sync = () => {
        updateVoiceMeta();
        saveGrokToolsUIState({
          voiceName: String(document.getElementById("voiceName")?.value || "ara"),
          voiceCustomID: String(document.getElementById("voiceCustomID")?.value || ""),
          voicePersonality: String(document.getElementById("voicePersonality")?.value || "assistant"),
          voiceSpeed: Number(document.getElementById("voiceSpeed")?.value || 1),
          voiceInstruction: String(document.getElementById("voiceInstruction")?.value || ""),
        });
      };
      input.addEventListener("change", sync);
      input.addEventListener("input", sync);
    });
    const voiceMuteOutputBtn = document.getElementById("voiceMuteOutputBtn");
    if (voiceMuteOutputBtn) {
      voiceMuteOutputBtn.addEventListener("click", () => {
        voiceState.outputMuted = !voiceState.outputMuted;
        syncVoiceOutputMute();
      });
    }

    const cacheRefreshBtn = document.getElementById("cacheRefreshBtn");
    if (cacheRefreshBtn) {
      cacheRefreshBtn.addEventListener("click", async () => {
        try {
          await refreshCacheView();
          showToast("缓存已刷新", "success");
        } catch (err) {
          showToast(`刷新失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheClearBtn = document.getElementById("cacheClearBtn");
    if (cacheClearBtn) {
      cacheClearBtn.addEventListener("click", async () => {
        try {
          await clearCache();
        } catch (err) {
          showToast(`清空失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheFilter = document.getElementById("cacheTypeFilter");
    if (cacheFilter) {
      cacheFilter.addEventListener("change", async () => {
        try {
          await loadCacheList();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheListBody = document.getElementById("cacheListBody");
    if (cacheListBody) {
      cacheListBody.addEventListener("click", async (event) => {
        const btn = event.target.closest(".cache-delete-btn");
        if (!btn || !cacheListBody.contains(btn)) return;
        const mediaType = decodeURIComponent(btn.dataset.mediaType || "");
        const name = decodeURIComponent(btn.dataset.name || "");
        if (!mediaType || !name) return;
        if (!window.confirm(`确认删除 ${mediaType}/${name} ?`)) return;
        try {
          await deleteCacheItem(mediaType, name);
          showToast("删除成功", "success");
        } catch (err) {
          showToast(`删除失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheOnlineBody = document.getElementById("cacheOnlineBody");
    if (cacheOnlineBody) {
      cacheOnlineBody.addEventListener("change", (event) => {
        const input = event.target.closest(".cache-online-check");
        if (!input || !cacheOnlineBody.contains(input)) return;
        const token = normalizeOnlineToken(decodeURIComponent(input.dataset.token || ""));
        if (!token) return;
        if (input.checked) {
          cacheOnlineState.selectedTokens.add(token);
        } else {
          cacheOnlineState.selectedTokens.delete(token);
        }
        syncCacheOnlineSelectAll();
      });
      cacheOnlineBody.addEventListener("click", async (event) => {
        const btn = event.target.closest(".cache-online-clear-btn");
        if (!btn || !cacheOnlineBody.contains(btn)) return;
        const token = normalizeOnlineToken(decodeURIComponent(btn.dataset.token || ""));
        if (!token) return;
        try {
          await clearSingleOnlineAssets(token);
        } catch (err) {
          showToast(`在线清理失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheOnlineSelectAll = document.getElementById("cacheOnlineSelectAll");
    if (cacheOnlineSelectAll) {
      cacheOnlineSelectAll.addEventListener("change", () => {
        const body = document.getElementById("cacheOnlineBody");
        if (!body) return;
        const checked = !!cacheOnlineSelectAll.checked;
        const checkboxes = Array.from(body.querySelectorAll("input.cache-online-check"));
        checkboxes.forEach((item) => {
          const token = normalizeOnlineToken(decodeURIComponent(item.dataset.token || ""));
          if (!token) return;
          item.checked = checked;
          if (checked) {
            cacheOnlineState.selectedTokens.add(token);
          } else {
            cacheOnlineState.selectedTokens.delete(token);
          }
        });
        syncCacheOnlineSelectAll();
      });
    }

    const cacheOnlineLoadSelectedBtn = document.getElementById("cacheOnlineLoadSelectedBtn");
    if (cacheOnlineLoadSelectedBtn) {
      cacheOnlineLoadSelectedBtn.addEventListener("click", async () => {
        try {
          await loadSelectedOnlineStats();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineLoadAllBtn = document.getElementById("cacheOnlineLoadAllBtn");
    if (cacheOnlineLoadAllBtn) {
      cacheOnlineLoadAllBtn.addEventListener("click", async () => {
        try {
          await loadAllOnlineStats();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineClearSelectedBtn = document.getElementById("cacheOnlineClearSelectedBtn");
    if (cacheOnlineClearSelectedBtn) {
      cacheOnlineClearSelectedBtn.addEventListener("click", async () => {
        try {
          await clearSelectedOnlineAssets();
        } catch (err) {
          showToast(`在线清理失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineBatchCancelBtn = document.getElementById("cacheOnlineBatchCancelBtn");
    if (cacheOnlineBatchCancelBtn) {
      cacheOnlineBatchCancelBtn.addEventListener("click", async () => {
        try {
          await cancelCacheBatchTask();
        } catch (err) {
          showToast(`取消失败: ${err.message || err}`, "error");
        }
      });
    }
  }

  // The image panel hides its batch actions while no batch has been rendered.
  // The class is derived from the feed contents so the markup stays in sync with
  // grok-imagine.js prepending batches, marking the empty state and clearing.
  function watchImagineResultActions() {
    const grid = document.getElementById("imagineGrid");
    const actions = document.getElementById("imagineHeaderActions");
    if (!grid || !actions) return;
    const sync = () => {
      actions.classList.toggle("is-empty", grid.querySelector(".imagine-masonry-batch") === null);
    };
    new MutationObserver(sync).observe(grid, { childList: true });
    sync();
  }

  async function init() {
    await initChat();
    bindEvents();
    window.addEventListener("beforeunload", () => {
      if (window.GrokImagine && typeof window.GrokImagine.stop === "function") {
        window.GrokImagine.stop({ silent: true });
      }
        stopVideoElapsedTimer();
      revokeVideoObjectURLs();
      stopVoiceSession().catch(() => {});
      closeCacheBatchStream();
      persistChatSessionsNow();
    });
    const uiState = loadGrokToolsUIState();
    await switchGrokToolTab(String(uiState.activeToolTab || "imagine"));
    if (window.GrokImagine && typeof window.GrokImagine.init === "function") {
      window.GrokImagine.init({ uiState, saveState: saveGrokToolsUIState, showToast });
    }
    watchImagineResultActions();
    const videoRatio = document.getElementById("videoRatio");
    const videoLength = document.getElementById("videoLength");
    const videoResolution = document.getElementById("videoResolution");
    const videoPreset = document.getElementById("videoPreset");
    const videoEffort = document.getElementById("videoEffort");
    if (videoRatio && typeof uiState.videoRatio === "string") videoRatio.value = uiState.videoRatio;
    if (videoLength && typeof uiState.videoLength === "string") videoLength.value = uiState.videoLength;
    if (videoResolution && typeof uiState.videoResolution === "string") videoResolution.value = uiState.videoResolution;
    if (videoPreset && typeof uiState.videoPreset === "string") videoPreset.value = uiState.videoPreset;
    if (videoEffort && typeof uiState.videoEffort === "string") videoEffort.value = uiState.videoEffort;
    resetVideoOutput();
    updateVideoMeta();
    setVideoButtons(false);
    setVideoStatus(t("common.idle"));
    const voiceName = document.getElementById("voiceName");
    const voiceCustomID = document.getElementById("voiceCustomID");
    const voicePersonality = document.getElementById("voicePersonality");
    const voiceSpeed = document.getElementById("voiceSpeed");
    const voiceInstruction = document.getElementById("voiceInstruction");
    if (voiceName && typeof uiState.voiceName === "string" && uiState.voiceName) voiceName.value = uiState.voiceName;
    if (voiceCustomID && typeof uiState.voiceCustomID === "string") voiceCustomID.value = uiState.voiceCustomID;
    if (voicePersonality && typeof uiState.voicePersonality === "string" && uiState.voicePersonality) voicePersonality.value = uiState.voicePersonality;
    if (voiceSpeed && typeof uiState.voiceSpeed === "number" && Number.isFinite(uiState.voiceSpeed)) voiceSpeed.value = String(uiState.voiceSpeed);
    if (voiceInstruction && typeof uiState.voiceInstruction === "string") voiceInstruction.value = uiState.voiceInstruction;
    updateVoiceMeta();
    startVoiceVisualizer();
    stopVoiceVisualizer();
    window.addEventListener("resize", buildVoiceVisualizerBars);
    setVoiceButtons(false);
    setVoiceStatus(t("common.notConnected"));
    syncVoiceOutputMute();
    updateCacheBatchUI();
    await loadGrokCapabilities();
  }

  document.addEventListener("DOMContentLoaded", init);
})();
