const { make, attach, setText } = ConsoleUI;
// Configuration management JavaScript

// ── element builders ─────────────────────────────────────────────────────────
// The API Key list uses small DOM builders to keep rendering readable.




// styled applies a CSS declaration block. Inline styles are kept where they are
// (a one-off cell width or colour), not replaced by a class on the page's sheet.
function styled(node, styles) {
  Object.keys(styles || {}).forEach((key) => { node.style[key] = styles[key]; });
  return node;
}



// setValue / setChecked guard every control write: a page section that is not
// rendered (or a template that changes) must not throw on load.
function setValue(id, value) {
  const node = document.getElementById(id);
  if (node) node.value = value == null ? "" : String(value);
}

// openModal / closeModal are the two states of every dialog on this page: the
// .active class drives the transition and display carries the layout.
function openModal(id) { ConsoleUI.modal(id, true); }

function closeModal(id) { ConsoleUI.modal(id, false); }

let apiKeys = [];
let createdKeys = [];

// Switch between config tabs
function switchConfigTab(tab) {
  document.querySelectorAll("#configTabs .tab-item").forEach(btn => {
    btn.classList.toggle("active",
      (tab === 'basic' && btn.textContent.includes('基础')) ||
      (tab === 'auth' && btn.textContent.includes('API Key'))
    );
  });
  document.getElementById("basicConfig").style.display = tab === 'basic' ? 'block' : 'none';
  document.getElementById("authConfig").style.display = tab === 'auth' ? 'block' : 'none';

  if (tab === 'auth') loadApiKeys();
}

// Toggle password visibility
function togglePassword(fieldId) {
  const field = document.getElementById(fieldId);
  if (field) { field.type = field.type === 'password' ? 'text' : 'password'; }
}

// ── Unsaved-change tracking ───────────────────────────────────────────────────
// The sticky save bar compares the live control values against the values the
// server returned on load. Only the fields the save payload actually reads are
// counted.
// parseAnonymousAllowIPs turns the textarea into a list of trimmed, non-empty
// entries. An empty box means "nobody", which is the reference behaviour.
function parseAnonymousAllowIPs() {
  const field = document.getElementById("cfg_anonymous_allow_ips");
  if (!field) return [];
  return field.value
    .split("\n")
    .map((entry) => entry.trim())
    .filter(Boolean);
}

const PERFORMANCE_FIELDS = { shared_stream_idle_timeout_seconds: 300, workbuddy_default_max_tokens: 8192, first_token_timeout_seconds: 60, request_timeout: 7200, concurrency_timeout: 7200, qoder_queue_wait_budget_ms: 0, qoder_queue_retry_interval_ms: 0 };
const CONFIG_TRACKED_FIELDS = [
  ...Object.keys(PERFORMANCE_FIELDS).map(key => "cfg_" + key),
  "cfg_admin_pass",
  "cfg_anonymous_allow_ips",
  "cfg_proxy_url",
  "cfg_proxy_bypass",
];
let configBaseline = null;
let configSaving = false;

function readConfigFieldValue(id) {
  const field = document.getElementById(id);
  if (!field) return null;
  if (field.type === "checkbox") return field.checked ? "true" : "false";
  return field.value;
}

function collectConfigState(ids) {
  const state = {};
  ids.forEach((id) => {
    const value = readConfigFieldValue(id);
    if (value !== null) state[id] = value;
  });
  return state;
}

// main.css paints a switch from `.toggle.active`; keep that class in step with
// the native checkbox so a hydrated switch is never drawn off while checked.
function syncToggleElement(checkbox) {
  const label = checkbox && checkbox.closest ? checkbox.closest(".toggle") : null;
  if (label) label.classList.toggle("active", !!checkbox.checked);
}

function syncAllToggleStates() { document.querySelectorAll(".toggle input[type=checkbox]").forEach(syncToggleElement); }

