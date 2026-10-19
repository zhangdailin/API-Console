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

// ── Unsaved-change tracking ───────────────────────────────────────────────────
// The sticky save bar compares the live control values against the values the
// server returned on load. Only the fields the save payload actually reads are
// counted.
const CONFIG_TRACKED_FIELDS = [
  "cfg_admin_pass",
  "cfg_proxy_url",
  "cfg_proxy_bypass",
  "cfg_qoder_checkin_device",
  "cfg_inference_profile",
  "cfg_grok_low_latency",
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
  setConfigControlValue("cfg_proxy_url", cfg.proxy_url || "");
  setConfigControlValue("cfg_proxy_bypass", normalizeProxyBypass(cfg.proxy_bypass).join("\n"));
  setConfigControlValue("cfg_inference_profile", cfg.inference_profile || "standard");
  setConfigControlValue("cfg_qoder_checkin_device", cfg.qoder_checkin_device || "");
  const lowLatency = document.getElementById("cfg_grok_low_latency");
  if (lowLatency) lowLatency.checked = cfg.grok_low_latency === true;
  syncAllToggleStates();
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
    proxy_url: document.getElementById("cfg_proxy_url").value.trim(),
    proxy_bypass: parseProxyBypass(proxyBypassRaw),
    inference_profile: document.getElementById("cfg_inference_profile")?.value || "standard",
    grok_low_latency: !!document.getElementById("cfg_grok_low_latency")?.checked,
  };
  const checkinDevice = document.getElementById("cfg_qoder_checkin_device");
  if (checkinDevice) data.qoder_checkin_device = checkinDevice.value.trim();
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
 loadConfiguration().then(loaded => { if (loaded) captureConfigBaseline(); });
 if (new URLSearchParams(window.location.search).get('section') === 'auth') switchConfigTab('auth');
});
