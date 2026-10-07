const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');
function element(tag='div'){
 return {tag,children:[],textContent:'',classList:{add(){},remove(){},toggle(){}},appendChild(n){n.parentNode=this;this.children.push(n);return n},replaceChildren(){this.children=[]},remove(){const parent=this.parentNode;if(parent)parent.children.splice(parent.children.indexOf(this),1);this.parentNode=null},listeners:{},addEventListener(event,fn){this.listeners[event]=fn},setAttribute(){}};
}
function load(fetch=async()=>{throw new Error('unexpected fetch')}){
 const blobs=new Map(),downloads=[],timers=[];
 class DownloadURL extends URL {
  static createObjectURL(blob){const id='blob:test-'+blobs.size;blobs.set(id,blob);return id}
  static revokeObjectURL(id){blobs.delete(id)}
 }
 const context=vm.createContext({console,URLSearchParams,URL:DownloadURL,Blob,fetch,setTimeout:(fn)=>timers.push(fn),document:{readyState:'loading',addEventListener(){},body:element('body')},window:{location:{}}});
 context.document.createElement=(tag)=>{
  const n=element(tag);
  n.click=()=>{assert.equal(n.parentNode,context.document.body,'download anchor must be attached');downloads.push({name:n.download,blob:blobs.get(n.href)})};
  return n;
 };
 let src=fs.readFileSync(path.join(__dirname,'static/js/logs.js'),'utf8');
 src=src.replace(/\}\)\(\);\s*$/,'globalThis.review={sectionLabel,renderBundle,renderDiagnosticsSection};})();');
 vm.runInContext(src,context);return {...context.review,context,downloads,timers,blobs};
}
const allText=n=>[n.textContent,...n.children.map(allText)].join(' ');
test('diagnostic attempt labels distinguish request, response, status and read failure',()=>{
 const api=load();
 for(const [name,label] of [['request.json','请求'],['response.txt','响应内容'],['result.json','HTTP 状态'],['read_error.json','响应读取错误']]){
  assert.equal(api.sectionLabel({name:'upstream_002_'+name}),'上游尝试 2 · '+label);
 }
 // The remaining numbered sections keep their own labels...
 assert.equal(api.sectionLabel({name:'3_upstream_request.json'}),'3 · 上游请求');
 assert.equal(api.sectionLabel({name:'5_client_sse.jsonl'}),'5 · 返回客户端 SSE');
 // ...and the upstream SSE capture that no longer exists has no stale label.
 assert.equal(api.sectionLabel({name:'4_upstream_sse.jsonl'}),'4_upstream_sse.jsonl');
});
test('bundle renders both attempts without hiding truncation or interpreting response markup',()=>{
 const api=load(),container=element();
 api.renderBundle(container,{bytes:200,duration_ms:31,truncated:true,sections:[
  {name:'upstream_001_request.json',payload:'first',bytes:5},
  {name:'upstream_001_read_error.json',payload:'read failed',bytes:11},
  {name:'upstream_002_response.txt',payload:'<script>example</script>',bytes:24,truncated:true},
 ]},'24 小时');
 assert.match(allText(container),/上游尝试 1/);assert.match(allText(container),/上游尝试 2/);
 assert.match(allText(container),/已截断/);assert.match(allText(container),/请求耗时 31 ms/);
 const details=container.children.filter(n=>n.tag==='details');assert.equal(details[1].open,true);
 assert.equal(details[2].children[1].textContent,''); details[2].open=true; details[2].listeners.toggle(); assert.equal(details[2].children[1].textContent,'<script>example</script>'); assert.match(allText(container),/下载完整诊断/);
});

test('latency diagnostics show zero, reused connections and upstream clock separately',()=>{
 const api=load(),container=element();
 api.renderBundle(container,{sections:[{name:'upstream_002_latency.json',payload:JSON.stringify({connection_reused:true,first_sse_ms:0,first_text_ms:838,upstream_firstTokenDuration:700,model_key:'qfmodel',httpdns_ip:''}),bytes:100}]},'');
 const text=allText(container);
 assert.match(text,/复用连接：是/);assert.match(text,/首条 SSE：0 ms/);assert.match(text,/首个正文：838 ms/);assert.match(text,/上游报告首 Token：700 ms/);assert.match(text,/实际模型路由：qfmodel/);assert.match(text,/HTTPDNS 请求头：未记录/);
});

const findButton=(node,label)=>node.children.find(n=>n.tag==='button'&&n.textContent===label);
test('download from the journal index fetches and saves the complete bundle',async()=>{
 let resolve;const calls=[];
 const api=load((url,options)=>{calls.push({url,options});return new Promise(r=>{resolve=r})});
 const panel=element(),entry={request_id:'req/a',sections:[{name:'response.txt',payload:'complete response'}]};
 api.renderDiagnosticsSection(panel,{event:{request_id:'req/a'},diagnostics:{metadata:{sections:['response.txt']}}});
 const button=findButton(panel.children[0],'下载完整诊断');
 const pending=button.listeners.click();
 assert.equal(button.disabled,true);assert.match(button.textContent,/正在/);
 assert.equal(calls[0].url,'/api/journal/diagnostics?request_id=req%2Fa');
 assert.equal(calls[0].options.credentials,'same-origin');
 resolve({ok:true,status:200,json:async()=>({available:true,entry})});await pending;
 assert.equal(button.disabled,false);assert.equal(api.downloads.length,1);
 assert.equal(api.downloads[0].blob.type,'application/json;charset=utf-8');
 assert.deepEqual(JSON.parse(await api.downloads[0].blob.text()),entry);
 assert.equal(api.context.document.body.children.length,0);
 assert.equal(api.blobs.size,1);api.timers.forEach(fn=>fn());assert.equal(api.blobs.size,0);
});

test('unavailable diagnostics and network failures show a message and allow retry',async()=>{
 let attempt=0;
 const api=load(async()=>{
  attempt++;if(attempt===1)throw new Error('network offline');
  return {ok:true,status:200,json:async()=>({available:false,note:'诊断已过期'})};
 });
 const panel=element();
 api.renderDiagnosticsSection(panel,{event:{request_id:'req-1'},diagnostics:{metadata:{}}});
 const container=panel.children[0],button=findButton(container,'下载完整诊断');
 await button.listeners.click();assert.match(allText(container),/下载诊断失败：network offline/);assert.equal(button.disabled,false);
 await button.listeners.click();assert.match(allText(container),/诊断已过期/);assert.equal(button.disabled,false);assert.equal(api.downloads.length,0);
});

test('loaded bundle and section downloads retain the entire payload',async()=>{
 const api=load(),container=element(),payload='x'.repeat(70000)+'<script>raw</script>';
 const entry={request_id:'req-2',sections:[{name:'response.txt',payload}]};
 api.renderBundle(container,entry,'');
 await findButton(container,'下载完整诊断').listeners.click();
 const details=container.children.find(n=>n.tag==='details');
 await findButton(details,'下载本段完整内容').listeners.click();
 assert.equal(api.downloads[0].name,'diagnostics-req-2.json');
 assert.deepEqual(JSON.parse(await api.downloads[0].blob.text()),entry);
 assert.equal(api.downloads[1].name,'response.txt');assert.equal(await api.downloads[1].blob.text(),payload);
});
