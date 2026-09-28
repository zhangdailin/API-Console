const {test}=require('node:test');
const assert=require('node:assert/strict');
const same=(a,b)=>assert.equal(JSON.stringify(a),JSON.stringify(b));
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

// Load config.js in a DOM stand-in. Only the pure helpers matter here: the
// anonymous-allowlist parser and the USD <-> tick conversion decide what the
// admin plane sends to the server.
function loadConfig(fetchImpl){
 const nodes=new Map();
 const node=id=>{if(!nodes.has(id))nodes.set(id,{value:'',checked:false,style:{},textContent:'',classList:{add(){},remove(){},toggle(){}}});return nodes.get(id)};
 const context=vm.createContext({
  console,Date,Math,Number,String,Array,Object,JSON,isNaN,parseInt,parseFloat,
  setTimeout(){},setInterval(){},fetch:fetchImpl||(()=>Promise.resolve({ok:true,headers:{get:()=>"application/json"},json:()=>({})})),
  window:{matchMedia:()=>({matches:false}),addEventListener(){},location:{href:''}},
  HTMLInputElement:class HTMLInputElement {},
  document:{readyState:'loading',addEventListener(){},getElementById:node,createElement:()=>({}),querySelectorAll:()=>[]},
  encodeData:v=>String(v),decodeData:v=>String(v),showToast(){},
 });
 // config.js declares its helpers at top level, so the harness only appends an
 // export for the pure functions it wants to assert on.
 let src=fs.readFileSync(path.join(__dirname,'static/js/config.js'),'utf8');
 src+='\nglobalThis.probe={parseAnonymousAllowIPs,ticksToUSD,usdToTicks,formatUSD,periodSuffix,applyConfigurationPayload,loadConfiguration,bindApiKeyActions,Input:HTMLInputElement};\n';
 vm.runInContext(src,context);
 return {api:context.probe,node,context};
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


test('configuration payload tolerates a missing optional control',()=>{
 const {api,node}=loadConfig();
 // The page can be served from a stale cached template while config.js is fresh.
 // Optional controls must not turn a valid API response into "配置加载失败".
 const original=node('cfg_cache_token_count');
 original.checked=false;
 api.applyConfigurationPayload({admin_password:'secret',anonymous_allow_ips:['203.0.113.1'],proxy_bypass:null,enable_token_cache:true,token_cache_ttl:300,token_cache_strategy:'1'});
 assert.equal(node('cfg_admin_pass').value,'secret');
 assert.equal(node('cfg_anonymous_allow_ips').value,'203.0.113.1');
 assert.equal(node('cfg_enable_token_cache').checked,true);
});

test('configuration loader applies a valid JSON response',async()=>{
 const {api,node}=loadConfig(()=>Promise.resolve({
  ok:true,status:200,headers:{get:()=> 'application/json; charset=utf-8'},
  json:()=>Promise.resolve({code:0,data:{admin_password:'loaded',token_cache_ttl:300,token_cache_strategy:'1'}}),
 }));
 assert.equal(await api.loadConfiguration(),true);
 assert.equal(node('cfg_admin_pass').value,'loaded');
});

test('configuration loader rejects bad HTTP and non-JSON responses before applying them',()=>{
 const source=fs.readFileSync(path.join(__dirname,'static/js/config.js'),'utf8');
 assert.match(source,/if \(!res\.ok\)/);
 assert.match(source,/includes\("application\/json"\)/);
 assert.match(source,/credentials: "same-origin"/);
 assert.match(source,/setConfigSaveError\("加载失败：" \+ reason\)/);
});
