// Browser acceptance using the local mock preview only. Never opens a real gateway.
const {chromium}=require('playwright'),{expect}=require('playwright/test');
const {spawn}=require('node:child_process'),fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
(async()=>{
  const child=spawn(process.execPath,[path.join(__dirname,'console_preview.cjs')],{env:{...process.env,CONSOLE_PREVIEW_PORT:'0'},windowsHide:true,stdio:['ignore','pipe','pipe']});
  let browser;
  try {
    const origin=await new Promise((resolve,reject)=>{
      let output='';const timer=setTimeout(()=>reject(Error('Preview startup timed out')),10000);
      child.stdout.on('data',chunk=>{output+=chunk;const match=output.match(/http:\/\/127\.0\.0\.1:\d+/);if(match){clearTimeout(timer);resolve(match[0]);}});
      child.on('error',error=>{clearTimeout(timer);reject(error);});child.stderr.on('data',chunk=>{clearTimeout(timer);reject(Error(String(chunk)));});
    });
    browser=await chromium.launch({headless:true,executablePath:process.env.BROWSER_PATH||undefined});
    const context=await browser.newContext({viewport:{width:1440,height:1050},locale:'zh-CN'});
    await context.route('**/*',route=>new URL(route.request().url()).origin===origin?route.continue():route.abort());
    const page=await context.newPage(),errors=[],patches=[];
    page.on('pageerror',error=>errors.push(error.message));
    page.on('request',request=>{if(request.method()==='PATCH'&&request.url()===origin+'/admin/api/settings')patches.push(request.postDataJSON());});
    const out=path.resolve(__dirname,'../outputs/settings-layout');fs.mkdirSync(out,{recursive:true});
    const nav=id=>page.locator('[data-settings-category="'+id+'"]');
    const row=key=>page.locator('[data-setting-row="'+key+'"]');
    const shot=name=>page.screenshot({path:path.join(out,name+'.png'),fullPage:false});
    const noOverflow=async()=>assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'page overflows horizontally');
    await page.goto(origin+'/admin#settings');
    await expect(page.locator('#settingsStatus')).toContainText('配置已读取');
    await expect(page.locator('[data-setting-row]')).toHaveCount(39);
    await expect(page.locator('[data-setting-row]:visible')).toHaveCount(6);
    await noOverflow();await shot('settings-desktop');
    for(const [id,count] of [['accounts',8],['requests',9],['automation',13],['logs',6],['compatibility',3]]){
      await nav(id).click();await expect(page.locator('[data-setting-row]:visible')).toHaveCount(count);
    }
    await nav('accounts').click();await page.locator('#setting-pool-max_in_flight').fill('7');
    await nav('logs').click();await page.locator('#setting-usage_log-max_backups').fill('4');
    await nav('changed').click();await expect(page.locator('[data-setting-row]:visible')).toHaveCount(2);
    await expect(page.locator('#settingsDirty')).toContainText('2 项');
    await page.locator('#settingsSearch').fill('最低额度');await expect(page.locator('[data-setting-row]:visible')).toHaveCount(1);
    await expect(row('pool.selection_mode')).toBeVisible();
    await page.locator('#settingsSearch').fill('nothing-matches-123');await expect(page.locator('#settingsEmpty')).toBeVisible();
    await page.locator('#settingsClearSearch').click();await expect(page.locator('#settingsCategoryTitle')).toHaveText('待保存');
    await nav('automation').click();await page.locator('#setting-schedule-checkin_hours').fill('9,9');
    await nav('compatibility').click();await page.locator('#settingsSave').click();
    await expect(page.locator('#settingsCategoryTitle')).toHaveText('自动任务');await expect(page.locator('#setting-schedule-checkin_hours')).toBeFocused();
    assert.equal(patches.length,0,'invalid hidden field submitted');
    await page.locator('#setting-schedule-checkin_hours').fill('9,21');await nav('changed').click();await page.locator('#settingsSave').click();
    await expect(page.locator('#settingsStatus')).toContainText('已保存并生效');assert.equal(patches.length,1);
    assert.deepEqual(patches[0].changes,{'pool.max_in_flight':7,'usage_log.max_backups':4});
    await expect(page.locator('#settingsSave')).toBeDisabled();
    await nav('requests').click();await row('upstream.idle_timeout_seconds').getByRole('button',{name:'常用配置：流空闲超时（秒）',exact:true}).click();
    await nav('common').click();await expect(row('upstream.idle_timeout_seconds')).toBeVisible();
    await page.reload();await expect(page.locator('#settingsStatus')).toContainText('配置已读取');await expect(row('upstream.idle_timeout_seconds')).toBeVisible();
    assert.ok((await page.evaluate(()=>JSON.parse(localStorage.getItem('wb2api.settings.favorites.v1')))).every(key=>typeof key==='string'&&key.includes('.')));
    await row('upstream.idle_timeout_seconds').getByRole('button',{name:'常用配置：流空闲超时（秒）',exact:true}).click();
    await nav('accounts').click();await expect(page.locator('#setting-pool-max_in_flight')).toHaveValue('7');
    await nav('requests').click();await shot('settings-category-desktop');
    await nav('common').focus();await page.keyboard.press('ArrowRight');await expect(nav('accounts')).toBeFocused();await page.keyboard.press('Enter');await expect(page.locator('#settingsCategoryTitle')).toHaveText('账号与调度');
    await row('pool.max_in_flight').locator('summary').click();await row('pool.max_in_flight').getByRole('button',{name:'填入默认值'}).click();await expect(page.locator('#setting-pool-max_in_flight')).toHaveValue('3');await page.locator('#settingsDiscard').click();await expect(page.locator('#setting-pool-max_in_flight')).toHaveValue('7');
    await page.locator('[data-view="credentials"]').click();await expect(page.locator('#authorization')).not.toHaveAttribute('open','');
    await page.getByRole('link',{name:'＋ 添加账号',exact:true}).click();await expect(page.locator('#loginRealm')).toBeVisible();await expect(page.locator('[data-view="credentials"]')).toHaveAttribute('aria-current','page');
    await page.locator('#loginBtn').click();await expect(page.locator('#loginHint')).toContainText('生成失败');await expect(page.locator('#toast')).toContainText('预览模式');
    await page.route(origin+'/admin/api/login/start',route=>route.fulfill({json:{auth_url:origin+'/preview-authorization',state:'layout-mock-state',expires_in_seconds:600}}));
    await page.route(origin+'/admin/api/login/poll?*',route=>route.fulfill({json:{status:'pending'}}));
    await page.locator('#loginBtn').click();await expect(page.locator('#loginUrl')).toHaveValue(origin+'/preview-authorization');await expect(page.locator('#loginBox')).toBeVisible();await shot('authorization-desktop');
    await page.locator('[data-view="account"]').click();await expect(page.locator('#currentAdminPassword')).toBeVisible();await expect(page.locator('#authorization')).not.toBeVisible();
    await page.locator('[data-view="connect"]').click();await expect(page.locator('#baseURL')).toHaveValue(origin+'/v1');await expect(page.getByRole('link',{name:'查看模型列表 →'})).toBeVisible();await shot('connect-desktop');
    // Standard clipboard API is stubbed locally so this check cannot touch the user's clipboard.
    await page.evaluate(()=>Object.defineProperty(navigator,'clipboard',{value:{writeText:async value=>{window.copiedPreviewText=value;}},configurable:true}));
    await page.locator('#copyBaseURL').click();assert.equal(await page.evaluate(()=>window.copiedPreviewText),origin+'/v1');
    await expect(page.locator('#toast')).not.toHaveClass(/show/);
    for(const width of [768,390,320]){
      await page.setViewportSize({width,height:844});await page.locator('[data-view="settings"]').click();await nav('accounts').click();await noOverflow();
      await page.locator('#setting-pool-max_in_flight').fill('8');await page.locator('#settingsSave').scrollIntoViewIfNeeded();
      const box=await page.locator('#settingsSave').boundingBox();assert.ok(box&&box.x>=0&&box.x+box.width<=width&&box.y+box.height<=844,'save button clipped');
      await page.locator('#settingsDiscard').click();await nav('common').click();await page.evaluate(()=>scrollTo(0,0));await shot('settings-'+width);
      if(width===390){const input=await page.locator('#setting-pool-selection_mode').boundingBox(),bar=await page.locator('.settings-savebar').boundingBox();assert.ok(input.y+input.height<bar.y,'mobile first setting hidden behind savebar');}
      await page.locator('[data-view="connect"]').click();await noOverflow();await expect(page.locator('#baseURL')).toBeVisible();
      if(width===390)await shot('connect-mobile');
      await page.locator('[data-view="account"]').click();await noOverflow();await expect(page.locator('#newAdminPassword')).toBeVisible();
    }
    // Storage restrictions must not prevent the settings UI from loading.
    const restricted=await browser.newContext();await restricted.addInitScript(()=>{Storage.prototype.getItem=()=>{throw new Error('blocked');};Storage.prototype.setItem=()=>{throw new Error('blocked');};});
    const restrictedPage=await restricted.newPage();restrictedPage.on('pageerror',error=>errors.push(error.message));await restrictedPage.goto(origin+'/admin#settings');await expect(restrictedPage.locator('#settingsStatus')).toContainText('配置已读取');await restricted.close();
    assert.deepEqual(errors,[]);console.log('PASS: 39 fields, categories, drafts, hidden validation, hot save, favorites, keyboard, relocated flows, 1440/768/390/320px, blocked storage.');console.log('Screenshots: '+out);
  } finally {if(browser)await browser.close();child.kill();}
})().catch(error=>{console.error(error);process.exitCode=1;});
