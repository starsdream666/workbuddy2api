// Isolated UI preview. Mock data only; never reads config/auths or calls upstream.
const http=require('node:http'), fs=require('node:fs'), path=require('node:path');
const root=path.resolve(__dirname,'..'), port=Number(process.env.CONSOLE_PREVIEW_PORT||41863);
const accounts=Array.from({length:8},(_,i)=>({uid:'preview-account-'+i,nickname:['主力账号','研发工作空间','备用账号','日常开发','测试账号','个人工作区','海外账号','临时停用'][i],realm:i%3===0?'cn':'workbuddy',credits:900-i*70,credits_exact:899.45-i*70,credits_known:true,credits_checked_at:new Date().toISOString(),healthy:i<5,cooling:i===5,cool_remaining_seconds:420,disabled:i===6,disabled_reason:i===6?'凭证需要重新授权':'',manual_disabled:i===7,success_count:320-i*20,err_total:i*2,in_flight:i===0?2:0,
 // 选号设置三件套：预览要能真实演练「排除 / 落位 / 自定义优先级」，
 // 否则本地看不出账号行上的「选号」按钮长什么样（曾因此漏掉整块交互的回归）。
 selection_excluded:i===2||i===5,selection_placement:i===5?'first':'last',selection_priority:i===3?10:0}));
