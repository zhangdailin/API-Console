const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

// A tile: enough element for the slot helpers.
function element(){
  const classes=new Set();
  const node={children:[],dataset:{},style:{setProperty(){}},textContent:'',innerHTML:'',
    classList:{add:(...c)=>c.forEach((x)=>classes.add(x)),remove:(...c)=>c.forEach((x)=>classes.delete(x)),toggle(){},contains:(c)=>classes.has(c)},
    appendChild(child){this.children.push(child);return child},replaceChildren(...n){this.children=n},prepend(){},
    querySelector:()=>element(),querySelectorAll:()=>[],setAttribute(){},addEventListener(){},closest:()=>null};
  return node;
}

test('image generation runs one request for one image at a time',async()=>{
  let inFlight=0, peak=0;
  const bodies=[];
  const context=vm.createContext({
    console,Promise,JSON,Math,Number,String,Array,
    document:{getElementById:()=>null,querySelector:()=>null,querySelectorAll:()=>[],createElement:()=>element(),addEventListener(){}},
    window:{addEventListener(){},setInterval:()=>1,clearInterval(){},location:{origin:'http://x'}},
    localStorage:{getItem:()=>null,setItem(){}},sessionStorage:{getItem:()=>null},
    fetch:async(_url,options)=>{
      inFlight+=1; peak=Math.max(peak,inFlight); bodies.push(JSON.parse(options.body));
      await new Promise((r)=>setImmediate(r));
      inFlight-=1;
      return {ok:true,json:async()=>({data:[{url:'https://assets.grok.com/a/'+bodies.length+'.jpg'}]})};
    },
  });
  let src=fs.readFileSync(path.join(__dirname,'static/js/grok-imagine.js'),'utf8');
  src=src.replace(/  window\.GrokImagine = \{[\s\S]*?\};\n\}\)\(\);\s*$/,'  globalThis.review={runBatch};\n})();');
  vm.runInContext(src,context);
  const slots=Array.from({length:6},()=>{const tile=element();return {tile,body:element(),status:'pending',progress:0,url:''};});
  const batch={slots,ready:0,failed:0};
  const results=await context.review.runBatch(batch,'a cat','1:1','grok-imagine-image-lite','lite',false,undefined);
  assert.equal(peak,1,'never more than one request in flight');
  assert.equal(bodies.length,6,'one request per image');
  assert.ok(bodies.every((body)=>body.n===1),'each request asks for one image');
  assert.deepEqual(Array.from(results),[true,true,true,true,true,true]);
});
