// Egress management on the configuration page: exit nodes, proxy profiles,
// subscriptions and automation. Every action applies immediately through
// /api/grok/admin/egress/*; nothing here enters the config save bar.
// Proxy addresses, subscription URLs and cookies are write-only: the server
// returns masked addresses and fingerprints, and the forms never prefill them.
(function () {
  const API = "/api/grok/admin/egress";
  const { make, attach, emptyRow, setText } = ConsoleUI;
  const $ = (id) => document.getElementById(id);
  const SCOPES = { grok_build: "Build", grok_web: "Web", grok_web_asset: "Web 资源" };
  const CLEARANCE = { manual: "手动", ready: "就绪", stale: "待刷新", missing: "未获取", invalid: "已失效", not_applicable: "—" };
  const state = {
    started: false, page: 1, pageSize: 20, sortBy: "health", sortOrder: "desc",
    nodes: [], total: 0, selected: new Set(), clearanceMode: "manual", defaultUAs: {},
    profiles: [], sources: [], operations: null, editing: null, editingProfile: null, editingSource: null, busy: false,
  };

  const call = (path, method = "GET", body) => ConsoleAPI.json(API + path, body === undefined ? { method, cache: "no-store" } : ConsoleAPI.jsonOptions(method, body));
  const toast = (message, kind = "success") => (typeof showToast === "function" ? showToast(message, kind) : null);
  const fail = (prefix) => (err) => toast(prefix + "：" + (err.message || err), "error");

  function relativeTime(value) {
    if (!value) return "—";
    const at = new Date(value).getTime();
    if (!Number.isFinite(at) || at <= 0) return "—";
    const seconds = Math.round((Date.now() - at) / 1000);
    if (seconds < 60) return "刚刚";
    if (seconds < 3600) return Math.floor(seconds / 60) + " 分钟前";
    if (seconds < 86400) return Math.floor(seconds / 3600) + " 小时前";
    return Math.floor(seconds / 86400) + " 天前";
  }

  // actionMenu is a disclosure button with a menu of row actions; one open at a time.
  function actionMenu(label, items) {
    const wrap = make("div", "egress-menu");
    const toggle = make("button", "btn btn-outline btn-sm", "操作 ▾");
    toggle.type = "button";
    toggle.setAttribute("aria-haspopup", "menu");
    toggle.setAttribute("aria-expanded", "false");
    toggle.setAttribute("aria-label", label + " 的操作");
    const menu = make("div", "egress-menu-list");
    menu.setAttribute("role", "menu");
    menu.hidden = true;
    const close = () => { menu.hidden = true; toggle.setAttribute("aria-expanded", "false"); };
    for (const [text, handler, danger] of items) {
      const item = make("button", "egress-menu-item" + (danger ? " egress-danger" : ""), text);
      item.type = "button";
      item.setAttribute("role", "menuitem");
      item.addEventListener("click", () => { close(); handler(); });
      menu.appendChild(item);
    }
    toggle.addEventListener("click", (event) => {
      event.stopPropagation();
      const opening = menu.hidden;
      document.querySelectorAll(".egress-menu-list").forEach((other) => { other.hidden = true; });
      document.querySelectorAll(".egress-menu > button").forEach((other) => other.setAttribute("aria-expanded", "false"));
      menu.hidden = !opening;
      toggle.setAttribute("aria-expanded", String(opening));
      if (opening) menu.querySelector("button")?.focus();
    });
    menu.addEventListener("keydown", (event) => { if (event.key === "Escape") { close(); toggle.focus(); } });
    return attach(wrap, [toggle, menu]);
  }

  function pill(text, tone) { return make("span", "egress-pill" + (tone ? " is-" + tone : ""), text); }

  // ── subtabs ─────────────────────────────────────────────────────────────
  function showPane(name) {
    document.querySelectorAll("[data-egress-tab]").forEach((tab) => {
      const active = tab.dataset.egressTab === name;
      tab.classList.toggle("active", active);
      tab.setAttribute("aria-selected", String(active));
      tab.tabIndex = active ? 0 : -1;
    });
    document.querySelectorAll("[data-egress-pane]").forEach((pane) => { pane.hidden = pane.dataset.egressPane !== name; });
    if (name === "profiles") loadProfiles();
    if (name === "sources") loadSources();
    if (name === "operations") loadOperations();
  }

  // ── nodes ───────────────────────────────────────────────────────────────
  function query() {
    const params = new URLSearchParams({ page: String(state.page), page_size: String(state.pageSize), sort_by: state.sortBy, sort_order: state.sortOrder });
    for (const [id, key] of [["egressSearch", "search"], ["egressScopeFilter", "scope"], ["egressEnabledFilter", "enabled"], ["egressProbeFilter", "probe"], ["egressAssignmentFilter", "assignment"]]) {
      const value = $(id)?.value?.trim();
      if (value) params.set(key, value);
    }
    return params.toString();
  }

  async function loadNodes() {
    try {
      const data = await call("/nodes?" + query());
      state.nodes = data.items || [];
      state.total = data.total || 0;
      state.clearanceMode = data.clearance_mode || "manual";
      state.defaultUAs = data.default_user_agents || {};
      const pages = Math.max(1, Math.ceil(state.total / state.pageSize));
      if (state.page > pages) { state.page = pages; return loadNodes(); }
      renderSummary(data.summary || {});
      renderNodes();
      setText("egressPageInfo", state.total ? `共 ${state.total} 个节点 · 第 ${state.page}/${pages} 页` : "");
      ConsoleUI.pagination($("egressPager"), state.page, pages, (page) => { state.page = page; loadNodes(); });
    } catch (err) {
      emptyRow($("egressRows"), 9, "加载失败：" + (err.message || err));
    }
  }

  function renderSummary(summary) {
    const box = $("egressSummary");
    if (!box) return;
    box.replaceChildren();
    const item = (label, value, tone) => attach(make("div", "egress-summary-item" + (tone ? " is-" + tone : "")), [make("strong", "", String(value || 0)), make("span", "", label)]);
    attach(box, [item("节点", summary.total), item("已启用", summary.enabled), item("探测可用", summary.healthy, "ok"), item("探测不可用", summary.unhealthy, summary.unhealthy ? "bad" : "")]);
  }

  function healthMeter(node) {
    const wrap = make("div", "egress-health");
    if (node.health < 0) { wrap.appendChild(make("span", "egress-muted", "未使用")); return wrap; }
    const pct = Math.round(Math.max(0, Math.min(1, node.health)) * 100);
    const tone = node.cooldown_until ? "bad" : pct >= 70 ? "ok" : pct >= 35 ? "warn" : "bad";
    const bar = make("span", "egress-meter is-" + tone);
    const fill = make("span");
    fill.style.width = (node.cooldown_until ? 100 : Math.max(pct, 4)) + "%";
    bar.appendChild(fill);
    attach(wrap, [bar, make("span", "egress-meter-label", node.cooldown_until ? "冷却中" : pct + "%")]);
    if (node.failure_count) wrap.title = `连续失败 ${node.failure_count} 次` + (node.last_error ? `（${node.last_error}）` : "");
    return wrap;
  }

  function probeCell(node) {
    const probe = node.probe;
    if (!probe) return make("span", "egress-muted", "未探测");
    const wrap = make("div", "egress-probe");
    if (probe.status === "healthy") {
      attach(wrap, [pill(probe.latency_ms + " ms", probe.latency_ms > 1500 ? "warn" : "ok"), make("span", "egress-ip", probe.exit_ip || "")]);
      const families = [probe.ipv4 ? "IPv4" : "", probe.ipv6 ? "IPv6" : ""].filter(Boolean).join(" · ");
      if (families) wrap.appendChild(make("span", "egress-muted", families));
    } else {
      attach(wrap, [pill("不可用", "bad"), make("span", "egress-muted", probe.error || "")]);
    }
    wrap.title = `${probe.provider || ""} · ${relativeTime(probe.probed_at)}`;
    return wrap;
  }

  function clearanceCell(node) {
    if (node.scope === "grok_build") return make("span", "egress-muted", "不适用");
    const info = node.clearance;
    if (!info) return make("span", "egress-muted", "—");
    const tone = { ready: "ok", manual: node.cookie_configured ? "ok" : "", stale: "warn", invalid: "bad", missing: "warn" }[info.status] || "";
    const label = info.status === "manual" ? (node.cookie_configured ? "节点 Cookie" : "全局") : (CLEARANCE[info.status] || info.status);
    const wrap = make("div", "egress-probe");
    wrap.appendChild(pill(label, tone));
    if (info.refreshed_at && info.status !== "manual") wrap.appendChild(make("span", "egress-muted", relativeTime(info.refreshed_at)));
    return wrap;
  }

  function renderNodes() {
    const body = $("egressRows");
    if (!body) return;
    if (!state.nodes.length) {
      emptyRow(body, 9, "没有节点。点击“添加节点”或“文本导入”，或在“订阅”中添加订阅。");
      syncSelection();
      return;
    }
    body.replaceChildren();
    for (const node of state.nodes) {
      const tr = make("tr", node.enabled ? "" : "is-disabled");
      const check = make("input");
      check.type = "checkbox";
      check.checked = state.selected.has(node.id);
      check.setAttribute("aria-label", "选择 " + node.name);
      check.addEventListener("change", () => { check.checked ? state.selected.add(node.id) : state.selected.delete(node.id); syncSelection(); });

      const name = make("div", "egress-name");
      const title = attach(make("div", "egress-name-line"), [make("span", "egress-dot " + (node.enabled ? "is-on" : "is-off")), make("strong", "", node.name)]);
      if (!node.routable && node.enabled) {
        const warn = pill("不可路由", "bad");
        warn.title = node.invalid_reason || "";
        title.appendChild(warn);
      }
      name.appendChild(title);
      if (node.source_name) name.appendChild(make("span", "egress-muted", "订阅 · " + node.source_name));

      const proxy = make("div", "egress-proxy");
      if (node.proxy_configured) {
        const code = make("code", "", node.proxy_display);
        code.title = node.proxy_display;
        proxy.appendChild(code);
        const meta = [node.proxy_profile_name ? "配置 · " + node.proxy_profile_name : "", node.proxy_fingerprint ? "#" + node.proxy_fingerprint : "", node.proxy_pool ? "代理池" : ""].filter(Boolean).join(" · ");
        if (meta) proxy.appendChild(make("span", "egress-muted", meta));
      } else {
        proxy.appendChild(pill("直连", ""));
      }

      const accounts = make("span", "", `${node.assigned_account_count} / ${node.account_capacity || "∞"}`);
      const items = [["编辑", () => openNode(node)], ["测试", () => testNode(node)]];
      const tunnel = ["trojan", "vless", "ss", "vmess"].includes(node.proxy_protocol);
      if (node.scope !== "grok_build" && state.clearanceMode !== "manual" && !tunnel) items.push(["刷新 Clearance", () => refreshClearance(node)]);
      items.push(["删除", () => deleteNodes([node.id], node.name), true]);
      const ops = actionMenu(node.name, items);

      const cells = [attach(make("td", "egress-check"), [check]), attach(make("td"), [name]), attach(make("td"), [pill(SCOPES[node.scope] || node.scope, "scope")]),
        attach(make("td"), [proxy]), attach(make("td"), [clearanceCell(node)]), attach(make("td"), [accounts]), attach(make("td"), [healthMeter(node)]),
        attach(make("td"), [probeCell(node)]), attach(make("td", "egress-ops"), [ops])];
      attach(tr, cells);
      body.appendChild(tr);
    }
    ConsoleUI.responsiveTable(body.closest("table"));
    syncSelection();
  }

  function syncSelection() {
    const visible = state.nodes.map((node) => node.id);
    for (const id of [...state.selected]) if (!visible.includes(id)) state.selected.delete(id);
    const count = state.selected.size;
    const bar = $("egressBatchBar");
    if (bar) bar.hidden = count === 0;
    setText("egressSelectedCount", `已选 ${count} 个`);
    const all = $("egressSelectPage");
    if (all) { all.checked = count > 0 && count === visible.length; all.indeterminate = count > 0 && count < visible.length; }
  }

  async function testNode(node) {
    toast(`正在测试 ${node.name}…`, "info");
    try {
      const result = await call(`/nodes/${encodeURIComponent(node.id)}/test`, "POST", {});
      const probe = result.probe || {};
      toast(probe.status === "healthy" ? `${node.name} 可用：${probe.exit_ip}，${probe.latency_ms} ms` : `${node.name} 不可用：${probe.error || "无响应"}`, probe.status === "healthy" ? "success" : "error");
    } catch (err) { fail("测试失败")(err); }
    loadNodes();
  }

  async function refreshClearance(node) {
    toast(`正在为 ${node.name} 求解 Clearance…`, "info");
    try { await call(`/nodes/${encodeURIComponent(node.id)}/refresh-clearance`, "POST", {}); toast(`${node.name} 的 Clearance 已刷新`); }
    catch (err) { fail("刷新失败")(err); }
    loadNodes();
  }

  async function deleteNodes(ids, label) {
    if (!ids.length || !confirm(label ? `删除节点「${label}」？绑定到它的账号会重新分配。` : `删除选中的 ${ids.length} 个节点？`)) return;
    try {
      const result = ids.length === 1 ? await call(`/nodes/${encodeURIComponent(ids[0])}`, "DELETE") : await call("/nodes/batch", "POST", { action: "delete", ids });
      toast(`已删除 ${result.deleted || 0} 个节点`);
      ids.forEach((id) => state.selected.delete(id));
    } catch (err) { fail("删除失败")(err); }
    loadNodes();
  }

  async function batch(action) {
    const ids = [...state.selected];
    if (!ids.length) return;
    if (action === "delete") return deleteNodes(ids);
    try {
      if (action === "test") {
        const result = await call("/nodes/test-all", "POST", { ids });
        toast(`已测试 ${result.tested || 0} 个：可用 ${result.healthy || 0}，不可用 ${result.unhealthy || 0}`);
      } else {
        const result = await call("/nodes/batch", "POST", { action, ids });
        toast(`${action === "enable" ? "已启用" : "已停用"} ${result.updated || 0} 个节点`);
      }
    } catch (err) { fail("操作失败")(err); }
    loadNodes();
  }

  async function testAll() {
    const button = $("egressTestAll");
    button.disabled = true;
    button.textContent = "测试中…";
    try {
      const result = await call("/nodes/test-all", "POST", {});
      toast(`已测试 ${result.tested || 0} 个：可用 ${result.healthy || 0}，不可用 ${result.unhealthy || 0}`);
    } catch (err) { fail("测试失败")(err); }
    button.disabled = false;
    button.textContent = "测试全部";
    loadNodes();
  }

  async function cleanup() {
    try {
      const preview = await call("/nodes/cleanup", "POST", { apply: false });
      const candidates = preview.candidates || [];
      if (!candidates.length) { toast("没有探测不可用的节点", "info"); return; }
      const names = candidates.slice(0, 10).map((c) => "· " + c.name).join("\n") + (candidates.length > 10 ? `\n…共 ${candidates.length} 个` : "");
      if (!confirm(`删除以下最近一次探测不可用的节点？\n${names}`)) return;
      const result = await call("/nodes/cleanup", "POST", { apply: true });
      toast(`已清理 ${result.deleted || 0} 个节点`);
      loadNodes();
    } catch (err) { fail("清理失败")(err); }
  }

  // ── node dialog ─────────────────────────────────────────────────────────
  function syncNodeForm() {
    const scope = $("egressNodeScope").value;
    const profile = $("egressNodeProfile").value;
    const manual = state.clearanceMode === "manual";
    $("egressNodeProxyRow").hidden = !!profile;
    $("egressNodeManualRows").hidden = !(manual && scope !== "grok_build");
    const ua = $("egressNodeUA");
    ua.placeholder = state.defaultUAs[scope] || "默认";
    const note = $("egressNodeClearanceRow");
    note.textContent = scope === "grok_build" ? "Build 不经过 Cloudflare，不需要 Clearance。" : { manual: "Clearance：手动维护，在下方填写此节点的 Cookie 与 UA。", flaresolverr: "Clearance：由 FlareSolverr 按周期为此节点求解。", on_demand: "Clearance：按需求解，上游拒绝时为此节点重新求解。" }[state.clearanceMode];
    $("egressNodeClearProxy").closest("label").hidden = !state.editing?.proxy_configured || !!state.editing?.proxy_profile_id;
    $("egressNodeClearCookies").closest("label").hidden = !state.editing?.cookie_configured;
  }

  async function openNode(node) {
    state.editing = node || null;
    await loadProfiles(true);
    const select = $("egressNodeProfile");
    select.replaceChildren(attach(make("option", "", "不使用（填写下方代理地址）"), []));
    select.firstChild.value = "";
    for (const profile of state.profiles) {
      const option = make("option", "", `${profile.name} · ${profile.proxy_display}`);
      option.value = profile.id;
      select.appendChild(option);
    }
    setText("egressNodeModalTitle", node ? "编辑节点" : "添加节点");
    $("egressNodeEnabled").checked = node ? node.enabled : true;
    $("egressNodeName").value = node?.name || "";
    $("egressNodeScope").value = node?.scope || "grok_web";
    $("egressNodeCapacity").value = node?.account_capacity ? String(node.account_capacity) : "";
    select.value = node?.proxy_profile_id || "";
    $("egressNodeProxy").value = "";
    $("egressNodeProxy").placeholder = node?.proxy_configured && !node.proxy_profile_id ? `已保存 ${node.proxy_display}（留空保留）` : "socks5h://user:pass@host:port";
    $("egressNodeClearProxy").checked = false;
    $("egressNodePool").checked = !!node?.proxy_pool;
    $("egressNodeUA").value = node?.user_agent || "";
    $("egressNodeCookies").value = "";
    $("egressNodeCookies").placeholder = node?.cookie_configured ? "已保存（留空保留）" : "cf_clearance=...; __cf_bm=...";
    $("egressNodeClearCookies").checked = false;
    setText("egressNodeError", "");
    syncNodeForm();
    if (typeof syncToggleElement === "function") document.querySelectorAll("#egressNodeModal .toggle input").forEach(syncToggleElement);
    ConsoleUI.modal("egressNodeModal", true);
    setTimeout(() => $("egressNodeName").focus(), 0);
  }

  async function submitNode(event) {
    event.preventDefault();
    if (state.busy) return;
    const capacity = $("egressNodeCapacity").value.trim();
    const body = {
      name: $("egressNodeName").value.trim(),
      scope: $("egressNodeScope").value,
      enabled: $("egressNodeEnabled").checked,
      proxy_pool: $("egressNodePool").checked,
      proxy_profile_id: $("egressNodeProfile").value,
      account_capacity: capacity ? Number(capacity) : 0,
      user_agent: $("egressNodeUA").value.trim(),
      proxy_url: $("egressNodeProxy").value.trim(),
      cloudflare_cookies: $("egressNodeCookies").value.trim(),
      clear_proxy_url: $("egressNodeClearProxy").checked,
      clear_cookies: $("egressNodeClearCookies").checked,
    };
    if (!body.name) { setText("egressNodeError", "请填写名称"); return; }
    state.busy = true;
    $("egressNodeSubmit").disabled = true;
    try {
      const id = state.editing?.id;
      await call(id ? `/nodes/${encodeURIComponent(id)}` : "/nodes", id ? "PUT" : "POST", body);
      ConsoleUI.modal("egressNodeModal", false);
      toast(id ? "节点已更新" : "节点已添加");
      loadNodes();
    } catch (err) {
      setText("egressNodeError", err.message || String(err));
    } finally {
      state.busy = false;
      $("egressNodeSubmit").disabled = false;
    }
  }

  // ── import dialog ───────────────────────────────────────────────────────
  // countImportLines previews how many lines look like exits; the server parses
  // Clash and Base64 too and is the authority.
  function countImportLines(text) {
    const trimmed = String(text || "").trim();
    if (!trimmed) return 0;
    if (/^\s*proxies\s*:/m.test(trimmed)) return (trimmed.match(/^\s*-\s/mg) || []).length;
    return trimmed.split(/\r?\n/).filter((line) => /^(https?|socks5h?|trojan|vless|ss|vmess):\/\//i.test(line.trim())).length;
  }

  function openImport() {
    $("egressImportName").value = "";
    $("egressImportCapacity").value = "";
    $("egressImportContent").value = "";
    setText("egressImportDetected", "尚未识别到地址");
    setText("egressImportError", "");
    ConsoleUI.modal("egressImportModal", true);
    setTimeout(() => $("egressImportContent").focus(), 0);
  }

  async function submitImport(event) {
    event.preventDefault();
    if (state.busy) return;
    const capacity = $("egressImportCapacity").value.trim();
    state.busy = true;
    $("egressImportSubmit").disabled = true;
    try {
      const result = await call("/imports", "POST", { name: $("egressImportName").value.trim(), scope: $("egressImportScope").value, account_capacity: capacity ? Number(capacity) : 0, content: $("egressImportContent").value });
      ConsoleUI.modal("egressImportModal", false);
      toast(`已导入 ${result.created || 0} 个节点`);
      loadNodes();
    } catch (err) {
      setText("egressImportError", err.message || String(err));
    } finally {
      state.busy = false;
      $("egressImportSubmit").disabled = false;
    }
  }

  // ── proxy profiles ──────────────────────────────────────────────────────
  async function loadProfiles(quiet) {
    try {
      state.profiles = await call("/profiles");
      if (!quiet) renderProfiles();
    } catch (err) { if (!quiet) emptyRow($("egressProfileRows"), 4, "加载失败：" + (err.message || err)); }
  }

  function renderProfiles() {
    const body = $("egressProfileRows");
    if (!body) return;
    if (!state.profiles.length) { emptyRow(body, 4, "还没有代理配置。"); return; }
    body.replaceChildren();
    for (const profile of state.profiles) {
      const edit = make("button", "btn btn-outline btn-sm", "编辑");
      edit.type = "button";
      edit.addEventListener("click", () => openProfile(profile));
      const del = make("button", "btn btn-outline btn-sm egress-danger", "删除");
      del.type = "button";
      del.disabled = profile.bound_node_count > 0;
      del.title = profile.bound_node_count > 0 ? "仍被节点引用，不能删除" : "";
      del.addEventListener("click", async () => {
        if (!confirm(`删除代理配置「${profile.name}」？`)) return;
        try { await call(`/profiles/${encodeURIComponent(profile.id)}`, "DELETE"); toast("已删除"); loadProfiles(); } catch (err) { fail("删除失败")(err); }
      });
      const address = attach(make("div", "egress-proxy"), [make("code", "", profile.proxy_display), make("span", "egress-muted", "#" + profile.proxy_fingerprint)]);
      body.appendChild(attach(make("tr"), [attach(make("td"), [make("strong", "", profile.name)]), attach(make("td"), [address]), make("td", "", String(profile.bound_node_count)), attach(make("td", "egress-ops"), [attach(make("div", "egress-row-actions"), [edit, del])])]));
    }
    ConsoleUI.responsiveTable(body.closest("table"));
  }

  function openProfile(profile) {
    state.editingProfile = profile || null;
    setText("egressProfileModalTitle", profile ? "编辑代理配置" : "新增代理配置");
    $("egressProfileName").value = profile?.name || "";
    $("egressProfileProxy").value = "";
    $("egressProfileProxy").placeholder = profile ? `已保存 ${profile.proxy_display}（留空保留）` : "socks5h://user:pass@host:port";
    $("egressProfileHint").hidden = !profile;
    setText("egressProfileError", "");
    ConsoleUI.modal("egressProfileModal", true);
  }

  async function submitProfile(event) {
    event.preventDefault();
    const id = state.editingProfile?.id;
    try {
      await call(id ? `/profiles/${encodeURIComponent(id)}` : "/profiles", id ? "PUT" : "POST", { name: $("egressProfileName").value.trim(), proxy_url: $("egressProfileProxy").value.trim() });
      ConsoleUI.modal("egressProfileModal", false);
      toast("代理配置已保存");
      loadProfiles();
    } catch (err) { setText("egressProfileError", err.message || String(err)); }
  }

  // ── subscriptions ───────────────────────────────────────────────────────
  async function loadSources() {
    try { state.sources = await call("/sources"); renderSources(); }
    catch (err) { emptyRow($("egressSourceRows"), 7, "加载失败：" + (err.message || err)); }
  }

  function renderSources() {
    const body = $("egressSourceRows");
    if (!body) return;
    if (!state.sources.length) { emptyRow(body, 7, "还没有订阅。"); return; }
    body.replaceChildren();
    for (const source of state.sources) {
      const actions = make("div", "egress-row-actions");
      const add = (label, handler, cls = "btn btn-outline btn-sm") => { const b = make("button", cls, label); b.type = "button"; b.addEventListener("click", () => handler(b)); actions.appendChild(b); };
      add("同步", async (b) => {
        b.disabled = true; b.textContent = "同步中…";
        try { const r = await call(`/sources/${encodeURIComponent(source.id)}/sync`, "POST", {}); toast(`同步完成：${r.entries} 个出口，新增 ${r.created}，移除 ${r.removed}`); }
        catch (err) { fail("同步失败")(err); }
        loadSources();
      });
      add("编辑", () => openSource(source));
      add("删除", async () => {
        if (!confirm(`删除订阅「${source.name}」及其 ${source.node_count} 个节点？`)) return;
        try { await call(`/sources/${encodeURIComponent(source.id)}`, "DELETE"); toast("订阅已删除"); loadSources(); } catch (err) { fail("删除失败")(err); }
      }, "btn btn-outline btn-sm egress-danger");
      const name = attach(make("div", "egress-name"), [attach(make("div", "egress-name-line"), [make("span", "egress-dot " + (source.enabled ? "is-on" : "is-off")), make("strong", "", source.name)]), make("span", "egress-muted", source.url_display)]);
      const sync = make("div", "egress-probe");
      sync.appendChild(make("span", "", relativeTime(source.last_sync_at)));
      if (source.last_sync_error) sync.appendChild(pill(source.last_sync_error, "bad"));
      body.appendChild(attach(make("tr"), [attach(make("td"), [name]), attach(make("td"), [pill(SCOPES[source.scope] || source.scope, "scope")]),
        attach(make("td"), [source.subscription_proxy ? make("code", "", source.subscription_proxy) : pill("直连", "")]), make("td", "", String(source.node_count)),
        attach(make("td"), [sync]), make("td", "", source.default_account_capacity ? String(source.default_account_capacity) : "不限"), attach(make("td", "egress-ops"), [actions])]));
    }
    ConsoleUI.responsiveTable(body.closest("table"));
  }

  function openSource(source) {
    state.editingSource = source || null;
    setText("egressSourceModalTitle", source ? "编辑订阅" : "新增订阅");
    $("egressSourceEnabled").checked = source ? source.enabled : true;
    $("egressSourceName").value = source?.name || "";
    $("egressSourceScope").value = source?.scope || "grok_web";
    $("egressSourceURL").value = "";
    $("egressSourceURL").placeholder = source ? `已保存 ${source.url_display}（留空保留）` : "https://…";
    setText("egressSourceURLHint", source ? "订阅地址只写不读。" : "");
    $("egressSourceUseProxy").checked = !!source?.subscription_proxy;
    $("egressSourceProxy").value = "";
    $("egressSourceProxy").placeholder = source?.subscription_proxy ? `已保存 ${source.subscription_proxy}（留空保留）` : "http://host:port";
    $("egressSourceProxy").hidden = !$("egressSourceUseProxy").checked;
    $("egressSourceInterval").value = String(source?.refresh_interval_seconds || 900);
    $("egressSourceCapacity").value = source?.default_account_capacity ? String(source.default_account_capacity) : "";
    setText("egressSourceError", "");
    if (typeof syncToggleElement === "function") document.querySelectorAll("#egressSourceModal .toggle input").forEach(syncToggleElement);
    ConsoleUI.modal("egressSourceModal", true);
  }

  async function submitSource(event) {
    event.preventDefault();
    const id = state.editingSource?.id;
    const useProxy = $("egressSourceUseProxy").checked;
    const body = {
      name: $("egressSourceName").value.trim(), scope: $("egressSourceScope").value, enabled: $("egressSourceEnabled").checked,
      url: $("egressSourceURL").value.trim(), subscription_proxy_url: useProxy ? $("egressSourceProxy").value.trim() : "",
      clear_subscription_proxy: !useProxy, refresh_interval_seconds: Number($("egressSourceInterval").value) || 900,
      default_account_capacity: Number($("egressSourceCapacity").value) || 0,
    };
    try {
      const saved = await call(id ? `/sources/${encodeURIComponent(id)}` : "/sources", id ? "PUT" : "POST", body);
      ConsoleUI.modal("egressSourceModal", false);
      toast("订阅已保存");
      if (!id && saved?.id) {
        try { const r = await call(`/sources/${encodeURIComponent(saved.id)}/sync`, "POST", {}); toast(`首次同步：${r.entries} 个出口`); }
        catch (err) { fail("首次同步失败")(err); }
      }
      loadSources();
    } catch (err) { setText("egressSourceError", err.message || String(err)); }
  }

  // ── automation ──────────────────────────────────────────────────────────
  function setInterval2(inputId, seconds) {
    const unit = $(inputId + "Unit");
    const factor = [3600, 60, 1].find((f) => seconds % f === 0 && seconds >= f) || 1;
    unit.value = String(factor);
    $(inputId).value = String(seconds / factor);
  }

  function readInterval(inputId) { return Math.round((Number($(inputId).value) || 0) * Number($(inputId + "Unit").value || 1)); }

  async function loadOperations() {
    try {
      const [ops, nodes] = await Promise.all([call("/operations"), call("/nodes?page_size=200&enabled=true")]);
      state.operations = ops;
      $("egressProbeProvider").value = ops.probe_provider || "cloudflare";
      setInterval2("egressProbeInterval", ops.probe_interval_seconds || 900);
      setInterval2("egressAssignInterval", ops.assignment_interval_seconds || 300);
      $("egressAutoAssign").checked = !!ops.auto_assign_enabled;
      $("egressAutoBalance").checked = !!ops.auto_balance_enabled;
      if (typeof syncToggleElement === "function") [$("egressAutoAssign"), $("egressAutoBalance")].forEach(syncToggleElement);
      renderFallbacks(ops, nodes.items || []);
      setText("egressOpsStatus", "");
    } catch (err) { setText("egressOpsStatus", "加载失败：" + (err.message || err)); }
  }

  function renderFallbacks(ops, nodes) {
    const box = $("egressFallbacks");
    box.replaceChildren();
    for (const [scope, label] of Object.entries(SCOPES)) {
      const current = (ops.fallbacks || {})[scope] || { mode: "direct" };
      const row = make("div", "egress-fallback-row");
      const mode = make("select", "form-input");
      mode.setAttribute("aria-label", label + " 兜底");
      mode.dataset.scope = scope;
      for (const [value, text] of [["direct", "通用代理"], ["none", "拒绝请求"], ["fixed", "固定节点"]]) { const o = make("option", "", text); o.value = value; mode.appendChild(o); }
      mode.value = current.mode;
      const node = make("select", "form-input");
      node.setAttribute("aria-label", label + " 固定节点");
      node.dataset.scopeNode = scope;
      const candidates = nodes.filter((n) => n.routable && !n.proxy_pool && n.proxy_configured && (n.scope === scope || (scope === "grok_web_asset" && n.scope === "grok_web")));
      for (const n of candidates) { const o = make("option", "", `${n.name} · ${SCOPES[n.scope]}`); o.value = n.id; node.appendChild(o); }
      if (!candidates.length) { const o = make("option", "", "没有可用的固定代理节点"); o.value = ""; node.appendChild(o); }
      node.value = current.node_id || node.value;
      node.hidden = mode.value !== "fixed";
      mode.addEventListener("change", () => { node.hidden = mode.value !== "fixed"; });
      attach(row, [make("span", "egress-fallback-label", label), mode, node]);
      box.appendChild(row);
    }
  }

  async function saveOperations() {
    const fallbacks = {};
    document.querySelectorAll("#egressFallbacks select[data-scope]").forEach((select) => {
      const scope = select.dataset.scope;
      fallbacks[scope] = { mode: select.value };
      if (select.value === "fixed") fallbacks[scope].node_id = document.querySelector(`#egressFallbacks select[data-scope-node="${scope}"]`).value;
    });
    const body = {
      probe_provider: $("egressProbeProvider").value, probe_interval_seconds: readInterval("egressProbeInterval"), assignment_interval_seconds: readInterval("egressAssignInterval"),
      auto_assign_enabled: $("egressAutoAssign").checked, auto_balance_enabled: $("egressAutoBalance").checked, fallbacks,
    };
    try { await call("/operations", "PUT", body); toast("自动化设置已保存"); loadOperations(); }
    catch (err) { setText("egressOpsStatus", "保存失败：" + (err.message || err)); fail("保存失败")(err); }
  }

  async function rebalance() {
    const button = $("egressRebalance");
    button.disabled = true;
    try {
      const r = await call("/operations/rebalance", "POST", {});
      toast(`重新分配完成：${r.accounts} 个账号，新分配 ${r.assigned}，迁移 ${r.moved}，未安置 ${r.unplaced}`);
    } catch (err) { fail("重新分配失败")(err); }
    button.disabled = false;
  }

  // ── wiring ──────────────────────────────────────────────────────────────
  function bind() {
    document.addEventListener("click", (event) => {
      if (event.target.closest && event.target.closest(".egress-menu")) return;
      document.querySelectorAll(".egress-menu-list").forEach((menu) => { menu.hidden = true; });
    });
    document.querySelectorAll("[data-egress-tab]").forEach((tab) => tab.addEventListener("click", () => showPane(tab.dataset.egressTab)));
    let searchTimer;
    $("egressSearch")?.addEventListener("input", () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { state.page = 1; loadNodes(); }, 250); });
    ["egressScopeFilter", "egressEnabledFilter", "egressProbeFilter", "egressAssignmentFilter"].forEach((id) => $(id)?.addEventListener("change", () => { state.page = 1; loadNodes(); }));
    document.querySelectorAll(".egress-sort").forEach((button) => button.addEventListener("click", () => {
      const key = button.dataset.sort;
      state.sortOrder = state.sortBy === key && state.sortOrder === "desc" ? "asc" : "desc";
      state.sortBy = key;
      document.querySelectorAll(".egress-sort").forEach((b) => b.removeAttribute("aria-sort"));
      button.setAttribute("aria-sort", state.sortOrder === "desc" ? "descending" : "ascending");
      loadNodes();
    }));
    $("egressSelectPage")?.addEventListener("change", (event) => {
      state.nodes.forEach((node) => (event.target.checked ? state.selected.add(node.id) : state.selected.delete(node.id)));
      renderNodes();
    });
    document.querySelectorAll("[data-egress-batch]").forEach((button) => button.addEventListener("click", () => batch(button.dataset.egressBatch)));
    $("egressRefresh")?.addEventListener("click", loadNodes);
    $("egressTestAll")?.addEventListener("click", testAll);
    $("egressCleanup")?.addEventListener("click", cleanup);
    $("egressAddOpen")?.addEventListener("click", () => openNode(null));
    $("egressImportOpen")?.addEventListener("click", openImport);
    $("egressImportContent")?.addEventListener("input", () => {
      const count = countImportLines($("egressImportContent").value);
      setText("egressImportDetected", count ? `识别到约 ${count} 个地址（以服务器解析为准）` : "尚未识别到地址");
    });
    $("egressNodeForm")?.addEventListener("submit", submitNode);
    $("egressImportForm")?.addEventListener("submit", submitImport);
    $("egressProfileForm")?.addEventListener("submit", submitProfile);
    $("egressSourceForm")?.addEventListener("submit", submitSource);
    ["egressNodeScope", "egressNodeProfile"].forEach((id) => $(id)?.addEventListener("change", syncNodeForm));
    $("egressSourceUseProxy")?.addEventListener("change", () => { $("egressSourceProxy").hidden = !$("egressSourceUseProxy").checked; });
    $("egressProfileAdd")?.addEventListener("click", () => openProfile(null));
    $("egressSourceAdd")?.addEventListener("click", () => openSource(null));
    $("egressOpsSave")?.addEventListener("click", saveOperations);
    $("egressRebalance")?.addEventListener("click", rebalance);
    document.querySelectorAll("[data-egress-close]").forEach((button) => button.addEventListener("click", () => ConsoleUI.modal(button.dataset.egressClose, false)));
  }

  // activate loads the node list the first time the delivery panel opens.
  function activate() {
    if (state.started || !$("egressCard")) return;
    state.started = true;
    loadNodes();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", bind);
  else bind();
  globalThis.EgressNodes = Object.freeze({ activate, reload: loadNodes, countImportLines });
})();
