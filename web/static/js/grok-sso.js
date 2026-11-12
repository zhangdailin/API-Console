// Grok account connection inside the 添加账号 dialog, following grok2api: the
// Grok channel has two planes, Build (device OAuth) and Web (SSO cookies), and
// the dialog asks which one before it shows a form. Web accounts are imported
// in bulk from pasted tokens or a TXT/JSON file. Cookies are write-only: they
// leave the browser once, on import, and the server never returns them.
(function () {
  const state = { mode: "build", plane: "", busy: false };
  const $ = (id) => document.getElementById(id);

  // parseTokens mirrors the server's accepted shapes so the operator sees the
  // count before submitting. The server re-parses and is the authority.
  function parseTokens(text) {
    const trimmed = String(text || "").trim();
    if (!trimmed) return [];
    const raw = [];
    if (trimmed.startsWith("[") || trimmed.startsWith("{")) {
      try {
        const walk = (value) => {
          if (typeof value === "string") { raw.push(value); return; }
          if (Array.isArray(value)) { value.forEach(walk); return; }
          if (value && typeof value === "object") {
            for (const key of ["token", "sso", "cookie", "client_cookie"]) {
              if (typeof value[key] === "string" && value[key].trim()) { raw.push(value[key]); return; }
            }
            Object.values(value).forEach(walk);
          }
        };
        walk(JSON.parse(trimmed));
      } catch (_) {
        trimmed.split(/\r?\n/).forEach((line) => {
          try { const value = JSON.parse(line); raw.push(typeof value === "string" ? value : (value.token || value.sso || "")); } catch (_) { raw.push(line); }
        });
      }
    } else {
      trimmed.split(/\r?\n/).forEach((line) => {
        line = line.trim();
        if (!line || line.startsWith("#")) return;
        if (line.includes(";") || /sso=/i.test(line)) { raw.push(line); return; }
        line.split(",").forEach((part) => raw.push(part));
      });
    }
    const seen = new Set();
    const out = [];
    raw.forEach((value) => {
      let token = String(value || "").trim().replace(/^["']|["']$/g, "");
      const cookie = token.match(/(?:^|[;\s])sso=([^;\s]+)/i);
      if (cookie) token = cookie[1];
      else if (/^sso=/i.test(token)) token = token.slice(4);
      token = token.trim();
      if (!token || /\s/.test(token) || seen.has(token)) return;
      seen.add(token);
      out.push(token);
    });
    return out;
  }

  function updateDetected() {
    const count = parseTokens($("grokSSOText")?.value || "").length;
    const node = $("grokSSODetected");
    if (node) {
      node.textContent = count ? `已识别 ${count} 个 Token（已去重）` : "尚未识别到 Token";
      node.classList.toggle("is-ready", count > 0);
    }
    const submit = $("grokSSOSubmit");
    if (submit) submit.disabled = state.busy || count === 0;
  }

  // applyMode shows the form of the chosen plane. It runs after the generic
  // modal code has shown the channel's login group, so it only ever narrows.
  function applyMode() {
    const type = String($("accountType")?.value || "").trim().toLowerCase();
    const editing = Boolean(String($("accountId")?.value || ""));
    const grokNew = type === "grok" && !editing;
    const switcher = $("grokConnectMode");
    if (switcher) switcher.hidden = !grokNew;
    document.querySelectorAll("[data-grok-connect]").forEach((tab) => {
      const active = tab.dataset.grokConnect === state.mode;
      tab.classList.toggle("active", active);
      tab.setAttribute("aria-selected", String(active));
    });
    const web = grokNew && state.mode === "web";
    const importGroup = $("grokSSOImportGroup");
    if (importGroup) importGroup.hidden = !web;
    const login = $("grokDeviceLoginGroup");
    if (login && grokNew) login.hidden = web;
    // Imported accounts are always created enabled; the toggle belongs to the
    // single-account forms.
    const enabled = $("accountEnabledGroup");
    if (enabled) enabled.hidden = web;
    if (web) updateDetected();
  }

  function setMode(mode) {
    state.mode = mode === "web" ? "web" : "build";
    applyMode();
    if (state.mode === "web") setTimeout(() => $("grokSSOText")?.focus(), 0);
  }

  // resetForNewAccount is called when the dialog opens: the plane follows the
  // Grok sub-tab the operator is looking at, Build otherwise.
  function resetForNewAccount() {
    state.mode = state.plane === "web" ? "web" : "build";
    if ($("grokSSOText")) $("grokSSOText").value = "";
    const result = $("grokSSOResult");
    if (result) { result.hidden = true; result.innerHTML = ""; }
    applyMode();
  }

  function readFile(file) {
    if (!file) return;
    if (file.size > 4 * 1024 * 1024) { showToast("文件超过 4 MB", "error"); return; }
    const reader = new FileReader();
    reader.onload = () => {
      const text = $("grokSSOText");
      if (text) {
        const current = text.value.trim();
        text.value = current ? `${current}\n${String(reader.result || "")}` : String(reader.result || "");
      }
      updateDetected();
      showToast(`已载入 ${file.name}`, "info");
    };
    reader.readAsText(file);
  }

  const STATUS_LABELS = { created: "已添加", duplicate: "已存在", invalid: "无效", failed: "失败" };

  function renderResult(result) {
    const node = $("grokSSOResult");
    if (!node) return;
    node.hidden = false;
    node.innerHTML = "";
    const summary = document.createElement("div");
    summary.className = "grok-sso-summary";
    [["created", "新增"], ["duplicate", "重复"], ["invalid", "无效"], ["failed", "失败"]].forEach(([key, label]) => {
      const cell = document.createElement("div");
      cell.className = `grok-sso-summary-cell is-${key}`;
      const value = document.createElement("strong");
      value.textContent = String(result[key] || 0);
      const caption = document.createElement("span");
      caption.textContent = label;
      cell.append(value, caption);
      summary.appendChild(cell);
    });
    node.appendChild(summary);
    const items = Array.isArray(result.items) ? result.items.filter((item) => item.status !== "created") : [];
    if (items.length) {
      const list = document.createElement("ul");
      list.className = "grok-sso-items";
      items.slice(0, 200).forEach((item) => {
        const li = document.createElement("li");
        const token = document.createElement("code");
        token.textContent = item.token || "-";
        const status = document.createElement("span");
        status.className = `grok-sso-item-status is-${item.status}`;
        status.textContent = STATUS_LABELS[item.status] || item.status;
        li.append(token, status);
        if (item.account_id) {
          const id = document.createElement("span");
          id.className = "grok-sso-item-meta";
          id.textContent = `#${item.account_id}`;
          li.appendChild(id);
        }
        if (item.error) {
          const err = document.createElement("span");
          err.className = "grok-sso-item-meta";
          err.textContent = item.error;
          li.appendChild(err);
        }
        list.appendChild(li);
      });
      node.appendChild(list);
    }
    if (result.refreshing) {
      const note = document.createElement("p");
      note.className = "grok-sso-note";
      note.textContent = "正在后台同步额度与身份，稍后列表会自动刷新。";
      node.appendChild(note);
    }
  }

  async function submit() {
    const tokens = parseTokens($("grokSSOText")?.value || "");
    if (!tokens.length || state.busy) return;
    state.busy = true;
    const button = $("grokSSOSubmit");
    if (button) { button.disabled = true; button.textContent = "导入中…"; }
    try {
      const result = await ConsoleAPI.json("/api/grok/sso/import", ConsoleAPI.jsonOptions("POST", {
        tokens,
        pool: $("grokSSOPool")?.value || "auto",
        name_prefix: $("grokSSOPrefix")?.value || "",
        weight: parseInt($("grokSSOWeight")?.value || "1", 10) || 1,
        refresh: $("grokSSORefresh")?.checked !== false,
        enable_nsfw: $("grokSSONSFW")?.checked === true,
      }));
      renderResult(result);
      showToast(`导入完成：新增 ${result.created || 0}，重复 ${result.duplicate || 0}`, result.created ? "success" : "info");
      if ($("grokSSOText")) $("grokSSOText").value = "";
      if (typeof loadAccounts === "function") loadAccounts();
      if (result.refreshing && typeof loadAccounts === "function") setTimeout(loadAccounts, 8000);
    } catch (err) {
      showToast("SSO 导入失败: " + (err.message || err), "error");
    } finally {
      state.busy = false;
      if (button) button.textContent = "开始导入";
      updateDetected();
    }
  }

  // ── Grok sub-tabs (Build / Web) and the Web batch toolbar ─────────────────

  function grokPlane() { return state.plane; }

  function matchesPlane(acc) {
    if (!state.plane || String(acc?.account_type || "").toLowerCase() !== "grok") return true;
    // The Build tab lists Build (OAuth) accounts only; Console accounts show
    // under the unfiltered list.
    return state.plane === "web" ? isSidebarGrokWebAccount(acc) : isSidebarGrokOAuthAccount(acc);
  }

  function syncToolbar() {
    const platform = typeof currentPlatform === "string" ? currentPlatform : "";
    const grok = platform === "grok";
    const planes = $("grokPlaneFilters");
    if (planes) planes.hidden = !grok;
    if (!grok && state.plane) state.plane = "";
    document.querySelectorAll("[data-grok-plane]").forEach((tab) => tab.classList.toggle("active", tab.dataset.grokPlane === state.plane));
    const showWebActions = grok && state.plane !== "build";
    document.querySelectorAll("[data-sso-toolbar]").forEach((node) => { node.hidden = !showWebActions; });
  }

  function setPlane(plane) {
    state.plane = plane === "web" || plane === "build" ? plane : "";
    if (typeof currentPage !== "undefined") currentPage = 1;
    if (typeof renderAccounts === "function") renderAccounts();
    else syncToolbar();
  }

  // selectedWebAccountIDs returns the checked Web SSO rows, or every Web SSO
  // account when nothing is checked.
  function selectedWebAccountIDs() {
    const all = (typeof accounts !== "undefined" ? accounts : []).filter((acc) => isSidebarGrokWebAccount(acc));
    const checked = new Set(Array.from(document.querySelectorAll(".row-checkbox:checked")).map((cb) => String(parseDataId(cb.dataset.id || ""))));
    const picked = checked.size ? all.filter((acc) => checked.has(String(acc.id))) : all;
    return picked.map((acc) => acc.id);
  }

  async function batch(path, label) {
    const ids = selectedWebAccountIDs();
    if (!ids.length) { showToast("没有可操作的 Grok Web 账号", "error"); return; }
    if (!confirm(`对 ${ids.length} 个 Grok Web 账号执行「${label}」？`)) return;
    showToast(`正在${label}…`, "info");
    try {
      const result = await ConsoleAPI.json(path, ConsoleAPI.jsonOptions("POST", { account_ids: ids }));
      const firstError = (Array.isArray(result.results) ? result.results : []).find((item) => !item.success && item.error)?.error;
      showToast(`${label}完成：成功 ${result.ok || 0}，失败 ${result.failed || 0}${firstError ? `（${firstError}）` : ""}`, result.failed ? "error" : "success");
    } catch (err) {
      showToast(`${label}失败: ${err.message || err}`, "error");
    } finally {
      if (typeof loadAccounts === "function") loadAccounts();
    }
  }

  document.addEventListener("DOMContentLoaded", () => {
    document.querySelectorAll("[data-grok-connect]").forEach((tab) => tab.addEventListener("click", () => setMode(tab.dataset.grokConnect)));
    document.querySelectorAll("[data-grok-plane]").forEach((tab) => tab.addEventListener("click", () => setPlane(tab.dataset.grokPlane)));
    $("grokSSOText")?.addEventListener("input", updateDetected);
    $("grokSSOUpload")?.addEventListener("click", () => $("grokSSOFile")?.click());
    $("grokSSOFile")?.addEventListener("change", (event) => { readFile(event.target.files?.[0]); event.target.value = ""; });
    $("grokSSOSubmit")?.addEventListener("click", submit);
    $("grokSSORefreshBtn")?.addEventListener("click", () => batch("/api/grok/sso/refresh", "同步额度"));
    $("grokSSONSFWBtn")?.addEventListener("click", () => batch("/api/grok/sso/nsfw", "开启 NSFW"));
    $("grokSSOConvertBuildBtn")?.addEventListener("click", () => batch("/api/grok/sso/convert-build", "转为 Build"));
    $("grokSSOConvertConsoleBtn")?.addEventListener("click", () => batch("/api/grok/sso/convert-console", "转为 Console"));
    syncToolbar();
  });

  globalThis.GrokSSO = Object.freeze({ parseTokens, applyMode, resetForNewAccount, syncToolbar, matchesPlane, grokPlane });
})();
