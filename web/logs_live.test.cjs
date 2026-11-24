const {test}=require('node:test');
const assert=require('node:assert/strict');
const {allText, loadJournal} = require('./journal-test-support.cjs');

function setup() {
  const timers = [], streams = [], fetches = [];
  class EventSource {
    constructor(url) { this.url = url; this.listeners = {}; streams.push(this); }
    addEventListener(name, listener) { this.listeners[name] = listener; }
    close() { this.closed = true; }
  }
  const api = loadJournal(['load', 'select', 'connectLive'], {
    EventSource,
    window: {location: {search: ''}, setTimeout: (fn, ms) => timers.push({fn, ms})},
    fetch: (url) => new Promise(resolve => fetches.push({url: String(url), resolve})),
  });
  return {...api, timers, streams, fetches};
}

const record=(id)=>({event:{kind:'request',action:'grok_request',channel:'grok',model:'grok-4.6',status:'success',request_id:id,timestamp:'2026-11-07T08:00:0'+id.slice(-1)+'Z'}});
const reply=(data,next='')=>({ok:true,status:200,headers:{get:()=>'application/json'},json:async()=>({data,next_cursor:next}),text:async()=>JSON.stringify({data,next_cursor:next})});
const flow=(stream,completed)=>stream.listeners.flow({data:JSON.stringify({instance_started:'i',routes:[{completed}]})});
const settle=()=>new Promise((r)=>setImmediate(r));

test('a finished request makes new rows appear on page one without losing the open detail',async()=>{
  const api=setup();
  const first=api.load(0); api.fetches[0].resolve(reply([record('req-2'),record('req-1')],'c1')); await first; await settle();
  api.select(1); // the operator opened req-1
  const detail = allText(api.ids.logsDetailBody);
  assert.match(detail, /req-1/);
  api.connectLive();
  const stream=api.streams[0];
  assert.match(stream.url,/\/api\/ops\/flow$/);
  flow(stream,5); // the first snapshot only sets the baseline
  assert.equal(api.timers.length,0);
  flow(stream,6);
  assert.equal(api.timers.length,1,'a finished request schedules a read');
  const live=api.timers.shift().fn(); await settle();
  api.fetches[1].resolve(reply([record('req-3'),record('req-2'),record('req-1')],'c1')); await live; await settle();
  const rows=api.ids.logsRows.children;
  assert.equal(rows.length,3);
  assert.equal(rows[0].classList.contains('is-new'),true,'the new row is marked');
  assert.equal(rows[1].classList.contains('is-new'),false);
  assert.equal(api.ids.logsDetail.classList.contains('is-open'),true,'the mobile drawer stays open');
  assert.equal(allText(api.ids.logsDetailBody), detail, 'the selected detail stays rendered');
  assert.equal(rows[2].classList.contains('is-selected'),true);
});

test('later pages do not move under the reader',async()=>{
  const api=setup();
  const first=api.load(0); api.fetches[0].resolve(reply([record('req-2')],'c1')); await first; await settle();
  const second=api.load(1); api.fetches[1].resolve(reply([record('req-1')],'')); await second; await settle();
  api.connectLive();
  flow(api.streams[0],1); flow(api.streams[0],2);
  assert.equal(api.timers.length,0,'page two is not refreshed live');
});
