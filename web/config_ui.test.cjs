const {test}=require('node:test');
const assert=require('node:assert/strict');
const same=(a,b)=>assert.equal(JSON.stringify(a),JSON.stringify(b));
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

// Execute config.js with the real shared UI/API primitives and a small DOM.
// Both pure billing/security helpers and HTTP failure behavior are covered.
function loadConfig(fetchImpl){
 const nodes=new Map();
 const notices=[];
 const node=id=>{
  if(!nodes.has(id)) {
   const classes=new Set();
   nodes.set(id,{value:'',checked:false,style:{},textContent:'',classList:{
    add:name=>classes.add(name),remove:name=>classes.delete(name),
    toggle:(name,on)=>on?classes.add(name):classes.delete(name),contains:name=>classes.has(name),
   }});
  }
  return nodes.get(id);
 };
 const context=vm.createContext({
  console,Date,Math,Number,String,Array,Object,JSON,isNaN,parseInt,parseFloat,
  setTimeout(){},setInterval(){},fetch:fetchImpl||(()=>Promise.resolve({ok:true,headers:{get:()=>"application/json"},json:()=>({})})),
  window:{matchMedia:()=>({matches:false}),addEventListener(){},location:{href:''}},
  HTMLInputElement:class HTMLInputElement {},
  document:{readyState:'loading',addEventListener(){},getElementById:node,createElement:()=>({}),querySelectorAll:()=>[]},
  encodeData:v=>String(v),decodeData:v=>String(v),showToast:(message,kind)=>notices.push({message,kind}),
 });
 // Explicitly execute ui.js so failures exercise ConsoleAPI, not a test double.
 vm.runInContext(fs.readFileSync(path.join(__dirname,'static/js/ui.js'),'utf8'),context);
 // Export the config helpers without changing the production asset.
 let src=fs.readFileSync(path.join(__dirname,'static/js/config.js'),'utf8');
 src+='\nglobalThis.probe={parseAnonymousAllowIPs,ticksToUSD,usdToTicks,formatUSD,periodSuffix,applyConfigurationPayload,loadConfiguration,saveConfiguration,bindApiKeyActions,Input:HTMLInputElement};\n';
 vm.runInContext(src,context);
 return {api:context.probe,node,context,notices};
}

// Arrays built inside the vm belong to that realm, so a strict deep comparison
// would fail on the prototype rather than on the value.
const plain=v=>Array.from(v);

test('API key actions delegate equally for desktop and mobile, without exposing masked keys',()=>{
 const {api,context}=loadConfig();
 const calls=[];
 context.openEditKeyModal=id=>calls.push(['edit',id]);
 context.rotateApiKey=id=>calls.push(['rotate',id]);
 context.openDeleteKeyModal=(id,label)=>calls.push(['delete',id,label]);
 context.toggleKeyStatus=(id,checked)=>calls.push(['toggle',id,checked]);
 const container={contains:()=>true};
 api.bindApiKeyActions(container);
 const click=(action,id,label)=>container.onclick({target:{closest:()=>({dataset:{action,id,label}})}});
 click('edit-key','a');
 click('rotate-key','b');
 click('delete-key','c',encodeURIComponent('masked…suffix'));
 container.onchange({target:Object.assign(new api.Input(),{dataset:{action:'toggle-key',id:'d'},checked:true})});
 assert.deepStrictEqual(calls,[['edit','a'],['rotate','b'],['delete','c','masked…suffix'],['toggle','d',true]]);
});

test('anonymous_allow_ips is parsed into a trimmed list, and an empty box means nobody',()=>{
 const {api,node}=loadConfig();
 node('cfg_anonymous_allow_ips').value=' 203.0.113.20 \n\n198.51.100.0/24\n';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),['203.0.113.20','198.51.100.0/24']);
 node('cfg_anonymous_allow_ips').value='   \n ';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),[]);
 node('cfg_anonymous_allow_ips').value='';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),[]);
});

test('USD and ticks convert both ways without losing the integer ledger',()=>{
 const {api}=loadConfig();
 assert.equal(api.usdToTicks(0.5),5_000_000_000);
 assert.equal(api.usdToTicks(0),0);
 assert.equal(api.usdToTicks(''),0);
 assert.equal(api.usdToTicks(-1),0);
 assert.equal(api.ticksToUSD(5_000_000_000),0.5);
 assert.equal(api.ticksToUSD(0),0);
 assert.equal(api.formatUSD(0.5),'$0.50');
 // A round trip through the ledger stays an integer.
 assert.equal(Number.isInteger(api.usdToTicks(api.ticksToUSD(9_000_000_000))),true);
});

test('a billing policy line reads in dollars and names the period',()=>{
 const {api}=loadConfig();
 assert.equal(api.periodSuffix({billing_period_days:30}),' · 30 天账期');
 assert.equal(api.periodSuffix({billing_period_days:0}),'');
 assert.equal(api.periodSuffix({}),'');
});


test('configuration payload hydrates security and proxy controls without simulated cache fields',()=>{
 const {api,node}=loadConfig();
 api.applyConfigurationPayload({admin_password:'secret',anonymous_allow_ips:['203.0.113.1'],proxy_url:'http://proxy.example:8080',proxy_bypass:['example.com'],shared_stream_idle_timeout_seconds:300,workbuddy_default_max_tokens:8192,first_token_timeout_seconds:60,request_timeout:7200,concurrency_timeout:7200,qoder_queue_wait_budget_ms:0,qoder_queue_retry_interval_ms:0});
 assert.equal(node('cfg_admin_pass').value,'secret');
 assert.equal(node('cfg_anonymous_allow_ips').value,'203.0.113.1');
 assert.equal(node('cfg_proxy_url').value,'http://proxy.example:8080');
 assert.equal(node('cfg_proxy_bypass').value,'example.com');
});

