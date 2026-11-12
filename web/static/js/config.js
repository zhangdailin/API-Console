const { make, attach, setText } = ConsoleUI;
// Configuration management JavaScript

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

// ── Settings fields ─────────────────────────────────────────────────────────
// One table drives hydration, change tracking, reset and the save payload. A
// control the page does not render is skipped, so the table can name every
// setting the page may show.
//   text      trimmed string            lines  newline/comma list
//   bool      checkbox (def: default)   int    whole number, empty = 0
//   float     decimal, empty = 0        secs   number + unit select (<id>_unit)
//   radio     radio group named by ids  (value of the checked option)
const CONFIG_FIELDS = [
  { id: "cfg_proxy_url", key: "proxy_url", type: "text" },
  { id: "cfg_proxy_bypass", key: "proxy_bypass", type: "lines" },
  { id: "cfg_grok_cf_clearance", key: "grok_cf_clearance", type: "text" },
  { id: "cfg_grok_web_proxy_url", key: "grok_web_proxy_url", type: "text" },
  { id: "cfg_grok_cli_base_url", key: "grok_cli_base_url", type: "text", optional: true },
  { id: "cfg_grok_cli_client_version", key: "grok_cli_client_version", type: "text", optional: true },
  { id: "cfg_grok_cli_client_identifier", key: "grok_cli_client_identifier", type: "text", optional: true },
  { id: "cfg_grok_cli_user_agent", key: "grok_cli_user_agent", type: "text", optional: true },
  { id: "cfg_grok_build_rps", key: "grok_build_rps", type: "float", optional: true },
  { id: "cfg_grok_web_base_url", key: "grok_web_base_url", type: "text", optional: true },
  { id: "cfg_grok_web_user_agent", key: "grok_web_user_agent", type: "text", optional: true },
  { id: "cfg_grok_statsig_id", key: "grok_statsig_id", type: "text", optional: true },
  { id: "cfg_grok_statsig_signer_url", key: "grok_statsig_signer_url", type: "text", optional: true },
  { id: "cfg_statsig_mode", key: "", type: "radio", options: ["manual", "url"], radio: "cfg_statsig_mode_", fallback: "url", optional: true },
  { id: "cfg_grok_web_timeout_seconds", key: "grok_web_timeout_seconds", type: "secs", optional: true },
  { id: "cfg_grok_web_stream_idle_seconds", key: "grok_web_stream_idle_seconds", type: "secs", optional: true },
  { id: "cfg_grok_web_rps", key: "grok_web_rps", type: "float", optional: true },
  { id: "cfg_grok_temporary", key: "grok_temporary", type: "bool", default: true, optional: true },
  { id: "cfg_grok_disable_memory", key: "grok_disable_memory", type: "bool", optional: true },
  { id: "cfg_grok_custom_instruction", key: "grok_custom_instruction", type: "text", optional: true },
  { id: "cfg_grok_image_nsfw", key: "grok_image_nsfw", type: "bool", default: true, optional: true },
  { id: "cfg_doubao_base_url", key: "doubao_base_url", type: "text", optional: true },
  { id: "cfg_doubao_accounts_url", key: "doubao_accounts_url", type: "text", optional: true },
  { id: "cfg_doubao_user_agent", key: "doubao_user_agent", type: "text", optional: true },
  { id: "cfg_doubao_request_gap_ms", key: "doubao_request_gap_ms", type: "int", optional: true },
];
// The admin password is write-only: tracked for the save bar, sent when set.
const CONFIG_TRACKED_FIELDS = ["cfg_admin_pass", ...CONFIG_FIELDS.map((field) => field.id)];
let configBaseline = null;
let configSaving = false;

function configField(id) { return CONFIG_FIELDS.find((field) => field.id === id); }

// radioValue reads a radio group; a group the page does not render reads null.
function radioValue(field) {
  let rendered = false;
  for (const option of field.options) {
    const node = document.getElementById(field.radio + option);
    if (!node) continue;
    rendered = true;
    if (node.checked) return option;
  }
  return rendered ? field.fallback : null;
}

