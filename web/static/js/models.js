// Models management JavaScript

let models = [];
let currentModelChannel = "";
let modelSearchTerm = "";
let modelStatusFilter = "";
let modelPageSize = 20;
let modelCurrentPage = 1;
let modelRefreshInFlight = false;
let modelBatchInFlight = false;
let modelDeleteOfflineInFlight = false;
let modelRefreshResults = {};
let modelRefreshConcurrency = 4;

function modelChannels() {
  const defaultChannels = Array.isArray(window.OrchidsProviderRegistry?.channels)
    ? [...window.OrchidsProviderRegistry.channels]
    : [];
  const seen = new Set();
  const ordered = [];

  defaultChannels.forEach((channel) => {
    if (seen.has(channel.toLowerCase())) return;
    seen.add(channel.toLowerCase());
    ordered.push(channel);
  });

  models
    .map((m) => String(m.channel || "").trim())
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b))
    .forEach((channel) => {
      const key = channel.toLowerCase();
      if (seen.has(key)) return;
      seen.add(key);
      ordered.push(channel);
    });

  return ordered;
}

function normalizeModelStatus(status) {
  if (status === true) return "available";
  const value = String(status || "").trim().toLowerCase();
  return value || "offline";
}

function statusMeta(status) {
  switch (normalizeModelStatus(status)) {
    case "available":
      return { label: "可用", bg: "rgba(34, 197, 94, 0.12)", color: "#4ade80", border: "rgba(34, 197, 94, 0.22)" };
    case "maintenance":
      return { label: "维护中", bg: "rgba(245, 158, 11, 0.12)", color: "#fbbf24", border: "rgba(245, 158, 11, 0.24)" };
    default:
      return { label: "已下线", bg: "rgba(148, 163, 184, 0.12)", color: "#cbd5e1", border: "rgba(148, 163, 184, 0.24)" };
  }
}

function sortModels(list) {
  return list.sort((a, b) => {
    const sortDiff = Number(a.sort_order || 0) - Number(b.sort_order || 0);
    if (sortDiff !== 0) return sortDiff;
    return String(a.model_id || "").localeCompare(String(b.model_id || ""));
  });
}

function getChannelScopedModels() {
  let scoped = models.slice();

  if (currentModelChannel) {
    scoped = scoped.filter((m) => String(m.channel || "").toLowerCase() === currentModelChannel.toLowerCase());
  }

  return sortModels(scoped);
}

function getFilteredModels(channelModels = getChannelScopedModels()) {
  let filtered = channelModels;

  if (modelStatusFilter) {
    filtered = filtered.filter((m) => normalizeModelStatus(m.status) === modelStatusFilter);
  }

  if (modelSearchTerm) {
    const term = modelSearchTerm.toLowerCase();
    filtered = filtered.filter((m) => {
      return [
        m.model_id,
        m.name,
        m.channel,
        m.id,
      ].some((value) => String(value || "").toLowerCase().includes(term));
    });
  }

  return filtered;
}

function updateModelSummary(channelModels, filtered) {
  const channelLabel = currentModelChannel || "全部";

  const totalModelCount = document.getElementById("totalModelCount");
  if (totalModelCount) {
    totalModelCount.textContent = String(filtered.length);
  }

  const currentChannelPill = document.getElementById("currentChannelPill");
  if (currentChannelPill) {
    currentChannelPill.textContent = channelLabel;
  }

  const filterMeta = document.getElementById("modelsFilterMeta");
  if (filterMeta) {
    filterMeta.textContent = `当前渠道共 ${channelModels.length} 条，筛选后 ${filtered.length} 条。`;
  }

  const panelTitle = document.getElementById("modelsPanelTitle");
  if (panelTitle) {
    panelTitle.textContent = currentModelChannel ? `${channelLabel} 模型` : "全部模型";
  }

  const panelHint = document.getElementById("modelsPanelHint");
  if (panelHint) {
    panelHint.textContent = filtered.length > 0
      ? "启停、编辑与删除都在行内完成；默认模型请在编辑弹窗里维护。"
      : "当前筛选条件下没有命中的模型记录。";
  }
}

function refreshChannelKey(channel) {
  return String(channel || "").trim().toLowerCase();
}

