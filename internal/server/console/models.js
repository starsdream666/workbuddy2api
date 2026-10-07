// 模型列表页：展示全部启用产品线的可用模型与上游倍率（credits）。
//
// 数据源：GET /admin/api/models —— 与网关 /v1/models 同源（fetchDynamicModelsFor），
// 倍率即上游下发的 credits 字段，网关不换算价格。
//
// 与 tasks.js 同样的组织约定：IIFE、复用 console.js 暴露的 $/esc/api/toast/state/ui，
// 只在当前视图为 models 且页面可见时加载，避免无谓请求。
(() => {
  // refreshing 手动刷新在途标志：与自动加载的 loading 分开计数，
  // 因为两者守卫语义不同（自动加载可被静默跳过，手动点击必须有明确回执）。
  let loading = false, refreshing = false;
  const view = { search: "", realm: "", kind: "", sort: "realm", dir: "asc" };
  const visible = () => state.authenticated && ui.view === "models" && !document.hidden;

  // 类型归一：媒体标签（image/video）单列，其余都是对话模型。
  const kindOf = m => (m.kind === "image" || m.kind === "video") ? m.kind : "chat";
  const kindLabels = { chat: "对话", image: "图片", video: "视频" };

  // 倍率展示：以解析后的数值为准（上游格式为 "x0.34 credits"）——
  //   0 → 免费；>0 → ×数值；无法解析时退回原始文本；都没有则显示 —。
  function rateText(m) {
    if (typeof m.rate === "number") return m.rate === 0 ? "免费" : "×" + m.rate;
    if (m.credits) return m.credits;
    return "";
  }
  function rateClass(m) {
    if (m.rate === 0) return "s-ok";
    if (typeof m.rate === "number" && m.rate > 0) return "s-warn";
    return "";
  }

  function filtered() {
    const rows = (state.models && state.models.data) || [];
    const q = view.search.trim().toLowerCase();
    return rows.filter(m => {
      if (view.realm && m.realm !== view.realm) return false;
      if (view.kind && kindOf(m) !== view.kind) return false;
      if (q) {
        const hay = ((m.id || "") + " " + (m.name || "") + " " + (m.vendor || "")).toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
  }

  function sorted(rows) {
    const key = view.sort, dir = view.dir === "asc" ? 1 : -1;
    const val = m => {
      switch (key) {
        case "rate": return typeof m.rate === "number" ? m.rate : (m.credits ? Infinity : -1);
        case "context_length": return Number(m.context_length) || 0;
        case "max_output_tokens": return Number(m.max_output_tokens) || 0;
        case "kind": return kindOf(m);
        case "realm": return (m.realm || "") + "/" + (m.id || "");
        default: return (m.id || "");
      }
    };
    return rows.slice().sort((a, b) => {
      const x = val(a), y = val(b);
      if (x < y) return -1 * dir;
      if (x > y) return 1 * dir;
      return 0;
    });
  }

  function sortMark(key) {
    if (view.sort !== key) return "";
    return view.dir === "asc" ? " ▲" : " ▼";
  }
  const sortBtn = (key, label) =>
    '<button type="button" class="usage-sort" data-model-sort="' + key + '">' + label + sortMark(key) + "</button>";

  function renderStats(all) {
    const free = all.filter(m => m.rate === 0).length;
    const realms = [...new Set(all.map(m => m.realm).filter(Boolean))];
    $("modelsTotal").textContent = all.length;
    $("modelsFree").textContent = free;
    $("modelsPaid").textContent = all.length - free;
    $("modelsRealms").textContent = realms.length;
    $("modelsRealmsNote").textContent = realms.length ? realms.join(" · ") : "无启用产品线";
  }

  function renderRealmFilter(all) {
    const realms = [...new Set(all.map(m => m.realm).filter(Boolean))].sort();
    const select = $("modelsRealmFilter");
    const current = select.value;
    select.innerHTML = '<option value="">全部产品线</option>' +
      realms.map(rn => '<option value="' + esc(rn) + '">' + esc(rn) + "</option>").join("");
    if (realms.includes(current)) select.value = current;
  }

  function renderTable() {
    const rows = sorted(filtered());
    $("modelsCount").textContent = rows.length;
    $("modelsFilterCount").textContent = rows.length + " 个模型";
    $("modelsTable").setAttribute("aria-busy", "false");
    if (!rows.length) {
      $("modelsTable").innerHTML = '<div class="empty"><strong>没有匹配的模型</strong>调整搜索或筛选条件后重试。</div>';
      return;
    }
    const body = rows.map(m => {
      const rate = rateText(m);
      const rateCell = rate
        ? '<span class="status ' + rateClass(m) + '">' + esc(rate) + "</span>"
        : '<span class="hint">—</span>';
      const ctx = m.context_length ? compact(m.context_length) : "—";
      const maxOut = m.max_output_tokens ? compact(m.max_output_tokens) : "—";
      const kind = kindOf(m);
      // 档位：上游下发 supportedEfforts 时按其列出；未下发但模型带推理时后端已回退官方全档，
      // 这里对"全档"做紧凑展示，避免 6 个档位撑爆单元格。
      const eff = m.supported_efforts || [];
      let effort;
      if (!eff.length) effort = m.default_effort || "—";
      else if (eff.length >= 6) effort = "全档（" + eff.length + "）";
      else effort = eff.join(" / ");
      if (eff.length && m.default_effort) effort += "（默认 " + m.default_effort + "）";
      // 供应商：优先可读名（由模型 id 推导）；原始字母无图例，仅作悬浮提示。
      const vendorCell = m.vendor
        ? '<span' + (m.raw_vendor ? ' title="上游 vendor 字段：' + esc(m.raw_vendor) + '"' : "") + ">" + esc(m.vendor) + "</span>"
        : '<span class="hint">—</span>';
      return "<tr>" +
        '<td><div class="request-model"><span class="request-symbol">◈</span><span><span class="account-name">' + esc(m.id) + "</span>" +
        (m.name && m.name !== m.id ? '<span class="hint">' + esc(m.name) + "</span>" : "") + "</span></div></td>" +
        '<td><span class="badge">' + esc(m.realm) + "</span></td>" +
        "<td>" + rateCell + "</td>" +
        '<td class="hint">' + esc(kindLabels[kind]) + "</td>" +
        '<td class="hint">' + esc(ctx) + "</td>" +
        '<td class="hint">' + esc(maxOut) + "</td>" +
        '<td class="hint">' + esc(effort) + "</td>" +
        '<td class="hint">' + vendorCell + "</td>" +
        "</tr>";
    }).join("");
    $("modelsTable").innerHTML =
      '<div class="table-wrap" tabindex="0" role="region" aria-label="模型列表，可横向滚动"><table><thead><tr>' +
      "<th>" + sortBtn("id", "模型") + "</th>" +
      "<th>" + sortBtn("realm", "产品线") + "</th>" +
      "<th>" + sortBtn("rate", "倍率") + "</th>" +
      "<th>" + sortBtn("kind", "类型") + "</th>" +
      "<th>" + sortBtn("context_length", "上下文") + "</th>" +
      "<th>" + sortBtn("max_output_tokens", "最大输出") + "</th>" +
      "<th>推理档位</th><th>供应商</th></tr></thead><tbody>" + body + "</tbody></table></div>";
  }

  function renderAll() {
    const all = (state.models && state.models.data) || [];
    renderStats(all);
    renderRealmFilter(all);
    renderTable();
  }

  // fetchModels 拉一次模型列表并落库渲染；返回本次结果是否被采纳
  // （false = 期间会话已切换，返回值已作废，调用方不要再改 UI 文案）。
  //
  // 只做"取数 → 落库 → 渲染 → 盖时间戳"这一段共享流程：自动加载与手动刷新
  // 在**守卫口径**和**反馈方式**上不同（见两处调用），但取数与渲染必须只有一份
  // 实现，否则两处的字段名、渲染时序、时间戳格式迟早漂移。
  async function fetchModels() {
    const key = state.authVersion;
    const d = await api("/admin/api/models");
    if (key !== state.authVersion) return false; // 会话已切换：丢弃这次结果
    state.models = d;
    $("modelsError").hidden = true;
    renderAll();
    $("modelsUpdated").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", { hour12: false });
    return true;
  }

  // loadModels 自动加载：只在本视图可见时拉，且同时只允许一个在飞。
  // 失败时给出页内错误；首次加载失败还要把空表换成可读提示。
  async function loadModels() {
    if (loading || !visible()) return;
    loading = true;
    $("modelsTable").setAttribute("aria-busy", "true");
    const key = state.authVersion;
    try {
      await fetchModels();
    } catch (e) {
      if (key !== state.authVersion) return;
      $("modelsError").textContent = "加载失败：" + e.message;
      $("modelsError").hidden = false;
      if (!state.models) $("modelsTable").innerHTML = '<div class="empty"><strong>暂时无法加载模型列表</strong>请检查网关连接与登录状态，再点击「刷新」。</div>';
    } finally {
      loading = false;
      $("modelsTable").setAttribute("aria-busy", "false");
    }
  }

  $("modelsSearch").addEventListener("input", () => { view.search = $("modelsSearch").value; renderTable(); });
  $("modelsRealmFilter").addEventListener("change", () => { view.realm = $("modelsRealmFilter").value; renderTable(); });
  $("modelsKindFilter").addEventListener("change", () => { view.kind = $("modelsKindFilter").value; renderTable(); });
  // 表头排序：事件委托，重绘会重建 thead，逐个绑定会在重绘后失效。
  $("modelsTable").addEventListener("click", e => {
    const btn = e.target.closest("button[data-model-sort]");
    if (!btn) return;
    const key = btn.dataset.modelSort;
    if (view.sort === key) view.dir = view.dir === "asc" ? "desc" : "asc";
    else { view.sort = key; view.dir = key === "rate" || key.includes("token") || key === "context_length" ? "desc" : "asc"; }
    renderTable();
  });
  // 手动刷新：显式点击要求"必定刷一次并给出反馈"，与自动加载的守卫口径不同。
  //
  // loadModels 的两道守卫对手动点击都是错的：
  //   - !visible()：按钮就在 models 页里，可见性必然成立，留着它只会在
  //     "切到后台再点"这类时序下静默什么都不做；
  //   - loading：上一次请求还在飞时点击会被**静默吞掉**，用户看到按钮毫无反应
  //     ——这正是这个按钮此前被当成"坏的"的原因之一（另一个原因是它挂了个
  //     没有任何脚本处理的 data-console-action）。
  // 所以这里不复用 loadModels 的守卫，只复用它的取数实现（fetchModels），
  // 再补上按钮自旋 + toast 回执。重复点击最多多发一次 GET，幂等。
  $("modelsRefreshBtn").addEventListener("click", async () => {
    if (refreshing) return;
    refreshing = true;
    busy("modelsRefreshBtn", true, "刷新中…");
    const key = state.authVersion;
    try {
      if (await fetchModels()) toast("模型列表已刷新");
    } catch (e) {
      if (key !== state.authVersion) return;
      $("modelsError").textContent = "刷新失败：" + e.message;
      $("modelsError").hidden = false;
      toast("刷新失败：" + e.message, 4200, "error");
    } finally {
      refreshing = false;
      // 会话已切换时按钮文案可能已被其它流程重置，busy(…, false) 会按
      // dataset.label 还原，两种情况都安全。
      busy("modelsRefreshBtn", false);
    }
  });
  window.addEventListener("hashchange", loadModels);
  document.addEventListener("visibilitychange", loadModels);
  window.addEventListener("console-key-changed", () => { state.models = null; loadModels(); });
  loadModels();
})();
