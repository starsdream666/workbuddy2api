// 选号相关控制台用例：策略下拉的中文文案、缺表回落、以及选号对话框的接线完整性。
//
// 为什么单独一个文件：settings.js 的选中文案与 console.js 的选号对话框属于同一次改动，
// 放在一起才能在同一个 harness 里互相印证（下拉里选的"自定义优先级"对应的就是
// 对话框里填的那个优先级）。
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');
const read = (p) => fs.readFileSync(path.join(root, p), 'utf8');
const html = read('internal/server/console.html');
const settingsJS = read('internal/server/console/settings.js');
const consoleJS = read('internal/server/console/console.js');
const consoleModelsJS = read('internal/server/console/models.js');

class Element {
  constructor(tag = 'div', nodes = new Map()) { this.tag = tag; this.nodes = nodes; this.children = []; this.dataset = {}; this.listeners = {}; this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.open = false; this.textContent = ''; this.classes = new Set(); this.classList = { add: (c) => this.classes.add(c), toggle: (c, on) => on ? this.classes.add(c) : this.classes.delete(c) }; }
  set id(v) { this._id = v; this.nodes.set(v, this) } get id() { return this._id }
  append(...nodes) { for (const n of nodes) { n.parent = this; this.children.push(n) } }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes) }
  addEventListener(name, fn) { (this.listeners[name] ??= []).push(fn) }
  async emit(name, event = {}) { await Promise.all((this.listeners[name] || []).map(fn => fn({ preventDefault() {}, target: this, ...event }))) }
  setAttribute(name, value) { this[name] = value }
  setCustomValidity(value) { this.validityMessage = value }
  checkValidity() { return !this.validityMessage }
  reportValidity() { this.reported = true; return this.checkValidity() }
  matches(selector) { return selector === '[data-setting-row]' ? !!this.dataset.settingRow : this.tag === selector }
  querySelectorAll(selector) { return this.children.flatMap(n => [...(n.matches(selector) ? [n] : []), ...n.querySelectorAll(selector)]) }
  closest(selector) { for (let n = this; n; n = n.parent) if (n.matches(selector)) return n; return null }
}

// fixture 带一个 select 字段：取值是配置里的英文，文案另由 labels 表给。
const LABELS = {
  weighted: '随机调用（打散热点，各号均衡分配）',
  lowest_credits: '最低额度优先（集中打光一个号再换下一个）',
  highest_credits: '最高额度优先（先吃厚号，低额度号留后）',
  custom_priority: '自定义优先级（按账号优先级排序）',
};
const fixture = (withLabels = true) => {
  const snap = {
    fields: [{ key: 'pool.selection_mode', group: '账号池', label: '选号策略', kind: 'select', min: 0, max: 0, options: Object.keys(LABELS), help: '选号策略' }],
    values: { 'pool.selection_mode': 'custom_priority' },
    current: { 'pool.selection_mode': 'custom_priority' },
    defaults: { 'pool.selection_mode': 'weighted' },
    locked: {}, revision: 'rev1', pending: [], restart_required: false, timezone: 'CST +08:00',
  };
  if (withLabels) snap.labels = { 'pool.selection_mode': { ...LABELS } };
  return snap;
};

function app() {
  const nodes = new Map();
  const document = { createElement: (tag) => new Element(tag, nodes) };
  for (const [, id] of html.matchAll(/\bid="([^"]+)"/g)) { const node = new Element('div', nodes); node.id = id }
  const $ = (id) => { assert.ok(nodes.has(id), 'missing DOM ' + id); return nodes.get(id) };
  const window = new Element(), state = { authenticated: true, authVersion: 1, csrf: '' }, ui = { view: 'settings' }, calls = [];
  const api = (url, opts) => new Promise((resolve, reject) => calls.push({ url, opts, resolve, reject }));
  vm.runInNewContext(settingsJS, { $, document, window, state, ui, api });
  return { $, document, window, state, ui, calls, nodes };
}
const tick = () => new Promise((resolve) => setImmediate(resolve));