function countConfigChanges() {
  if (!configBaseline) return 0;
  return CONFIG_TRACKED_FIELDS.reduce((count, id) => {
    const current = readConfigFieldValue(id);
    if (current === null || !(id in configBaseline)) return count;
    return current === configBaseline[id] ? count : count + 1;
  }, 0);
}

function setConfigSaveError(message) {
  const errorEl = document.getElementById("cfgSaveError");
  if (!errorEl) return;
  errorEl.textContent = message || "";
  errorEl.classList.toggle("hidden", !message);
}

function renderConfigDirtyState() {
  const dirtyEl = document.getElementById("cfgDirtyState");
  const cleanEl = document.getElementById("cfgCleanState");
  const resetBtn = document.getElementById("cfgResetBtn");
  const count = countConfigChanges();

  if (resetBtn) resetBtn.disabled = count === 0;

  if (!configBaseline) {
    // A failed load leaves nothing to compare against; say so instead of
    // claiming the settings already match the server.
    if (dirtyEl) dirtyEl.classList.add("hidden");
    if (cleanEl) {
      cleanEl.textContent = "配置未加载，可直接编辑后保存";
      cleanEl.classList.remove("hidden");
    }
    return;
  }

  if (dirtyEl) {
    dirtyEl.textContent = count + " 项未保存";
    dirtyEl.classList.toggle("hidden", count === 0);
  }
  if (cleanEl) {
    cleanEl.textContent = "已与服务器同步";
    cleanEl.classList.toggle("hidden", count > 0);
  }
}

function captureConfigBaseline() {
  configBaseline = collectConfigState(CONFIG_TRACKED_FIELDS);
  syncAllToggleStates();
  setConfigSaveError("");
  renderConfigDirtyState();
}

function handleConfigFieldChange() {
  syncAllToggleStates();
  renderConfigDirtyState();
}

function bindConfigDirtyTracking() {
  CONFIG_TRACKED_FIELDS.forEach((id) => {
    const field = document.getElementById(id);
    if (!field) return;
    field.addEventListener("input", handleConfigFieldChange);
    field.addEventListener("change", handleConfigFieldChange);
  });
  // Checkboxes outside the tracked set (the API key rows) still paint correctly.
  document.addEventListener("change", (event) => {
    const target = event.target;
    if (target && target.type === "checkbox") syncToggleElement(target);
  });
}

// Discard local edits and fall back to the values the server returned on load.
function resetConfigChanges() {
  if (!configBaseline) return;
  CONFIG_TRACKED_FIELDS.forEach((id) => {
    const field = document.getElementById(id);
    if (!field || !(id in configBaseline)) return;
    if (field.type === "checkbox") {
      field.checked = configBaseline[id] === "true";
    } else { field.value = configBaseline[id]; }
  });
  setConfigSaveError("");
  handleConfigFieldChange();
  showToast("已重置为服务器上的配置");
}

// ── Group nav (sticky left rail on the basic tab) ─────────────────────────────
function setActiveConfigNav(group) {
  const nav = document.getElementById("configNav");
  if (!nav) return;
  nav.querySelectorAll(".config-nav-link").forEach((link) => {
    link.classList.toggle("active", link.getAttribute("data-nav-group") === group);
  });
}

function bindConfigNav() {
  const nav = document.getElementById("configNav");
  if (!nav) return;
  const links = Array.prototype.slice.call(nav.querySelectorAll(".config-nav-link"));
  if (links.length === 0) return;

  links.forEach((link) => {
    link.addEventListener("click", () => setActiveConfigNav(link.getAttribute("data-nav-group")));
  });

  // The highlight follows the last section whose heading passed the fold, so it
  // stays honest after a manual scroll as well as after a click.
  const highlight = () => {
    let current = links[0].getAttribute("data-nav-group");
    links.forEach((link) => {
      const section = document.getElementById(link.getAttribute("data-nav-group"));
      if (section && section.getBoundingClientRect().top <= 160) { current = link.getAttribute("data-nav-group"); }
    });
    setActiveConfigNav(current);
  };

  window.addEventListener("scroll", highlight, { passive: true });
  highlight();
}

