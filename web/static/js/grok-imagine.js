(() => {
  const batchSize = () => Math.max(1, Math.min(6, Number(document.getElementById("imagineCount")?.value) || 6));
  const STORAGE_KEY = "grok_tools_ui_v1";
  let imageModels = [];
  // availability is what the tools page learned about the Grok Web pool:
  // webPlans counts accounts by plan, media whether any account still has
  // the image / image_pro / image_edit allowance. Unknown (null) changes nothing.
  let availability = { webPlans: null, media: null };
  const PARALLELISM = 2;

  // freeOnlyPool reports a pool of Free Web accounts only. Such a pool runs
  // one image at a time; any paid or unclassified account keeps the parallel
  // batch.
  function freeOnlyPool() {
    const plans = availability.webPlans;
    return Boolean(plans) && Number(plans.free) > 0 && !Number(plans.paid) && !Number(plans.unknown);
  }

  // Quality tiers: 标准 is the Imagine model, 高质量 Imagine 2.0. The lite
  // (快速) tier is not offered: grok.com answered it with stock pictures
  // instead of drawings of the prompt.
  function qualityModels() {
    const ids = imageModels.map((item) => String(item?.id || "")).filter(Boolean);
    const basic = ids.find((id) => /^grok-imagine-image$/i.test(id)) || "grok-imagine-image";
    const quality = ids.find((id) => /image-2\.0$/i.test(id)) || basic;
    return { basic, quality };
  }

  const state = {
    initialized: false,
    ready: false,
    running: false,
    abortController: null,
    selectionMode: false,
    selected: new Set(),
    finalMinBytes: 100000,
    nsfwEnabled: true,
    saveState: null,
    showToast: null,
    editFileIDs: [],
    editFileNames: [],
    editUploadVersion: 0,
  };

  function $(id) {
    return document.getElementById(id);
  }

  function toast(message, type = "info") {
    if (typeof state.showToast === "function") state.showToast(message, type);
    else if (typeof window.showToast === "function") window.showToast(message, type);
  }

  function loadState() {
    try {
      const parsed = JSON.parse(localStorage.getItem(STORAGE_KEY) || "{}");
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (err) {
      return {};
    }
  }

  function saveState(patch) {
    if (typeof state.saveState === "function") {
      state.saveState(patch);
      return;
    }
    try {
      const current = loadState();
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...current, ...patch }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function readToggle(selector, attr, fallback) {
    const active = document.querySelector(`${selector} .is-active`);
    return String(active?.dataset?.[attr] || fallback || "").trim();
  }

  function setToggle(selector, attr, value) {
    const dataAttr = attr.replace(/[A-Z]/g, (m) => `-${m.toLowerCase()}`);
    document.querySelectorAll(`${selector} [data-${dataAttr}]`).forEach((btn) => {
      const active = String(btn.dataset[attr] || "") === String(value || "");
      btn.classList.toggle("is-active", active);
      btn.setAttribute("aria-pressed", active ? "true" : "false");
    });
  }

  function qualityModel(quality) {
    const models = qualityModels();
    return models[quality] || models.basic;
  }

  function normalizeQuality(value) {
    const raw = String(value || "").toLowerCase();
    return raw === "quality" ? "quality" : "basic";
  }

  function syncQualityModel() {
    const quality = normalizeQuality(readToggle("#imagineQualityToggle", "imagineQuality", "basic"));
    const model = qualityModel(quality);
    return { quality, model };
  }

  // Both tiers accept a 1k/2k resolution.
  function syncResolution() {
    const resolution = document.getElementById("imagineResolution");
    if (resolution) resolution.disabled = false;
  }

  function aspectRatioCss(value) {
    const ratio = String(value || "1:1").trim();
    if (!ratio.includes(":")) return "1 / 1";
    const [w, h] = ratio.split(":");
    return `${Math.max(1, Number(w) || 1)} / ${Math.max(1, Number(h) || 1)}`;
  }

  function sizeForRatio(value) {
    switch (String(value || "").trim()) {
      case "16:9":
      case "3:2":
        return "1536x1024";
      case "9:16":
      case "2:3":
        return "1024x1536";
      default:
        return "1024x1024";
    }
  }

  function inferMime(raw) {
    const value = String(raw || "");
    if (value.startsWith("iVBOR")) return "image/png";
    if (value.startsWith("R0lGOD")) return "image/gif";
    if (value.startsWith("UklGR")) return "image/webp";
    return "image/jpeg";
  }

  function imageSrc(raw) {
    let value = String(raw || "");
    if (!value) return "";
    if (inferencePrefix() === "/api/grok/tools/v1") {
      value = value.replace(/^\/(?:grok\/)?v1\/files\//i, `${inferencePrefix()}/files/`);
    }
    if (/^(https?:|data:|\/)/i.test(value)) return value;
    return `data:${inferMime(value)};base64,${value}`;
  }

  function inferenceHeaders(headers = {}) {
    return window.GrokToolRequest?.headers?.(headers) || headers;
  }
  function inferencePrefix() { return window.GrokToolRequest?.prefix?.() || "/api/grok/tools/v1"; }
  function imageOperation() { return readToggle("#imagineOperationToggle", "imagineOperation", "generate") === "edit" ? "edit" : "generate"; }

  function editURLInputs() {
    return String($("imagineImageURLs")?.value || "").split(/\r?\n/).map((value) => value.trim()).filter(Boolean);
  }

  function syncEditUI() {
    const edit = imageOperation() === "edit";
    $("imagineEditInputs")?.classList.toggle("hidden", !edit);
    $("imagineRunModeToggle")?.closest(".imagine-basic-field")?.classList.toggle("hidden", edit);
    if ($("imagineRunModeHint")) $("imagineRunModeHint").textContent = edit
      ? "编辑模式为单轮请求，最多可组合 7 张已暂存图片或 URL。"
      : "单轮只生成一批图；连续模式会在上一轮结束后自动开始下一轮，直到点击停止。";
  }

  function renderEditFiles(message) {
    const status = $("imagineImageFilesStatus");
    if (!status) return;
    status.textContent = message || (state.editFileNames.length ? state.editFileNames.join(" · ") : "未选择文件");
  }

  async function stageEditFiles(files) {
    const selected = Array.from(files || []).slice(0, 7);
    if (!selected.length) return;
    const version = ++state.editUploadVersion;
    state.editFileIDs = [];
    state.editFileNames = selected.map((file) => file.name);
    renderEditFiles(`正在暂存 0/${selected.length}…`);
    try {
      for (let index = 0; index < selected.length; index += 1) {
        const fileID = await window.GrokMediaStaging.upload(selected[index], "image");
        if (version !== state.editUploadVersion) return;
        state.editFileIDs.push(fileID);
        renderEditFiles(`正在暂存 ${index + 1}/${selected.length}…`);
      }
      renderEditFiles();
    } catch (err) {
      if (version !== state.editUploadVersion) return;
      state.editFileIDs = [];
      renderEditFiles("暂存失败");
      toast(err.message || "图片暂存失败", "error");
    }
  }

  async function requestImageEdit(prompt, ratio, model, quality, signal, count = 1) {
    const inputs = state.editFileIDs.map((file_id) => ({ file_id }));
    for (const url of editURLInputs()) inputs.push({ url });
    if (!inputs.length) throw new Error("请上传或填写至少一张编辑素材");
    if (inputs.length > 7) throw new Error("编辑素材不能超过 7 张");
    const res = await fetch(`${inferencePrefix()}/images/edits`, {
      method: "POST",
      headers: inferenceHeaders({ "Content-Type": "application/json" }),
      signal,
      body: JSON.stringify({
        // Edits are served by the dedicated edit route whatever tier is picked.
        model: "grok-imagine-image-edit",
        prompt,
        images: inputs,
        n: Math.max(1, Math.min(2, count)),
        aspect_ratio: ratio,
        size: sizeForRatio(ratio),
        response_format: "url",
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    const images = extractImageValues(await res.json());
    if (!images.length) throw new Error("no image generated");
    return images;
  }

  function resizePrompt() {
    const input = $("imaginePrompt");
    if (!input) return;
    input.style.height = "52px";
    input.style.height = `${Math.min(Math.max(input.scrollHeight, 52), 160)}px`;
    input.style.overflowY = input.scrollHeight > 160 ? "auto" : "hidden";
  }

  function syncRatioUI() {
    const wrap = $("imagineRatioWrap");
    if (wrap) wrap.dataset.ratio = String($("imagineRatio")?.value || "2:3");
  }

  function setStatus(text) {
    const el = $("imagineStatus");
    if (el) el.textContent = text;
  }

  function setButtons(running) {
    const start = $("imagineStartBtn");
    const stop = $("imagineStopBtn");
    if (start) {
      start.disabled = false;
      start.title = running ? "停止" : "生成";
      start.setAttribute("aria-label", running ? "停止" : "生成");
      start.classList.toggle("is-running", running);
      start.innerHTML = running
        ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 7v10"></path><path d="M14 7v10"></path></svg>'
        : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"></path><path d="m6 11 6-6 6 6"></path></svg>';
    }
    if (stop) stop.classList.toggle("hidden", !running);
    document.querySelectorAll("#imagineRunModeToggle button, #imagineQualityToggle button").forEach((btn) => {
      btn.disabled = running;
    });
    ["imaginePrompt", "imagineRatio"].forEach((id) => {
      const el = $(id);
      if (el) el.disabled = running;
    });
  }

  function setEmptyState() {
    const grid = $("imagineGrid");
    const empty = $("imagineEmpty");
    if (!grid || !empty) return;
    const hasBatch = grid.querySelector(".imagine-masonry-batch") !== null;
    empty.hidden = hasBatch;
    empty.style.display = hasBatch ? "none" : "";
  }

  function createBatch(prompt, ratio, quality, round) {
    const grid = $("imagineGrid");
    if (!grid) return null;
    const batch = document.createElement("section");
    batch.className = "imagine-masonry-batch";

    const head = document.createElement("header");
    head.className = "imagine-masonry-batch-head";
    const promptEl = document.createElement("div");
    promptEl.className = "imagine-masonry-batch-prompt";
    promptEl.textContent = prompt;
    const meta = document.createElement("div");
    meta.className = "imagine-masonry-batch-meta";
    [
      ["is-round", `第 ${round} 轮`],
      ["is-param", ratio],
      ["is-param", quality === "basic" ? "Basic" : quality === "quality" ? "Quality" : "Lite"],
      ["is-count", `0/${batchSize()}`],
      ["is-state", "正在生成"],
    ].forEach(([cls, text]) => {
      const chip = document.createElement("span");
      chip.className = `imagine-masonry-batch-chip ${cls}`;
      chip.textContent = text;
      if (cls === "is-state") chip.dataset.state = "generating";
      meta.appendChild(chip);
    });
    head.appendChild(promptEl);
    head.appendChild(meta);

    const slotGrid = document.createElement("div");
    slotGrid.className = "imagine-masonry-grid";
    slotGrid.style.setProperty("--tile-aspect", aspectRatioCss(ratio));

    const slots = Array.from({ length: batchSize() }, (_, index) => {
      const tile = document.createElement("article");
      tile.className = "imagine-masonry-tile waterfall-item is-pending";
      tile.dataset.prompt = prompt;
      const checkbox = document.createElement("div");
      checkbox.className = "image-checkbox";
      const badge = document.createElement("div");
      badge.className = "imagine-masonry-tile-badge";
      badge.textContent = String(index + 1);
      const body = document.createElement("div");
      body.className = "imagine-masonry-tile-body";
      body.innerHTML = '<div class="imagine-masonry-tile-progress"><div class="imagine-masonry-tile-progress-value">排队中</div><div class="imagine-masonry-tile-progress-track"><div class="imagine-masonry-tile-progress-fill"></div></div></div>';
      tile.appendChild(checkbox);
      tile.appendChild(badge);
      tile.appendChild(body);
      slotGrid.appendChild(tile);
      return { tile, body, status: "pending", progress: 0, url: "" };
    });

    batch.appendChild(head);
    batch.appendChild(slotGrid);
    grid.prepend(batch);
    setEmptyState();
    return {
      countEl: meta.querySelector(".is-count"),
      stateEl: meta.querySelector(".is-state"),
      slots,
      ready: 0,
      failed: 0,
    };
  }

  function updateBatch(batch, final) {
    if (!batch) return;
    if (batch.countEl) batch.countEl.textContent = `${batch.ready}/${batch.slots.length}`;
    if (!batch.stateEl) return;
    if (!final) return;
    if (batch.ready >= batch.slots.length) {
      batch.stateEl.dataset.state = "success";
      batch.stateEl.textContent = "生成成功";
    } else if (batch.ready > 0) {
      batch.stateEl.dataset.state = "partial";
      batch.stateEl.textContent = "部分失败";
    } else {
      batch.stateEl.dataset.state = "failed";
      batch.stateEl.textContent = "生成失败";
    }
  }

  function setSlotProgress(slot, value) {
    if (!slot || slot.status !== "pending") return;
    const progress = Math.max(0, Math.min(99, Number(value) || 0));
    slot.progress = progress;
    const text = slot.tile.querySelector(".imagine-masonry-tile-progress-value");
    const fill = slot.tile.querySelector(".imagine-masonry-tile-progress-fill");
    if (text) text.textContent = `${progress}%`;
    if (fill) fill.style.width = `${progress}%`;
  }

  function failSlot(slot, label) {
    if (!slot || slot.status !== "pending") return;
    slot.status = "failed";
    slot.tile.classList.remove("is-pending", "is-ready");
    slot.tile.classList.add("is-filtered");
    const message = document.createElement("div");
    message.className = "imagine-masonry-tile-label";
    message.textContent = label || "失败";
    slot.body.replaceChildren(message);
  }

  function finishSlot(slot, raw, meta) {
    const src = imageSrc(raw);
    if (!slot || slot.status !== "pending" || !src) {
      failSlot(slot, "失败");
      return;
    }
    slot.status = "ready";
    slot.url = src;
    slot.tile.classList.remove("is-pending", "is-filtered");
    slot.tile.classList.add("is-ready");
    slot.tile.dataset.imageUrl = src;
    slot.tile.dataset.prompt = String(meta?.prompt || "");
    if (state.selectionMode) slot.tile.classList.add("selection-mode");

    const link = document.createElement("div");
    link.className = "imagine-masonry-tile-link";
    const img = document.createElement("img");
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = "image";
    img.src = src;
    link.appendChild(img);
    slot.body.replaceChildren(link);
  }

  function extractImageValues(data) {
    return (Array.isArray(data?.data) ? data.data : [])
      .map((item) => String(item?.url || item?.b64_json || item?.base64 || item?.b64 || ""))
      .filter(Boolean);
  }

  // Every tier goes through /images/generations, where the server picks an
  // account that still has the tier's allowance. Edits go to /images/edits.
  async function requestImages(prompt, ratio, model, quality, nsfw, signal, count = 1) {
    if (imageOperation() === "edit") return requestImageEdit(prompt, ratio, model, quality, signal, count);
    const chosen = String(model || "").trim() || "grok-imagine-image";
    const res = await fetch(`${inferencePrefix()}/images/generations`, {
      method: "POST",
      headers: inferenceHeaders({ "Content-Type": "application/json" }),
      signal,
      body: JSON.stringify({
        model: chosen,
        prompt,
        n: Math.max(1, Math.min(2, count)),
        aspect_ratio: ratio,
        size: sizeForRatio(ratio),
        response_format: "url",
        nsfw,
        resolution: document.getElementById("imagineResolution")?.value || "1k",
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    const images = extractImageValues(await res.json());
    if (!images.length) throw new Error("no image generated");
    return images;
  }

  async function runSlots(batch, slots, prompt, ratio, model, quality, nsfw, signal) {
    let tick = 0;
    slots.forEach((slot) => setSlotProgress(slot, 0));
    const timer = window.setInterval(() => {
      tick += 1;
      slots.forEach((slot) => setSlotProgress(slot, Math.min(92, 8 + tick * 7)));
    }, 900);
    try {
      const images = await requestImages(prompt, ratio, model, quality, nsfw, signal, slots.length);
      slots.forEach((slot, index) => {
        if (images[index]) {
          finishSlot(slot, images[index], { prompt });
          batch.ready += 1;
        } else {
          failSlot(slot, "未返回");
          batch.failed += 1;
        }
      });
      updateBatch(batch, false);
      return slots.map((_, index) => Boolean(images[index]));
    } catch (err) {
      const exhausted = String(err?.message || "").includes("media_quota_exhausted");
      if (exhausted) batch.quotaExhausted = true;
      const reason = signal?.aborted ? "已停止" : (exhausted ? "额度用完" : "失败");
      slots.forEach((slot) => failSlot(slot, reason));
      batch.failed += slots.length;
      updateBatch(batch, false);
      if (!signal?.aborted && err?.message && !batch.errorShown) {
        batch.errorShown = true;
        toast(readableImageError(err.message), "error");
      }
      return slots.map(() => false);
    } finally {
      window.clearInterval(timer);
    }
  }

  function readableImageError(message) {
    try {
      const parsed = JSON.parse(message);
      return parsed?.error?.message || message;
    } catch (_) {
      return String(message || "").slice(0, 200);
    }
  }

  // runBatch fills the slots of one round. A Free-only pool runs one request
  // for one image at a time (the other slots wait as 排队中); otherwise two
  // requests of two images each run side by side.
  async function runBatch(batch, prompt, ratio, model, quality, nsfw, signal) {
    if (!freeOnlyPool()) return runBatchParallel(batch, prompt, ratio, model, quality, nsfw, signal);
    const results = new Array(batch.slots.length).fill(false);
    for (let index = 0; index < batch.slots.length; index += 1) {
      if (signal?.aborted) break;
      const [ok] = await runSlots(batch, [batch.slots[index]], prompt, ratio, model, quality, nsfw, signal);
      results[index] = ok;
      // A spent allowance does not come back within the batch.
      if (batch.quotaExhausted) break;
    }
    return results;
  }

  async function runBatchParallel(batch, prompt, ratio, model, quality, nsfw, signal) {
    const results = new Array(batch.slots.length).fill(false);
    let cursor = 0;
    const workers = Array.from({ length: Math.min(PARALLELISM, Math.ceil(batch.slots.length / 2)) }, async () => {
      while (cursor < batch.slots.length) {
        if (signal?.aborted) break;
        const index = cursor;
        cursor += 2;
        const slots = batch.slots.slice(index, index + 2);
        const outcome = await runSlots(batch, slots, prompt, ratio, model, quality, nsfw, signal);
        outcome.forEach((ok, offset) => { results[index + offset] = ok; });
        // A spent allowance does not come back within the batch.
        if (batch.quotaExhausted) break;
      }
    });
    await Promise.all(workers);
    return results;
  }

  // applyAvailability reflects what the pool can serve: 标准 needs an account
  // with the standard image allowance, 高质量 one with image_pro, and 编辑 one
  // with image_edit (Free plans have none). An edit choice that is no longer
  // offered falls back.
  function applyAvailability() {
    const media = availability.media || {};
    const allowed = { basic: media.image !== false, quality: media.image_pro !== false };
    const editAllowed = media.image_edit !== false;
    document.querySelectorAll("#imagineQualityToggle [data-imagine-quality]").forEach((btn) => {
      const quality = btn.dataset.imagineQuality === "quality" ? "quality" : "basic";
      if (allowed[quality]) btn.removeAttribute("title");
      else btn.title = quality === "quality" ? "当前账号的高质量出图额度已用完，等待重置后再试" : "当前账号的标准出图额度已用完，等待重置后再试";
    });
    document.querySelectorAll("#imagineOperationToggle [data-imagine-operation='edit']").forEach((btn) => {
      btn.disabled = !editAllowed;
      if (editAllowed) btn.removeAttribute("title");
      else btn.title = "当前账号没有改图额度（Free 账号不提供改图）";
    });
    if (!editAllowed && imageOperation() === "edit") {
      setToggle("#imagineOperationToggle", "imagineOperation", "generate");
      syncEditUI();
    }
  }

  async function start() {
    if (state.running) {
      await stop();
      return;
    }
    const promptInput = $("imaginePrompt");
    const prompt = String(promptInput?.value || "").trim();
    if (!prompt) {
      toast("请输入 Prompt", "error");
      return;
    }
    const ratio = String($("imagineRatio")?.value || "2:3");
    const operation = imageOperation();
    const runMode = operation === "edit" ? "single" : (readToggle("#imagineRunModeToggle", "imagineRunMode", "single") === "continuous" ? "continuous" : "single");
    if (operation === "edit" && !state.editFileIDs.length && !editURLInputs().length) {
      toast("请上传或填写至少一张编辑素材", "error");
      return;
    }
    const { quality, model } = syncQualityModel();

    saveState({
      imagineRatio: ratio,
      imagineRunMode: runMode,
      imagineQuality: quality,
    });

    state.running = true;
    state.abortController = new AbortController();
    setButtons(true);
    setStatus("生成中");

    let round = 0;
    try {
      while (state.running) {
        round += 1;
        const batch = createBatch(prompt, ratio, quality, round);
        if (!batch) throw new Error("瀑布流容器不存在");
        setStatus(`生成中 · 第 ${round} 轮`);
        const results = await runBatch(batch, prompt, ratio, model, quality, state.nsfwEnabled, state.abortController.signal);
        updateBatch(batch, true);
        if (!state.running || runMode !== "continuous") break;
        // A failed batch used to resolve normally and immediately start another
        // one forever. Stop after a completely failed round; partial failures
        // yield before retrying so continuous mode cannot become a request storm.
        const succeeded = results.filter(Boolean).length;
        if (succeeded === 0) {
          state.running = false;
          setStatus("连续生成已停止：本轮全部失败");
          toast("连续生成已停止：本轮全部失败，请检查配置或稍后重试", "error");
          break;
        }
        await new Promise((resolve) => window.setTimeout(resolve, succeeded < results.length ? 3000 : 500));
      }
      if (state.running) setStatus("完成");
    } catch (err) {
      if (!state.abortController?.signal?.aborted) {
        setStatus("错误");
        toast(`启动失败: ${err.message || err}`, "error");
      }
    } finally {
      state.running = false;
      state.abortController = null;
      setButtons(false);
      if (document.activeElement !== promptInput) promptInput?.focus();
    }
  }

  async function stop() {
    state.running = false;
    if (state.abortController) {
      state.abortController.abort();
      state.abortController = null;
    }
    setButtons(false);
    setStatus("已停止");
  }

  function clearGrid() {
    const grid = $("imagineGrid");
    const empty = $("imagineEmpty");
    if (grid) grid.innerHTML = "";
    if (empty && grid) {
      empty.hidden = false;
      empty.style.display = "";
      grid.appendChild(empty);
    }
    state.selected.clear();
    state.selectionMode = false;
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    setStatus("空闲");
  }

  function dataUrlToBlob(dataUrl) {
    const parts = String(dataUrl || "").split(",");
    if (parts.length < 2) return null;
    const match = parts[0].match(/data:(.*?);base64/);
    const mime = match ? match[1] : "application/octet-stream";
    try {
      const bytes = atob(parts.slice(1).join(","));
      const arr = new Uint8Array(bytes.length);
      for (let i = 0; i < bytes.length; i += 1) arr[i] = bytes.charCodeAt(i);
      return new Blob([arr], { type: mime });
    } catch (err) {
      return null;
    }
  }

  function updateSelectedCount() {
    const count = $("selectedCount");
    if (count) count.textContent = String(state.selected.size);
    const btn = $("downloadSelectedBtn");
    if (btn) btn.disabled = state.selected.size === 0;
    const toggle = $("toggleSelectAllBtn");
    if (toggle) {
      const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
      toggle.textContent = items.length > 0 && state.selected.size === items.length ? "取消全选" : "全选";
    }
  }

  function enterSelection() {
    state.selectionMode = true;
    state.selected.clear();
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.remove("hidden");
    const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
    if (items.length === 0) {
      state.selectionMode = false;
      if (toolbar) toolbar.classList.add("hidden");
      toast("暂无可下载图片", "info");
      updateSelectedCount();
      return;
    }
    items.forEach((item) => item.classList.add("selection-mode"));
    updateSelectedCount();
  }

  function exitSelection() {
    state.selectionMode = false;
    state.selected.clear();
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    document.querySelectorAll("#imagineGrid .waterfall-item").forEach((item) => {
      item.classList.remove("selection-mode", "selected");
    });
    updateSelectedCount();
  }

  function toggleSelectionMode() {
    if (state.selectionMode) exitSelection();
    else enterSelection();
  }

  function toggleItem(item) {
    if (!state.selectionMode || !item) return;
    if (item.classList.contains("selected")) {
      item.classList.remove("selected");
      state.selected.delete(item);
    } else {
      item.classList.add("selected");
      state.selected.add(item);
    }
    updateSelectedCount();
  }

  function toggleSelectAll() {
    const items = Array.from(document.querySelectorAll("#imagineGrid .waterfall-item.is-ready"));
    const allSelected = items.length > 0 && state.selected.size === items.length;
    state.selected.clear();
    items.forEach((item) => {
      item.classList.toggle("selected", !allSelected);
      if (!allSelected) state.selected.add(item);
    });
    updateSelectedCount();
  }

  const JSZIP_URL = "https://cdnjs.cloudflare.com/ajax/libs/jszip/3.10.1/jszip.min.js";
  const JSZIP_INTEGRITY = "sha384-+mbV2IY1Zk/X1p/nWllGySJSUN8uMs+gUAN10Or95UBH0fpj6GfKgPmgC5EXieXG";
  let jsZipPromise = null;

  function loadJSZip() {
    if (window.JSZip) return Promise.resolve(window.JSZip);
    if (jsZipPromise) return jsZipPromise;

    jsZipPromise = new Promise((resolve, reject) => {
      const script = document.createElement("script");
      script.src = JSZIP_URL;
      script.integrity = JSZIP_INTEGRITY;
      script.crossOrigin = "anonymous";
      script.onload = () => window.JSZip ? resolve(window.JSZip) : reject(new Error("JSZip unavailable"));
      script.onerror = () => reject(new Error("JSZip load failed"));
      document.head.appendChild(script);
    }).catch((error) => {
      jsZipPromise = null;
      throw error;
    });
    return jsZipPromise;
  }

  async function downloadSelected() {
    if (state.selected.size === 0) {
      toggleSelectAll();
      if (state.selected.size === 0) {
        toast("未选择图片", "info");
        return;
      }
    }
    const btn = $("downloadSelectedBtn");
    if (btn) {
      btn.disabled = true;
      btn.textContent = "打包中...";
    }
    let processed = 0;
    try {
      const JSZip = await loadJSZip();
      const zip = new JSZip();
      const folder = zip.folder("images");
      for (const item of state.selected) {
        const url = item.dataset.imageUrl || "";
        let blob = null;
        if (url.startsWith("data:")) blob = dataUrlToBlob(url);
        else if (url) {
          const response = await fetch(url);
          blob = await response.blob();
        }
        if (!blob) continue;
        processed += 1;
        folder.file(`image_${processed}.png`, blob);
        if (btn) btn.textContent = `打包中 ${processed}/${state.selected.size}`;
      }
      if (processed === 0) {
        toast("没有可下载的图片", "error");
        return;
      }
      const content = await zip.generateAsync({ type: "blob" });
      const link = document.createElement("a");
      link.href = URL.createObjectURL(content);
      link.download = `imagine_${new Date().toISOString().slice(0, 10)}_${Date.now()}.zip`;
      link.click();
      URL.revokeObjectURL(link.href);
      toast(`已打包 ${processed} 张图片`, "success");
      exitSelection();
    } catch (err) {
      toast("打包失败", "error");
    } finally {
      if (btn) {
        btn.disabled = state.selected.size === 0;
        btn.innerHTML = `下载 <span id="selectedCount" class="selected-count">${state.selected.size}</span>`;
      }
    }
  }

  async function ensureReady() {
    if (state.ready) return;
    try {
      const res = await fetch("/api/grok/admin/imagine/config", { cache: "no-store" });
      if (res.ok) {
        const data = await res.json();
        const value = parseInt(data?.final_min_bytes, 10);
        if (Number.isFinite(value) && value >= 0) state.finalMinBytes = value;
        if (typeof data?.nsfw === "boolean") state.nsfwEnabled = data.nsfw;
      }
    } catch (err) {
      // config is optional for the page
    }
    state.ready = true;
  }

  function bindEvents() {
    document.querySelectorAll("[data-imagine-operation]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = btn.dataset.imagineOperation === "edit" ? "edit" : "generate";
        setToggle("#imagineOperationToggle", "imagineOperation", value);
        saveState({ imagineOperation: value });
        syncEditUI();
      });
    });
    document.querySelectorAll("[data-imagine-run-mode]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = String(btn.dataset.imagineRunMode || "single");
        setToggle("#imagineRunModeToggle", "imagineRunMode", value);
        saveState({ imagineRunMode: value });
      });
    });
    document.querySelectorAll("[data-imagine-quality]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = normalizeQuality(btn.dataset.imagineQuality);
        setToggle("#imagineQualityToggle", "imagineQuality", value);
        syncResolution(value);
        saveState({ imagineQuality: value });
      });
    });
    const prompt = $("imaginePrompt");
    if (prompt) {
      prompt.addEventListener("keydown", async (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          event.preventDefault();
          await start();
        }
      });
      prompt.addEventListener("input", resizePrompt);
    }
    const ratio = $("imagineRatio");
    if (ratio) {
      ratio.addEventListener("change", () => {
        syncRatioUI();
        saveState({ imagineRatio: String(ratio.value || "") });
      });
    }
    $("imagineSelectImagesBtn")?.addEventListener("click", () => $("imagineImageFiles")?.click());
    $("imagineImageFiles")?.addEventListener("change", (event) => stageEditFiles(event.target.files));
    $("imagineClearImagesBtn")?.addEventListener("click", () => {
      state.editUploadVersion += 1;
      state.editFileIDs = [];
      state.editFileNames = [];
      if ($("imagineImageFiles")) $("imagineImageFiles").value = "";
      if ($("imagineImageURLs")) $("imagineImageURLs").value = "";
      renderEditFiles();
    });
    $("imagineStartBtn")?.addEventListener("click", () => start());
    $("imagineStopBtn")?.addEventListener("click", () => stop());
    $("imagineClearBtn")?.addEventListener("click", () => clearGrid());
    $("batchDownloadBtn")?.addEventListener("click", toggleSelectionMode);
    $("toggleSelectAllBtn")?.addEventListener("click", toggleSelectAll);
    $("downloadSelectedBtn")?.addEventListener("click", downloadSelected);
    $("exitSelectionBtn")?.addEventListener("click", exitSelection);
    $("imagineGrid")?.addEventListener("click", (event) => {
      const item = event.target.closest(".waterfall-item");
      if (!item || !item.classList.contains("is-ready")) return;
      toggleItem(item);
    });
  }

  function init(options = {}) {
    if (state.initialized) return;
    state.initialized = true;
    state.saveState = options.saveState || null;
    state.showToast = options.showToast || null;
    const ui = options.uiState || loadState();
    if ($("imagineRatio") && typeof ui.imagineRatio === "string" && ui.imagineRatio) $("imagineRatio").value = ui.imagineRatio;
    const models = qualityModels();
    const quality = normalizeQuality(ui.imagineQuality || (ui.imagineModel === models.quality ? "quality" : "basic"));
    setToggle("#imagineQualityToggle", "imagineQuality", quality);
    syncResolution(quality);
    setToggle("#imagineOperationToggle", "imagineOperation", ui.imagineOperation === "edit" ? "edit" : "generate");
    setToggle("#imagineRunModeToggle", "imagineRunMode", ui.imagineRunMode === "continuous" ? "continuous" : "single");
    bindEvents();
    if (window.GrokAvailability) availability = window.GrokAvailability;
    syncEditUI();
    syncRatioUI();
    resizePrompt();
    setButtons(false);
    applyAvailability();
    setStatus("空闲");
  }

  window.addEventListener("grok-availability", (event) => {
    availability = event?.detail && typeof event.detail === "object" ? event.detail : { webPlans: null, media: null };
    applyAvailability();
  });

  window.addEventListener("grok-models-loaded", (event) => {
    imageModels = (Array.isArray(event.detail) ? event.detail : []).filter((item) => Array.isArray(item?.capabilities) && item.capabilities.includes("image"));
  });

  window.GrokImagine = {
    init,
    ensureReady,
    start,
    stop,
  };
})();
