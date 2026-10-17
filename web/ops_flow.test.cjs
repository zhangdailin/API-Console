const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const modelSource=fs.readFileSync(__dirname+'/static/js/ops-flow-model.js','utf8');
const controllerSource=fs.readFileSync(__dirname+'/static/js/ops-flow.js','utf8');
function model(){const context=vm.createContext({});vm.runInContext(modelSource,context);return context.FlowScreenModel;}
const start=Date.parse('2026-10-17T06:00:00Z');
function snapshot(at=start, overrides={}) {
 const routes=['workbuddy','cline','qoder','grok'].flatMap(channel=>['chat','responses','messages'].map(protocol=>({channel,protocol,active:0,started:0,completed:0,qps:0,input_tps:0,output_tps:0,usage_available:true})));
 routes[4]={...routes[4],active:2,started:12,completed:10,qps:.4,input_tps:40,output_tps:10};
 const empty={samples:0,p50_ms:null,p95_ms:null};
 const stats={completed:10,failed:2,failure_rate:.2,waiting:1,responding:1,observed:10,usage_reported:8,retried:2,attempts:12,account_switches:1,retry_wait_ms:2000,queue_wait_ms:1000,longest_active_ms:3000,rate_limited:1,timeouts:0,stream_errors:1,canceled:0,
  first_generation:{samples:8,p50_ms:100,p95_ms:1200},first_visible:empty,body_ttfb:empty,duration:{samples:10,p50_ms:2000,p95_ms:4000},http_phases:{dns_ms:{samples:2,avg_ms:0}}};
 return {available:true,instance_started:'one',sampled_at:new Date(at).toISOString(),routes,nodes:Object.fromEntries(['gateway','workbuddy','cline','qoder','grok','chat','responses','messages'].map(key=>[key,{...stats}])),...overrides};
}
function pools(at=start,scope='') { return {available:true,sampled_at:new Date(at).toISOString(),rows:['workbuddy','cline','qoder','grok'].filter(key=>!scope||scope===key).map(channel=>({channel,accounts_available:2,accounts_enabled:3,capacity_available:true,capacity_slots:20,occupied_slots:5,model_cooldowns:1}))}; }

test('flow scope and totals count admitted routes once, and separate output from input tokens',()=>{
 const M=model(),data=snapshot();
 const all=M.normalize(data,''),cline=M.normalize(data,'cline');
 assert.equal(all.routes.length,12);assert.equal(cline.routes.length,3);
 assert.equal(M.rate(M.rowsFor(all,'gateway')).active,2);
 assert.equal(M.rate(M.rowsFor(all,'gateway')).qps,.4);
 assert.equal(M.rate(M.rowsFor(all,'gateway')).output,10);
 assert.equal(M.rate(M.rowsFor(cline,'responses')).output,10);
 data.routes[4].usage_available=false;
 assert.equal(M.rate(M.rowsFor(M.normalize(data,''),'gateway')).output,null);
 assert.equal(M.number(0),'0');assert.equal(M.percent(null),'暂无样本');assert.equal(M.milliseconds(0),'0 ms');
});
test('invalid, duplicate and empty snapshots cannot look like idle traffic',()=>{
 const M=model(),data=snapshot();
 assert.equal(M.normalize({...data,available:false},''),null);
 assert.equal(M.normalize({...data,routes:[]},''),null);
 assert.equal(M.normalize({...data,routes:[data.routes[0],data.routes[0]]},''),null);
 assert.equal(M.normalize({...data,sampled_at:'bad'},''),null);
 assert.equal(M.normalize({...data,instance_started:''},''),null);
 assert.equal(M.normalize({...data,routes:data.routes.slice(1)},''),null);
 assert.equal(M.normalize({...data,nodes:null},''),null);
 assert.equal(M.normalize({...data,routes:[{...data.routes[0],active:null}]},''),null);
 assert.equal(M.normalize({...data,routes:[{...data.routes[0],channel:'unknown'}]},''),null);
});
test('incoming and completion pulses use positive counter deltas and reset across instances',()=>{
 const M=model(),previous=snapshot(),next=snapshot(start+1000);next.routes[4].started+=3;next.routes[4].completed+=1;
 const diff=M.deltas(previous,next);
 assert.equal(diff.get('cline').incoming,3);assert.equal(diff.get('responses').outgoing,1);assert.equal(diff.get('gateway').incoming,3);assert.equal(diff.get('grok').incoming,0);
 assert.equal(M.deltas(previous,{...next,instance_started:'two'}).get('gateway').incoming,0);
 next.routes[4].started=0;assert.equal(M.deltas(previous,next).get('cline').incoming,0);
});
test('pool observations expire, stay scoped and never invent capacity',()=>{
 const M=model(),data=pools();
 const cline=M.pool(data,'cline','',start+1000,start);assert.equal(cline.available,2);assert.equal(cline.occupied,5);
 assert.equal(M.pool(data,'gateway','',start+1000,start).capacity,80);
 assert.equal(M.pool(pools(start,'cline'),'gateway','cline',start+1000,start).capacity,20);
 assert.equal(M.pool(data,'cline','',start+30000,start).fresh,false);
 assert.equal(M.pool({...data,rows:[]},'gateway','',start,start).fresh,false);
 data.rows[1].capacity_available=false;assert.equal(M.pool(data,'cline','',start,start).capacity,null);assert.equal(M.pool(data,'cline','',start,start).available,2);
});
test('trend has a bounded 60-second buffer and leaves outage gaps disconnected',()=>{
 const M=model(),trend=new M.Trend();trend.add(start,{qps:0,active:2});trend.add(start+3000,{qps:4,active:1});
 const values=trend.values(start+3000);assert.equal(values.length,60);assert.equal(values[57],null);assert.equal(values[58],null);
 assert.equal((trend.path('qps',start+3000).match(/M/g)||[]).length,2);
 trend.add(start+3000,{qps:9,active:3});assert.equal(trend.values(start+3000).at(-1).qps,9);
 for(let i=4;i<100;i++)trend.add(start+i*1000,{qps:i,active:1});
 assert.equal(trend.points.size,60);assert.equal(trend.values(start+99000).length,60);
 trend.clear();assert.equal(trend.path('qps',start+99000).trim(),'');
});

