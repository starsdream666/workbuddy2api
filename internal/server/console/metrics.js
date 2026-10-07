/* Pure presentation helpers. No network, DOM or persistent state. */
(function (root) {
  "use strict";
  const number = value => value === null || value === undefined || value === "" || !Number.isFinite(Number(value)) ? null : Number(value);
  const failed = entry => { const code = number(entry.status); return code !== null && code > 0 && (code < 200 || code >= 300); };
  const successful = entry => { const code = number(entry.status); return code !== null && code >= 200 && code < 300; };
  function filterEntries(entries, filters = {}, now = Date.now()) {
    const query = String(filters.query || "").trim().toLowerCase();
    const hours = number(filters.hours);
    return entries.filter(e => {
      if (query && ![e.uid, e.uid8, e.nickname, e.model, e.realm, e.status, e.seq, e.error].join(" ").toLowerCase().includes(query)) return false;
      if (filters.status === "failed" && !failed(e)) return false;
      if (filters.status === "success" && !successful(e)) return false;
      if (filters.status === "unknown" && (failed(e) || successful(e))) return false;
      if (filters.realm && e.realm !== filters.realm) return false;
      if (filters.model && e.model !== filters.model) return false;
      if (hours && !(Date.parse(e.time) >= now - hours * 3600000 && Date.parse(e.time) <= now)) return false;
      if (filters.from !== undefined && !(Date.parse(e.time) >= filters.from && Date.parse(e.time) <= now)) return false;
      return true;
    });
  }
  function aggregate(entries) {
    const durations = entries.map(e => number(e.duration_ms)).filter(n => n !== null && n >= 0);
    const firstBytes = entries.map(e => number(e.ttfb_ms)).filter(n => n !== null && n > 0);
    const mean = values => values.length ? values.reduce((a,b) => a+b,0) / values.length : null;
    const success = entries.filter(successful).length, errors = entries.filter(failed).length;
    return { requests: entries.length, success, failed: errors, unknown: entries.length-success-errors, avgDuration: mean(durations), avgTTFB: mean(firstBytes), successRate: success+errors ? success/(success+errors)*100 : null };
  }
  function metricValue(entry, metric) {
    if (metric === "credits") return entry.credits_known === true ? number(entry.credits_used) : null;
    if (metric === "tokens") return number(entry.total_tokens) ?? ((number(entry.prompt_tokens)||0)+(number(entry.completion_tokens)||0));
    return 1;
  }
  function rangeStart(range, now = Date.now()) {
    const date = new Date(now);
    if (range === "week") { date.setHours(0,0,0,0); date.setDate(date.getDate()-(date.getDay()+6)%7); return date.getTime(); }
    if (range === "month") { date.setHours(0,0,0,0); date.setDate(1); return date.getTime(); }
    return range === "all" ? undefined : now-Number(range)*3600000;
  }
  function timeline(entries, hours, now = Date.now(), count = 24, start) {
    const valid = entries.filter(e => Number.isFinite(Date.parse(e.time)) && Date.parse(e.time) <= now);
    const from = start ?? (hours ? now-hours*3600000 : valid.reduce((first,e) => Math.min(first,Date.parse(e.time)),now-60000));
    const width = Math.max(1,(now-from)/count);
    const buckets = Array.from({length:count},(_,i) => ({time:from+i*width,success:0,failed:0,unknown:0,tokens:0,credits:0,creditsUnknown:0}));
    valid.forEach(e => { const t=Date.parse(e.time); if(t<from) return; const bucket=buckets[Math.min(count-1,Math.floor((t-from)/width))]; bucket[failed(e)?"failed":successful(e)?"success":"unknown"]++; bucket.tokens+=metricValue(e,"tokens"); const credits=metricValue(e,"credits"); if(credits!==null)bucket.credits+=credits; else if(successful(e))bucket.creditsUnknown++; });
    return {from,to:now,buckets};
  }
  function dayKey(value) {
    const d = new Date(value);
    return Number.isFinite(d.getTime()) ? [d.getFullYear(),String(d.getMonth()+1).padStart(2,"0"),String(d.getDate()).padStart(2,"0")].join("-") : null;
  }
  function dailyUsage(entries, year, now = Date.now(), metric = "tokens", start) {
    const totals = new Map(), years = new Set([new Date(now).getFullYear()]);
    for (const entry of entries) {
      const time = Date.parse(entry.time);
      if (!Number.isFinite(time) || time > now || new Date(time).getFullYear() < 1970) continue;
      const date = dayKey(time), entryYear = new Date(time).getFullYear();
      years.add(entryYear);
      if (entryYear !== year || (start !== undefined && time < start)) continue;
      const day = totals.get(date) || {tokens:0,credits:0,creditsUnknown:0,prompt:0,completion:0,cached:0,requests:0};
      day.prompt += Math.max(0,number(entry.prompt_tokens) || 0);
      day.completion += Math.max(0,number(entry.completion_tokens) || 0);
      day.cached += Math.max(0,number(entry.cached_tokens) || 0);
      day.tokens += Math.max(0,metricValue(entry,"tokens"));
      const credits = metricValue(entry,"credits");
      if(credits !== null) day.credits += credits;
      else if(successful(entry)) day.creditsUnknown++;
      day.requests++;
      totals.set(date,day);
    }
    const days = [], today = dayKey(now);
    for (let date = new Date(year,0,1,12); date.getFullYear() === year; date.setDate(date.getDate()+1)) {
      const key = dayKey(date);
      days.push({date:key,month:date.getMonth(),weekday:date.getDay(),future:key>today,outside:start !== undefined && key<dayKey(start),...(totals.get(key)||{tokens:0,credits:0,creditsUnknown:0,prompt:0,completion:0,cached:0,requests:0})});
    }
    const max = days.reduce((n,d) => Math.max(n,d[metric]),0);
    for (const day of days) day.level = day[metric] > 0 ? Math.min(4,Math.max(1,Math.ceil(day[metric]/max*4))) : 0;
    return {days,years:[...years].sort((a,b)=>b-a),max,total:days.reduce((n,d)=>n+d[metric],0),activeDays:days.filter(d=>d[metric]>0).length,creditsUnknown:days.reduce((n,d)=>n+d.creditsUnknown,0)};
  }
  const dailyTokens = (entries,year,now) => dailyUsage(entries,year,now);
  function csv(entries) {
    const columns=[["序号","seq"],["时间","time"],["账号 UID","uid"],["昵称","nickname"],["产品线","realm"],["模型","model"],["状态","status"],["耗时 ms","duration_ms"],["TTFB ms","ttfb_ms"],["消耗积分","credits_used"],["输入 token","prompt_tokens"],["输出 token","completion_tokens"],["缓存 token","cached_tokens"]];
    const cell=value => { let s=String(value ?? ""); if (/^[=+@\-\t\r\n]/.test(s)) s="'"+s; return '"'+s.replace(/"/g,'""')+'"'; };
    return "\uFEFF" + [columns.map(c=>cell(c[0])).join(","),...entries.map(e=>columns.map(c=>cell(e[c[1]])).join(","))].join("\r\n");
  }
  const api={number,failed,successful,filterEntries,aggregate,timeline,metricValue,rangeStart,dayKey,dailyUsage,dailyTokens,csv};
  if (typeof module !== "undefined" && module.exports) module.exports=api;
  else root.ConsoleMetrics=api;
})(typeof globalThis !== "undefined" ? globalThis : this);