function sortTextValues(values) {
  return values
    .map((value) => String(value || "").trim())
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" }));
}

function normalizeModelRefreshResult(data, fallbackChannel) {
  return {
    channel: String(data.channel || fallbackChannel || "").trim(),
    source: String(data.source || "").trim(),
    concurrency: Number(data.concurrency ?? modelRefreshConcurrency),
    discovered: Number(data.discovered ?? 0),
    verified: Number(data.verified ?? 0),
    added: Number(data.added ?? 0),
    updated: Number(data.updated ?? 0),
    deleted: Number(data.deleted ?? 0),
    offline: Number(data.offline ?? 0),
    skipped: Boolean(data.skipped),
    deletedModelIDs: sortTextValues(Array.isArray(data.deleted_model_ids) ? data.deleted_model_ids : []),
    offlineModelIDs: sortTextValues(Array.isArray(data.offline_model_ids) ? data.offline_model_ids : []),
  };
}

// modelRefreshSourceLabel names the source that produced a refresh result.
//
// Every publishable source is an upstream catalog read for an active account.
// A locally cached or compiled-in catalog is never a refresh result, so if one
// ever appears here it is labelled as non-upstream instead of being dressed up
// as an observation.
function modelRefreshSourceLabel(source) {
  const value = String(source || "").trim();
  if (!value) return "未知来源";
  const labels = {
    // Upstream catalogs.
    grok_build_models: "Grok Build OAuth 目录",
    workbuddy_cli_models: "WorkBuddy /v3/config 白名单",
    qoder_upstream_models: "Qoder 有符号上游目录",
    cline_recommended_models: "Cline 推荐模型目录",
    // Not an observation: nothing was read from an upstream account.
    no_active_account: "无 active 账号（未拉取，未发布）",
  };
  if (labels[value]) return labels[value];
  if (value.startsWith("grok_build_models")) return "Grok Build OAuth 目录";
  if (value.startsWith("workbuddy_cli_models")) return "WorkBuddy /v3/config 白名单";
  if (value.startsWith("qoder_upstream_models")) return "Qoder 有符号上游目录";
  if (value.startsWith("cline_recommended_models")) return "Cline 推荐模型目录";
  // Anything reaching here is a cached or compiled-in list. It must not be
  // mistaken for a fresh upstream observation.
  if (value.includes("cached") || value.includes("builtin") || value.endsWith("_unverified")) {
    return `${value}（非上游目录，不应出现）`;
  }
  return value;
}

// isUpstreamModelRefreshSource reports whether a result came from an upstream
// catalog read. Only those may change the published model list.
function isUpstreamModelRefreshSource(source) {
  const value = String(source || "").trim();
  return value.startsWith("grok_build_models") ||
    value.startsWith("workbuddy_cli_models") ||
    value.startsWith("qoder_upstream_models") ||
    value.startsWith("cline_recommended_models");
}

function renderModelRefreshSummary() {
  const summary = document.getElementById("modelsRefreshSummary");
  const title = document.getElementById("modelsRefreshSummaryTitle");
  const meta = document.getElementById("modelsRefreshSummaryMeta");
  const statGrid = document.getElementById("modelsRefreshStatGrid");
  const deletedBlock = document.getElementById("modelsRefreshDeletedBlock");
  const deletedList = document.getElementById("modelsRefreshDeletedList");
  if (!summary || !title || !meta || !statGrid || !deletedBlock || !deletedList) return;

  const channel = currentModelChannel || modelChannels()[0] || "";
  const result = modelRefreshResults[refreshChannelKey(channel)];
  if (!result) {
    summary.hidden = true;
    statGrid.innerHTML = "";
    deletedList.innerHTML = "";
    deletedBlock.hidden = true;
    return;
  }

  summary.hidden = false;
  title.textContent = `${result.channel || channel} 最近一次刷新结果`;
  // A refresh that published nothing has two very different causes: no active
  // account (nothing was read), or a catalog-only read (verified stays 0 on
  // purpose). Naming the cause is what keeps "同步 0" from looking like a bug.
  let skippedNote = "";
  if (result.skipped || result.source === "no_active_account") {
    skippedNote = "该渠道没有 active 账号，本次未向上游拉取，也未写入任何模型。";
  } else if (Number(result.verified) === 0) {
    skippedNote = "本次只读取上游目录，未逐个验证模型可用性。";
  } else if (!isUpstreamModelRefreshSource(result.source)) {
    skippedNote = "该来源不是上游目录，结果不会写入模型列表。";
  }
  meta.textContent = `来源：${modelRefreshSourceLabel(result.source)}。并发数 ${result.concurrency || modelRefreshConcurrency}。${skippedNote}`;

  const stats = [
    { label: "发现", value: result.discovered },
    { label: "同步", value: result.verified },
    { label: "新增", value: result.added },
    { label: "更新", value: result.updated },
    { label: "下线", value: result.offline },
  ];
  statGrid.innerHTML = stats.map((item) => `
    <div class="models-refresh-stat">
      <div class="models-refresh-stat-label">${escapeHtml(item.label)}</div>
      <div class="models-refresh-stat-value">${escapeHtml(String(item.value))}</div>
    </div>
  `).join("");

  const deletedItems = sortTextValues([...(result.offlineModelIDs || []), ...(result.deletedModelIDs || [])]);
  deletedBlock.hidden = deletedItems.length === 0;
  deletedList.innerHTML = deletedItems.map((item) => `
    <span class="models-refresh-deleted-item">${escapeHtml(item)}</span>
  `).join("");
}