function parseProxyBypass(raw) {
  if (!raw) return [];
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function normalizeProxyBypass(value) {
  if (Array.isArray(value)) return value;
  if (typeof value === "string") return parseProxyBypass(value);
  return [];
}

// Load configuration from API. Returns true only when the server values were
// applied: a failed load must not become the baseline the save bar diffs against.
function setConfigControlValue(id, value) {
  const field = document.getElementById(id);
  if (field) field.value = value == null ? "" : String(value);
}

function applyConfigurationPayload(cfg) {
  if (!cfg || typeof cfg !== "object" || Array.isArray(cfg)) { throw new Error("配置接口返回格式无效"); }
  setConfigControlValue("cfg_admin_pass", cfg.admin_password || cfg.admin_pass || "");
  setConfigControlValue("cfg_anonymous_allow_ips", Array.isArray(cfg.anonymous_allow_ips) ? cfg.anonymous_allow_ips.join("\n") : "");
  Object.entries(PERFORMANCE_FIELDS).forEach(([key, fallback]) => setConfigControlValue("cfg_" + key, cfg[key] ?? fallback));
  setConfigControlValue("cfg_proxy_url", cfg.proxy_url || "");
  setConfigControlValue("cfg_proxy_bypass", normalizeProxyBypass(cfg.proxy_bypass).join("\n"));
}

async function loadConfiguration() {
  try {
    const payload = await ConsoleAPI.json('/api/config/list', { cache: 'no-store' });
    if (payload && typeof payload.code !== 'undefined' && payload.code !== 0) { throw new Error(ConsoleAPI.detail(payload, '加载配置失败')); }
    applyConfigurationPayload(payload?.data ?? payload);
    setConfigSaveError("");
    return true;
  } catch (err) {
    // Without a baseline the save bar reports that nothing was loaded. Preserve
    // the actual cause in the page and toast instead of collapsing every HTTP,
    // JSON and DOM compatibility error into the same opaque message.
    renderConfigDirtyState();
    const reason = err?.message || String(err || "未知错误");
    setConfigSaveError("加载失败：" + reason);
    showToast("配置加载失败：" + reason, "error");
    return false;
  }
}

// Save configuration to API
async function saveConfiguration() {
  if (configSaving) return;
  const proxyBypassRaw = document.getElementById("cfg_proxy_bypass").value;
  const data = {
    admin_password: document.getElementById("cfg_admin_pass").value,
    anonymous_allow_ips: parseAnonymousAllowIPs(),
    proxy_url: document.getElementById("cfg_proxy_url").value.trim(),
    proxy_bypass: parseProxyBypass(proxyBypassRaw),
  };

  for (const key of Object.keys(PERFORMANCE_FIELDS)) {
    const input = document.getElementById("cfg_" + key);
    if (!input) continue;
    const value = Number(input.value);
    if (input.value.trim() === "" || (key === "shared_stream_idle_timeout_seconds" && (value < 30 || value > 600)) || (value < 0 && !["first_token_timeout_seconds", "qoder_queue_wait_budget_ms"].includes(key)) || !Number.isSafeInteger(value) || value < -1 || value > (key.includes("tokens") ? 131072 : key.endsWith("_ms") ? 86400000 : 86400) || ((key === "request_timeout" || key === "concurrency_timeout" || key === "workbuddy_default_max_tokens") && value < 1)) {
      setConfigSaveError("性能配置必须填写范围内的整数"); return;
    }
    data[key] = value;
  }
  const saveBtn = document.getElementById("cfgSaveBtn");
  configSaving = true;
  if (saveBtn) {
    saveBtn.disabled = true;
    saveBtn.textContent = "保存中…";
  }

  try {
    const res = await ConsoleAPI.request("/api/config/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data)
    });
    if (!res.ok) throw new Error(await res.text());
    const payload = await res.json();
    if (payload.code !== 0) { throw new Error(payload.message || payload.msg || "保存失败"); }
    setConfigSaveError("");
    showToast("配置保存成功");
    // What was just saved becomes the new comparison baseline: the bar goes
    // clean because the fields now match the server.
    captureConfigBaseline();
  } catch (err) {
    // A failed save keeps every edit and the unsaved count, and says why.
    setConfigSaveError("保存失败：" + err.message);
    renderConfigDirtyState();
    showToast("保存失败: " + err.message, "error");
  } finally {
    configSaving = false;
    if (saveBtn) {
      saveBtn.disabled = false;
      saveBtn.textContent = "保存配置";
    }
  }
}

