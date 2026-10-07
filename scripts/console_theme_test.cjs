// Theme switching is entirely local; exercise it against the isolated mock server.
const {chromium} = require('playwright');
const {expect} = require('playwright/test');
const {spawn} = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const assert = require('node:assert/strict');

(async () => {
  const child = spawn(process.execPath, [path.join(__dirname, 'console_preview.cjs')], {
    env: {...process.env, CONSOLE_PREVIEW_PORT: '0'}, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
  });
  let browser;
  try {
    const origin = await new Promise((resolve, reject) => {
      let output = '';
      const timer = setTimeout(() => reject(Error('Preview startup timed out')), 10000);
      child.stdout.on('data', chunk => {
        output += chunk;
        const match = output.match(/http:\/\/127\.0\.0\.1:\d+/);
        if (match) { clearTimeout(timer); resolve(match[0]); }
      });
      child.on('error', reject);
      child.stderr.on('data', chunk => reject(Error(String(chunk))));
    });
    browser = await chromium.launch({headless: true, executablePath: process.env.BROWSER_PATH || undefined});
    const context = await browser.newContext({viewport: {width: 1440, height: 1000}, locale: 'zh-CN'});
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(origin + '/admin');
    await expect(page.locator('#gateText')).toHaveText('网关在线');
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'default');
    await expect(page.locator('.sidebar')).toHaveCSS('background-color', 'rgb(251, 253, 252)');
    const toggle = page.locator('#themeToggle');
    const menu = page.locator('#themeMenu');
    const out = path.resolve(__dirname, '../outputs/themes');
    fs.mkdirSync(out, {recursive: true});
    async function choose(theme) {
      await toggle.click();
      await expect(menu).toBeVisible();
      await page.locator('[data-theme-choice="' + theme + '"]').click();
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
      await expect(menu).toBeHidden();
      await expect(toggle).toBeFocused();
    }
    const positions = await page.locator('.topbar').evaluate(el => {
      const bounds = selector => el.querySelector(selector).getBoundingClientRect();
      return {status: bounds('.badge').right, toggle: bounds('.theme-picker').left, pickerRight: bounds('.theme-picker').right, account: bounds('.account-entry').left};
    });
    assert.ok(positions.status < positions.toggle && positions.pickerRight <= positions.account);
    let documentRequests = 0;
    page.on('request', request => { if (request.isNavigationRequest()) documentRequests++; });
    for (const theme of ['mint', 'grid', 'soft', 'default']) {
      await choose(theme);
      await expect(page.locator('.theme-backdrop')).toHaveCSS('display', theme === 'default' ? 'none' : 'block');
      await toggle.click();
      await expect(page.locator('[data-theme-choice="' + theme + '"]')).toHaveAttribute('aria-checked', 'true');
      await page.screenshot({path: path.join(out, theme + '-desktop.png'), animations: 'disabled'});
      await page.keyboard.press('Escape');
    }
    assert.equal(documentRequests, 0, 'Theme switching must not navigate');
    await choose('mint');
    await page.locator('[data-view="credentials"]').click();
    await page.locator('#accountSearch').fill('demo');
    await choose('soft');
    await expect(page.locator('#accountSearch')).toHaveValue('demo');
    await expect(page.locator('[data-page="credentials"]')).toBeVisible();
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'soft');
    await toggle.press('ArrowDown');
    await expect(page.locator('[data-theme-choice="default"]')).toBeFocused();
    await page.keyboard.press('End');
    await expect(page.locator('[data-theme-choice="soft"]')).toBeFocused();
    await page.keyboard.press('Home');
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'mint');
    await toggle.click();
    await page.locator('#pageTitle').click();
    await expect(menu).toBeHidden();
    await toggle.click();
    await page.keyboard.press('Tab');
    await expect(menu).toBeHidden();
    const other = await context.newPage();
    await other.goto(origin + '/admin');
    await other.locator('#themeToggle').click();
    await other.locator('[data-theme-choice="grid"]').click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'grid');
    await other.close();
    await page.locator('[data-view="overview"]').click();
    await choose('mint');
    await page.emulateMedia({reducedMotion: 'reduce'});
    await expect(page.locator('.theme-backdrop>span').first()).toHaveCSS('animation-name', 'none');
    await expect(page.locator('[data-page="overview"]')).toHaveCSS('animation-name', 'none');
    for (const width of [768, 390, 320]) {
      await page.setViewportSize({width, height: 844});
      await toggle.click();
      await expect(menu).toBeVisible();
      const box = await menu.boundingBox();
      assert.ok(box.x >= 0 && box.x + box.width <= width, 'Dropdown fits viewport at ' + width);
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'No page overflow at ' + width);
      await page.screenshot({path: path.join(out, 'mint-' + width + '.png'), animations: 'disabled'});
      await page.keyboard.press('Escape');
    }
    const blocked = await browser.newContext();
    await blocked.addInitScript(() => {
      Object.defineProperty(window, 'localStorage', {get() { throw new DOMException('Blocked', 'SecurityError'); }});
    });
    const blockedPage = await blocked.newPage();
    await blockedPage.goto(origin + '/admin');
    await blockedPage.locator('#themeToggle').click();
    await blockedPage.locator('[data-theme-choice="grid"]').click();
    await expect(blockedPage.locator('html')).toHaveAttribute('data-theme', 'grid');
    await page.evaluate(() => localStorage.setItem('workbuddy.console.theme', 'unknown'));
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'default');
    assert.deepEqual(errors, []);
    console.log('PASS four themes, default appearance, header placement, no reload, preserved filters, persistence, keyboard, outside click, cross-tab sync, reduced motion, responsive layout, blocked storage and invalid preference');
  } finally {
    if (browser) await browser.close();
    child.kill();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
