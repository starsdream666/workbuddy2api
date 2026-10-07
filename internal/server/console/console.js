
const $ = (id) => document.getElementById(id);
const state = {
  authenticated: false, csrf: "", authVersion: 0, adminRequests: new Set(), accounts: [], overview: null, overviewVersion: 0, refreshBusy: false, balanceBusy: false,
  // overviewRaw 上次**已渲染**数据的原始 JSON：静默轮询据此判断"数据没变就别重绘"
  // （renderAccounts 走 innerHTML 整体重建，无脑重绘会重置横向滚动、踢走键盘焦点）。
  overviewRaw: null,
  creditsTimer: null, pollTimer: null, loginVersion: 0, loginUrl: "", copyTimer: null, accountsTimer: null,
  deleteTarget: null, deleteBusy: false, deleteTrigger: null, toggleBusy: false,
  // 选号设置对话框态：目标账号 / 在途标志 / 触发按钮（关闭后归还焦点）。
  selectionTarget: null, selectionBusy: false, selectionTrigger: null,
  // uidBusy 单号刷新在途的 uid 集合：按钮各自转圈，互不影响。
  uidBusy: new Set(),
  usage: null, usageError: "", usageBusy: false, usageVersion: 0, usageTimer: null,
  usageClearing: false, usageNotice: "", usageNoticeTimer: null,
  // 分维度明细的本地 UI 态：当前页签 / 排序键 / 排序方向 / 是否展开全部行。
  // 只放页面状态，数据永远从 state.usage.summary 现取——避免两份数据源不一致。
  usageTab: "by_uid", usageSort: "credits", usageSortDir: "desc", usageShowAll: false,
  // 单条日志流水的分页态：每页条数 + 当前页（1 起）。
  // 只放页面状态——数据永远从 state.usage.entries 现取，条数上限由后端 limit 决定。
  // pageSize 是**用户意图**（可能是下拉值来的数字，也可能是自定义输入的原值），
  // 渲染时统一经 usagePageSize() 归一化，避免把"未填的自定义值"写死进状态。
  usagePageSize: "all", usagePage: 1, usagePageCustom: "",
  // accountsAuto 自动刷新总开关（与 #accountsAuto 复选框同步，供多处读取）。
  accountsAuto: { checked: true },
  // usageAuto 用量面板的实时更新开关（与 #usageAuto 同步）：关掉只保留当前快照，不应用推送。
  usageAuto: { checked: true },
  // models 模型列表页快照（GET /admin/api/models）；null = 尚未加载。
  models: null,
  // overviewAt 最近一次已渲染的 overview 的服务端时间戳（毫秒）：用于丢弃乱序到达的旧快照。
  overviewAt: 0,
  // SSE 运行态：连接句柄（用于主动关闭）、重连定时器、退避时长、状态文案。
  eventsAbort: null, eventsTimer: null, eventsBackoff: 1000, eventsHint: ""
};


// View state is separate from server snapshots; changing a filter never changes cumulative totals.
const ui = { view: "overview", trend: "24", heatmapRange: "all", chartMode: "bars", metric: "requests", heatmapYear: new Date().getFullYear(), filters: {}, request: null, requestTrigger: null };
const views = {
  overview: ["网关总览", "WORKSPACE OVERVIEW", "每一次调用，每一份用量，尽在掌握。", "总览"],
  credentials: ["账号管理", "ACCOUNT MANAGEMENT", "统一管理凭证、可用额度与账号健康状态。", "账号管理"],
  analysis: ["使用分析", "USAGE ANALYTICS", "从账号到模型，了解每一份用量的去向。", "使用分析"],
  usage: ["请求日志", "REQUEST EXPLORER", "搜索、筛选与查看详情，快速找到你关心的调用。", "请求日志"],
  tasks: ["任务中心", "MAINTENANCE TASKS", "提交手动运维任务，查看执行进度与结果。", "任务中心"],
  models: ["模型列表", "MODEL CATALOG", "查看全部可用模型及其上游倍率。", "模型列表"],
  settings: ["系统设置", "WORKSPACE SETTINGS", "按分类查找配置，常用项随手可达。", "系统设置"],
  account: ["管理账号", "ADMINISTRATOR ACCOUNT", "管理你的登录账号与控制台安全。", "管理账号"],
  connect: ["快速接入", "CLIENT CONNECTION", "获取服务地址，连接你常用的 AI 客户端。", "快速接入"],
  keys: ["访问密钥", "ACCESS KEYS", "创建与管理客户端 Key，控制渠道和调用权限。", "访问密钥"]
};
function switchView(focus = false) {
  const hash = location.hash.slice(1);
  if (hash === "mainContent") { $("mainContent").focus(); return; }
  const key = hash === "authorization" ? "credentials" : Object.hasOwn(views, hash) ? hash : "overview";
  ui.view = key;
  document.querySelectorAll("[data-page]").forEach(el => el.hidden = el.dataset.page !== key);
  document.querySelectorAll("[data-view]").forEach(el => { if (el.dataset.view === key) el.setAttribute("aria-current", "page"); else el.removeAttribute("aria-current"); });
  const [title, eyebrow, description, crumb] = views[key];
  $("pageTitle").textContent = title; $("pageEyebrow").textContent = eyebrow; $("pageSubtitle").textContent = description; $("breadcrumbView").textContent = crumb;
  document.title = crumb + " · WorkBuddy2API";
  if (focus) { $("mainContent").focus({preventScroll:true}); window.scrollTo({top:0,behavior:"instant"}); }
  if (hash === "authorization") { $("authorization").open = true; $("loginRealm").focus(); }
}
function compact(value) {
  const n=ConsoleMetrics.number(value);
  if(n === null) return "—";
  return new Intl.NumberFormat("en-US", {maximumFractionDigits:n>=10000?1:2, notation:n>=10000?"compact":"standard"}).format(n);
}
function filteredLogs() {
  return ConsoleMetrics.filterEntries(Array.isArray(state.usage?.entries) ? state.usage.entries : [],{...ui.filters,from:ConsoleMetrics.rangeStart(ui.filters.range||"all")}).slice().reverse();
}
function syncLogOptions() {
  const entries = state.usage?.entries || [];
  for(const [id,key,label] of [["logRealm","realm","全部产品线"],["logModel","model","全部模型"]]) {
    const select=$(id), current=select.value;
    const values=[...new Set(entries.map(e=>String(e[key]||"")).filter(Boolean))].sort();
    if(current && !values.includes(current)) values.push(current);
    const markup='<option value="">'+label+'</option>'+values.map(v=>'<option value="'+esc(v)+'">'+esc(v)+'</option>').join("");
    if(select.innerHTML !== markup) { select.innerHTML=markup; select.value=current; }
  }
}
function updateLogFilters() {
  ui.filters={query:$("logSearch").value,status:$("logStatus").value,realm:$("logRealm").value,model:$("logModel").value,range:$("logHours").value||"all"};
  state.usagePage=1; renderUsage();
}
function renderDashboard() {
  if(!$("dashRequests")) return;
  const d=state.usage, summary=usageSummaryOf(d), valid=!!summary && !usageUnavailableReason(d);
  const totals=state.overview?.totals;
  $("dashboardUsageError").hidden=!state.usageError;
  $("dashboardUsageError").textContent=state.usageError ? "使用数据读取失败："+state.usageError+(d?"。当前显示上次成功同步的快照。":"。请检查连接设置后重试。") : "";
  $("dashRequests").textContent=valid?compact(summary.requests):"—";
  $("dashTokens").textContent=valid?compact(summary.total_tokens):"—";
  const success=Number(summary?.success)||0, failed=Number(summary?.failed)||0;
  $("dashSuccess").textContent=valid && success+failed ? (success/(success+failed)*100).toFixed(1)+"%":"—";
  $("dashCredits").textContent=totals?compact(totals.credits):"—";
  $("dashRequestsNote").innerHTML=valid?'<span class="s-ok">'+esc(compact(success))+' 次成功</span><span> · '+esc(compact(failed))+' 次失败</span>':"等待可用的累计统计";
  $("dashTokensNote").textContent=valid?"输入 "+compact(summary.prompt_tokens)+" / 输出 "+compact(summary.completion_tokens):"输入与输出总量";
  $("dashSuccessNote").textContent=valid?"累计请求 · "+compact(success+failed)+" 次 · 2xx 为成功":"暂无可用的状态统计";
  const unknownCredits=state.accounts.filter(a=>!a.credits_known).length;
  $("dashCreditsNote").textContent=totals?(unknownCredits?unknownCredits+" 个账号余额未观测 · 已知额度合计":"credits · "+totals.accounts+" 个账号（含停用）"):"等待账号池数据";
  $("dashboardScope").textContent=$("usageStatsRange").textContent || "等待累计统计";
  renderHealth(totals);
  renderTrend();
  renderRecent();
  const groups=valid?usageGroupsOf(summary,"by_model").slice().sort((a,b)=>(b.totals?.requests||0)-(a.totals?.requests||0)).slice(0,5):[];
  const max=Math.max(1,...groups.map(g=>Number(g.totals?.requests)||0));
  $("modelRanking").innerHTML=groups.length?groups.map((g,i)=>'<div class="model-row"><div class="model-row-head"><span class="rank-number">'+String(i+1).padStart(2,"0")+'</span><b title="'+esc(g.key)+'">'+esc(g.key||"未知模型")+'</b><span class="model-number">'+esc(compact(g.totals?.requests))+' 次</span></div><div class="model-track"><span style="width:'+Math.max(0,(Number(g.totals?.requests)||0)/max*100)+'%"></span></div></div>').join(""):'<div class="empty"><strong>'+(valid?'暂无模型调用':'累计统计不可用')+'</strong>'+esc(valid?'产生请求后，可在这里比较模型使用量。':usageUnavailableReason(d)||'等待统计数据')+'</div>';
  const recentErrors=(d?.entries||[]).filter(ConsoleMetrics.failed).length;
  $("navErrors").hidden=!recentErrors; $("navErrors").textContent=recentErrors; $("navErrors").title="保留日志中的失败请求数";
}
function renderHealth(totals) {
  const count=totals?.accounts||0, healthy=totals?.healthy||0, ratio=count?healthy/count*100:0;
  $("healthRatio").textContent=totals&&count?Math.round(ratio)+"%":"—";
  $("healthTotal").textContent=totals?count:"—";
  $("healthRing").style.background="conic-gradient(#33b189 "+ratio+"%,#eef3f1 "+ratio+"%)";
  const items=[["健康可用",healthy,"#39b38b"],["冷却 / 冻结",(totals?.cooling||0)+(totals?.frozen||0),"#d6ad64"],["系统禁用",totals?.disabled||0,"#d88189"],["手动停用",totals?.manual_disabled||0,"#b2bec9"]];
  $("healthList").innerHTML=items.map(([name,n,color])=>'<div class="health-item"><i style="--color:'+color+'"></i><span>'+name+'</span><b>'+(totals?n:"—")+'</b></div>').join("");
}
function renderTrend() {
  const heatmap = ui.chartMode === "heatmap", metric = ui.metric;
  const metricLabel = {requests:"请求量",tokens:"Token 消耗",credits:"积分消耗"}[metric];
  const unit = {requests:"次请求",tokens:"tokens",credits:"积分"}[metric];
  document.querySelectorAll("[data-chart-mode]").forEach(btn=>btn.setAttribute("aria-pressed",String(btn.dataset.chartMode===ui.chartMode)));
  $("trendTitle").textContent = heatmap ? "每日"+metricLabel : metricLabel+"趋势";
  $("trendUnit").textContent = unit;
  $("chartMetric").value=metric;
  $("statsRange").value=heatmap?ui.heatmapRange:ui.trend;
  $("requestLegend").hidden = heatmap || metric!=="requests";
  $("requestFooter").hidden = heatmap;
  $("chartCoverage").hidden = true;
  for (const id of ["heatmapYearControl","heatmapLegend","heatmapFooter"]) $(id).hidden = !heatmap;
  $("chartTooltip").hidden = true;
  $("trendChart").classList.toggle("heatmap-chart",heatmap);
  if (heatmap) { renderHeatmap(); return; }
  const all=state.usage?.entries||[], now=Date.now(), rangeFrom=ConsoleMetrics.rangeStart(ui.trend,now);
  const entries=ConsoleMetrics.filterEntries(all,{from:rangeFrom},now), a=ConsoleMetrics.aggregate(entries);
  const amount=entries.reduce((sum,e)=>sum+(ConsoleMetrics.metricValue(e,metric)||0),0);
  $("trendRequests").textContent=state.usage?compact(amount):"—";
  $("trendRequests").title=usageCount(amount)+" "+unit;
  $("trendDuration").textContent=usageDuration(a.avgDuration); $("trendTTFB").textContent=usageMillis(a.avgTTFB);
  $("trendScope").textContent=historyScope();
  renderCreditsCoverage(entries.filter(e=>ConsoleMetrics.successful(e)&&ConsoleMetrics.metricValue(e,"credits")===null).length);
  if(!entries.length){$("trendChart").innerHTML='<div class="empty"><strong>'+(state.usage?'此时间范围暂无请求':'等待请求数据')+'</strong></div>';return;}
  const {buckets,from,to}=ConsoleMetrics.timeline(entries,null,now,ui.trend==="1"?12:24,rangeFrom);
  const bucketValue=b=>metric==="requests"?b.success+b.failed+b.unknown:b[metric];
  const peak=buckets.reduce((n,b)=>Math.max(n,bucketValue(b)),metric==="credits"?.01:1);
  const magnitude=10**Math.floor(Math.log10(Math.max(metric==="credits"?.0025:1,peak/4)));
  const tick=[1,2,5,10].map(n=>n*magnitude).find(n=>n>=peak/4), max=tick*4;
  const top=26,height=142,left=52,width=540,step=width/buckets.length;
  const clock=t=>new Date(t).toLocaleTimeString("zh-CN",{hour:"2-digit",minute:"2-digit",hour12:false});
  const date=t=>new Date(t).toLocaleDateString("zh-CN",{month:"2-digit",day:"2-digit",...(new Date(from).getFullYear()!==new Date(to).getFullYear()?{year:"numeric"}:{})});
  const stamp=t=>new Date(t).toLocaleString("zh-CN",{year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hour12:false});
  const axisValue=value=>metric==="credits"&&max<1?value.toLocaleString("zh-CN",{maximumFractionDigits:4}):compact(value);
  let content='<text x="52" y="14" fill="#61736b" font-size="11">'+metricLabel+'</text>';
  content+=[0,.25,.5,.75,1].map(f=>'<line x1="52" x2="592" y1="'+(top+height*(1-f))+'" y2="'+(top+height*(1-f))+'" stroke="#dfe7e3" stroke-dasharray="3 4"/><text x="43" y="'+(top+height*(1-f)+4)+'" text-anchor="end" fill="#61736b" font-size="12">'+axisValue(max*f)+'</text>').join("");
  buckets.forEach((b,i)=>{
    let y=top+height;
    const sum=b.success+b.failed+b.unknown,end=i===buckets.length-1?to:buckets[i+1].time;
    const detail=stamp(b.time)+' 至 '+stamp(end)+'\n'+sum+' 次请求 · '+usageCount(b.tokens)+' tokens\n'+usageCount(b.credits)+' 积分'+(b.creditsUnknown?' · '+b.creditsUnknown+' 次成功请求积分未记录':'')+'\n成功 '+b.success+' · 失败 '+b.failed+' · 未知 '+b.unknown;
    content+='<g tabindex="0" role="img" aria-label="'+esc(detail)+'" data-chart-detail="'+esc(detail)+'">';
    const series=metric==="requests"?[["success","#329a74"],["failed","#ce5668"],["unknown","#8795a2"]]:[[metric,metric==="credits"?"#b48532":"#328cad"]];
    for(const [key,color] of series){const h=Math.max(0,b[key]/max*height);y-=h;content+='<rect x="'+(left+i*step+3)+'" y="'+y+'" width="'+(step-6)+'" height="'+h+'" rx="2" fill="'+color+'"/>';}
    if(bucketValue(b)>0&&i%Math.ceil(buckets.length/8)===0)content+='<text class="bar-count" x="'+(left+(i+.5)*step)+'" y="'+(y-5)+'" text-anchor="middle" fill="#42594f" font-size="10">'+compact(bucketValue(b))+'</text>';
    content+='<rect class="chart-hit" x="'+(left+i*step)+'" y="'+top+'" width="'+step+'" height="'+height+'" fill="transparent"/></g>';
  });
  const longRange=to-from>48*3600000;
  content+=[0,.25,.5,.75,1].map(f=>{const x=left+width*f,t=from+(to-from)*f,anchor=f===0?'start':f===1?'end':'middle';return '<text x="'+x+'" y="191" text-anchor="'+anchor+'" fill="#53685e" font-size="12">'+(longRange?date(t):clock(t))+'</text>'+(longRange?'':'<text x="'+x+'" y="209" text-anchor="'+anchor+'" fill="#74857c" font-size="11">'+date(t)+'</text>');}).join("");
  $("trendChart").innerHTML='<svg viewBox="0 0 616 222" role="group" aria-label="'+esc(metricLabel+'趋势：'+usageCount(amount)+' '+unit)+'">'+content+'</svg>';
}
function renderCreditsCoverage(unknown) {
  $("chartCoverage").hidden=ui.metric!=="credits" || !unknown;
  $("chartCoverage").textContent=unknown+" 次成功请求的积分未记录，当前为已知消耗合计。";
}
function historyScope() {
  const d=state.usage;
  if(!d)return state.usageError?"日志读取失败":"正在加载日志";
  return (d.history_incomplete?"部分日志读取失败":d.file?"全部保留日志":"内存保留日志")+" · "+usageCount(d.entries?.length||0)+" 条";
}
function renderHeatmap() {
  const data=ConsoleMetrics.dailyUsage(state.usage?.entries||[],ui.heatmapYear,Date.now(),ui.metric,ConsoleMetrics.rangeStart(ui.heatmapRange));
  const unit={requests:"次请求",tokens:"tokens",credits:"积分"}[ui.metric];
  const options=[...new Set([...data.years,ui.heatmapYear])].sort((a,b)=>b-a).map(year=>'<option value="'+year+'">'+year+' 年</option>').join("");
  if($("heatmapYear").innerHTML!==options)$("heatmapYear").innerHTML=options;
  $("heatmapYear").value=String(ui.heatmapYear);
  $("heatmapYear").disabled=ui.heatmapRange!=="all";
  $("trendScope").textContent=historyScope()+" · "+ui.heatmapYear+" 年";
  $("trendRequests").textContent=state.usage?compact(data.total):"—";
  $("trendRequests").title=usageCount(data.total)+" "+unit;
  $("heatmapActive").textContent=state.usage?data.activeDays:"—";
  $("heatmapPeak").textContent=state.usage?compact(data.max)+" "+unit:"—";
  renderCreditsCoverage(data.creditsUnknown);
  $("heatmapTimezone").textContent=Intl.DateTimeFormat().resolvedOptions().timeZone;
  if(!state.usage){$("trendChart").innerHTML='<div class="empty"><strong>等待请求数据</strong></div>';return;}
  const offset=data.days[0].weekday,columns=Math.ceil((offset+data.days.length)/7);
  const labels=data.days.filter(d=>d.date.endsWith("-01")).map(d=>'<span style="grid-column:'+(Math.floor((offset+data.days.indexOf(d))/7)+1)+'">'+(d.month+1)+'月</span>').join("");
  const cells=data.days.map((d,i)=>{
    const detail=d.date+'\n'+usageCount(d.tokens)+' tokens · '+d.requests+' 次请求\n'+usageCount(d.credits)+' 积分'+(d.creditsUnknown?' · '+d.creditsUnknown+' 次成功请求积分未记录':'')+'\n输入 '+usageCount(d.prompt)+' · 输出 '+usageCount(d.completion)+'\n缓存 '+usageCount(d.cached);
    return '<span class="heatmap-day'+(d.future||d.outside?' future':'')+'" style="grid-column:'+(Math.floor((i+offset)/7)+1)+';grid-row:'+(d.weekday+1)+'" data-level="'+d.level+'"'+(d.future||d.outside?' aria-hidden="true"':' tabindex="0" role="img" aria-label="'+esc(detail)+'" data-chart-detail="'+esc(detail)+'"')+'></span>';
  }).join("");
  $("trendChart").innerHTML='<div class="heatmap-scroll" tabindex="0" role="region" aria-label="'+ui.heatmapYear+' 年每日'+unit+'统计"><div class="heatmap-calendar" style="--weeks:'+columns+'"><div class="heatmap-months">'+labels+'</div><div class="heatmap-weekdays"><span>日</span><span>一</span><span>二</span><span>三</span><span>四</span><span>五</span><span>六</span></div><div class="heatmap-days">'+cells+'</div></div></div>';
}
function showChartTooltip(event) {
  const cell=event.target.closest("[data-chart-detail]");
  if(!cell)return;
  const tip=$("chartTooltip");
  tip.textContent=cell.dataset.chartDetail;tip.hidden=false;
  const rect=cell.getBoundingClientRect(),box=tip.getBoundingClientRect();
  tip.style.left=Math.max(12,Math.min(window.innerWidth-box.width-12,rect.left+rect.width/2-box.width/2))+"px";
  tip.style.top=Math.max(12,rect.top-box.height-10)+"px";
}
function renderRecent() {
  const entries=(state.usage?.entries||[]).slice(-5).reverse();
  if(!entries.length){$("recentRequests").innerHTML='<div class="empty"><strong>'+(state.usage?'还没有调用记录':'等待请求日志')+'</strong>网关收到请求后，最新调用会显示在这里。</div>';return;}
  const rows=entries.map(e=>'<tr><td><div class="request-model"><span class="request-symbol">↗</span><span><span class="account-name">'+esc(e.model||"未知模型")+'</span><span class="hint">'+esc(e.nickname||e.uid8||String(e.uid||'').slice(0,8)||'未知账号')+'</span></span></div></td><td><span class="status '+usageStatusClass(e.status)+'">'+esc(e.status||'未知')+'</span></td><td>'+esc(usageDuration(e.duration_ms))+'</td><td class="hint">'+esc(usageClock(e.time))+'</td><td><button class="detail-link" data-request="'+state.usage.entries.indexOf(e)+'" aria-label="查看请求 '+esc(e.seq??'')+' 详情">详情</button></td></tr>').join("");
  $("recentRequests").innerHTML='<div class="table-wrap" tabindex="0" role="region" aria-label="最近请求，可横向滚动"><table><thead><tr><th>模型 / 账号</th><th>状态</th><th>耗时</th><th>时间</th><th></th></tr></thead><tbody>'+rows+'</tbody></table></div>';
}
function openRequest(index,trigger) {
  const e=state.usage?.entries?.[index]; if(!e)return;
  ui.request=JSON.parse(JSON.stringify(e)); ui.requestTrigger=trigger;
  $("requestTitle").textContent="请求 #"+(e.seq??"—");
  const fields=[["发生时间",fmtTime(e.time)],["模型",e.model||"—"],["账号",e.nickname||"—"],["完整 UID",e.uid||e.uid8||"—"],["产品线 / 模式",(e.realm||"—")+" / "+(e.mode||"—")],["HTTP 状态",e.status||"未知"],["总耗时",usageDuration(e.duration_ms)],["首字节 TTFB",usageMillis(e.ttfb_ms)],["输入 / 输出 Token",(e.prompt_tokens??"—")+" / "+(e.completion_tokens??"—")],["缓存命中 Token",e.cached_tokens??"—"],["消耗额度",e.credits_known?fmtCredits(e.credits_used):"未观测（不等于 0）"],["扣除前 → 扣除后",e.credits_known?(e.credits_before??"—")+" → "+(e.credits_after??"—"):"—"]];
  $("requestDetail").innerHTML='<dl class="detail-grid">'+fields.map(([k,v])=>'<div class="detail-field"><dt>'+k+'</dt><dd>'+esc(v)+'</dd></div>').join("")+'</dl>'+(e.error||e.balance_error?'<p class="detail-warning">'+esc(e.error||e.balance_error)+'</p>':'')+'<details class="detail-json"><summary>查看原始日志字段</summary><pre>'+esc(JSON.stringify(e,null,2))+'</pre></details>';
  $("requestDialog").showModal();
}
async function copyText(text,message) {
  try { if(navigator.clipboard?.writeText) await navigator.clipboard.writeText(text); else if(!legacyCopy(text)) throw new Error("copy"); toast(message); }
  catch (_) { if(legacyCopy(text))toast(message);else toast("复制失败，请选中文本手动复制",3500,"error"); }
}
function initializeSidebar() {
  const storageKey = "wb2api_sidebar_collapsed";
  const compactViewport = window.matchMedia("(min-width: 481px) and (max-width: 1000px)");
  const toggle = $("sidebarToggle");
  let preference = null;
  try {
    const saved = localStorage.getItem(storageKey);
    if (saved === "true" || saved === "false") preference = saved === "true";
  } catch (_) { /* 禁用存储时仍可展开和收起 */ }
  function render() {
    const collapsed = preference ?? compactViewport.matches;
    document.documentElement.classList.toggle("sidebar-collapsed", collapsed);
    toggle.setAttribute("aria-expanded", String(!collapsed));
    toggle.setAttribute("aria-label", collapsed ? "展开侧边栏" : "收起侧边栏");
    toggle.title = collapsed ? "展开侧边栏" : "收起侧边栏";
  }
  toggle.addEventListener("click", () => {
    preference = !document.documentElement.classList.contains("sidebar-collapsed");
    try { localStorage.setItem(storageKey, String(preference)); } catch (_) { /* 当前页面保留选择 */ }
    render();
  });
  compactViewport.addEventListener("change", render);
  render();
}

