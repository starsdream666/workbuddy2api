// Mock-only regression for full history and shared analytics views.
const {chromium,expect}=require('playwright/test');
const {spawn}=require('node:child_process');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
(async()=>{
 const child=spawn(process.execPath,[path.join(__dirname,'console_preview.cjs')],{env:{...process.env,CONSOLE_PREVIEW_PORT:'0'},windowsHide:true,stdio:['ignore','pipe','pipe']});
 let browser;
 try {
  const origin=await new Promise((resolve,reject)=>{let output='';const timer=setTimeout(()=>reject(Error('Preview startup timed out')),10000);child.stdout.on('data',chunk=>{output+=chunk;const url=output.match(/http:\/\/127\.0\.0\.1:\d+/);if(url){clearTimeout(timer);resolve(url[0]);}});child.on('error',reject);child.stderr.on('data',chunk=>reject(Error(String(chunk))));});
  browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_PATH||undefined});
  const page=await browser.newPage({viewport:{width:1440,height:1100},locale:'zh-CN',timezoneId:'Asia/Shanghai'});
  const errors=[];page.on('pageerror',error=>errors.push(error.message));
  const out=path.resolve(__dirname,'../outputs/request-history');fs.mkdirSync(out,{recursive:true});
  async function shot(name) {
   await page.evaluate(async()=>{await document.fonts.ready;await Promise.all(document.getAnimations().map(a=>a.finished));window.scrollTo({top:0,behavior:'instant'});});
   await page.screenshot({path:path.join(out,name),fullPage:true,animations:'disabled'});
  }
  await page.goto(origin+'/admin?preview=history');
  await expect(page.locator('#trendScope')).toContainText('650');
  const fixture=await (await page.request.get(origin+'/admin/api/usage')).json();
  const totals={requests:650,tokens:fixture.entries.reduce((s,e)=>s+e.total_tokens,0),credits:fixture.entries.reduce((s,e)=>s+(e.credits_known?e.credits_used:0),0)};
  for(const metric of ['requests','tokens','credits']) {
   await page.locator('#chartMetric').selectOption(metric);
   await page.locator('[data-chart-mode="bars"]').click();
   await page.locator('#statsRange').selectOption('all');
   assert.ok(Math.abs(Number((await page.locator('#trendRequests').getAttribute('title')).replace(/[^0-9.]/g,''))-totals[metric])<.02);
   await expect(page.locator('#trendChart svg')).toBeVisible();
   await page.locator('#trendChart [data-chart-detail]').first().hover();
   await expect(page.locator('#chartTooltip')).toBeVisible();
   await page.locator('#trendTitle').hover();
   await shot(metric+'-bars-desktop.png');
   for(const range of ['72','week','month','all']) {await page.locator('#statsRange').selectOption(range);await expect(page.locator('#statsRange')).toHaveValue(range);}
   await page.locator('[data-chart-mode="heatmap"]').click();
   await expect(page.locator('.heatmap-day')).toHaveCount(new Date().getFullYear()%4===0?366:365);
   await page.locator('.heatmap-day[data-level="4"]').first().hover();
   await expect(page.locator('#chartTooltip')).toBeVisible();
   if(metric==='credits')await expect(page.locator('#chartTooltip')).toContainText('积分');
   await page.locator('#trendTitle').hover();
   await shot(metric+'-heatmap-desktop.png');
   await page.locator('#heatmapYear').selectOption(String(new Date().getFullYear()-1));
   await expect(page.locator('#trendScope')).toContainText(String(new Date().getFullYear()-1));
   await page.locator('#statsRange').selectOption('month');
   await expect(page.locator('#heatmapYear')).toBeDisabled();
   await expect(page.locator('#heatmapYear')).toHaveValue(String(new Date().getFullYear()));
   await page.locator('#statsRange').selectOption('all');
   const chart=await page.locator('#trendChart').boundingBox(),toggle=await page.locator('.chart-mode').boundingBox();
   assert.ok(toggle.y>=chart.y+chart.height-1,'view switch must be below chart');
   assert.ok(Math.abs((toggle.x+toggle.width/2)-(chart.x+chart.width/2))<2,'view switch must be centered');
  }
  await page.locator('[data-view="usage"]').click();
  await expect(page.locator('#usagePageSize')).toHaveValue('all');
  await expect(page.locator('#usageTable tbody tr')).toHaveCount(650);
  await page.locator('#usagePageSize').selectOption('custom');
  await page.locator('#usagePageSizeCustom').fill('500');
  await expect(page.locator('#usageTable tbody tr')).toHaveCount(500);
  await page.locator('#usagePageLast').click();
  await expect(page.locator('#usageTable tbody tr')).toHaveCount(150);
  await page.locator('#usageTable button[data-request="0"]').click();
  await expect(page.locator('#requestTitle')).toHaveText('请求 #1');
  await page.locator('#closeRequest').click();
  await page.locator('#usagePageSize').selectOption('all');
  await page.locator('#logHours').selectOption('week');
  await expect(page.locator('#usageStreamCount')).not.toContainText('650');
  await page.locator('#resetLogFilters').click();
  await expect(page.locator('#usageTable tbody tr')).toHaveCount(650);
  await page.locator('[data-view="overview"]').click();
  for(const width of [390,768]) {
   await page.setViewportSize({width,height:1000});
   for(const mode of ['bars','heatmap']) {
    await page.locator('[data-chart-mode="'+mode+'"]').click();
    await page.evaluate(()=>Promise.all(document.getAnimations().map(a=>a.finished)));
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'page must not overflow');
    await shot(mode+'-'+width+'.png');
   }
  }
  await page.goto(origin+'/admin?preview=empty');
  await expect(page.locator('#trendRequests')).toHaveText('0');
  await page.locator('[data-chart-mode="heatmap"]').click();
  await expect(page.locator('.heatmap-day[data-level="4"]')).toHaveCount(0);
  assert.deepEqual(errors,[]);
  console.log('PASS: 650-row history, unlimited custom pagination, metric totals, all six views, time/year controls, tooltips, centered switch, empty state, desktop/tablet/mobile layouts');
 } finally {if(browser)await browser.close();child.kill();}
})().catch(error=>{console.error(error);process.exitCode=1;});
