const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const root=path.resolve(__dirname,'..'),script=fs.readFileSync(path.join(root,'internal/server/console/settings.js'),'utf8'),html=fs.readFileSync(path.join(root,'internal/server/console.html'),'utf8');
class Element {
 constructor(tag='div',nodes=new Map()){this.tag=tag;this.nodes=nodes;this.children=[];this.dataset={};this.listeners={};this.value='';this.checked=false;this.disabled=false;this.hidden=false;this.open=false;this.textContent='';this.classes=new Set();this.classList={add:(c)=>this.classes.add(c),toggle:(c,on)=>on?this.classes.add(c):this.classes.delete(c)};}
 set id(v){this._id=v;this.nodes.set(v,this)}get id(){return this._id}
 append(...nodes){for(const n of nodes){n.parent=this;this.children.push(n)}}
 replaceChildren(...nodes){this.children=[];this.append(...nodes)}
 addEventListener(name,fn){(this.listeners[name]??=[]).push(fn)}
 async emit(name,event={}){await Promise.all((this.listeners[name]||[]).map(fn=>fn({preventDefault(){},target:this,...event})))}
 setAttribute(name,value){this[name]=value}
 setCustomValidity(value){this.validityMessage=value}
 focus(){this.focused=true}
 checkValidity(){if(this.validityMessage)return false;if(this.required&&!String(this.value).trim())return false;if(this.type==='number'){const n=Number(this.value);return Number.isFinite(n)&&n>=Number(this.min)&&n<=Number(this.max)&&(this.step!=='1'||Number.isInteger(n))}return true}
 reportValidity(){this.reported=true;return this.checkValidity()}
 matches(selector){return selector==='[data-setting-row]'?!!this.dataset.settingRow:this.tag===selector}
 querySelectorAll(selector){return this.children.flatMap(n=>[...(n.matches(selector)?[n]:[]),...n.querySelectorAll(selector)])}
 closest(selector){for(let n=this;n;n=n.parent)if(n.matches(selector))return n;return null}
}
const fixture=()=>({fields:[
 {key:'pool.max_in_flight',group:'账号池',label:'单账号并发',kind:'integer',min:0,max:10000,help:'0 表示不限'},
 {key:'features.prompt_cache_key',group:'兼容性',label:'缓存标识',kind:'boolean',help:'缓存'},
 {key:'schedule.checkin_hours',group:'任务',label:'签到时间',kind:'hours',help:'小时'},
 {key:'upstream.timeout_seconds',group:'请求',label:'超时',kind:'integer',min:1,max:3600,help:'秒'}
 ],values:{'pool.max_in_flight':3,'features.prompt_cache_key':true,'schedule.checkin_hours':[9,21],'upstream.timeout_seconds':120},current:{'pool.max_in_flight':3,'features.prompt_cache_key':true,'schedule.checkin_hours':[9,21],'upstream.timeout_seconds':120},defaults:{'pool.max_in_flight':3,'features.prompt_cache_key':true,'schedule.checkin_hours':[9,21],'upstream.timeout_seconds':120},locked:{'upstream.timeout_seconds':'WB2A_TIMEOUT_SECONDS'},revision:'rev1',pending:[],restart_required:false,timezone:'CST +08:00'});