const entries=Array.from({length:86},(_,i)=>({seq:i+1,time:new Date(Date.now()-(86-i)*820000).toISOString(),uid:accounts[i%8].uid,uid8:accounts[i%8].uid.slice(0,8),nickname:accounts[i%8].nickname,realm:accounts[i%8].realm,model:['claude-sonnet-4.6','gpt-5.4','gemini-3.1-pro','claude-opus-4.6','gpt-5.4-mini'][i%5],mode:i%3?'stream':'non-stream',status:i%13===0?429:200,duration_ms:800+(i*937)%9500,ttfb_ms:200+(i*53)%1000,credits_known:i%11!==0,credits_before:800,credits_after:798.45,credits_used:i%11===0?null:1.55,prompt_tokens:1200+i*130,completion_tokens:300+i*35,total_tokens:1500+i*165,cached_tokens:i%3===0?600:0,balance_error:i%11===0?'未获取到余额':'',error:i%13===0?'上游触发请求频率限制':''}));
function totals(rows){return {requests:rows.length,success:rows.filter(e=>e.status>=200&&e.status<300).length,failed:rows.filter(e=>!(e.status>=200&&e.status<300)).length,prompt_tokens:rows.reduce((s,e)=>s+e.prompt_tokens,0),completion_tokens:rows.reduce((s,e)=>s+e.completion_tokens,0),total_tokens:rows.reduce((s,e)=>s+e.total_tokens,0),cached_tokens:rows.reduce((s,e)=>s+e.cached_tokens,0),credits_used:rows.reduce((s,e)=>s+(e.credits_used||0),0),credits_unknown:rows.filter(e=>!e.credits_known&&e.status>=200&&e.status<300).length,credits_failed_unknown:rows.filter(e=>!e.credits_known&&!(e.status>=200&&e.status<300)).length,duration_sum_ms:rows.reduce((s,e)=>s+e.duration_ms,0),timed_count:rows.length,ttfb_sum_ms:rows.reduce((s,e)=>s+(e.ttfb_ms>0?e.ttfb_ms:0),0),ttfb_count:rows.filter(e=>e.ttfb_ms>0).length};}
function summary(rows){return {...totals(rows),first_time:rows[0]?.time,last_time:rows.at(-1)?.time,...Object.fromEntries(['uid','model','realm'].map(key=>['by_'+key,[...new Set(rows.map(e=>e[key]))].map(value=>({key:value,totals:totals(rows.filter(e=>e[key]===value))}))]))};}
const historyEntries=Array.from({length:650},(_,i)=>({...entries[i%entries.length],seq:i+1,time:new Date(Date.now()-(650-i)*43200000).toISOString()}));
const tasks=[];
const previewKeys=[];
// All settings are represented; edits remain in this process's memory.
const previewSettings=require('./console_settings_fixture.cjs').settingsFixture();
const server=http.createServer((req,res)=>{
 const securitySource=fs.readFileSync(path.join(root,'internal/server/security.go'),'utf8');
 res.setHeader('Content-Security-Policy',securitySource.match(/header\.Set\("Content-Security-Policy", "([^"]+)"\)/)[1]);
 const url=new URL(req.url,'http://localhost'), scenario=(req.headers.cookie||'').match(/preview=(\w+)/)?.[1]||'normal';
 const json=(value,status=200)=>{res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(value));};
 if(url.pathname==='/admin'||url.pathname==='/admin/') {const chosen=url.searchParams.get('preview')||'normal';res.setHeader('Set-Cookie','preview='+chosen+'; SameSite=Strict; Path=/');res.setHeader('Content-Type','text/html; charset=utf-8');return res.end(fs.readFileSync(path.join(root,'internal/server/console.html'),'utf8').replace('<main class="wrap"','<div style="padding:7px 20px;background:#fff8df;color:#9a772b;font-size:11px;text-align:center">本地预览 · 全部为模拟数据 · 不连接真实网关</div><main class="wrap"'));}
 const assets={'/admin/assets/console.css':['console.css','text/css'],'/admin/assets/metrics.js':['metrics.js','text/javascript'],'/admin/assets/console.js':['console.js','text/javascript'],'/admin/assets/tasks.js':['tasks.js','text/javascript'],'/admin/assets/models.js':['models.js','text/javascript'],'/admin/assets/packages.js':['packages.js','text/javascript'],'/admin/assets/access.js':['access.js','text/javascript']};
 assets['/admin/assets/settings.js']=['settings.js','text/javascript'];
 assets['/admin/assets/theme.js']=['theme.js','text/javascript'];
 if(assets[url.pathname]){const [name,type]=assets[url.pathname];res.setHeader('Content-Type',type);res.setHeader('Cache-Control','no-store');return res.end(fs.readFileSync(path.join(root,'internal/server/console',name)));}
 if(url.pathname==='/admin/api/auth/session')return json({initialized:true,authenticated:scenario!=='unauthorized',username:'预览管理员',csrf_token:'preview-csrf'});
 if(url.pathname==='/admin/api/auth/login')return json({authenticated:true,username:'预览管理员',csrf_token:'preview-csrf'});
 if(url.pathname==='/admin/api/auth/logout'||url.pathname==='/admin/api/auth/password')return json({ok:true});
 if(scenario==='unauthorized')return json({error:'请先登录管理账号'},401);
 if(url.pathname==='/admin/api/settings'){
  if(scenario==='error')return json({error:'模拟配置读取失败'},503);
  if(req.method==='GET')return json(previewSettings);
  if(req.method!=='PATCH')return json({error:'method not allowed'},405);
  let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{try{
   const input=JSON.parse(body);if(input.revision!==previewSettings.revision)return json({error:'配置已修改，请重新读取'},409);
   if(Object.keys(input.changes||{}).some(key=>!Object.hasOwn(previewSettings.values,key)))return json({error:'未知配置'},400);
   Object.assign(previewSettings.values,input.changes);previewSettings.revision='preview-'+Date.now();
   previewSettings.current=structuredClone(previewSettings.values);previewSettings.pending=[];previewSettings.applied_version++;
   previewSettings.restart_required=false;json(previewSettings);
  }catch{json({error:'invalid JSON'},400)}});return;
 }
 if(url.pathname==='/admin/api/keys'&&req.method==='GET')return json({keys:previewKeys,channels:['cn','workbuddy','codebuddy'],active_channels:['cn','workbuddy','codebuddy']});
 if(url.pathname==='/admin/api/keys'&&req.method==='POST'){let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{try{const key={...JSON.parse(body),id:'preview-'+Date.now(),prefix:'sk-preview…only',created_at:new Date().toISOString(),revoked_at:null};previewKeys.push(key);json({key,token:'sk-preview-only-not-a-real-key'},201);}catch{json({error:'invalid JSON'},400);}});return;}
 if(url.pathname.startsWith('/admin/api/keys/')){const key=previewKeys.find(k=>k.id===url.pathname.split('/').at(-1));if(!key)return json({error:'not found'},404);if(req.method==='DELETE'){key.revoked_at=new Date().toISOString();key.enabled=false;return json({ok:true});}let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{try{Object.assign(key,JSON.parse(body));json({key});}catch{json({error:'invalid JSON'},400);}});return;}
 if(url.pathname==='/admin/api/models')return json({object:'list',count:6,data:[
  {id:'claude-sonnet-4.6',name:'Claude Sonnet 4.6',vendor:'Anthropic',realm:'workbuddy',kind:'chat',rate:1,credits:'x1 credits',context_length:200000,max_output_tokens:64000},
  {id:'claude-opus-4.6',name:'Claude Opus 4.6',vendor:'Anthropic',realm:'workbuddy',kind:'chat',rate:5,credits:'x5 credits',context_length:200000,max_output_tokens:32000},
  {id:'gpt-5.4',name:'GPT-5.4',vendor:'OpenAI',realm:'cn',kind:'chat',rate:0.34,credits:'x0.34 credits',context_length:400000,max_output_tokens:128000},
  {id:'gpt-5.4-mini',name:'GPT-5.4 mini',vendor:'OpenAI',realm:'cn',kind:'chat',rate:0,credits:'x0 credits',context_length:400000,max_output_tokens:64000},
  {id:'gemini-3.1-pro',name:'Gemini 3.1 Pro',vendor:'Google',realm:'workbuddy',kind:'chat',rate:1.2,credits:'x1.2 credits',context_length:1000000,max_output_tokens:64000},
  {id:'text-to-image-v3',name:'文生图 v3',vendor:'Tencent',realm:'workbuddy',kind:'image',rate:0.5,credits:'x0.5 credits'}
 ]});
 if(url.pathname==='/admin/api/account-packages') {
  if(scenario==='error')return json({error:'模拟套餐查询失败，请稍后重试'},502);
  const uid=url.searchParams.get('uid'),realm=url.searchParams.get('realm');
  if(!accounts.some(a=>a.uid===uid&&a.realm===realm))return json({error:'凭证不存在'},404);
  const common={product:'Tencent Cloud CodeBuddy（IDE）',unit:'credits',status:0,basis:'cycle',precise:true,cycle_start:'2026-10-01 00:00:00',cycle_end:'2026-10-31 23:59:59',usable_from:'2026-09-13T02:00:00Z'};
  const packages=scenario==='packageempty'?[]:[
   {...common,name:'Free Plan Subscription',total:'100',used:'100',remaining:'0',usable_until:'2034-12-13T00:00:00Z'},
   {...common,name:'Bonus Pack',total:'30',used:'13.51',remaining:'16.49',usable_until:'2026-10-12T23:59:59Z'},
   {...common,name:'Bonus Pack',total:'30',used:'0',remaining:'30',usable_until:'2026-11-05T23:59:59Z'}
  ];
  return json({uid,realm,checked_at:new Date().toISOString(),count:packages.length,totals:packages.length?[{unit:'credits',total:'160',used:'113.51',remaining:'46.49'}]:[],packages});
 }
 if(url.pathname==='/admin/api/tasks') {
  if(req.method==='GET')return json({enabled:true,realms:['cn','workbuddy'],tasks});
  let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{
   const input=JSON.parse(body),existing=tasks.find(task=>task.realm===input.realm&&task.kind===input.kind&&task.status==='queued');
   if(existing)return json({task:existing,deduplicated:true});
   const task={id:String(tasks.length+1),realm:input.realm,kind:input.kind,status:'queued',created_at:new Date().toISOString(),progress:{total:8,succeeded:0,failed:0,skipped:0}};
   tasks.unshift(task);json({task,deduplicated:false},202);
  });return;
 }
 if(url.pathname.startsWith('/admin/api/tasks/')&&req.method==='DELETE'){const task=tasks.find(task=>task.id===url.pathname.split('/').at(-1));if(!task)return json({error:'not found'},404);task.status='cancelled';return json(task);}
 if(url.pathname==='/admin/api/overview'){const a=scenario==='empty'?[]:accounts;return json({now:new Date().toISOString(),accounts:a,realms:['cn','workbuddy'],routes:{codebuddy:'workbuddy'},default_realm:'workbuddy',auth_dir:'./auths',totals:{accounts:a.length,healthy:a.filter(a=>!a.disabled&&!a.manual_disabled&&!a.cooling).length,cooling:a.filter(a=>a.cooling).length,disabled:a.filter(a=>a.disabled).length,manual_disabled:a.filter(a=>a.manual_disabled).length,credits:a.reduce((s,a)=>s+a.credits_exact,0)}});}
 if(url.pathname==='/admin/api/usage') {if(scenario==='error')return json({error:'模拟用量读取失败'},503);if(req.method==='DELETE'){entries.length=0;return json({ok:true,removed:356200,enabled:true,file:"./data/usage.jsonl",summary:summary([]),size:{total_bytes:0,limit_bytes:67108864,file_bytes:0,backup_bytes:0,max_backups:3,memory_entries:0}});}const rows=scenario==='empty'?[]:scenario==='history'?historyEntries:entries;return json({enabled:scenario!=='disabled',file:scenario==='memory'?'':'./data/usage.jsonl',total:rows.length,kept:rows.length,entries:rows,summary:summary(rows),size:{total_bytes:356200,limit_bytes:67108864,file_bytes:356200,backup_bytes:0,max_backups:3,memory_entries:rows.length}});}
 if(url.pathname==='/admin/api/balance')return json({updated:{},failed:{}});
 if(url.pathname==='/admin/api/reload')return json({ok:true,accounts:{workbuddy:5,cn:3}});
 if(url.pathname==='/admin/api/events'){res.writeHead(200,{'Content-Type':'text/event-stream','Cache-Control':'no-cache'});res.write(': preview connected\n\n');return;}
 if(url.pathname==='/admin/api/accounts'){let body='';req.on('data',v=>body+=v);req.on('end',()=>{try{const d=JSON.parse(body),i=accounts.findIndex(a=>a.uid===d.uid);if(i>=0){if(req.method==='DELETE')accounts.splice(i,1);else if(d.selection){ // 选号设置：与真实服务端同语义（未排除时清掉落位，优先级始终保留）
  accounts[i].selection_excluded=!!d.selection.excluded;accounts[i].selection_priority=d.selection.priority||0;accounts[i].selection_placement=d.selection.excluded?(d.selection.placement||'last'):'last';}
 else {accounts[i].manual_disabled=!d.enabled;accounts[i].healthy=d.enabled;}}json({ok:true,...d,selection:d.selection});}catch{json({error:'invalid json'},400);}});return;}
 if(url.pathname==='/admin/api/login/start')return json({error:'预览模式不会创建真实授权，请在运行中的网关使用此功能'},400);
 json({error:'not found'},404);
});
server.listen(port,'127.0.0.1',()=>console.log('Mock console preview: http://127.0.0.1:'+server.address().port+'/admin'));