function initializeConsole() {
  initializeSidebar();
  switchView(); window.addEventListener("hashchange",()=>switchView(true));
  document.querySelectorAll('a[href^="#"]').forEach(link=>link.addEventListener("click",event=>{
    if(!event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey && link.hash===location.hash && Object.hasOwn(views,link.hash.slice(1))){
      event.preventDefault(); switchView(true);
    }
  }));
  // Hashes are application routes, not scroll destinations. Reset the initial native anchor jump.
  window.addEventListener("load",()=>{if(location.hash!=="#mainContent")window.scrollTo({top:0,behavior:"instant"});});
  for(const id of ["logSearch","logStatus","logRealm","logModel","logHours"]) $(id).addEventListener(id==="logSearch"?"input":"change",updateLogFilters);
  $("resetLogFilters").addEventListener("click",()=>{for(const id of ["logSearch","logStatus","logRealm","logModel","logHours"])$(id).value="";updateLogFilters();});
  $("showFailed").addEventListener("click",()=>{$("resetLogFilters").click();$("logStatus").value="failed";$("logHours").value=ui.trend==="all"?"":ui.trend;updateLogFilters();location.hash="usage";});
  $("statsRange").addEventListener("change",()=>{if(ui.chartMode==="heatmap"){ui.heatmapRange=$("statsRange").value;if(ui.heatmapRange!=="all")ui.heatmapYear=new Date().getFullYear();}else ui.trend=$("statsRange").value;renderTrend();});
  $("chartMetric").addEventListener("change",()=>{ui.metric=$("chartMetric").value;renderTrend();});
  document.querySelectorAll("[data-chart-mode]").forEach(btn=>btn.addEventListener("click",()=>{ui.chartMode=btn.dataset.chartMode;renderTrend();}));
  $("heatmapYear").addEventListener("change",()=>{ui.heatmapYear=Number($("heatmapYear").value);renderTrend();});
  for(const event of ["pointerover","focusin","click"])$("trendChart").addEventListener(event,showChartTooltip);
  for(const event of ["pointerleave","focusout"])$("trendChart").addEventListener(event,()=>{$("chartTooltip").hidden=true;});
  $("trendChart").addEventListener("keydown",event=>{if(event.key==="Escape")$("chartTooltip").hidden=true;});
  window.addEventListener("scroll",()=>{$("chartTooltip").hidden=true;},true);
  for(const id of ["usageTable","recentRequests"]) $(id).addEventListener("click",e=>{const button=e.target.closest("button[data-request]");if(button)openRequest(Number(button.dataset.request),button);});
  $("closeRequest").addEventListener("click",()=>$("requestDialog").close());
  $("requestDialog").addEventListener("close",()=>{if(ui.requestTrigger?.isConnected)ui.requestTrigger.focus();else $("mainContent").focus({preventScroll:true});});
  $("copyRequest").addEventListener("click",()=>{if(ui.request)copyText(JSON.stringify(ui.request,null,2),"请求详情已复制");});
  $("baseURL").value=location.origin+"/v1";
  $("copyBaseURL").addEventListener("click",()=>copyText($("baseURL").value,"API 地址已复制"));
  $("exportLogs").addEventListener("click",()=>{const entries=filteredLogs();if(!entries.length){toast("没有可导出的匹配日志",3000,"error");return;}const url=URL.createObjectURL(new Blob([ConsoleMetrics.csv(entries)],{type:"text/csv;charset=utf-8"}));const a=document.createElement("a");a.href=url;a.download="workbuddy-requests-"+new Date().toISOString().slice(0,10)+".csv";document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);toast("已导出 "+entries.length+" 条筛选结果");});
}

