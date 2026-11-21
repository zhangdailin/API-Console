const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('./test-support.cjs');

function load(fetch) {
 const nodes=new Map(); const node=id=>nodes.get(id)||nodes.set(id,{
   hidden:true,textContent:'',src:'',value:'',disabled:false,classList:{toggle(){}},
   addEventListener(event,handler) { this[event]=handler; }
 }).get(id);
 const context={fetch,AbortController,clearInterval,setInterval:()=>1,showToast(){},closeModal(){},loadAccounts(){},document:{getElementById:node}};
 vm.runInNewContext(fs.readFileSync(path.join(__dirname,'static/js/doubao-auth.js'),'utf8'),context);
 return {context,node};
}

test('Doubao login reports an HTML gateway response instead of a JSON parser error',async()=>{
 const {context,node}=load(async()=>({ok:true,status:200,text:async()=>'<html>old gateway</html>'}));
 await context.DoubaoLogin.start();
 assert.match(node('doubaoLoginStatus').textContent,/v1\.0\.180/);
 assert.match(node('doubaoLoginStatus').textContent,/HTML/);
});

test('Doubao Cookie import clears the credential field before the request',async()=>{
 let sent;
 const {node}=load(async (url,options)=>{
   assert.equal(node('doubaoCookieImport').value,'');
   sent={url,options};
   return {ok:true,status:201,text:async()=>JSON.stringify({id:5})};
 });
 node('doubaoCookieImport').value='sessionid=synthetic';
 await node('doubaoCookieImportButton').click();
 assert.equal(sent.url,'/api/doubao/cookie-import');
 assert.deepEqual(JSON.parse(sent.options.body),{cookie:'sessionid=synthetic',enabled:true});
 assert.doesNotMatch(node('doubaoLoginStatus').textContent,/synthetic/);
});
