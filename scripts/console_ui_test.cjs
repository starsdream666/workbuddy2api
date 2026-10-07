// Isolated browser regression. Uses only the mock preview server; no real credentials or upstream.
const {chromium}=require('playwright');
const {expect}=require('playwright/test');
const {spawn}=require('node:child_process');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
(async()=>{
 const child=spawn(process.execPath,[path.join(__dirname,'console_preview.cjs')],{env:{...process.env,CONSOLE_PREVIEW_PORT:'0'},windowsHide:true,stdio:['ignore','pipe','pipe']});
 let browser;
 try{
 const origin=await new Promise((resolve,reject)=>{let output='';const timer=setTimeout(()=>reject(Error('Preview startup timed out')),10000);child.stdout.on('data',chunk=>{output+=chunk;const match=output.match(/http:\/\/127\.0\.0\.1:\d+/);if(match){clearTimeout(timer);resolve(match[0]);}});child.on('error',reject);child.stderr.on('data',chunk=>{clearTimeout(timer);reject(Error(String(chunk)));});});
 browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_PATH||undefined});
 const page=await browser.newPage({viewport:{width:1440,height:1050},locale:'zh-CN',acceptDownloads:true});
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const out=path.resolve(__dirname,'../outputs/frontend');fs.mkdirSync(out,{recursive:true});
 let navigationID=0;
 async function loaded(scenario='normal',view='overview'){await page.goto(origin+'/admin?preview='+scenario+'&visit='+(++navigationID)+'#'+view);await expect(page.locator('#refreshBtn')).toBeEnabled();if(scenario!=='unauthorized')await expect(page.locator('#gateText')).toHaveText('网关在线');}
 async function settle(){await page.evaluate(async()=>{await document.fonts.ready;await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));});}
 async function navigate(view){await page.locator('[data-view="'+view+'"]').click();await expect(page.locator('[data-page="'+view+'"]')).toBeVisible();await expect(page.locator('[data-view="'+view+'"]')).toHaveAttribute('aria-current','page');await expect.poll(()=>page.evaluate(()=>window.scrollY)).toBe(0);await settle();}
 async function shot(name){await expect(page.locator('#toast')).not.toHaveClass(/show/);await page.evaluate(()=>window.scrollTo({top:0,behavior:'instant'}));await settle();await page.screenshot({path:path.join(out,name+'.png'),fullPage:true,animations:'disabled'});}
 async function noOverflow(width){await page.locator(".app-shell").evaluate(el=>Promise.all(el.getAnimations().map(a=>a.finished)));const actual=await page.evaluate(()=>document.documentElement.scrollWidth);assert.ok(actual<=width,'page overflow '+actual+' > '+width);}
 await loaded();
 const sidebarToggle=page.locator('#sidebarToggle');
 await expect(page.locator('.sidebar #sidebarToggle')).toHaveCount(1);
 await expect(page.locator('.topbar #sidebarToggle')).toHaveCount(0);
 const viewOrder=['overview','credentials','analysis','usage','tasks','settings'];
 assert.deepEqual(await page.locator('.sidebar nav a').evaluateAll(links=>links.map(a=>a.dataset.view)),viewOrder);
 assert.deepEqual(await page.locator('.sidebar nav a').evaluateAll(links=>links.map(a=>a.getAttribute('aria-label'))),['总览','账号管理','使用分析','请求日志','任务中心','连接设置']);
 await expect(page.locator('.sidebar')).toHaveCSS('background-color','rgb(251, 253, 252)');
 await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
 await page.setViewportSize({width:768,height:900});
 await expect(sidebarToggle).toHaveAttribute('aria-expanded','false');
 await page.setViewportSize({width:1440,height:1050});
 await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
 await sidebarToggle.click();
 await expect(sidebarToggle).toHaveAccessibleName('展开侧边栏');
 await expect.poll(()=>page.locator('.sidebar').evaluate(el=>Math.round(el.getBoundingClientRect().width))).toBe(76);
 await expect(page.locator('.app-shell')).toHaveCSS('margin-left','76px');
 for(const view of viewOrder)await navigate(view);
 await navigate('tasks');
 await expect(page.locator('#taskSubmit')).toBeEnabled();
 await page.locator('#taskSubmit').click();
 await expect(page.locator('#taskRows')).toContainText('余额巡检');
 await page.locator('#taskSubmit').click();
 await expect(page.locator('#toast')).toContainText('相同任务已在队列中');
 await expect(page.locator('#taskRows tr')).toHaveCount(1);
 await page.locator('#taskRows button').click();
 await expect(page.locator('#taskRows')).toContainText('已取消');
 await page.locator('#taskKind').selectOption('refresh_tokens');
 await page.locator('#taskSubmit').click();
 await expect(page.locator('#taskRows')).toContainText('刷新 Token');
 await shot('tasks-desktop');
 console.log('PASS maintenance submit, deduplication, cancellation and token operation');
 await navigate('settings');
 await navigate('overview');await shot('sidebar-collapsed-desktop');
 await loaded();await expect(sidebarToggle).toHaveAttribute('aria-expanded','false');
 await sidebarToggle.focus();await page.keyboard.press('Enter');
 await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
 await loaded();await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
 await expect(page.locator('.sidebar')).toHaveCSS('width','238px');
 await shot('sidebar-expanded-desktop');
 for(const width of [320,390,481,600,768,1000,1024]){
   await page.setViewportSize({width,height:900});
   for(const view of viewOrder){
     await navigate(view);await noOverflow(width);
     await sidebarToggle.click();await expect(sidebarToggle).toHaveAttribute('aria-expanded','false');
     if(width<=480)await expect(page.locator('#sidebarContent')).toBeHidden();
     await expect(sidebarToggle).toBeVisible();await noOverflow(width);
     await sidebarToggle.click();await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
   }
 }
 // The toggle must remain usable after scrolling, without Playwright scrolling it into view.
 async function toggleIsOnscreen(){
   await expect(sidebarToggle).toBeInViewport();
   assert.ok(await sidebarToggle.evaluate(el=>{
     const rect=el.getBoundingClientRect();
     const hit=document.elementFromPoint(rect.x+rect.width/2,rect.y+rect.height/2);
     return rect.top>=0 && rect.bottom<=innerHeight && el.contains(hit);
   }),'sidebar toggle must be visible and unobstructed');
 }
 for(const width of [1440,768,390,320]){
   await page.setViewportSize({width,height:600});await navigate('usage');
   await page.evaluate(()=>window.scrollTo({top:500,behavior:'instant'}));
   await expect.poll(()=>page.evaluate(()=>window.scrollY)).toBeGreaterThan(100);
   await toggleIsOnscreen();
   await sidebarToggle.click();await expect(sidebarToggle).toHaveAttribute('aria-expanded','false');
   await toggleIsOnscreen();assert.ok(await page.evaluate(()=>window.scrollY>100),'collapse must not jump to page top');
   await sidebarToggle.click();await expect(sidebarToggle).toHaveAttribute('aria-expanded','true');
   await toggleIsOnscreen();assert.ok(await page.evaluate(()=>window.scrollY>100),'expand must not jump to page top');
   if(width===1440||width===390)await page.screenshot({path:path.join(out,'sidebar-toggle-scrolled-'+width+'.png')});
 }
 await page.setViewportSize({width:1440,height:320});
 await page.locator('#sidebarContent').evaluate(el=>{el.scrollTop=el.scrollHeight;});
 await toggleIsOnscreen();await sidebarToggle.click();await toggleIsOnscreen();await sidebarToggle.click();
 await page.locator('#sidebarContent').evaluate(el=>{el.scrollTop=0;});
 await page.setViewportSize({width:1440,height:1050});
 console.log('PASS sidebar-contained toggle stays clickable after page and sidebar scrolling, including collapsed mobile navigation');
 const noStorage=await browser.newPage({viewport:{width:1440,height:900}});
 await noStorage.addInitScript(()=>Object.defineProperty(window,'localStorage',{get(){throw new DOMException('Storage disabled','SecurityError');}}));
 await noStorage.goto(origin+'/admin');
 await expect(noStorage.locator('#refreshBtn')).toBeEnabled();
 await noStorage.locator('#sidebarToggle').click();await expect(noStorage.locator('#sidebarToggle')).toHaveAttribute('aria-expanded','false');
 await noStorage.locator('#sidebarToggle').click();await expect(noStorage.locator('#sidebarToggle')).toHaveAttribute('aria-expanded','true');
 await noStorage.close();
 await page.evaluate(()=>localStorage.removeItem('wb2api_sidebar_collapsed'));
 await page.setViewportSize({width:1440,height:1050});
 console.log('PASS light sidebar, ordered navigation, collapsed routes, keyboard, persisted states, blocked storage and responsive sidebar combinations across all six views');
 await loaded();await expect.poll(()=>page.evaluate(()=>window.scrollY)).toBe(0);await expect(page.locator('#dashRequests')).toHaveText('86');await expect(page.locator('#dashSuccess')).toHaveText('91.9%');await expect(page.locator('#recentRequests tbody tr')).toHaveCount(5);await shot('overview-desktop');console.log('PASS dashboard metrics, trend, health and recent requests');
 await page.locator('[data-trend="1"]').click();await expect(page.locator('[data-trend="1"]')).toHaveAttribute('aria-pressed','true');assert.ok(Number(await page.locator('#trendRequests').textContent())<86);
 await page.locator('[data-view="usage"]').click();await expect(page.locator('#usage')).toBeVisible();await expect(page.locator('#overview')).toBeHidden();
 await page.locator('#logStatus').selectOption('failed');await expect(page.locator('#usageTable tbody tr')).toHaveCount(7);await expect(page.locator('#logFilterSummary')).toContainText('匹配 7 / 86');await page.locator('#logSearch').fill('no-such-model');await expect(page.locator('#usageTable')).toContainText('没有匹配的请求');await expect(page.locator('#exportLogs')).toBeDisabled();await page.locator('#resetLogFilters').click();await expect(page.locator('#usageTable tbody tr')).toHaveCount(20);console.log('PASS combined filters and honest empty state');
 await page.locator('#usagePageSize').selectOption('10');await page.locator('#usagePageLast').click();await expect(page.locator('#usageStreamCount')).toContainText('9 / 9');await expect(page.locator('#usageTable tbody tr')).toHaveCount(6);await page.locator('#logStatus').selectOption('failed');await expect(page.locator('#usageTable tbody tr')).toHaveCount(7);await expect(page.locator('#usagePager')).toBeHidden();await page.locator('#resetLogFilters').click();await page.locator('#usagePageSize').selectOption('20');await shot('request-logs-desktop');console.log('PASS filtered pagination and page reset');
 await page.locator('#usageTable [data-request]').first().click();await expect(page.locator('#requestDialog')).toBeVisible();await expect(page.locator('#requestDetail')).toContainText('claude-sonnet-4.6');await page.keyboard.press('Escape');await expect(page.locator('#requestDialog')).toBeHidden();console.log('PASS request details and keyboard dismissal');
 const downloadPromise=page.waitForEvent('download');await page.locator('#exportLogs').click();const download=await downloadPromise;const csv=fs.readFileSync(await download.path(),'utf8');assert.ok(csv.startsWith('\uFEFF'));assert.equal(csv.trim().split('\r\n').length,87);console.log('PASS CSV exports all matching pages');
 await page.locator('[data-view="analysis"]').click();await expect(page.locator('#usageStatRequests')).toHaveText('86');await page.locator('#usageTabModel').click();await expect(page.locator('#usageTabModel')).toHaveAttribute('aria-pressed','true');await expect(page.locator('#usageBreakdownTable tbody tr')).toHaveCount(5);await shot('usage-analysis-desktop');await page.locator('.skip-link').focus();await page.keyboard.press('Enter');await expect(page.locator('#analysis')).toBeVisible();await expect(page.locator('#mainContent')).toBeFocused();console.log('PASS cumulative analysis and dimension switching');
 await page.locator('[data-view="credentials"]').click();await expect(page.locator('#accounts tbody tr')).toHaveCount(8);await page.locator('#accountSearch').fill('主力');await expect(page.locator('#accounts tbody tr')).toHaveCount(1);await page.locator('#accountSearch').fill('');await shot('accounts-desktop');await page.locator('[data-toggle="0"]').click();await expect(page.locator('[data-toggle="0"]')).toHaveText('启用');await expect(page.locator('[data-toggle="0"]')).toBeEnabled();await page.locator('[data-toggle="0"]').click();await expect(page.locator('[data-toggle="0"]')).toHaveText('停用');console.log('PASS account disable and re-enable');
 await page.locator('[data-view="settings"]').click();await expect(page.locator('#apiKey')).toBeVisible();await expect(page.locator('#baseURL')).toHaveValue(origin+'/v1');await page.locator('#apiKey').fill('preview-only-key');await page.locator('#saveKeyBtn').click();await expect(page.locator('#toast')).toContainText('Key 已保存');await shot('settings-desktop');console.log('PASS account search, settings and API address');
 await page.locator('[data-view="usage"]').click();await page.locator('#usageAuto').uncheck();await page.evaluate(()=>applyPush({usage:{...state.usage,entries:[],summary:{...state.usage.summary,requests:999}}}));await expect(page.locator('#usageTable tbody tr')).toHaveCount(20);await page.locator('#usageAuto').check();await expect(page.locator('#usageAutoHint')).toContainText('随推送更新');await page.evaluate(()=>applyPush({usage:{...state.usage,summary:{...state.usage.summary,requests:100}}}));await expect(page.locator('#dashRequests')).toHaveText('100');console.log('PASS realtime pause and push updates');
 await loaded();await page.evaluate(()=>{state.usage.entries[0].model='<img src=x onerror="window.injected=true">';state.usage.summary.by_model[0].key='<script>alert(1)</script>';renderUsage();});await expect(page.locator('#modelRanking script')).toHaveCount(0);assert.equal(await page.evaluate(()=>window.injected),undefined);console.log('PASS escaped model and account text');
 for(const width of [320,390,768,1024]){await page.setViewportSize({width,height:900});for(const view of viewOrder){await navigate(view);await noOverflow(width);}if(width===390){await loaded();await navigate('overview');await shot('overview-mobile');await navigate('usage');await shot('request-logs-mobile');await navigate('tasks');await shot('tasks-mobile');}}
 console.log('PASS 24 responsive view/viewport combinations');
 await page.setViewportSize({width:1440,height:1050});await loaded('empty');await expect(page.locator('#dashRequests')).toHaveText('0');await expect(page.locator('#dashSuccess')).toHaveText('—');await expect(page.locator('#recentRequests')).toContainText('还没有调用记录');
 for(const scenario of ['disabled','memory']){await loaded(scenario);await expect(page.locator('#dashboardScope')).toContainText('统计不可用');await expect(page.locator('#dashRequests')).toHaveText('—');}
 await loaded('error');await expect(page.locator('#dashboardUsageError')).toContainText('模拟用量读取失败');await expect(page.locator('#dashRequests')).toHaveText('—');
 await loaded('unauthorized','settings');await expect(page.locator('#loadError')).toContainText('Key 无效或缺失');await expect(page.locator('#apiKey')).toBeVisible();
 await navigate('tasks');await expect(page.locator('#taskNotice')).toContainText('Key 无效或缺失');await expect(page.locator('#taskSubmit')).toBeDisabled();
 console.log('PASS empty, disabled, memory-only, server error and unauthorized states');
 await loaded('normal','usage');
 await expect.poll(()=>page.evaluate(()=>state.usageBusy)).toBe(false);
 const dismissed=page.waitForEvent('dialog').then(dialog=>dialog.dismiss());
 await page.locator('#usageClearBtn').click();await dismissed;
 await expect(page.locator('#dashRequests')).toHaveText('86');
 const accepted=page.waitForEvent('dialog').then(dialog=>dialog.accept());
 await page.locator('#usageClearBtn').click();await accepted;
 await expect(page.locator('#dashRequests')).toHaveText('0');await expect(page.locator('#usageNotice')).toContainText('已清空');console.log('PASS clear-log cancellation and confirmed reset');
 assert.deepEqual(errors,[]);
 console.log('All new console UI regressions passed. Screenshots: '+out);
 } finally {if(browser)await browser.close();child.kill();}
})().catch(e=>{console.error(e);process.exitCode=1;});
