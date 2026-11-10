const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

// A tile: enough element for the slot helpers.
function element(){
  const classes=new Set();
  const node={children:[],dataset:{},style:{setProperty(){}},textContent:'',innerHTML:'',
    classList:{add:(...c)=>c.forEach((x)=>classes.add(x)),remove:(...c)=>c.forEach((x)=>classes.delete(x)),toggle:(c,on)=>{if(on===undefined?!classes.has(c):on)classes.add(c);else classes.delete(c)},contains:(c)=>classes.has(c)},
    appendChild(child){this.children.push(child);return child},replaceChildren(...n){this.children=n},prepend(){},
    querySelector:()=>element(),querySelectorAll:()=>[],setAttribute(){},addEventListener(){},closest:()=>null};
  return node;
}

function setup(){
  const state={inFlight:0,peak:0,bodies:[]};
  const context=vm.createContext({
    console,Promise,JSON,Math,Number,String,Array,Boolean,
    document:{getElementById:()=>null,querySelector:()=>null,querySelectorAll:()=>[],createElement:()=>element(),addEventListener(){}},
    window:{addEventListener(){},setInterval:()=>1,clearInterval(){},location:{origin:'http://x'}},
    localStorage:{getItem:()=>null,setItem(){}},sessionStorage:{getItem:()=>null},
    fetch:async(_url,options)=>{
      state.inFlight+=1; state.peak=Math.max(state.peak,state.inFlight);
      const body=JSON.parse(options.body); state.bodies.push(body);
      await new Promise((r)=>setImmediate(r));
      state.inFlight-=1;
      const data=Array.from({length:body.n},(_,i)=>({url:'https://assets.grok.com/a/'+state.bodies.length+'-'+i+'.jpg'}));
      return {ok:true,json:async()=>({data})};
    },
  });
  let src=fs.readFileSync(path.join(__dirname,'static/js/grok-imagine.js'),'utf8');
  src=src.replace(/  window\.GrokImagine = \{[\s\S]*?\};\n\}\)\(\);\s*$/,'  globalThis.review={runBatch,setAvailability:(value)=>{availability=value;}};\n})();');
  vm.runInContext(src,context);
  const slots=Array.from({length:6},()=>({tile:element(),body:element(),status:'pending',progress:0,url:''}));
  return {state,review:context.review,batch:{slots,ready:0,failed:0}};
}

test('a Free-only pool runs one request for one image at a time',async()=>{
  const {state,review,batch}=setup();
  review.setAvailability({webPlans:{free:3,paid:0,unknown:0},media:{image_pro:true,image_edit:false}});
  const results=await review.runBatch(batch,'a cat','1:1','grok-imagine-image','basic',false,undefined);
  assert.equal(state.peak,1,'never more than one request in flight');
  assert.equal(state.bodies.length,6,'one request per image');
  assert.ok(state.bodies.every((body)=>body.n===1),'each request asks for one image');
  assert.deepEqual(Array.from(results),[true,true,true,true,true,true]);
});

test('a pool with a paid account keeps the parallel batch',async()=>{
  for (const plans of [{free:3,paid:1,unknown:0},{free:0,paid:2,unknown:0},{free:2,paid:0,unknown:1},null]) {
    const {state,review,batch}=setup();
    review.setAvailability({webPlans:plans,media:null});
    const results=await review.runBatch(batch,'a cat','1:1','grok-imagine-image','basic',false,undefined);
    assert.equal(state.peak,2,`two requests run side by side for ${JSON.stringify(plans)}`);
    assert.equal(state.bodies.length,3,'three requests of two images fill six slots');
    assert.ok(state.bodies.every((body)=>body.n===2));
    assert.deepEqual(Array.from(results),[true,true,true,true,true,true]);
  }
});

