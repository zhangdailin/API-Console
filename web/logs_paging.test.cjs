const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

// A minimal DOM: enough of an element for the log centre's row and detail builders,
// with parent tracking so the placeholder row can be removed from the table body.
function element(tag='div'){
  const node={
    tagName:String(tag).toUpperCase(),
    children:[],textContent:'',className:'',hidden:false,dataset:{},parentNode:null,
    classList:{add(){},remove(){},toggle(){}},
    appendChild(child){child.parentNode=this;this.children.push(child);return child},
    replaceChildren(){this.children.forEach((c)=>{c.parentNode=null});this.children=[]},
    remove(){const parent=this.parentNode;if(!parent)return;const i=parent.children.indexOf(this);if(i>=0)parent.children.splice(i,1);this.parentNode=null},
    listeners:{},setAttribute(){},removeAttribute(){},addEventListener(name,fn){this.listeners[name]=fn},
    querySelectorAll(selector){
      const wanted=String(selector).replace(/\s+/g,'');
      const matches=(n)=>wanted==='tr'
        ? n.tagName==='TR'
        : wanted==='tr[data-index]'
          ? n.tagName==='TR'&&n.dataset.index!==undefined
          : false;
      const out=[];
      const walk=(n)=>{n.children.forEach((child)=>{if(matches(child))out.push(child);walk(child)})};
      walk(this);
      return out;
    },
  };
  return node;
}

// load() runs the page script against the stub DOM and returns the internals the
// assertions need. The startup block is replaced so no request is fired on load.
function load(){
  const ids={};
  const body=element('tbody');
  ids.logsRows=body;
  ids.logsDetailBody=element('div');
  ids.logsDetailTitle=element('div');
  ids.logsDetailSub=element('div');
  const context=vm.createContext({
    console,URLSearchParams,Promise,
    document:{
      readyState:'complete',
      createElement:(tag)=>element(tag),
      getElementById:(id)=>ids[id]||null,
      addEventListener(){},
      querySelectorAll:()=>[],
      body:element('body'),
    },
    window:{location:{search:''}},
    fetch:async()=>{throw new Error('fetch was not stubbed for this test')},
  });
  let src=fs.readFileSync(path.join(__dirname,'static/js/logs.js'),'utf8');
  src=src.replace(/  if \(document\.readyState === 'loading'\)[\s\S]*?\r?\n  \}\r?\n\}\)\(\);\s*$/,
    'globalThis.review={state,load,renderRows,renderDetail,filters,bind,scopeNote,renderPager};\n})();');
  vm.runInContext(src,context);
  return {...context.review,context,ids};
}

const allText=(node)=>[node.textContent||'',...node.children.map(allText)].join(' ');

const successResponse=(payload)=>({ok:true,status:200,json:async()=>payload});

test('a page replaces the "nothing matched" placeholder with its records',()=>{
  const api=load();
  const body=api.ids.logsRows;
  // The page that matched nothing left the placeholder row behind.
  api.state.records=[];
  api.renderRows();
  assert.equal(body.children.length,1,'an empty page states that nothing matched');
  // The next page returns exactly one record.
  api.state.records=[{event:{kind:'request',action:'grok_request',channel:'grok',model:'grok-4.6',status:'success',request_id:'req-1',timestamp:new Date().toISOString()}}];
  api.renderRows();
  const rows=body.querySelectorAll('tr[data-index]');
  assert.equal(rows.length,1,'the first record of the next page was never drawn');
  assert.equal(rows[0].dataset.index,'0');
  assert.equal(body.querySelectorAll('tr').length,1,'the "no matching records" placeholder must not stay in the list');
  assert.match(allText(rows[0]),/Grok 请求/);
});

