// Configuration management JavaScript

let apiKeys = [];
let createdKeys = [];
const TOKEN_CACHE_TTL_PRESETS = ["60", "300", "900", "1800", "3600", "86400", "259200", "604800"];

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
  if (field) {
    field.type = field.type === 'password' ? 'text' : 'password';
  }
}

// ── Unsaved-change tracking ───────────────────────────────────────────────────
// The sticky save bar compares the live control values against the values the
// server returned on load. Only the fields the save payload actually reads are
// counted; the TTL preset select and its custom box mirror the effective TTL and
// are rebuilt from it instead of being tracked separately.
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

const CONFIG_TRACKED_FIELDS = [
  "cfg_admin_pass",
  "cfg_anonymous_allow_ips",
  "cfg_proxy_url",
  "cfg_proxy_bypass",
  "cfg_token_cache_ttl",
  "cfg_token_cache_strategy",
  "cfg_enable_token_cache",
  "cfg_cache_token_count",
];
const CONFIG_MIRROR_FIELDS = ["cfg_token_cache_ttl_preset", "cfg_token_cache_ttl_custom"];
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

function syncAllToggleStates() {
  document.querySelectorAll(".toggle input[type=checkbox]").forEach(syncToggleElement);
}

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
  CONFIG_TRACKED_FIELDS.concat(CONFIG_MIRROR_FIELDS).forEach((id) => {
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
    } else {
      field.value = configBaseline[id];
    }
  });
  syncTokenCacheTTLControls(getTokenCacheTTLValue());
  const cacheEnabled = !!document.getElementById("cfg_enable_token_cache")?.checked;
  toggleCacheConfig(cacheEnabled);
  updateMemoryEstimation();
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
      if (section && section.getBoundingClientRect().top <= 160) {
        current = link.getAttribute("data-nav-group");
      }
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

function normalizeTokenCacheTTLValue(raw) {
  const value = parseInt(raw, 10);
  if (Number.isFinite(value) && value > 0) {
    return String(value);
  }
  return "300";
}

function normalizeFlagValue(value) {
  if (typeof value === "boolean") return value;
  if (typeof value === "string") {
    const normalized = value.trim().toLowerCase();
    if (normalized === "true" || normalized === "1" || normalized === "yes" || normalized === "on") return true;
    if (normalized === "false" || normalized === "0" || normalized === "no" || normalized === "off") return false;
  }
  return !!value;
}

function syncTokenCacheTTLControls(raw) {
  const normalized = normalizeTokenCacheTTLValue(raw);
  const hiddenInput = document.getElementById("cfg_token_cache_ttl");
  const presetInput = document.getElementById("cfg_token_cache_ttl_preset");
  const customInput = document.getElementById("cfg_token_cache_ttl_custom");
  const customWrap = document.getElementById("cfg_token_cache_ttl_custom_wrap");
  if (!hiddenInput || !presetInput || !customInput || !customWrap) return;

  hiddenInput.value = normalized;
  customInput.value = normalized;
  const isPreset = TOKEN_CACHE_TTL_PRESETS.includes(normalized);
  presetInput.value = isPreset ? normalized : "custom";
  customWrap.style.display = isPreset ? "none" : "block";
}

function getTokenCacheTTLValue() {
  const hiddenInput = document.getElementById("cfg_token_cache_ttl");
  return normalizeTokenCacheTTLValue(hiddenInput?.value);
}

function handleTokenCacheTTLPresetChange() {
  const hiddenInput = document.getElementById("cfg_token_cache_ttl");
  const presetInput = document.getElementById("cfg_token_cache_ttl_preset");
  const customInput = document.getElementById("cfg_token_cache_ttl_custom");
  const customWrap = document.getElementById("cfg_token_cache_ttl_custom_wrap");
  if (!hiddenInput || !presetInput || !customInput || !customWrap) return;

  if (presetInput.value === "custom") {
    customWrap.style.display = "block";
    hiddenInput.value = normalizeTokenCacheTTLValue(customInput.value);
  } else {
    customWrap.style.display = "none";
    hiddenInput.value = normalizeTokenCacheTTLValue(presetInput.value);
  }
  updateMemoryEstimation();
}