function renderChannelTabs() {
  const container = document.getElementById("modelPlatformFilters");
  if (!container) return;

  const channels = modelChannels();
  if (!currentModelChannel || !channels.includes(currentModelChannel)) {
    currentModelChannel = channels[0] || "";
  }

  container.innerHTML = "";
  channels.forEach((channel) => {
    const btn = document.createElement("button");
    btn.className = `tab-item ${currentModelChannel === channel ? "active" : ""}`.trim();
    btn.type = "button";
    btn.textContent = channel;
    btn.dataset.channel = encodeData(channel);
    btn.addEventListener("click", () => {
      filterModelsByChannel(channel);
    });
    container.appendChild(btn);
  });

  updateRefreshButton();
}

function renderPagination(current, total) {
  const container = document.getElementById("modelsPaginationControls");
  if (!container) return;

  container.innerHTML = "";

  const appendButton = (label, page, disabled, active) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = `btn ${active ? "btn-primary" : "btn-outline"}`;
    btn.disabled = disabled;
    btn.dataset.page = String(page);
    btn.textContent = label;
    btn.style.padding = "6px 12px";
    container.appendChild(btn);
  };

  appendButton("首页", 1, current === 1, false);
  appendButton("上一页", current - 1, current === 1, false);

  let startPage = Math.max(1, current - 2);
  let endPage = Math.min(total, startPage + 4);
  if (endPage-startPage < 4) {
    startPage = Math.max(1, endPage - 4);
  }

  for (let page = startPage; page <= endPage; page += 1) {
    appendButton(String(page), page, false, page === current);
  }

  appendButton("下一页", current + 1, current === total, false);
  appendButton("末页", total, current === total, false);

  container.onclick = (event) => {
    const btn = event.target.closest("button[data-page]");
    if (!btn || btn.disabled) return;
    const page = parseInt(btn.dataset.page || "", 10);
    if (Number.isNaN(page)) return;
    modelCurrentPage = page;
    renderModels();
  };
}

