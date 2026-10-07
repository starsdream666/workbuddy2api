// Package UI behavior without a browser dependency or real credentials.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(__dirname, '..');
const source = fs.readFileSync(path.join(root, 'internal/server/console/packages.js'), 'utf8');

class Element {
  constructor() { this.events = {}; this.innerHTML = ''; this.textContent = ''; this.checked = false; this.disabled = false; this.open = false; this.isConnected = true; this.attrs = {}; }
  addEventListener(name, fn) { (this.events[name] ||= []).push(fn); }
  emit(name, value = {}) { for (const fn of this.events[name] || []) fn(value); }
  setAttribute(k, v) { this.attrs[k] = v; }
  focus() { this.focused = true; }
  showModal() { this.open = true; }
  close() { this.open = false; this.emit('close'); }
}
function setup() {
  const elements = new Map();
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const state = {accounts:[{uid:'one&two',realm:'workbuddy',nickname:'测试账号'},{uid:'two',realm:'cn'}]};
  const requests = [], window = new Element();
  const api = (url, opts) => new Promise((resolve, reject) => requests.push({url, opts, resolve, reject}));
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  vm.runInNewContext(source, {$, state, api, window, esc, fmtTime: t => t, URLSearchParams, AbortController});
  function open(index = 0) {
    const account = state.accounts[index], button = new Element();
    button.dataset = {packageUid:account.uid,packageRealm:account.realm};
    $('accounts').emit('click', {target:{closest:()=>button}});
    return button;
  }
  return {$, state, window, requests, open};
}
const tick = () => new Promise(resolve => setImmediate(resolve));
function data(uid = 'one&two', realm = 'workbuddy') {
  return {uid,realm,checked_at:'2026-10-05T00:58:28Z',count:2,totals:[{unit:'credits',total:'130',used:'113.51',remaining:'16.49'}],packages:[
    {name:'<img src=x onerror=alert(1)>',unit:'credits',total:'30',used:'13.51',remaining:'16.49',precise:true,basis:'cycle',cycle_end:'2026-10-31 23:59:59',usable_until:'2034-12-13T00:00:00Z'},
    {name:'Free Plan Subscription',unit:'credits',total:'100',used:'100',remaining:'0',precise:true,basis:'cycle'}
  ]};
}

test('on-demand lookup, exact decimals, escaping, time distinction, filter and refresh', async () => {
  const app = setup(), {$, requests, open} = app;
  assert.equal(requests.length, 0);
  const trigger = open();
  assert.equal($('packageDialog').open, true);
  assert.equal($('packageRefresh').disabled, true);
  assert.equal(new URL(requests[0].url,'http://localhost').searchParams.get('uid'), 'one&two');
  requests[0].resolve(data()); await tick();
  assert.match($('packageContent').innerHTML,/113\.51/);
  assert.match($('packageContent').innerHTML,/16\.49/);
  assert.match($('packageContent').innerHTML,/&lt;img/);
  assert.doesNotMatch($('packageContent').innerHTML,/<img/);
  assert.match($('packageContent').innerHTML,/本周期结束.*2026-10-31.*资源有效至.*2034-12-13/s);
  assert.match($('packageContent').innerHTML,/未提供/);
  $('packageRemaining').checked = true; $('packageRemaining').emit('change');
  assert.match($('packageContent').innerHTML,/显示 1 \/ 2/);
  assert.doesNotMatch($('packageContent').innerHTML,/Free Plan Subscription/);
  assert.match($('packageContent').innerHTML,/113\.51/); // filtering never changes totals
  $('packageRefresh').emit('click');
  assert.equal(requests.length, 2);
  assert.doesNotMatch($('packageContent').innerHTML,/16\.49/);
  $('packageClose').emit('click');
  assert.equal(requests[1].opts.signal.aborted, true);
  assert.equal($('packageContent').innerHTML, '');
  assert.equal(trigger.focused, true);
});

test('failure is visible, retry works, empty is distinct from failure', async () => {
  const {$,requests,open} = setup(); open();
  requests[0].reject(new Error('模拟上游错误')); await tick();
  assert.match($('packageContent').innerHTML,/套餐查询失败.*模拟上游错误.*重试/s);
  assert.equal($('packageRefresh').disabled, false);
  $('packageContent').emit('click',{target:{closest:()=>true}});
  requests[1].resolve({...data(),count:0,totals:[],packages:[]}); await tick();
  assert.match($('packageContent').innerHTML,/当前查询范围内没有套餐/);
  assert.doesNotMatch($('packageContent').innerHTML,/套餐查询失败/);
});

test('closed or superseded requests cannot expose a different account snapshot', async () => {
  const {$,requests,open,window} = setup(); open();
  $('packageClose').emit('click'); open(1);
  requests[1].resolve(data('two','cn')); await tick();
  const current = $('packageContent').innerHTML;
  requests[0].resolve({...data(),totals:[{unit:'credits',total:'999',used:'0',remaining:'999'}]}); await tick();
  assert.equal($('packageContent').innerHTML,current);
  window.emit('console-key-changed');
  assert.equal($('packageDialog').open,false);
  assert.equal($('packageContent').innerHTML,'');
  open(); window.emit('pagehide');
  assert.equal(requests[2].opts.signal.aborted,true);
});

test('malformed responses and wrong identities show errors instead of zero totals', async () => {
  for (const response of [{...data(),uid:'another'}, {...data(),count:3}, {...data(),totals:[]}, {...data(),packages:[{},{}]}, {...data(),totals:[{total:'NaN',used:'0',remaining:'0'}]}]) {
    const {$,requests,open} = setup(); open(); requests[0].resolve(response); await tick();
    assert.match($('packageContent').innerHTML,/套餐查询失败/);
    assert.doesNotMatch($('packageContent').innerHTML,/package-totals/);
  }
});

test('shared API helper propagates caller cancellation and keeps timeout behavior', async () => {
  const consoleSource = fs.readFileSync(path.join(root,'internal/server/console/console.js'),'utf8');
  const helper = consoleSource.slice(consoleSource.indexOf('async function api('), consoleSource.indexOf('function fmtTime('));
  let request;
  const context = {state:{authenticated:true,csrf:'test-csrf',authVersion:1,adminRequests:new Set()}, AbortController, setTimeout, clearTimeout,
    fetch:(url,opts)=>new Promise((resolve,reject)=>{request=opts;opts.signal.addEventListener('abort',()=>reject(Object.assign(new Error('aborted'),{name:'AbortError'})));})};
  vm.createContext(context); vm.runInContext(helper,context);
  const controller = new AbortController(), pending = context.api('/test',{signal:controller.signal,method:'POST'});
  assert.equal(request.headers.Authorization,undefined);
  assert.equal(request.headers['X-CSRF-Token'],'test-csrf');
  assert.equal(request.credentials,'same-origin');
  controller.abort();
  await assert.rejects(pending,{name:'AbortError'});
  assert.equal(request.signal.aborted,true);
});
