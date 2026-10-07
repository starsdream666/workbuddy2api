(() => {
  let setup = false, signingIn = false, keys = [], editing = null, revoking = null, saving = false, loadingKeys = false;

  function lock(message = "") {
    state.authenticated = false; state.csrf = ""; state.authVersion++;
    state.overviewVersion++; state.usageVersion++;
    for (const request of state.adminRequests) request.abort();
    stopEvents(); stopPoll(); clearTimeout(state.creditsTimer); clearTimeout(state.accountsTimer);
    state.accounts = []; state.overview = null; state.overviewRaw = null; state.overviewAt = 0;
    state.usage = null; state.models = null; keys = [];
    $("accounts").innerHTML = ""; $("keysList").innerHTML = ""; $("issuedKey").value = "";
    document.querySelectorAll("dialog[open]").forEach(dialog => dialog.close());
    document.body.classList.add("auth-locked"); $("authGate").hidden = false;
    $("authError").textContent = message; $("adminPassword").value = "";
    $("currentAdminPassword").value = ""; $("newAdminPassword").value = ""; $("setupProof").value = "";
    $("adminIdentity").textContent = "";
    window.dispatchEvent(new Event("console-key-changed"));
  }

  function acceptSession(data) {
    state.authenticated = true; state.csrf = data.csrf_token; state.authVersion++;
    $("adminIdentity").textContent = data.username;
    $("authGate").hidden = true; document.body.classList.remove("auth-locked");
    $("adminPassword").value = ""; $("setupProof").value = "";
    refreshAll(false); loadUsage(); setAccountsAuto();
    window.dispatchEvent(new Event("console-key-changed")); loadKeys();
  }

  async function connect() {
    $("authRetry").hidden = true; $("adminLoginButton").disabled = true;
    try {
      const data = await api("/admin/api/auth/session");
      setup = !data.initialized;
      $("setupProofField").hidden = !setup;
      $("adminPassword").autocomplete = setup ? "new-password" : "current-password";
      $("authTitle").textContent = setup ? "创建管理账号" : "管理账号登录";
      $("authDescription").textContent = setup ? "首次使用，请设置易记的账号和密码（8–72 字节）。" : "使用账号密码登录控制台。";
      $("adminLoginButton").textContent = setup ? "创建管理员并登录" : "登录";
      $("adminLoginButton").disabled = false;
      if (data.authenticated) acceptSession(data); else lock();
    } catch (error) { lock("连接失败：" + error.message); $("authRetry").hidden = false; }
  }

  async function signIn() {
    if (signingIn) return;
    signingIn = true; $("adminLoginButton").disabled = true; $("authError").textContent = "";
    const username = $("adminUsername").value.trim(), password = $("adminPassword").value;
    try {
      if (setup) {
        await api("/admin/api/auth/setup", {method:"POST",body:JSON.stringify({username,password,setup_token:$("setupProof").value.trim()})});
        setup = false; $("setupProofField").hidden = true; $("adminLoginButton").textContent = "登录";
      }
      const data = await api("/admin/api/auth/login", {method:"POST",body:JSON.stringify({username,password})});
      acceptSession(data);
    } catch (error) { $("authError").textContent = error.message; }
    finally { signingIn = false; $("adminLoginButton").disabled = false; }
  }
  $("adminLoginButton").addEventListener("click", signIn);
  for (const id of ["adminUsername", "adminPassword", "setupProof"]) $(id).addEventListener("keydown", event => { if (event.key === "Enter" && !$("adminLoginButton").disabled) signIn(); });
  $("authRetry").addEventListener("click", connect);
  window.addEventListener("console-session-expired", () => lock("登录已失效，请重新登录"));
  $("adminLogout").addEventListener("click", async () => {
    try { await api("/admin/api/auth/logout", {method:"POST",body:"{}"}); lock("已退出登录"); }
    catch (error) { toast("退出失败：" + error.message, 4000, "error"); }
  });
  $("adminChangePassword").addEventListener("click", async () => {
    $("adminChangePassword").disabled = true;
    try {
      await api("/admin/api/auth/password", {method:"POST",body:JSON.stringify({current_password:$("currentAdminPassword").value,new_password:$("newAdminPassword").value})});
      $("currentAdminPassword").value = ""; $("newAdminPassword").value = "";
      lock("密码已修改，请重新登录");
    } catch (error) { $("passwordNotice").textContent = error.message; }
    finally { $("adminChangePassword").disabled = false; }
  });

  const keyState = key => key.revoked_at ? "已吊销" : key.expires_at && Date.parse(key.expires_at) <= Date.now() ? "已过期" : key.enabled ? "启用" : "停用";
  const policy = key => ({name:key.name,channels:key.channels,default_channel:key.default_channel,expires_at:key.expires_at,rpm:key.rpm,enabled:key.enabled});
  async function loadKeys() {
    if (!state.authenticated || ui.view !== "keys" || loadingKeys) return;
    loadingKeys = true;
    $("keyNew").disabled = true;
    try {
      const data = await api("/admin/api/keys"); keys = data.keys;
      $("keysNotice").textContent = "完整 Key 仅创建时显示一次。修改权限、停用或吊销后，对新请求立即生效。";
      $("keysList").innerHTML = keys.length ? `<div class="table-wrap" tabindex="0" role="region" aria-label="访问密钥列表"><table><thead><tr><th>名称 / 标识</th><th>状态</th><th>允许渠道</th><th>默认渠道</th><th>有效期</th><th>请求上限</th><th>操作</th></tr></thead><tbody>${keys.map(key => `<tr><td><b>${esc(key.name)}</b><div class="hint mono">${esc(key.prefix)}</div></td><td>${keyState(key)}</td><td>${esc(key.channels.join("、"))}</td><td>${esc(key.default_channel)}</td><td>${key.expires_at ? esc(fmtTime(key.expires_at)) : "长期有效"}</td><td>${key.rpm ? esc(key.rpm) + " / 分钟" : "不限"}</td><td>${key.revoked_at ? "—" : `<button data-key-edit="${esc(key.id)}">编辑</button> <button data-key-toggle="${esc(key.id)}">${key.enabled ? "停用" : "启用"}</button> <button class="danger" data-key-revoke="${esc(key.id)}">吊销</button>`}</td></tr>`).join("")}</tbody></table></div>` : '<div class="empty"><strong>尚无访问密钥</strong>创建 Key 后，客户端才可调用 API。</div>';
      $("keyChannels").innerHTML = data.channels.map(name => `<label><input type="checkbox" value="${esc(name)}"> ${esc(name)}${data.active_channels.includes(name) ? "" : "（未启用）"}</label>`).join("");
      $("keyNew").disabled = false;
    } catch (error) { $("keysNotice").textContent = "读取失败：" + error.message; }
    finally { loadingKeys = false; }
  }
  function syncDefault() {
    const selected = $("keyDefault").value;
    const channels = Array.from($("keyChannels").querySelectorAll("input:checked"), el => el.value);
    $("keyDefault").innerHTML = channels.map(name => `<option value="${esc(name)}">${esc(name)}</option>`).join("");
    if (channels.includes(selected)) $("keyDefault").value = selected;
  }
  function editKey(key = null) {
    editing = key; $("keyEditorTitle").textContent = key ? "编辑访问密钥" : "创建访问密钥";
    $("keyName").value = key?.name || ""; $("keyRPM").value = key?.rpm || 0; $("keyEnabled").checked = key ? key.enabled : true;
    $("keyExpiry").value = key?.expires_at ? new Date(Date.parse(key.expires_at) - new Date(key.expires_at).getTimezoneOffset()*60000).toISOString().slice(0,16) : "";
    $("keyChannels").querySelectorAll("input").forEach(el => el.checked = !!key?.channels.includes(el.value));
    syncDefault(); if (key) $("keyDefault").value = key.default_channel;
    $("keyEditorError").textContent = ""; $("keyDialog").showModal();
  }
  $("keyChannels").addEventListener("change", syncDefault);
  $("keyNew").addEventListener("click", () => editKey());
  $("keyCancel").addEventListener("click", () => { if (!saving) $("keyDialog").close(); });
  $("keyDialog").addEventListener("cancel", event => { if (saving) event.preventDefault(); });
  $("keySave").addEventListener("click", async () => {
    if (saving) return; saving = true; $("keySave").disabled = true;
    try {
      const body = {name:$("keyName").value,channels:Array.from($("keyChannels").querySelectorAll("input:checked"), el => el.value),default_channel:$("keyDefault").value,expires_at:$("keyExpiry").value ? new Date($("keyExpiry").value).toISOString() : null,rpm:Number($("keyRPM").value),enabled:$("keyEnabled").checked};
      if (!Number.isInteger(body.rpm) || body.rpm < 0) throw new Error("请求上限应为非负整数");
      const result = await api("/admin/api/keys" + (editing ? "/" + encodeURIComponent(editing.id) : ""), {method:editing ? "PATCH" : "POST",body:JSON.stringify(body)});
      $("keyDialog").close();
      if (result.token) { $("issuedKey").value = result.token; $("issuedKeyDialog").showModal(); }
      await loadKeys();
    } catch (error) { $("keyEditorError").textContent = error.message; }
    finally { saving = false; $("keySave").disabled = false; }
  });
  $("issuedKeyCopy").addEventListener("click", () => copyText($("issuedKey").value,"Key 已复制，请妥善保存"));
  $("issuedKeyClose").addEventListener("click", () => $("issuedKeyDialog").close());
  $("issuedKeyDialog").addEventListener("close", () => { $("issuedKey").value = ""; });
  $("keysList").addEventListener("click", async event => {
    const button = event.target.closest("button"); if (!button || button.disabled) return;
    const key = keys.find(key => key.id === (button.dataset.keyEdit || button.dataset.keyToggle || button.dataset.keyRevoke)); if (!key) return;
    if (button.dataset.keyEdit) { editKey(key); return; }
    if (button.dataset.keyRevoke) { revoking = key; $("keyRevokeName").textContent = key.name; $("keyRevokeError").textContent = ""; $("keyRevokeDialog").showModal(); return; }
    button.disabled = true;
    try { await api("/admin/api/keys/"+encodeURIComponent(key.id),{method:"PATCH",body:JSON.stringify({...policy(key),enabled:!key.enabled})}); await loadKeys(); }
    catch (error) { $("keysNotice").textContent = error.message; button.disabled = false; }
  });
  $("keyRevokeCancel").addEventListener("click", () => { if (!$("keyRevokeConfirm").disabled) $("keyRevokeDialog").close(); });
  $("keyRevokeDialog").addEventListener("cancel", event => { if ($("keyRevokeConfirm").disabled) event.preventDefault(); });
  $("keyRevokeConfirm").addEventListener("click", async () => {
    if (!revoking) return; $("keyRevokeConfirm").disabled = true;
    try { await api("/admin/api/keys/"+encodeURIComponent(revoking.id),{method:"DELETE"}); $("keyRevokeDialog").close(); await loadKeys(); }
    catch (error) { $("keyRevokeError").textContent = error.message; }
    finally { $("keyRevokeConfirm").disabled = false; }
  });
  $("keysRefresh").addEventListener("click", loadKeys);
  window.addEventListener("hashchange", loadKeys);
  connect();
})();
