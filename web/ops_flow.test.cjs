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
  const send = (active, started, completed) => streams.at(-1).listeners.flow({ data: JSON.stringify({ available:true, instance_started:'instance1', routes:[{channel:'cline',protocol:'responses',active,started,completed,qps:0.4,tps:12.8,usage_available:true}] }) });
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
