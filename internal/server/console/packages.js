// On-demand billing details. Account identity is captured from the clicked row,
// not a changing table index; cancellation/versioning prevents stale disclosure.
(() => {
  const dialog = $("packageDialog"), content = $("packageContent");
  let target = null, trigger = null, snapshot = null, controller = null, version = 0;
  const decimal = value => typeof value === "string" && /^\d+(\.\d+)?$/.test(value);
  const amountsValid = value => value && [value.total, value.used, value.remaining].every(decimal);
  const positive = value => decimal(value) && /[1-9]/.test(value);
  const unitName = unit => unit === "unknown" ? "单位未提供" : unit || "单位未提供";
  const dateText = value => value ? fmtTime(value) : "未提供";

  function reset() {
    version++;
    controller?.abort(); controller = null;
    target = null; snapshot = null;
    content.innerHTML = ""; content.setAttribute("aria-busy", "false");
    $("packageChecked").textContent = "";
    $("packageIdentity").textContent = "";
    $("packageRefresh").disabled = false;
  }

  function render() {
    const data = snapshot;
    if (!data) return;
    const rows = $("packageRemaining").checked ? data.packages.filter(p => positive(p.remaining)) : data.packages;
    const summaries = data.totals.map(t => `<section class="package-totals" aria-label="${esc(unitName(t.unit))} 汇总">${[
      ["额度总量", t.total], ["已使用", t.used], ["剩余额度", t.remaining]
    ].map(([label, value]) => `<div><span>${label}</span><strong>${esc(value)}</strong><small>${esc(unitName(t.unit))}</small></div>`).join("")}</section>`).join("");
    const table = rows.length ? `<div class="table-wrap" tabindex="0" role="region" aria-label="套餐明细，可横向滚动"><table class="package-table"><thead><tr><th scope="col">套餐 / 统计范围</th><th scope="col">总量</th><th scope="col">已用</th><th scope="col">剩余</th><th scope="col">周期与有效期</th></tr></thead><tbody>${rows.map(p => `<tr>
      <td><strong>${esc(p.name || "未命名套餐")}</strong><div class="hint">${esc(p.product || "")}</div><span class="tag">${p.basis === "cycle" ? "本周期" : "套餐总量"}</span> <span class="hint">${esc(unitName(p.unit))}${p.precise ? "" : " · 部分额度未提供精确值"}</span></td>
      <td class="mono">${esc(p.total)}</td><td class="mono">${esc(p.used)}</td><td class="mono ${positive(p.remaining) ? "s-ok" : "s-dim"}"><b>${esc(p.remaining)}</b>${positive(p.remaining) ? "" : '<div class="hint">已耗尽</div>'}</td>
      <td><dl class="package-dates"><dt>本周期开始</dt><dd>${esc(p.cycle_start || "未提供")}</dd><dt>本周期结束</dt><dd>${esc(p.cycle_end || "未提供")}</dd><dt>资源生效</dt><dd>${esc(dateText(p.usable_from))}</dd><dt>资源有效至</dt><dd>${esc(dateText(p.usable_until))}</dd>${p.expired_time ? `<dt>上游过期时间</dt><dd>${esc(p.expired_time)}</dd>` : ""}</dl></td>
    </tr>`).join("")}</tbody></table></div>` : `<div class="empty"><strong>${data.count ? "没有剩余额度的套餐" : "当前查询范围内没有套餐"}</strong>${data.count ? "取消上方筛选可查看已耗尽套餐。" : "上游未返回未过期套餐，可稍后刷新。"}</div>`;
    content.innerHTML = summaries + `<p class="hint package-count">显示 ${rows.length} / ${data.count} 个套餐 · 汇总始终包含全部已查询套餐</p>` + table;
    $("packageChecked").textContent = "查询时间：" + fmtTime(data.checked_at);
  }

  async function load() {
    if (!target) return;
    controller?.abort(); controller = new AbortController();
    const active = controller, requestVersion = ++version, account = { ...target };
    snapshot = null;
    $("packageChecked").textContent = "";
    $("packageRefresh").disabled = true;
    content.setAttribute("aria-busy", "true");
    content.innerHTML = '<div class="empty">正在查询账号套餐…</div>';
    try {
      const query = new URLSearchParams({ uid: account.uid, realm: account.realm });
      const data = await api("/admin/api/account-packages?" + query, { signal: active.signal });
      if (requestVersion !== version || !dialog.open) return;
      if (data.uid !== account.uid || data.realm !== account.realm || !Array.isArray(data.packages) ||
          data.count !== data.packages.length || !Array.isArray(data.totals) ||
          !data.packages.every(amountsValid) || !data.totals.every(amountsValid) ||
          (data.count > 0 && data.totals.length === 0)) throw new Error("服务器返回的套餐数据不完整，请重试");
      snapshot = data;
      render();
    } catch (error) {
      if (requestVersion !== version || active.signal.aborted || !dialog.open) return;
      content.innerHTML = `<div class="empty" role="alert"><strong>套餐查询失败</strong><p>${esc(error.message)}</p><button id="packageRetry">重试</button></div>`;
    } finally {
      if (requestVersion === version) {
        controller = null;
        $("packageRefresh").disabled = false;
        content.setAttribute("aria-busy", "false");
      }
    }
  }

  $("accounts").addEventListener("click", event => {
    const button = event.target.closest("button[data-package-uid]");
    if (!button) return;
    const account = state.accounts.find(a => a.uid === button.dataset.packageUid && a.realm === button.dataset.packageRealm);
    if (!account) return;
    reset(); trigger = button;
    target = { uid: account.uid, realm: account.realm };
    $("packageIdentity").textContent = `${account.nickname || "未命名账号"} · ${account.realm} · ${account.uid}`;
    $("packageRemaining").checked = false;
    dialog.showModal(); load();
  });
  $("packageClose").addEventListener("click", () => { reset(); dialog.close(); });
  dialog.addEventListener("cancel", reset);
  dialog.addEventListener("close", () => {
    reset();
    if (trigger?.isConnected) trigger.focus(); else $("accountSearch").focus();
    trigger = null;
  });
  $("packageRefresh").addEventListener("click", load);
  $("packageRemaining").addEventListener("change", render);
  content.addEventListener("click", event => { if (event.target.closest("#packageRetry")) load(); });
  for (const name of ["console-key-changed", "pagehide", "hashchange"]) window.addEventListener(name, () => { reset(); if (dialog.open) dialog.close(); });
})();