function toast(msg, ms = 3200, kind = "success") {
  const t = $("toast"); t.textContent = msg; t.dataset.kind = kind; t.classList.add("show");
  clearTimeout(t._h); t._h = setTimeout(() => t.classList.remove("show"), ms);
}
function busy(id, value, label) {
  const b = $(id);
  if (!b.dataset.label) b.dataset.label = b.textContent;
  b.disabled = value; b.setAttribute("aria-busy", String(value));
  b.textContent = value ? label : b.dataset.label;
}
function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
async function api(path, opts = {}) {
  const authCall = path.startsWith("/admin/api/auth/");
  if (!authCall && !state.authenticated) throw new Error("请先登录管理账号");
  const generation = state.authVersion;
  const headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
  if (opts.method && !["GET", "HEAD"].includes(opts.method)) headers["X-CSRF-Token"] = state.csrf;
  const controller = new AbortController();
  state.adminRequests.add(controller);
  const cancelFromCaller = () => controller.abort();
  if (opts.signal?.aborted) controller.abort();
  else opts.signal?.addEventListener("abort", cancelFromCaller, { once: true });
  const timer = setTimeout(() => controller.abort(), opts.method ? 60000 : 30000);
  try {
    const res = await fetch(path, Object.assign({}, opts, { headers, signal: controller.signal, cache: "no-store", credentials: "same-origin" }));
    const text = await res.text();
    if (generation !== state.authVersion) throw new Error("登录会话已变化");
    let data = null; try { data = text ? JSON.parse(text) : null; } catch (_) { /* 非 JSON 错误不直接显示原始 HTML */ }
    if (!res.ok) {
      const detail = data && (typeof data.error === "string" ? data.error : data.error?.message || data.message);
      if (res.status === 401 && !authCall) window.dispatchEvent(new Event("console-session-expired"));
      const err = new Error(detail || (res.status === 401 ? "请先登录管理账号" : "HTTP " + res.status));
      err.status = res.status; err.data = data; throw err;
    }
    if (data === null) throw new Error("服务器返回了无效数据");
    return data;
  } catch (e) {
    if (opts.signal?.aborted) throw e;
    if (e.name === "AbortError") throw new Error("请求超时，请刷新列表确认服务器状态");
    throw e;
  } finally { clearTimeout(timer); state.adminRequests.delete(controller); opts.signal?.removeEventListener("abort", cancelFromCaller); }
}
function fmtTime(t) {
  if (!t) return "—";
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString("zh-CN", { hour12: false });
}

// fmtCredits 额度格式化：整数原样，有小数才带两位。
//
// 为什么不一律保留两位（"358.00"）：多数账号的整数余额与精确值相同
// （没有未落账的小数消耗），拖两个 .00 只会让表格变吵。
//
// 处理三种取值：
//   - null/undefined（余额未知）→ "—"，不能显示成 0（那是"已耗尽"）
//   - 非数字（脏数据）→ "—"
//   - 数值 → 整数原样，小数两位（toFixed 顺带收掉 357.449999 这类浮点尾巴）
function fmtCredits(v) {
  if (v === null || v === undefined || v === "") return "—";
  const n = Number(v);
  if (!Number.isFinite(n)) return "—";
  return Number.isInteger(n) ? String(n) : n.toFixed(2);
}

// creditsCell 额度单元格：**并列**展示两个口径，并标清各自含义。
//
// 两者的观测点不同：
//   本地估算（credits_exact）= 减去本地已观测消耗后的池内余额，可带小数；
//   上游快照（credits）    = 当前池内保存的整数快照，且含计费延迟；套餐详情另取上游精确值。
// 大字展示本地估算，小字展示整数快照。上游精确余额可在「套餐」查看。
function creditsCell(a) {
  // 余额未知：两个口径都没有意义，显示 — 并说明原因（而非显示 0）。
  if (!a.credits_known) {
    return `<b class="s-dim" title="尚未从上游观测到余额。点 ↻ 可立即查询">—</b>` +
      `<div class="hint">未观测</div>` + refreshBtn(a);
  }
  const exact = fmtCredits(a.credits_exact ?? a.credits);
  const snap = fmtCredits(a.credits);
  const differs = String(exact) !== String(snap);
  const tip = `本地估算 ${exact}（已扣除本地观测到的消耗；上游精确余额见套餐）` +
    (differs ? `；上游整数快照 ${snap}（含计费延迟，未反映最近消耗）` : `；与上游快照一致`);
  return `<b title="${esc(tip)}">${esc(exact)}</b>` +
    (differs ? `<div class="snap mono" title="上游权威快照（整数）">↑${esc(snap)}</div>` : ``) +
    refreshBtn(a);
}

// refreshBtn 单号额度刷新按钮（额度列共用，避免两处重复模板）。
function refreshBtn(a) {
  const busy = state.uidBusy.has(a.uid);
  return `<button class="mini" data-uid-refresh="${esc(a.uid)}" ${busy ? "disabled" : ""} ` +
    `aria-label="刷新 ${esc(a.nickname || a.uid)} 的额度" title="向上游查询该账号的整数余额快照；精确额度见套餐">` +
    `${busy ? "…" : "↻"}</button>`;
}
function fmtSecs(s) {
  if (!s || s <= 0) return "即将恢复";
  if (s < 60) return s + " 秒";
  if (s < 3600) return Math.ceil(s / 60) + " 分钟";
  return (s / 3600).toFixed(1) + " 小时";
}
function rate(a) {
  const n = (a.success_count || 0) + (a.err_total || 0);
  return n ? (((a.success_count || 0) / n) * 100).toFixed(0) + "%" : "—";
}
function statusCell(a) {
  // 手动停用优先展示：这是运维自己刚做的动作，不该被系统状态盖住；
  // 两层同时为真时把系统判定原因一并透出，方便区分「我停的」和「它本来就坏了」。
  if (a.manual_disabled) return `<span class="status s-err">手动停用</span><div class="hint status-note">${esc(a.manual_reason || "已在控制台停用")}${a.disabled ? " · 系统判定：" + esc(a.disabled_reason || "") : ""}</div>`;
  if (a.disabled) return `<span class="status s-err">系统禁用</span><div class="hint status-note">${esc(a.disabled_reason)}</div>`;
  // 额度冻结：余额为 0，靠额度巡检确认恢复后自动解冻（不是时间冷却，故无"剩余时长"）。
  if (a.frozen) return `<span class="status s-warn">额度冻结</span><div class="hint status-note">${esc(a.frozen_reason || "余额为 0，等恢复")}</div>`;
  if (a.cooling) return `<span class="status s-warn">冷却 ${esc(fmtSecs(a.cool_remaining_seconds))}</span><div class="hint status-note">${esc(a.cool_kind || a.reason)}</div>`;
  return '<span class="status s-ok">正常</span>';
}
// statusKey 账号的展示态（优先序与 statusCell 完全一致）：状态筛选用它，
// 保证「筛出来的」和「看到的」永远是同一个口径。
function statusKey(a) {
  return a.manual_disabled ? "manual" : a.disabled ? "disabled" : a.frozen ? "frozen" : a.cooling ? "cooling" : "healthy";
}
function renderAccounts() {
  if (!state.overview) return;
  const query = $("accountSearch").value.trim().toLowerCase();
  const realmName = $("realmFilter").value, status = $("statusFilter").value;
  const acc = state.accounts.filter(a => (!query || `${a.nickname || ""} ${a.uid || ""}`.toLowerCase().includes(query)) &&
    (!realmName || a.realm === realmName) && (!status || status === statusKey(a)));
  $("accountCount").textContent = state.accounts.length;
  $("filterCount").textContent = `显示 ${acc.length} / ${state.accounts.length} 个账号`;
  if (!acc.length) {
    $("accounts").innerHTML = state.accounts.length ? '<div class="empty"><strong>没有匹配的凭证</strong>试试其他昵称、UID 或筛选条件。</div>' :
      `<div class="empty"><strong>还没有凭证</strong>通过连接设置中的「开始授权」添加账号，或将凭证导入 <span class="mono">${esc(state.overview.auth_dir || "./auths")}</span> 后重扫目录。</div>`;
    return;
  }
  const rows = acc.map(a => `<tr>
    <td><span class="account-name" title="${esc(a.nickname || a.uid)}">${esc(a.nickname || "未命名账号")}</span><span class="mono s-dim" title="${esc(a.uid)}">${esc(String(a.uid || "").slice(0, 12))}${String(a.uid || "").length > 12 ? "…" : ""}</span></td>
    <td><span class="tag ${a.realm === "workbuddy" || a.realm === "codebuddy" || a.realm === "ai" ? "intl" : "cn"}">${esc(a.realm)}</span></td>
    <td>${statusCell(a)}</td><td>${creditsCell(a)}</td>
    <td>${rate(a)} <span class="hint">(${esc(a.success_count || 0)}/${esc((a.success_count || 0) + (a.err_total || 0))})</span></td>
    <td>${esc(a.in_flight || 0)}</td><td class="hint">${esc(fmtTime(a.credits_checked_at))}</td>
    <td><button data-package-uid="${esc(a.uid)}" data-package-realm="${esc(a.realm)}" aria-label="查看 ${esc(a.nickname || a.uid)} 的套餐">套餐</button> <button id="accountSelection${state.accounts.indexOf(a)}" data-selection="${state.accounts.indexOf(a)}" ${a.selection_excluded ? 'class="primary"' : ""} ${state.selectionBusy ? "disabled" : ""} aria-label="设置 ${esc(a.nickname || a.uid)} 的选号顺序">选号</button> <button data-toggle="${state.accounts.indexOf(a)}" ${a.manual_disabled ? 'class="primary"' : ""} ${state.toggleBusy ? "disabled" : ""} aria-label="${a.manual_disabled ? "启用" : "停用"} ${esc(a.nickname || a.uid)}">${a.manual_disabled ? "启用" : "停用"}</button> <button class="danger" data-delete="${state.accounts.indexOf(a)}" aria-label="删除 ${esc(a.nickname || a.uid)} 的凭证" ${state.deleteBusy ? "disabled" : ""}>删除</button></td>
  </tr>`).join("");
  $("accounts").innerHTML = `<div class="table-wrap" tabindex="0" role="region" aria-label="凭证列表，可横向滚动"><table><thead><tr><th scope="col">账号</th><th scope="col">产品线</th><th scope="col">状态</th><th scope="col" title="大字 = 本地估算余额；小字 ↑N = 上游整数快照（含计费延迟）；上游精确额度见套餐">额度</th><th scope="col">成功率</th><th scope="col">在途</th><th scope="col">积分刷新</th><th scope="col">操作</th></tr></thead><tbody>${rows}</tbody></table></div>`;
}
function renderOverview(d) {
  // 乱序保护：SSE 推送与手动/重连时的 HTTP 拉取可能交织到达，只应用"较新"的那份。
  // 判据用服务端给的时间戳（两条路都带 now），不依赖浏览器时钟。
  const at = Date.parse(d.now || "");
  if (at && state.overviewAt && at < state.overviewAt) return;
  if (at) state.overviewAt = at;
  state.overview = d; state.accounts = d.accounts || [];
  // 累计统计的「按账号」用 state.accounts 把 uid 补成昵称，而 overview 通常比 usage 晚回来
  // （两个请求各自独立）。这里补一次明细重绘，否则昵称要等到下一次日志刷新才出现。
  // 只重绘本区块：不改动上面的既有渲染与状态。
  if (typeof renderUsageBreakdown === "function") renderUsageBreakdown(state.usage);
  // 路线别名（codebuddy → workbuddy）共用同一批账号与额度，后端不重复计数：
  // 这里把别名与去向写清楚，否则「产品线：cn + workbuddy + codebuddy」会被读成三份独立账号。
  const aliases = Object.entries(d.routes || {}).map(([a, s]) => `${a} → ${s}`);
  $("realmBadge").textContent = "产品线：" + (d.realms || []).join(" + ") +
    (aliases.length ? `（别名：${aliases.join("、")}，与其来源线共用同一批账号与额度，不重复计数）` : "") +
    " · 默认 " + (d.default_realm || "—");
  const t = d.totals || {};
  for (const [id, key] of [["tAccounts", "accounts"], ["tHealthy", "healthy"], ["tCooling", "cooling"]]) $(id).textContent = t[key] ?? 0;
  // 积分合计走 fmtCredits：后端 totals.credits 已是**精确值之和**（含小数扣减），
  // 与逐行 credits_exact 相加一致；直接显示会露出 1486.3000000000002 这种浮点尾巴。
  $("tCredits").textContent = fmtCredits(t.credits);
  // 「冷却中」= 冷却 + 额度冻结：两者都是"被移出轮转、等恢复"，只是恢复条件不同
  // （限流/时间冷却 vs 余额为 0 等巡检解冻）。后端 totals 里两个字段**互斥**（一个号只计一类），
  // 所以直接相加既不会重复计数，也正好等于"等待恢复的账号总数"。
  // 明细写进 note，而不是把冻结单列一格——12 格刚好两行六列，多一格就会让末行落单。
  const cooling = t.cooling ?? 0, frozen = t.frozen ?? 0;
  $("tCooling").textContent = cooling + frozen;
  $("tCoolingNote").textContent = frozen > 0
    ? `冷却 ${cooling} · 额度冻结 ${frozen}`
    : "等待额度或限流恢复";
  // 「已禁用」卡片 = 系统判定 + 手动停用（后端两字段互斥，直接相加不重复计数）。
  $("tDisabled").textContent = (t.disabled ?? 0) + (t.manual_disabled ?? 0);
  $("tDisabledNote").textContent = "系统 " + (t.disabled ?? 0) + " · 手动 " + (t.manual_disabled ?? 0);
  const checked = state.accounts.map(a => a.credits_checked_at).filter(Boolean).sort().pop();
  $("tCreditsAt").textContent = checked ? fmtTime(checked) : "尚未刷新";
  renderAccounts();
  renderDashboard();
}
// loadOverview 拉取凭证列表并渲染。
//
// opts 两个正交开关，刻意不合并成一个 silent（它们服务于不同目的，合并会互相拖累）：
//   poll     —— 来自后台轮询：失败不改页头状态，且用户正在表格内操作时跳过本轮；
//   forceRedraw —— 忽略"数据没变就不重绘"的优化，强制重建表格。
//     单号刷新必须用它：刷新结果正是要显示的东西，而对比基线还是旧值。
//     若这里也套用静默守卫，点按钮后焦点正在表格内 → 直接 return，刷新结果永远看不见。
async function loadOverview(opts = {}) {
  const poll = opts.poll === true, forceRedraw = opts.forceRedraw === true;
  if (poll && accountsHasFocus()) return false;
  const version = ++state.overviewVersion;
  const d = await api("/admin/api/overview");
  if (version !== state.overviewVersion) return false;
  if (forceRedraw || !(poll && sameOverview(d))) {
    renderOverview(d);
    state.overviewRaw = stableOverviewJSON(d);
  }
  if (!poll) {
    $("accounts").setAttribute("aria-busy", "false");
    $("gateDot").className = "dot ok"; $("gateText").textContent = "网关在线";
    $("loadError").hidden = true;
  }
  $("lastUpdated").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", { hour12: false });
  return true;
}