// modelsApp 载入 models.js 的 harness。models.js 依赖 console.js 暴露的一批全局
// （$ / state / ui / api / toast / busy / esc / compact / document / window），
// 这里按**真实签名**提供最小实现——尤其 busy 必须与 console.js 的实现同语义
// （进入时记下原文案、恢复时还原），否则"刷新完成后按钮还原成刷新"这条断言
// 就变成在测假实现。
function modelsApp() {
  const nodes = new Map();
  const document = { hidden: false, createElement: (tag) => new Element(tag, nodes), addEventListener() {} };
  for (const [, id] of html.matchAll(/\bid="([^"]+)"/g)) { const node = new Element('div', nodes); node.id = id }
  const $ = (id) => { assert.ok(nodes.has(id), 'missing DOM ' + id); return nodes.get(id) };
  const calls = [], toasts = [];
  const state = { authenticated: false, authVersion: 1, models: null };
  const ui = { view: 'models' };
  const api = (url, opts) => new Promise((resolve, reject) => calls.push({ url, opts, resolve, reject }));
  const esc = (v) => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const compact = (n) => String(n);
  const toast = (msg, ms, kind = 'success') => { toasts.push({ msg, kind }); };
  // 与 console.js 的 busy 同语义：首次进入记下原文案，恢复时还原。
  const busy = (id, value, label) => {
    const b = $(id);
    if (!b.dataset.label) b.dataset.label = b.textContent;
    b.disabled = value; b.setAttribute('aria-busy', String(value));
    b.textContent = value ? label : b.dataset.label;
  };
  const window = new Element();
  window.addEventListener = () => {};
  // 按钮初始文案由 harness 先摆好——真实 DOM 里它来自 console.html。
  $('modelsRefreshBtn').textContent = '刷新';
  vm.runInNewContext(consoleModelsJS, { $, document, window, state, ui, api, toast, busy, esc, compact });
  return { $, document, window, state, ui, calls, toasts, nodes };
}

// 取某个 select 字段渲染出的 option 文案（value → 显示文本）。
function options($, key) {
  const select = $(`setting-${key.replaceAll('.', '-')}`);
  return select.children.map((o) => [o.value, o.textContent]);
}

test('选号策略下拉显示中文文案，提交的仍是英文取值', async () => {
  const a = app();
  a.calls[0].resolve(fixture()); await tick();

  const rendered = options(a.$, 'pool.selection_mode');
  for (const [value, text] of rendered) {
    assert.notEqual(text, value, `${value} 未中文化`);
    assert.equal(text, LABELS[value], `${value} 文案不符`);
  }
  // 选项顺序即服务端给的顺序：随机 → 最低 → 最高 → 自定义。
  assert.deepEqual(rendered.map((o) => o[0]), ['weighted', 'lowest_credits', 'highest_credits', 'custom_priority']);

  // 当前值 custom_priority 必须被选中（value 是机器取值，显示是中文）。
  const select = a.$('setting-pool-selection_mode');
  assert.equal(select.value, 'custom_priority');
});

test('缺少 labels 表时回落显示原始取值，不出现空白选项', async () => {
  const a = app();
  a.calls[0].resolve(fixture(false)); await tick();
  const rendered = options(a.$, 'pool.selection_mode');
  assert.deepEqual(rendered.map((o) => o[1]), ['weighted', 'lowest_credits', 'highest_credits', 'custom_priority']);
  for (const [, text] of rendered) assert.ok(text.trim(), '选项文案不能为空');
});

test('未在目录里的当前值也会被补进下拉并显示原始取值', async () => {
  const a = app();
  const snap = fixture();
  snap.values['pool.selection_mode'] = 'legacy_mode'; // 目录里没有的取值
  a.calls[0].resolve(snap); await tick();
  const rendered = options(a.$, 'pool.selection_mode');
  assert.equal(rendered[0][0], 'legacy_mode', '未知当前值应补在下拉首位，否则选中项会丢');
  assert.equal(rendered[0][1], 'legacy_mode');
});

