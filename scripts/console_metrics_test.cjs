const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const root=path.resolve(__dirname,'..');
const M=require('../internal/server/console/metrics.js');
const now=Date.parse('2026-09-30T12:00:00Z');
const logs=[{seq:1,model:'Alpha',uid:'user-a',realm:'cn',status:200,time:'2026-09-30T11:30:00Z',duration_ms:100,ttfb_ms:20},{seq:2,model:'Beta',uid:'user-b',realm:'workbuddy',status:429,time:'2026-09-30T10:00:00Z',duration_ms:300},{seq:3,model:'Alpha',status:0,time:'2026-09-29T11:00:00Z',duration_ms:null}];
test('query, status, realm, model and time filters compose without mutating input',()=>{assert.deepEqual(M.filterEntries(logs,{query:'ALPHA',status:'success',realm:'cn',model:'Alpha',hours:1},now),[logs[0]]);assert.equal(M.filterEntries(logs,{status:'failed'},now)[0],logs[1]);assert.equal(M.filterEntries(logs,{status:'unknown'},now)[0],logs[2]);assert.equal(logs.length,3);assert.equal(M.filterEntries(logs,{hours:1},now).length,1);});
test('unknown metrics are not presented as zero or successful',()=>{assert.equal(M.number(null),null);assert.equal(M.aggregate([]).avgDuration,null);const a=M.aggregate(logs);assert.equal(a.success,1);assert.equal(a.failed,1);assert.equal(a.unknown,1);assert.equal(a.successRate,50);assert.equal(a.avgDuration,200);assert.equal(a.avgTTFB,20);assert.equal(M.aggregate([{status:null}]).successRate,null);});
test('timeline maintains counts at inclusive boundaries and ignores bad dates',()=>{const data=[...logs,{status:200,time:new Date(now).toISOString()},{status:500,time:new Date(now-3600000).toISOString()},{time:'bad'},{time:new Date(now+1).toISOString()}];const t=M.timeline(data,1,now);assert.equal(t.buckets.length,24);assert.equal(t.buckets.reduce((s,b)=>s+b.success+b.failed+b.unknown,0),3);assert.equal(t.buckets.at(-1).success,1);assert.equal(t.buckets[0].failed,1);});
test('CSV quotes delimiters and blocks spreadsheet formula injection',()=>{const csv=M.csv([{nickname:'=HYPERLINK("evil")',model:'model,one',uid:'@SUM(1)',credits_used:1.55}]);assert.ok(csv.startsWith('\uFEFF'));assert.ok(csv.includes(`"'=HYPERLINK(""evil"")"`));assert.ok(csv.includes('"model,one"'));assert.ok(csv.includes(`"'@SUM(1)"`));assert.ok(csv.includes('"1.55"'));});
test('every literal DOM reference exists and IDs are unique',()=>{const html=fs.readFileSync(path.join(root,'internal/server/console.html'),'utf8');const js=fs.readFileSync(path.join(root,'internal/server/console/console.js'),'utf8');const ids=[...html.matchAll(/\bid="([^"]+)"/g)].map(m=>m[1]);assert.equal(new Set(ids).size,ids.length,'duplicate IDs');for(const m of js.matchAll(/\$\("([^"]+)"\)/g))assert.ok(ids.includes(m[1]),'missing '+m[1]);new vm.Script(js);assert.ok(!html.includes('<style>'));assert.ok(!html.includes('<script>'));});

test("only 2xx succeeds; redirects fail and zero TTFB is unmeasured",()=>{assert.equal(M.successful({status:302}),false);assert.equal(M.failed({status:302}),true);assert.equal(M.failed({status:0}),false);assert.equal(M.aggregate([{status:200,duration_ms:0,ttfb_ms:0}]).avgDuration,0);assert.equal(M.aggregate([{status:200,ttfb_ms:0}]).avgTTFB,null);assert.deepEqual(M.filterEntries([{status:204},{status:302}],{status:"failed"}),[{status:302}]);});
test('all-history timeline handles hundreds of thousands of records without spread limits',()=>{
  const entries=Array.from({length:180000},(_,i)=>({time:new Date(now-i*1000).toISOString(),status:200}));
  const t=M.timeline(entries,null,now);
  assert.equal(t.buckets.reduce((n,b)=>n+b.success,0),entries.length);
});
test('daily tokens use local calendar days, include leap day and do not double count cache',()=>{
  const now=new Date(2024,11,31,23,59).getTime();
  const entry=(date,extra={})=>({time:date.toISOString(),prompt_tokens:100,completion_tokens:20,total_tokens:120,cached_tokens:80,...extra});
  const data=M.dailyTokens([entry(new Date(2024,1,28,23,59)),entry(new Date(2024,1,29,0,1)),entry(new Date(2024,1,29,12)),entry(new Date(2023,11,31,12)),entry(new Date(2025,0,1)),{time:'bad'}],2024,now);
  assert.equal(data.days.length,366);
  assert.equal(data.days.find(d=>d.date==='2024-02-29').tokens,240);
  assert.equal(data.days.find(d=>d.date==='2024-02-29').cached,160);
  assert.equal(data.total,360);
  assert.equal(data.max,240);
  assert.equal(data.activeDays,2);
  assert.deepEqual(data.years,[2024,2023]);
});
test('empty calendar and future dates have zero heat; token total remains authoritative',()=>{
  const now=new Date(2026,0,1,12).getTime();
  const data=M.dailyTokens([{time:new Date(now).toISOString(),prompt_tokens:12,completion_tokens:3,total_tokens:0}],2026,now);
  assert.equal(data.days.length,365);assert.equal(data.total,0);assert.equal(data.days[0].requests,1);
  assert.equal(data.days[0].future,false);assert.equal(data.days[1].future,true);
  assert.ok(data.days.every(d=>d.level===0));
});
test('calendar ranges start on local Monday and the first day of the month',()=>{
  const now=new Date(2026,9,6,15,32).getTime();
  assert.equal(M.rangeStart('week',now),new Date(2026,9,5).getTime());
  assert.equal(M.rangeStart('month',now),new Date(2026,9,1).getTime());
  assert.equal(M.rangeStart('72',now),now-72*3600000);
  assert.equal(M.rangeStart('all',now),undefined);
  const sunday=new Date(2026,9,11,23,59).getTime();
  assert.equal(M.rangeStart('week',sunday),new Date(2026,9,5).getTime());
});
test('credit charts preserve decimals and distinguish unknown credit from measured zero',()=>{
  const now=new Date(2026,9,6,12).getTime();
  const entries=[{credits_known:true,credits_used:1.55},{credits_known:true,credits_used:0},{credits_known:false,credits_used:40},{credits_known:true,credits_used:null}].map(e=>({...e,status:200,time:new Date(now).toISOString(),total_tokens:20}));
  const data=M.dailyUsage(entries,2026,now,'credits');
  assert.equal(data.total,1.55);assert.equal(data.max,1.55);assert.equal(data.creditsUnknown,2);
  const t=M.timeline(entries,24,now);
  assert.equal(t.buckets.at(-1).credits,1.55);assert.equal(t.buckets.at(-1).tokens,80);assert.equal(t.buckets.at(-1).creditsUnknown,2);
  const futureRange=M.dailyUsage(entries,2026,now,'credits',now+1);
  assert.equal(futureRange.total,0);
});
