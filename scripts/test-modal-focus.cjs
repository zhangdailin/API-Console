const {spawn} = require('node:child_process');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '../web/static');
const os = require('node:os');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'console-modal-test-'));
const modal=fs.readFileSync(path.resolve(__dirname, '../web/templates/components/modals/key-modals.html'),'utf8');
const html='<html><head><link rel="stylesheet" href="/admin/css/main.css"></head><body><button id="trigger" onclick="ConsoleUI.modal(\'createKeyModal\',true)">打开</button><button id="backgroundAction">背景操作</button>'+modal+'<script src="/admin/js/ui.js"></script><script>function closeModal(id){ConsoleUI.modal(id,false)};let createdKeys=[];</script><script src="/admin/js/api-keys.js"></script></body></html>';
const pause = ms => new Promise(r=>setTimeout(r,ms));
async function until(test) {for(let i=0;i<100;i++){const value=await test();if(value)return value;await pause(100);}throw new Error('Timed out');}
(async()=>{
  const server = http.createServer((req,res)=>{
    const pathname = new URL(req.url,'http://localhost').pathname;
    res.setHeader('Cache-Control','no-store');
    if(pathname==='/admin/'){res.setHeader('Content-Type','text/html; charset=utf-8');res.end(html);return;}
    if(pathname.startsWith('/admin/js/')||pathname.startsWith('/admin/css/')){
      const file=path.resolve(root,pathname.slice('/admin/'.length));
      if(!file.startsWith(root+path.sep)){res.writeHead(404).end();return;}
      res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':'text/css');
      try{res.end(fs.readFileSync(file));}catch{res.writeHead(404).end();}return;
    }
    res.end('{}');
  });
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  const url='http://127.0.0.1:'+server.address().port+'/admin/?tab=accounts';
  const defaultBrowser = process.platform === 'win32' ? path.join(process.env['ProgramFiles(x86)'] || 'C:/Program Files (x86)', 'Microsoft/Edge/Application/msedge.exe') : '/usr/bin/google-chrome';
  const chrome=spawn(process.env.CHROME_BIN || defaultBrowser,['--headless','--no-sandbox','--disable-gpu','--remote-debugging-port=0','--user-data-dir='+profile,'about:blank'],{stdio:['ignore','ignore','pipe'],windowsHide:true});
  const browserClosed = new Promise(resolve => chrome.once('close', resolve));
  const waitForBrowserClose = async timeout => {
    let timer;
    try {
      return await Promise.race([
        browserClosed.then(() => true),
        new Promise(resolve => { timer = setTimeout(() => resolve(false), timeout); })
      ]);
    } finally { clearTimeout(timer); }
  };
  let chromeOutput='';chrome.stderr.on('data',data=>chromeOutput+=data);
  let browserStartError;
  chrome.once('error', error => { browserStartError = error; });
  let socket;
  try{
    const endpoint=await until(()=>{if(browserStartError)throw browserStartError;return chromeOutput.match(/ws:\/\/[^\s]+/)?.[0];});
    socket=new WebSocket(endpoint);await new Promise((resolve,reject)=>{socket.onopen=resolve;socket.onerror=reject;});
    let sequence=0;const pending=new Map();const browserErrors=[];
    socket.onmessage=({data})=>{const m=JSON.parse(data);if(m.method==="Runtime.exceptionThrown")browserErrors.push(m.params.exceptionDetails.text);const item=pending.get(m.id);if(item){pending.delete(m.id);m.error?item.reject(new Error(JSON.stringify(m.error))):item.resolve(m.result);}};
    const call=(method,params={},sessionId)=>new Promise((resolve,reject)=>{const id=++sequence;pending.set(id,{resolve,reject});socket.send(JSON.stringify({id,method,params,sessionId}));});
    const target=await call('Target.createTarget',{url:'about:blank'});
    const {sessionId}=await call('Target.attachToTarget',{targetId:target.targetId,flatten:true});
    const evaluate=async expression=>{const response=await call('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true},sessionId);if(response.exceptionDetails)throw new Error(response.exceptionDetails.exception?.description||'Browser evaluation failed');return response.result.value;};
    await call('Runtime.enable',{},sessionId);
    await call('Page.navigate',{url},sessionId);
    await until(()=>evaluate('typeof ConsoleUI!=="undefined"'));
    await evaluate('document.getElementById("trigger").focus();document.getElementById("trigger").click()');
    assert.equal(await evaluate('document.activeElement.id'),'keyName');
    assert.equal(await evaluate('document.getElementById("backgroundAction").inert'),true);
    const key = async (name, modifiers=0) => {
      await call('Input.dispatchKeyEvent',{type:'keyDown',key:name,code:name,windowsVirtualKeyCode:name==='Tab'?9:27,modifiers},sessionId);
      await call('Input.dispatchKeyEvent',{type:'keyUp',key:name,code:name,windowsVirtualKeyCode:name==='Tab'?9:27,modifiers},sessionId);
    };
    // Repeated keyboard traversal must never enter the background.
    for(let i=0;i<25;i++) {
      await key('Tab');
      assert.equal(await evaluate('document.getElementById("createKeyModal").contains(document.activeElement)'),true);
    }
    await evaluate('document.querySelector("#createKeyModal .modal-close").focus()');
    await key('Tab',8);
    assert.equal(await evaluate('document.activeElement.id'),'createKeySubmit');
    await key('Tab');
    assert.equal(await evaluate('document.activeElement.classList.contains("modal-close")'),true);
    // Escape respects the real key submission guard.
    await evaluate('document.getElementById("createKeySubmit").disabled=true');
    await key('Escape');
    assert.equal(await evaluate('getComputedStyle(document.getElementById("createKeyModal")).display'),'flex');
    await evaluate('document.getElementById("createKeySubmit").disabled=false');
    await key('Escape');
    assert.equal(await evaluate('document.activeElement.id'),'trigger');
    assert.equal(await evaluate('document.getElementById("backgroundAction").inert'),false);
    // Key display has no X button: its business close handler must still clear secrets.
    await evaluate('createdKeys=[{key:"fixture-secret"}];document.getElementById("fullKeyDisplay").textContent="fixture-secret";ConsoleUI.modal("showKeyModal",true)');
    await key('Escape');
    assert.equal(await evaluate('createdKeys.length'),0);
    assert.equal(await evaluate('document.getElementById("fullKeyDisplay").textContent'),'');
    assert.equal(await evaluate('document.activeElement.id'),'trigger');
    // Closing a nested dialog returns focus to the underlying modal, then its opener.
    await evaluate('ConsoleUI.modal("createKeyModal",true);ConsoleUI.modal("deleteKeyModal",true)');
    await key('Escape');
    assert.equal(await evaluate('document.getElementById("createKeyModal").contains(document.activeElement)'),true);
    await key('Escape');
    assert.equal(await evaluate('document.activeElement.id'),'trigger');
    assert.deepEqual(browserErrors,[]);
  } finally {
    let closed = false;
    try {
      // Ask Chromium to finish its profile writes before stopping its processes.
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({id: 0, method: 'Browser.close'}));
      }
      closed = await waitForBrowserClose(5000);
      if (!closed) {
        chrome.kill('SIGKILL');
        closed = await waitForBrowserClose(5000);
      }
    } finally {
      socket?.close();
      server.closeAllConnections();
      await new Promise(resolve => server.close(resolve));
    }
    if (!closed) throw new Error('Browser did not stop before profile cleanup');
    const resolvedProfile = path.resolve(profile);
    assert.equal(path.dirname(resolvedProfile), path.resolve(os.tmpdir()));
    assert.ok(path.basename(resolvedProfile).startsWith('console-modal-test-'));
    // Chromium helpers can release profile files after the main process exits.
    // Retry transient ENOTEMPTY/EBUSY/EPERM errors; permanent failures still fail CI.
    await fs.promises.rm(resolvedProfile, {recursive: true, force: true, maxRetries: 10, retryDelay: 100});
  }
  console.log('PASS: modal focus entry, Tab/Shift+Tab containment, inert background, guarded Escape, secret cleanup and focus restoration');
})().catch(err=>{console.error(err);process.exitCode=1;});