// Load API Keys
async function loadApiKeys() {
  try {
    apiKeys = await ConsoleAPI.json('/api/keys', {}, { array: true });
    renderApiKeys();
    // The key rows carry switches: paint them from their checkbox state.
    syncAllToggleStates();
  } catch (err) { showToast("加载 API Keys 失败", "error"); }
}

// Both table rows and mobile cards use the same delegated key actions.
function bindApiKeyActions(container) {
  container.onclick = (event) => {
    const actionEl = event.target.closest("[data-action]");
    if (!actionEl || !container.contains(actionEl)) return;
    const id = decodeData(actionEl.dataset.id || "");
    if (!id) return;
    switch (actionEl.dataset.action) {
      case "edit-key":
        openEditKeyModal(id);
        break;
      case "rotate-key":
        rotateApiKey(id);
        break;
      case "delete-key":
        openDeleteKeyModal(id, actionEl.dataset.label ? decodeURIComponent(actionEl.dataset.label) : "");
        break;
    }
  };

  container.onchange = (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement) || target.dataset.action !== "toggle-key") return;
    const id = decodeData(target.dataset.id || "");
    if (id) toggleKeyStatus(id, target.checked);
  };
}

// Render API Keys table
function renderApiKeys() {
  const container = document.getElementById("keysList");
  bindApiKeyActions(container);
  if (apiKeys.length === 0) {
    const empty = make("div", "empty-state empty-state-panel");
    attach(empty, [
      make("span", "empty-state-mark", "KY"),
      make("p", "", "暂无 API Key，点击上方按钮创建"),
    ]);
    container.replaceChildren(empty);
    return;
  }


  const headRow = attach(make("tr"), ["Token", "状态", "访问策略", "最后使用", "操作"].map((label) => make("th", "", label)));
  const table = attach(document.createElement("table"), [attach(document.createElement("thead"), [headRow])]);

  const tbody = document.createElement("tbody");
  apiKeys.forEach((k) => {
    const encodedLabel = encodeURIComponent(`${k.key_prefix}...${k.key_suffix}`);
    // The server stores only the hash, so the list can never show the secret
    // again. It used to render an eye toggle and a click-to-copy over this
    // masked string, which handed out "sk-****1234" as if it were the key; the
    // masked form is now inert and points at the action that issues a new one.
    const display = make("span", "key-display", `${k.key_prefix || ""}****${k.key_suffix || ""}`);
    display.title = "完整 Key 仅在创建或重置时显示一次，之后无法再次查看；需要新 Key 请点「重置」";
    const tokenCell = attach(make("td"), [
      attach(styled(make("div"), { display: "flex", alignItems: "center", gap: "8px" }), [
        display,
        make("span", "secret-badge", "密钥"),
      ]),
    ]);

    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = !!k.enabled;
    checkbox.dataset.action = "toggle-key";
    checkbox.dataset.id = encodeData(k.id);
    const toggle = styled(make("label", "toggle"), { transform: "scale(0.8)" });
    // keyButton is one of the three row actions; all three carry the row id the
    // delegated handler reads back with decodeData.
    const keyButton = (className, action, label, title) => {
      const button = styled(make("button", className, label), { padding: "4px 8px" });
      button.type = "button";
      button.dataset.action = action;
      button.dataset.id = encodeData(k.id);
      if (action === "rotate-key") {
        button.dataset.name = encodedLabel;
        button.title = "生成新的完整 Key（旧 Key 立即失效），仅显示一次";
      }
      if (action === "delete-key") button.dataset.label = encodedLabel;
      if (title) button.title = title;
      return button;
    };
    const rows = attach(make("tr"), [
      tokenCell,
      attach(make("td"), [attach(toggle, [checkbox, make("span", "toggle-slider")])]),
      styled(make("td", "", formatKeyPolicy(k)), { color: "var(--text-secondary)", fontSize: "0.8rem", whiteSpace: "pre-line" }),
      styled(make("td", "", k.last_used_at ? formatTime(k.last_used_at) : "从未使用"), { color: "var(--text-secondary)", fontSize: "0.8rem" }),
      attach(make("td"), [attach(make("div", "key-actions"), [
        styled(keyButton("btn btn-neutral", "edit-key", "策略"), { marginRight: "6px" }),
        styled(keyButton("btn btn-neutral", "rotate-key", "重置"), { marginRight: "6px" }),
        keyButton("btn btn-danger-outline", "delete-key", "删除"),
      ])]),
    ]);
    tbody.appendChild(rows);
  });
  table.appendChild(tbody);
  container.replaceChildren(ConsoleUI.responsiveTable(table), keyTip());
}