//
// 为什么不能直接整体 JSON 对比：响应里带 **易变字段**（`now` 每次请求都不同），
// 整体对比会永远判定"变了"，让"数据没变就不重绘"彻底失效——
// 那样每 5 秒都会重建表格 DOM，重置横向滚动、并踢走键盘焦点。
// 所以先剔除易变字段再比：这些字段**只影响页头时间戳**，与表格内容无关。
function sameOverview(d) {
  if (state.overviewRaw === null) return false;
  return state.overviewRaw === stableOverviewJSON(d);
}
// stableOverviewJSON 序列化"影响表格渲染"的部分：剔除时间戳类易变字段。
// 剔除白名单而非保留白名单：后端新增的表格字段会自动纳入比较（不会漏判）。
function stableOverviewJSON(d) {
  const volatile = new Set(["now", "server_time", "timestamp"]);
  const clean = {};
  for (const k of Object.keys(d).sort()) {
    if (volatile.has(k)) continue;
    clean[k] = d[k];
  }
  return JSON.stringify(clean);
}
// accountsHasFocus 用户当前焦点是否落在凭证表格内（键盘操作中）。
function accountsHasFocus() {
  const el = document.activeElement;
  return !!(el && $("accounts").contains(el));
}
async function refreshAll(withBalance) {
  if (!state.authenticated) return;
  if (state.refreshBusy) return false;
  state.refreshBusy = true; busy("refreshBtn", true, "刷新中…");
  $("accounts").setAttribute("aria-busy", "true");
  try {
    const loaded = await loadOverview();
    if (withBalance && loaded) await refreshBalance();
    loadUsage(true);
    return loaded;
  } catch (e) {
    $("gateDot").className = "dot err"; $("gateText").textContent = "连接异常";
    $("loadError").textContent = "加载失败：" + e.message + (state.overview ? "。以下保留上次数据。" : "。请检查连接与登录状态后重试。");
    $("loadError").hidden = false;
    if (!state.overview) $("accounts").innerHTML = '<div class="empty"><strong>暂时无法加载凭证</strong>请检查网关连接与登录状态，再点击「刷新数据」。</div>';
    return false;
  } finally {
    state.refreshBusy = false; busy("refreshBtn", false); $("accounts").setAttribute("aria-busy", "false"); scheduleCredits();
  }
}
async function refreshBalance() {
  if (state.balanceBusy) return;
  state.balanceBusy = true; $("creditsSchedule").textContent = "正在查询余额…";
  try {
    const r = await api("/admin/api/balance", { method: "POST", body: "{}" });
    const n = Object.keys(r.updated || {}).length, f = Object.keys(r.failed || {}).length;
    await loadOverview();
    if (n || f) toast(`积分已刷新：成功 ${n}，失败 ${f}`, 3500, f ? "error" : "success");
  } catch (e) { toast("刷新积分失败：" + e.message, 4200, "error"); }
  finally { state.balanceBusy = false; $("creditsSchedule").textContent = "每 10 分钟自动刷新"; scheduleCredits(); }
}
// refreshOneBalance 只刷新**指定账号**的额度（真实上游查询，会消耗一次上游请求）。
//
// 复用同一个接口：POST /admin/api/balance 传 {"uid":...} 即只刷该号
// （后端按 uid 过滤，见 adminBalance）。前端不需要新接口。
//
// 与"更新列表"的区别：这里刻意走**强制重绘**路径——刷新结果正是我们要显示的东西，
// 而静默轮询的"数据没变就不重绘"优化会把这次变化吞掉（对比基线 overviewRaw 仍是旧值）。
async function refreshOneBalance(uid) {
  if (!uid || state.uidBusy.has(uid)) return;
  state.uidBusy.add(uid);
  // 立刻重绘出忙碌态（按钮变 "…" 且禁用）：上游余额查询要 1~2 秒，
  // 没有即时反馈的话用户会以为按钮没响应而反复点。
  renderAccounts();
  try {
    const r = await api("/admin/api/balance", { method: "POST", body: JSON.stringify({ uid }) });
    const ok = (r.updated || {})[uid];
    const err = (r.failed || {})[uid];
    if (err) {
      toast("刷新失败：" + err, 4200, "error");
    } else if (ok !== undefined) {
      toast(`额度已刷新：${ok} credits`, 3000);
    }
    // 清掉对比基线：刷新结果正是要显示的东西，
    // 留着旧基线会让下面的静默取数被判为"数据没变"而跳过重绘。
    state.overviewRaw = null;
    await loadOverview({ forceRedraw: true });
  } catch (e) {
    toast("刷新失败：" + e.message, 4200, "error");
  } finally {
    state.uidBusy.delete(uid);
    // 只重绘按钮的忙碌态，避免整表重建打断用户。
    renderAccounts();
  }
}
// 每次完成后重新安排，避免原先 setTimeout 只执行一次以及慢请求重叠。
function scheduleCredits() {
  clearTimeout(state.creditsTimer);
  state.creditsTimer = setTimeout(refreshBalance, 10 * 60 * 1000);
}

// 凭证列表**没有兜底轮询**：实时性完全由 SSE 推送承担（见 connectEvents 与 applyPush）。
//
// 那"推送失效会不会永久停在旧数据上"？不会——关键在 connectEvents 的
// **重连即补一次全量**：浏览器断线、服务端重启、代理掐连接，都会走重连路径，
// 而重连成功的第一件事就是拉一次全量，把断线期间漏掉的变化补齐。
// 也就是说"最终一致"由重连语义保证，不需要一个常驻定时器再叠一层。
//
// 代价与取舍：连得上就一直实时（不用等 30 秒相位），连不上就明确显示"重连中"
// 并自动重试——比"看起来正常但数据其实停在 30 秒前"更好排查。

// refreshAccountsQuietly 静默刷新列表：推送与兜底轮询共用，统一做用户操作守卫。
//
// 守卫的意义：自动刷新绝不能打断确认弹窗、覆盖输入焦点，也不该在已有刷新
// 在途时叠加请求（那会让按钮的忙碌态错乱）。用户打开弹窗时暂停自动刷新是
// 正确取舍——他要做的是决策，不该被底下跳动的表格干扰。
//
// **刻意不按 document.hidden 跳过刷新**（曾经这么写过，是个错误设计）：
// 该标志在嵌入式面板 / 远程桌面 / 副屏窗口中会被误报为 true，而"把控制台
// 挂在副屏长期盯着"正是本页面的典型用法。一旦因此跳过，实时更新会在最需要
// 它的场景里**静默失效**——表现为"数据不动了"，且没有任何提示可排查。
// 成本上也站不住：这两个请求都是纯读内存（不发上游、不消耗额度），
// 在事件驱动 + 30 秒兜底的频率下，牺牲正确性去省这点开销并不划算。
async function refreshAccountsQuietly() {
  if (!state.authenticated) return;
  if (state.refreshBusy || state.balanceBusy || state.deleteBusy || state.toggleBusy || state.selectionBusy) return;
  // 任何对话框打开时都不静默重绘列表：innerHTML 重建会踢掉焦点，
  // 让正在填写表单的用户丢掉光标位置。
  if ($("deleteDialog").open || $("selectionDialog").open) return;
  try {
    await loadOverview({ poll: true });
    // 使用日志与列表同源变化（请求出口同时写日志与改状态），一并静默跟进。
    await loadUsage(true);
  } catch (_) { /* 静默刷新失败保留旧数据，下次再试 */ }
}

// setAccountsAuto 开关：同时启停"兜底轮询"与"SSE 连接"。
function setAccountsAuto() {
  if (!state.authenticated) return;
  state.accountsAuto.checked = $("accountsAuto").checked;
    if (state.accountsAuto.checked) {
      connectEvents();
    } else {
    clearTimeout(state.accountsTimer);
    state.accountsTimer = null;
    stopEvents();
  }
  setEventStatus();
}
$("accountsAuto").addEventListener("change", setAccountsAuto);

// connectEvents 建立 SSE 长连接：服务端状态变化时即时唤醒页面刷新。
//
// 控制台通过同源会话 Cookie 鉴权，fetch + ReadableStream 统一处理取消和会话变化。
//
// 断线处理（最容易做错的一环）：重连前必须**先补齐一次全量状态**，
// 否则断线期间发生的变化会永久丢失，页面"冻"在旧值上——比轮询更糟。
// 重连用指数退避（1s 起、30s 封顶），避免服务端不可用时把浏览器拖死。
function connectEvents() {
  if (!state.authenticated) return;
  const generation = state.authVersion;
  if (!state.accountsAuto.checked) return;
  if (state.eventsAbort) return; // 已有连接

  const ctl = new AbortController();
  state.eventsAbort = ctl;

  fetch("/admin/api/events", {
    credentials: "same-origin",
    cache: "no-store",
    signal: ctl.signal,
  }).then(async (res) => {
    if (generation !== state.authVersion || ctl.signal.aborted) return;
    if (res.status === 401) {
      window.dispatchEvent(new Event("console-session-expired"));
      state.eventsHint = "鉴权失败";
      setEventStatus();
      return;
    }
    if (!res.ok || !res.body) throw new Error("events status " + res.status);

    // 连上：重置退避，并立刻补齐一次（覆盖断线期间的所有变化）。
    state.eventsBackoff = 1000;
    state.eventsHint = "已连接";
    setEventStatus();
    await refreshAccountsQuietly();

    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (!state.authenticated || generation !== state.authVersion) return;
      if (done) break;
      buf += dec.decode(value, { stream: true });
      // SSE 以空行分隔事件；按事件块解析，未完整的一段留在 buf 里。
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        // 事件本身不带状态内容，"收到即去拉全量"（见服务端设计说明）。
        // 事件带状态负载：直接应用，不再回拉（见服务端 admin_events.go 说明）。
        // 负载缺失（旧服务端只发 data:{}，或解析失败）时退回拉取，保持向后兼容。
        if (block.indexOf("event: change") >= 0) {
          const payload = parseSSEPayload(block);
          if (payload) applyPush(payload);
          else refreshAccountsQuietly();
        }
        // 服务端关闭前的告别事件：直接退出循环去重连。
        if (block.indexOf("event: bye") >= 0) return;
      }
    }
  }).catch(() => {
    /* 断开或网络错误：统一在 finally 里重连 */
  }).finally(() => {
    if (state.eventsAbort !== ctl) return;
    state.eventsAbort = null;
    if (ctl.signal.aborted) return;   // 主动关闭（用户关了开关/页面卸载）
    state.eventsHint = "重连中";
    setEventStatus();
    state.eventsBackoff = Math.min((state.eventsBackoff || 1000) * 2, 30000);
    state.eventsTimer = setTimeout(connectEvents, state.eventsBackoff);
  });
}

// stopEvents 关闭 SSE 连接（用户关掉开关 / 页面卸载时）。
function stopEvents() {
  if (state.eventsTimer) clearTimeout(state.eventsTimer);
  state.eventsTimer = null;
  if (state.eventsAbort) state.eventsAbort.abort();
  state.eventsAbort = null;
  state.eventsHint = "";
}

// parseSSEPayload 从 SSE 事件块里取出 data: 行的负载（解析失败 / 空负载返回 null）。
//
// 空负载（data: {}）是服务端"快照构建失败"时的降级信号，也是旧版服务端的全部内容——
// 两者都返回 null，调用方退回 HTTP 拉取，从而天然向后兼容。
function parseSSEPayload(block) {
  const m = /(?:^|\n)data: ?(.*)/.exec(block);
  if (!m) return null;
  let obj = null;
  try { obj = JSON.parse(m[1]); } catch (_) { return null; }
  if (!obj || typeof obj !== "object" || !obj.overview) return null;
  return obj;
}

// applyPush 直接应用推送来的状态快照——"实时"的主路径，不发任何请求。
//
// 与 refreshAccountsQuietly 的分工：那条是"拉"（重连补齐、手动刷新），
// 这条是"推"。两条路最终都落到 renderOverview，渲染口径一致，区别只在数据来源。
function applyPush(payload) {
  // 用户正在确认删除：不要在弹窗背后把表格换掉（与 refreshAccountsQuietly 的守卫同源）。
  if ($("deleteDialog").open) return;
  if (payload.overview) renderOverview(payload.overview);
  // usage 只在服务端判定"用量有变化"时才附带；关掉实时更新则保留当前快照。
  if (payload.usage && state.usageAuto.checked) {
    // A newer pushed snapshot wins over an HTTP refresh that started before it.
    ++state.usageVersion;
    state.usage = payload.usage;
    state.usageError = "";
    renderUsage();
  }
}

// setEventStatus 把连接状态显示出来——实时功能"看起来没生效"时，
// 这一行能立刻区分"没连上"与"连上了但确实没变化"。
function setEventStatus() {
  const el = $("accountsAutoHint");
  if (!el) return;
  if (!state.accountsAuto.checked) { el.textContent = "实时已停用"; return; }
  const h = state.eventsHint || "";
  el.textContent = h ? "实时：" + h : "实时：连接中…";
}
async function reloadAuths() {
  busy("reloadBtn", true, "重扫中…");
  try {
    const r = await api("/admin/api/reload", { method: "POST", body: "{}" });
    await loadOverview();
    toast("目录已重扫：" + Object.entries(r.accounts || {}).map(([rn, n]) => `${rn} ${n} 个`).join("，"));
  } catch (e) { toast("重扫或刷新失败：" + e.message, 4200, "error"); }
  finally { busy("reloadBtn", false); }
}
function confirmDelete(index, trigger) {
  if (state.deleteBusy) return;
  const account = state.accounts[index];
  if (!account) return;
  state.deleteTarget = { uid: account.uid, realm: account.realm, nickname: account.nickname };
  state.deleteTrigger = trigger;
  $("deleteName").textContent = account.nickname || "未命名账号";
  $("deleteIdentity").textContent = `${account.realm} · ${account.uid}`;
  $("deleteError").hidden = true;
  $("deleteDialog").showModal();
  $("deleteCancel").focus();
}
function closeDelete() { if (!state.deleteBusy) $("deleteDialog").close(); }
async function deleteAccount() {
  if (!state.deleteTarget || state.deleteBusy) return;
  const target = state.deleteTarget;
  state.deleteBusy = true; ++state.overviewVersion;
  busy("deleteConfirm", true, "删除中…"); $("deleteCancel").disabled = true; $("deleteError").hidden = true;
  try {
    const r = await api("/admin/api/accounts", { method: "DELETE", body: JSON.stringify({ realm: target.realm, uid: target.uid }) });
    if (r.ok !== true) throw new Error("服务器未确认删除，请刷新列表核实");
    ++state.overviewVersion;
    $("deleteDialog").close();
    try {
      await loadOverview();
      toast("凭证已删除，列表已更新");
    } catch (e) {
      $("loadError").textContent = "凭证已删除，但列表刷新失败。请点击「刷新数据」核实最新状态。";
      $("loadError").hidden = false;
      toast("删除成功，但刷新失败：" + e.message, 5000, "error");
    }
  } catch (e) {
    $("deleteError").textContent = "删除未确认：" + e.message;
    $("deleteError").hidden = false;
  } finally {
    state.deleteBusy = false; busy("deleteConfirm", false); $("deleteCancel").disabled = false;
    renderAccounts();
    if (!$("deleteDialog").open) { state.deleteTarget = null; $("accountSearch").focus(); }
  }
}