test('选号对话框的按钮都接到已注册的动作上', () => {
  // 每个 data-console-action 都必须在 console.js 的 consoleActions 表里存在，
  // 否则按钮是个哑巴：点击毫无反应，且不会有任何报错。
  //
  // 这条曾放行过 modelsRefresh（模型页刷新按钮，HTML 里挂了 data-console-action
  // 但没有任何脚本注册它）。现已修好——改为在 models.js 里按 id 绑定，与
  // tasks.js / packages.js 的刷新按钮同款。**不再保留任何豁免**：
  // 全量校验才能保证下一个哑巴按钮当场就被抓住。
  const registered = new Set([...consoleJS.matchAll(/^\s{2}([a-zA-Z]+):\s*/gm)].map((m) => m[1]));
  const used = [...html.matchAll(/data-console-action="([^"]+)"/g)].map((m) => m[1]);
  assert.ok(used.length > 0, '未找到任何 data-console-action 按钮');
  for (const action of used) {
    assert.ok(registered.has(action), `动作 ${action} 未在 consoleActions 注册（按钮会变成哑巴）`);
  }
  for (const action of ['saveSelection', 'closeSelection']) {
    assert.ok(used.includes(action), `缺少选号对话框按钮 ${action}`);
    assert.ok(registered.has(action), `选号动作 ${action} 未注册`);
  }
});

test('选号对话框的控件 id 与 console.js 引用一致，且无重复 id', () => {
  const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]);
  assert.equal(new Set(ids).size, ids.length, '存在重复 id');
  for (const m of consoleJS.matchAll(/\$\("([^"]+)"\)/g)) assert.ok(ids.includes(m[1]), 'missing ' + m[1]);
  for (const id of ['selectionDialog', 'selectionExcluded', 'selectionPlacement', 'selectionPriority', 'selectionSave', 'selectionCancel', 'selectionError']) {
    assert.ok(ids.includes(id), `缺少选号对话框控件 ${id}`);
  }
});

// ── 模型页刷新按钮（曾是个哑巴按钮：挂了 data-console-action 但没人注册）──

