// Local UI fixture: public field metadata + synthetic values, never live config.
const fs = require('node:fs'), path = require('node:path');
function settingsFixture() {
  const source = fs.readFileSync(path.join(__dirname,'../internal/settings/catalog.go'),'utf8');
  const quoted = '"(?:[^"\\\\]|\\\\.)*"';
  const pattern = new RegExp('^\\s*\\{('+quoted+'), ('+quoted+'), ('+quoted+'), ('+quoted+'), ('+quoted+'), ([\\d.]+), ([\\d.]+), (nil|\\[\\]string\\{[^}]*\\}), ('+quoted+')\\},?$', 'gm');
  const fields = [...source.matchAll(pattern)].map(m => ({key:JSON.parse(m[1]),group:JSON.parse(m[2]),label:JSON.parse(m[3]),help:JSON.parse(m[4]),kind:JSON.parse(m[5]),min:Number(m[6]),max:Number(m[7]),options:m[8]==='nil'?null:JSON.parse('['+m[8].slice(9,-1)+']'),env:JSON.parse(m[9])}));
  const mock = {
    server:{max_body_mb:8}, upstream:{timeout_seconds:120,header_timeout_seconds:0,idle_timeout_seconds:300},
    pool:{selection_mode:'custom_priority',max_in_flight:3,credit_floor:0,idle_weight_per_hour:0.5,idle_weight_max:6,breaker_threshold:5,breaker_cooldown:'30m',breaker_cooldown_max:'6h'},
    cooldown:{soft_rate:'10m',soft_rate_max:'2h'}, session_sticky:{enabled:true,ttl:'30m',gc_interval:'5m'},
    features:{sanitize_blacklist_fingerprints:true,prompt_cache_key:true,repair_tool_history:false},
    schedule:{checkin_enabled:true,checkin_hours:[9,21],travel_enabled:true,travel_hours:[9,21],activity_enabled:true,activity_hours:[9,21],activity_report_count:5,keepalive_enabled:true,keepalive_hours:[3,15],credit_watch_enabled:true,credit_watch_interval:'30m',credit_watch_scope:'frozen',credit_freeze_max:'72h'},
    usage_log:{enabled:true,memory_size:1000,max_size_mb:64,max_backups:3,refresh_balance:true,calibrate_interval:'5m'}
  };
  const values = Object.fromEntries(fields.map(field => {const [group,key]=field.key.split('.');if(!Object.hasOwn(mock[group]||{},key))throw Error('Missing mock value: '+field.key);return [field.key,mock[group][key]];}));
  if(fields.length!==39)throw Error('Update preview coverage for the changed catalog');
  return {fields,values,current:structuredClone(values),defaults:structuredClone(values),
    labels:{'pool.selection_mode':{weighted:'随机调用（打散热点，各号均衡分配）',lowest_credits:'最低额度优先（集中打光一个号再换下一个）',highest_credits:'最高额度优先（先吃厚号，低额度号留后）',custom_priority:'自定义优先级（按账号优先级排序）'},'schedule.credit_watch_scope':{frozen:'仅冻结账号',all:'全部账号'}},
    locked:{},revision:'preview-0',pending:[],restart_required:false,hot_reload:true,applied_version:0,timezone:'模拟时区 +08:00'};
}
module.exports={settingsFixture};
