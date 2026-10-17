// Explicitly simulated observations for local visual acceptance only.
function installFlowSimulation({theme='light'}={}) {
 localStorage.setItem('api-console-theme',theme);
 let mode='normal',paused=false,instance='preview-instance-1',tick=0;
 const streams=[],counters=new Map(),channels=['workbuddy','cline','qoder','grok'],protocols=['chat','responses','messages'];
 const sum=(rows,key)=>rows.reduce((n,row)=>n+(row[key]||0),0);
 function snapshot(scope='') {
  const all=channels.flatMap((channel,c)=>protocols.map((protocol,p)=>{
   const id=channel+':'+protocol,previous=counters.get(id)||0;
   const enabled=(c+p)%3!==1,active=mode==='idle'||!enabled?0:(c+1)+(p===1?2:0);
   const qps=mode==='idle'||!enabled?0:((c+1)*.65+(p+1)*.2)*(mode==='burst'?3:1);
   const started=previous+(qps>0?Math.max(1,Math.floor(qps)):0);counters.set(id,Math.max(started,active));
   const completed=Math.max(0,started-active);
   const failed=mode==='failed'&&channel==='cline'?Math.min(2,completed):0;
   return {channel,protocol,active,started:Math.max(started,active),completed,qps,input_tps:qps*240,output_tps:mode==='waiting'?0:qps*(16+c*3),usage_available:mode!=='waiting'||active===0,_failed:failed};
  }));
  const routes=all.filter(row=>!scope||row.channel===scope),nodes={};
  for(const key of [...channels,'gateway',...protocols]) {
   const rows=routes.filter(row=>key==='gateway'||row.channel===key||row.protocol===key);if(!rows.length)continue;
   const completed=sum(rows,'completed'),active=sum(rows,'active'),waiting=mode==='waiting'?active:rows.reduce((n,row)=>n+Math.floor(row.active*.4),0);
   const distribution=ms=>completed?{samples:Math.min(256,completed),p50_ms:Math.round(ms*.65),p95_ms:ms}:{samples:0,p50_ms:null,p95_ms:null};
   nodes[key]={completed,failed:sum(rows,'_failed'),failure_rate:completed?sum(rows,'_failed')/completed:null,waiting,responding:active-waiting,longest_active_ms:active?3200+tick*20:0,
    observed:completed,usage_reported:mode==='waiting'?0:completed,retried:mode==='failed'?Math.min(completed,2):0,attempts:completed+(mode==='failed'?2:0),account_switches:mode==='failed'?1:0,retry_wait_ms:mode==='failed'?2000:0,queue_wait_ms:mode==='failed'?500:0,
    rate_limited:mode==='failed'?1:0,stream_errors:mode==='failed'?1:0,timeouts:mode==='failed'?1:0,canceled:0,
    first_generation:distribution(1240),first_visible:distribution(1680),body_ttfb:{samples:0,p50_ms:null,p95_ms:null},duration:distribution(7420),http_phases:{dns_ms:{samples:completed,avg_ms:2},connection_wait_ms:{samples:completed,avg_ms:11},response_headers_ms:{samples:completed,avg_ms:240}}};
  }
  return {available:true,instance_started:instance,sampled_at:new Date().toISOString(),rate_seconds:5,window_seconds:60,latency_window_seconds:900,sample_limit_per_route:256,routes,nodes};
 }
 class SimulatedEventSource {
  constructor(url){this.url=url;this.listeners={};this.closed=false;streams.push(this);setTimeout(()=>this.deliver(),40);}
  addEventListener(type,fn){this.listeners[type]=fn;}
  close(){this.closed=true;}
  deliver(){if(this.closed||paused)return;const scope=new URL(this.url,location.href).searchParams.get('channel')||'';this.listeners.flow?.({data:JSON.stringify(snapshot(scope))});}
 }
 window.EventSource=SimulatedEventSource;
 function emit(){tick++;streams.filter(stream=>!stream.closed).forEach(stream=>stream.deliver());}
 function pools(scope='',empty=false,stale=false){return {available:true,sampled_at:new Date(Date.now()-(stale?31000:0)).toISOString(),capacity_scope:'account_pool',rows:channels.filter(key=>!scope||scope===key).map((channel,i)=>({channel,accounts_enabled:empty?0:12+i*2,accounts_available:empty?0:10+i,accounts_needing_login:empty?0:1,model_cooldowns:0,capacity_available:true,concurrency_available:true,capacity_slots:empty?0:80+i*20,occupied_slots:empty?0:8+i*7,active_requests:empty?0:6+i,capacity_sampled_at:new Date().toISOString()}))};}
 let autoTimer=setInterval(emit,200);
 window.__flowSim={streams,pools,emit,
  mode(value){mode=value;paused=false;emit();},
  pause(error=false){paused=true;if(error)streams.filter(stream=>!stream.closed).forEach(stream=>stream.onerror?.());},
  resume(){paused=false;emit();},
  restart(){instance='preview-instance-'+Date.now();counters.clear();paused=false;emit();},
  burst(){paused=false;for(let i=0;i<100;i++)emit();paused=true;},
  stop(){clearInterval(autoTimer);autoTimer=null;},
 };
}
module.exports={installFlowSimulation};
