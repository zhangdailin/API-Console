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
    'globalThis.review={state,load,renderRows,renderDetail,filters,bind,scopeNote};\n})();');
  vm.runInContext(src,context);
  return {...context.review,context,ids};
}

const allText=(node)=>[node.textContent||'',...node.children.map(allText)].join(' ');

const successResponse=(payload)=>({ok:true,status:200,json:async()=>payload});

test('加载更多 draws the first record after an empty page',()=>{
  const api=load();
  const body=api.ids.logsRows;
  // The page that matched nothing left the placeholder row behind.
  api.state.records=[];
  api.renderRows(false);
  assert.equal(body.children.length,1,'an empty page states that nothing matched');
  // The next page returns exactly one record.
  api.state.records=[{event:{kind:'request',action:'grok_request',channel:'grok',model:'grok-4.6',status:'success',request_id:'req-1',timestamp:new Date().toISOString()}}];
  api.renderRows(true);
  const rows=body.querySelectorAll('tr[data-index]');
  assert.equal(rows.length,1,'the first record of the appended page was never drawn');
  assert.equal(rows[0].dataset.index,'0');
  assert.equal(body.querySelectorAll('tr').length,1,'the "no matching records" placeholder must not stay in the list');
  assert.match(allText(rows[0]),/Grok 请求/);
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

for(const [resolution,label,received] of [
  ['retried','换号后正常交付',false],
  ['delivered_degraded','已交付降级回答',true],
  ['rejected','已拒绝请求',false],
  ['undeliverable','无可交付内容',false],
]){
 test('quality results show '+resolution+' in both the list and detail',()=>{
  const api=load();
  const record={event:{kind:'request',action:'grok_request',channel:'grok',request_id:'quality-'+resolution,status:'success',metadata:{quality_resolution:resolution,quality_withheld_attempts:1,quality_reasons:['no_reasoning']}}};
  api.state.records=[record]; api.renderRows(false); api.renderDetail(record);
  assert.match(allText(api.ids.logsRows),new RegExp(label));
  const detail=allText(api.ids.logsDetailBody);
  assert.match(detail,/Grok 质量检查/);
  assert.match(detail,new RegExp(label));
  assert.match(detail,received?/客户端收到了被质量检查拦截的回答/:/客户端没有收到被拦截的回答/);
  assert.match(detail,/要求推理但没有任何推理内容/);
  assert.match(detail,/当前日志窗口未找到逐次拦截明细/);
 });
}

test('quality details include each detector, account penalty, counts and retry policy',()=>{
 const api=load();
 const reasons=['burst_dump','cipher_drool','fake_encrypted_dump','fast_reasoning_ratio_dump','no_reasoning'];
 const record={event:{kind:'request',action:'grok_request',channel:'grok',request_id:'quality-detail'},quality:[
  ...reasons.map((reason,index)=>({action:'grok_quality_degraded',attempt:index+1,account_id:10+index,reasoning_tokens:80,metadata:{reason,mode:index===0?'collected':'stream',guard:index===0?'disabled':'parked',visible_chars:8,reasoning_chars:100,encrypted_chars:256,first_visible_ms:150}})),
  {action:'grok_quality_resolution',status:'rejected',metadata:{withheld_attempts:5,stopped:'attempts_exhausted',max_attempts:6,on_exhausted:'fail_closed'}},
 ]};
 api.renderDetail(record);
 const detail=allText(api.ids.logsDetailBody);
 for(const label of ['长时间等待后一次性吐出短回答','只有密文推理、推理计费为零','密文或大额推理计费后瞬间吐出全文','推理占输出八成以上且瞬间吐出','要求推理但没有任何推理内容','账号 #10','非流式','凭据已停用','凭据已冷却','可见 8 字符','推理计费 80 token','首字 150 ms','已达质量重试次数上限','拒绝（返回 502）'])assert(detail.includes(label),label);
});

test('partial quality histories disclose missing attempts without inferring client delivery',()=>{
 const api=load();
 api.renderDetail({event:{kind:'request',action:'grok_request',metadata:{quality_withheld_attempts:3}},quality:[{action:'grok_quality_degraded',attempt:2,metadata:{reason:'future_detector'}}]});
 const detail=allText(api.ids.logsDetailBody);
 assert.match(detail,/客户端交付结果未记录/);
 assert.doesNotMatch(detail,/客户端没有收到被拦截的回答/);
 assert.match(detail,/当前显示 1 \/ 3 次拦截/);
 assert.match(detail,/future_detector/);
});

test('credential guard results do not claim a withheld answer was never delivered',()=>{
 const api=load();
 api.renderDetail({event:{kind:'system',action:'grok_quality_guard',status:'disabled',metadata:{failures:2,disable_threshold:2,account_enabled:false,withheld:true}}});
 const detail=allText(api.ids.logsDetailBody);
 assert.match(detail,/Grok 质量护栏/);
 assert.match(detail,/凭据已停用/);
 assert.match(detail,/2 \/ 2 次停用/);
 assert.match(detail,/交付前被暂存拦截/);
 assert.doesNotMatch(detail,/客户端未收到/);
 api.renderDetail({event:{kind:'system',action:'grok_quality_guard',status:'parked'}});
 assert.match(allText(api.ids.logsDetailBody),/未记录/);
});

test('the visible quality selector reloads immediately and cannot filter system logs',async()=>{
 const api=load(),calls=[];
 api.ids.filterQuality=element('select'); api.ids.filterQuality.value='';
 api.bind();
 api.context.fetch=async url=>{calls.push(url);return successResponse({data:[],next_cursor:''});};
 api.state.cursor='old-cursor';api.ids.filterQuality.value='delivered_degraded';
 await api.ids.filterQuality.listeners.change();
 await new Promise(resolve=>setImmediate(resolve));
 assert.equal(api.state.cursor,'');
 assert.match(calls[0],/quality=delivered_degraded/);
 assert.match(calls[0],/kind=request/);
 api.state.kind='system'; api.scopeNote();
 assert.equal(api.ids.filterQuality.disabled,true);
 await api.load(false);
 assert.doesNotMatch(calls[1],/quality=/);
 api.state.kind='request';api.scopeNote();
 assert.equal(api.ids.filterQuality.disabled,false);
 assert.equal(api.filters().get('quality'),'delivered_degraded');
});

test('the quality selector is outside the collapsed filters and has one identity',()=>{
 const html=fs.readFileSync(path.join(__dirname,'templates/pages/logs.html'),'utf8');
 assert.equal((html.match(/id="filterQuality"/g)||[]).length,1);
 assert(html.indexOf('id="filterQuality"')<html.indexOf('id="logsFilters"'));
});
