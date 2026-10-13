const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');
function element(tag='div'){
 return {tag,children:[],textContent:'',innerHTML:'',style:{},dataset:{},value:'',hidden:false,
 classList:{add(){},remove(){},toggle(){}},setAttribute(k,v){this[k]=v},addEventListener(){},
 appendChild(node){this.children.push(node);return node},replaceChildren(...nodes){this.children=nodes;this.textContent=''},
 querySelector(selector){if(selector==='.ops-kpi-head')return this.children[0];return null},querySelectorAll(){return []}};
}
function loadOps(){
 const nodes=new Map();const node=id=>{if(!nodes.has(id))nodes.set(id,element());return nodes.get(id)};
 const context=vm.createContext({console,Date,URLSearchParams,setInterval(){},window:{},document:{readyState:'loading',addEventListener(){},getElementById:node,createElement:element,createElementNS:(_,tag)=>element(tag),querySelectorAll:()=>[]}});
 let src=fs.readFileSync(path.join(__dirname,'static/js/ops.js'),'utf8');
 src=src.replace(/\}\)\(\);\s*$/,'globalThis.review={state,renderKpis,renderResources,liveBuckets,renderTrends,trendBuckets,windowRange};})();');vm.runInContext(src,context);
 return {api:context.review,node};
}
const text=node=>[node.textContent,...node.children.map(text)].join(' ');
function payload(series){return {available:true,until:'2026-09-13T08:30:30Z',window_minutes:180,totals:{requests:60,success:60},series};}
const current={minute:'2026-09-13T08:30:00Z',requests:60,input_tokens:600,output_tokens:300,usage_samples:60};
test('log drilldown keeps the rendered overview time boundaries',()=>{
 const {api}=loadOps();
 const p=payload([current]);p.since='2026-09-13T05:30:30Z';
 api.state.overview=p;
 const range=api.windowRange();
 assert.equal(range.since.getTime(),Date.parse(p.since));assert.equal(range.until.getTime(),Date.parse(p.until));
});



test('unavailable monitoring keeps error charts unknown across redraws',()=>{
 const {api,node}=loadOps();
 api.renderTrends({available:false});
 assert.match(text(node('opsThroughput')),/未采集/);
 assert.match(text(node('opsErrorTrend')),/未采集/);
});
test('P95-only historical data is never relabelled P99 or used for a failed-request cohort',()=>{
 const {api,node}=loadOps();const p=payload([]);p.totals={requests:1,success:1,duration_p95_ms:950,samples:1};
 api.renderKpis(p);assert.match(text(node('opsKpis')),/P95/);assert.doesNotMatch(text(node('opsKpis')),/P99/);
 api.state.outcome='failed';api.renderKpis(p);assert.doesNotMatch(text(node('opsKpis')),/950/);
});
test('resource cards retain host CPU, memory and RSS alongside Go metrics',()=>{
 const {api,node}=loadOps();api.renderResources({available:true,metrics:['主机 CPU','主机内存','进程内存 RSS'].map(label=>({label,value:'12%',available:true}))});
 assert.equal(node('opsResources').children.length,3);assert.match(text(node('opsResources')),/主机 CPU/);assert.match(text(node('opsResources')),/主机内存/);
});
function loadModels(){
 const list=element();const calls=[];
 const ctx=vm.createContext({console,document:{addEventListener(){},getElementById:id=>id==='modelsList'?list:null,querySelectorAll:()=>[]},window:{matchMedia:()=>({matches:false})},encodeData:encodeURIComponent,decodeData:decodeURIComponent,escapeHtml:String,showToast(){},confirm:()=>true,fetch:async(url,options)=>{calls.push({url,options});return {ok:true}}});
 vm.runInContext(fs.readFileSync(path.join(__dirname,'static/js/models.js'),'utf8'),ctx);
 vm.runInContext(`updateModelSummary=()=>{};renderModelRefreshSummary=()=>{};renderPagination=()=>{};updateRefreshButton=()=>{};loadModels=async()=>{};
 models=[{id:'cline-a',channel:'Cline',model_id:'one',status:'available'},{id:'grok-b',channel:'Grok',model_id:'two',status:'available'}];currentModelChannel='Cline';`,ctx);
 return {ctx,list,calls};
}
test('selected model remains checked after redraw and changing provider clears the selection',async()=>{
 const {ctx,list,calls}=loadModels();vm.runInContext(`modelsSelectedIds.add('cline-a');renderModels()`,ctx);
 assert.match(list.innerHTML,/data-action="row-select"[^>]+checked/);
 vm.runInContext(`filterModelsByChannel('Grok')`,ctx);assert.equal(vm.runInContext('modelsSelectedIds.size',ctx),0);
 await vm.runInContext(`runModelBatch('disable')`,ctx);assert.equal(calls.length,0);
});
test('batch action rejects stale cross-channel IDs even if selection state is corrupted',async()=>{
 const {ctx,calls}=loadModels();vm.runInContext(`currentModelChannel='Grok';modelsSelectedIds.add('cline-a');modelsSelectedIds.add('grok-b')`,ctx);
 await vm.runInContext(`runModelBatch('disable')`,ctx);assert.deepEqual(calls.map(c=>c.url),['/api/models/grok-b']);
});


test('rate-limit count excludes gateway rejection and exhausted quota',()=>{
 const {api,node}=loadOps();
 api.renderKpis({available:true,window_minutes:60,totals:{requests:3,failed:3,rate_limited:1,rejected:1,quota_exhausted:1,detailed_requests:3}});
 const card=node('opsKpis').children[5];
 const row=card.children[2].children.find(row=>row.children[0].textContent==='429 / 529 限流');
 assert.equal(row.children[1].textContent,'1');
});
test('throughput retains idle minutes and uses the server-aligned partial minute',()=>{
 const {api,node}=loadOps();
 const p=payload([{...current,minute:'2026-09-13T07:31:00Z'},current]);
 p.since='2026-09-13T07:30:30Z';p.window_minutes=60;
 api.state.overview=p;api.renderTrends(p);
 const buckets=api.trendBuckets();
 assert.equal(buckets.length,61);
 assert.equal(buckets[30].requests,0);
 assert.equal(buckets.at(-1).requests/buckets.at(-1).seconds,2);
 const line=node('opsThroughput').children[0].children.find(n=>n.tag==='path'&&n.class==='series-qps').d;
 assert.equal((line.match(/[ML]/g)||[]).length,61);
 assert.match(line,/170\.0/); // idle minute reaches the baseline
 assert.equal(node('opsErrorTrend').children[0].children.filter(n=>n.tag==='rect').length,61);
});
test('a single switch sample is visible without bridging unknown minutes',()=>{
 const {api,node}=loadOps();api.state.overview=payload([{...current,account_switch_count:1,account_switch_sum:2}]);api.renderTrends(api.state.overview);
 const svg=node('opsSwitchTrend').children[0];
 assert.equal(svg.tag,'svg');assert.ok(svg.children.some(n=>n.tag==='circle'));
 const line=svg.children.find(n=>n.tag==='path').d;
 assert.equal((line.match(/M/g)||[]).length,1);assert.doesNotMatch(line,/L/);
});
test('token coverage remains visible when only part of the traffic reported usage',()=>{
 const {api,node}=loadOps();api.renderKpis({available:true,window_minutes:60,totals:{requests:10,usage_samples:3,input_tokens:500}});
 assert.match(text(node('opsKpis').children[0]),/用量覆盖 3 \/ 10 次请求/);
});