function handleTokenCacheTTLCustomInput() {
  const presetInput = document.getElementById("cfg_token_cache_ttl_preset");
  const hiddenInput = document.getElementById("cfg_token_cache_ttl");
  const customInput = document.getElementById("cfg_token_cache_ttl_custom");
  if (!presetInput || !hiddenInput || !customInput || presetInput.value !== "custom") return;

  hiddenInput.value = normalizeTokenCacheTTLValue(customInput.value);
  updateMemoryEstimation();
}

// Load configuration from API. Returns true only when the server values were
// applied: a failed load must not become the baseline the save bar diffs against.
function setConfigControlValue(id, value) {
  const field = document.getElementById(id);
  if (field) field.value = value == null ? "" : String(value);
}

function applyConfigurationPayload(cfg) {
  if (!cfg || typeof cfg !== "object" || Array.isArray(cfg)) {
    throw new Error("配置接口返回格式无效");
  }
  setConfigControlValue("cfg_admin_pass", cfg.admin_password || cfg.admin_pass || "");
  setConfigControlValue("cfg_anonymous_allow_ips", Array.isArray(cfg.anonymous_allow_ips) ? cfg.anonymous_allow_ips.join("\n") : "");
  setConfigControlValue("cfg_proxy_url", cfg.proxy_url || "");
  setConfigControlValue("cfg_proxy_bypass", normalizeProxyBypass(cfg.proxy_bypass).join("\n"));

  const cacheTokenCount = document.getElementById("cfg_enable_token_cache");
  if (cacheTokenCount) cacheTokenCount.checked = normalizeFlagValue(cfg.enable_token_cache);
  const estimateTokenCache = document.getElementById("cfg_cache_token_count");
  if (estimateTokenCache) estimateTokenCache.checked = normalizeFlagValue(cfg.cache_token_count);

  syncTokenCacheTTLControls(cfg.token_cache_ttl || 300);
  setConfigControlValue("cfg_token_cache_strategy", cfg.token_cache_strategy || "1");
}