function renderModels() {
  const container = document.getElementById("modelsList");
  const channelModels = getChannelScopedModels();
  const filtered = getFilteredModels(channelModels);
  updateModelSummary(channelModels, filtered);
  renderModelRefreshSummary();

  const total = filtered.length;
  const totalPages = Math.max(1, Math.ceil(total / modelPageSize));
  if (modelCurrentPage > totalPages) modelCurrentPage = totalPages;
  if (modelCurrentPage < 1) modelCurrentPage = 1;

  const start = (modelCurrentPage - 1) * modelPageSize;
  const pageItems = filtered.slice(start, start + modelPageSize);
  const pageCount = document.getElementById("currentPageCount");
  if (pageCount) {
    pageCount.textContent = String(pageItems.length);
  }

  const paginationInfo = document.getElementById("modelsPaginationInfo");
  if (paginationInfo) {
    paginationInfo.textContent = `共 ${total} 条记录，第 ${modelCurrentPage}/${totalPages} 页`;
  }
  renderPagination(modelCurrentPage, totalPages);

  updateModelsBatchBar();
  if (pageItems.length === 0) {
    container.innerHTML = `
      <div class="models-empty empty-state-panel">
        <span class="models-empty-icon empty-state-mark">◈</span>
        <p>当前筛选条件下暂无模型数据</p>
      </div>
    `;
    return;
  }

  if (window.matchMedia("(max-width: 640px)").matches) {
    renderModelsMobile(container, pageItems);
    return;
  }

  const rows = pageItems.map((m) => {
    const status = statusMeta(m.status);
    const defaultBadge = m.is_default ? `<span class="models-default-badge">默认</span>` : "";

    return `
      <tr data-id="${encodeData(m.id)}">
        <td class="col-select">
          <input type="checkbox" class="row-checkbox" data-action="row-select" data-id="${encodeData(m.id)}" ${modelsSelectedIds.has(String(m.id)) ? "checked" : ""} />
        </td>
        <td class="col-model">
          <div class="models-cell-main">
            <div class="models-cell-title">
              <strong>${escapeHtml(m.name || m.model_id || "-")}</strong>
              ${defaultBadge}
            </div>
            <span class="models-model-id">${escapeHtml(m.model_id || "-")}</span>
          </div>
        </td>
        <td class="col-channel">${escapeHtml(m.channel || "-")}</td>
        <td class="col-status">
          <span class="models-status-badge" style="background:${status.bg};color:${status.color};border-color:${status.border};">${status.label}</span>
        </td>
        <td class="col-sort">${escapeHtml(String(m.sort_order ?? 0))}</td>
        <td class="col-toggle">
          <label class="toggle${normalizeModelStatus(m.status) === "available" ? " active" : ""}" title="${normalizeModelStatus(m.status) === "available" ? "点击下线" : "点击启用"}">
            <input type="checkbox" data-action="toggle-status" data-id="${encodeData(m.id)}" ${normalizeModelStatus(m.status) === "available" ? "checked" : ""} />
            <span class="toggle-slider"></span>
          </label>
        </td>
        <td class="col-actions">
          <div class="models-actions">
            <button type="button" class="btn btn-outline models-action-btn" data-action="edit" data-id="${encodeData(m.id)}">编辑</button>
            <button type="button" class="btn btn-outline models-action-btn models-action-btn-danger" data-action="delete" data-id="${encodeData(m.id)}">删除</button>
          </div>
        </td>
      </tr>
    `;
  }).join("");

  container.innerHTML = `
    <div class="table-wrap models-table-wrap">
      <table class="models-table">
        <thead>
          <tr>
            <th class="col-select"><input type="checkbox" data-action="select-all" aria-label="选择本页全部模型" /></th>
            <th class="col-model">模型</th>
            <th class="col-channel">渠道</th>
            <th class="col-status">状态</th>
            <th class="col-sort">排序</th>
            <th class="col-toggle">启用</th>
            <th class="col-actions">操作</th>
          </tr>
        </thead>
        <tbody>${rows}</tbody>
      </table>
    </div>
  `;

  container.onclick = (event) => {
    const target = event.target.closest("[data-action]");
    if (!target || !container.contains(target)) return;
    const action = target.dataset.action;
    if (action === "select-all") {
      pageItems.forEach(m => {
        if (target.checked) modelsSelectedIds.add(String(m.id));
        else modelsSelectedIds.delete(String(m.id));
      });
      renderModels();
      return;
    }
    if (action === "row-select") {
      const id = decodeData(target.dataset.id || "");
      if (target.checked) modelsSelectedIds.add(id);
      else modelsSelectedIds.delete(id);
      updateModelsBatchBar();
      refreshRowSelectionStyles(container);
      return;
    }
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    if (action === "edit") editModel(id);
    if (action === "delete") deleteModel(id);
  };

  container.onchange = (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action === "select-all") {
      // Handled by the click handler so the header checkbox and the rows stay in
      // step; letting both run would toggle the set twice.
      return;
    }
    if (target.dataset.action !== "toggle-status") return;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    toggleModelStatus(id, target.checked);
  };

  refreshRowSelectionStyles(container);
  updateModelsBatchBar();
}

