(() => {
  let snapshot = null, busy = false, version = 0, conflicted = false;
  const controls = new Map();
  const rows = new Map(), navButtons = new Map();
  const categories = [
    {id:"common", label:"常用配置", help:"点击配置旁的星标，自定义你的常用项。仅保存在此浏览器。"},
    {id:"accounts", label:"账号与调度", help:"选择账号使用方式，调整并发、积分保底与会话保持。"},
    {id:"requests", label:"请求与稳定性", help:"控制请求大小、等待时间，以及限流和连续失败后的恢复策略。"},
    {id:"automation", label:"自动任务", help:"配置签到、旅行、保活与额度巡检。各渠道已有的任务覆盖仍然适用。"},
    {id:"logs", label:"日志与用量", help:"管理请求记录、磁盘占用与余额校准。"},
    {id:"compatibility", label:"兼容性", help:"调整请求指纹、提示词缓存与工具历史适配。"},
    {id:"other", label:"其他配置", help:"当前服务提供的其他配置。"},
    {id:"changed", label:"待保存", help:"集中检查所有分类的草稿；保存时统一应用全部修改。"}
  ];
  const favoriteKey = "wb2api.settings.favorites.v1";
  let selectedCategory = "common";
  let favorites = new Set(["pool.selection_mode","pool.max_in_flight","pool.credit_floor","session_sticky.enabled","schedule.checkin_enabled","usage_log.enabled"]);
  try {
    const saved = JSON.parse(localStorage.getItem(favoriteKey));
    if (Array.isArray(saved) && saved.every(key => typeof key === "string")) favorites = new Set(saved);
  } catch { /* Storage may be blocked; keep an in-memory preference. */ }
  function categoryOf(field) {
    if (field.key.startsWith("pool.breaker_") || field.key.startsWith("cooldown.") || /^(server|upstream)\./.test(field.key)) return "requests";
    if (/^(pool|session_sticky)\./.test(field.key)) return "accounts";
    if (field.key.startsWith("schedule.")) return "automation";
    if (field.key.startsWith("usage_log.")) return "logs";
    if (field.key.startsWith("features.")) return "compatibility";
    return "other";
  }
  const aliases = {
    accounts:"换号 轮换 轮询 并发 额度 粘性 sticky",
    requests:"限流 429 错误 失败 慢 首包 断流 卡住 超时 timeout",
    automation:"自动化 定时 签到 登录过期 刷新令牌 解冻 余额",
    logs:"日志 统计 磁盘 空间 记录 credits 用量",
    compatibility:"适配 cache tool 工具报错"
  };
  const belongs = (field, category, dirty) => category === "common" ? favorites.has(field.key) : category === "changed" ? Object.hasOwn(dirty,field.key) : categoryOf(field) === category;
  const equal = (a,b) => JSON.stringify(a) === JSON.stringify(b);
  const display = value => Array.isArray(value) ? value.join(", ") : typeof value === "boolean" ? (value ? "开启" : "关闭") : String(value ?? "—");
  const active = (v, auth) => v === version && auth === state.authVersion && state.authenticated;

  function notice(message, error = false) {
    $("settingsStatus").textContent = message;
    $("settingsStatus").classList.toggle("s-err", error);
  }
  function read(field, input) {
    if (field.kind === "boolean") return input.checked;
    if (["integer", "number"].includes(field.kind)) return input.value.trim() === "" ? null : Number(input.value);
    if (field.kind === "hours") return input.value.trim() ? input.value.split(/[,，]/).map(v => v.trim() === "" ? null : Number(v.trim())) : [];
    return input.value.trim();
  }
  function set(field, input, value) {
    if (field.kind === "boolean") input.checked = !!value;
    else input.value = Array.isArray(value) ? value.join(", ") : String(value ?? "");
  }
  function changes() {
    const out = {};
    if (!snapshot) return out;
    for (const field of snapshot.fields) {
      if (snapshot.locked[field.key]) continue;
      const value = read(field, controls.get(field.key));
      if (!equal(value,snapshot.values[field.key])) out[field.key] = value;
    }
    return out;
  }
  function update() {
    const dirty = changes(), count = Object.keys(dirty).length;
    const pending = snapshot?.hot_reload && snapshot.pending.some(key => !snapshot.locked[key]);
    $("settingsSave").disabled = !snapshot || busy || conflicted || (!count && !pending);
    $("settingsSave").textContent = snapshot?.hot_reload ? "保存并应用" : "保存配置";
    $("settingsDiscard").disabled = !snapshot || busy || !count;
    $("settingsReload").disabled = busy;
    $("settingsReload").textContent = count ? "重新读取并放弃修改" : "重新读取";
    $("settingsFields").disabled = busy || !snapshot;
    $("settingsDirty").textContent = count ? count + " 项修改尚未保存 · 保存会应用所有分类的修改" : "没有未保存修改";
    for (const [key,row] of rows) row.classList.toggle("is-dirty",Object.hasOwn(dirty,key));
    for (const [id,{button,count:badge}] of navButtons) {
      const fields = snapshot.fields.filter(field => belongs(field,id,dirty));
      const edited = fields.filter(field => Object.hasOwn(dirty,field.key)).length;
      badge.textContent = String(fields.length);
      badge.classList.toggle("has-edits",edited > 0);
      const label = categories.find(category => category.id === id).label;
      button.setAttribute("aria-label", label + "，" + fields.length + " 项" + (edited ? "，" + edited + " 项未保存" : ""));
      button.title = edited ? edited + " 项未保存修改" : label;
    }
    if (selectedCategory === "changed") filter();
  }
  function filter() {
    if (!snapshot) return;
    const query = $("settingsSearch").value.trim().toLowerCase();
    const terms = query.split(/\s+/).filter(Boolean), dirty = changes();
    $("settingsGroups").classList.toggle("is-searching",!!query);
    $("settingsGroups").classList.toggle("is-common",!query && selectedCategory === "common");
    let count = 0;
    for (const field of snapshot.fields) {
      const row = rows.get(field.key);
      row.hidden = query ? !terms.every(term => row.dataset.search.includes(term)) : !belongs(field,selectedCategory,dirty);
      if (!row.hidden) count++;
    }
    $("settingsGroups").querySelectorAll("section").forEach(group => {
      group.hidden = !Array.from(group.querySelectorAll("[data-setting-row]")).some(row => !row.hidden);
    });
    const category = categories.find(item => item.id === selectedCategory);
    $("settingsCategoryTitle").textContent = query ? "搜索结果" : category.label;
    $("settingsCategoryDescription").textContent = query ? "搜索全部配置 · 结果下方标明所属分类" : category.help;
    $("settingsResultCount").textContent = count + " 项";
    $("settingsClearSearch").hidden = !query;
    $("settingsEmpty").hidden = count > 0;
    $("settingsEmpty").textContent = query ? "没有匹配的配置。试试“签到”“限流”或配置项名称，也可清除搜索。" : selectedCategory === "common" ? "还没有常用配置。进入左侧分类，点击配置旁的星标即可收藏。" : selectedCategory === "changed" ? "没有待保存的修改。你可以放心切换分类，草稿会保留在当前页面。" : "此分类暂无配置。";
    for (const [id,{button}] of navButtons) button.setAttribute("aria-pressed",String(!query && selectedCategory === id));
  }
  function selectCategory(id) {
    selectedCategory = id;
    $("settingsSearch").value = "";
    filter();
  }
  function renderNav(data) {
    navButtons.clear(); $("settingsNav").replaceChildren();
    for (const category of categories) {
      if (category.id === "other" && !data.fields.some(field => categoryOf(field) === "other")) continue;
      const button = document.createElement("button"); button.type = "button";
      button.dataset.settingsCategory = category.id;
      const label = document.createElement("span"); label.textContent = category.label;
      const count = document.createElement("span"); count.className = "settings-nav-count"; count.setAttribute("aria-hidden","true");
      button.append(label,count);
      button.addEventListener("click",() => selectCategory(category.id));
      $("settingsNav").append(button); navButtons.set(category.id,{button,count});
    }
    if (!navButtons.has(selectedCategory)) selectedCategory = "common";
  }
  function render(data) {
    snapshot = data; conflicted = false; controls.clear(); rows.clear();
    renderNav(data);
    $("settingsGroups").replaceChildren();
    $("settingsPending").hidden = !data.pending.length;
    $("settingsPending").textContent = data.pending.length ? data.pending.length + (data.hot_reload ? " 项文件配置与运行值不同，点击保存并应用使其生效。" : " 项已保存配置尚未生效。请在部署环境重启网关；当前请求继续使用下方标注的当前值。") : "";
    $("settingsTimezone").textContent = "任务时区：" + (data.timezone || "服务器本地时区");
    const groups = new Map();
    for (const field of data.fields) {
      if (!groups.has(field.group)) {
        const group = document.createElement("section"); group.className = "settings-group";
        const title = document.createElement("h4"); title.textContent = field.group;
        const grid = document.createElement("div"); grid.className = "settings-options";
        group.append(title,grid); $("settingsGroups").append(group); groups.set(field.group,grid);
      }
      const category = categories.find(item => item.id === categoryOf(field));
      const row = document.createElement("div"); row.className = "setting-row"; row.dataset.settingRow = field.key;
      row.dataset.search = [field.key,field.group,field.label,field.help,category.label,aliases[category.id] || "",...Object.values((data.labels || {})[field.key] || {})].join(" ").toLowerCase();
      rows.set(field.key,row);
      const id = "setting-" + field.key.replaceAll(".","-");
      const label = document.createElement("label"); label.htmlFor = id; label.className = "field-label"; label.textContent = field.label;
      const input = document.createElement(field.kind === "select" ? "select" : "input"); input.id = id;
      if (field.kind === "select") {
        const options = [...field.options];
        if (!options.includes(data.values[field.key])) options.unshift(data.values[field.key]);
        // 选项文案：优先用服务端给的 labels（如 weighted → 随机调用），
        // 缺表时回落显示原始取值——宁可露出机器词，也不能显示空白选项。
        const labels = (data.labels || {})[field.key] || {};
        for (const value of options) {
          const option = document.createElement("option");
          option.value = value; option.textContent = labels[value] || value;
          input.append(option);
        }
      } else if (field.kind === "boolean") input.type = "checkbox";
      else if (["integer","number"].includes(field.kind)) { input.type = "number"; input.min = field.min; input.max = field.max; input.step = field.kind === "integer" ? "1" : "any"; input.required = true; }
      else { input.type = "text"; input.required = true; input.maxLength = 180; }
      input.disabled = !!data.locked[field.key]; input.setAttribute("aria-describedby",id+"-help");
      set(field,input,data.values[field.key]); controls.set(field.key,input);
      const help = document.createElement("p"); help.id = id+"-help"; help.className = "hint"; help.textContent = field.help;
      const context = document.createElement("p"); context.className = "setting-context"; context.textContent = category.label + " / " + field.group;
      const info = document.createElement("div"); info.className = "setting-info"; info.append(context,label,help);
      const control = document.createElement("div"); control.className = "setting-control"; control.append(input);
      if (field.kind === "boolean") {
        input.setAttribute("role","switch");
        const toggleText = document.createElement("span"); toggleText.className = "hint"; toggleText.setAttribute("aria-hidden","true");
        toggleText.textContent = input.checked ? "已开启" : "已关闭";
        input.addEventListener("change",() => toggleText.textContent = input.checked ? "已开启" : "已关闭");
        control.append(toggleText);
      }
      const favorite = document.createElement("button"); favorite.type = "button"; favorite.className = "setting-favorite";
      const syncFavorite = () => { favorite.textContent = favorites.has(field.key) ? "★" : "☆"; favorite.setAttribute("aria-pressed",String(favorites.has(field.key))); favorite.setAttribute("aria-label","常用配置："+field.label); favorite.title = favorites.has(field.key) ? "取消常用："+field.label : "设为常用："+field.label; };
      syncFavorite();
      favorite.addEventListener("click",() => {
        if (favorites.has(field.key)) favorites.delete(field.key); else favorites.add(field.key);
        try { localStorage.setItem(favoriteKey,JSON.stringify([...favorites].filter(key => controls.has(key)))); } catch { /* Preference remains usable without storage. */ }
        syncFavorite(); filter(); update();
        if (row.hidden) navButtons.get("common").button.focus();
      });
      const more = document.createElement("details"); more.className = "setting-details";
      const summary = document.createElement("summary"); summary.textContent = "详情与默认值";
      const code = document.createElement("code"); code.textContent = field.key;
      const detail = document.createElement("p"); detail.className = "hint setting-value";
      const shown = value => (data.labels || {})[field.key]?.[value] || display(value);
      detail.textContent = "默认：" + shown(data.defaults[field.key]);
      if (data.pending.includes(field.key)) { const pending = document.createElement("p"); pending.className = "hint s-warn"; pending.textContent = "当前生效：" + shown(data.current[field.key]) + (data.hot_reload ? " · 等待应用" : " · 等待重启"); info.append(pending); }
      const reset = document.createElement("button"); reset.type = "button"; reset.className = "setting-reset"; reset.textContent = "填入默认值"; reset.disabled = input.disabled;
      reset.addEventListener("click",() => { set(field,input,snapshot.defaults[field.key]); input.setCustomValidity(""); if (field.kind === "boolean") control.children[1].textContent = input.checked ? "已开启" : "已关闭"; update(); });
      more.append(summary,code,detail,reset); info.append(more);
      row.append(info,control,favorite);
      if (data.locked[field.key]) { const lock = document.createElement("p"); lock.className = "hint s-warn"; lock.textContent = "由环境变量 " + data.locked[field.key] + " 控制"; info.append(lock); }
      groups.get(field.group).append(row);
    }
    filter(); update();
  }
  async function load(force = false) {
    if (!state.authenticated || ui.view !== "settings" || busy || (snapshot && !force)) return;
    const v = ++version, auth = state.authVersion; busy = true; update(); notice("正在读取配置…");
    try {
      const data = await api("/admin/api/settings");
      if (!active(v,auth)) return;
      render(data); notice(data.hot_reload ? "配置已读取。保存后立即应用于后续请求与任务，无需重启。" : "配置已读取。保存仅写入文件，重启网关后生效。");
    } catch (error) {
      if (!active(v,auth)) return;
      // Do not permit saving a stale form after a failed explicit reload.
      conflicted = true; notice("读取失败：" + error.message + "。请重新读取后再编辑。",true);
    } finally { if (active(v,auth)) { busy = false; update(); } }
  }
  $("settingsForm").addEventListener("submit", async event => {
    event.preventDefault();
    if (!snapshot || busy || conflicted || !state.authenticated) return;
    const patch = changes();
    if (snapshot.hot_reload) for (const key of snapshot.pending) {
      if (!snapshot.locked[key] && !Object.hasOwn(patch,key)) patch[key] = snapshot.values[key];
    }
    if (!Object.keys(patch).length) return;
    for (const field of snapshot.fields) {
      if (!Object.hasOwn(patch,field.key)) continue;
      const input = controls.get(field.key), value = patch[field.key];
      input.setCustomValidity("");
      if (field.kind === "hours" && (!value.length || value.some(v => !Number.isInteger(v) || v < 0 || v > 23) || new Set(value).size !== value.length)) input.setCustomValidity("请输入不重复的 0–23 小时值，例如 9,21");
      if (!input.checkValidity()) { selectCategory(categoryOf(field)); input.focus(); input.reportValidity(); notice(field.label + "：请检查输入",true); return; }
    }
    const v = ++version, auth = state.authVersion; busy = true; update(); notice(snapshot.hot_reload ? "正在保存并应用…" : "正在保存…");
    try {
      const data = await api("/admin/api/settings",{method:"PATCH",body:JSON.stringify({revision:snapshot.revision,changes:patch})});
      if (!active(v,auth)) return;
      render(data); notice(data.hot_reload ? "已保存并生效 · 版本 " + data.applied_version + "。正在处理的请求继续完成。" : data.restart_required ? "保存成功，请重启网关使配置生效。" : "保存成功，配置与当前运行值一致。");
    } catch (error) {
      if (!active(v,auth)) return;
      if (error.status === 409) conflicted = true;
      notice("保存失败：" + error.message + (conflicted ? "；请重新读取后再次修改。" : "；输入已保留，可修正后重试。"),true);
    } finally { if (active(v,auth)) { busy = false; update(); } }
  });
  $("settingsGroups").addEventListener("input",event => { event.target.setCustomValidity?.(""); update(); });
  $("settingsGroups").addEventListener("change",update);
  $("settingsSearch").addEventListener("input",filter);
  $("settingsClearSearch").addEventListener("click",() => { $("settingsSearch").value = ""; filter(); $("settingsSearch").focus(); });
  $("settingsNav").addEventListener("keydown",event => {
    const buttons = [...navButtons.values()].map(item => item.button), index = buttons.indexOf(event.target);
    if (index < 0 || !["ArrowDown","ArrowUp","ArrowLeft","ArrowRight","Home","End"].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length-1 : (index + (["ArrowDown","ArrowRight"].includes(event.key) ? 1 : -1) + buttons.length) % buttons.length;
    buttons[next].focus();
  });
  $("settingsReload").addEventListener("click",() => load(true));
  $("settingsDiscard").addEventListener("click",() => { if (!busy && snapshot) { const wasConflicted = conflicted; render(snapshot); conflicted = wasConflicted; update(); notice(wasConflicted ? "已撤销未保存修改，请重新读取配置。" : "已撤销未保存修改。"); } });
  window.addEventListener("hashchange",() => load());
  window.addEventListener("beforeunload",event => { if (Object.keys(changes()).length) { event.preventDefault(); event.returnValue = ""; } });
  window.addEventListener("console-key-changed",() => {
    version++; snapshot = null; busy = false; conflicted = false; controls.clear(); rows.clear(); navButtons.clear(); selectedCategory = "common";
    $("settingsGroups").replaceChildren(); $("settingsPending").hidden = true; $("settingsTimezone").textContent = "";
    $("settingsNav").replaceChildren(); $("settingsResultCount").textContent = ""; $("settingsEmpty").hidden = true; $("settingsClearSearch").hidden = true;
    $("settingsCategoryTitle").textContent = "常用配置"; $("settingsCategoryDescription").textContent = "登录后加载配置分类。";
    $("settingsSearch").value = ""; notice(""); update(); load();
  });
  load();
})();
