(() => {
  let timer = null, loading = false, submitting = false, enabled = false;
  const labels = { queued: "排队中", running: "执行中", cancelling: "正在取消", cancelled: "已取消", succeeded: "已完成", failed: "失败" };
  const operations = { balance: "余额巡检", refresh_tokens: "刷新 Token" };
  const visible = () => state.authenticated && ui.view === "tasks" && !document.hidden;
  const setNotice = message => { $("taskNotice").textContent = message; };
  const syncSubmit = () => { $("taskSubmit").disabled = submitting || !enabled || !$("taskRealm").value; };

  function renderTasks(data) {
    enabled = data.enabled === true;
    const selected = $("taskRealm").value;
    const realms = Array.isArray(data.realms) ? data.realms : [];
    $("taskRealm").innerHTML = realms.length ? realms.map(name => '<option value="'+esc(name)+'">'+esc(name)+'</option>').join("") : '<option value="">无可操作产品线</option>';
    if (realms.includes(selected)) $("taskRealm").value = selected;
    $("taskRealm").disabled = !enabled || !realms.length;
    syncSubmit();
    const tasks = Array.isArray(data.tasks) ? data.tasks : [];
    $("taskRows").innerHTML = tasks.length ? tasks.map(task => {
      const progress = task.progress || {};
      const counts = [progress.succeeded, progress.failed, progress.skipped, progress.total].map(value => Number(value) || 0);
      const detail = '成功 '+counts[0]+' / 失败 '+counts[1]+' / 跳过 '+counts[2]+' / 共 '+counts[3];
      const cancel = ["queued", "running"].includes(task.status) ? '<button data-cancel-task="'+esc(task.id)+'">取消</button>' : '—';
      return '<tr><td title="'+esc(task.id)+'">'+esc(operations[task.kind] || task.kind)+' #'+esc(String(task.id || "").slice(0,8))+'</td><td>'+esc(task.realm)+'</td><td>'+esc(labels[task.status] || task.status)+(task.error ? '<div class="hint">'+esc(task.error)+'</div>' : '')+'</td><td>'+esc(detail)+'</td><td>'+esc(new Date(task.created_at).toLocaleString())+'</td><td>'+cancel+'</td></tr>';
    }).join("") : '<tr><td colspan="6">暂无任务，提交操作后可在这里查看进度。</td></tr>';
    setNotice(enabled ? "串行执行 · 最多 64 个待执行/在执行任务 · 最近 200 条记录 · 每 3 秒刷新" : "当前实例未启用任务队列");
  }

  async function loadTasks() {
    clearTimeout(timer);
    if (loading || !visible()) return;
    loading = true;
    const key = state.authVersion;
    try {
      const data = await api("/admin/api/tasks");
      if (key === state.authVersion) renderTasks(data);
    } catch (error) {
      if (key === state.authVersion) { enabled = false; syncSubmit(); setNotice("任务读取失败："+error.message); }
    } finally {
      loading = false;
      if (visible()) timer = setTimeout(loadTasks, 3000);
    }
  }

  $("taskSubmit").addEventListener("click", async () => {
    if (submitting || !enabled) return;
    submitting = true;
    syncSubmit();
    const key = state.authVersion;
    try {
      const result = await api("/admin/api/tasks", { method: "POST", body: JSON.stringify({realm: $("taskRealm").value, kind: $("taskKind").value}) });
      if (key === state.authVersion) { toast(result.deduplicated ? "相同任务已在队列中" : "任务已提交"); await loadTasks(); }
    } catch (error) { if (key === state.authVersion) setNotice("提交失败："+error.message); }
    finally { submitting = false; syncSubmit(); }
  });

  $("taskRows").addEventListener("click", async event => {
    const button = event.target.closest("button[data-cancel-task]");
    if (!button || button.disabled) return;
    button.disabled = true;
    const key = state.authVersion;
    try {
      await api("/admin/api/tasks/"+encodeURIComponent(button.dataset.cancelTask), {method:"DELETE"});
      if (key === state.authVersion) { toast("已请求取消，已完成的操作不会撤销"); await loadTasks(); }
    } catch (error) { if (key === state.authVersion) { button.disabled = false; setNotice("取消失败："+error.message); } }
  });

  $("tasksRefresh").addEventListener("click", loadTasks);
  $("taskRealm").addEventListener("change", syncSubmit);
  window.addEventListener("hashchange", loadTasks);
  window.addEventListener("pagehide", () => clearTimeout(timer));
  document.addEventListener("visibilitychange", loadTasks);
  window.addEventListener("console-key-changed", () => {
    enabled = false;
    $("taskRows").innerHTML = '<tr><td colspan="6">登录会话已变化，等待重新加载</td></tr>';
    $("taskRealm").innerHTML = '<option value="">等待加载</option>';
    syncSubmit();
    loadTasks();
  });
  loadTasks();
})();