async function loadConfiguration() {
  try {
    const res = await fetch("/api/config/list", { credentials: "same-origin", cache: "no-store" });
    if (res.status === 401 || res.status === 403) {
      window.location.href = "./login.html";
      return false;
    }
    const contentType = res.headers?.get?.("content-type") || "";
    if (!res.ok) {
      const detail = (await res.text()).trim();
      throw new Error(detail || `HTTP ${res.status}`);
    }
    if (!contentType.toLowerCase().includes("application/json")) {
      throw new Error("配置接口未返回 JSON，登录状态可能已失效");
    }
    const payload = await res.json();
    if (payload && typeof payload.code !== "undefined" && payload.code !== 0) {
      throw new Error(payload.message || payload.msg || "加载配置失败");
    }
    applyConfigurationPayload(payload && payload.data ? payload.data : payload);
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
    enable_token_cache: document.getElementById("cfg_enable_token_cache").checked ? "true" : "false",
    cache_token_count: document.getElementById("cfg_cache_token_count")?.checked ? "true" : "false",
    token_cache_ttl: getTokenCacheTTLValue(),
    token_cache_strategy: document.getElementById("cfg_token_cache_strategy").value,
  };

  const saveBtn = document.getElementById("cfgSaveBtn");
  configSaving = true;
  if (saveBtn) {
    saveBtn.disabled = true;
    saveBtn.textContent = "保存中…";
  }

  try {
    const res = await fetch("/api/config/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data)
    });
    if (!res.ok) throw new Error(await res.text());
    const payload = await res.json();
    if (payload.code !== 0) {
      throw new Error(payload.message || payload.msg || "保存失败");
    }
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
    const res = await fetch("/api/keys");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    apiKeys = (await res.json()) || [];
    renderApiKeys();
    // The key rows carry switches: paint them from their checkbox state.
    syncAllToggleStates();
  } catch (err) {
    showToast("加载 API Keys 失败", "error");
  }
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
    container.innerHTML = "";
    const empty = document.createElement("div");
    empty.className = "empty-state empty-state-panel";
    const mark = document.createElement("span");
    mark.className = "empty-state-mark";
    mark.textContent = "KY";
    const p = document.createElement("p");
    p.textContent = "暂无 API Key，点击上方按钮创建";
    empty.appendChild(mark);
    empty.appendChild(p);
    container.appendChild(empty);
    return;
  }

  if (window.matchMedia("(max-width: 640px)").matches) {
    renderApiKeysMobile(container);
    return;
  }

  container.innerHTML = "";
  const table = document.createElement("table");
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  ["Token", "状态", "访问策略", "最后使用", "操作"].forEach((label) => {
    const th = document.createElement("th");
    th.textContent = label;
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");
  apiKeys.forEach((k) => {
    const encodedLabel = encodeURIComponent(`${k.key_prefix}...${k.key_suffix}`);
    const tr = document.createElement("tr");

    const tdToken = document.createElement("td");
    const tokenWrap = document.createElement("div");
    tokenWrap.style.display = "flex";
    tokenWrap.style.alignItems = "center";
    tokenWrap.style.gap = "8px";
    // The server stores only the hash, so the list can never show the secret
    // again. It used to render an eye toggle and a click-to-copy over this
    // masked string, which handed out "sk-****1234" as if it were the key; the
    // masked form is now inert and points at the action that issues a new one.
    const display = document.createElement("span");
    display.className = "key-display";
    display.title = "完整 Key 仅在创建或重置时显示一次，之后无法再次查看；需要新 Key 请点「重置」";
    display.textContent = `${k.key_prefix || ""}****${k.key_suffix || ""}`;
    const badge = document.createElement("span");
    badge.className = "secret-badge";
    badge.textContent = "密钥";
    tokenWrap.appendChild(display);
    tokenWrap.appendChild(badge);
    tdToken.appendChild(tokenWrap);
    tr.appendChild(tdToken);

    const tdStatus = document.createElement("td");
    const label = document.createElement("label");
    label.className = "toggle";
    label.style.transform = "scale(0.8)";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = !!k.enabled;
    checkbox.dataset.action = "toggle-key";
    checkbox.dataset.id = encodeData(k.id);
    const slider = document.createElement("span");
    slider.className = "toggle-slider";
    label.appendChild(checkbox);
    label.appendChild(slider);
    tdStatus.appendChild(label);
    tr.appendChild(tdStatus);

    const tdPolicy = document.createElement("td");
    tdPolicy.style.color = "var(--text-secondary)";
    tdPolicy.style.fontSize = "0.8rem";
    tdPolicy.style.whiteSpace = "pre-line";
    tdPolicy.textContent = formatKeyPolicy(k);
    tr.appendChild(tdPolicy);

    const tdLast = document.createElement("td");
    tdLast.style.color = "var(--text-secondary)";
    tdLast.style.fontSize = "0.8rem";
    tdLast.textContent = k.last_used_at ? formatTime(k.last_used_at) : "从未使用";
    tr.appendChild(tdLast);

    const tdAction = document.createElement("td");
    const editBtn = document.createElement("button");
    editBtn.className = "btn btn-neutral";
    editBtn.style.padding = "4px 8px";
    editBtn.style.marginRight = "6px";
    editBtn.dataset.action = "edit-key";
    editBtn.dataset.id = encodeData(k.id);
    editBtn.textContent = "策略";
    const rotateBtn = document.createElement("button");
    rotateBtn.className = "btn btn-neutral";
    rotateBtn.style.padding = "4px 8px";
    rotateBtn.style.marginRight = "6px";
    rotateBtn.dataset.action = "rotate-key";
    rotateBtn.dataset.id = encodeData(k.id);
    rotateBtn.dataset.name = encodedLabel;
    rotateBtn.textContent = "重置";
    rotateBtn.title = "生成新的完整 Key（旧 Key 立即失效），仅显示一次";
    const delBtn = document.createElement("button");
    delBtn.className = "btn btn-danger-outline";
    delBtn.style.padding = "4px 8px";
    delBtn.dataset.action = "delete-key";
    delBtn.dataset.id = encodeData(k.id);
    delBtn.dataset.label = encodedLabel;
    delBtn.textContent = "删除";
    tdAction.appendChild(editBtn);
    tdAction.appendChild(rotateBtn);
    tdAction.appendChild(delBtn);
    tr.appendChild(tdAction);

    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  container.appendChild(table);

  const tip = document.createElement("div");
  tip.className = "config-key-tip";
  const tipRow = document.createElement("div");
  tipRow.style.display = "flex";
  tipRow.style.gap = "8px";
  tipRow.style.alignItems = "start";
  const tipIcon = document.createElement("span");
  tipIcon.style.fontSize = "1.2rem";
  tipIcon.textContent = "💡";
  const tipBody = document.createElement("div");
  tipBody.style.flex = "1";
  const tipTitle = document.createElement("div");
  tipTitle.style.fontWeight = "600";
  tipTitle.style.marginBottom = "4px";
  tipTitle.textContent = "提示";
  const tipText = document.createElement("div");
  tipText.style.fontSize = "0.9rem";
  tipText.style.lineHeight = "1.6";
  const tipLines = [
    "• API Key 用于访问接口的身份认证",
    "• 禁用的 Key 将无法访问 API",
    "• 请妥善保管您的 API Key，不要泄露给他人",
  ];
  tipLines.forEach((line, idx) => {
    if (idx > 0) tipText.appendChild(document.createElement("br"));
    tipText.appendChild(document.createTextNode(line));
  });
  tipBody.appendChild(tipTitle);
  tipBody.appendChild(tipText);
  tipRow.appendChild(tipIcon);
  tipRow.appendChild(tipBody);
  tip.appendChild(tipRow);
  container.appendChild(tip);

}

function renderApiKeysMobile(container) {
  const cards = apiKeys.map((k) => {
    const encodedLabel = encodeURIComponent(`${k.key_prefix}...${k.key_suffix}`);
    const lastUsed = k.last_used_at ? formatTime(k.last_used_at) : "从未使用";
    const policy = formatKeyPolicy(k);
    return `
      <article class="config-key-card">
        <div class="config-key-head">
          <div class="config-key-token">
            <span class="key-display" title="完整 Key 仅在创建或重置时显示一次，之后无法再次查看">${escapeHtml(`${k.key_prefix || ""}****${k.key_suffix || ""}`)}</span>
            <span class="secret-badge">密钥</span>
          </div>
          <label class="toggle">
            <input type="checkbox" data-action="toggle-key" data-id="${encodeData(k.id)}" ${k.enabled ? "checked" : ""}>
            <span class="toggle-slider"></span>
          </label>
        </div>
        <div class="config-key-meta">
          <div class="config-key-item">
            <span class="config-key-label">最后使用</span>
            <span>${escapeHtml(lastUsed)}</span>
          </div>
          <div class="config-key-item">
            <span class="config-key-label">访问策略</span>
            <span style="white-space: pre-line">${escapeHtml(policy)}</span>
          </div>
        </div>
        <div class="config-key-actions">
          <button type="button" class="btn btn-neutral" data-action="edit-key" data-id="${encodeData(k.id)}">策略</button>
          <button type="button" class="btn btn-neutral" data-action="rotate-key" data-id="${encodeData(k.id)}" title="生成新的完整 Key（旧 Key 立即失效），仅显示一次">重置</button>
          <button type="button" class="btn btn-danger-outline" data-action="delete-key" data-id="${encodeData(k.id)}" data-label="${encodedLabel}">删除</button>
        </div>
      </article>
    `;
  }).join("");

  container.innerHTML = `<div class="config-key-list">${cards}</div>`;

  const tip = document.createElement("div");
  tip.className = "config-key-tip";
  tip.innerHTML = `
    <div class="config-key-tip-title">提示</div>
    <div class="config-key-tip-body">• API Key 用于访问接口的身份认证<br>• 禁用的 Key 将无法访问 API<br>• 请妥善保管您的 API Key，不要泄露给他人</div>
  `;
  container.appendChild(tip);

}

function parseAllowedModels(value) {
  return String(value || "").split(/[\n,]/).map((item) => item.trim()).filter(Boolean);
}

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
function ticksToUSD(ticks) {
  return Math.floor(Number(ticks) || 0) / USD_TICKS;
}
function usdToTicks(value) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) return 0;
  return Math.round(parsed * USD_TICKS);
}
function formatUSD(amount) {
  return `$${Number(amount || 0).toFixed(2)}`;
}

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
    const res = await fetch(`/api/keys/${id}/rotate`, { method: "POST" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.message || data.error || "重置失败");
    createdKeys = [{ name: data.name || label, key: data.key }];
    renderCreatedKeys();
    openShowKeyModal();
    loadApiKeys();
  } catch (err) {
    showToast(err.message || "重置失败", "error");
  }
}

