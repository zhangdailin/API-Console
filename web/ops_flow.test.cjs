const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('./test-support.cjs');

test('SSE starts only the matching route, keeps short requests visible and clears stale activity', () => {
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) {
      const classes = new Set();
      nodes.set(id, { value: '', textContent: '', listeners: {},
        classList: { add: x => classes.add(x), remove: x => classes.delete(x), contains: x => classes.has(x), toggle: (x,on) => on ? classes.add(x) : classes.delete(x) },
        addEventListener(type, fn) { this.listeners[type] = fn; },
      });
    }
    return nodes.get(id);
  };
  const streams = [];
  const timers = new Map();
  let clock = 1000;
  let timerID = 0;
  const context = vm.createContext({
    document: { readyState: 'complete', hidden: false, getElementById: node, querySelectorAll: () => [], addEventListener() {} },
    window: { addEventListener() {}, setTimeout: fn => { timers.set(++timerID,fn); return timerID; }, clearTimeout: id => timers.delete(id) },
    Date: { now: () => clock },
    EventSource: class {
      constructor(url) { this.url = url; streams.push(this); this.listeners = {}; }
      addEventListener(type, fn) { this.listeners[type] = fn; }
      close() { this.closed = true; }
    },
  });
  vm.runInContext(fs.readFileSync(__dirname+'/static/js/ops-flow.js','utf8'), context);
  const send = (active, started, completed) => streams.at(-1).listeners.flow({ data: JSON.stringify({ available:true, instance_started:'instance1', routes:[{channel:'cline',protocol:'responses',active,started,completed,qps:0.4,tps:52.8,input_tps:40,output_tps:12.8,usage_available:true}] }) });
  const glowing = key => node('opsWire-'+key).classList.contains('is-flowing');
  send(0,0,0);
  assert(!glowing('cline'));
  send(1,1,0);
  assert(glowing('cline')); assert(glowing('responses')); assert(!glowing('chat')); assert(!glowing('grok'));
  assert.equal(node('opsFlowActive').textContent, '1 个进行中');
  assert.equal(node('opsNodeQps-cline').textContent, '0.40');
  assert.equal(node('opsNodeTps-cline').textContent, '12.8');
  assert.equal(node('opsGatewayTps').textContent, '12.8');
  send(0,1,1);
  assert(glowing('cline'), 'short requests get one visible traversal');
  clock += 1100;
  [...timers.values()].forEach(fn => fn());
  assert(!glowing('cline'));
  send(1,2,1);
  streams.at(-1).onerror();
  assert(!glowing('cline'), 'lost connections must clear old active counts');
  assert.equal(node('opsFlowActive').textContent,'— 个进行中');
  node('opsChannel').value = 'grok';
  node('opsChannel').listeners.change();
  assert(streams[0].closed);
  assert.equal(streams.at(-1).url, '/api/ops/flow?channel=grok');
});


test('topology removes standby, minute-window and peak/average panels', () => {
  const template = fs.readFileSync(__dirname+'/templates/pages/ops.html','utf8');
  assert.doesNotMatch(template, /opsGauge|opsLiveTabs|opsLiveQps|opsLiveTps|opsSpark/);
  for (const provider of ['workbuddy','cline','qoder','grok']) {
    assert(template.includes('opsNodeQps-'+provider));
    assert(template.includes('opsNodeTps-'+provider));
  }
});