// modelsSelectedIds survives paging so a selection is not silently lost when the
// operator moves between pages.
const modelsSelectedIds = new Set();

function refreshRowSelectionStyles(container) {
  container.querySelectorAll('input[data-action="row-select"]').forEach(input => {
    input.checked = modelsSelectedIds.has(decodeData(input.dataset.id || ''));
  });
  const header = container.querySelector('input[data-action="select-all"]');
  const rows = Array.from(container.querySelectorAll('input[data-action="row-select"]'));
  if (header) {
    const selected = rows.filter(input => input.checked).length;
    header.checked = rows.length > 0 && selected === rows.length;
    header.indeterminate = selected > 0 && selected < rows.length;
  }

  container.querySelectorAll("tr[data-id]").forEach((tr) => {
    const id = decodeData(tr.dataset.id || "");
    tr.classList.toggle("is-selected", modelsSelectedIds.has(id));
  });
}

function updateModelsBatchBar() {
  const bar = document.getElementById("modelsBatchBar");
  const count = document.getElementById("modelsSelectedCount");
  if (count) count.textContent = String(modelsSelectedIds.size);
  if (bar) bar.hidden = modelsSelectedIds.size === 0;
}

function clearModelSelection() {
  modelsSelectedIds.clear();
  renderModels();
}