// ── 选号设置（账号级排除 / 落位 / 自定义优先级）──────────────────────────────
// 与启停开关的分工：启停是**可用性**（停用后不参与任何调度），
// 这里改的是**排序**（账号照常健康、照常调度，只是不再按策略与其它号竞争）。
// 界面上刻意分成两个入口，避免运维把"我想让它排在后面"误操作成"把它停掉"。
function openSelection(index) {
  const account = state.accounts[index];
  if (!account || state.selectionBusy) return;
  state.selectionTarget = { uid: account.uid, realm: account.realm, nickname: account.nickname };
  state.selectionTrigger = $(`accountSelection${index}`);
  $("selectionName").textContent = account.nickname || "未命名账号";
  $("selectionIdentity").textContent = `${account.realm} · ${account.uid}`;
  $("selectionExcluded").checked = !!account.selection_excluded;
  // 落位：服务端只存 first/last（空值按 last 解释），故这里直接读值即可。
  $("selectionPlacement").value = account.selection_placement === "first" ? "first" : "last";
  $("selectionPriority").value = String(account.selection_priority || 0);
  $("selectionError").hidden = true;
  syncSelectionForm();
  $("selectionDialog").showModal();
  $("selectionCancel").focus();
}
// syncSelectionForm 未勾选排除时禁掉落位选择：placement 只在排除时才有意义，
// 留一个可编辑但无效的控件会让人以为它生效。
function syncSelectionForm() {
  const excluded = $("selectionExcluded").checked;
  $("selectionPlacement").disabled = !excluded;
  $("selectionPlacementBox").style.opacity = excluded ? "" : "0.55";
}
function closeSelection() { if (!state.selectionBusy) $("selectionDialog").close(); }
async function saveSelection() {
  if (!state.selectionTarget || state.selectionBusy) return;
  const target = state.selectionTarget;
  const priority = Number($("selectionPriority").value);
  if (!Number.isInteger(priority)) {
    $("selectionError").textContent = "自定义优先级必须是整数。";
    $("selectionError").hidden = false;
    return;
  }
  const payload = {
    realm: target.realm, uid: target.uid,
    selection: {
      excluded: $("selectionExcluded").checked,
      // 未排除时不传 placement：让服务端把残留的落位清掉，
      // 而不是前端硬塞一个语义上无效的值。
      priority,
    },
  };
  if (payload.selection.excluded) payload.selection.placement = $("selectionPlacement").value;
  state.selectionBusy = true; ++state.overviewVersion;
  busy("selectionSave", true, "保存中…"); $("selectionCancel").disabled = true;
  $("selectionError").hidden = true;
  try {
    const r = await api("/admin/api/accounts", { method: "PATCH", body: JSON.stringify(payload) });
    if (r.ok !== true) throw new Error("服务器未确认保存，请刷新列表核实");
    ++state.overviewVersion;
    $("selectionDialog").close();
    try {
      await loadOverview();
      toast("选号设置已保存：" + (target.nickname || target.uid));
    } catch (e) {
      $("loadError").textContent = "选号设置已保存，但列表刷新失败。请点击「刷新数据」核实最新状态。";
      $("loadError").hidden = false;
      toast("已保存，但刷新失败：" + e.message, 5000, "error");
    }
  } catch (e) {
    $("selectionError").textContent = "保存未确认：" + e.message;
    $("selectionError").hidden = false;
  } finally {
    state.selectionBusy = false; busy("selectionSave", false); $("selectionCancel").disabled = false;
    renderAccounts();
    if (!$("selectionDialog").open) state.selectionTarget = null;
  }
}
function stopPoll() { clearTimeout(state.pollTimer); state.pollTimer = null; ++state.loginVersion; }
async function startLogin() {
  stopPoll(); const version = state.loginVersion, realmName = $("loginRealm").value;
  state.loginUrl = ""; $("loginBox").hidden = true; busy("loginBtn", true, "生成中…");
  try {
    const r = await api("/admin/api/login/start", { method: "POST", body: JSON.stringify({ realm: realmName }) });
    if (version !== state.loginVersion) return;
    const url = new URL(r.auth_url);
    if (!["https:", "http:"].includes(url.protocol) || !r.state) throw new Error("授权链接或会话无效，请重试");
    state.loginUrl = r.auth_url; $("loginUrl").value = r.auth_url;
    $("loginBox").hidden = false; $("copyHint").textContent = "";
    $("loginHint").textContent = `已生成 ${realmName} 授权链接，请在 ${Math.ceil((r.expires_in_seconds || 600) / 60)} 分钟内完成登录。`;
    $("pollDot").className = "dot"; $("pollText").textContent = "等待登录…";
    pollLogin(r.state, realmName, version, Date.now() + (r.expires_in_seconds || 600) * 1000);
  } catch (e) { $("loginHint").textContent = "生成失败，可重新发起授权。"; toast("发起授权失败：" + e.message, 4200, "error"); }
  finally { busy("loginBtn", false); }
}
function openLogin() { if (state.loginUrl) window.open(state.loginUrl, "_blank", "noopener,noreferrer"); }
function legacyCopy(text) {
  const active = document.activeElement, area = document.createElement("textarea");
  area.value = text; area.readOnly = true; area.style.cssText = "position:fixed;left:-9999px;top:0;font-size:16px";
  document.body.appendChild(area);
  try { area.focus(); area.select(); area.setSelectionRange(0, area.value.length); return document.execCommand("copy") === true; }
  catch (_) { return false; }
  finally { area.remove(); if (active && typeof active.focus === "function") active.focus({ preventScroll: true }); }
}
async function copyLogin() {
  const url = state.loginUrl;
  if (!url) { toast("请先生成授权链接", 3200, "error"); return; }
  clearTimeout(state.copyTimer); busy("copyBtn", true, "复制中…");
  let copied = false;
  try {
    if (window.isSecureContext && navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      try { await navigator.clipboard.writeText(url); copied = true; } catch (_) { /* 权限拒绝时尝试传统复制 */ }
    }
    if (!copied) copied = legacyCopy(url);
    if (copied) {
      $("copyHint").textContent = "授权链接已复制，可以粘贴到浏览器打开。";
      toast("授权链接已复制");
    } else {
      $("loginUrl").focus(); $("loginUrl").select(); $("loginUrl").setSelectionRange(0, $("loginUrl").value.length);
      $("copyHint").textContent = "浏览器限制了自动复制。链接已选中，请按 Ctrl+C（macOS：Command+C），或长按复制。";
      toast("自动复制受限，请手动复制已选中的链接", 5000, "error");
    }
  } finally {
    busy("copyBtn", false);
    if (copied) { $("copyBtn").textContent = "已复制"; state.copyTimer = setTimeout(() => { $("copyBtn").textContent = "复制授权链接"; }, 2200); }
  }
}
function pollLogin(sessionState, realmName, version, deadline) {
  state.pollTimer = setTimeout(async () => {
    if (version !== state.loginVersion) return;
    if (Date.now() >= deadline) { $("pollText").textContent = "已超时，请重新授权"; $("pollDot").className = "dot err"; return; }
    try {
      const r = await api(`/admin/api/login/poll?state=${encodeURIComponent(sessionState)}&realm=${encodeURIComponent(realmName)}`);
      if (version !== state.loginVersion) return;
      if (r.status === "pending") { pollLogin(sessionState, realmName, version, deadline); return; }
      if (r.status === "done") {
        stopPoll(); state.loginUrl = "";
        $("pollDot").className = "dot ok"; $("pollText").textContent = "登录成功";
        $("loginBox").hidden = true;
        $("loginHint").textContent = r.reload_error ? "凭证已保存，但热加载失败，请点击「重扫凭证目录」。" : "登录成功，凭证已保存并自动热加载。";
        toast(r.reload_error ? "凭证已保存，重载失败：" + r.reload_error : "登录成功：" + (r.nickname || r.uid), 5000, r.reload_error ? "error" : "success");
        await refreshAll(true);
      } else {
        $("pollDot").className = "dot err"; $("pollText").textContent = "授权失败";
        toast("登录失败：" + (r.error || "请重新发起授权"), 4200, "error");
      }
    } catch (e) {
      if (version !== state.loginVersion) return;
      $("pollDot").className = "dot err"; $("pollText").textContent = e.status === 410 || e.status === 404 ? "会话已过期" : "轮询中断";
      toast("授权检测失败：" + e.message, 4200, "error");
    }
  }, 3000);
}

// toggleAccount 启停单个凭证：当前手动停用中 → 点的是「启用」，否则是「停用」。
// 停用是可逆操作（凭证文件与系统状态都不动），故不弹确认框，直接用 toast 回执 +
// 刷新列表；但请求期间锁住所有启停按钮，避免连点把两层的期望状态打成不一致。
async function toggleAccount(index) {
  const account = state.accounts[index];
  if (!account || state.toggleBusy) return;
  const enable = !!account.manual_disabled;
  const label = account.nickname || account.uid;
  state.toggleBusy = true;
  renderAccounts();
  try {
    const r = await api("/admin/api/accounts", { method: "PATCH", body: JSON.stringify({ realm: account.realm, uid: account.uid, enabled: enable }) });
    if (r.ok !== true) throw new Error("服务器未确认，请刷新列表核实");
    await loadOverview();
    toast((enable ? "已启用：" : "已停用：") + label + (enable ? "" : "（不再参与账号调度）"));
  } catch (e) {
    toast((enable ? "启用失败：" : "停用失败：") + e.message, 4200, "error");
    try { await loadOverview(); } catch (_) { /* 刷新失败保留旧列表，用户可手动刷新 */ }
  } finally {
    state.toggleBusy = false;
    renderAccounts();
  }
}
// ── 使用日志（04）────────────────────────────────────────────────────────────
// The API returns all retained history. Pagination only controls presentation.
const USAGE_PAGE_DEFAULT = 20;
function usagePageSize() {
  if (state.usagePageSize === "all") return Math.max(1,state.usage?.entries?.length || 0);
  const n = usageInt(state.usagePageSize);
  return Number.isSafeInteger(n) && n > 0 ? n : USAGE_PAGE_DEFAULT;
}
// 总页数：至少 1 页（即使 0 条），这样页码显示不会出现 "1 / 0"。
function usagePageCount(total) {
  return Math.max(1, Math.ceil(total / usagePageSize()));
}
// 当前页夹到合法区间并返回：数据变少（清空、切小每页条数）后旧页码可能越界，
// 统一在这里收敛，调用方不必各自判边界。
function usageCurrentPage(total) {
  const n = usagePageCount(total);
  state.usagePage = Math.min(Math.max(1, usageInt(state.usagePage) ?? 1), n);
  return state.usagePage;
}
function usageInt(value) {
  const n = Number(value);
  return Number.isFinite(n) ? n : null;
}