test('node health and details preserve missing samples, output rate and fresh capacity', () => {
  const nodes = new Map(), streams = [], windowEvents = {};
  let clock = 1700000000000;
  class Clock extends Date { static now() { return clock; } }
  function element(id = '') {
    let text = '';
    const classes = new Set();
    const item = { children: [], listeners: {}, value: '',
      set id(value) { nodes.set(value, this); },
      get textContent() { return text + this.children.map(child => child.textContent).join(' '); },
      set textContent(value) { text = String(value); this.children = []; },
      appendChild(child) { this.children.push(child); return child; },
      replaceChildren() { this.children = []; text = ''; },
      addEventListener(type, fn) { this.listeners[type] = fn; },
      setAttribute() {}, querySelector() { return null; },
      classList: { add:x=>classes.add(x), remove:x=>classes.delete(x), contains:x=>classes.has(x), toggle:(x,on)=>on?classes.add(x):classes.delete(x) },
    };
    if (id) nodes.set(id, item);
    return item;
  }
  const node = id => nodes.get(id) || element(id);
  const buttons = ['cline', 'grok', 'gateway', 'responses'].map(key => {
    const button = node('opsNode-'+key); button.getAttribute = () => key; return button;
  });
  const context = vm.createContext({
    Date: Clock,
    document: { readyState:'complete', hidden:false, getElementById:node, createElement:()=>element(), querySelectorAll:()=>buttons, addEventListener(){} },
    window: { addEventListener(type,fn) { windowEvents[type]=fn; }, setTimeout(){return 1;}, clearTimeout(){} },
    EventSource: class { constructor(url){this.url=url;this.listeners={};streams.push(this);} addEventListener(type,fn){this.listeners[type]=fn;} close(){this.closed=true;} },
  });
  vm.runInContext(fs.readFileSync(__dirname+'/static/js/ops-flow.js','utf8'), context);
  const emptyDistribution = {samples:0,p50_ms:null,p95_ms:null};
  const stats = {completed:10,failed:2,failure_rate:.2,rate_limited:1,stream_errors:1,timeouts:0,canceled:0,waiting:1,responding:1,longest_active_ms:3000,
    observed:10,usage_reported:8,retried:2,attempts:12,account_switches:1,retry_wait_ms:2000,queue_wait_ms:1000,
    first_generation:{samples:8,p50_ms:100,p95_ms:1200}, first_visible:emptyDistribution, body_ttfb:emptyDistribution,
    duration:{samples:10,p50_ms:2000,p95_ms:4000}, http_phases:{dns_ms:{samples:2,avg_ms:10}}};
  const payload = {available:true,instance_started:'one',latency_window_seconds:900,window_seconds:60,routes:[{channel:'cline',protocol:'responses',active:2,started:12,completed:10,qps:.4,input_tps:40,output_tps:10,tps:50,usage_available:true}],
    nodes:{cline:stats,responses:stats,gateway:stats,grok:{...stats,completed:0,failed:0,failure_rate:null,first_generation:emptyDistribution,body_ttfb:{samples:1,p50_ms:2997,p95_ms:2997}}}};
  const send = value => streams.at(-1).listeners.flow({data:JSON.stringify(value)});
  send(payload);
  assert.equal(node('opsNodeTps-cline').textContent,'10.0', 'output TPS must exclude input tokens');
  assert.equal(node('opsNodeLatency-cline').textContent,'首生成 P95 1.2 s');
  assert.equal(node('opsNodeLatency-grok').textContent,'正文 TTFB P95 3.0 s', 'nonstream latency must be shown even after 60-second counters expire');
  assert(node('opsNodeLatency-grok').title.includes('近 15 分钟'));
  assert(node('opsNodeLatency-grok').title.includes('1 个非流式样本'));
  assert.equal(node('opsNodeFailure-cline').textContent,'失败 20.0%');
  assert(node('opsNodeFailure-cline').classList.contains('is-degraded'));
  assert.equal(node('opsNodeFailure-grok').textContent,'失败 —', 'missing evidence must not be healthy zero');
  node('opsNode-cline').listeners.click();
  let detail = node('opsFlowDetailFacts').textContent;
  assert(detail.includes('P50 100 ms / P95 1.2 s · 8 样本'));
  assert(detail.includes('可见首字延迟 — · 近 15 分钟无可见文本样本'));
  assert(node('opsFlowDetailTitle').textContent.includes('延迟近 15 分钟 · 错误与等待近 60 秒'));
  assert(detail.includes('1 尚未输出 / 1 已开始输出'));
  assert(detail.includes('2 个请求重试 · 12 次尝试 · 1 次切号'));
  assert(detail.includes('重试 200 ms · 其中排队 100 ms'));
  assert(detail.includes('DNS 10 ms（2 次）'));
  assert(!detail.includes('TLS 0'));
  windowEvents['ops:pool']({detail:[{channel:'cline',capacity_available:true,capacity_sampled_at:new Date(clock).toISOString(),accounts_available:2,accounts_enabled:3,capacity_slots:20,occupied_slots:5}]});
  assert(node('opsFlowDetailFacts').textContent.includes('5/20 可用池槽位占用（25.0%）'));
  clock+=31000; send(payload);
  assert(node('opsFlowDetailFacts').textContent.includes('容量快照未就绪或已过期'));
  send({...payload,nodes:{...payload.nodes,cline:{...stats,body_ttfb:{samples:1,p50_ms:8287,p95_ms:8287}}}});
  assert.equal(node('opsNodeLatency-cline').textContent,'首生成 P95 1.2 s', 'mixed requests keep distinct distributions and prefer streaming');
  assert(node('opsNodeLatency-cline').title.includes('正文 TTFB P95 8.3 s'));
  assert(node('opsFlowDetailFacts').textContent.includes('P50 8.3 s / P95 8.3 s · 1 样本'));
  send({...payload,nodes:{...payload.nodes,cline:{...stats,first_generation:emptyDistribution,body_ttfb:{samples:1,p50_ms:0,p95_ms:0}}}});
  assert.equal(node('opsNodeLatency-cline').textContent,'正文 TTFB P95 0 ms', 'measured zero is valid');
  send({...payload,nodes:{...payload.nodes,cline:{...stats,first_generation:emptyDistribution}}});
  assert.equal(node('opsNodeLatency-cline').textContent,'P95 无近期样本');
  assert(node('opsNodeLatency-cline').title.includes('请求结束后计入'));
  assert(node('opsFlowDetailFacts').textContent.includes('近 15 分钟无流式首生成样本'));
  send({...payload,latency_window_seconds:undefined});
  assert(node('opsNodeLatency-cline').title.includes('近 1 分钟'), 'older server metadata must not claim a 15-minute window');
  send({...payload,routes:[{...payload.routes[0],usage_available:false}]});
  assert.equal(node('opsNodeTps-cline').textContent,'—');
  const old=streams.at(-1);
  node('opsChannel').value='grok'; node('opsChannel').listeners.change();
  old.listeners.flow({data:JSON.stringify(payload)});
  assert.equal(node('opsFlowActive').textContent,'— 个进行中');
  assert.equal(node('opsNodeFailure-cline').textContent,'失败 —');
  assert.equal(node('opsNodeLatency-cline').textContent,'P95 范围外');
  assert.equal(node('opsNodeLatency-grok').textContent,'P95 待采集');
  send({available:false,routes:[]});
  assert.equal(node('opsFlowDetailFacts').textContent,'');
});