async function runModelBatch(action) {
  if (modelBatchInFlight || !['enable', 'disable', 'delete', 'clear'].includes(action)) return;
  const scopedIds = new Set(getChannelScopedModels().map(model => String(model.id)));
  const ids = Array.from(modelsSelectedIds).filter(id => scopedIds.has(id));
  if (ids.length === 0) return;
  if (action === "clear") {
    clearModelSelection();
    return;
  }
  const labels = { enable: "启用", disable: "停用", delete: "删除" };
  if (action === "delete" && !confirm(`确定删除选中的 ${ids.length} 个模型吗？此操作不可撤销。`)) return;

  modelBatchInFlight = true;
  const batchButtons = document.querySelectorAll('#modelsBatchBar button');
  batchButtons.forEach(button => { button.disabled = true; });
  let done = 0;
  let failed = 0;
  for (const id of ids) {
    try {
      if (action === "delete") {
        const response = await fetch(`/api/models/${encodeURIComponent(id)}`, { method: "DELETE" });
        if (!response.ok) throw new Error("HTTP " + response.status);
      } else {
        const current = models.find((m) => String(m.id) === String(id));
        if (!current) throw new Error("模型已不在列表中");
        const response = await fetch(`/api/models/${encodeURIComponent(id)}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...current, status: action === "enable" ? "available" : "offline" }),
        });
        if (!response.ok) throw new Error("HTTP " + response.status);
      }
      done += 1;
    } catch (error) {
      failed += 1;
      console.warn("batch model action failed", id, error);
    }
  }
  modelsSelectedIds.clear();
  showToast(failed === 0
    ? `已批量${labels[action]} ${done} 个模型`
    : `批量${labels[action]}完成：成功 ${done}，失败 ${failed}`, failed === 0 ? "success" : "error");
  try { await loadModels(); } finally {
    modelBatchInFlight = false;
    batchButtons.forEach(button => { button.disabled = false; });
  }
}

function renderModelsMobile(container, pageItems) {
  const cards = pageItems.map((m) => {
    const status = statusMeta(m.status);
    const defaultBadge = m.is_default ? `<span class="models-default-badge">默认</span>` : "";
    return `
      <article class="models-mobile-card">
        <div class="models-mobile-head">
          <div class="models-cell-title">
            <strong>${escapeHtml(m.name || m.model_id || "-")}</strong>
            ${defaultBadge}
          </div>
          <span class="models-status-badge" style="background:${status.bg};color:${status.color};border-color:${status.border};">${status.label}</span>
        </div>
        <div class="models-model-id">${escapeHtml(m.model_id || "-")}</div>
        <div class="models-mobile-grid">
          <div class="models-mobile-item">
            <span class="models-mobile-label">渠道</span>
            <span>${escapeHtml(m.channel || "-")}</span>
          </div>
          <div class="models-mobile-item">
            <span class="models-mobile-label">排序</span>
            <span>${escapeHtml(String(m.sort_order ?? 0))}</span>
          </div>
          <div class="models-mobile-item">
            <span class="models-mobile-label">启用</span>
            <label class="toggle${normalizeModelStatus(m.status) === "available" ? " active" : ""}" title="${normalizeModelStatus(m.status) === "available" ? "点击下线" : "点击启用"}">
              <input type="checkbox" data-action="toggle-status" data-id="${encodeData(m.id)}" ${normalizeModelStatus(m.status) === "available" ? "checked" : ""} />
              <span class="toggle-slider"></span>
            </label>
          </div>
        </div>
        <div class="models-mobile-actions">
          <button type="button" class="btn btn-outline models-action-btn" data-action="edit" data-id="${encodeData(m.id)}">编辑</button>
          <button type="button" class="btn btn-outline models-action-btn models-action-btn-danger" data-action="delete" data-id="${encodeData(m.id)}">删除</button>
        </div>
      </article>
    `;
  }).join("");

  container.innerHTML = `<div class="models-mobile-list">${cards}</div>`;

  container.onclick = (event) => {
    const target = event.target.closest("[data-action]");
    if (!target || !container.contains(target)) return;
    const action = target.dataset.action;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    if (action === "edit") editModel(id);
    if (action === "delete") deleteModel(id);
  };

  container.onchange = (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action !== "toggle-status") return;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    toggleModelStatus(id, target.checked);
  };
}

async function loadModels() {
  try {
    const res = await fetch("/api/models");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    models = await res.json() || [];
    renderChannelTabs();
    updateModelChannelOptions();
    renderModels();
    updateRefreshButton();
  } catch (err) {
    showToast("加载模型失败", "error");
  }
}

function filterModelsByChannel(channel) {
  modelsSelectedIds.clear();
  updateModelsBatchBar();
  currentModelChannel = channel;
  modelCurrentPage = 1;
  document.querySelectorAll("#modelPlatformFilters .tab-item").forEach((btn) => {
    btn.classList.toggle("active", btn.textContent === channel);
  });
  renderModels();
  updateRefreshButton();
}

function updateModelChannelOptions() {
  const select = document.getElementById("modelChannel");
  if (!select) return;

  const previous = select.value;
  select.innerHTML = "";
  modelChannels().forEach((channel) => {
    const option = document.createElement("option");
    option.value = channel;
    option.textContent = channel;
    select.appendChild(option);
  });

  if (previous && Array.from(select.options).some((option) => option.value === previous)) {
    select.value = previous;
  } else if (currentModelChannel && Array.from(select.options).some((option) => option.value === currentModelChannel)) {
    select.value = currentModelChannel;
  }
}

function openModelModal(model = null) {
  const modal = document.getElementById("modelModal");
  const title = document.getElementById("modelModalTitle");
  const form = document.getElementById("modelForm");

  updateModelChannelOptions();

  const setSelectValue = (el, value) => {
    if (!el) return;
    const raw = value === null || value === undefined ? "" : String(value);
    el.value = raw;
    if (el.tagName === "SELECT" && el.value !== raw) {
      el.selectedIndex = 0;
    }
  };

  if (model) {
    title.textContent = "编辑模型";
    document.getElementById("modelId").value = model.id;
    setSelectValue(document.getElementById("modelChannel"), model.channel);
    document.getElementById("modelModelId").value = model.model_id;
    document.getElementById("modelName").value = model.name;
    document.getElementById("modelSortOrder").value = model.sort_order;
    setSelectValue(document.getElementById("modelStatus"), normalizeModelStatus(model.status));
    document.getElementById("modelIsDefault").checked = !!model.is_default;
  } else {
    title.textContent = "添加模型";
    form.reset();
    document.getElementById("modelId").value = "";
    const defaultProvider = window.OrchidsProviderRegistry?.get(window.OrchidsProviderRegistry?.defaultProviderKey);
    setSelectValue(document.getElementById("modelChannel"), currentModelChannel || defaultProvider?.label || "");
    document.getElementById("modelSortOrder").value = "0";
    setSelectValue(document.getElementById("modelStatus"), "available");
  }

  modal.classList.add("active");
  modal.style.display = "flex";
}

function closeModelModal() {
  const modal = document.getElementById("modelModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

async function saveModel(event) {
  event.preventDefault();

  const id = document.getElementById("modelId").value;
  const data = {
    channel: document.getElementById("modelChannel").value,
    model_id: document.getElementById("modelModelId").value,
    name: document.getElementById("modelName").value,
    sort_order: parseInt(document.getElementById("modelSortOrder").value, 10) || 0,
    status: document.getElementById("modelStatus").value,
    is_default: document.getElementById("modelIsDefault").checked,
  };

  if (id) {
    data.id = id;
  }

  try {
    const url = id ? `/api/models/${id}` : "/api/models";
    const method = id ? "PUT" : "POST";
    const res = await fetch(url, {
      method,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
    });
    if (!res.ok) throw new Error(await res.text());
    closeModelModal();
    await loadModels();
    showToast("保存成功");
  } catch (err) {
    showToast(`保存失败: ${err.message}`, "error");
  }
}

function editModel(id) {
  const model = models.find((item) => item.id === id);
  if (model) openModelModal(model);
}

async function toggleModelStatus(id, enabled) {
  const model = models.find((item) => item.id === id);
  if (!model) return;

  try {
    const updatedModel = { ...model, status: enabled ? "available" : "offline" };
    const res = await fetch(`/api/models/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(updatedModel),
    });
    if (!res.ok) throw new Error(await res.text());
    showToast(enabled ? "模型已启用" : "模型已禁用");
    await loadModels();
  } catch (err) {
    showToast(`操作失败: ${err.message}`, "error");
  }
}

async function deleteModel(id) {
  if (!confirm("确定要删除这个模型吗？")) return;
  try {
    const res = await fetch(`/api/models/${id}`, { method: "DELETE" });
    if (!res.ok) throw new Error(await res.text());
    showToast("删除成功");
    await loadModels();
  } catch (err) {
    showToast(`删除失败: ${err.message}`, "error");
  }
}

function updateRefreshButton() {
  const button = document.getElementById("refreshModelsButton");
  const deleteButton = document.getElementById("deleteOfflineModelsButton");

  const channel = currentModelChannel || modelChannels()[0] || "";
  if (button) {
    button.disabled = modelRefreshInFlight || modelDeleteOfflineInFlight || !channel;
    if (!channel) {
      button.textContent = "刷新当前渠道";
    } else {
      button.textContent = modelRefreshInFlight
        ? `正在刷新 ${channel}...`
        : `刷新 ${channel} 列表`;
    }
  }
  if (deleteButton) {
    const offlineCount = getChannelScopedModels()
      .filter((m) => normalizeModelStatus(m.status) === "offline")
      .length;
    deleteButton.disabled = modelRefreshInFlight || modelDeleteOfflineInFlight || !channel || offlineCount === 0;
    deleteButton.textContent = modelDeleteOfflineInFlight
      ? "正在删除..."
      : `删除已下线${offlineCount > 0 ? ` (${offlineCount})` : ""}`;
  }
}

async function refreshModelsForCurrentChannel() {
  const channel = currentModelChannel || modelChannels()[0] || "";
  if (!channel || modelRefreshInFlight) return;

  modelRefreshInFlight = true;
  updateRefreshButton();

  try {
    const res = await fetch("/api/models/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ channel, concurrency: modelRefreshConcurrency }),
    });

    const raw = await res.text();
    let data = {};
    if (raw) {
      try {
        data = JSON.parse(raw);
      } catch (err) {
        data = { message: raw };
      }
    }

    if (!res.ok) {
      throw new Error(data.message || raw || "刷新失败");
    }

    const normalized = normalizeModelRefreshResult(data, channel);
    if (normalized.channel) {
      modelRefreshResults[refreshChannelKey(normalized.channel)] = normalized;
    }

    await loadModels();

    if (normalized.skipped || data.source === "no_active_account") {
      showToast(`${channel} 未刷新：该渠道没有 active 账号，未从上游拉取，也未写入模型`, "info");
      return;
    }

    const parts = [
      `并发 ${data.concurrency ?? modelRefreshConcurrency}`,
      `发现 ${data.discovered ?? 0}`,
      `同步 ${data.verified ?? 0}`,
      `新增 ${data.added ?? 0}`,
      `更新 ${data.updated ?? 0}`,
      `删除 ${data.deleted ?? 0}`,
    ];
    const hasDeleted = normalized.deletedModelIDs.length > 0;
    showToast(`${channel} 刷新完成：${parts.join("，")}${hasDeleted ? "。已在下方列出删除模型" : ""}`);
  } catch (err) {
    showToast(`刷新失败: ${err.message}`, "error");
  } finally {
    modelRefreshInFlight = false;
    updateRefreshButton();
  }
}

