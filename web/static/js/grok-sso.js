// Grok Web SSO account import and batch maintenance. Cookies are write-only:
// they leave the browser once, on import, and the server never returns them.
(function () {
  const state = { source: "paste", fileText: "", busy: false };
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
        // JSONL: one JSON value per line.
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

  function currentText() { return state.source === "file" ? state.fileText : ($("grokSSOText")?.value || ""); }

  function updateDetected() {
    const count = parseTokens(currentText()).length;
    const node = $("grokSSODetected");
    if (node) {
      node.textContent = count ? `已识别 ${count} 个 Token（已去重）` : "尚未识别到 Token";
      node.classList.toggle("is-ready", count > 0);
    }
    const submit = $("grokSSOSubmit");
    if (submit) submit.disabled = state.busy || count === 0;
  }

  function setSource(source) {
    state.source = source === "file" ? "file" : "paste";
    document.querySelectorAll("[data-sso-source]").forEach((tab) => {
      const active = tab.dataset.ssoSource === state.source;
      tab.classList.toggle("active", active);
      tab.setAttribute("aria-selected", String(active));
    });
    document.querySelectorAll("[data-sso-pane]").forEach((pane) => { pane.hidden = pane.dataset.ssoPane !== state.source; });
    updateDetected();
  }

  function readFile(file) {
    if (!file) return;
    if (file.size > 4 * 1024 * 1024) { showToast("文件超过 4 MB", "error"); return; }
    const reader = new FileReader();
    reader.onload = () => {
      state.fileText = String(reader.result || "");
      const name = $("grokSSOFileName");
      if (name) name.textContent = file.name;
      updateDetected();
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
      note.textContent = "正在后台同步额度与身份，稍后刷新列表查看。";
      node.appendChild(note);
    }
  }

  async function submit() {
    const tokens = parseTokens(currentText());
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
      showToast(`SSO 导入完成：新增 ${result.created || 0}，重复 ${result.duplicate || 0}`, result.created ? "success" : "info");
      if ($("grokSSOText")) $("grokSSOText").value = "";
      state.fileText = "";
      if (typeof loadAccounts === "function") loadAccounts();
      if (result.refreshing && typeof loadAccounts === "function") setTimeout(loadAccounts, 8000);
    } catch (err) {
      showToast("SSO 导入失败: " + (err.message || err), "error");
    } finally {
      state.busy = false;
      if (button) button.textContent = "导入";
      updateDetected();
    }
  }

  function open() {
    state.fileText = "";
    if ($("grokSSOText")) $("grokSSOText").value = "";
    if ($("grokSSOFileName")) $("grokSSOFileName").textContent = "";
    if ($("grokSSOResult")) { $("grokSSOResult").hidden = true; $("grokSSOResult").innerHTML = ""; }
    setSource("paste");
    ConsoleUI.modal("grokSSOModal", true);
    setTimeout(() => $("grokSSOText")?.focus(), 0);
  }

  function close() { ConsoleUI.modal("grokSSOModal", false); }

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
    if (!ids.length) { showToast("没有可操作的 Grok SSO 账号", "error"); return; }
    if (!confirm(`对 ${ids.length} 个 Grok SSO 账号执行「${label}」？`)) return;
    showToast(`正在${label}…`, "info");
    try {
      const result = await ConsoleAPI.json(path, ConsoleAPI.jsonOptions("POST", { account_ids: ids }));
      showToast(`${label}完成：成功 ${result.ok || 0}，失败 ${result.failed || 0}`, result.failed ? "error" : "success");
    } catch (err) {
      showToast(`${label}失败: ${err.message || err}`, "error");
    } finally {
      if (typeof loadAccounts === "function") loadAccounts();
    }
  }

  function syncToolbar() {
    const platform = typeof currentPlatform === "string" ? currentPlatform : "";
    const visible = !platform || platform === "grok";
    document.querySelectorAll("[data-sso-toolbar]").forEach((node) => { node.hidden = !visible; });
  }

  document.addEventListener("DOMContentLoaded", () => {
    document.querySelectorAll("[data-sso-open]").forEach((button) => button.addEventListener("click", open));
    document.querySelectorAll("[data-sso-close]").forEach((button) => button.addEventListener("click", close));
    document.querySelectorAll("[data-sso-source]").forEach((tab) => tab.addEventListener("click", () => setSource(tab.dataset.ssoSource)));
    $("grokSSOText")?.addEventListener("input", updateDetected);
    $("grokSSOFile")?.addEventListener("change", (event) => readFile(event.target.files?.[0]));
    const drop = $("grokSSODrop");
    if (drop) {
      ["dragenter", "dragover"].forEach((name) => drop.addEventListener(name, (event) => { event.preventDefault(); drop.classList.add("is-over"); }));
      ["dragleave", "drop"].forEach((name) => drop.addEventListener(name, (event) => { event.preventDefault(); drop.classList.remove("is-over"); }));
      drop.addEventListener("drop", (event) => readFile(event.dataTransfer?.files?.[0]));
    }
    $("grokSSOSubmit")?.addEventListener("click", submit);
    $("grokSSORefreshBtn")?.addEventListener("click", () => batch("/api/grok/sso/refresh", "同步 SSO 额度"));
    $("grokSSONSFWBtn")?.addEventListener("click", () => batch("/api/grok/sso/nsfw", "开启 NSFW"));
    syncToolbar();
  });

  globalThis.GrokSSO = Object.freeze({ open, close, parseTokens, syncToolbar });
})();