test('pages walk the journal cursor forward and back to the same windows',async()=>{
  const api=load();
  ['logsPager','logsPageInfo','logsFirst','logsPrev','logsNext','logsPageNo'].forEach((id)=>{api.ids[id]=element(id==='logsPager'?'nav':'button');});
  const calls=[];
  // Three pages: cur-1 and cur-2 start pages 2 and 3; page 3 is the last.
  const pages={'':{next:'cur-1',action:'p1'},'cur-1':{next:'cur-2',action:'p2'},'cur-2':{next:'',action:'p3'}};
  api.context.fetch=async(url)=>{
    const params=new URL(url,'http://x').searchParams;
    calls.push({before:params.get('before')||'',limit:params.get('limit')});
    const page=pages[params.get('before')||''];
    return successResponse({data:[{event:{kind:'operation',action:page.action,status:'ok'}}],next_cursor:page.next});
  };
  api.state.kind='operation';
  await api.load(0);
  assert.equal(api.ids.logsPrev.disabled,true,'page 1 has no previous page');
  assert.equal(api.ids.logsNext.disabled,false);
  assert.equal(api.ids.logsPager.hidden,false);
  await api.load(api.state.page+1);
  await api.load(api.state.page+1);
  assert.equal(api.state.page,2);
  assert.equal(api.ids.logsNext.disabled,true,'the last page has no next page');
  assert.match(api.ids.logsPageInfo.textContent,/第 3 页.*已到最早记录/);
  await api.load(api.state.page-1);
  assert.equal(api.state.page,1);
  assert.match(allText(api.ids.logsRows),/p2/,'going back re-reads the same window');
  assert.deepEqual(calls.map((c)=>c.before),['','cur-1','cur-2','cur-1']);
  assert.ok(calls.every((c)=>c.limit==='50'),'the default page size is 50');
  // A changed scope restarts at the newest record.
  await api.load(0);
  assert.equal(calls.at(-1).before,'');
  assert.equal(api.ids.logsFirst.disabled,true);
});

test('a superseded response does not overwrite the newer tab',async()=>{
  const api=load();
  const pending=[];
  api.context.fetch=()=>{
    const entry={};
    entry.promise=new Promise((resolve)=>{entry.resolve=resolve});
    pending.push(entry);
    return entry.promise;
  };
  const requests=api.load(false);
  api.state.kind='operation';
  const operations=api.load(false);
  assert.equal(pending.length,2);
  // The newer request (操作日志) answers first, the older one (请求日志) lands after it.
  pending[1].resolve(successResponse({data:[{event:{kind:'operation',action:'config_update',status:'ok'}}],next_cursor:''}));
  await operations;
  pending[0].resolve(successResponse({data:[{event:{kind:'request',action:'grok_request',status:'success'}}],next_cursor:''}));
  await requests;
  assert.equal(api.state.records.length,1);
  assert.equal(api.state.records[0].event.kind,'operation','the stale answer replaced the list of the tab in force');
  assert.match(allText(api.ids.logsRows),/更新配置/);
});

test('detail panel reads the HTTP status and first-token latency from metadata',()=>{
  const api=load();
  api.renderDetail({
    outcome_class:'rate_limited',
    outcome_label:'限流 429/529',
    event:{
      kind:'request',action:'grok_request',channel:'grok',model:'grok-4.6',request_id:'req-429',
      status:'error',duration_ms:820,timestamp:new Date().toISOString(),
      metadata:{http_status:429,first_token_ms:135,path:'/v1/chat/completions'},
    },
  });
  const text=allText(api.ids.logsDetailBody);
  assert.match(text,/HTTP 429/,'the recorded HTTP status was not shown');
  assert.match(text,/首生成（含推理）[\s\S]*135 ms/,'the measured first-token latency was not shown');
  assert.match(text,/限流/);
});

test('obsolete log failures preserve current records and pagination cursor',async()=>{
  const api=load(), pending=[];
  api.context.fetch=()=>new Promise((resolve,reject)=>pending.push({resolve,reject}));
  const old=api.load(false); api.state.kind='operation'; const current=api.load(false);
  pending[1].resolve(successResponse({data:[{event:{kind:'operation',action:'config_update'}}],next_cursor:'current-cursor'})); await current;
  pending[0].reject(new Error('old failure')); await old;
  assert.equal(api.state.cursor,'current-cursor'); assert.equal(api.state.records[0].event.kind,'operation');
  assert.match(allText(api.ids.logsRows),/更新配置/); assert.doesNotMatch(allText(api.ids.logsRows),/old failure/);
});

test('quality filtering is removed without removing the diagnostic toggle',()=>{
 const html=fs.readFileSync(path.join(__dirname,'templates/pages/logs.html'),'utf8');
 assert.doesNotMatch(html,/filterQuality|logs-quality-filter/);
 assert.match(html,/logsDiagnosticsToggle/);
 const api=load();api.context.window.location.search='?quality=any';api.bind();
 assert.equal(api.filters().has('quality'),false);
});

test('request rows show the upstream route, cost and latency in the list', () => {
  const api = load();
  api.state.kind = 'request';
  api.state.records = [{ id: '1', outcome: 'success', event: {
    kind: 'request', action: 'chat_request', status: 'success', channel: 'cline', model: 'claude-sonnet-4.6',
    input_tokens: 3789, output_tokens: 1214, cost_in_usd_ticks: 197180000, duration_ms: 26810, client_ip: '161.118.140.32',
    metadata: { path: '/cline/v1/messages', upstream_endpoint: '/api/v1/chat/completions', upstream_model_sent: 'anthropic/claude-sonnet-4.6',
      upstream_model: 'grok-4.5-0309', first_token_ms: 1320, first_visible_token_ms: 1320 } } }];
  api.renderRows(false);
  const row = allText(api.ids.logsRows);
  for (const expected of ['claude-sonnet-4.6', '上游', 'grok-4.5-0309', '/cline/v1/messages', '/api/v1/chat/completions',
    '↓ 3,789', '↑ 1,214', '$0.019718', '1.32s', '26.81s', '47.6 tok/s', '161.118.140.32']) {
    assert.ok(row.includes(expected), 'row is missing ' + expected + ': ' + row);
  }
});

