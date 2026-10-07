// 控制台浏览器回归：仅使用模拟接口，不读取真实配置、凭证或连接上游。
// NODE_PATH 指向安装了 @playwright/test 的目录；可用 BROWSER_PATH 指定本机浏览器。
// 运行：node scripts/console_test.cjs；截图输出到 outputs/。
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { chromium, expect } = require("playwright/test");
const root = path.resolve(__dirname, "..");
const html = fs.readFileSync(path.join(root, "internal/server/console.html"), "utf8");
const securitySource = fs.readFileSync(path.join(root, "internal/server/security.go"), "utf8");
const csp = securitySource.match(/header\.Set\("Content-Security-Policy", "([^"]+)"\)/)[1];
const out = path.join(root, "outputs");
const origin = "http://127.0.0.1:41863";
const authURL = "https://login.example/authorize?state=demo-123&realm=ai&redirect_uri=https%3A%2F%2Fexample.com%2Fdone";
const fixtures = [
  { uid: "demo-ai-primary", nickname: "主力账号", realm: "ai", credits: 1280, success_count: 428, err_total: 2, in_flight: 1 },
  { uid: "demo-cn-work", nickname: "工作账号", realm: "cn", credits: 860, success_count: 186, err_total: 0, in_flight: 0 },
  { uid: "demo-ai-backup", nickname: "备用账号", realm: "ai", credits: 620, success_count: 96, err_total: 1, in_flight: 0 },
  { uid: "demo-cn-cooling", nickname: "日常开发", realm: "cn", credits: 210, success_count: 54, err_total: 3, in_flight: 0, cooling: true, cool_remaining_seconds: 720, cool_kind: "请求频率限制" },
  { uid: "demo-ai-disabled", nickname: "测试账号", realm: "ai", credits: 0, success_count: 12, err_total: 4, in_flight: 0, disabled: true, disabled_reason: "凭证需要重新授权" }
].map(a => ({ ...a, credits_checked_at: "2026-09-14T07:50:00Z" }));