function harness({fetchPools,search=''}={}) {
 let clock=start,timerID=0,focus=null;const timers=new Map(),nodes=new Map(),streams=[],windowEvents={},documentEvents={},requests=[];
 function element(id='',tag='div') {
  const classes=new Set();let text='';
  const node={id,tagName:tag.toUpperCase(),children:[],dataset:{},value:'',hidden:false,disabled:false,open:false,attributes:{},
   style:{setProperty(key,value){this[key]=value;}},classList:{add:key=>classes.add(key),remove:key=>classes.delete(key),contains:key=>classes.has(key),toggle:(key,on)=>on?classes.add(key):classes.delete(key)},listeners:{},
   addEventListener(type,fn){(this.listeners[type]||=[]).push(fn);},setAttribute(key,value){this.attributes[key]=String(value);},getAttribute(key){return this.attributes[key];},
   appendChild(child){this.children.push(child);return child;},focus(){focus=this;},
   get textContent(){return text+this.children.map(child=>child.textContent).join(' ');},set textContent(value){text=String(value);this.children=[];},
   get options(){return this.children.filter(child=>child.tagName==='OPTION');},
   querySelector(selector){if(selector==='span')return this.span||(this.span=element());if(selector==='details')return this.details||(this.details=element());if(selector==='.flow-stat-failed')return get('failed-stat');return null;},
   querySelectorAll(selector){if(selector==='path')return this.paths||(this.paths=[element(),element(),element(),element()]);if(selector==='[data-flow-node]')return [...keys.map(key=>get('opsNode-'+key))];if(selector==='[data-flow-tab]')return [get('flowTabOverview'),get('flowTabDetails')];return [];},
   getBoundingClientRect(){return{left:0,right:100,top:0,bottom:100,width:100,height:100};},
   showModal(){this.open=true;},close(){this.open=false;this.emit('close',{});},
   emit(type,event={}){for(const fn of this.listeners[type]||[])fn(event);},dispatchEvent(event){this.emit(event.type,event);return true;},
  };
  if(id)nodes.set(id,node);return node;
 }
 const keys=['workbuddy','cline','qoder','grok','gateway','chat','responses','messages'];
 const get=id=>nodes.get(id)||element(id);
 for(const key of keys)get('opsNode-'+key).dataset.flowNode=key;
 get('flowTabOverview').dataset.flowTab='overview';get('flowTabDetails').dataset.flowTab='details';
 const root=get('opsFlowScreen');
 function timeout(fn,delay=0,repeat=0){const id=++timerID;timers.set(id,{fn,time:clock+delay,repeat});return id;}
 function advance(ms){const end=clock+ms;let steps=0;while(true){const next=[...timers].filter(([,t])=>t.time<=end).sort((a,b)=>a[1].time-b[1].time)[0];if(!next)break;if(++steps>10000)throw Error('timer loop');clock=next[1].time;timers.delete(next[0]);if(next[1].repeat)timers.set(next[0],{...next[1],time:clock+next[1].repeat});next[1].fn();}clock=end;}
 class Clock extends Date{static now(){return clock;}}
 const document={readyState:'complete',hidden:false,body:{style:{}},fullscreenElement:null,getElementById:get,createElement:tag=>element('',tag),addEventListener(type,fn){(documentEvents[type]||=[]).push(fn);}};
 const window={location:{search},setTimeout:(fn,delay)=>timeout(fn,delay),clearTimeout:id=>timers.delete(id),setInterval:(fn,delay)=>timeout(fn,delay,delay),clearInterval:id=>timers.delete(id),requestAnimationFrame:fn=>timeout(fn,16),cancelAnimationFrame:id=>timers.delete(id),addEventListener(type,fn){(windowEvents[type]||=[]).push(fn);},matchMedia:()=>({matches:false,addEventListener(){}}),scrollTo(){},scrollY:0};
 const context=vm.createContext({console,Date:Clock,AbortController,URLSearchParams,Event:class{constructor(type){this.type=type;}},document,window,
  ConsoleUI:{el:get,setText:(id,text)=>{get(id).textContent=text;},make:(tag,css,text='')=>{const n=element('',tag);n.textContent=text;return n;}},
  ConsoleAPI:{json:async(url,options)=>{requests.push({url,options});return fetchPools?fetchPools(url,options):pools(clock,new URL(url,'http://local').searchParams.get('channel')||'');}},
  EventSource:class{constructor(url){this.url=url;this.listeners={};this.closed=false;streams.push(this);}addEventListener(type,fn){this.listeners[type]=fn;}close(){this.closed=true;}},
 });
 vm.runInContext(modelSource,context);vm.runInContext(controllerSource,context);
 const send=data=>streams.at(-1).listeners.flow({data:JSON.stringify(data)});
 async function settle(){for(let i=0;i<8;i++)await Promise.resolve();advance(220);}
 return{context,document,window,root,get,streams,timers,requests,advance,send,settle,time:()=>clock,focus:()=>focus,
  hide(){document.hidden=true;(documentEvents.visibilitychange||[]).forEach(fn=>fn());},show(){document.hidden=false;(documentEvents.visibilitychange||[]).forEach(fn=>fn());},pagehide(){(windowEvents.pagehide||[]).forEach(fn=>fn());}};
}

