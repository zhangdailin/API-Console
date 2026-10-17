// Run `go run scripts/preview-flow.go` first. No live gateway is contacted.
// PLAYWRIGHT_MODULE can point to an existing Playwright installation.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const {installFlowSimulation}=require('./flow-screen-fixture.cjs');
const assert=require('node:assert/strict');
const fs=require('node:fs');const path=require('node:path');
const base=process.env.FLOW_PREVIEW_URL||'http://127.0.0.1:3099/preview';
const output=path.resolve(process.env.FLOW_ARTIFACTS||'docs/verification/flow-screen-20261017');fs.mkdirSync(output,{recursive:true});
const chrome=process.env.CHROME_BIN||['/usr/bin/google-chrome','/usr/bin/chromium','/usr/bin/chromium-browser'].find(file=>fs.existsSync(file));
(async()=>{
 const browser=await chromium.launch({headless:true,...(chrome?{executablePath:chrome}:{}),args:['--no-sandbox']});
 const context=await browser.newContext({viewport:{width:1920,height:1080},recordVideo:{dir:output,size:{width:1920,height:1080}}});
 await context.addInitScript(installFlowSimulation,{theme:'dark'});
 const page=await context.newPage(),errors=[],report={simulated:true,checks:[],screenshots:[],browser:await browser.version()};
 page.on('pageerror',error=>errors.push(error.message));
 const poolState={fail:false,empty:false,stale:false};
 const poolRequests=[];
 await page.route('**/api/**',async route=>{
  const req=route.request(),url=new URL(req.url()),pathname=url.pathname;
  const json=(data,status=200)=>route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
  if(pathname==='/api/ops/pools'){
   poolRequests.push(req.url());if(poolState.fail)return json({error:'simulated pool failure'},503);
   const scope=url.searchParams.get('channel')||'';
   const data=await page.evaluate(({scope,empty,stale})=>window.__flowSim.pools(scope,empty,stale),{scope,...poolState});return json(data);
  }
  if(pathname==='/api/accounts')return json([]);
  if(pathname.startsWith('/api/system/'))return json({current:{version:'local-preview',commit:'simulated',build_type:'preview'},capabilities:{},has_update:false});
  return json({error:'Historical panels are outside this simulated preview'},503);
 });
 const waitLive=()=>page.waitForFunction(()=>document.querySelector('#opsFlowScreen')?.dataset.live==='true');
 async function screenshot(name,full=false){
  const file=path.join(output,name+'.png');
  if(full)await page.screenshot({path:file});else {
   await page.evaluate(()=>window.scrollTo(0,0));await page.waitForTimeout(50);
   const clip=await page.locator('#opsFlowScreen').evaluate(node=>{const r=node.getBoundingClientRect();return {x:r.x+scrollX,y:r.y+scrollY,width:r.width,height:r.height,scale:1};});
   const client=await page.context().newCDPSession(page);const result=await client.send('Page.captureScreenshot',{format:'png',clip,captureBeyondViewport:true});await client.detach();fs.writeFileSync(file,Buffer.from(result.data,'base64'));
  }
  report.screenshots.push(name+'.png');
 }
 function check(name){report.checks.push(name);console.log('PASS',name);}
 await page.goto(base);await waitLive();await page.waitForFunction(()=>document.querySelector('#flowPool-cline').textContent.includes('可用账号'));
 await page.locator('#opsFlowScreen').scrollIntoViewIfNeeded();
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 const geometry=await page.evaluate(()=>{
  const rect=selector=>document.querySelector(selector).getBoundingClientRect();const board=rect('#flowBoard'),left=rect('#opsNode-cline'),center=rect('#opsNode-gateway'),right=rect('#opsNode-responses');
  return {ordered:left.right<center.left&&center.right<right.left,paths:[...document.querySelectorAll('.flow-wire-base')].every(path=>path.getAttribute('d')?.includes('C')),boardHeight:board.height};
 });
 assert.ok(geometry.ordered&&geometry.paths);check('fixed channel → gateway → interface geometry and measured SVG connections');
 await page.waitForTimeout(1400);await screenshot('desktop-dark-1920');
 const rootCount=await page.locator('#opsFlowScreen *').count();
 await page.evaluate(()=>window.__flowSim.burst());await page.waitForTimeout(350);
 assert.equal(await page.locator('#opsFlowScreen *').count(),rootCount);assert.equal(await page.evaluate(()=>window.__flowSim.streams.filter(s=>!s.closed).length),1);
 check('100 snapshots retain stable DOM and one SSE connection');
 await page.evaluate(()=>window.__flowSim.resume());
 await page.click('#opsNode-cline');await page.waitForSelector('#flowDrawer[open]');
 await page.click('#flowTabOverview');await page.keyboard.press('ArrowRight');assert.equal(await page.locator('#flowTabDetails').getAttribute('aria-selected'),'true');
 assert.ok((await page.locator('#flowDetailedFacts').innerText()).includes('P95 1.2 s'));
 assert.ok((await page.locator('#flowDetailedFacts').innerText()).includes('暂无样本'));
 await screenshot('node-details-dark',true);await page.keyboard.press('Escape');assert.ok(await page.locator('#opsNode-cline').evaluate(node=>node===document.activeElement));
 check('drawer tabs, latency cohorts, Escape and focus restoration');
 await page.click('#flowFullscreen');await page.waitForFunction(()=>document.fullscreenElement===document.querySelector('#opsFlowScreen'));
 await page.waitForTimeout(400);await screenshot('fullscreen-dark-1920',true);
 assert.ok(await page.evaluate(()=>{const node=document.querySelector('#opsFlowScreen'),r=node.getBoundingClientRect();return Math.abs(r.width-innerWidth)<2&&Math.abs(r.height-innerHeight)<2&&node.scrollHeight<=node.clientHeight+1;}));
 await page.selectOption('#flowChannel','cline');await page.waitForFunction(()=>document.querySelector('#opsNode-grok').disabled);
 await page.waitForFunction(()=>document.querySelector('#flowTotal-qps').textContent==='3.60');
 assert.equal(await page.locator('#opsChannel').inputValue(),'cline');assert.equal(await page.evaluate(()=>window.__flowSim.streams.filter(s=>!s.closed).length),1);
 assert.ok(await page.evaluate(()=>{
  const value=id=>Number(document.querySelector('#'+id).textContent.replace(/,/g,''));
  return value('flowTotal-active')===value('flowGatewayActive')&&value('flowTotal-active')===value('flowActive-cline')&&value('flowTotal-active')===['chat','responses','messages'].reduce((n,key)=>n+value('flowActive-'+key),0)&&value('flowTotal-active')===value('flowTotal-waiting')+value('flowTotal-responding')&&document.querySelector('#flowTrendQpsValue').textContent==='3.60 req/s';
 }));
 await page.click('#opsNode-cline');await page.waitForFunction(()=>document.querySelector('#flowDetailScope').textContent.startsWith('Cline'));
 assert.ok((await page.locator('#opsFlowDetailFacts').innerText()).includes('3.60 req/s'));await page.keyboard.press('Escape');
 await page.selectOption('#flowChannel','');await page.waitForFunction(()=>!document.querySelector('#opsNode-grok').disabled);await page.click('#flowFullscreen');
 check('native fullscreen and synchronized channel scope');
 await page.evaluate(()=>{document.querySelector('#opsFlowScreen').requestFullscreen=async()=>{throw Error('simulated permission denial');};});
 const overflow=await page.evaluate(()=>document.body.style.overflow);await page.click('#flowFullscreen');await page.waitForSelector('#opsFlowScreen.is-maximized');
 await page.keyboard.press('Escape');assert.equal(await page.evaluate(()=>document.body.style.overflow),overflow);await page.evaluate(()=>delete document.querySelector('#opsFlowScreen').requestFullscreen);
 check('fullscreen permission fallback exits cleanly');
 await page.emulateMedia({reducedMotion:'reduce'});await page.waitForFunction(()=>document.querySelector('#opsFlowScreen').dataset.reducedMotion==='true');
 assert.ok(await page.evaluate(()=>[...document.querySelectorAll('.flow-wire-in,.flow-orbit-a')].every(node=>getComputedStyle(node).animationName==='none')));
 await page.emulateMedia({reducedMotion:'no-preference'});await page.click('#flowMotion');await page.waitForFunction(()=>document.querySelector('#opsFlowScreen').dataset.motion==='false');
 assert.ok(await page.evaluate(()=>[...document.querySelectorAll('.flow-wire-in,.flow-orbit-a')].every(node=>getComputedStyle(node).animationName==='none')));await page.click('#flowMotion');
 check('reduced-motion preference and explicit motion switch');
 await page.evaluate(()=>window.__flowSim.mode('waiting'));await page.waitForFunction(()=>document.querySelector('#flowTotal-tps').textContent==='待上报');
 assert.equal(await page.locator('#flowTotal-responding').innerText(),'0');check('waiting output never invents token throughput');
 await page.evaluate(()=>window.__flowSim.mode('failed'));await page.waitForFunction(()=>Number(document.querySelector('#flowTotal-failed').textContent)>0);
 assert.ok(await page.locator('#flowIncident-timeouts').evaluate(node=>node.classList.contains('is-degraded')));await screenshot('failure-state-dark');
 check('partial failures, rate limits, stream interruption and retries');
 await page.evaluate(()=>window.__flowSim.pause(false));const active=await page.locator('#flowTotal-active').innerText();
 await page.waitForFunction(()=>document.querySelector('#opsFlowScreen').dataset.live==='false',{},{timeout:8000});
 assert.equal(await page.locator('#flowTotal-active').innerText(),active);assert.ok((await page.locator('#opsFlowStatus').innerText()).includes('过期'));
 assert.equal(await page.locator('.flow-wire.has-in,.flow-wire.has-out').count(),0);await screenshot('stale-state-dark');
 await page.evaluate(()=>window.__flowSim.resume());await waitLive();check('five-second watchdog retains last observations and stops light trails');
 await page.evaluate(()=>window.__flowSim.pause(true));await page.waitForFunction(()=>document.querySelector('#opsFlowStatus').textContent.includes('重连'));
 await page.evaluate(()=>window.__flowSim.resume());await waitLive();
 await page.evaluate(()=>window.__flowSim.restart());await page.waitForTimeout(300);assert.ok((await page.locator('#flowTrendNote').innerText()).includes('1/60'));
 check('connection recovery and instance restart reset history');
 poolState.fail=true;await page.selectOption('#flowChannel','qoder');await page.waitForFunction(()=>document.querySelector('#flowPool-qoder').textContent.includes('读取失败'));await waitLive();
 assert.equal(await page.evaluate(()=>window.__flowSim.streams.filter(s=>!s.closed).length),1);check('pool read failure is independent of live flow');
 poolState.fail=false;poolState.stale=true;await page.selectOption('#flowChannel','grok');await page.waitForFunction(()=>document.querySelector('#flowPool-grok').textContent.includes('过期'));
 assert.equal(await page.locator('#flowCapacity-grok').innerText(),'槽位 —');check('stale pool observations cannot look like idle capacity');
 poolState.stale=false;poolState.empty=true;await page.evaluate(()=>window.__flowSim.mode('idle'));await page.selectOption('#flowChannel','workbuddy');
 await page.waitForFunction(()=>document.querySelector('#flowState-workbuddy').textContent==='未启用');assert.equal(await page.locator('#flowCapacity-workbuddy').innerText(),'槽位 0/0');check('empty account pool and idle traffic remain explicit');
 poolState.empty=false;await page.selectOption('#flowChannel','');await page.evaluate(()=>window.__flowSim.mode('normal'));await waitLive();
 await page.evaluate(()=>document.documentElement.dataset.theme='light');await page.waitForTimeout(300);
 assert.ok(await page.evaluate(()=>{
  const colors=['workbuddy','cline','qoder','grok'].map(key=>getComputedStyle(document.querySelector('#opsWire-'+key)).getPropertyValue('--node-color').trim());return new Set(colors).size===4;
 }));await screenshot('desktop-light-1920');
 await page.setViewportSize({width:1440,height:1000});await page.waitForTimeout(250);await screenshot('desktop-light-1440');
 await page.evaluate(()=>{
  const option=document.createElement('option');option.value='long-model-fixture';option.textContent='claude-opus-4-example-with-a-long-model-identifier';document.querySelector('#opsModel').appendChild(option);
  window.__flowSim.mode('waiting');
 });
 for(const [width,height]of [[1366,768],[1280,720],[1024,768],[960,720],[901,768],[1280,600],[1024,600]]){
  await page.setViewportSize({width,height});await page.evaluate(()=>window.scrollTo(0,0));await page.waitForTimeout(250);
  assert.ok(await page.evaluate(()=>{
   const root=document.querySelector('#opsFlowScreen'),r=root.getBoundingClientRect();
   return r.bottom<=innerHeight&&root.scrollHeight<=root.clientHeight+1&&document.documentElement.scrollWidth<=innerWidth+1;
  }),`embedded topology must fit ${width}×${height}`);
  assert.ok(await page.evaluate(()=>[...document.querySelectorAll('[data-flow-node]')].every(node=>{
   const box=node.getBoundingClientRect();
   return [...node.querySelectorAll(':scope > .flow-node-head,:scope > .flow-node-metrics,:scope > .flow-pool-line,:scope > .flow-capacity-track,:scope > code')].every(child=>{
    const r=child.getBoundingClientRect();return !r.width||!r.height||(r.left>=box.left&&r.right<=box.right+1&&r.top>=box.top&&r.bottom<=box.bottom+1);
   });
  })),`node contents must remain inside their cards at ${width}×${height}`);
  await screenshot(`compact-${width}x${height}`,true);
  await page.click('#flowFullscreen');await page.waitForFunction(()=>document.fullscreenElement===document.querySelector('#opsFlowScreen'));await page.waitForTimeout(200);
  assert.ok(await page.locator('#opsFlowScreen').evaluate(node=>node.scrollHeight<=node.clientHeight+1),`fullscreen must fit ${width}×${height}`);
  if(height===600)await screenshot(`fullscreen-${width}x${height}`,true);
  await page.click('#flowFullscreen');await page.waitForFunction(()=>document.fullscreenElement==null);
 }
 check('compact embedded and fullscreen layouts fit 1366×768, 1280×720, 1024×768 and 600px height');
 await page.evaluate(()=>window.__flowSim.mode('normal'));
 await page.setViewportSize({width:3840,height:2160});await page.evaluate(()=>document.documentElement.dataset.theme='dark');await page.waitForTimeout(250);await screenshot('desktop-dark-4k');
 await page.setViewportSize({width:390,height:844});await page.waitForTimeout(250);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'mobile page overflow');
 assert.equal(await page.locator('#flowWires').evaluate(node=>getComputedStyle(node).display),'none');await screenshot('mobile-dark-390');
 await page.evaluate(()=>document.documentElement.dataset.theme='light');await screenshot('mobile-light-390');check('light/dark themes at 1920, 1440, 4K and 390px without horizontal overflow');
 await page.setViewportSize({width:1920,height:1080});await page.evaluate(()=>document.documentElement.dataset.theme='dark');
 await page.click('#flowFullscreen');await page.waitForTimeout(1200);await page.evaluate(()=>window.__flowSim.mode('burst'));await page.waitForTimeout(1800);await page.evaluate(()=>window.__flowSim.mode('normal'));await page.click('#flowFullscreen');
 await page.goto(base+'?channel=cline');await waitLive();
 assert.equal(await page.locator('#flowChannel').inputValue(),'cline');assert.equal(await page.locator('#opsChannel').inputValue(),'cline');
 assert.equal(await page.evaluate(()=>window.__flowSim.streams.filter(s=>!s.closed).map(s=>s.url).join()),'/api/ops/flow?channel=cline');
 await page.selectOption('#flowChannel','');await waitLive();check('bookmarked channel scope starts with one matching SSE connection');
 assert.deepEqual(errors,[]);report.poolRequests=poolRequests.length;report.errors=errors;
 const html=await page.locator('#opsFlowScreen').evaluate(node=>{const copy=node.cloneNode(true);copy.querySelectorAll('.flow-facts').forEach(n=>n.replaceChildren());copy.querySelector('dialog').removeAttribute('open');return copy.outerHTML;});
 const read=name=>fs.readFileSync(path.join(__dirname,'../web/static',name),'utf8');
 const safe=js=>js.replace(/<\/script/gi,'<\\/script');
 const demo=`<!doctype html><html lang="zh-CN" data-theme="dark"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>实时流量态势 · 模拟演示</title><style>${read('css/main.css')}\n${read('css/ops-flow.css')}\nbody{padding:24px;background:var(--bg-main,#080f1d)}.demo-banner{max-width:1700px;margin:0 auto 16px;color:#8b9eb9;font:13px sans-serif}.demo-shell{max-width:1700px;margin:auto}.demo-banner button{padding:6px 12px;margin:4px;border:1px solid #45556c;border-radius:6px;background:transparent;color:inherit;cursor:pointer}</style><div class="demo-banner">交互演示 · 模拟数据，仅用于布局与动效验收。<button onclick="__flowSim.mode('normal')">正常流量</button><button onclick="__flowSim.mode('burst')">突发流量</button><button onclick="__flowSim.mode('waiting')">等待输出</button><button onclick="__flowSim.mode('failed')">异常</button><button onclick="__flowSim.mode('idle')">空闲</button><button onclick="__flowSim.pause(true)">断线</button><button onclick="__flowSim.resume()">恢复</button><button onclick="document.documentElement.dataset.theme=document.documentElement.dataset.theme==='dark'?'light':'dark'">切换主题</button></div><main class="demo-shell">${html}</main><script>${safe('('+installFlowSimulation.toString()+')({theme:"dark"});')}</script><script>${safe(read('js/ui.js'))}</script><script>globalThis.ConsoleAPI={json:async url=>__flowSim.pools(new URL(url,location.href).searchParams.get('channel')||'')};</script><script>${safe(read('js/ops-flow-model.js'))}</script><script>${safe(read('js/ops-flow.js'))}</script></html>`;
 fs.writeFileSync(path.join(output,'interactive-demo.html'),demo);
 const video=page.video();await context.close();await video.saveAs(path.join(output,'flow-screen-demo.webm'));await video.delete();for(const name of fs.readdirSync(output))if(/^page@.*\.webm$/.test(name))fs.unlinkSync(path.join(output,name));report.video='flow-screen-demo.webm';
 fs.writeFileSync(path.join(output,'browser-report.json'),JSON.stringify(report,null,2));await browser.close();console.log('Artifacts:',output);
})().catch(error=>{console.error(error);process.exit(1);});