async function setup(browser, options = {}) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, locale: "zh-CN" });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", e => errors.push(e.message));
  const model = { accounts: structuredClone(fixtures), deletes: [], balances: 0, overviews: 0, reloads: 0, failDelete: false, failOverview: false, delayDelete: false, releaseDelete: null, ...options };
  if (options.storageBlocked) await context.addInitScript(() => {
    Storage.prototype.getItem = () => { throw new DOMException("Blocked", "SecurityError"); };
    Storage.prototype.setItem = () => { throw new DOMException("Blocked", "SecurityError"); };
  });
  await page.route(origin + "/**", async route => {
    const request = route.request(), url = new URL(request.url());
    const respond = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (url.pathname === "/admin") return route.fulfill({ contentType: "text/html; charset=utf-8", headers: {"Content-Security-Policy": csp}, body: html });
    const asset = {"/admin/assets/console.css":"console.css", "/admin/assets/metrics.js":"metrics.js", "/admin/assets/console.js":"console.js", "/admin/assets/tasks.js":"tasks.js", "/admin/assets/models.js":"models.js"}[url.pathname];
    if (asset) return route.fulfill({contentType: asset.endsWith(".css") ? "text/css" : "text/javascript",body:fs.readFileSync(path.join(root,"internal/server/console",asset),"utf8")});
    if (url.pathname === "/admin/api/overview") {
      model.overviews++;
      if (model.unauthorized) return respond({ error: { message: "bad key" } }, 401);
      if (model.failOverview) return respond({ error: "模拟列表刷新失败" }, 503);
      const accounts = model.accounts;
      return respond({ accounts, realms: ["ai", "cn"], default_realm: "ai", auth_dir: "./auths", totals: {
        accounts: accounts.length, healthy: accounts.filter(a => !a.disabled && !a.cooling).length,
        cooling: accounts.filter(a => a.cooling && !a.disabled).length, disabled: accounts.filter(a => a.disabled).length,
        credits: accounts.reduce((n, a) => n + a.credits, 0)
      }});
    }
    if (url.pathname === "/admin/api/models") {
      if (model.unauthorized) return respond({ error: { message: "bad key" } }, 401);
      return respond({ object: "list", count: 3, data: [
        { id: "glm-5.2", realm: "cn", credits: "x0.00", rate: 0, vendor: "智谱 GLM", raw_vendor: "e", context_length: 131072, max_output_tokens: 8192, supported_efforts: ["minimal", "low", "medium", "high", "xhigh", "max"], default_effort: "high" },
        { id: "gpt-6-astra", realm: "ai", credits: "x6.67", rate: 6.67, vendor: "OpenAI", raw_vendor: "e", context_length: 1000000, max_output_tokens: 128000, supported_efforts: ["low", "medium", "high", "xhigh", "max"], default_effort: "high" },
        { id: "gpt-image-2.5-sunburst", realm: "ai", kind: "image", vendor: "OpenAI", tags: ["text-to-image"] }
      ] });
    }
    if (url.pathname === "/admin/api/balance") { model.balances++; return respond({ updated: {}, failed: {} }); }
    if (url.pathname === "/admin/api/accounts") {
      assert.equal(request.method(), "DELETE");
      model.deletes.push(request.postDataJSON());
      if (model.delayDelete) await new Promise(resolve => { model.releaseDelete = resolve; });
      if (model.failDelete) return respond({ error: "模拟权限拒绝" }, 403);
      const target = request.postDataJSON();
      model.accounts = model.accounts.filter(a => a.realm !== target.realm || a.uid !== target.uid);
      if (model.failAfterDelete) model.failOverview = true;
      return respond({ ok: true, ...target });
    }
    if (url.pathname === "/admin/api/reload") { model.reloads++; return respond({ ok: true, accounts: { ai: 3, cn: 2 } }); }
    if (url.pathname === "/admin/api/login/start") return respond({ state: "demo-123", auth_url: authURL, expires_in_seconds: 600 });
    if (url.pathname === "/admin/api/login/poll") return respond(model.pollResult || { status: "pending" }, model.pollStatus || 200);
    return route.fulfill({ status: 404, body: "" });
  });
  await page.goto(origin + "/admin#credentials");
  await expect(page.locator("#refreshBtn")).toBeEnabled();
  return { context, page, model, errors };
}