// Toggle key status
async function toggleKeyStatus(id, enabled) {
  try {
    await fetch(`/api/keys/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled }),
    });
    showToast(enabled ? "已启用" : "已禁用");
  } catch (err) {
    showToast("操作失败", "error");
  }
}

// Open create key modal
function openCreateKeyModal() {
  document.getElementById("keyName").value = "";
  document.getElementById("keyAllowedModels").value = "";
  document.getElementById("keyRPMLimit").value = "0";
  document.getElementById("keyExpiresAt").value = "";
  document.getElementById("createKeyModal").classList.add("active");
  document.getElementById("createKeyModal").style.display = "flex";
}

// Close create key modal
function closeCreateKeyModal() {
  document.getElementById("createKeyModal").classList.remove("active");
  document.getElementById("createKeyModal").style.display = "none";
}

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
      const res = await fetch("/api/keys", {
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
  document.getElementById("editKeyId").value = id;
  document.getElementById("editKeyAllowedModels").value = (key.allowed_models || []).join("\n");
  document.getElementById("editKeyRPMLimit").value = String(key.rpm_limit || 0);
  document.getElementById("editKeyExpiresAt").value = toDatetimeLocal(key.expires_at);
  document.getElementById("editKeyBillingLimit").value = String(ticksToUSD(key.billing_limit_usd_ticks));
  document.getElementById("editKeyBillingPeriod").value = String(key.billing_period_days || 0);
  const usageHint = document.getElementById("editKeyBillingUsage");
  if (usageHint) {
    const used = ticksToUSD(key.billing_used_usd_ticks);
    usageHint.textContent = Number(key.billing_limit_usd_ticks) > 0
      ? `已用 ${formatUSD(used)} / ${formatUSD(ticksToUSD(key.billing_limit_usd_ticks))}${periodSuffix(key)}`
      : `已用 ${formatUSD(used)}${periodSuffix(key)}`;
  }
  const modal = document.getElementById("editKeyModal");
  modal.classList.add("active");
  modal.style.display = "flex";
}

function closeEditKeyModal() {
  const modal = document.getElementById("editKeyModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

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
    const res = await fetch(`/api/keys/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) throw new Error(await res.text() || "保存失败");
    closeEditKeyModal();
    showToast("策略已保存");
    await loadApiKeys();
  } catch (err) {
    showToast("保存失败: " + err.message, "error");
  }
}

