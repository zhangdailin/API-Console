const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

// Just enough DOM for rows: class lists are real sets so the "new row" mark is visible.
function element(tag='div'){
  const classes=new Set();
  const node={
    tagName:String(tag).toUpperCase(),children:[],textContent:'',className:'',hidden:false,dataset:{},parentNode:null,disabled:false,
    classList:{add:(c)=>classes.add(c),remove:(c)=>classes.delete(c),toggle:(c,on)=>{if(on===undefined?!classes.has(c):on)classes.add(c);else classes.delete(c)},contains:(c)=>classes.has(c)},
    appendChild(child){child.parentNode=this;this.children.push(child);return child},
    replaceChildren(...nodes){this.children=[];nodes.forEach((n)=>this.appendChild(n))},
    setAttribute(){},removeAttribute(){},addEventListener(){},querySelectorAll:()=>[],
  };
  return node;
}

function setup(){
  const ids={};
  for(const id of ['logsRows','logsDetailBody','logsDetailTitle','logsDetailSub','logsPager','logsPageInfo','logsPageNo','logsFirst','logsPrev','logsNext','logsTableWrap']) ids[id]=element(id==='logsRows'?'tbody':'div');
  const timers=[], streams=[], fetches=[];
  class FakeEventSource{constructor(url){this.url=url;this.listeners={};streams.push(this)}addEventListener(n,f){this.listeners[n]=f}close(){this.closed=true}}
  const context=vm.createContext({
    console,URLSearchParams,Promise,Date,EventSource:FakeEventSource,
    document:{readyState:'complete',hidden:false,createElement:(t)=>element(t),getElementById:(id)=>ids[id]||null,addEventListener(){},querySelectorAll:()=>[],body:element('body')},
    window:{location:{search:''},setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length},setInterval(){},history:{replaceState(){}}},
    fetch:(url)=>{let resolve;const p=new Promise((r)=>{resolve=r});fetches.push({url:String(url),resolve});return p},
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname,'static/js/ui.js'),'utf8'),context);
  let src=fs.readFileSync(path.join(__dirname,'static/js/logs.js'),'utf8');
  src=src.replace(/  if \(document\.readyState === 'loading'\)[\s\S]*?\r?\n  \}\r?\n\}\)\(\);\s*$/,
    'globalThis.review={state,load,connectLive,live_};\n})();');
  vm.runInContext(src,context);
  return {...context.review,ids,timers,streams,fetches};
}

const record=(id)=>({event:{kind:'request',action:'grok_request',channel:'grok',model:'grok-4.6',status:'success',request_id:id,timestamp:'2026-11-07T08:00:0'+id.slice(-1)+'Z'}});
const reply=(data,next='')=>({ok:true,status:200,headers:{get:()=>'application/json'},json:async()=>({data,next_cursor:next}),text:async()=>JSON.stringify({data,next_cursor:next})});
const flow=(stream,completed)=>stream.listeners.flow({data:JSON.stringify({instance_started:'i',routes:[{completed}]})});
const settle=()=>new Promise((r)=>setImmediate(r));

test('a finished request makes new rows appear on page one without losing the open detail',async()=>{
  const api=setup();
  const first=api.load(0); api.fetches[0].resolve(reply([record('req-2'),record('req-1')],'c1')); await first; await settle();
  api.state.selected=1; // the operator opened req-1
  api.connectLive();
  const stream=api.streams[0];
  assert.match(stream.url,/\/api\/ops\/flow$/);
  flow(stream,5); // the first snapshot only sets the baseline
  assert.equal(api.timers.length,0);
  flow(stream,6);
  assert.equal(api.timers.length,1,'a finished request schedules a read');
  const live=api.timers.shift().fn(); await settle();
  api.fetches[1].resolve(reply([record('req-3'),record('req-2'),record('req-1')],'c1')); await live; await settle();
  const rows=api.ids.logsRows.children;
  assert.equal(rows.length,3);
  assert.equal(rows[0].classList.contains('is-new'),true,'the new row is marked');
  assert.equal(rows[1].classList.contains('is-new'),false);
  assert.equal(api.state.selected,2,'the open detail follows its row');
  assert.equal(rows[2].classList.contains('is-selected'),true);
});

test('later pages do not move under the reader',async()=>{
  const api=setup();
  const first=api.load(0); api.fetches[0].resolve(reply([record('req-2')],'c1')); await first; await settle();
  const second=api.load(1); api.fetches[1].resolve(reply([record('req-1')],'')); await second; await settle();
  api.connectLive();
  flow(api.streams[0],1); flow(api.streams[0],2);
  assert.equal(api.timers.length,0,'page two is not refreshed live');
});
