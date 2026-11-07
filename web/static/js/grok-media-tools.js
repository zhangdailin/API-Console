(() => {
  const CUSTOM_VOICE = '__custom__';
  const AUDIO_MIME = { mp3: 'audio/mpeg', wav: 'audio/wav', wave: 'audio/wav', opus: 'audio/ogg', ogg: 'audio/ogg', aac: 'audio/aac', flac: 'audio/flac', pcm: 'audio/L16' };

  function capability(route, name) {
    return Array.isArray(route?.capabilities) && route.capabilities.includes(name);
  }

  function normalizeVoices(payload) {
    const source = Array.isArray(payload) ? payload : (Array.isArray(payload?.voices) ? payload.voices : (Array.isArray(payload?.data) ? payload.data : []));
    return source.map((voice) => {
      if (typeof voice === 'string') return { id: voice, name: voice, language: '' };
      const id = String(voice?.voice_id || voice?.id || voice?.value || '').trim();
      return { id, name: String(voice?.name || voice?.display_name || id).trim(), language: String(voice?.language || voice?.locale || '').trim() };
    }).filter((voice) => voice.id);
  }

  function decodeBase64(value, mime) {
    const raw = String(value || '').replace(/^data:[^;,]+;base64,/, '').replace(/\s+/g, '');
    const bytes = Uint8Array.from(atob(raw), (character) => character.charCodeAt(0));
    return new Blob([bytes], { type: mime || 'audio/mpeg' });
  }

  function jsonAudioSource(payload, contentType) {
    const item = Array.isArray(payload?.data) ? payload.data[0] : null;
    const directURL = payload?.url || payload?.audio_url || item?.url;
    if (typeof directURL === 'string' && directURL.trim()) return { url: directURL.trim(), revoke: false };
    const encoded = payload?.audio || payload?.audio_base64 || payload?.b64_json || item?.audio || item?.audio_base64 || item?.b64_json;
    if (typeof encoded !== 'string' || !encoded.trim()) throw new Error('TTS JSON 响应中没有音频数据');
    const format = String(payload?.format || payload?.response_format || item?.format || '').toLowerCase();
    const mime = /^audio\//i.test(contentType || '') ? contentType.split(';')[0] : (AUDIO_MIME[format] || 'audio/mpeg');
    return { blob: decodeBase64(encoded, mime), revoke: true };
  }

  async function responseError(response) {
    const text = await response.text();
    try {
      const data = JSON.parse(text);
      return data?.error?.message || data?.message || text || `请求失败 (${response.status})`;
    } catch (_) {
      return text || `请求失败 (${response.status})`;
    }
  }

  function formatSeconds(value) {
    const number = Number(value);
    return Number.isFinite(number) ? `${number.toFixed(number < 10 ? 2 : 1)} 秒` : '';
  }

  function init() {
    const host = document.getElementById('voiceFileTools');
    if (!host || host.dataset.initialized === 'true') return;
    host.dataset.initialized = 'true';
    const el = (id) => document.getElementById(id);
    let routes = [], objectURL = '', busy = false, voiceRequest = 0;

    function setStatus(message, type) {
      const status = el('audioStatus');
      status.textContent = message || '';
      status.classList.toggle('is-error', type === 'error');
    }

    function clearObjectURL() {
      if (objectURL) URL.revokeObjectURL(objectURL);
      objectURL = '';
    }

    function selectedVoice() {
      return el('audioVoice').value === CUSTOM_VOICE ? el('audioCustomVoice').value.trim() : el('audioVoice').value;
    }

    function syncCustomVoice() {
      el('audioCustomVoiceBlock').classList.toggle('hidden', el('audioVoice').value !== CUSTOM_VOICE);
    }

    async function loadVoices() {
      const requestID = ++voiceRequest;
      const model = el('audioModel').value;
      const select = el('audioVoice');
      select.replaceChildren(new Option('加载音色…', ''));
      select.disabled = true;
      syncCustomVoice();
      if (!model || el('audioOperation').value !== 'tts') return;
      let payload, lastError;
      const prefix = window.GrokToolRequest?.prefix?.() || '/api/grok/tools/v1';
      for (const endpoint of [`${prefix}/tts/voices`, '/v1/tts/voices']) {
        try {
          const response = await fetch(`${endpoint}?model=${encodeURIComponent(model)}`, { headers: window.GrokToolRequest?.headers?.({ Accept: 'application/json' }) || { Accept: 'application/json' } });
          if (!response.ok) throw new Error(await responseError(response));
          payload = await response.json();
          break;
        } catch (error) { lastError = error; }
      }
      if (requestID !== voiceRequest) return;
      const voices = normalizeVoices(payload);
      select.replaceChildren();
      for (const voice of voices) {
        const option = new Option(`${voice.name}${voice.language ? ` · ${voice.language}` : ''}`, voice.id);
        select.appendChild(option);
      }
      select.appendChild(new Option('自定义 voice_id…', CUSTOM_VOICE));
      select.disabled = false;
      if (!voices.length) {
        select.value = CUSTOM_VOICE;
        setStatus(lastError ? `音色列表不可用：${lastError.message}` : '未返回预设音色，请输入 voice_id', lastError ? 'error' : '');
      } else if (el('voiceName')?.value && voices.some((voice) => voice.id === el('voiceName').value)) {
        select.value = el('voiceName').value;
      }
      syncCustomVoice();
    }

    function refresh() {
      const tts = el('audioOperation').value === 'tts';
      el('audioTextLabel').hidden = !tts;
      el('audioFileLabel').hidden = tts;
      el('audioVoiceBlock').hidden = !tts;
      el('audioCustomVoiceBlock').hidden = !tts || el('audioVoice').value !== CUSTOM_VOICE;
      const select = el('audioModel');
      const previous = select.value;
      select.replaceChildren();
      for (const route of routes.filter((route) => capability(route, tts ? 'tts' : 'stt'))) {
        select.appendChild(new Option(route.id, route.id));
      }
      if (Array.from(select.options).some((option) => option.value === previous)) select.value = previous;
      el('audioSubmit').disabled = busy || !select.value;
      if (!select.value) setStatus('没有配置支持此操作的模型'); else setStatus('');
      if (tts) loadVoices(); else ++voiceRequest;
    }

    function acceptRoutes(value) {
      routes = Array.isArray(value) ? value : [];
      refresh();
    }

    function renderTTS(source, response) {
      const result = el('audioResult');
      const audio = document.createElement('audio');
      audio.controls = true;
      audio.src = source.url;
      const link = document.createElement('a');
      link.className = 'btn btn-outline';
      link.href = source.url;
      const type = response.headers.get('Content-Type') || source.blob?.type || 'audio/mpeg';
      const extension = Object.entries(AUDIO_MIME).find(([, mime]) => type.startsWith(mime))?.[0] || 'mp3';
      link.download = `speech.${extension === 'wave' ? 'wav' : extension}`;
      link.textContent = '下载语音';
      result.append(audio, link);
    }

    function renderSTT(data) {
      const result = el('audioResult');
      const text = document.createElement('pre');
      text.className = 'voice-transcript';
      text.textContent = String(data?.text || '');
      result.appendChild(text);
      const metadata = document.createElement('div');
      metadata.className = 'voice-transcript-meta';
      if (data?.language) metadata.appendChild(Object.assign(document.createElement('span'), { textContent: `语言：${data.language}` }));
      if (data?.duration != null) metadata.appendChild(Object.assign(document.createElement('span'), { textContent: `时长：${formatSeconds(data.duration)}` }));
      if (metadata.children.length) result.appendChild(metadata);
      const words = Array.isArray(data?.words) ? data.words : [];
      if (!words.length) return;
      const table = document.createElement('table');
      table.className = 'voice-word-table';
      table.innerHTML = '<thead><tr><th>词语</th><th>开始</th><th>结束</th><th>说话人</th></tr></thead>';
      const body = document.createElement('tbody');
      for (const word of words) {
        const row = document.createElement('tr');
        const values = [word.word ?? word.text ?? '', formatSeconds(word.start), formatSeconds(word.end), word.speaker ?? '—'];
        for (const value of values) row.appendChild(Object.assign(document.createElement('td'), { textContent: String(value) }));
        body.appendChild(row);
      }
      table.appendChild(body);
      const wrap = document.createElement('div'); wrap.className = 'voice-word-table-wrap'; wrap.appendChild(table); result.appendChild(wrap);
    }

    window.addEventListener('grok-models-loaded', (event) => acceptRoutes(event.detail));
    const catalog = window.GrokModelCatalogPromise;
    if (catalog) catalog.then(acceptRoutes).catch((error) => setStatus(error.message, 'error'));
    el('audioOperation').addEventListener('change', refresh);
    el('audioModel').addEventListener('change', () => { if (el('audioOperation').value === 'tts') loadVoices(); });
    el('audioVoice').addEventListener('change', syncCustomVoice);
    el('audioSubmit').addEventListener('click', async () => {
      if (busy) return;
      const tts = el('audioOperation').value === 'tts';
      const model = el('audioModel').value;
      const language = el('audioLanguage').value.trim();
      busy = true; el('audioSubmit').disabled = true; setStatus('处理中…');
      try {
        let body, headers;
        if (tts) {
          const input = el('audioText').value.trim();
          if (!input) throw new Error('请输入文本');
          const voice = selectedVoice();
          if (!voice) throw new Error('请选择音色或输入 voice_id');
          body = JSON.stringify({ model, input, voice, voice_id: voice, language: language || undefined, speed: Number(el('voiceSpeed')?.value || 1), response_format: 'mp3' });
          headers = { 'Content-Type': 'application/json', Accept: 'audio/*, application/json' };
        } else {
          const file = el('audioFile').files[0];
          if (!file) throw new Error('请选择音频文件');
          body = new FormData(); body.set('model', model); body.set('file', file); body.set('response_format', 'verbose_json'); if (language) body.set('language', language);
        }
        const prefix = window.GrokToolRequest?.prefix?.() || '/api/grok/tools/v1';
        const requestHeaders = window.GrokToolRequest?.headers?.(headers || {}) || headers;
        const response = await fetch(`${prefix}/audio/${tts ? 'speech' : 'transcriptions'}`, { method: 'POST', headers: requestHeaders, body });
        if (!response.ok) throw new Error(await responseError(response));
        clearObjectURL(); el('audioResult').replaceChildren();
        if (tts) {
          const contentType = response.headers.get('Content-Type') || '';
          let source;
          if (/json/i.test(contentType)) source = jsonAudioSource(await response.json(), contentType);
          else { const blob = await response.blob(); if (!blob.size) throw new Error('TTS 返回了空音频'); source = { blob, revoke: true }; }
          if (source.blob) { objectURL = URL.createObjectURL(source.blob); source.url = objectURL; }
          renderTTS(source, response);
        } else {
          const contentType = response.headers.get('Content-Type') || '';
          renderSTT(/json/i.test(contentType) ? await response.json() : { text: await response.text() });
        }
        setStatus('完成');
      } catch (error) { setStatus(error?.message || String(error), 'error'); }
      finally { busy = false; el('audioSubmit').disabled = !el('audioModel').value; }
    });
  }

  const mediaHistoryState = { imagePage: 1, videoPage: 1, imagePageSize: 12, videoPageSize: 12, selectedImages: new Set() };

  function escapeMediaHTML(value) {
    return String(value ?? '').replace(/[&<>"']/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[character]);
  }
  function mediaPages(total, size) { return Math.max(1, Math.ceil(Number(total || 0) / size)); }
  function formatMediaBytes(value) { const bytes = Number(value || 0); return bytes < 1024 ? `${bytes} B` : bytes < 1048576 ? `${(bytes / 1024).toFixed(1)} KB` : `${(bytes / 1048576).toFixed(1)} MB`; }
  function debounceMedia(fn, wait = 300) { let timer; return (...args) => { clearTimeout(timer); timer = setTimeout(() => fn(...args), wait); }; }
  async function mediaJSON(url, options = {}) {
    const response = await fetch(url, { credentials: 'same-origin', ...options });
    if (!response.ok) throw new Error(await response.text());
    return response.json();
  }
  async function mediaPost(url, body) { return mediaJSON(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }

  async function deleteAdminImages(names) {
    if (!names.length || !window.confirm(`确认删除 ${names.length} 张图片？`)) return;
    await mediaPost('/api/grok/admin/media/images/delete', { names }); names.forEach((name) => mediaHistoryState.selectedImages.delete(name)); await loadAdminImages();
  }
  async function deleteAdminVideo(id) { if (!window.confirm(`确认删除终态任务 ${id}？`)) return; await mediaPost('/api/grok/admin/media/videos/delete', { id }); await loadAdminVideos(); }

  const MEDIA_ICON = {
    download: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/></svg>',
    trash: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M19 6l-1 14H6L5 6"/></svg>',
    open: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 3h7v7"/><path d="M10 14 21 3"/><path d="M21 14v7H3V3h7"/></svg>',
    film: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="2" y="5" width="15" height="14" rx="2"/><path d="m22 8-5 4 5 4z"/></svg>',
  };
  const VIDEO_STATUS = { completed: ['已完成', 'is-ok'], failed: ['失败', 'is-bad'], cancelled: ['已取消', 'is-idle'], canceled: ['已取消', 'is-idle'], queued: ['排队中', 'is-warn'], in_progress: ['生成中', 'is-info'] };
  function videoStatusLabel(status) { return (VIDEO_STATUS[String(status || '').toLowerCase()] || [String(status || '-')])[0]; }

  // mediaImageTile is one generated image: the picture fills the tile, the
  // selection box and the actions sit on it, the name and size below.
  function mediaImageTile(item) {
    const name = escapeMediaHTML(item.name), view = escapeMediaHTML(item.view_url), selected = mediaHistoryState.selectedImages.has(item.name);
    const time = item.updated_at ? new Date(Number(item.updated_at)).toLocaleString() : '-';
    return `<article class="gw-media-tile${selected ? ' is-selected' : ''}">
      <a class="gw-media-thumb" href="${view}" target="_blank" rel="noopener" title="${name}"><img src="${escapeMediaHTML(item.preview_url || item.view_url)}" loading="lazy" alt="${name}"></a>
      <label class="gw-media-check" title="选择"><input type="checkbox" data-image-select="${name}" ${selected ? 'checked' : ''}><span aria-hidden="true"></span></label>
      <div class="gw-media-actions"><a class="gw-media-action" href="${view}?download=1" title="下载" aria-label="下载">${MEDIA_ICON.download}</a><button class="gw-media-action is-danger" type="button" data-image-delete="${name}" title="删除" aria-label="删除">${MEDIA_ICON.trash}</button></div>
      <div class="gw-media-meta"><span class="gw-media-name" title="${name}">${name}</span><span class="gw-media-sub">${formatMediaBytes(item.size_bytes)} · ${escapeMediaHTML(time)}</span></div>
    </article>`;
  }

  // mediaVideoCard is one video task: the player (or its status) on top, the
  // prompt and metadata below.
  function mediaVideoCard(item) {
    const status = String(item.status || '').toLowerCase();
    const [label, tone] = VIDEO_STATUS[status] || [String(item.status || '-'), 'is-idle'];
    const terminal = ['completed', 'failed', 'cancelled', 'canceled'].includes(status);
    const frame = item.content_url
      ? `<video src="${escapeMediaHTML(item.content_url)}" preload="metadata" controls playsinline></video>`
      : `<div class="gw-video-placeholder ${tone}">${MEDIA_ICON.film}<span>${status === 'in_progress' ? `${Number(item.progress || 0)}%` : escapeMediaHTML(label)}</span></div>`;
    const created = item.created_at ? new Date(Number(item.created_at) * 1000).toLocaleString() : '-';
    const actions = [
      item.content_url ? `<a class="btn btn-outline btn-sm" target="_blank" rel="noopener" href="${escapeMediaHTML(item.content_url)}">${MEDIA_ICON.open}打开</a>` : '',
      item.download_url ? `<a class="btn btn-outline btn-sm" href="${escapeMediaHTML(item.download_url)}">${MEDIA_ICON.download}下载</a>` : '',
      terminal ? `<button class="btn btn-danger-outline btn-sm" type="button" data-video-delete="${escapeMediaHTML(item.id)}">删除</button>` : '',
    ].join('');
    return `<article class="gw-video-card">
      <div class="gw-video-frame">${frame}</div>
      <div class="gw-video-body">
        <div class="gw-video-top"><span class="gw-status ${tone}">${escapeMediaHTML(label)}</span><code title="${escapeMediaHTML(item.id)}">${escapeMediaHTML(item.id)}</code></div>
        <p class="gw-video-prompt" title="${escapeMediaHTML(item.prompt || item.error_message || '')}">${escapeMediaHTML(item.prompt || item.error_message || '无提示词')}</p>
        <div class="gw-video-meta"><span>${escapeMediaHTML(item.model || '-')}</span><span>${escapeMediaHTML(item.size || '-')}</span><span>${Number(item.seconds || 0)}s</span></div>
        <div class="gw-video-time">${escapeMediaHTML(created)}</div>
        ${actions ? `<div class="gw-video-actions">${actions}</div>` : ''}
      </div>
    </article>`;
  }

  async function loadAdminImages() {
    const host = document.getElementById('mediaImageGallery'); if (!host) return;
    const query = String(document.getElementById('mediaImageSearch')?.value || '').trim();
    const sort = String(document.getElementById('mediaImageSort')?.value || 'updated:desc').split(':');
    const params = new URLSearchParams({ page: String(mediaHistoryState.imagePage), page_size: String(mediaHistoryState.imagePageSize), sort: sort[0], order: sort[1] }); if (query) params.set('search', query);
    try {
      const [data, stats] = await Promise.all([mediaJSON(`/api/grok/admin/media/images?${params}`), mediaJSON('/api/grok/admin/media/images/stats')]);
      const items = Array.isArray(data.items) ? data.items : [];
      const pages = mediaPages(data.total, mediaHistoryState.imagePageSize);
      // Deleting the last card on the last page leaves the requested page out of
      // range: the server answers an empty list. Clamping only the label would
      // show "2 / 2" above an empty grid, so fetch the surviving page instead.
      if (items.length === 0 && data.total > 0 && mediaHistoryState.imagePage > pages) {
        mediaHistoryState.imagePage = pages;
        return loadAdminImages();
      }
      host.innerHTML = items.length ? items.map(mediaImageTile).join('') : '<div class="gw-empty">暂无图片</div>';
      mediaHistoryState.imagePage = Math.min(mediaHistoryState.imagePage, pages);
      document.getElementById('mediaImagePage').textContent = `${mediaHistoryState.imagePage} / ${pages}`; document.getElementById('mediaImagePrevBtn').disabled = mediaHistoryState.imagePage <= 1; document.getElementById('mediaImageNextBtn').disabled = mediaHistoryState.imagePage >= pages;
      document.getElementById('mediaImageStats').textContent = `${stats.count || 0} 张 · ${Number(stats.size_mb || 0).toFixed(2)} MB`;
    } catch (error) { host.innerHTML = `<div class="gw-empty is-error">加载失败：${escapeMediaHTML(error.message)}</div>`; }
  }

  async function loadAdminVideos() {
    const body = document.getElementById('mediaVideoHistory'); if (!body) return;
    const sort = String(document.getElementById('mediaVideoSort')?.value || 'updated:desc').split(':');
    const params = new URLSearchParams({ page: String(mediaHistoryState.videoPage), page_size: String(mediaHistoryState.videoPageSize), sort: sort[0], order: sort[1] });
    const search = String(document.getElementById('mediaVideoSearch')?.value || '').trim(), status = String(document.getElementById('mediaVideoStatus')?.value || '').trim(); if (search) params.set('search', search); if (status) params.set('status', status);
    try {
      const [data, stats] = await Promise.all([mediaJSON(`/api/grok/admin/media/videos?${params}`), mediaJSON('/api/grok/admin/media/videos/stats')]); const items = Array.isArray(data.items) ? data.items : [];
      const pages = mediaPages(data.total, mediaHistoryState.videoPageSize);
      // Same clamp-and-refetch rule as the image gallery: an out-of-range page
      // answers empty, which must not be rendered as "no tasks".
      if (items.length === 0 && data.total > 0 && mediaHistoryState.videoPage > pages) {
        mediaHistoryState.videoPage = pages;
        return loadAdminVideos();
      }
      body.innerHTML = items.length ? items.map(mediaVideoCard).join('') : '<div class="gw-empty">暂无视频任务</div>';
      mediaHistoryState.videoPage = Math.min(mediaHistoryState.videoPage, pages); document.getElementById('mediaVideoPage').textContent = `${mediaHistoryState.videoPage} / ${pages}`; document.getElementById('mediaVideoPrevBtn').disabled = mediaHistoryState.videoPage <= 1; document.getElementById('mediaVideoNextBtn').disabled = mediaHistoryState.videoPage >= pages;
      const statuses = stats.statuses || {}; document.getElementById('mediaVideoStats').textContent = `${stats.total || 0} 个任务` + Object.entries(statuses).map(([key, value]) => ` · ${videoStatusLabel(key)} ${value}`).join('');
    } catch (error) { body.innerHTML = `<div class="gw-empty is-error">加载失败：${escapeMediaHTML(error.message)}</div>`; }
  }

  function initAdminMediaHistory() {
    const gallery = document.getElementById('mediaImageGallery'); if (!gallery) return;
    document.getElementById('mediaImageRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage = 1; loadAdminImages(); }); document.getElementById('mediaVideoRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaImageSearch')?.addEventListener('input', debounceMedia(() => { mediaHistoryState.imagePage = 1; loadAdminImages(); })); document.getElementById('mediaVideoSearch')?.addEventListener('input', debounceMedia(() => { mediaHistoryState.videoPage = 1; loadAdminVideos(); }));
    ['mediaVideoStatus', 'mediaVideoSort'].forEach((id) => document.getElementById(id)?.addEventListener('change', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); })); document.getElementById('mediaImageSort')?.addEventListener('change', () => { mediaHistoryState.imagePage = 1; loadAdminImages(); });
    document.getElementById('mediaImagePageSize')?.addEventListener('change', (event) => { mediaHistoryState.imagePageSize = Number(event.target.value); mediaHistoryState.imagePage = 1; loadAdminImages(); }); document.getElementById('mediaVideoPageSize')?.addEventListener('change', (event) => { mediaHistoryState.videoPageSize = Number(event.target.value); mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaImageDeleteSelectedBtn')?.addEventListener('click', () => deleteAdminImages([...mediaHistoryState.selectedImages])); gallery.addEventListener('change', (event) => { const name = event.target?.dataset?.imageSelect; if (!name) return; if (event.target.checked) mediaHistoryState.selectedImages.add(name); else mediaHistoryState.selectedImages.delete(name); event.target.closest('.gw-media-tile')?.classList.toggle('is-selected', event.target.checked); }); gallery.addEventListener('click', (event) => { const name = event.target?.closest?.('[data-image-delete]')?.dataset.imageDelete; if (name) deleteAdminImages([name]); }); document.getElementById('mediaVideoHistory')?.addEventListener('click', (event) => { const id = event.target?.closest?.('[data-video-delete]')?.dataset.videoDelete; if (id) deleteAdminVideo(id); });
    document.getElementById('mediaImagePrevBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage--; loadAdminImages(); }); document.getElementById('mediaImageNextBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage++; loadAdminImages(); }); document.getElementById('mediaVideoPrevBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage--; loadAdminVideos(); }); document.getElementById('mediaVideoNextBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage++; loadAdminVideos(); }); loadAdminImages(); loadAdminVideos();
  }

  if (typeof window !== 'undefined') window.GrokMediaTools = { normalizeVoices, jsonAudioSource, formatSeconds, loadAdminImages, loadAdminVideos, mediaImageTile, mediaVideoCard };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => { init(); initAdminMediaHistory(); }); else { init(); initAdminMediaHistory(); }
})();
