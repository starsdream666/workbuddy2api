// Runs an isolated gateway binary with empty upstream accounts; never reads the
// user's config/auths and never submits a model request to an upstream service.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),net=require('node:net');
const {spawn}=require('node:child_process');
const root=path.resolve(__dirname,'..');
const binary=path.resolve(root,process.argv[2]||'Temp/out/server-managed-auth.exe');
const scratch=fs.mkdtempSync(path.join(root,'Temp','access-smoke-'));
const env=Object.fromEntries(Object.entries(process.env).filter(([name])=>!name.startsWith('WB2A_')));
let child,origin;
const pause=ms=>new Promise(resolve=>setTimeout(resolve,ms));
async function stop(){if(child&&child.exitCode===null){await new Promise(resolve=>{child.once('exit',resolve);child.kill();});}}
async function call(method,url,body,headers={}){
 const response=await fetch(origin+url,{method,headers:{'Content-Type':'application/json',...headers},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(5000)});
 const text=await response.text();let data;try{data=JSON.parse(text);}catch{data=text;}
 return {status:response.status,data,cookie:response.headers.get('set-cookie'),headers:response.headers};
}
async function start(config){
 child=spawn(binary,['-config',config],{cwd:root,env,windowsHide:true,stdio:['ignore','ignore','ignore']});
 let spawnError;child.on('error',error=>spawnError=error);
 for(let i=0;i<100;i++){
  if(spawnError)throw spawnError;if(child.exitCode!==null)throw Error('Isolated gateway exited during startup');
  try{const r=await call('GET','/admin/api/auth/session');if(r.status===200)return;}catch{}
  await pause(50);
 }
 throw Error('Isolated gateway did not become ready');
}
(async()=>{
 try{
  const listener=net.createServer();await new Promise(resolve=>listener.listen(0,'127.0.0.1',resolve));const port=listener.address().port;await new Promise(resolve=>listener.close(resolve));
  origin='http://127.0.0.1:'+port;
  const accounts=path.join(scratch,'auths');fs.mkdirSync(accounts);
  const config=path.join(scratch,'config.json'),store=path.join(scratch,'access.json');
  fs.writeFileSync(config,JSON.stringify({listen:'127.0.0.1:'+port,api_key:'legacy-smoke-token',auth_dir:accounts,state_file:path.join(scratch,'state.json'),security:{store_file:store},upstream:{realm:'workbuddy'},realms:{workbuddy:{},codebuddy:{auth_realm:'workbuddy'}},schedule:{checkin_enabled:false,travel_enabled:false,activity_enabled:false,keepalive_enabled:false,credit_watch_enabled:false},usage_log:{enabled:false,file:path.join(scratch,'usage.jsonl')}}));
  await start(config);
  assert.equal((await call('GET','/admin/api/auth/session')).data.initialized,false);
  assert.equal((await call('GET','/admin/api/overview',undefined,{Authorization:'Bearer legacy-smoke-token'})).status,401);
  const proof=fs.readFileSync(store+'.setup-token','utf8').trim(),password='isolated-test-password';
  assert.equal((await call('POST','/admin/api/auth/setup',{username:'smoke-admin',password,setup_token:proof})).status,200);
  const login=await call('POST','/admin/api/auth/login',{username:'smoke-admin',password});assert.equal(login.status,200);
  assert.match(login.cookie,/HttpOnly/i);assert.match(login.cookie,/SameSite=Strict/i);
  const headers={Cookie:login.cookie.split(';')[0],'X-CSRF-Token':login.data.csrf_token};
  const settingsRead=await call('GET','/admin/api/settings',undefined,headers);assert.equal(settingsRead.status,200);
  assert.equal(settingsRead.data.fields.length,39);assert.equal(settingsRead.data.restart_required,false);
  assert.equal((await call('GET','/admin/api/settings')).status,401);
  const settingsPatch={revision:settingsRead.data.revision,changes:{'pool.max_in_flight':7,'server.max_body_mb':1,'features.prompt_cache_key':false}};
  assert.equal((await call('PATCH','/admin/api/settings',settingsPatch,{Cookie:headers.Cookie})).status,403);
  const settingsSaved=await call('PATCH','/admin/api/settings',settingsPatch,headers);assert.equal(settingsSaved.status,200);
  assert.equal(settingsSaved.data.restart_required,true);assert.equal(settingsSaved.data.current['pool.max_in_flight'],3);
  assert.equal(settingsSaved.data.values['pool.max_in_flight'],7);assert.equal(settingsSaved.data.values['features.prompt_cache_key'],false);
  assert.equal((await call('PATCH','/admin/api/settings',settingsPatch,headers)).status,409);
  const invalidSettings={revision:settingsSaved.data.revision,changes:{'security.store_file':'forbidden'}};
  assert.equal((await call('PATCH','/admin/api/settings',invalidSettings,headers)).status,400);
  const persistedSettings=JSON.parse(fs.readFileSync(config,'utf8'));
  assert.equal(persistedSettings.api_key,'legacy-smoke-token');assert.equal(persistedSettings.schedule.checkin_enabled,false);
  assert.equal(persistedSettings.pool.max_in_flight,7);
  const policy={name:'smoke client',channels:['workbuddy'],default_channel:'workbuddy',expires_at:null,rpm:0,enabled:true};
  const creation=await call('POST','/admin/api/keys',policy,headers);assert.equal(creation.status,201);
  const token=creation.data.token,keyID=creation.data.key.id;
  assert.ok(token.startsWith('sk-wb2a-'));
  const apiHeaders={Authorization:'Bearer '+token};
  assert.equal((await call('GET','/v1/models',undefined,apiHeaders)).status,200);
  assert.equal((await call('GET','/admin/api/keys',undefined,apiHeaders)).status,401);
  assert.equal((await call('GET','/admin/api/settings',undefined,apiHeaders)).status,401);
  assert.equal((await call('GET','/v1/models',undefined,{'X-Realm':'codebuddy',...apiHeaders})).status,403);
  assert.equal((await call('POST','/v1/responses',{model:'codebuddy/model',input:'not sent upstream'},apiHeaders)).status,403);
  assert.equal((await call('PATCH','/admin/api/keys/'+keyID,{...policy,enabled:false},headers)).status,200);
  assert.equal((await call('GET','/v1/models',undefined,apiHeaders)).status,401);
  assert.equal((await call('PATCH','/admin/api/keys/'+keyID,policy,headers)).status,200);
  assert.equal((await call('DELETE','/admin/api/keys/'+keyID,undefined,headers)).status,200);
  const list=(await call('GET','/admin/api/keys',undefined,headers)).data.keys;
  assert.ok(!JSON.stringify(list).includes(token));
  const legacy=list.find(k=>k.prefix==='已导入密钥');assert.ok(legacy);
  assert.equal((await call('DELETE','/admin/api/keys/'+legacy.id,undefined,headers)).status,200);
  const disk=fs.readFileSync(store,'utf8');for(const secret of [token,password,'legacy-smoke-token',proof])assert.ok(!disk.includes(secret),'plaintext secret on disk');
  assert.ok(!fs.existsSync(store+'.setup-token'));
  const page=await call('GET','/admin');assert.match(page.data,/账号密码|管理账号登录/);assert.ok(page.headers.get('content-security-policy'));
  assert.equal((await call('GET','/admin/assets/access.js')).status,200);
  await stop();await start(config);
  assert.equal((await call('GET','/admin/api/auth/session',undefined,headers)).data.authenticated,false);
  assert.equal((await call('GET','/v1/models',undefined,apiHeaders)).status,401);
  assert.equal((await call('GET','/v1/models',undefined,{Authorization:'Bearer legacy-smoke-token'})).status,401);
  const relogin=await call('POST','/admin/api/auth/login',{username:'smoke-admin',password});assert.equal(relogin.status,200);
  const reloadedSettings=await call('GET','/admin/api/settings',undefined,{Cookie:relogin.cookie.split(';')[0]});assert.equal(reloadedSettings.status,200);
  assert.equal(reloadedSettings.data.restart_required,false);assert.equal(reloadedSettings.data.current['pool.max_in_flight'],7);
  assert.equal(reloadedSettings.data.current['server.max_body_mb'],1);assert.equal(reloadedSettings.data.current['features.prompt_cache_key'],false);
  console.log('PASS isolated binary: authentication, key lifecycle, channel denial, settings validation/CSRF/conflicts/persistence/restart, and legacy non-resurrection');
 }finally{await stop();}
})().catch(error=>{console.error(error.message);process.exitCode=1;});