test('模型刷新按钮：HTML 不再挂无人处理的 data-console-action，改为按 id 绑定', () => {
  const htmlSnippet = html.match(/<button id="modelsRefreshBtn"[^>]*>/);
  assert.ok(htmlSnippet, '未找到 modelsRefreshBtn');
  assert.doesNotMatch(htmlSnippet[0], /data-console-action/, 'modelsRefreshBtn 不该再用 data-console-action（那张表在 console.js 里，调不到 models.js 的私有函数）');
  // models.js 必须显式绑定这个 id——与 tasks.js（tasksRefresh）、packages.js（packageRefresh）同款。
  assert.match(consoleModelsJS, /\$\("modelsRefreshBtn"\)\.addEventListener\("click"/, 'models.js 未绑定 modelsRefreshBtn 的点击');
});

test('模型刷新按钮：点击真的发请求并有回执（实证，不只是静态断言）', async () => {
  const a = modelsApp();
  // 初始自动加载（页面不可见时被守卫跳过，故不发请求）。
  assert.equal(a.calls.length, 0, '初始 state 未认证，不该发请求');

  a.state.authenticated = true;
  a.state.models = { data: [] };
  const click = a.$('modelsRefreshBtn').emit('click');
  await tick();
  assert.equal(a.calls.length, 1, '点击刷新必须发一次请求');
  assert.equal(a.calls[0].url, '/admin/api/models');

  // 在途时按钮进入忙态（文案变化 + 禁用），防止重复点击。
  assert.equal(a.$('modelsRefreshBtn').disabled, true, '刷新中按钮应禁用');
  assert.equal(a.$('modelsRefreshBtn').textContent, '刷新中…', '刷新中应有可见文案');

  a.calls[0].resolve({ data: [{ id: 'glm-5.2', realm: 'cn', rate: 0 }] });
  await click;
  assert.equal(a.$('modelsRefreshBtn').disabled, false, '刷新完成后按钮应恢复');
  assert.equal(a.$('modelsRefreshBtn').textContent, '刷新', '刷新完成后应还原按钮文案');
  assert.match(a.$('modelsUpdated').textContent, /^更新于 /, '成功后应盖上更新时间');
  assert.ok(a.toasts.some(t => t.msg.includes('已刷新')), '成功后应有 toast 回执');
  assert.equal(a.$('modelsError').hidden, true, '成功后不该显示错误');
});

test('模型刷新按钮：失败时给出页内错误与 toast，且按钮恢复可点', async () => {
  const a = modelsApp();
  a.state.authenticated = true;
  a.state.models = { data: [] };
  const click = a.$('modelsRefreshBtn').emit('click');
  await tick();
  a.calls[0].reject(new Error('模拟上游 503'));
  await click;
  assert.equal(a.$('modelsError').hidden, false, '失败必须显示错误');
  assert.match(a.$('modelsError').textContent, /刷新失败/);
  assert.ok(a.toasts.some(t => t.kind === 'error'), '失败应有 error toast');
  assert.equal(a.$('modelsRefreshBtn').disabled, false, '失败后按钮要能再点');
  assert.equal(a.$('modelsRefreshBtn').textContent, '刷新');
});

test('模型刷新按钮：在途重复点击不叠加请求', async () => {
  const a = modelsApp();
  a.state.authenticated = true;
  a.state.models = { data: [] };
  const first = a.$('modelsRefreshBtn').emit('click');
  await tick();
  await a.$('modelsRefreshBtn').emit('click'); // 在途再点
  await tick();
  assert.equal(a.calls.length, 1, '在途重复点击不该再发请求');
  a.calls[0].resolve({ data: [] });
  await first;
});

test('模型刷新按钮：会话已切换时丢弃回包且不覆盖新会话的 UI', async () => {
  const a = modelsApp();
  a.state.authenticated = true;
  a.state.models = { data: [] };
  const click = a.$('modelsRefreshBtn').emit('click');
  await tick();
  // 模拟登出：authVersion 变化。
  a.state.authenticated = false;
  a.state.authVersion++;
  a.calls[0].resolve({ data: [{ id: 'stale', realm: 'cn' }] });
  await click;
  assert.equal(a.state.models.data.length, 0, '过期会话的回包不该写进 state');
  assert.ok(!a.toasts.some(t => t.msg.includes('已刷新')), '过期会话不该报"已刷新"');
});

test('选号保存走 PATCH /admin/api/accounts 且 payload 形状正确', () => {
  // 用 vm 载入 console.js 代价过高（它依赖大量 DOM 与其它视图脚本），
  // 改用静态断言锁住关键契约：端点、方法、字段名三者必须同时出现，任何一处改名都会被抓住。
  assert.match(consoleJS, /"\/admin\/api\/accounts"/, '选号保存必须复用账号 PATCH 端点');
  assert.match(consoleJS, /method: "PATCH"/, '选号保存必须是 PATCH');
  // payload 形状：selection.{excluded, priority} 恒发，placement 只在排除时附加。
  assert.match(consoleJS, /selection: \{[\s\S]{0,200}?excluded:/, 'payload 缺少 selection.excluded');
  assert.match(consoleJS, /selection: \{[\s\S]{0,200}?priority,/, 'payload 缺少 selection.priority');
  // 未排除时不传 placement：服务端据此清掉残留落位（语义上无效的值不该留在 state.json）。
  assert.match(consoleJS, /if \(payload\.selection\.excluded\) payload\.selection\.placement = /, '未排除时必须不传 placement');
  // 整数校验：非整数优先级不该发出去（服务端只接受整数）。
  assert.match(consoleJS, /Number\.isInteger\(priority\)/, '缺少优先级整数校验');
});

// ── 顶层作用域完整性（回归：选号代码块曾被意外嵌进 deleteAccount）──────────────
//
// 缺陷形态：deleteAccount() 少了一个右花括号，紧跟其后的 openSelection /
// syncSelectionForm / closeSelection / saveSelection 就被吞进了 deleteAccount 内部。
// 这种错位极其隐蔽：
//   · node --check 通过 —— 全文件花括号总数仍然平衡，语法完全合法；
//   · 纯文本断言全绿 —— 函数名确实还在文件里，任何正则都匹配得到；
//   · 但顶层不存在 syncSelectionForm，于是 console.js 的顶层代码
//     $("selectionExcluded").addEventListener("change", syncSelectionForm)
//     抛 ReferenceError，整份脚本当场终止 —— 末尾的 initializeConsole() 再不会执行。
// 用户看到的现象就是：点侧边栏 hash 变了但页面不切、账号行没有「选号」按钮、
// 模型页刷新按钮点了毫无反应。
//
// 所以必须**按作用域**校验，而不是数括号总数：顶格（第 0 列）写的
// function/const/let/class 声明，其所在位置的花括号深度必须是 0。
// 下面的扫描器正确处理模板串（含 ${} 内嵌套模板串）、正则字面量与注释 ——
// 这三处正是朴素括号计数会算错、从而漏报或误报的地方。
function scanTopLevel(source) {
  let i = 0, depth = 0, prev = "";
  const decls = [];
  // 这些关键字后面出现的 / 是正则字面量，不是除号。
  const REGEX_AFTER = new Set(["return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "case", "do", "else", "yield", "await"]);
  const skipLine = () => { while (i < source.length && source[i] !== "\n") i++; };
  const skipBlock = () => { i += 2; while (i < source.length && !(source[i] === "*" && source[i + 1] === "/")) i++; i += 2; };
  const skipString = (q) => { i++; while (i < source.length && source[i] !== q) { if (source[i] === "\\") i++; i++; } i++; };
  function skipTemplate() {
    i++;
    while (i < source.length) {
      const c = source[i];
      if (c === "\\") { i += 2; continue; }
      if (c === "`") { i++; return; }
      if (c === "$" && source[i + 1] === "{") {
        i += 2;
        let brace = 1;
        while (i < source.length && brace > 0) {
          const d = source[i];
          if (d === "\\") { i += 2; continue; }
          if (d === "/" && source[i + 1] === "/") { skipLine(); continue; }
          if (d === "/" && source[i + 1] === "*") { skipBlock(); continue; }
          if (d === '"' || d === "'") { skipString(d); continue; }
          if (d === "`") { skipTemplate(); continue; }
          if (d === "{") brace++;
          else if (d === "}") brace--;
          i++;
        }
        continue;
      }
      i++;
    }
  }
  while (i < source.length) {
    const c = source[i], n = source[i + 1];
    if (c === "\n" || c === " " || c === "\t" || c === "\r") { i++; continue; }
    if (c === "/" && n === "/") { skipLine(); continue; }
    if (c === "/" && n === "*") { skipBlock(); continue; }
    if (c === '"' || c === "'") { skipString(c); prev = "str"; continue; }
    if (c === "`") { skipTemplate(); prev = "str"; continue; }
    if (c === "/") {
      const isRegex = prev === "" || /[=(,:[!&|?{};+\-*%^~<>]/.test(prev) || REGEX_AFTER.has(prev);
      if (isRegex) {
        i++;
        let inClass = false;
        while (i < source.length && (inClass || source[i] !== "/")) {
          if (source[i] === "\\") i++;
          else if (source[i] === "[") inClass = true;
          else if (source[i] === "]") inClass = false;
          i++;
        }
        i++;
        while (i < source.length && /[a-z]/i.test(source[i])) i++;
        prev = "regex"; continue;
      }
      i++; prev = "/"; continue;
    }
    if (c === "{") { depth++; i++; prev = "{"; continue; }
    if (c === "}") { depth--; i++; prev = "}"; continue; }
    // 只认第 0 列的声明：缩进声明本来就在函数体内，不参与判断。
    if (i === 0 || source[i - 1] === "\n") {
      const m = /^(?:async\s+)?function\s+([A-Za-z_$][\w$]*)|^(?:const|let|var|class)\s+([A-Za-z_$][\w$]*)/.exec(source.slice(i, i + 100));
      if (m) decls.push({ name: m[1] || m[2], depth });
    }
    prev = /[A-Za-z0-9_$]/.test(c) ? "ident" : c;
    i++;
  }
  return { depth, decls };
}

test('控制台脚本的花括号必须整体平衡（模板串与正则不能把计数带偏）', () => {
  const dir = path.join(root, 'internal/server/console');
  for (const name of fs.readdirSync(dir).filter((f) => f.endsWith('.js')).sort()) {
    const { depth } = scanTopLevel(fs.readFileSync(path.join(dir, name), 'utf8'));
    assert.equal(depth, 0, `${name} 花括号不平衡（净深度 ${depth}）`);
  }
});

test('console.js 的顶层声明必须真的在顶层（回归：选号块曾被嵌进 deleteAccount）', () => {
  const { decls } = scanTopLevel(consoleJS);
  const nested = decls.filter((d) => d.depth !== 0);
  assert.deepEqual(
    nested.map((d) => `${d.name}@depth${d.depth}`), [],
    '这些顶格声明被包进了别的函数里：顶层拿不到它们，引用处会抛 ReferenceError 并终止整份脚本',
  );
  // 逐个点名：少一个都会让「选号」按钮或整页交互失效。
  const names = new Set(decls.map((d) => d.name));
  for (const fn of ['openSelection', 'syncSelectionForm', 'closeSelection', 'saveSelection', 'deleteAccount', 'initializeConsole']) {
    assert.ok(names.has(fn), `${fn} 不是顶层函数`);
  }
});