function app(authenticated=true,storage=new Map()){
 const nodes=new Map(),document={createElement:tag=>new Element(tag,nodes)};
 for(const [,id] of html.matchAll(/\bid="([^"]+)"/g)){const node=new Element('div',nodes);node.id=id}
 const $=id=>{assert.ok(nodes.has(id),'missing DOM '+id);return nodes.get(id)};
 const window=new Element(),state={authenticated,authVersion:1},ui={view:'settings'},calls=[];
 const api=(url,opts)=>new Promise((resolve,reject)=>calls.push({url,opts,resolve,reject}));
 const localStorage={getItem:key=>storage.get(key)??null,setItem:(key,value)=>storage.set(key,value)};
 vm.runInNewContext(script,{$,document,window,state,ui,api,localStorage});
 return {$,document,window,state,ui,calls,nodes,storage};
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const category=(a,id)=>a.$('settingsNav').children.find(button=>button.dataset.settingsCategory===id);
const visibleRows=a=>a.$('settingsGroups').querySelectorAll('[data-setting-row]').filter(row=>!row.hidden);

test('all catalog fields have one control and are reachable in categories',async()=>{
 const a=app(),data=require('./console_settings_fixture.cjs').settingsFixture();a.calls[0].resolve(data);await tick();
 assert.equal(a.$('settingsGroups').querySelectorAll('[data-setting-row]').length,39);
 const visited=[];
 for(const id of ['accounts','requests','automation','logs','compatibility']){await category(a,id).emit('click');visited.push(...visibleRows(a).map(row=>row.dataset.settingRow));}
 assert.equal(visited.length,39);assert.equal(new Set(visited).size,39);
 assert.equal(a.calls.length,1);
});
test('category and global search switches keep drafts; hidden invalid fields are revealed',async()=>{
 const a=await ready();await category(a,'accounts').emit('click');await edit(a,'setting-pool-max_in_flight','7');
 await category(a,'compatibility').emit('click');await edit(a,'setting-features-prompt_cache_key',false);
 await category(a,'changed').emit('click');assert.equal(visibleRows(a).length,2);assert.match(a.$('settingsDirty').textContent,/2 项/);
 a.$('settingsSearch').value='签到';await a.$('settingsSearch').emit('input');assert.deepEqual(visibleRows(a).map(row=>row.dataset.settingRow),['schedule.checkin_hours']);
 await edit(a,'setting-schedule-checkin_hours','9,9');await category(a,'accounts').emit('click');await a.$('settingsForm').emit('submit');
 assert.equal(a.$('settingsCategoryTitle').textContent,'自动任务');assert.equal(a.$('setting-schedule-checkin_hours').focused,true);assert.equal(a.calls.length,1);
 assert.equal(a.$('setting-pool-max_in_flight').value,'7');assert.equal(a.$('setting-features-prompt_cache_key').checked,false);
});
test('favorites persist only field identifiers; an empty common view stays usable',async()=>{
 const a=await ready();await category(a,'compatibility').emit('click');
 const favorite=a.$('setting-features-prompt_cache_key').closest('[data-setting-row]').querySelectorAll('button').find(button=>button.className==='setting-favorite');
 await favorite.emit('click');const saved=JSON.parse(a.storage.get('wb2api.settings.favorites.v1'));
 assert.ok(saved.includes('features.prompt_cache_key'));assert.ok(saved.every(key=>typeof key==='string'&&key.includes('.')));assert.equal(a.calls.length,1);
 const restored=app(true,a.storage);restored.calls[0].resolve(fixture());await tick();assert.ok(visibleRows(restored).some(row=>row.dataset.settingRow==='features.prompt_cache_key'));
 const empty=app(true,new Map([['wb2api.settings.favorites.v1','[]']]));empty.calls[0].resolve(fixture());await tick();assert.equal(visibleRows(empty).length,0);assert.equal(empty.$('settingsEmpty').hidden,false);
 await category(empty,'requests').emit('click');assert.ok(visibleRows(empty).length>0);
});
test('search matches localized options, no-results clears back to the selected category',async()=>{
 const a=app(),data=require('./console_settings_fixture.cjs').settingsFixture();a.calls[0].resolve(data);await tick();
 await category(a,'logs').emit('click');a.$('settingsSearch').value='最低额度';await a.$('settingsSearch').emit('input');
 assert.deepEqual(visibleRows(a).map(row=>row.dataset.settingRow),['pool.selection_mode']);
 a.$('settingsSearch').value='no-such-setting';await a.$('settingsSearch').emit('input');assert.equal(a.$('settingsEmpty').hidden,false);
 await a.$('settingsClearSearch').emit('click');assert.equal(a.$('settingsCategoryTitle').textContent,'日志与用量');assert.equal(visibleRows(a).length,6);
});
test('hot save reports applied version and can apply external pending changes',async()=>{
 const a=app(),data=fixture();data.hot_reload=true;data.applied_version=0;data.pending=['pool.max_in_flight'];data.values['pool.max_in_flight']=7;
 a.calls[0].resolve(data);await tick();assert.equal(a.$('settingsSave').disabled,false);assert.equal(a.$('settingsSave').textContent,'保存并应用');
 const saving=a.$('settingsForm').emit('submit');await tick();const sent=JSON.parse(a.calls[1].opts.body);assert.deepEqual(sent.changes,{'pool.max_in_flight':7});
 const applied={...data,current:{...data.values},pending:[],revision:'rev2',applied_version:1};a.calls[1].resolve(applied);await saving;
 assert.equal(a.$('settingsPending').hidden,true);assert.match(a.$('settingsStatus').textContent,/已保存并生效.*版本 1/);assert.equal(a.$('settingsSave').disabled,true);
});
async function ready(){const a=app();a.calls[0].resolve(fixture());await tick();return a}
async function edit(a,id,value){const el=a.$(id);if(typeof value==='boolean')el.checked=value;else el.value=value;await a.$('settingsGroups').emit('input',{target:el})}

test('settings DOM and script are wired; private config waits for login',async()=>{
 for(const [,id] of script.matchAll(/\$\("([^"]+)"\)/g))assert.match(html,new RegExp('id="'+id+'"'));
 assert.match(html,/\/admin\/assets\/settings.js/);
 const a=app(false);assert.equal(a.calls.length,0);a.state.authenticated=true;a.state.authVersion++;await a.window.emit('console-key-changed');assert.equal(a.calls.length,1);
 a.calls[0].resolve(fixture());await tick();assert.equal(a.$('setting-upstream-timeout_seconds').disabled,true);
 assert.match(a.$('settingsTimezone').textContent,/CST/);
});
test('save patches only changed fields, preserves false/zero, and displays pending restart',async()=>{
 const a=await ready();await edit(a,'setting-pool-max_in_flight','0');await edit(a,'setting-features-prompt_cache_key',false);
 const saving=a.$('settingsForm').emit('submit');await tick();assert.equal(a.calls.length,2);
 const payload=JSON.parse(a.calls[1].opts.body);assert.deepEqual(payload,{revision:'rev1',changes:{'pool.max_in_flight':0,'features.prompt_cache_key':false}});
 assert.equal(a.$('settingsFields').disabled,true);
 const response=fixture();response.values={...response.values,...payload.changes};response.revision='rev2';response.pending=Object.keys(payload.changes);response.restart_required=true;
 a.calls[1].resolve(response);await saving;
 assert.equal(a.$('settingsPending').hidden,false);assert.match(a.$('settingsStatus').textContent,/重启/);assert.equal(a.$('settingsSave').disabled,true);
});
test('invalid hours and non-integer numbers never submit',async()=>{
 const a=await ready();await edit(a,'setting-schedule-checkin_hours','9,9');await a.$('settingsForm').emit('submit');assert.equal(a.calls.length,1);assert.equal(a.$('setting-schedule-checkin_hours').reported,true);
 await edit(a,'setting-schedule-checkin_hours','9,21');await edit(a,'setting-pool-max_in_flight','1.5');await a.$('settingsForm').emit('submit');assert.equal(a.calls.length,1);
 await edit(a,'setting-pool-max_in_flight','3');await edit(a,'setting-schedule-checkin_hours','9,');await a.$('settingsForm').emit('submit');assert.equal(a.calls.length,1);
});
test('conflict preserves edits and requires explicit reload before saving again',async()=>{
 const a=await ready();await edit(a,'setting-pool-max_in_flight','8');const saving=a.$('settingsForm').emit('submit');await tick();
 a.calls[1].reject(Object.assign(Error('配置版本冲突'),{status:409}));await saving;
 assert.equal(a.$('setting-pool-max_in_flight').value,'8');assert.equal(a.$('settingsSave').disabled,true);assert.match(a.$('settingsStatus').textContent,/重新读取/);
 await a.$('settingsForm').emit('submit');assert.equal(a.calls.length,2);
 const reload=a.$('settingsReload').emit('click');await tick();const next=fixture();next.values['pool.max_in_flight']=5;next.revision='new';a.calls[2].resolve(next);await reload;
 assert.equal(a.$('setting-pool-max_in_flight').value,'5');
});
test('failed save retains draft for retry; default fill and discard do not write',async()=>{
 const a=await ready();await edit(a,'setting-pool-max_in_flight','8');const saving=a.$('settingsForm').emit('submit');await tick();a.calls[1].reject(Error('磁盘不可写'));await saving;
 assert.equal(a.$('setting-pool-max_in_flight').value,'8');assert.equal(a.$('settingsSave').disabled,false);
 const row=a.$('setting-pool-max_in_flight').closest('[data-setting-row]');await row.querySelectorAll('button').find(n=>n.className==='setting-reset').emit('click');assert.equal(a.$('setting-pool-max_in_flight').value,'3');assert.equal(a.calls.length,2);
 await edit(a,'setting-pool-max_in_flight','6');await a.$('settingsDiscard').emit('click');assert.equal(a.$('setting-pool-max_in_flight').value,'3');assert.equal(a.calls.length,2);
});
test('logout clears settings and ignores old in-flight load or save responses',async()=>{
 const a=app();a.state.authenticated=false;a.state.authVersion++;await a.window.emit('console-key-changed');a.calls[0].resolve(fixture());await tick();assert.equal(a.$('settingsGroups').children.length,0);
 a.state.authenticated=true;a.state.authVersion++;await a.window.emit('console-key-changed');a.calls[1].resolve(fixture());await tick();
 await edit(a,'setting-pool-max_in_flight','8');const saving=a.$('settingsForm').emit('submit');await tick();
 a.state.authenticated=false;a.state.authVersion++;await a.window.emit('console-key-changed');a.calls[2].resolve(fixture());await saving;
 assert.equal(a.$('settingsGroups').children.length,0);assert.equal(a.$('settingsSave').disabled,true);
});