async function deleteOfflineModelsForCurrentChannel() {
  const channel = currentModelChannel || modelChannels()[0] || "";
  if (!channel || modelDeleteOfflineInFlight) return;

  const targets = getChannelScopedModels()
    .filter((m) => normalizeModelStatus(m.status) === "offline");
  if (targets.length === 0) {
    showToast(`${channel} 没有已下线模型`, "info");
    updateRefreshButton();
    return;
  }
  if (!confirm(`确定删除 ${channel} 渠道的 ${targets.length} 个已下线模型吗？`)) {
    return;
  }

  modelDeleteOfflineInFlight = true;
  updateRefreshButton();
  let deleted = 0;
  const failures = [];
  try {
    for (const model of targets) {
      try {
        const res = await fetch(`/api/models/${encodeURIComponent(model.id)}`, { method: "DELETE" });
        if (!res.ok) throw new Error(await res.text());
        deleted += 1;
      } catch (err) {
        failures.push(`${model.model_id || model.id}: ${err.message || err}`);
      }
    }
    await loadModels();
    if (failures.length > 0) {
      showToast(`已删除 ${deleted} 个，失败 ${failures.length} 个`, "error");
      console.warn("delete offline models failures", failures);
    } else {
      showToast(`已删除 ${deleted} 个已下线模型`);
    }
  } finally {
    modelDeleteOfflineInFlight = false;
    updateRefreshButton();
  }
}



