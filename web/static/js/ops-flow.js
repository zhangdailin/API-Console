// The screen consumes bounded snapshots. Animation is aggregate activity, never
// a reconstruction of individual requests or a per-token generation meter.
(() => {
  const M = FlowScreenModel;
  const {el,make,setText}=ConsoleUI;
  let root, source=null, snapshot=null, pools=null, lastReceived=0, poolReceived=0;
  let connected=false, message='正在连接实时流量…', selected='', detailTab='overview';
  let currentScope='', motion=true, reduced=false, poolError=false, nextPoolAt=0;
  let tickTimer=null, pulseTimer=null, paintTimer=null, frame=null, layoutFrame=null, lastPaint=0;
  let poolController=null, poolSequence=0, poolInFlight=false, resizeObserver=null;
  let fallbackFullscreen=false, priorOverflow='', priorScroll=0, fullscreenTrigger=null;
  let opener=null;
  const pulses=new Map(), trend=new M.Trend(), overviewFacts=new Map(), detailedFacts=new Map();
  const raf=callback=>window.requestAnimationFrame?window.requestAnimationFrame(callback):window.setTimeout(callback,16);
  const caf=id=>window.cancelAnimationFrame?window.cancelAnimationFrame(id):window.clearTimeout(id);
  const now=()=>Date.now();
  function live(){return connected && snapshot!=null && now()-lastReceived<5000 && !document.hidden;}
  function rows(key){return M.rowsFor(snapshot,key);}
  function stats(key){return snapshot?.nodes?.[key]||null;}
  function pool(key){return M.pool(pools,key,currentScope,now(),poolReceived);}
  function outside(key){return M.channels.includes(key) && currentScope && currentScope!==key;}
  function clock(value){const date=new Date(value);return Number.isFinite(date.getTime())?date.toLocaleTimeString('zh-CN',{hour12:false}):'—';}
  function field(map,key,value){const entry=map.get(key);if(entry && entry.value.textContent!==String(value))entry.value.textContent=value;}
  function bar(id,ratio){el(id)?.style.setProperty('width',(ratio==null?0:Math.max(0,Math.min(1,ratio))*100)+'%');}
  function statusText(){if(document.hidden)return '页面在后台，实时采集已暂停';if(!connected)return message;if(now()-lastReceived>=5000)return '数据已过期，等待新快照';return '实时推送已连接';}

  function schedule(){
    if(document.hidden || frame!=null || paintTimer!=null)return;
    const wait=Math.max(0,200-(now()-lastPaint));
    if(wait>0){paintTimer=window.setTimeout(()=>{paintTimer=null;schedule();},wait);return;}
    frame=raf(()=>{frame=null;lastPaint=now();render();});
  }
  function pulse(key,delta){
    const previous=pulses.get(key)||{incoming:0,outgoing:0};
    if(delta.incoming>0)previous.incoming=now()+1100;
    if(delta.outgoing>0)previous.outgoing=now()+1300;
    pulses.set(key,previous);
  }
  function sample(){
    const rate=M.rate(rows('gateway'));
    trend.add(now(),live()?{qps:rate.qps,active:rate.active}:null);
  }
  function accept(payload){
    const next=M.normalize(payload,currentScope);
    if(!next){connected=false;message='实时数据无效，等待恢复';pulses.clear();sample();schedule();return;}
    if(snapshot?.instance_started===next.instance_started && Date.parse(next.sampled_at)<Date.parse(snapshot.sampled_at))return;
    if(snapshot && snapshot.instance_started!==next.instance_started){trend.clear();pulses.clear();}
    const delta=M.deltas(snapshot,next);
    snapshot=next;lastReceived=now();connected=true;
    for(const [key,value] of delta)pulse(key,value);
    sample();schedule();
  }
  function related(){
    if(!selected || selected==='gateway')return new Set(M.keys);
    const result=new Set([selected,'gateway']);
    for(const row of rows(selected)) if(row.started>0 || row.active>0){result.add(row.channel);result.add(row.protocol);}
    return result;
  }
  function render(){
    if(!root)return;
    const fresh=live(), rate=M.rate(rows('gateway')), total=stats('gateway');
    root.dataset.live=String(fresh);root.dataset.motion=String(motion);root.dataset.reducedMotion=String(reduced);
    setText('opsFlowStatus',statusText());
    setText('flowUpdated',snapshot?'最近观测 '+clock(snapshot.sampled_at):'尚未收到数据');
    setText('flowScope',currentScope?M.names[currentScope]:'全部渠道');
    const totals={qps:snapshot?M.number(rate.qps,2):'—',active:snapshot?M.number(rate.active):'—',waiting:M.number(total?.waiting),responding:M.number(total?.responding),failed:M.number(total?.failed),tps:snapshot?(rate.output==null?'待上报':M.number(rate.output,1)):'—'};
    for(const [key,value]of Object.entries(totals))setText('flowTotal-'+key,value);
    root.querySelector('.flow-stat-failed')?.classList.toggle('is-degraded',(total?.failed||0)>0);
    setText('flowGatewayActive',totals.active);setText('flowGatewayWaiting',totals.waiting);setText('flowGatewayResponding',totals.responding);
    setText('opsFlowActive',snapshot?(fresh?'进行中请求':'上次观测 · 进行中'):'等待实时数据');
    const highlight=related();let nextPulse=Infinity;
    for(const key of M.keys){
      const node=el('opsNode-'+key),wire=el('opsWire-'+key),rates=M.rate(rows(key)),metric=stats(key),out=!!outside(key);
      const value=pulses.get(key)||{};
      const incoming=fresh&&!out&&value.incoming>now(), outgoing=fresh&&!out&&(value.outgoing>now()||(metric?.responding||0)>0);
      const active=fresh&&!out&&rates.active>0;
      if(value.incoming>now())nextPulse=Math.min(nextPulse,value.incoming);
      if(value.outgoing>now())nextPulse=Math.min(nextPulse,value.outgoing);
      node?.classList.toggle('is-flowing',active||incoming||outgoing);
      node?.classList.toggle('is-selected',selected===key);
      node?.classList.toggle('is-outside',out);
      node?.setAttribute('aria-pressed',String(selected===key));
      if(node)node.disabled=out;
      if(wire){
        wire.classList.toggle('is-flowing',active||incoming||outgoing);
        wire.classList.toggle('has-in',incoming);wire.classList.toggle('has-out',outgoing);
        wire.classList.toggle('is-selected',!!selected&&highlight.has(key));
        wire.classList.toggle('is-outside',out||(!!selected&&!highlight.has(key)));
        wire.style.setProperty('--flow-speed',Math.max(.4,1.4/(1+Math.log2(rates.qps+1)/3)).toFixed(2)+'s');
      }
      if(key==='gateway')continue;
      setText('opsNodeQps-'+key,snapshot&&!out?M.number(rates.qps,2):'—');
      setText('flowActive-'+key,snapshot&&!out?M.number(rates.active):'—');
      if(M.channels.includes(key)){
        setText('opsNodeTps-'+key,snapshot&&!out?(rates.output==null?'待上报':M.number(rates.output,1)):'—');
        const capacity=pool(key);
        setText('flowPool-'+key,out?'当前筛选范围外':capacity.fresh?`${M.number(capacity.available)}/${M.number(capacity.enabled)} 可用账号`:(poolError?'账号池读取失败':'账号池未就绪或已过期'));
        setText('flowCapacity-'+key,!out&&capacity.capacityKnown?`槽位 ${M.number(capacity.occupied)}/${M.number(capacity.capacity)}`:'槽位 —');
        bar('flowCapacityBar-'+key,!out&&capacity.capacityKnown&&capacity.capacity>0?capacity.occupied/capacity.capacity:null);
        const state=out?'范围外':!fresh?(snapshot?'上次数据':'等待数据'):rates.active>0?'有流量':capacity.fresh&&capacity.enabled===0?'未启用':capacity.fresh&&capacity.available===0?'池受限':metric?.failed>0?'近期失败':'空闲';
        setText('flowState-'+key,state);
        el('flowState-'+key)?.classList.toggle('is-warning',state==='池受限'||state==='近期失败');
        if(el('flowPool-'+key))el('flowPool-'+key).title=poolError?'刷新失败，保留上次采样；30秒后过期':capacity.fresh?'账号池采样 '+clock(capacity.sampledAt)+'；未扣除模型级限制':'';
      }else{
        const share=snapshot&&rate.qps>0?rates.qps/rate.qps:null;
        setText('flowShare-'+key,share==null?(snapshot?'暂无流量':'—'):M.percent(share));
        bar('flowShareBar-'+key,share);
      }
    }
    if(pulseTimer!=null)window.clearTimeout(pulseTimer);
    pulseTimer=fresh&&Number.isFinite(nextPulse)?window.setTimeout(()=>{pulseTimer=null;schedule();},Math.max(1,nextPulse-now()+1)):null;
    renderTrends();renderIncidents(total);renderDetail();
  }
  function renderTrends(){
    for(const [id,key]of [['flowTrendQps','qps'],['flowTrendActive','active']])el(id)?.setAttribute('d',trend.path(key,now()));
    const points=trend.values(now()).filter(Boolean), last=points.at(-1);
    setText('flowTrendQpsValue',last?M.number(last.qps,2)+' req/s':'—');setText('flowTrendActiveValue',last?M.number(last.active)+' 个':'—');
    setText('flowTrendNote',points.length?'已采样 '+points.length+'/60 秒 · 缺失区间留空':'等待实时采样');
  }
  function renderIncidents(metric){
    for(const key of ['rate_limited','timeouts','stream_errors','canceled','retried','account_switches']){
      setText('flowIncident-'+key,M.number(metric?.[key]));
      el('flowIncident-'+key)?.classList.toggle('is-degraded',(metric?.[key]||0)>0);
    }
    setText('flowQualityNote',!metric?'尚未收到数据':metric.completed===0?'近60秒暂无结束请求样本':`${M.number(metric.completed)} 个请求结束 · 失败率 ${M.percent(metric.failure_rate)}${live()?'':' · 上次观测'}`);
  }
  const overviewLabels={rate:'请求速率 · 近5秒',active:'请求状态 · 当前',usage:'上报用量速率 · 近5秒',total:'实例累计',failure:'失败记录 · 近60秒',accounts:'账号池',capacity:'共享账号池槽位',routes:'关联流量'};
  const detailLabels={generation:'首生成延迟 · 近15分钟',visible:'可见首字延迟 · 近15分钟',body:'非流式正文 TTFB · 近15分钟',duration:'请求总时长 · 近15分钟',retry:'重试与切号 · 近60秒',wait:'平均等待 · 近60秒',longest:'最长进行时长 · 当前',coverage:'用量上报覆盖 · 近60秒',phases:'HTTP 阶段均值 · 近60秒'};
  function facts(container,labels,target){
    if(!container)return;
    for(const [key,label]of Object.entries(labels)){
      const card=make('div','flow-fact'),title=make('span','',label),value=make('strong','','—');
      card.appendChild(title);card.appendChild(value);container.appendChild(card);target.set(key,{card,value});
    }
  }
  function distribution(value){return value?.samples>0&&M.numeric(value.p50_ms)!=null&&M.numeric(value.p95_ms)!=null?`P50 ${M.milliseconds(value.p50_ms)} / P95 ${M.milliseconds(value.p95_ms)} · ${M.number(value.samples)} 样本`:'暂无样本';}
  function renderDetail(){
    if(!selected)return;
    const out=outside(selected), metric=out?null:stats(selected), data=out?[]:rows(selected),rate=M.rate(data),capacity=pool(selected);
    setText('opsFlowDetailTitle',M.names[selected]);
    setText('flowDetailScope',`${currentScope?M.names[currentScope]:'全部渠道'} · 当前实例 · ${out?'当前筛选范围外':live()?'实时观测':snapshot?'上次观测，数据已过期':'等待数据'}${snapshot?' · '+clock(snapshot.sampled_at):''}`);
    const known=snapshot&&!out;
    field(overviewFacts,'rate',known?M.number(rate.qps,2)+' req/s':'—');
    field(overviewFacts,'active',known?`${M.number(rate.active)} 进行中 · ${M.number(metric?.waiting)} 等待输出 · ${M.number(metric?.responding)} 已开始输出`:'—');
    field(overviewFacts,'usage',known&&rate.output!=null?`输入 ${data.every(row=>M.numeric(row.input_tps)!=null)?M.number(M.sum(data,'input_tps'),1):'待上报'} / 输出 ${M.number(rate.output,1)} token/s；上游用量上报，不是逐字生成速度`:'待上报');
    field(overviewFacts,'total',known?`接收 ${M.number(M.sum(data,'started'))} · 结束 ${M.number(M.sum(data,'completed'))}`:'—');
    field(overviewFacts,'failure',metric?`${M.number(metric.failed)}/${M.number(metric.completed)} 失败 · ${M.percent(metric.failure_rate)} · 限流 ${M.number(metric.rate_limited)} · 超时 ${M.number(metric.timeouts)} · 流中断 ${M.number(metric.stream_errors)} · 取消 ${M.number(metric.canceled)}`:'暂无样本');
    for(const key of ['accounts','capacity'])if(overviewFacts.get(key))overviewFacts.get(key).card.hidden=M.protocols.includes(selected);
    field(overviewFacts,'accounts',!out&&capacity.fresh?`${M.number(capacity.available)}/${M.number(capacity.enabled)} 可用账号 · ${M.number(capacity.modelCooldowns)} 个模型限制 · ${clock(capacity.sampledAt)} 采样${poolError?' · 刷新失败，保留上次采样':''}`:'账号池未就绪或已过期');
    field(overviewFacts,'capacity',!out&&capacity.capacityKnown?`${M.number(capacity.occupied)}/${M.number(capacity.capacity)} 可用池槽位占用 · 可能包含其他实例占用，未扣模型级限制`:'容量未采集或已过期');
    const other=M.channels.includes(selected)?M.protocols:M.channels;
    field(overviewFacts,'routes',known?other.map(key=>`${M.names[key]} ${M.number(M.sum(data.filter(row=>row.channel===key||row.protocol===key),'active'))} 进行中`).join(' / '):'—');
    for(const [key,metricKey]of [['generation','first_generation'],['visible','first_visible'],['body','body_ttfb'],['duration','duration']])field(detailedFacts,key,distribution(metric?.[metricKey]));
    field(detailedFacts,'retry',metric?.observed>0?`${M.number(metric.retried)} 个请求重试 · ${M.number(metric.attempts)} 次尝试 · ${M.number(metric.account_switches)} 次切号`:'未采集');
    field(detailedFacts,'wait',metric?.observed>0?`重试 ${M.milliseconds(metric.retry_wait_ms/metric.observed)} · 其中排队 ${M.milliseconds(metric.queue_wait_ms/metric.observed)} · ${M.number(metric.observed)} 请求`:'未采集');
    field(detailedFacts,'longest',known&&rate.active>0?M.milliseconds(metric?.longest_active_ms):'无进行中请求');
    field(detailedFacts,'coverage',metric?`${M.number(metric.usage_reported)}/${M.number(metric.completed)} 个已结束请求`:'未采集');
    const labels={dns_ms:'DNS',tcp_ms:'TCP',tls_ms:'TLS',connection_wait_ms:'取连接',response_headers_ms:'响应头'};
    field(detailedFacts,'phases',Object.entries(labels).flatMap(([key,label])=>metric?.http_phases?.[key]?.samples>0?[label+' '+M.milliseconds(metric.http_phases[key].avg_ms)+'（'+M.number(metric.http_phases[key].samples)+'次）']:[]).join(' · ')||'未采集');
  }
  function tab(value){
    detailTab=value;
    const overview=value==='overview';
    el('flowOverviewPanel').hidden=!overview;el('flowDetailsPanel').hidden=overview;
    for(const [id,on]of [['flowTabOverview',overview],['flowTabDetails',!overview]]){
      el(id).setAttribute('aria-selected',String(on));el(id).tabIndex=on?0:-1;
    }
  }
  function openDetail(key,trigger,details=false){
    selected=key;opener=trigger;tab(details?'details':detailTab);renderDetail();schedule();
    const drawer=el('flowDrawer');
    if(!drawer.open){drawer.showModal();el('flowDrawerClose').focus();}
    if(details){const definitions=drawer.querySelector('details');if(definitions)definitions.open=true;}
  }
  function closeDetail(){el('flowDrawer').close();}
  function layout(){
    if(!root || document.hidden)return;
    const board=el('flowBoard'),svg=el('flowWires');if(!board||!svg)return;
    const rect=board.getBoundingClientRect(),gateway=el('opsNode-gateway')?.getBoundingClientRect();
    if(!gateway||!rect.width||!rect.height)return;
    svg.setAttribute('viewBox',`0 0 ${rect.width} ${rect.height}`);
    for(const key of [...M.channels,...M.protocols]){
      const node=el('opsNode-'+key)?.getBoundingClientRect();if(!node)continue;
      const left=M.channels.includes(key),x1=(left?node.right:gateway.right)-rect.left,x2=(left?gateway.left:node.left)-rect.left;
      const y1=(left?node.top+node.height/2:gateway.top+gateway.height/2)-rect.top,y2=(left?gateway.top+gateway.height/2:node.top+node.height/2)-rect.top;
      const bend=Math.max(12,(x2-x1)*.5),path=`M ${x1} ${y1} C ${x1+bend} ${y1}, ${x2-bend} ${y2}, ${x2} ${y2}`;
      el('opsWire-'+key)?.querySelectorAll('path').forEach(item=>item.setAttribute('d',path));
    }
  }
  function scheduleLayout(){if(document.hidden)return;if(layoutFrame==null)layoutFrame=raf(()=>{layoutFrame=null;layout();});}
  async function loadPools(){
    if(document.hidden||poolInFlight)return;
    const id=++poolSequence, scope=currentScope,controller=new AbortController();poolController=controller;poolInFlight=true;nextPoolAt=now()+15000;
    const timeout=window.setTimeout(()=>controller.abort(),6000);
    try{
      const result=await ConsoleAPI.json('/api/ops/pools'+(scope?'?channel='+encodeURIComponent(scope):''),{signal:controller.signal,cache:'no-store'});
      if(id!==poolSequence||document.hidden||scope!==currentScope)return;
      const expected=scope?[scope]:M.channels;
      if(result?.available!==true||!Number.isFinite(Date.parse(result.sampled_at))||!Array.isArray(result.rows)||result.rows.length!==expected.length||expected.some(key=>result.rows.filter(row=>row.channel===key).length!==1))throw new Error('invalid pool snapshot');
      pools=result;poolReceived=now();poolError=false;
    }catch(error){if(id===poolSequence&&!document.hidden){poolError=true;}}
    finally{window.clearTimeout(timeout);if(id===poolSequence){poolController=null;poolInFlight=false;schedule();}}
  }
  function abortPools(){poolSequence++;poolController?.abort();poolController=null;poolInFlight=false;}
  function disconnect(){
    if(source)source.close();source=null;connected=false;
    if(pulseTimer!=null)window.clearTimeout(pulseTimer);pulseTimer=null;pulses.clear();
  }
  function connect(){
    disconnect();message='正在连接实时流量…';schedule();
    if(document.hidden)return;
    if(typeof EventSource==='undefined'){message='浏览器不支持实时推送';schedule();return;}
    const stream=new EventSource('/api/ops/flow'+(currentScope?'?channel='+encodeURIComponent(currentScope):''));source=stream;
    stream.addEventListener('flow',event=>{if(source!==stream)return;try{accept(JSON.parse(event.data));}catch(_){connected=false;message='实时数据解析失败，等待恢复';pulses.clear();schedule();}});
    stream.onerror=()=>{if(source!==stream)return;connected=false;message='实时连接中断，自动重连中';pulses.clear();sample();schedule();};
  }
  function changeScope(value){
    const scope=M.channels.includes(value)?value:'';
    if(scope===currentScope)return;
    currentScope=scope;snapshot=null;pools=null;lastReceived=0;poolReceived=0;poolError=false;trend.clear();pulses.clear();
    el('flowChannel').value=scope;abortPools();connect();loadPools();
    if(selected&&outside(selected)){selected='';el('flowDrawer').open&&closeDetail();}
  }
  function heartbeat(){
    sample();schedule();if(now()>=nextPoolAt&&!poolInFlight)loadPools();
  }
  function stop(){
    disconnect();abortPools();resizeObserver?.disconnect();if(tickTimer!=null)window.clearInterval(tickTimer);tickTimer=null;
    if(paintTimer!=null)window.clearTimeout(paintTimer);paintTimer=null;
    if(frame!=null)caf(frame);frame=null;
    if(layoutFrame!=null)caf(layoutFrame);layoutFrame=null;
  }
  function start(){
    if(document.hidden||tickTimer!=null)return;
    if(resizeObserver){resizeObserver.observe(el('flowBoard'));root.querySelectorAll('[data-flow-node]').forEach(node=>resizeObserver.observe(node));}
    connect();loadPools();tickTimer=window.setInterval(heartbeat,1000);scheduleLayout();
  }
  function fullscreenState(){
    const expanded=document.fullscreenElement===root||fallbackFullscreen;
    el('flowFullscreen').querySelector('span').textContent=expanded?'退出全屏':'全屏';
    el('flowFullscreen').setAttribute('aria-label',expanded?'退出拓扑全屏':'全屏查看实时拓扑');
    scheduleLayout();
    if(!expanded){window.scrollTo?.(0,priorScroll);fullscreenTrigger?.focus({preventScroll:true});}
  }
  async function fullscreen(){
    if(fallbackFullscreen){root.classList.remove('is-maximized');document.body.style.overflow=priorOverflow;fallbackFullscreen=false;fullscreenState();return;}
    if(document.fullscreenElement===root){await document.exitFullscreen();return;}
    priorScroll=window.scrollY;fullscreenTrigger=el('flowFullscreen');
    if(root.requestFullscreen){try{await root.requestFullscreen();return;}catch(_){/* Use in-page expansion when fullscreen permission is unavailable. */}}
    priorOverflow=document.body.style.overflow;document.body.style.overflow='hidden';root.classList.add('is-maximized');fallbackFullscreen=true;fullscreenState();
  }
  function init(){
    root=el('opsFlowScreen');if(!root)return;
    const outer=el('opsChannel');
    if(outer)for(const key of M.channels)if(!Array.from(outer.options).some(option=>option.value===key)){const option=make('option','',M.names[key]);option.value=key;outer.appendChild(option);}
    const bookmarked=new URLSearchParams(window.location?.search||'').get('channel');
    currentScope=M.channels.includes(bookmarked)?bookmarked:M.channels.includes(outer?.value)?outer.value:'';
    if(outer)outer.value=currentScope;
    el('flowChannel').value=currentScope;
    facts(el('opsFlowDetailFacts'),overviewLabels,overviewFacts);facts(el('flowDetailedFacts'),detailLabels,detailedFacts);
    root.querySelectorAll('[data-flow-node]').forEach(button=>button.addEventListener('click',()=>openDetail(button.dataset.flowNode,button)));
    el('flowDrawerClose').addEventListener('click',closeDetail);
    el('flowDrawer').addEventListener('click',event=>{if(event.target===el('flowDrawer')){const rect=event.target.getBoundingClientRect();if(event.clientX<rect.left||event.clientX>rect.right||event.clientY<rect.top||event.clientY>rect.bottom)closeDetail();}});
    el('flowDrawer').addEventListener('close',()=>{selected='';opener?.focus({preventScroll:true});schedule();});
    root.querySelectorAll('[data-flow-tab]').forEach(button=>{
      button.addEventListener('click',()=>tab(button.dataset.flowTab));
      button.addEventListener('keydown',event=>{if(['ArrowLeft','ArrowRight','Home','End'].includes(event.key)){event.preventDefault();tab(event.key==='Home'?'overview':event.key==='End'?'details':detailTab==='overview'?'details':'overview');el(detailTab==='overview'?'flowTabOverview':'flowTabDetails').focus();}});
    });
    el('flowHelpOpen').addEventListener('click',()=>openDetail('gateway',el('flowHelpOpen'),true));
    el('flowMotion').addEventListener('click',()=>{motion=!motion;el('flowMotion').setAttribute('aria-pressed',String(motion));el('flowMotion').textContent=motion?'动画开启':'动画关闭';schedule();});
    el('flowFullscreen').addEventListener('click',()=>fullscreen().catch(()=>{}));
    document.addEventListener('fullscreenchange',fullscreenState);
    document.addEventListener('keydown',event=>{if(event.key==='Escape'&&fallbackFullscreen&&!el('flowDrawer').open)fullscreen();});
    const media=window.matchMedia?.('(prefers-reduced-motion: reduce)');reduced=!!media?.matches;
    media?.addEventListener?.('change',event=>{reduced=event.matches;schedule();});
    el('flowChannel').addEventListener('change',()=>{
      const value=el('flowChannel').value,outer=el('opsChannel');
      if(outer&&outer.value!==value){outer.value=value;outer.dispatchEvent(new Event('change',{bubbles:true}));}
      changeScope(value);
    });
    el('opsChannel')?.addEventListener('change',()=>changeScope(el('opsChannel').value));
    document.addEventListener('visibilitychange',()=>{if(document.hidden){stop();sample();render();}else start();});
    window.addEventListener('pagehide',()=>{stop();if(fallbackFullscreen){document.body.style.overflow=priorOverflow;fallbackFullscreen=false;root.classList.remove('is-maximized');}});
    window.addEventListener('pageshow',start);
    window.addEventListener('resize',scheduleLayout);
    if(typeof ResizeObserver!=='undefined')resizeObserver=new ResizeObserver(scheduleLayout);
    render();start();
  }
  if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',init);else init();
})();