test('bookmarked scope applies to the first connection, pool request and both selectors',async()=>{
 const h=harness({search:'?tab=ops&channel=cline'});await h.settle();
 assert.equal(h.streams.length,1);assert.equal(h.streams[0].url,'/api/ops/flow?channel=cline');
 assert.equal(h.requests[0].url,'/api/ops/pools?channel=cline');
 assert.equal(h.get('opsChannel').value,'cline');assert.equal(h.get('flowChannel').value,'cline');
});
test('screen retains last observations on outage, stops animation, and expires snapshots without an error event',async()=>{
 const h=harness();h.send(snapshot(h.time()));await h.settle();
 assert.equal(h.get('flowTotal-active').textContent,'2');assert.equal(h.get('flowTotal-tps').textContent,'10.0');
 assert.equal(h.root.dataset.live,'true');assert.equal(h.get('opsNode-cline').classList.contains('is-flowing'),true);
 h.advance(5100);assert.equal(h.root.dataset.live,'false');assert.match(h.get('opsFlowStatus').textContent,/过期/);assert.equal(h.get('flowTotal-active').textContent,'2');assert.equal(h.get('opsWire-cline').classList.contains('is-flowing'),false);
 h.send(snapshot(h.time()));await h.settle();assert.equal(h.root.dataset.live,'true');
 h.streams.at(-1).onerror();await h.settle();assert.match(h.get('opsFlowStatus').textContent,/重连/);assert.equal(h.get('flowTotal-active').textContent,'2');assert.equal(h.get('opsWire-cline').classList.contains('has-out'),false);
});
test('scope changes replace a single SSE connection, ignore stale pool replies and reset trends',async()=>{
 const pending=[];const h=harness({fetchPools:(url,options)=>new Promise(resolve=>pending.push({url,options,resolve}))});
 h.send(snapshot(h.time()));await h.settle();
 const old=h.streams[0];h.get('flowChannel').value='grok';h.get('flowChannel').emit('change');
 assert(old.closed);assert.equal(h.streams.filter(stream=>!stream.closed).length,1);assert.equal(h.streams.at(-1).url,'/api/ops/flow?channel=grok');assert.equal(h.get('opsChannel').value,'grok');
 assert(pending[0].options.signal.aborted);pending[0].resolve(pools(h.time()));pending[1].resolve(pools(h.time(),'grok'));
 h.send(snapshot(h.time()));await h.settle();
 assert.equal(h.get('flowTotal-active').textContent,'0');assert.equal(h.get('opsNode-cline').disabled,true);assert.equal(h.get('flowPool-grok').textContent,'2/3 可用账号');
 old.listeners.flow({data:JSON.stringify(snapshot(h.time()))});await h.settle();assert.equal(h.get('flowTotal-active').textContent,'0');
});
test('background and page lifecycle stop timers and polling and reconnect once on return',async()=>{
 const h=harness();h.send(snapshot(h.time()));await h.settle();h.hide();await Promise.resolve();await Promise.resolve();
 assert.equal(h.streams.filter(stream=>!stream.closed).length,0);assert.equal(h.timers.size,0);
 const calls=h.requests.length;h.advance(60000);assert.equal(h.requests.length,calls);
 h.show();h.show();await h.settle();assert.equal(h.streams.filter(stream=>!stream.closed).length,1);assert.equal(h.requests.length,calls+1);
 h.pagehide();await Promise.resolve();assert.equal(h.streams.filter(stream=>!stream.closed).length,0);
});
test('high-frequency snapshots coalesce and drawer details preserve distinct latency cohorts',async()=>{
 const h=harness();for(let i=0;i<100;i++)h.send(snapshot(h.time(),{sampled_at:new Date(h.time()).toISOString()}));await h.settle();
 assert.equal(h.streams.length,1);assert(h.timers.size<=4);assert.equal(h.get('opsFlowDetailFacts').children.length,8);
 h.get('opsNode-cline').emit('click');assert.equal(h.get('flowDrawer').open,true);assert.match(h.get('flowDetailedFacts').textContent,/P50 100 ms \/ P95 1.2 s · 8 样本/);assert.match(h.get('flowDetailedFacts').textContent,/非流式正文 TTFB.*暂无样本/);
 assert.match(h.get('flowDetailedFacts').textContent,/DNS 0 ms/);
 const next=snapshot(h.time());next.nodes.cline.first_generation={samples:0,p50_ms:null,p95_ms:null};next.nodes.cline.body_ttfb={samples:1,p50_ms:0,p95_ms:0};h.send(next);await h.settle();
 assert.match(h.get('flowDetailedFacts').textContent,/P50 0 ms \/ P95 0 ms/);
 h.get('flowDrawerClose').emit('click');assert.equal(h.get('flowDrawer').open,false);assert.equal(h.focus(),h.get('opsNode-cline'));
});
test('pool refresh failures do not close the flow stream and capacity becomes unknown after 30 seconds',async()=>{
 let fail=false;const h=harness({fetchPools:async()=>{if(fail)throw Error('unavailable');return pools(start);}});
 h.send(snapshot(h.time()));await h.settle();assert.match(h.get('flowCapacity-cline').textContent,/5\/20/);
 fail=true;h.advance(15000);await h.settle();assert.equal(h.streams.filter(stream=>!stream.closed).length,1);assert.match(h.get('flowCapacity-cline').textContent,/5\/20/);
 h.advance(16000);h.send(snapshot(h.time()));await h.settle();assert.equal(h.get('flowCapacity-cline').textContent,'槽位 —');assert.match(h.get('flowPool-cline').textContent,/读取失败/);assert.equal(h.root.dataset.live,'true');
});