// Render created keys
function renderCreatedKeys() {
  const container = document.getElementById("fullKeyDisplay");
  container.innerHTML = "";
  createdKeys.forEach((k) => {
    const wrap = document.createElement("div");
    wrap.className = "key-display";
    wrap.style.marginBottom = "8px";
    wrap.style.padding = "12px";
    wrap.style.background = "var(--surface-2)";
    wrap.style.border = "1px dashed var(--border-color)";
    wrap.style.borderRadius = "8px";

    const name = document.createElement("div");
    name.style.fontSize = "0.8rem";
    name.style.color = "var(--text-secondary)";
    name.textContent = k.name || "";

    const key = document.createElement("div");
    key.style.fontWeight = "bold";
    key.style.marginTop = "4px";
    key.style.wordBreak = "break-all";
    key.style.color = "var(--accent-green)";
    key.textContent = k.key || k.error || "";

    wrap.appendChild(name);
    wrap.appendChild(key);
    container.appendChild(wrap);
  });
}

// Copy all keys
function copyAllKeys() {
  const text = createdKeys.map(k => `${k.name}: ${k.key || k.error}`).join("\n");
  copyToClipboard(text);
}

// Open/close show key modal. This window is the only place a complete secret
// exists in the UI -- the list only ever holds a prefix and suffix.
function openShowKeyModal() {
  document.getElementById("showKeyModal").classList.add("active");
  document.getElementById("showKeyModal").style.display = "flex";
}

function closeShowKeyModal() {
  document.getElementById("showKeyModal").classList.remove("active");
  document.getElementById("showKeyModal").style.display = "none";
}

// Open delete key modal
function openDeleteKeyModal(id, name) {
  document.getElementById("deleteKeyId").value = id;
  document.getElementById("deleteKeyName").textContent = name;
  const modal = document.getElementById("deleteKeyModal");
  modal.classList.add("active");
  modal.style.display = "flex";
}