// keyTip is the standing note under both the table and the mobile card list: a
// key is a bearer secret, and the list never shows one.
function keyTip() {
  const tipText = styled(make("div"), { fontSize: "0.9rem", lineHeight: "1.6" });
  [
    "• API Key 用于访问接口的身份认证",
    "• 禁用的 Key 将无法访问 API",
    "• 请妥善保管您的 API Key，不要泄露给他人",
  ].forEach((line, idx) => {
    if (idx > 0) tipText.appendChild(document.createElement("br"));
    tipText.appendChild(document.createTextNode(line));
  });
  const body = styled(make("div"), { flex: "1" });
  attach(body, [
    styled(make("div", "", "提示"), { fontWeight: "600", marginBottom: "4px" }),
    tipText,
  ]);
  return attach(make("div", "config-key-tip"), [
    attach(styled(make("div"), { display: "flex", gap: "8px", alignItems: "start" }), [
      styled(make("span", "", "💡"), { fontSize: "1.2rem" }),
      body,
    ]),
  ]);
}



function parseAllowedModels(value) { return String(value || "").split(/[\n,]/).map((item) => item.trim()).filter(Boolean); }

function formatKeyPolicy(key) {
  const models = Array.isArray(key.allowed_models) && key.allowed_models.length
    ? key.allowed_models.join(", ")
    : "全部模型";
  const rpm = Number(key.rpm_limit) > 0 ? `${key.rpm_limit} RPM` : "不限速";
  const expiry = key.expires_at ? `到期 ${formatTime(key.expires_at)}` : "永不过期";
  const limit = Number(key.billing_limit_usd_ticks) > 0
    ? `预算 ${formatUSD(ticksToUSD(key.billing_limit_usd_ticks))}`
    : "预算不限";
  const used = Number(key.billing_used_usd_ticks) > 0
    ? ` · 已用 ${formatUSD(ticksToUSD(key.billing_used_usd_ticks))}`
    : "";
  const period = Number(key.billing_period_days) > 0 ? ` · ${key.billing_period_days} 天账期` : "";
  return `${models}\n${rpm} · ${expiry}\n${limit}${used}${period}`;
}

// The ledger counts USD ticks (1 USD = 10_000_000_000 ticks) because it is
// integer arithmetic; the admin plane shows dollars.
const USD_TICKS = 10000000000;
function ticksToUSD(ticks) { return Math.floor(Number(ticks) || 0) / USD_TICKS; }
function usdToTicks(value) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) return 0;
  return Math.round(parsed * USD_TICKS);
}
function formatUSD(amount) { return `$${Number(amount || 0).toFixed(2)}`; }