function setRadioValue(field, value) {
  const chosen = field.options.includes(value) ? value : field.fallback;
  field.options.forEach((option) => {
    const node = document.getElementById(field.radio + option);
    if (node) node.checked = option === chosen;
  });
}

function unitFactor(id) {
  const unit = document.getElementById(id + "_unit");
  const factor = unit ? Number(unit.value) : 1;
  return Number.isFinite(factor) && factor > 0 ? factor : 1;
}

// setSeconds shows a duration in the largest unit that divides it evenly.
function setSeconds(id, seconds) {
  const input = document.getElementById(id);
  if (!input) return;
  const unit = document.getElementById(id + "_unit");
  let value = Number(seconds) || 0;
  if (unit && value > 0) {
    const factors = Array.from(unit.options || []).map((option) => Number(option.value)).sort((a, b) => b - a);
    const factor = factors.find((f) => value % f === 0) || 1;
    unit.value = String(factor);
    value = value / factor;
  }
  input.value = value ? String(value) : "";
}

function readConfigFieldValue(id) {
  const spec = configField(id);
  if (spec && spec.type === "radio") return radioValue(spec);
  const field = document.getElementById(id);
  if (!field) return null;
  if (field.type === "checkbox" || (spec && spec.type === "bool")) return field.checked ? "true" : "false";
  if (spec && spec.type === "secs") return field.value === "" ? "" : String(Number(field.value) * unitFactor(id));
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

function captureConfigBaseline(state = collectConfigState(CONFIG_TRACKED_FIELDS)) {
  configBaseline = state;
  syncAllToggleStates();
  setConfigSaveError("");
  renderConfigDirtyState();
}

function handleConfigFieldChange() {
  syncAllToggleStates();
  renderConfigDirtyState();
}

function bindConfigDirtyTracking() {
  const ids = [];
  CONFIG_TRACKED_FIELDS.forEach((id) => {
    const spec = configField(id);
    if (spec && spec.type === "radio") spec.options.forEach((option) => ids.push(spec.radio + option));
    else ids.push(id, id + "_unit");
  });
  ids.forEach((id) => {
    const field = document.getElementById(id);
    if (!field || !field.addEventListener) return;
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
    if (!(id in configBaseline)) return;
    const spec = configField(id);
    if (spec && spec.type === "radio") { setRadioValue(spec, configBaseline[id]); return; }
    const field = document.getElementById(id);
    if (!field) return;
    if (field.type === "checkbox") {
      field.checked = configBaseline[id] === "true";
    } else if (spec && spec.type === "secs") {
      setSeconds(id, configBaseline[id]);
    } else { field.value = configBaseline[id]; }
  });
  setConfigSaveError("");
  handleConfigFieldChange();
  showToast("已重置为服务器上的配置");
}

// ── Vertical settings tabs ──────────────────────────────────────────────────
function showConfigPanel(name) {
  const nav = document.getElementById("configNav");
  if (!nav || !nav.querySelectorAll) return;
  nav.querySelectorAll("[data-cfg-tab]").forEach((tab) => {
    const active = tab.getAttribute("data-cfg-tab") === name;
    tab.classList.toggle("active", active);
    tab.setAttribute("aria-selected", String(active));
    tab.tabIndex = active ? 0 : -1;
    const panel = document.getElementById(tab.getAttribute("aria-controls"));
    if (panel) panel.hidden = !active;
  });
  try { localStorage.setItem("configPanel", name); } catch (_) { /* per-viewer convenience only */ }
}

function bindConfigNav() {
  const nav = document.getElementById("configNav");
  if (!nav || !nav.querySelectorAll) return;
  const tabs = Array.from(nav.querySelectorAll("[data-cfg-tab]"));
  tabs.forEach((tab, index) => {
    tab.addEventListener("click", () => showConfigPanel(tab.getAttribute("data-cfg-tab")));
    tab.addEventListener("keydown", (event) => {
      const step = { ArrowDown: 1, ArrowRight: 1, ArrowUp: -1, ArrowLeft: -1 }[event.key];
      if (!step) return;
      event.preventDefault();
      const next = tabs[(index + step + tabs.length) % tabs.length];
      showConfigPanel(next.getAttribute("data-cfg-tab"));
      next.focus();
    });
  });
  let saved = "";
  try { saved = localStorage.getItem("configPanel") || ""; } catch (_) { saved = ""; }
  const requested = new URLSearchParams(window.location.search).get("panel") || saved;
  showConfigPanel(tabs.some((tab) => tab.getAttribute("data-cfg-tab") === requested) ? requested : "build");
}

// Rows that only apply to one radio choice follow it.
function syncConditionalRows() {
  if (!document.querySelectorAll) return;
  const statsig = radioValue(configField("cfg_statsig_mode"));
  document.querySelectorAll("[data-statsig]").forEach((row) => { row.hidden = row.getAttribute("data-statsig") !== statsig; });
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
  CONFIG_FIELDS.forEach((field) => {
    if (!field.key) return;
    const value = cfg[field.key];
    switch (field.type) {
      case "bool": {
        const node = document.getElementById(field.id);
        if (node) node.checked = value == null ? field.default === true : value === true;
        break;
      }
      case "lines": setConfigControlValue(field.id, normalizeProxyBypass(value).join("\n")); break;
      case "radio": setRadioValue(field, value); break;
      case "secs": setSeconds(field.id, value); break;
      case "int":
      case "float": setConfigControlValue(field.id, value ? value : ""); break;
      default: setConfigControlValue(field.id, value == null ? "" : value);
    }
  });
  // Statsig: a fixed ID means manual; otherwise the signer (default or custom).
  setRadioValue(configField("cfg_statsig_mode"), String(cfg.grok_statsig_id || "").trim() ? "manual" : "url");
  syncAllToggleStates();
  syncConditionalRows();
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
  const data = {};
  CONFIG_FIELDS.forEach((field) => {
    if (!field.key) return;
    const raw = readConfigFieldValue(field.id);
    if (raw === null) return;
    switch (field.type) {
      case "bool": data[field.key] = raw === "true"; break;
      case "lines": data[field.key] = parseProxyBypass(raw); break;
      case "int":
      case "secs": data[field.key] = Math.max(0, Math.round(Number(raw) || 0)); break;
      case "float": data[field.key] = Math.max(0, Number(raw) || 0); break;
      default: data[field.key] = String(raw).trim();
    }
  });
  const statsig = readConfigFieldValue("cfg_statsig_mode");
  if (statsig === "manual") {
    // A fixed ID replaces the signer entirely.
    data.grok_statsig_signer_url = "";
  } else if (statsig === "url") {
    data.grok_statsig_id = "";
    // Empty means the built-in signer (null), not "signer disabled" ("").
    if (!data.grok_statsig_signer_url) data.grok_statsig_signer_url = null;
  }
	const password = document.getElementById("cfg_admin_pass").value;
	if (password.trim()) data.admin_password = password;

  const saveBtn = document.getElementById("cfgSaveBtn");
  const submittedState = collectConfigState(CONFIG_TRACKED_FIELDS);
  configSaving = true;
  if (saveBtn) {
    saveBtn.disabled = true;
    saveBtn.textContent = "保存中…";
  }

  try {
    await ConsoleAPI.json('/api/config/save', ConsoleAPI.jsonOptions('POST', data), { envelope: true });
    setConfigSaveError("");
    showToast("配置保存成功");
    // Edits made while the request was pending have not reached the server.
    captureConfigBaseline(submittedState);
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


// Load only the active section; key mutations never enter the config save baseline.
document.addEventListener('DOMContentLoaded', () => {
 bindConfigDirtyTracking(); bindConfigNav();
 document.addEventListener("change", (event) => { if (event.target && event.target.type === "radio") syncConditionalRows(); });
 loadConfiguration().then(loaded => { if (loaded) captureConfigBaseline(); });
 if (new URLSearchParams(window.location.search).get('section') === 'auth') switchConfigTab('auth');
});