// ── 累计统计（summary）────────────────────────────────────────────────────────
// 口径提醒：summary 统计的是「当前保留的日志文件（主文件 + 备份）覆盖的全部记录」，
// 更早被轮转删除的不在其中，手动清空后归零，进程重启后从文件重算——所以界面上必须把
// 口径写出来；日志筛选与分页不影响累计值。
// 千分位：token 可能上千万，裸数字读不出量级；用 zh-CN 分组，与页面其它数字口径一致。
function usageCount(n) {
  const v = usageInt(n);
  if (v === null) return "—";
  return v.toLocaleString("zh-CN");
}
// 缓存命中率 = cached / prompt；prompt 为 0 时没有分母，返回 null 让调用方显示 "—"，绝不做 0 除法。
function usageCacheRate(cached, prompt) {
  const p = usageInt(prompt), c = usageInt(cached);
  if (p === null || c === null || p <= 0) return null;
  return c / p;
}
// 平均耗时 = duration_sum_ms / timed_count；timed_count 为 0 时同样返回 null 而不是 NaN/Infinity。
function usageAvgDuration(sum, count) {
  const s = usageInt(sum), c = usageInt(count);
  if (s === null || c === null || c <= 0) return null;
  return s / c;
}
// credits 是小数：整数原样展示（132 而不是 132.00），非整数保留两位；缺失给 "—" 不伪装成 0。
function usageCreditsText(n) {
  const v = usageInt(n);
  if (v === null) return "—";
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}
// Go 的零时间在旧响应里会被序列化成 "0001-01-01T00:00:00Z"（omitempty 对 struct 不生效），
// 它和新版的 null 表达的是同一件事：从未记录。直接格式化会把"没有记录"渲染成一个像真的时间
// （例如「上次清空：08:00:00」），所以零值判定集中在一处，usageMinute 与 usageClock 共用，
// 免得只修了统计范围、漏掉上次清空。
function usageIsZeroTime(value) {
  return typeof value === "string" && value.startsWith("0001-01-01");
}
// 时间范围：只到分钟。秒级精度对"统计覆盖到哪"没有信息量，反而让这行变长。
function usageMinute(value) {
  if (!value || usageIsZeroTime(value)) return "";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "";
  // 明确列全字段：不写 year 就会带上年份（2026/09/14 22:03），这行会明显变长且冗余。
  return d.toLocaleString("zh-CN", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}
// 老版本后端没有 summary 字段：统一在这里判定，所有渲染分支共用同一个"有没有"的判断。
function usageSummaryOf(d) {
  const s = d && d.summary;
  return s && typeof s === "object" ? s : null;
}
// 累计统计"为什么不可用"的唯一判定点：卡片组与明细表共用，保证同一状态下两处给的是同一句话。
// 为什么需要它：enabled=false（上报关闭，磁盘上可能还留着旧日志）与 file 为空（仅内存模式）时，
// 后端返回的是**全零快照**——那是"没在记"，不是"确实一条都没有"。把它当权威 0 显示，会和同一张
// 卡片组里「仅保留既有文件内容」的说明自相矛盾。三种原因要分清（还没加载 / 读取失败 / 无 summary）。
function usageUnavailableReason(d) {
  if (!d) return state.usageError ? "使用日志读取失败" : "";
  if (!usageSummaryOf(d)) return "当前后端未返回 summary";
  if (d.enabled === false) return "后台上报已关闭";
  if (!d.file) return "使用日志未落盘（仅内存模式）";
  return "";
}
// by_* 永远是数组；顺手过滤掉非对象元素，避免脏数据把表格渲染成 undefined。
function usageGroupsOf(summary, key) {
  const arr = summary && summary[key];
  return Array.isArray(arr) ? arr.filter(entry => entry && typeof entry === "object") : [];
}
// 分组元素的 totals 与 summary 同构（不含 first/last 与 by_*），缺字段时按 0 处理，但不把 0 当成"确认为 0"。
function usageTotalsOf(entry) {
  const t = entry && entry.totals;
  return t && typeof t === "object" ? t : {};
}
// 产品的 key 是完整 uid，直接铺出来会撑爆列宽：取前 8 位（与日志表 uid8 同口径），
// 再去 state.accounts 里按完整 uid 找昵称补上；匹配不到就只显示 8 位，不猜。
function usageKeyCell(tab, key) {
  const raw = key === null || key === undefined || key === "" ? "" : String(key);
  if (!raw) return { html: '<span class="s-dim">—</span>', title: "" };
  if (tab !== "by_uid") return { html: esc(raw), title: raw };
  const uid8 = raw.slice(0, 8);
  const accounts = Array.isArray(state.accounts) ? state.accounts : [];
  const hit = accounts.find(a => a && String(a.uid || "") === raw);
  const nickname = hit && hit.nickname ? String(hit.nickname) : "";
  // 昵称可能很长，而表格是 white-space:nowrap：复用日志表那套 .account-name 的省略号封顶，
  // 否则「按账号」这一列会被长昵称撑到四五百像素，把 token 列全挤到横向滚动里。
  const inner = `<span class="mono">${esc(uid8)}</span>${nickname ? " · " + esc(nickname) : ""}`;
  return {
    html: `<span class="account-name">${inner}</span>`,
    title: raw + (nickname ? " · " + nickname : "")
  };
}
// 页签定义集中在常量里：空态提示文案、aria 标签都从这里取，避免三处各写一份 tab 名。
const USAGE_TABS = {
  by_uid: { id: "usageTabUid", empty: "尚无累计记录：当前日志文件里还没有能归入账号的调用。" },
  by_model: { id: "usageTabModel", empty: "尚无累计记录：当前日志文件里还没有带模型的调用。" },
  by_realm: { id: "usageTabRealm", empty: "尚无累计记录：当前日志文件里还没有带产品线的调用。" }
};
// 明细表的列定义：表头文案、单元格渲染、排序取值写在同一个对象里。分成表头/表体两套写法，
// 一旦加列就会走偏（表头有、单元格漏），这是本区块最容易被改坏的地方。
// key 为 null = 该列不参与排序：「成功/失败」是复合值，单个方向讲不清，就不给排序入口。
const USAGE_COLUMNS = [
  {
    key: "requests", label: "调用次数",
    num: t => usageInt(t.requests) ?? 0,
    cell: t => esc(usageCount(t.requests))
  },
  {
    key: null, label: "成功/失败",
    // 失败为 0 时弱化成灰、>0 时用 .s-warn 提出来：失败率是这张表的主要用途之一。
    cell: t => {
      const success = usageInt(t.success) ?? 0, failed = usageInt(t.failed) ?? 0;
      return `<span class="s-ok">${esc(usageCount(success))}</span> / `
        + (failed > 0 ? `<span class="s-warn">${esc(usageCount(failed))}</span>` : '<span class="s-dim">0</span>');
    }
  },
  {
    key: "prompt_tokens", label: "总上传 token",
    num: t => usageInt(t.prompt_tokens) ?? 0,
    // 缓存命中挂在同一个单元格的 title 上：它是"上传"的一个子集，另开一列会挤掉更有用的列。
    // 缓存命中挂在同一个单元格的 title 上：它是"上传"的一个子集，另开一列会挤掉更有用的列。
    cell: t => {
      const cached = usageInt(t.cached_tokens) ?? 0;
      const title = cached > 0 ? ` title="其中缓存命中 ${usageCount(cached)} tokens"` : "";
      return `<span${title}>${esc(usageCount(t.prompt_tokens))}</span>`;
    }
  },
  { key: "completion_tokens", label: "总消耗 token", num: t => usageInt(t.completion_tokens) ?? 0, cell: t => esc(usageCount(t.completion_tokens)) },
  { key: "total_tokens", label: "合计 token", num: t => usageInt(t.total_tokens) ?? 0, cell: t => esc(usageCount(t.total_tokens)) },
  {
    key: "credits", label: "额度消耗",
    num: t => usageInt(t.credits_used) ?? 0,
    // 这一行有未观测消耗时挂个 warn tag，绝不把"未知"当 0 静默过去。
    cell: t => {
      const unknown = usageInt(t.credits_unknown) ?? 0;
      const credits = usageInt(t.credits_used);
      const title = credits === null ? "" : ` title="未记录消耗的调用 ${usageCount(unknown)} 次"`;
      return `<b class="usage-credits"${title}>${esc(usageCreditsText(credits))}</b>`
        + (unknown > 0 ? ` <span class="tag warn">${esc(usageCount(unknown))} 次未知</span>` : "");
    }
  },
  {
    key: "avg", label: "平均耗时",
    // 没有 timed_count 的行没有"平均"，当 -1 排到末尾，而不是当 0 混进真实数据里。
    num: t => { const a = usageAvgDuration(t.duration_sum_ms, t.timed_count); return a === null ? -1 : a; },
    cell: t => esc(usageDuration(usageAvgDuration(t.duration_sum_ms, t.timed_count)))
  }
];
// 明细默认只展开前 12 行：一个活跃网关的模型/账号维度能到几十行，铺满会让区块失去焦点。
const USAGE_ROWS_PREVIEW = 12;
// 分维度明细：与 state.usage.summary 同一份数据，切页签/排序只重绘本区块，不触发网络请求。
function renderUsageBreakdown(d) {
  const table = $("usageBreakdownTable"), more = $("usageBreakdownMore");
  const headline = $("usageBreakdownHeadline"), note = $("usageBreakdownNote"), count = $("usageBreakdownCount");
  const summary = usageSummaryOf(d);
  // 页签按钮的选中态与 aria-pressed 一起收敛，键盘/读屏用户才知道自己在哪个维度。
  for (const [tab, meta] of Object.entries(USAGE_TABS)) {
    const btn = $(meta.id);
    if (!btn) continue;
    btn.setAttribute("aria-pressed", String(state.usageTab === tab));
  }
  more.hidden = true;
  $("usageBreakdown").setAttribute("aria-busy", "false");
  table.setAttribute("aria-busy", "false");
  // 降级路径一：加载失败。不要显示任何 0/NaN，直说读不到。
  if (!d) {
    count.textContent = "";
    note.textContent = "按当前保留日志累计，不受请求日志页的筛选和分页影响。";
    headline.hidden = false;
    table.innerHTML = state.usageError
      ? `<div class="empty"><strong>暂时无法读取累计统计</strong>${esc(state.usageError)}</div>`
      : '<div class="empty">正在加载累计统计…</div>';
    return;
  }
  // 降级路径二/三：后端无 summary，或上报关闭 / 未落盘（此时后端给的是全零快照）。
  // 整行页签隐藏、只留说明，明确区分是"后端能力"还是"配置状态"，而不是让人对着一堆 0 猜。
  const reason = usageUnavailableReason(d);
  if (reason) {
    headline.hidden = true;
    note.textContent = "";
    table.innerHTML = `<div class="empty"><strong>统计不可用</strong>累计统计不可用（${esc(reason)}）；此时后端给出的是未记账的全零快照，显示成 0 条会产生误导。下面的日志表不受影响。</div>`;
    return;
  }
  headline.hidden = false;
  const rows = usageGroupsOf(summary, state.usageTab);
  const overflow = usageInt(summary.group_overflow_keys) ?? 0;
  const parts = [`基于当前保留日志，共 ${rows.length} 项；不受请求日志页筛选影响。`];
  // group_overflow_keys 是**三个维度的合计**，不是当前页签的数字：只有真正溢出的那一维才会出现
  // 「其他」行。说成"该行是合并值"会在未溢出的页签上误导，所以只声明合计口径。
  if (overflow > 0) parts.push(`三个维度合计有 ${overflow} 个低频键被折叠进「其他」。`);
  note.textContent = parts.join(" ");
  if (!rows.length) {
    count.textContent = "";
    table.innerHTML = `<div class="empty"><strong>尚无累计记录</strong>${USAGE_TABS[state.usageTab].empty}</div>`;
    return;
  }
  // 排序：默认按额度消耗倒序（后端顺序是 requests 倒序，额度才是运维最关心的成本视角）。
  // 同值时按 key 升序兜底，保证顺序稳定、重绘不跳行。
  const spec = USAGE_COLUMNS.find(c => c.key === state.usageSort) || USAGE_COLUMNS.find(c => c.key === "credits");
  const dir = state.usageSortDir === "asc" ? 1 : -1;
  const sorted = rows.slice().sort((a, b) => {
    const delta = (spec.num(usageTotalsOf(a)) - spec.num(usageTotalsOf(b))) * dir;
    if (delta) return delta;
    return String(a.key || "").localeCompare(String(b.key || ""), "zh-CN");
  });
  const visible = state.usageShowAll ? sorted : sorted.slice(0, USAGE_ROWS_PREVIEW);
  count.textContent = sorted.length > USAGE_ROWS_PREVIEW
    ? `显示 ${visible.length} / ${sorted.length} 项`
    : `共 ${sorted.length} 项`;
  // 只有真的被截断时才给切换按钮：不超 12 行时出现「显示全部」是假控件。
  more.hidden = sorted.length <= USAGE_ROWS_PREVIEW;
  more.textContent = state.usageShowAll ? `只看前 ${USAGE_ROWS_PREVIEW} 项` : "显示全部";
  // aria-sort 只在当前排序列上出声（其余 none），方向同时给箭头字符（aria-hidden 避免读屏重复播报）。
  const sortAttr = key => state.usageSort === key ? (state.usageSortDir === "asc" ? "ascending" : "descending") : "none";
  const mark = key => state.usageSort === key ? `<span class="usage-sort-mark" aria-hidden="true">${state.usageSortDir === "asc" ? "↑" : "↓"}</span>` : "";
  // 「键」列固定为第一列且不可排序：按 uid/模型名排序对运维没有信息量，还会和"看谁最贵"抢默认视图。
  const head = `<th scope="col">键</th>` + USAGE_COLUMNS.map(c => {
    if (!c.key) return `<th scope="col">${c.label}</th>`;
    const current = state.usageSort === c.key;
    const label = `按${c.label}排序（当前${current ? (state.usageSortDir === "asc" ? "升序" : "降序") : "未排序"}）`;
    return `<th scope="col" aria-sort="${sortAttr(c.key)}"><button type="button" class="usage-sort" data-usage-sort="${c.key}" aria-label="${esc(label)}">${c.label}${mark(c.key)}</button></th>`;
  }).join("");
  const body = visible.map(entry => {
    const t = usageTotalsOf(entry);
    const cell = usageKeyCell(state.usageTab, entry.key);
    return `<tr>
    <td title="${esc(cell.title)}">${cell.html}</td>
    ${USAGE_COLUMNS.map(c => `<td>${c.cell(t)}</td>`).join("")}
  </tr>`;
  }).join("");
  table.innerHTML = `<div class="table-wrap" tabindex="0" role="region" aria-label="累计统计分维度明细，可横向滚动"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}
// 累计统计卡片组：6 张卡一次写完；不可用时整组走占位（不显示 0，也不显示 NaN/undefined）。
function renderUsageStats(d) {
  const summary = usageSummaryOf(d);
  const stats = $("statHubGrid");
  stats.setAttribute("aria-busy", "false");
  // 不可用 = 没加载完 / 读取失败 / 后端无 summary / 上报关闭 / 未落盘。五种情况都整组占位，
  // 因为此时 summary 里的 0 只是"没在记账"，显示成"累计调用 0 次"等于把未知伪装成事实。
  const reason = usageUnavailableReason(d);
  if (!summary || reason) {
    for (const id of ["usageStatRequests", "usageStatPrompt", "usageStatCompletion", "usageStatTokens", "usageStatCredits", "usageStatCached"]) {
      $(id).textContent = "—";
    }
    $("usageStatRequestsNote").textContent = "成功 — · 失败 —";
    $("usageStatCreditsNote").textContent = "未记录消耗的调用 — 次";
    const tag = $("usageStatCreditsTag");
    tag.hidden = true; tag.textContent = "—"; tag.className = "tag";
    $("usageStatCachedNote").textContent = "占上传 token 的比例：—";
    // 三种"没有数字"的原因不能混成一句话：还没回来是等待，读取失败是故障，无 summary/未落盘是
    // 后端能力或配置状态。混着说会让运维对着"统计不可用"去查错方向。
    $("usageStatsRange").textContent = !d
      ? (state.usageError ? "统计范围：统计不可用（使用日志读取失败）" : "统计范围：正在加载累计统计…")
      : `统计范围：累计统计不可用（${reason}）`;
    return;
  }
  const requests = usageInt(summary.requests) ?? 0;
  const success = usageInt(summary.success) ?? 0;
  const failed = usageInt(summary.failed) ?? 0;
  $("usageStatRequests").textContent = usageCount(requests);
  // 成功/失败分开上色，不用一句话带过：失败数是秒判"网关健康度"的信息。
  $("usageStatRequestsNote").innerHTML = `<span class="s-ok">成功 ${esc(usageCount(success))}</span> · ${failed > 0 ? `<span class="s-warn">失败 ${esc(usageCount(failed))}</span>` : '<span class="s-dim">失败 0</span>'}`;
  $("usageStatPrompt").textContent = usageCount(summary.prompt_tokens);
  $("usageStatCompletion").textContent = usageCount(summary.completion_tokens);
  $("usageStatTokens").textContent = usageCount(summary.total_tokens);
  const credits = usageInt(summary.credits_used);
  const unknown = usageInt(summary.credits_unknown) ?? 0;
  $("usageStatCredits").textContent = usageCreditsText(credits);
  const tag = $("usageStatCreditsTag");
  // credits_unknown = **成功调用**里没能观测到扣费的次数。钱可能花了没记上账，所以显示的额度总计
  // 是下界，必须显式标注，不能让人误以为是精确值。
  if (unknown > 0) { tag.hidden = false; tag.textContent = `${usageCount(unknown)} 次未记录`; tag.className = "tag warn"; }
  else { tag.hidden = true; tag.textContent = "—"; tag.className = "tag"; }
  // credits_failed_unknown = 失败调用的同类计数。失败本就不产生消耗，所以它**不是**告警，
  // 也不能并进上面的数字（相加会虚增"可能多花的钱"）。只在末尾补一句低强调说明，>0 才出现。
  const failedUnknown = usageInt(summary.credits_failed_unknown) ?? 0;
  $("usageStatCreditsNote").innerHTML = (unknown > 0
    ? `未记录消耗的调用 <span class="s-warn">${esc(usageCount(unknown))}</span> 次，实际消耗可能高于此值`
    : "未记录消耗的调用 0 次")
    + (failedUnknown > 0 ? ` <span class="s-dim">· 另有 ${esc(usageCount(failedUnknown))} 次失败调用无消耗数据</span>` : "");
  const cached = usageInt(summary.cached_tokens);
  $("usageStatCached").textContent = usageCount(cached);
  const rate = usageCacheRate(cached, summary.prompt_tokens);
  // prompt_tokens 为 0 时没有分母：显示 "—"，不做 0 除法，也不假装命中率是 0%。
  $("usageStatCachedNote").textContent = rate === null
    ? "占上传 token 的比例：—（无上传 token 记录）"
    : `占上传 token 的 ${(rate * 100).toFixed(rate < .1 ? 2 : 1)}%`;
  // 统计范围：把口径与首末记录一起说清。缺 first/last（从未记录）时降级为"尚无记录"。
  const first = usageMinute(summary.first_time), last = usageMinute(summary.last_time);
  const scope = first && last
    ? `统计范围：${first} 起 · 最近记录 ${last}（当前日志文件覆盖范围；清空后归零）`
    : first ? `统计范围：${first} 起（尚无最近记录时间）` : "统计范围：尚无记录";
  // scan_incomplete=true：本次统计有文件没读完（被占用/读盘错误/存在超长脏行）。
  // 不完整的数字在形态上与完整的一模一样，不说出来就会被当成"真的只用了这么多"，
  // 所以这里必须显式提示"数字可能偏低"，并用警告色——它是需要人去看一眼的信号。
  $("usageStatsRange").innerHTML = summary.scan_incomplete === true
    ? `${esc(scope)} <span class="s-warn">· 本次统计不完整：部分日志文件未能读取，数字可能偏低（后端不会缓存该结果，下次刷新会重算）</span>`
    : esc(scope);
}
// 页签 / 排序 / 展开切换：只改本地 UI 态后重绘明细，数据不重新拉取（summary 已随日志一起回来）。
function selectUsageTab(tab) {
  if (!USAGE_TABS[tab] || state.usageTab === tab) return;
  state.usageTab = tab;
  state.usageShowAll = false;
  renderUsageBreakdown(state.usage);
}
// 点同一列表头 = 反向；点新列 = 该列降序（先看"最大的是谁"是运维的默认诉求）。
function sortUsageBy(key) {
  if (!USAGE_COLUMNS.some(c => c.key === key)) return;
  if (state.usageSort === key) state.usageSortDir = state.usageSortDir === "asc" ? "desc" : "asc";
  else { state.usageSort = key; state.usageSortDir = "desc"; }
  renderUsageBreakdown(state.usage);
}
function toggleUsageRows() {
  state.usageShowAll = !state.usageShowAll;
  renderUsageBreakdown(state.usage);
}
function usageClock(value) {
  // 同一个 Go 零时间坑：旧响应里的 "0001-01-01T00:00:00Z" 会被 toLocaleTimeString 渲染成
  // 「08:00:00」，看起来像一次真实发生的清空/调用。这里统一退回 "—"。
  if (!value || usageIsZeroTime(value)) return "—";
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleTimeString("zh-CN", { hour12: false });
}
// 字节数按 KB/MB/GB 展示（二进制 1024 进制，与后端 MaxBytes/MaxSizeMB 的口径一致）。
function usageBytes(n) {
  const v = usageInt(n);
  if (v === null || v < 0) return "—";
  if (v < 1024) return v + " B";
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + " KB";
  if (v < 1024 * 1024 * 1024) return (v / 1048576).toFixed(1) + " MB";
  return (v / 1073741824).toFixed(2) + " GB";
}
// 占用档位 → 颜色：只用现成的 green/amber/red（--accent / --warn / --err）。
// 阈值 70% 预警、90% 危险、>=100% 越界；越界不是「后端坏了」，而是上限已顶住。
function usageLevel(ratio) {
  if (typeof ratio !== "number") return "unknown";
  if (ratio >= 1) return "over";
  if (ratio >= .9) return "crit";
  if (ratio >= .7) return "warn";
  return "ok";
}
const USAGE_LEVEL_TEXT = { ok: "正常", warn: "接近上限", crit: "即将写满", over: "已达上限", unknown: "—" };
const USAGE_LEVEL_CLASS = { ok: "ok", warn: "warn", crit: "crit", over: "crit", unknown: "" };
// 磁盘占用卡片：占用 / 上限 + 进度条 + 备份份数 + 上次清空。
// limit_bytes=0 表示不落盘（File 为空），此时不能让页面显示「0 B / 0 B」，要直说仅内存。
function renderUsageSize(si) {
  const bar = $("usageBar"), fill = $("usageBarFill"), tag = $("usageSizeTag"), note = $("usageSizeNote");
  if (!si) {
    $("usageSize").textContent = "—"; tag.textContent = "—"; tag.className = "tag";
    bar.dataset.level = "ok"; bar.removeAttribute("title"); bar.setAttribute("aria-label", "日志磁盘占用比例：未知");
    fill.style.width = "0"; note.textContent = "磁盘上的追加式日志文件";
    return null;
  }
  const total = usageInt(si.total_bytes), limit = usageInt(si.limit_bytes);
  const backups = usageInt(si.max_backups), fileBytes = usageInt(si.file_bytes), backupBytes = usageInt(si.backup_bytes);
  const bits = [];
  // limit_bytes=0 表示不落盘（File 为空）：不能说「0 B / 0 B」，也不该提「备份份数」。
  if (!limit) {
    $("usageSize").innerHTML = `不落盘<small>（仅内存）</small>`;
    tag.textContent = "仅内存"; tag.className = "tag";
    bar.dataset.level = "ok";
    bar.setAttribute("aria-label", "日志不落盘，仅保留在内存中");
    bar.removeAttribute("title");
    fill.style.width = "0";
    bits.push("内存保留 " + (usageInt(si.memory_entries) ?? 0) + " 条");
  } else {
    const used = total ?? 0;
    const ratio = used / limit;
    const level = usageLevel(ratio);
    const pct = Math.min(100, Math.max(0, ratio * 100));
    $("usageSize").innerHTML = `${esc(usageBytes(used))} <small>/ ${esc(usageBytes(limit))}（上限）</small>`;
    tag.textContent = USAGE_LEVEL_TEXT[level];
    tag.className = "tag " + USAGE_LEVEL_CLASS[level];
    bar.dataset.level = level;
    // 宽度只到 100%，越界靠颜色 + 描边表达，不撑破卡片。
    // 宽度只到 100%，越界靠颜色 + 描边表达，不撑破卡片；极小占用也留一条可见细缝。
    fill.style.width = ratio <= 0 ? "0" : ratio >= 1 ? "100%" : Math.max(pct, 1.5).toFixed(2) + "%";
    const ratioText = (ratio * 100).toFixed(ratio < .1 ? 2 : 1) + "%";
    bar.setAttribute("aria-label", `日志磁盘占用 ${usageBytes(used)}，上限 ${usageBytes(limit)}（已用 ${ratioText}）`);
    bar.title = `主文件 ${usageBytes(fileBytes)} + 备份 ${usageBytes(backupBytes)} = ${usageBytes(used)}（上限 ${usageBytes(limit)}）`;
    if (backups !== null) bits.push("保留备份 " + backups + " 份");
    if (backupBytes) bits.push("备份占用 " + usageBytes(backupBytes));
  }
  // cleared_at 现在是 null（从未清空），老响应可能是 Go 零时间：两者都该说「从未清空」，
  // 所以判定用"真值且非零时间"，而不是 !== undefined——否则会显示「上次清空：—」，读起来像清空失败。
  const cleared = si.cleared_at && !usageIsZeroTime(si.cleared_at) ? usageClock(si.cleared_at) : "从未清空";
  bits.push("上次清空：" + cleared);
  note.textContent = bits.join(" · ");
  return si;
}
// last_error 非空 = 轮转/写入失败过，上限可能已静默失效：这是告警，不是致命错误。
function renderUsageWarnings(si) {
  const banner = $("usageError");
  const parts = [];
  if (state.usageError) parts.push("自动刷新失败：" + state.usageError + "，以下保留上次数据。");
  if (si && si.last_error) parts.push("落盘异常：" + si.last_error + " —— 磁盘上限可能未被强制执行，请检查磁盘空间与文件权限。");
  banner.textContent = parts.join(" ");
  banner.hidden = !parts.length;
}
// 瞬时提示：清空成功/失败后告诉运维结果与释放量，几秒后自动收起。
function setUsageNotice(text, kind = "success", ms = 6000) {
  const el = $("usageNotice");
  clearTimeout(state.usageNoticeTimer);
  state.usageNotice = text || "";
  el.dataset.kind = kind;
  el.textContent = state.usageNotice;
  el.hidden = !state.usageNotice;
  if (state.usageNotice) state.usageNoticeTimer = setTimeout(() => { el.hidden = true; state.usageNotice = ""; }, ms);
}
function usageDuration(ms) {
  const n = usageInt(ms);
  if (n === null || n <= 0) return "—";
  return n < 1000 ? n + "ms" : (n / 1000).toFixed(2) + "s";
}
function usageMillis(ms) {
  const n = usageInt(ms);
  return n === null || n <= 0 ? "—" : Math.round(n) + "ms";
}
// 与累计统计一致：仅 2xx 算成功，其余已知状态算失败；缺失状态单独展示。
function usageStatusClass(code) {
  const n = usageInt(code);
  if (n === null || n <= 0) return "s-dim";
  if (n >= 200 && n < 300) return "s-ok";
  return "s-err";
}
// 余额没刷上时不编数字：给占位符，并把后端给的原因透出来（刷新失败或从未观测到余额）。
function usageCreditsCell(e) {
  if (!e.credits_known) {
    return `<span class="s-dim">-</span><div class="hint status-note">${esc(e.balance_error || "未获取到余额")}</div>`;
  }
  const before = usageInt(e.credits_before), after = usageInt(e.credits_after);
  return `<b class="usage-credits" title="扣除前 ${before ?? "—"} → 扣除后 ${after ?? "—"}">${esc(usageInt(e.credits_used) ?? 0)}</b>`;
}
function usageErrorMessage(e) {
  if (e.status === 404) return "接口尚未就绪（HTTP 404）：/admin/api/usage 暂不可用，后端补齐后会自动恢复。";
  return e.message;
}
function renderUsage() {
  const box = $("usageTable"), d = state.usage;
  // 清空进行中时按钮保持禁用（防重复触发），回到这里再按最新 enabled 状态收敛。
  syncUsageClearBtn(d);
  renderUsageWarnings(d && d.size);
  if (!d) {
    $("usageTotal").textContent = "—"; $("usageKept").textContent = "—"; $("usageFile").textContent = "—";
    $("usageCount").textContent = "—"; $("usageFileNote").textContent = "磁盘上的追加式日志文件";
    $("usageUpdated").textContent = "尚未同步";
    $("usageWindow").textContent = "按时间倒序";
    // 翻页控件一并复位：否则上一次的"共 200 条 · 第 3 / 10 页"会残留在加载态或错误态上。
    resetUsageStreamControls();
    renderUsageSize(null);
    // 累计统计与明细同样不能显示旧值：数据没回来时它们必须走各自的占位分支。
    renderUsageStats(null);
    renderUsageBreakdown(null);
    renderDashboard();
    box.innerHTML = state.usageError
      ? `<div class="empty"><strong>暂时无法读取使用日志</strong>${esc(state.usageError)}</div>`
      : '<div class="empty">正在加载使用日志…</div>';
    return;
  }
  syncLogOptions();
  const entries = filteredLogs();
  $("logFilterSummary").textContent = `${historyScope()} · 匹配 ${usageCount(entries.length)} 条`;
  $("exportLogs").disabled = !entries.length;
  $("usageTotal").textContent = d.total ?? 0;
  $("usageKept").textContent = d.kept ?? 0;
  $("usageFile").textContent = d.file || "未配置";
  $("usageFileNote").textContent = d.enabled === false ? "后台上报已关闭，仅保留既有文件内容" : "磁盘上的追加式日志文件";
  renderUsageSize(d.size);
  // 累计统计/明细渲染放在 entries 早退分支之前：日志表为空（例如刚清空）时统计数字仍然要显示。
  renderUsageStats(d);
  renderUsageBreakdown(d);
  renderDashboard();
  $("usageCount").textContent = entries.length;
  $("usageUpdated").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", { hour12: false });
  $("usageWindow").textContent = entries.length
    ? "最新调用 " + usageClock(entries[0].time) + " · 按时间倒序"
    : "按时间倒序";
  if (!entries.length) {
    resetUsageStreamControls();
    if ((d.entries || []).length) { box.innerHTML = '<div class="empty"><strong>没有匹配的请求</strong>试试其他搜索词，或点击「重置」恢复全部已加载日志。</div>'; return; }
    const where = d.file
      ? `网关还没有记录到调用；产生一次请求后即可在 <span class="mono">${esc(d.file)}</span> 看到记录。`
      : "使用日志未落盘（仅内存模式），产生一次请求后这里会出现记录，进程重启即丢失。";
    box.innerHTML = `<div class="empty"><strong>暂无使用日志</strong>${d.enabled === false
      ? "后台上报处于关闭状态，开启后这里会显示调用记录。"
      : where}</div>`;
    return;
  }
  // 分页：entries 已倒序（最新在前），按当前页切出这一段。
  // 页码先经 usageCurrentPage 夹紧，再算切片——否则"在第 8 页把每页条数调到 200"
  // 这种组合会算出负的起始下标（slice(-n) 会取到尾巴，行序看上去正常但内容是错的）。
  const size = usagePageSize();
  const pages = usagePageCount(entries.length);
  const page = usageCurrentPage(entries.length);
  const start = (page - 1) * size;
  const shown = entries.slice(start, start + size);
  renderUsagePager(entries.length, page, pages, start, shown.length);
  // 表头计数必须把"翻页"这件事说清：只写"共 200 条"会让人以为下面是全部 200 条。
  $("usageStreamCount").textContent = state.usagePageSize === "all" ? `全部 ${usageCount(entries.length)} 条` : `共 ${usageCount(entries.length)} 条 · 第 ${page} / ${pages} 页 · 每页 ${size} 条`;
  const head = ["序号", "时间", "账号", "产品线", "模型", "模式", "状态", "耗时", "TTFB", "消耗积分", "输入/输出 token", "缓存命中"]
    .map(h => `<th scope="col">${h}</th>`).join("");
  const indices = new Map(state.usage.entries.map((e,i)=>[e,i]));
  const rows = shown.map(e => {
    const uid8 = e.uid8 || String(e.uid || "").slice(0, 8) || "—";
    const status = usageInt(e.status), cached = usageInt(e.cached_tokens);
    const prompt = usageInt(e.prompt_tokens), completion = usageInt(e.completion_tokens), total = usageInt(e.total_tokens);
    return `<tr>
    <td><button class="detail-link mono" data-request="${indices.get(e)}" aria-label="查看请求 ${esc(e.seq ?? "")} 详情">#${esc(usageInt(e.seq) ?? "—")} ↗</button></td>
    <td class="mono">${esc(usageClock(e.time))}</td>
    <td><span class="account-name" title="${esc((e.uid || uid8) + (e.nickname ? " · " + e.nickname : ""))}"><span class="mono">${esc(uid8)}</span>${e.nickname ? " · " + esc(e.nickname) : ""}</span></td>
    <td><span class="tag${e.realm === "workbuddy" || e.realm === "codebuddy" || e.realm === "ai" ? " intl" : e.realm === "cn" ? " cn" : ""}">${esc(e.realm || "—")}</span></td>
    <td>${esc(e.model || "—")}</td>
    <td class="hint">${esc(e.mode || "—")}</td>
    <td><span class="status ${usageStatusClass(e.status)}">${status === null || status <= 0 ? "未知" : esc(status)}</span></td>
    <td>${esc(usageDuration(e.duration_ms))}</td>
    <td>${esc(usageMillis(e.ttfb_ms))}</td>
    <td>${usageCreditsCell(e)}</td>
    <td title="总计 ${total ?? "—"} tokens">${prompt === null ? "—" : esc(prompt)} / ${completion === null ? "—" : esc(completion)}</td>
    <td>${cached !== null && cached > 0 ? esc(cached) : '<span class="s-dim">-</span>'}</td>
  </tr>`;
  }).join("");
  box.innerHTML = `<div class="table-wrap" tabindex="0" role="region" aria-label="使用日志列表，可横向滚动"><table><thead><tr>${head}</tr></thead><tbody>${rows}</tbody></table></div>`;
}
// 清空按钮是破坏性操作：enabled=false 说明没有落盘日志可清 → 禁用并说明原因；
// 请求在途时同样禁用，避免双击发出第二个 DELETE（后端虽幂等，但这里不该依赖它）。
function syncUsageClearBtn(d) {
  const btn = $("usageClearBtn");
  const loaded = !!d, canClear = loaded && d.enabled !== false;
  btn.disabled = state.usageClearing || state.usageBusy || !canClear;
  btn.setAttribute("aria-busy", String(state.usageClearing));
  btn.title = !loaded ? "等待使用日志加载完成"
    : state.usageBusy ? "正在刷新使用日志，请稍候"
    : canClear ? "清空内存缓冲、落盘日志及其全部备份"
    : "使用日志上报已关闭，没有需要清空的内容";
}
// 复位单条日志区的控件（加载态 / 错误态 / 空数据时调用）。
// 必须显式复位：否则上一次的"共 200 条 · 第 3 / 10 页"会残留在空态上，读起来像还有数据。
function resetUsageStreamControls() {
  $("usageStreamCount").textContent = "";
  $("usagePager").hidden = true;
}
// renderUsagePager 渲染翻页条：只在多于一页时出现（单页时整条隐藏，不留空占位）。
// 首/末页按钮到边界即禁用而不是隐藏——按钮位置固定，用户不必追着位置找。
function renderUsagePager(total, page, pages, start, shownCount) {
  const pager = $("usagePager");
  pager.hidden = pages <= 1;
  $("usagePageInfo").textContent = `第 ${start + 1}–${start + shownCount} 条 / 共 ${total} 条`;
  $("usagePageFirst").disabled = page <= 1;
  $("usagePagePrev").disabled = page <= 1;
  $("usagePageNext").disabled = page >= pages;
  $("usagePageLast").disabled = page >= pages;
}
// 翻页：只改页码并重绘本表，不重新拉数据（数据已全在 state.usage.entries 里）。
function goUsagePage(action) {
  const total = filteredLogs().length;
  const pages = usagePageCount(total);
  const cur = usageCurrentPage(total);
  const next = action === "first" ? 1
    : action === "last" ? pages
    : action === "prev" ? cur - 1
    : cur + 1;
  state.usagePage = Math.min(Math.max(1, next), pages);
  renderUsage();
}
// 每页条数变更：下拉选预设值，或选"自定义…"后由输入框给出数字。
// 改条数一律回到第 1 页——当前页在新的分页下不再对应原来的内容，停在原地会让人以为"跳了"。
function onUsagePageSizeChange() {
  const sel = $("usagePageSize");
  const custom = $("usagePageSizeCustom");
  const isCustom = sel.value === "custom";
  custom.hidden = !isCustom;
  if (isCustom) {
    // 自定义：沿用上次填过的值（state.usagePageCustom）。焦点一律移到输入框——
    // 选了"自定义…"就是要打字，让用户再按一次 Tab 是多余的（键盘用户会以为没反应）。
    // 注意 focus 必须在 renderUsage 之前/之后都不影响：输入框不在被重绘的 #usageTable 里。
    const remembered = state.usagePageCustom;
    if (remembered) {
      state.usagePageSize = Number(remembered);
      state.usagePage = 1;
      renderUsage();
    }
    // 没记住值时不动 state：保持上一次生效的每页条数，等用户输入再切换。
    // 否则"点进自定义"会立刻把表变成默认 20 条，看起来像设置被重置了。
    custom.focus();
    custom.select();
    return;
  }
  state.usagePageSize = sel.value === "all" ? "all" : Number(sel.value);
  state.usagePage = 1;
  renderUsage();
}
// 自定义输入：边打字边生效（无需回车），但只在解析出合法正整数时才重绘，
// 否则每敲一个字符都把表重建一次（清空输入框的瞬间还会闪一下默认值）。
function onUsagePageSizeCustomInput() {
  const raw = $("usagePageSizeCustom").value.trim();
  state.usagePageCustom = raw;
  const n = usageInt(raw);
  if (!Number.isSafeInteger(n) || n < 1) return;
  state.usagePageSize = n;
  state.usagePage = 1;
  renderUsage();
}
const USAGE_CLEAR_CONFIRM = "确认清空使用日志？\n\n将删除磁盘上的日志文件及其全部备份，并清空内存中的调用记录，同时释放这部分磁盘占用。此操作不可撤销，网关无法恢复已删除的记录。";
// 清空流程：confirm 确认 → DELETE → 直接用响应里的 size/removed 收口 → 再拉一次兜底对齐。
async function clearUsage() {
  if (state.usageClearing || state.usageBusy) return false;
  if (!window.confirm(USAGE_CLEAR_CONFIRM)) return false;
  state.usageClearing = true;
  syncUsageClearBtn(state.usage);
  busy("usageClearBtn", true, "清空中…");
  setUsageNotice("");
  try {
    const d = await api("/admin/api/usage", { method: "DELETE" });
    // 后端把清空后的新快照（含 summary）一起返回：先用它立即归零，避免「点完还显示旧占用/旧累计」。
    // summary 缺失时传 null，让统计卡片走占位分支——绝不把上一份统计留在屏幕上冒充新值。
    const freed = usageInt(d.removed) ?? 0;
    const fresh = d.summary && typeof d.summary === "object" ? d.summary : null;
    if (state.usage) {
      state.usage = Object.assign({}, state.usage, {
        enabled: d.enabled, file: d.file, size: d.size || state.usage.size, entries: [], total: 0, kept: 0, summary: fresh
      });
    }
    state.usageError = "";
    renderUsage();
    setUsageNotice(freed > 0
      ? `已清空使用日志，释放 ${usageBytes(freed)}；磁盘占用与记录列表已同步。`
      : "已清空使用日志，本次没有可释放的磁盘占用（记录已在内存中归零）。");
    toast(freed > 0 ? "使用日志已清空，释放 " + usageBytes(freed) : "使用日志已清空");
    await loadUsage(true);
    return true;
  } catch (e) {
    const msg = usageErrorMessage(e);
    setUsageNotice("清空日志失败：" + msg + "，日志内容保持不变。", "error");
    toast("清空日志失败：" + msg, 5000, "error");
    return false;
  } finally {
    state.usageClearing = false;
    busy("usageClearBtn", false);
    syncUsageClearBtn(state.usage);
  }
}
async function loadUsage(quiet = false) {
  if (!state.authenticated) return;
  if (state.usageBusy) return false;
  state.usageBusy = true;
  const version = ++state.usageVersion;
  if (!quiet) busy("usageRefreshBtn", true, "刷新中…");
  $("usageTable").setAttribute("aria-busy", "true");
  // 累计统计与明细同样标记在途：它们与日志表是同一次请求的数据，收尾时由各自的渲染函数复位。
  $("statHubGrid").setAttribute("aria-busy", "true");
  $("usageBreakdown").setAttribute("aria-busy", "true");
  $("usageBreakdownTable").setAttribute("aria-busy", "true");
  syncUsageClearBtn(state.usage);
  if (!state.usage) $("usageTable").innerHTML = '<div class="empty">正在加载使用日志…</div>';
  try {
    const d = await api("/admin/api/usage");
    if (version !== state.usageVersion) return false;
    state.usage = d; state.usageError = "";
    renderUsage();
    return true;
  } catch (e) {
    // 404 / 鉴权失败 / 超时都在这里落地成可读文案，绝不让未捕获异常打断页面。
    if (version !== state.usageVersion) return false;
    state.usageError = usageErrorMessage(e);
    renderUsage();
    return false;
  } finally {
    state.usageBusy = false;
    if (!quiet) busy("usageRefreshBtn", false);
    $("usageTable").setAttribute("aria-busy", "false");
    // 刷新在途时按钮禁用并提示原因；刷新收尾后要让按钮回到可点状态。
    syncUsageClearBtn(state.usage);
  }
}
  // 用量面板的实时性由 SSE 推送承担（服务端在用量变化时把明细随事件一起推来），
  // 因此这里**没有轮询**：开关只决定"要不要应用推送来的用量"，关掉就保留当前快照。
  // 手动「刷新」按钮始终可用，作为一次性补齐手段。
  function setUsageAuto(fromUser) {
    const auto = $("usageAuto").checked;
    state.usageAuto.checked = auto;
    $("usageAutoHint").textContent = auto ? "实时：随推送更新" : "已暂停（保留当前快照）";
    // 用户刚打开时补一次全量，否则要等下一次状态变化才看到较新的日志。
    if (fromUser && auto) loadUsage(true);
  }
  setUsageAuto(false);
  $("usageAuto").addEventListener("change", () => setUsageAuto(true));
// 排序表头用事件委托：明细表每次重绘都重建 thead，逐个 onmouse 绑定会在重绘后失效。
$("usageBreakdownTable").addEventListener("click", e => {
  const btn = e.target.closest("button[data-usage-sort]");
  if (btn) sortUsageBy(btn.dataset.usageSort);
});
// 每页条数：下拉含预设值与"自定义…"。两个监听器分开注册——
// select 只在选项变化时触发，input 是逐字符触发，合并成一个会让"打字中途"也走下拉逻辑。
$("usagePageSize").addEventListener("change", onUsagePageSizeChange);
$("usagePageSizeCustom").addEventListener("input", onUsagePageSizeCustomInput);
$("accounts").addEventListener("click", e => {
  // 单号刷新：拿到 uid 直接刷该号（不经过整表 index，避免列表重绘后索引错位）。
  const refresh = e.target.closest("button[data-uid-refresh]");
  if (refresh) { refreshOneBalance(refresh.dataset.uidRefresh); return; }
  const selection = e.target.closest("button[data-selection]");
  if (selection) { openSelection(Number(selection.dataset.selection)); return; }
  const toggle = e.target.closest("button[data-toggle]");
  if (toggle) { toggleAccount(Number(toggle.dataset.toggle)); return; }
  const button = e.target.closest("button[data-delete]");
  if (button) confirmDelete(Number(button.dataset.delete), button);
});
// 排除勾选框控制落位下拉的启用态：未排除时落位无意义，禁用掉避免误解。
$("selectionExcluded").addEventListener("change", syncSelectionForm);
$("selectionDialog").addEventListener("cancel", e => { if (state.selectionBusy) e.preventDefault(); });
$("selectionDialog").addEventListener("close", () => {
  if (!state.selectionBusy) state.selectionTarget = null;
  const trigger = state.selectionTrigger;
  if (trigger && trigger.isConnected) trigger.focus(); else $("accountSearch").focus();
});
for (const id of ["accountSearch", "realmFilter", "statusFilter"]) $(id).addEventListener(id === "accountSearch" ? "input" : "change", renderAccounts);
$("deleteDialog").addEventListener("cancel", e => { if (state.deleteBusy) e.preventDefault(); });
$("deleteDialog").addEventListener("close", () => {
  if (!state.deleteBusy) state.deleteTarget = null;
  const trigger = state.deleteTrigger;
  if (trigger && trigger.isConnected) trigger.focus(); else $("accountSearch").focus();
});
try { localStorage.removeItem("wb2api_key"); } catch (_) { /* 旧管理 Key 不再保存在浏览器 */ }
window.addEventListener("pagehide", () => {
  clearTimeout(state.creditsTimer); clearTimeout(state.usageTimer);
  clearTimeout(state.accountsTimer); stopPoll();
  stopEvents(); // 关掉长连接，避免服务端积累无主订阅者
});
// 标签页切回前台时立即补齐一次：网络可能已断、连接可能已被回收，
// 顺手把状态与连接都拉回最新。这只是"尽快恢复"的优化，
// 真正的兜底是 30 秒无条件轮询（不依赖任何可见性事件，见 refreshAccountsQuietly）。
document.addEventListener("visibilitychange", () => {
  if (document.hidden) return;
  refreshAccountsQuietly();
  if (state.accountsAuto.checked && !state.eventsAbort) connectEvents();
});
// 初始化：把下拉对齐到 state 里的默认每页条数（HTML 里的 selected 只是静态初值，
// 真正的事实来源是 state——两者若不一致，首屏渲染会与控件显示的数字对不上）。
$("usagePageSize").value = String(state.usagePageSize);
const consoleActions = {
  refresh: () => refreshAll(false),
  usageUid: () => selectUsageTab("by_uid"),
  usageModel: () => selectUsageTab("by_model"),
  usageRealm: () => selectUsageTab("by_realm"),
  usageRows: () => toggleUsageRows(),
  reload: () => reloadAuths(),
  usageRefresh: () => loadUsage(),
  usageClear: () => clearUsage(),
  pageFirst: () => goUsagePage("first"),
  pagePrev: () => goUsagePage("prev"),
  pageNext: () => goUsagePage("next"),
  pageLast: () => goUsagePage("last"),
  startLogin: () => startLogin(),
  openLogin: () => openLogin(),
  copyLogin: () => copyLogin(),
  closeDelete: () => closeDelete(),
  deleteAccount: () => deleteAccount(),
  closeSelection: () => closeSelection(),
  saveSelection: () => saveSelection(),
};
document.addEventListener("click", event => {
  const button = event.target.closest("button[data-console-action]");
  if (!button || button.disabled) return;
  const action = button.dataset.consoleAction;
  if (Object.hasOwn(consoleActions, action)) consoleActions[action]();
});
initializeConsole();
// access.js checks the session before loading any private console data.