document.addEventListener("DOMContentLoaded", async () => {
  await window.OrchidsProviderRegistry?.ready;
  const searchInput = document.getElementById("modelSearchInput");
  const statusFilter = document.getElementById("modelStatusFilter");
  const pageSize = document.getElementById("modelPageSize");
  const refreshConcurrency = document.getElementById("modelRefreshConcurrency");

  // Batch actions act on the current selection through the per-model endpoints,
  // so no new server surface is needed for a multi-row edit.
  const batchBar = document.getElementById("modelsBatchBar");
  if (batchBar) {
    batchBar.addEventListener("click", (event) => {
      const button = event.target.closest("[data-batch]");
      if (!button) return;
      runModelBatch(button.dataset.batch);
    });
  }

  if (searchInput) {
    // Every keystroke rebuilt the whole table: the search box filters a list that
    // can hold thousands of rows, and renderModels() also rebuilds pagination and
    // the mobile card list. Deferring collapses a burst of typing (or a paste)
    // into a single render.
    let searchDebounceTimer = 0;
    searchInput.addEventListener("input", (event) => {
      const value = String(event.target.value || "").trim();
      if (value === modelSearchTerm) return;
      modelSearchTerm = value;
      modelCurrentPage = 1;
      if (searchDebounceTimer) clearTimeout(searchDebounceTimer);
      searchDebounceTimer = setTimeout(() => {
        searchDebounceTimer = 0;
        renderModels();
      }, 160);
    });
  }

  if (statusFilter) {
    statusFilter.addEventListener("change", (event) => {
      modelStatusFilter = String(event.target.value || "").trim();
      modelCurrentPage = 1;
      renderModels();
    });
  }

  if (pageSize) {
    // The select is the source of truth for the first render: initialising the
    // state to a different number than the markup's selected option made the page
    // render 50 rows while the control read 每页 20 条 (and the 本页 label said 50).
    modelPageSize = parseInt(pageSize.value || "20", 10) || 20;
    pageSize.addEventListener("change", (event) => {
      modelPageSize = parseInt(event.target.value || "20", 10) || 20;
      modelCurrentPage = 1;
      renderModels();
    });
  }

  if (refreshConcurrency) {
    modelRefreshConcurrency = parseInt(refreshConcurrency.value || "4", 10) || 4;
    refreshConcurrency.addEventListener("change", (event) => {
      modelRefreshConcurrency = parseInt(event.target.value || "4", 10) || 4;
    });
  }

  loadModels();
});
