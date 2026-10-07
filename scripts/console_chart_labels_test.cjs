// Non-zero bars must all have readable labels, including dense and equal peaks.
const {chromium, expect} = require('playwright/test');
const {spawn} = require('node:child_process');
const fs = require('node:fs'), path = require('node:path'), assert = require('node:assert/strict');

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
    const page = await browser.newPage({viewport: {width: 1440, height: 1050}, locale: 'zh-CN'});
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const out = path.resolve(__dirname, '../outputs/chart-labels');
    fs.mkdirSync(out, {recursive: true});
    let values = [];
    await page.route('**/admin/api/usage', async route => {
      const response = await route.fetch(), data = await response.json(), now = Date.now();
      data.entries = values.map((value, index) => ({
        ...data.entries[0], seq: index + 1, status: 200, total_tokens: value,
        credits_known: true, credits_used: value ? 9999.99 : 0,
        time: new Date(now - (23.5 - index) * 3600000).toISOString(),
      }));
      await route.fulfill({response, json: data});
    });
    const format = value => new Intl.NumberFormat('en-US', {
      maximumFractionDigits: value >= 10000 ? 1 : 2, notation: value >= 10000 ? 'compact' : 'standard',
    }).format(value);
    async function verify(expected) {
      const labels = page.locator('#trendChart .bar-count');
      await expect(labels).toHaveCount(expected.filter(value => value > 0).length);
      assert.deepEqual(await labels.allTextContents(), expected.filter(value => value > 0).map(format));
      const geometry = await page.locator('#trendChart svg').evaluate(svg => ({
        height: svg.viewBox.baseVal.height,
        labels: [...svg.querySelectorAll('.bar-count')].map(label => {
          const b = label.getBBox();
          return {text: label.textContent, x: b.x, y: b.y, width: b.width, height: b.height};
        }),
      }));
      for (let i = 0; i < geometry.labels.length; i++) {
        const a = geometry.labels[i];
        assert.ok(a.y >= 0 && a.y + a.height <= geometry.height, 'Label is not clipped: ' + a.text);
        for (const b of geometry.labels.slice(i + 1)) {
          const overlapX = Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x);
          const overlapY = Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y);
          assert.ok(overlapX <= 0 || overlapY <= 0, 'Labels do not overlap: ' + a.text + ' / ' + b.text);
        }
      }
    }
    const scenarios = {
      mixed: [1300000, 400000, 0, 394100, 2400000, 120000, 437600, 70000, 0, 611800, 0, 0,
        525100, 6800000, 0, 5800000, 7100000, 59000000, 13800000, 21800000, 30500000, 52200000, 42700000, 4000000],
      peaks: Array(24).fill(80000000),
      decimals: Array(24).fill(9999.99),
      zero: Array(24).fill(0),
    };
    for (const [name, fixture] of Object.entries(scenarios)) {
      values = fixture;
      await page.goto(origin + '/admin');
      await expect(page.locator('#gateText')).toHaveText('网关在线');
      for (const metric of ['tokens', 'credits', 'requests']) {
        await page.locator('#chartMetric').selectOption(metric);
        const expected = metric === 'tokens' ? values : values.map(value => metric === 'requests' ? 1 : value ? 9999.99 : 0);
        await verify(expected);
        if (metric === 'tokens' && name !== 'zero') {
          await page.locator('#trendChart').screenshot({path: path.join(out, name + '.png')});
        }
      }
    }
    values = scenarios.mixed;
    await page.goto(origin + '/admin');
    await page.locator('#chartMetric').selectOption('tokens');
    await expect(page.locator('#trendChart .bar-count')).toHaveCount(values.filter(value => value > 0).length);
    await page.locator('#trendChart [data-chart-detail]').nth(17).focus();
    await expect(page.locator('#chartTooltip')).toContainText('59,000,000 tokens');
    await page.setViewportSize({width: 390, height: 844});
    await verify(values);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'No mobile page overflow');
    await page.locator('#trendTitle').click();
    await page.locator('#trendChart').screenshot({path: path.join(out, 'mobile.png')});
    assert.deepEqual(errors, []);
    console.log('PASS complete labels, mixed values, equal peaks, decimals, zero buckets, all three metrics, no overlap/clipping, full-value tooltip and mobile layout');
  } finally {
    if (browser) await browser.close();
    child.kill();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
