// Authentication/key-management UI regression without browser or credentials.
const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const root=path.resolve(__dirname,'..'),html=fs.readFileSync(path.join(root,'internal/server/console.html'),'utf8');
const script=fs.readFileSync(path.join(root,'internal/server/console/access.js'),'utf8');
class Element{
 constructor(){this.events={};this.value='';this.hidden=false;this.disabled=false;this.checked=false;this.open=false;this.children=[];this.classes=new Set();this.classList={add:v=>this.classes.add(v),remove:v=>this.classes.delete(v)};}
 addEventListener(name,fn){(this.events[name]||=[]).push(fn);}
 async emit(name,event={}){for(const fn of this.events[name]||[])await fn(event);}
 set innerHTML(value){this.markup=value;this.children=[...value.matchAll(/<input type="checkbox" value="([^"]+)"/g)].map(match=>Object.assign(new Element(),{value:match[1]}));}
 get innerHTML(){return this.markup||'';}
 querySelectorAll(selector){return selector==='input:checked'?this.children.filter(el=>el.checked):this.children;}
 showModal(){this.open=true;}
 close(){this.open=false;this.emit('close');}
}
function app(){
 const nodes=new Map([...html.matchAll(/\bid="([^"]+)"/g)].map(match=>[match[1],new Element()]));
 const $=id=>{assert.ok(nodes.has(id),'unknown DOM id '+id);return nodes.get(id)};
 const calls=[],state={authenticated:false,csrf:'',authVersion:0,overviewVersion:0,usageVersion:0,adminRequests:new Set()},window=new Element();
 window.dispatchEvent=event=>window.emit(event.type);
 const document={body:new Element(),querySelectorAll:()=>[...nodes.values()].filter(el=>el.open)};
 const api=(url,opts)=>new Promise((resolve,reject)=>calls.push({url,opts,resolve,reject}));
 const counts={refresh:0};
 vm.runInNewContext(script,{$,api,state,window,document,ui:{view:'keys'},Event,clearTimeout,stopEvents:()=>{},stopPoll:()=>{},refreshAll:()=>counts.refresh++,loadUsage:()=>{},setAccountsAuto:()=>{},copyText:()=>{},toast:()=>{},fmtTime:t=>t,esc:value=>String(value??'').replaceAll('<','&lt;')});
 return {$,state,calls,window,document,counts};
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const session={initialized:true,authenticated:true,username:'admin',csrf_token:'csrf-test'};
const emptyKeys={keys:[],channels:['cn','workbuddy','codebuddy'],active_channels:['workbuddy','codebuddy']};

test('all access DOM references exist; old API-key login is removed',()=>{
 for(const match of script.matchAll(/\$\("([^"]+)"\)/g))assert.match(html,new RegExp('id="'+match[1]+'"'));
 assert.doesNotMatch(html,/id="apiKey"|保存 Key/);
 const core=fs.readFileSync(path.join(root,'internal/server/console/console.js'),'utf8');
 assert.doesNotMatch(core,/localStorage\.setItem\("wb2api_key"|headers\.Authorization\s*=/);
 assert.match(html,/body class="auth-locked"/);
});

test('fresh setup uses local proof then password login; private data waits for session',async()=>{
 const {$,calls,state,counts,document}=app();
 assert.equal(calls.length,1);assert.equal(counts.refresh,0);
 calls[0].resolve({initialized:false,authenticated:false});await tick();
 assert.equal($('setupProofField').hidden,false);assert.equal(document.body.classes.has('auth-locked'),true);
 $('adminUsername').value='admin';$('adminPassword').value='long-password';$('setupProof').value='local-proof';
 const signing=$('adminLoginButton').emit('click');await tick();
 assert.equal(calls[1].url,'/admin/api/auth/setup');assert.equal(JSON.parse(calls[1].opts.body).setup_token,'local-proof');
 calls[1].resolve({ok:true});await tick();assert.equal(calls[2].url,'/admin/api/auth/login');
 calls[2].resolve(session);await tick();calls[3].resolve(emptyKeys);await signing;await tick();
 assert.equal(state.authenticated,true);assert.equal(state.csrf,'csrf-test');assert.equal($('adminPassword').value,'');assert.equal($('setupProof').value,'');
 assert.equal(document.body.classes.has('auth-locked'),false);assert.equal(counts.refresh,1);
});

test('key creation sends selected channels and exposes token only in one-time dialog',async()=>{
 const {$,calls}=app();calls[0].resolve(session);await tick();calls[1].resolve(emptyKeys);await tick();
 await $('keyNew').emit('click');assert.equal($('keyDialog').open,true);
 $('keyName').value='development';$('keyChannels').children[1].checked=true;await $('keyChannels').emit('change');$('keyDefault').value='workbuddy';$('keyRPM').value='30';$('keyEnabled').checked=true;
 const saving=$('keySave').emit('click');await tick();const payload=JSON.parse(calls[2].opts.body);
 assert.deepEqual(payload.channels,['workbuddy']);assert.equal(payload.rpm,30);assert.equal(payload.expires_at,null);
 calls[2].resolve({token:'sk-one-time',key:{id:'1'}});await tick();assert.equal($('issuedKeyDialog').open,true);assert.equal($('issuedKey').value,'sk-one-time');
 calls[3].resolve(emptyKeys);await saving;await $('issuedKeyClose').emit('click');assert.equal($('issuedKey').value,'');
});

test('expired session aborts pending calls and clears password and issued-key fields',async()=>{
 const {$,calls,state,window,document}=app();calls[0].resolve(session);await tick();calls[1].resolve(emptyKeys);await tick();
 let aborted=false;state.adminRequests.add({abort:()=>aborted=true});$('currentAdminPassword').value='secret';$('newAdminPassword').value='secret2';$('issuedKey').value='sk-secret';$('issuedKeyDialog').showModal();
 await window.emit('console-session-expired');
 assert.equal(state.authenticated,false);assert.equal(state.csrf,'');assert.equal(aborted,true);assert.equal($('issuedKey').value,'');assert.equal($('currentAdminPassword').value,'');assert.equal($('newAdminPassword').value,'');assert.equal($('issuedKeyDialog').open,false);assert.equal(document.body.classes.has('auth-locked'),true);
});

test('invalid login stays locked with readable error and retry enabled',async()=>{
 const {$,calls,state}=app();calls[0].resolve({initialized:true,authenticated:false});await tick();
 $('adminUsername').value='admin';$('adminPassword').value='wrong-password';const pending=$('adminLoginButton').emit('click');await tick();calls[1].reject(Error('账号或密码错误'));await pending;
 assert.equal(state.authenticated,false);assert.equal($('authError').textContent,'账号或密码错误');assert.equal($('adminLoginButton').disabled,false);assert.equal(calls.length,2);
});