test('the panel offers 标准 and 高质量 only, and edits follow the pool allowance',()=>{
  const page=fs.readFileSync(path.join(__dirname,'templates/pages/grok-tools.html'),'utf8');
  const toggle=page.slice(page.indexOf('id="imagineQualityToggle"'),page.indexOf('</div>',page.indexOf('id="imagineQualityToggle"')));
  assert.ok(!toggle.includes('快速') && !toggle.includes('data-imagine-quality="lite"'),'快速 is no longer offered');
  assert.match(toggle,/data-imagine-quality="basic">标准/);
  assert.match(toggle,/data-imagine-quality="quality">高质量/);

  const button=(attrs)=>{const b=element();Object.assign(b.dataset,attrs);b.removeAttribute=(k)=>{if(k==='title')b.title=undefined};return b;};
  const quality=['basic','quality'].map((q)=>button({imagineQuality:q}));
  const operations=[button({imagineOperation:'generate'}),button({imagineOperation:'edit'})];
  quality[0].classList.add('is-active'); operations[1].classList.add('is-active');
  const query=(selector)=>{
    if (selector.includes('imagineQualityToggle') && selector.includes('.is-active')) return quality.find((b)=>b.classList.contains('is-active'))||null;
    if (selector.includes('imagineOperationToggle') && selector.includes('.is-active')) return operations.find((b)=>b.classList.contains('is-active'))||null;
    return null;
  };
  const queryAll=(selector)=>{
    if (selector.startsWith('#imagineQualityToggle')) return quality;
    if (selector.includes("data-imagine-operation='edit'")) return [operations[1]];
    if (selector.startsWith('#imagineOperationToggle')) return operations;
    return [];
  };
  const context=vm.createContext({console,Promise,JSON,Math,Number,String,Array,Boolean,
    document:{getElementById:()=>null,querySelector:query,querySelectorAll:queryAll,createElement:()=>element(),addEventListener(){}},
    window:{addEventListener(){},setInterval:()=>1,clearInterval(){}},localStorage:{getItem:()=>null,setItem(){}}});
  let src=fs.readFileSync(path.join(__dirname,'static/js/grok-imagine.js'),'utf8');
  src=src.replace(/  window\.GrokImagine = \{[\s\S]*?\};\n\}\)\(\);\s*$/,'  globalThis.review={applyAvailability,syncQualityModel,setAvailability:(value)=>{availability=value;}};\n})();');
  vm.runInContext(src,context);

  // A quality left over from the removed tier resolves to 标准.
  assert.equal(context.review.syncQualityModel().quality,'basic');
  assert.equal(context.review.syncQualityModel().model,'grok-imagine-image');

  context.review.setAvailability({webPlans:{free:2,paid:0,unknown:0},media:{image:false,image_pro:false,image_edit:false}});
  context.review.applyAvailability();
  assert.match(String(quality[0].title),/标准出图额度已用完/,'a spent standard allowance is explained on 标准');
  assert.match(String(quality[1].title),/高质量出图额度已用完/,'a spent image_pro allowance is explained on 高质量');
  assert.equal(operations[1].disabled,true,'编辑 is disabled without an edit allowance');
  assert.equal(operations[0].classList.contains('is-active'),true,'the operation falls back to 生成');

  // A paid pool whose imagePro is spent still draws standard images.
  context.review.setAvailability({webPlans:{free:0,paid:2,unknown:0},media:{image:true,image_pro:false,image_edit:true}});
  context.review.applyAvailability();
  assert.equal(quality[0].title,undefined,'标准 stays available on the weekly pool');
  assert.match(String(quality[1].title),/高质量出图额度已用完/);
  assert.equal(operations[1].disabled,false);
});

test('the video panel sends no reasoning effort and forgets a saved one', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const script = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  assert.ok(!page.includes('id="videoEffort"'), 'the video page has no reasoning effort control');
  assert.ok(!script.includes('payload.reasoning_effort'), 'video requests carry no reasoning_effort');
  assert.match(script, /saveGrokToolsUIState\(\{ videoEffort: undefined \}\)/, 'a saved video effort is cleared');
});

test('the video panel has no advanced settings', () => {
  const page = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const script = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
  for (const id of ['video-advanced-settings', 'videoAction', 'videoImageUrl', 'videoReferenceURL', 'videoSourceURL', 'videoReferenceVoice']) {
    assert.ok(!page.includes(id), `${id} is still on the page`);
    assert.ok(!script.includes(id), `${id} is still wired`);
  }
  assert.ok(script.includes('`${toolInferencePrefix()}/videos`'), 'video requests go to the generation endpoint');
});
