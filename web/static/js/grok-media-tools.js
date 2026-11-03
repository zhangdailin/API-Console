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
      const select = el('imagineModel');
      if (!select) return;
      const previous = select.value;
      while (select.options.length > 1) select.remove(1);
      for (const route of routes.filter((route) => capability(route, 'image'))) select.appendChild(new Option(route.id, route.id));
      if (Array.from(select.options).some((option) => option.value === previous)) select.value = previous;
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
    el('imagineModel')?.addEventListener('change', () => { el('imagineResolution').disabled = !el('imagineModel').value; });
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
      host.innerHTML = items.length ? items.map((item) => `<article class="admin-media-card"><label class="admin-media-select"><input type="checkbox" data-image-select="${escapeMediaHTML(item.name)}" ${mediaHistoryState.selectedImages.has(item.name) ? 'checked' : ''}> 选择</label><a href="${escapeMediaHTML(item.view_url)}" target="_blank" rel="noopener"><img src="${escapeMediaHTML(item.preview_url)}" loading="lazy" alt="${escapeMediaHTML(item.name)}"></a><strong title="${escapeMediaHTML(item.name)}">${escapeMediaHTML(item.name)}</strong><span class="meta-text">${formatMediaBytes(item.size_bytes)} · ${new Date(Number(item.updated_at)).toLocaleString()}</span><div class="media-card-actions"><a class="btn btn-outline" href="${escapeMediaHTML(item.view_url)}?download=1">下载</a><button class="btn btn-danger-outline" type="button" data-image-delete="${escapeMediaHTML(item.name)}">删除</button></div></article>`).join('') : '<div class="table-empty-cell">暂无图片</div>';
      mediaHistoryState.imagePage = Math.min(mediaHistoryState.imagePage, pages);
      document.getElementById('mediaImagePage').textContent = `${mediaHistoryState.imagePage} / ${pages}`; document.getElementById('mediaImagePrevBtn').disabled = mediaHistoryState.imagePage <= 1; document.getElementById('mediaImageNextBtn').disabled = mediaHistoryState.imagePage >= pages;
      document.getElementById('mediaImageStats').textContent = `${stats.count || 0} 张 · ${Number(stats.size_mb || 0).toFixed(2)} MB`;
    } catch (error) { host.innerHTML = `<div class="table-empty-cell">加载失败：${escapeMediaHTML(error.message)}</div>`; }
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
      body.innerHTML = items.length ? items.map((item) => { const terminal = ['completed', 'failed', 'cancelled', 'canceled'].includes(String(item.status).toLowerCase()); const media = item.content_url ? `<video class="admin-video-preview" src="${escapeMediaHTML(item.content_url)}" preload="metadata" controls></video><div class="media-card-actions"><a class="btn btn-outline" target="_blank" rel="noopener" href="${escapeMediaHTML(item.content_url)}">打开</a><a class="btn btn-outline" href="${escapeMediaHTML(item.download_url)}">下载</a></div>` : '-'; return `<tr><td><code>${escapeMediaHTML(item.id)}</code><br><span class="meta-text">${escapeMediaHTML(item.provider || '-')} · account ${escapeMediaHTML(item.account_id || '-')}</span></td><td><span class="tag">${escapeMediaHTML(item.status)}</span></td><td><strong>${escapeMediaHTML(item.model || '-')}</strong><br><span class="meta-text">${escapeMediaHTML(item.prompt || item.error_message || '-')}</span><br><span class="meta-text">${escapeMediaHTML(item.size || '-')} · ${escapeMediaHTML(item.quality || '-')} · ${Number(item.seconds || 0)}s</span></td><td>${media}</td><td>${Number(item.progress || 0)}%<br><span class="meta-text">${item.created_at ? new Date(Number(item.created_at) * 1000).toLocaleString() : '-'}</span></td><td>${terminal ? `<button class="btn btn-danger-outline" type="button" data-video-delete="${escapeMediaHTML(item.id)}">删除</button>` : '-'}</td></tr>`; }).join('') : '<tr><td colspan="6" class="table-empty-cell">暂无视频任务</td></tr>';
      mediaHistoryState.videoPage = Math.min(mediaHistoryState.videoPage, pages); document.getElementById('mediaVideoPage').textContent = `${mediaHistoryState.videoPage} / ${pages}`; document.getElementById('mediaVideoPrevBtn').disabled = mediaHistoryState.videoPage <= 1; document.getElementById('mediaVideoNextBtn').disabled = mediaHistoryState.videoPage >= pages;
      const statuses = stats.statuses || {}; document.getElementById('mediaVideoStats').textContent = `${stats.total || 0} 个任务` + Object.entries(statuses).map(([key, value]) => ` · ${key}: ${value}`).join('');
    } catch (error) { body.innerHTML = `<tr><td colspan="6" class="table-empty-cell">加载失败：${escapeMediaHTML(error.message)}</td></tr>`; }
  }

  function initAdminMediaHistory() {
    const gallery = document.getElementById('mediaImageGallery'); if (!gallery) return;
    document.getElementById('mediaImageRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage = 1; loadAdminImages(); }); document.getElementById('mediaVideoRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaImageSearch')?.addEventListener('input', debounceMedia(() => { mediaHistoryState.imagePage = 1; loadAdminImages(); })); document.getElementById('mediaVideoSearch')?.addEventListener('input', debounceMedia(() => { mediaHistoryState.videoPage = 1; loadAdminVideos(); }));
    ['mediaVideoStatus', 'mediaVideoSort'].forEach((id) => document.getElementById(id)?.addEventListener('change', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); })); document.getElementById('mediaImageSort')?.addEventListener('change', () => { mediaHistoryState.imagePage = 1; loadAdminImages(); });
    document.getElementById('mediaImagePageSize')?.addEventListener('change', (event) => { mediaHistoryState.imagePageSize = Number(event.target.value); mediaHistoryState.imagePage = 1; loadAdminImages(); }); document.getElementById('mediaVideoPageSize')?.addEventListener('change', (event) => { mediaHistoryState.videoPageSize = Number(event.target.value); mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaImageDeleteSelectedBtn')?.addEventListener('click', () => deleteAdminImages([...mediaHistoryState.selectedImages])); gallery.addEventListener('change', (event) => { const name = event.target?.dataset?.imageSelect; if (name) event.target.checked ? mediaHistoryState.selectedImages.add(name) : mediaHistoryState.selectedImages.delete(name); }); gallery.addEventListener('click', (event) => { const name = event.target?.dataset?.imageDelete; if (name) deleteAdminImages([name]); }); document.getElementById('mediaVideoHistory')?.addEventListener('click', (event) => { const id = event.target?.dataset?.videoDelete; if (id) deleteAdminVideo(id); });
    document.getElementById('mediaImagePrevBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage--; loadAdminImages(); }); document.getElementById('mediaImageNextBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage++; loadAdminImages(); }); document.getElementById('mediaVideoPrevBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage--; loadAdminVideos(); }); document.getElementById('mediaVideoNextBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage++; loadAdminVideos(); }); loadAdminImages(); loadAdminVideos();
  }

  if (typeof window !== 'undefined') window.GrokMediaTools = { normalizeVoices, jsonAudioSource, formatSeconds, loadAdminImages, loadAdminVideos };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => { init(); initAdminMediaHistory(); }); else { init(); initAdminMediaHistory(); }
})();