// Close delete key modal
function closeDeleteKeyModal() {
  const modal = document.getElementById("deleteKeyModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

// Confirm delete key
async function confirmDeleteKey() {
  const id = document.getElementById("deleteKeyId").value;
  try {
    await fetch(`/api/keys/${id}`, { method: "DELETE" });
    closeDeleteKeyModal();
    showToast("删除成功");
    loadApiKeys();
  } catch (err) {
    showToast("删除失败", "error");
  }
}




function toggleCacheConfig(checked) {
  const details = document.getElementById("cacheConfigDetails");
  if (!details) return;
  details.style.display = checked ? "block" : "none";
  if (checked) {
    updateMemoryEstimation();
    loadCacheStats();
  }
}

function updateMemoryEstimation() {
  const strategyInput = document.getElementById("cfg_token_cache_strategy");
  if (!strategyInput) return;

  const ttlSec = parseInt(getTokenCacheTTLValue(), 10) || 300;
  const strategy = strategyInput.value;
  const mult = (strategy === "1" || strategy === "0") ? 2 : 1;

  const ttlEl = document.getElementById("estTTLSeconds");
  const multEl = document.getElementById("estStrategyMult");
  const titleEl = document.getElementById("memoryEstTitle");
  if (ttlEl) ttlEl.textContent = String(ttlSec);
  if (multEl) multEl.textContent = mult === 2 ? "× 2" : "× 1";
  if (titleEl) {
    titleEl.textContent = `内存估算 (当前: TTL=${ttlSec}秒, 系数=${mult})`;
  }

  const calc = (qps) => {
    const kb = qps * ttlSec * 0.5 * mult;
    if (kb > 1024) return (kb / 1024).toFixed(1) + "MB";
    return kb.toFixed(1) + "KB";
  };

  const lowEl = document.getElementById("estLow");
  const midEl = document.getElementById("estMid");
  const highEl = document.getElementById("estHigh");
  if (lowEl) lowEl.textContent = calc(10);
  if (midEl) midEl.textContent = calc(50);
  if (highEl) highEl.textContent = calc(100);
}

async function loadCacheStats() {
  const statsEl = document.getElementById("cacheStatsText");
  const estimateStatsEl = document.getElementById("estimateCacheStatsText");
  if (!statsEl && !estimateStatsEl) return;

  try {
    const res = await fetch("/api/token-cache/stats");
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    const prompt = data?.data?.prompt_cache || {};
    const estimate = data?.data?.estimate_cache || {};

    if (statsEl) {
      if (data.code !== 0 || !prompt.connected) {
        statsEl.textContent = "Prompt 缓存未启用";
      } else {
        statsEl.textContent = `Prompt 缓存: ${Number(prompt.key_count) || 0} 条，占用内存: ${prompt.memory_used_str || "0 B"}`;
      }
    }

    if (estimateStatsEl) {
      if (data.code !== 0 || !estimate.connected) {
        estimateStatsEl.textContent = "Token 估算缓存未启用";
      } else {
        estimateStatsEl.textContent = `Token 估算缓存: ${Number(estimate.key_count) || 0} 条，占用内存: ${estimate.memory_used_str || "0 B"}`;
      }
    }
  } catch (err) {
    if (statsEl) statsEl.textContent = "缓存统计加载失败";
    if (estimateStatsEl) estimateStatsEl.textContent = "缓存统计加载失败";
  }
}

async function clearCache() {
  if (!confirm("确定要清空 Token 用量缓存吗？")) return;
  try {
    const res = await fetch("/api/token-cache/clear", { method: "POST" });
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    if (data.code !== 0) {
      throw new Error(data.message || data.msg || "清空失败");
    }
    const deleted = Number(data?.data?.deleted) || 0;
    showToast(`已清空 ${deleted} 条缓存`);
    loadCacheStats();
  } catch (err) {
    showToast("清空失败: " + err.message, "error");
  }
}

// Load configuration on page load
document.addEventListener('DOMContentLoaded', () => {
  bindConfigDirtyTracking();
  bindConfigNav();
  loadConfiguration().then((loaded) => {
    const cacheEnabled = !!document.getElementById("cfg_enable_token_cache")?.checked;
    toggleCacheConfig(cacheEnabled);
    updateMemoryEstimation();
    loadCacheStats();
    loadApiKeys();
    // The values the server just returned are the baseline the save bar diffs
    // against; a failed load leaves the bar in its "not loaded" state instead.
    if (loaded) captureConfigBaseline();
  });
});