test('save sends security and proxy settings but no local cache settings',async()=>{
 let saved;
 const {api,node}=loadConfig((_url,options)=>{
  saved=JSON.parse(options.body);
  return Promise.resolve({ok:true,json:()=>Promise.resolve({code:0})});
 });
 api.applyConfigurationPayload({});
 node('cfg_admin_pass').value='changed';
 node('cfg_proxy_url').value='http://proxy.example:8080';
 node('cfg_anonymous_allow_ips').value='203.0.113.1';
 node('cfg_proxy_bypass').value='example.com';
 await api.saveConfiguration();
 same(saved,{admin_password:'changed',anonymous_allow_ips:['203.0.113.1'],proxy_url:'http://proxy.example:8080',proxy_bypass:['example.com'],shared_stream_idle_timeout_seconds:300,workbuddy_default_max_tokens:8192,first_token_timeout_seconds:60,request_timeout:7200,concurrency_timeout:7200,qoder_queue_wait_budget_ms:0,qoder_queue_retry_interval_ms:0});
});

test('configuration page omits simulated cache section, stats and clear actions',()=>{
 const html=fs.readFileSync(path.join(__dirname,'templates/pages/config.html'),'utf8');
 const js=fs.readFileSync(path.join(__dirname,'static/js/config.js'),'utf8');
 assert.doesNotMatch(html,/groupCache|cacheStatsText|cfg_enable_token_cache|cfg_token_cache_ttl|cfg_token_cache_strategy|cacheConfigDetails|clearCache/);
 assert.doesNotMatch(js,/token-cache\/|enable_token_cache|token_cache_ttl|token_cache_strategy|cacheStatsText|clearCache|updateMemoryEstimation/);
});

test('configuration loader applies a valid JSON response',async()=>{
 const {api,node}=loadConfig(()=>Promise.resolve({
  ok:true,status:200,headers:{get:()=> 'application/json; charset=utf-8'},
  json:()=>Promise.resolve({code:0,data:{admin_password:'loaded',proxy_url:'http://proxy.example:8080'}}),
 }));
 assert.equal(await api.loadConfiguration(),true);
 assert.equal(node('cfg_admin_pass').value,'loaded');
});

for (const scenario of [
 {name:'403',status:403,body:JSON.stringify({error:{message:'forbidden by policy'}}),reason:'forbidden by policy'},
 {name:'503',status:503,body:'',reason:'HTTP 503'},
 {name:'HTML login page',status:200,contentType:'text/html; charset=utf-8',body:'<html>login</html>',reason:'接口未返回 JSON，登录状态可能已失效'},
 {name:'malformed JSON',status:200,contentType:'application/json',body:'{"data":',malformed:true},
]) {
 test(`configuration loader preserves form values on ${scenario.name}`,async()=>{
  const calls=[];
  let jsonReads=0;
  let textReads=0;
  const {api,node,notices}=loadConfig(async(url,options)=>{
   calls.push({url,options});
   return {
    ok:scenario.status===200,status:scenario.status,
    headers:{get:()=>scenario.contentType||'application/json'},
    text:async()=>{textReads++;return scenario.body;},
    json:async()=>{
     jsonReads++;
     if(scenario.malformed) return JSON.parse(scenario.body);
     // A faulty HTTP/content-type guard would apply this payload and overwrite edits.
     return {data:{admin_password:'must-not-apply',proxy_url:'must-not-apply'}};
    },
   };
  });
  const original={cfg_admin_pass:'local secret',cfg_anonymous_allow_ips:'203.0.113.20\n198.51.100.0/24',cfg_proxy_url:'http://local.example:8080',cfg_proxy_bypass:'example.net\nexample.org'};
  for(const [id,value] of Object.entries(original))node(id).value=value;
  assert.equal(await api.loadConfiguration(),false);
  assert.deepStrictEqual(Object.fromEntries(Object.keys(original).map(id=>[id,node(id).value])),original);
  assert.equal(calls.length,1);
  assert.equal(calls[0].url,'/api/config/list');
  assert.equal(calls[0].options.cache,'no-store');
  assert.equal(calls[0].options.credentials,'same-origin');
  assert.equal(jsonReads,scenario.malformed?1:0);
  assert.equal(textReads,scenario.status===200?0:1);
  const reason=scenario.malformed?(() => {try{JSON.parse(scenario.body);}catch(error){return error.message;}})():scenario.reason;
  assert.equal(node('cfgSaveError').textContent,'加载失败：'+reason);
  assert.equal(node('cfgSaveError').classList.contains('hidden'),false);
  assert.deepStrictEqual(notices,[{message:'配置加载失败：'+reason,kind:'error'}]);
 });
}


test('performance controls preserve explicit values and reject invalid save',async()=>{
 let calls=0; const {api,node}=loadConfig(()=>{calls++; return Promise.resolve({ok:true,json:()=>({code:0})});});
 api.applyConfigurationPayload({workbuddy_default_max_tokens:16384,first_token_timeout_seconds:-1,qoder_queue_wait_budget_ms:3000});
 assert.equal(node('cfg_workbuddy_default_max_tokens').value,'16384');
 assert.equal(node('cfg_first_token_timeout_seconds').value,'-1');
 node('cfg_workbuddy_default_max_tokens').value='0';
 await api.saveConfiguration(); assert.equal(calls,0);
});