test('a vendor prefix or a dated snapshot of the requested model is not flagged as a substitution', () => {
  const api = load();
  api.state.kind = 'request';
  const event = (served) => ({ kind: 'request', action: 'chat_request', status: 'success', model: 'claude-sonnet-4.6', metadata: { upstream_model: served } });
  api.state.records = [{ id: '1', event: event('anthropic/claude-sonnet-4.6') }, { id: '2', event: event('claude-opus-4.6') },
    { id: '3', event: event('claude-sonnet-4.6-20260301') }, { id: '4', event: event('claude-sonnet-4.6-fast') }];
  api.renderRows(false);
  const classes = [];
  const walk = (n) => { if (/logs-model-upstream/.test(n.className || '')) classes.push(n.className); n.children.forEach(walk); };
  walk(api.ids.logsRows);
  assert.deepEqual(classes, ['logs-model-upstream', 'logs-model-upstream is-remapped', 'logs-model-upstream', 'logs-model-upstream is-remapped']);
});

test('the gateway provider and its retry count are shown under the model', () => {
  const api = load();
  api.state.kind = 'request';
  const ev = (meta) => ({ kind: 'request', action: 'chat_request', status: 'success', channel: 'cline', model: 'deepseek-v4.1-flash', metadata: meta });
  api.state.records = [
    { id: '1', event: ev({ upstream_model: 'deepseek-v4.1-flash', upstream_provider: 'alibaba', upstream_provider_attempts: '2' }) },
    { id: '2', event: ev({ upstream_provider: 'deepseek', upstream_provider_attempts: '1' }) },
    { id: '3', event: ev({ upstream_model: 'deepseek-v4.1-flash' }) },
  ];
  api.renderRows();
  const rows = api.ids.logsRows.querySelectorAll('tr[data-index]').map(allText);
  assert.match(rows[0], /服务商[\s\S]*alibaba · 第 2 次尝试/);
  assert.match(rows[1], /服务商[\s\S]*deepseek/);
  assert.doesNotMatch(rows[1], /次尝试/, 'a first-attempt success carries no retry note');
  assert.doesNotMatch(rows[2], /服务商/, 'no provider line without a gateway report');
  api.renderDetail(api.state.records[0]);
  const detail = allText(api.ids.logsDetailBody);
  assert.match(detail, /上游服务商[\s\S]*alibaba/);
  assert.match(detail, /服务商尝试[\s\S]*2 次/);
});

test('output speed is never divided by the last milliseconds of a reply', () => {
  const api = load();
  api.state.kind = 'request';
  const row = (meta, extra = {}) => ({ id: String(Math.random()), event: { kind: 'request', action: 'chat_request', status: 'success', channel: 'cline',
    model: 'm', output_tokens: 10240, duration_ms: 117616, ...extra, metadata: { path: '/cline/v1/chat/completions', ...meta } } });
  api.state.records = [
    // Non-streaming: the body arrives in one piece at the end.
    row({ first_token_kind: 'body_ttfb', first_token_ms: 117615, ttfb_ms: 117615 }),
    // A stream released all at once at the end.
    row({ first_token_ms: 117600, first_visible_token_ms: 117600 }),
    // A real stream: reasoning from 2 s, visible text from 30 s; the window
    // starts at the first generated token, reasoning included.
    row({ first_token_ms: 2000, first_visible_token_ms: 30000 }),
  ];
  api.renderRows();
  const rows = api.ids.logsRows.querySelectorAll('tr[data-index]').map(allText);
  assert.match(rows[0], /首字\s*非流式/);
  assert.match(rows[0], /87\.1 tok\/s（整体）/, rows[0]);
  assert.match(rows[1], /87\.1 tok\/s（整体）/, rows[1]);
  assert.match(rows[2], /首字\s*30\.00s/);
  assert.match(rows[2], /88\.6 tok\/s/);
  assert.doesNotMatch(rows[2], /整体/);
  for (const text of rows) assert.doesNotMatch(text, /\d{4,}\.\d tok\/s/, 'an output speed in the thousands');
});