function periodSuffix(key) {
  const days = Number(key.billing_period_days) || 0;
  if (days <= 0) return "";
  const started = key.billing_period_started_at ? `（本期始于 ${formatTime(key.billing_period_started_at)}）` : "";
  return ` · ${days} 天账期${started}`;
}

function toDatetimeLocal(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
  return local.toISOString().slice(0, 16);
}

function localExpiryValue(id) {
  const value = document.getElementById(id).value;
  return value ? new Date(value).toISOString() : null;
}

// Rotate a key: issue a fresh secret and show it in the one-time window. The
// server stores only the hash, so a key that was not copied when it was created
// cannot be revealed afterwards -- rotating is how an existing entry gets back
// to a usable secret, and it retires the old one immediately.
async function rotateApiKey(id) {
  const key = apiKeys.find((item) => String(item.id) === String(id));
  const label = key ? key.name : `#${id}`;
  const confirmed = window.confirm(
    `为「${label}」生成新的完整 Key？\n\n旧的 Key 会立即失效，使用它的客户端需要更新。新的 Key 只会显示这一次。`
  );
  if (!confirmed) return;
  try {
    const res = await ConsoleAPI.request(`/api/keys/${id}/rotate`, { method: "POST" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.message || data.error || "重置失败");
    createdKeys = [{ name: data.name || label, key: data.key }];
    renderCreatedKeys();
    openShowKeyModal();
    loadApiKeys();
  } catch (err) { showToast(err.message || "重置失败", "error"); }
}

// Toggle key status
async function toggleKeyStatus(id, enabled) {
  try {
    await ConsoleAPI.request(`/api/keys/${id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ enabled }) });
    showToast(enabled ? "已启用" : "已禁用");
  } catch (err) { showToast("操作失败", "error"); }
}

// Open create key modal
function openCreateKeyModal() {
  // Every field starts blank: the window can hold a name left over from the last
  // key, and a stale expiry would be saved with the new one.
  setValue("keyName", "");
  setValue("keyAllowedModels", "");
  setValue("keyRPMLimit", "0");
  setValue("keyExpiresAt", "");
  openModal("createKeyModal");
}

// Close create key modal
function closeCreateKeyModal() { closeModal("createKeyModal"); }

// Create API key
async function createApiKey(e) {
  e.preventDefault();
  const names = document.getElementById("keyName").value.split("\n").filter(n => n.trim());
  if (names.length === 0) return;
  const allowedModels = parseAllowedModels(document.getElementById("keyAllowedModels").value);
  const rpmLimit = Number(document.getElementById("keyRPMLimit").value || 0);
  const expiresAt = localExpiryValue("keyExpiresAt");

  createdKeys = [];
  for (const name of names) {
    try {
      const res = await ConsoleAPI.request("/api/keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
        name,
        allowed_models: allowedModels,
        rpm_limit: rpmLimit,
        expires_at: expiresAt,
        billing_limit_usd_ticks: usdToTicks(document.getElementById("keyBillingLimit").value),
        billing_period_days: Number(document.getElementById("keyBillingPeriod").value || 0),
      }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || data.error || "创建失败");
      createdKeys.push({ name, key: data.key });
    } catch (err) {
      createdKeys.push({ name, error: err.message });
    }
  }
  closeCreateKeyModal();
  renderCreatedKeys();
  openShowKeyModal();
  loadApiKeys();
}

function openEditKeyModal(id) {
  const key = apiKeys.find((item) => String(item.id) === String(id));
  if (!key) return;
  setValue("editKeyId", id);
  setValue("editKeyAllowedModels", (key.allowed_models || []).join("\n"));
  setValue("editKeyRPMLimit", String(key.rpm_limit || 0));
  setValue("editKeyExpiresAt", toDatetimeLocal(key.expires_at));
  setValue("editKeyBillingLimit", String(ticksToUSD(key.billing_limit_usd_ticks)));
  setValue("editKeyBillingPeriod", String(key.billing_period_days || 0));
  const used = ticksToUSD(key.billing_used_usd_ticks);
  const limit = ticksToUSD(key.billing_limit_usd_ticks);
  setText("editKeyBillingUsage", Number(key.billing_limit_usd_ticks) > 0
    ? `已用 ${formatUSD(used)} / ${formatUSD(limit)}${periodSuffix(key)}`
    : `已用 ${formatUSD(used)}${periodSuffix(key)}`);
  openModal("editKeyModal");
}

function closeEditKeyModal() { closeModal("editKeyModal"); }

async function saveKeyPolicy(e) {
  e.preventDefault();
  const id = document.getElementById("editKeyId").value;
  const payload = {
    allowed_models: parseAllowedModels(document.getElementById("editKeyAllowedModels").value),
    rpm_limit: Number(document.getElementById("editKeyRPMLimit").value || 0),
    expires_at: localExpiryValue("editKeyExpiresAt"),
    billing_limit_usd_ticks: usdToTicks(document.getElementById("editKeyBillingLimit").value),
    billing_period_days: Number(document.getElementById("editKeyBillingPeriod").value || 0),
  };
  try {
    const res = await ConsoleAPI.request(`/api/keys/${id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
    if (!res.ok) throw new Error(await res.text() || "保存失败");
    closeEditKeyModal();
    showToast("策略已保存");
    await loadApiKeys();
  } catch (err) { showToast("保存失败: " + err.message, "error"); }
}

// Render created keys
function renderCreatedKeys() {
  const container = document.getElementById("fullKeyDisplay");
  container.replaceChildren(...createdKeys.map((k) => attach(
    styled(make("div", "key-display"), {
      marginBottom: "8px", padding: "12px", background: "var(--surface-2)", border: "1px dashed var(--border-color)", borderRadius: "8px" }),
    [
      styled(make("div", "", k.name || ""), { fontSize: "0.8rem", color: "var(--text-secondary)" }),
      styled(make("div", "", k.key || k.error || ""), { fontWeight: "bold", marginTop: "4px", wordBreak: "break-all", color: "var(--accent-green)" }),
    ],
  )));
}

// Copy all keys
function copyAllKeys() {
  const text = createdKeys.map(k => `${k.name}: ${k.key || k.error}`).join("\n");
  copyToClipboard(text);
}

// Open/close show key modal. This window is the only place a complete secret
// exists in the UI -- the list only ever holds a prefix and suffix.
function openShowKeyModal() { openModal("showKeyModal"); }

function closeShowKeyModal() { closeModal("showKeyModal"); }

// Open delete key modal
function openDeleteKeyModal(id, name) {
  setValue("deleteKeyId", id);
  setText("deleteKeyName", name);
  openModal("deleteKeyModal");
}

// Close delete key modal
function closeDeleteKeyModal() { closeModal("deleteKeyModal"); }

// Confirm delete key
async function confirmDeleteKey() {
  const id = document.getElementById("deleteKeyId").value;
  try {
    await ConsoleAPI.request(`/api/keys/${id}`, { method: "DELETE" });
    closeDeleteKeyModal();
    showToast("删除成功");
    loadApiKeys();
  } catch (err) { showToast("删除失败", "error"); }
}

// Load configuration on page load
document.addEventListener('DOMContentLoaded', () => {
  bindConfigDirtyTracking();
  bindConfigNav();
  loadConfiguration().then((loaded) => {
    loadApiKeys();
    // The values the server just returned are the baseline the save bar diffs
    // against; a failed load leaves the bar in its "not loaded" state instead.
    if (loaded) captureConfigBaseline();
  });
});
