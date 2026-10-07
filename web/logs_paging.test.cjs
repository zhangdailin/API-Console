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
    setAttribute(){},removeAttribute(){},addEventListener(){},
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
      body:element('body'),
    },
    window:{location:{search:''}},
    fetch:async()=>{throw new Error('fetch was not stubbed for this test')},
  });
  let src=fs.readFileSync(path.join(__dirname,'static/js/logs.js'),'utf8');
  src=src.replace(/  if \(document\.readyState === 'loading'\)[\s\S]*?\r?\n  \}\r?\n\}\)\(\);\s*$/,
    'globalThis.review={state,load,renderRows,renderDetail};\n})();');
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