(async () => {
  fs.mkdirSync(out, { recursive: true });
  const edge = "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe";
  const executablePath = process.env.BROWSER_PATH || (fs.existsSync(edge) ? edge : undefined);
  const browser = await chromium.launch({ headless: true, executablePath });
  let passed = 0;
  async function check(name, fn, options) {
    const app = await setup(browser, options);
    try { await fn(app); assert.deepEqual(app.errors, []); passed++; console.log("PASS " + name); }
    finally { if (app.model.releaseDelete) app.model.releaseDelete(); await app.context.close(); }
  }
  try {
    await check("桌面布局、统计、筛选、转义和移动端无页面横向溢出", async ({ page, model }) => {
      await expect(page.locator("#accounts tbody tr")).toHaveCount(5);
      await expect(page.locator("#tAccounts")).toHaveText("5");
      assert.equal(model.balances, 0);
      await page.screenshot({ path: path.join(out, "console-desktop.png"), fullPage: true });
      await page.locator("#accountSearch").fill("主力");
      await expect(page.locator("#accounts tbody tr")).toHaveCount(1);
      await page.locator("#accountSearch").fill("");
      await page.locator("#realmFilter").selectOption("cn");
      await expect(page.locator("#accounts tbody tr")).toHaveCount(2);
      await page.locator("#statusFilter").selectOption("cooling");
      await expect(page.locator("#accounts tbody tr")).toHaveCount(1);
      await page.locator("#realmFilter").selectOption("");
      await page.locator("#statusFilter").selectOption("");
      for (const width of [320, 375, 768, 1024]) {
        await page.setViewportSize({ width, height: 900 });
        const overflow = await page.evaluate(() => ({ width: window.innerWidth, scroll: document.documentElement.scrollWidth, elements: [...document.querySelectorAll("body *")].filter(el => el.getBoundingClientRect().right > window.innerWidth && !el.closest(".table-wrap")).map(el => [el.tagName, el.id, el.className, el.getBoundingClientRect().right]) }));
        assert.ok(overflow.scroll <= width, "overflow at " + width + " " + JSON.stringify(overflow));
      }
      await page.setViewportSize({ width: 390, height: 844 });
      await page.screenshot({ path: path.join(out, "console-mobile.png"), fullPage: true });
      model.accounts[0].nickname = '<img src=x onerror="window.injected=true">';
      await page.evaluate(() => loadOverview());
      await expect(page.locator("#accounts img")).toHaveCount(0);
      assert.equal(await page.evaluate(() => window.injected), undefined);
    });
    await check("删除确认默认取消、Esc/取消不发请求", async ({ page, model }) => {
      await page.locator("[data-delete]").first().click();
      await expect(page.locator("#deleteDialog")).toBeVisible();
      await expect(page.locator("#deleteCancel")).toBeFocused();
      await page.screenshot({ path: path.join(out, "console-delete-confirm.png"), fullPage: true });
      await page.keyboard.press("Escape");
      assert.equal(model.deletes.length, 0);
      await page.locator("[data-delete]").first().click();
      await page.locator("#deleteCancel").click();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(5);
      assert.equal(model.deletes.length, 0);
    });
    await check("确认删除准确携带 realm/uid、防重复点击、刷新列表与统计", async ({ page, model }) => {
      model.delayDelete = true;
      await page.locator("[data-delete]").first().click();
      await page.locator("#deleteConfirm").click();
      await expect.poll(() => model.deletes.length).toBe(1);
      await expect(page.locator("#deleteConfirm")).toBeDisabled();
      await page.keyboard.press("Escape");
      await expect(page.locator("#deleteDialog")).toBeVisible();
      model.releaseDelete();
      await expect(page.locator("#deleteDialog")).not.toBeVisible();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(4);
      await expect(page.locator("#tAccounts")).toHaveText("4");
      await expect(page.locator("#toast")).toHaveText("凭证已删除，列表已更新");
      assert.deepEqual(model.deletes, [{ realm: "ai", uid: "demo-ai-primary" }]);
    });
    await check("删除接口失败保留列表、显示错误并允许重试", async ({ page, model }) => {
      model.failDelete = true;
      await page.locator("[data-delete]").first().click();
      await page.locator("#deleteConfirm").click();
      await expect(page.locator("#deleteError")).toContainText("模拟权限拒绝");
      await expect(page.locator("#deleteConfirm")).toBeEnabled();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(5);
      model.failDelete = false;
      await page.locator("#deleteConfirm").click();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(4);
    });
    await check("删除成功但刷新失败明确区分，手动刷新可恢复", async ({ page, model }) => {
      model.failAfterDelete = true;
      await page.locator("[data-delete]").first().click();
      await page.locator("#deleteConfirm").click();
      await expect(page.locator("#loadError")).toContainText("凭证已删除，但列表刷新失败");
      model.failOverview = false;
      await page.locator("#refreshBtn").click();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(4);
      await expect(page.locator("#loadError")).not.toBeVisible();
    });
    await check("安全上下文使用 Clipboard API 写入完整授权链接", async ({ page, context }) => {
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#loginBtn").click();
      await expect(page.locator("#loginUrl")).toHaveValue(authURL);
      await page.locator("#copyBtn").click();
      await expect(page.locator("#copyBtn")).toHaveText("已复制");
      assert.equal(await page.evaluate(() => navigator.clipboard.readText()), authURL);
    });
    for (const mode of ["missing", "denied", "http"]) await check("剪贴板降级复制：" + mode, async ({ page, context }) => {
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#loginBtn").click();
      await expect(page.locator("#loginBox")).toBeVisible();
      await page.evaluate(mode => {
        window.originalClipboard = navigator.clipboard;
        if (mode === "missing") Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
        if (mode === "denied") Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: () => Promise.reject(new Error("Denied")) } });
        if (mode === "http") Object.defineProperty(window, "isSecureContext", { configurable: true, value: false });
      }, mode);
      await page.locator("#copyBtn").click();
      await expect(page.locator("#copyHint")).toContainText("已复制");
      assert.equal(await page.evaluate(() => window.originalClipboard.readText()), authURL);
      await expect(page.locator("body > textarea")).toHaveCount(0);
    });
    await check("所有自动复制失败时选中完整链接并提示手动复制", async ({ page }) => {
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#loginBtn").click();
      await expect(page.locator("#loginBox")).toBeVisible();
      await page.evaluate(() => {
        Object.defineProperty(navigator, "clipboard", { value: undefined });
        document.execCommand = () => false;
      });
      await page.locator("#copyBtn").click();
      await expect(page.locator("#copyHint")).toContainText("Ctrl+C");
      await expect(page.locator("#loginUrl")).toBeFocused();
      assert.equal(await page.locator("#loginUrl").evaluate(el => el.value.substring(el.selectionStart, el.selectionEnd)), authURL);
      await expect(page.locator("#copyBtn")).toBeEnabled();
    });
    await check("无链接时不静默失败", async ({ page }) => {
      await page.evaluate(() => copyLogin());
      await expect(page.locator("#toast")).toHaveText("请先生成授权链接");
    });
    await check("授权成功后热加载反馈及列表刷新", async ({ page, model }) => {
      await page.clock.install();
      model.pollResult = { status: "done", nickname: "模拟授权", uid: "demo-new", reload_error: "" };
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#loginBtn").click();
      await expect(page.locator("#loginBox")).toBeVisible();
      const before = model.overviews;
      await page.clock.fastForward(3100);
      await expect(page.locator("#loginHint")).toContainText("登录成功");
      await expect(page.locator("#loginBox")).not.toBeVisible();
      await expect.poll(() => model.overviews).toBeGreaterThan(before);
    });
    await check("过期会话 HTTP 错误正确呈现", async ({ page, model }) => {
      await page.clock.install();
      model.pollResult = { status: "expired", error: "授权会话已过期" }; model.pollStatus = 410;
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#loginBtn").click();
      await expect(page.locator("#loginBox")).toBeVisible();
      await page.clock.fastForward(3100);
      await expect(page.locator("#pollText")).toHaveText("会话已过期");
    });
    await check("自动余额刷新可连续执行两轮而非只执行一次", async ({ page, model }) => {
      await page.clock.install();
      await page.evaluate(() => scheduleCredits());
      const initial = model.balances;
      for (let n = 1; n <= 2; n++) {
        await page.clock.fastForward(600100);
        await expect.poll(() => model.balances).toBe(initial + n);
        await expect(page.locator("#creditsSchedule")).toHaveText("每 10 分钟自动刷新");
      }
    });
    await check("浏览器禁止 localStorage 仍可加载和保存临时 Key", async ({ page }) => {
      await expect(page.locator("#accounts tbody tr")).toHaveCount(5);
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#apiKey").fill("test-key-only");
      await page.locator("#saveKeyBtn").click();
      await expect(page.locator("#toast")).toContainText("仅在本页生效");
    }, { storageBlocked: true });
    await check("未授权展示可恢复错误，保存 Key 后重新加载", async ({ page, model }) => {
      await expect(page.locator("#loadError")).toContainText("Key 无效或缺失");
      model.unauthorized = false;
      await page.evaluate(() => { location.hash = "settings"; });
      await page.locator("#apiKey").fill("test-key-only");
      await page.locator("#saveKeyBtn").click();
      await expect(page.locator("#accounts tbody tr")).toHaveCount(5);
      await expect(page.locator("#loadError")).not.toBeVisible();
    }, { unauthorized: true });
    await check("重扫目录保留功能", async ({ page, model }) => {
      await page.locator("#reloadBtn").click();
      await expect(page.locator("#toast")).toContainText("目录已重扫");
      assert.equal(model.reloads, 1);
    });
    console.log(`\n${passed} browser checks passed. All API data is simulated.`);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
