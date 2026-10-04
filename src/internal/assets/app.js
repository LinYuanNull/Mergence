/* app.js 应用框架：slot 导航、集中状态、渠道管理、日志、设置。
 *
 * 桌面应用化的三条约定（参照 DSH 的做法）：
 *   1. 导航不用路由：侧栏 <a data-view> ↔ 主面板 #view-<id> 双向绑定，
 *      切换只改 hidden 属性，不写 history/hash，没有「页面」概念。
 *   2. 状态集中在一个 store（快照 + 订阅），视图只读快照渲染。
 *   3. 与原生相关的动作（唤出窗口、打开外部链接）走桌面桥，不当网页跳转。
 */

const $ = (id) => document.getElementById(id);

/* ── 集中状态（快照 + 订阅） ───────────────────────────── */

const Store = (() => {
  let snap = {
    view: 'overview',
    status: null,
    channels: [],
    presets: [],
    managedPresets: [],
    keyShown: false,
    upChan: '',
    fetched: [],
  };
  const subs = new Set();
  return {
    get: () => snap,
    set(patch) { snap = Object.assign({}, snap, patch); subs.forEach((fn) => fn(snap)); },
    sub(fn) { subs.add(fn); return () => subs.delete(fn); },
  };
})();

// S 是**活视图**而非快照：Store.set 会替换整个快照对象，若这里直接
// `const S = Store.get()` 捕获一次，后续所有 S.xxx 读的都是初始值
// （表现为侧栏计数永远是 0，而界面主体却是新的）。
const S = new Proxy({}, {
  get(_, key) { return Store.get()[key]; },
  set(_, key, val) { Store.set({ [key]: val }); return true; },
});

/* ── 工具 ─────────────────────────────────────────────── */

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/* paintStyles 把 data-style 上记的样式声明写进 CSSOM。
 *
 * 为什么不能直接在 markup 里写 style="..."：首页 CSP 是 style-src 'self'
 * （没有 'unsafe-inline'），**markup 中解析出来的 style 属性会被浏览器静默丢弃**
 * ——innerHTML 看着有 height:119px，实际渲染高度是 0，图表整张空白，而且不报任何错。
 * 通过 CSSOM（el.style.setProperty(...)）设置不受该限制。
 *
 * 用法：markup 写成 data-style="height:119.2px"，渲染完调 paintStyles(root)；
 * 调用必须在 innerHTML 赋值之后。
 */
function paintStyles(root) {
  (root || document).querySelectorAll('[data-style]').forEach((el) => {
    const decl = el.getAttribute('data-style');
    el.removeAttribute('data-style');   // 先摘掉：重复调用不会重复处理
    if (!decl) return;
    decl.split(';').forEach((kv) => {
      const i = kv.indexOf(':');
      if (i <= 0) return;
      el.style.setProperty(kv.slice(0, i).trim(), kv.slice(i + 1).trim());
    });
  });
}

function lines(text) {
  return String(text || '').split('\n').map((s) => s.trim()).filter(Boolean);
}

function intOf(v, def) {
  const n = parseInt(v, 10);
  return Number.isFinite(n) ? n : def;
}

// floatOf 解析价格输入。空串/非数字返回 null（表示「没填」），
// 而不是 0——0 会被当成「这个维度免费」，算出来的费用偏低。
function floatOf(v) {
  const s = String(v == null ? '' : v).trim();
  if (s === '') return null;
  const n = Number(s);
  return Number.isFinite(n) && n >= 0 ? n : null;
}

function when(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime())) return String(t);
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function toast(text, kind) {
  const el = $('toast');
  if (!el) return;
  el.textContent = text;
  el.className = 'toast show ' + (kind || '');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { el.className = 'toast'; }, kind === 'err' ? 6000 : 2600);
}

function parseModels(text) {
  const out = [];
  lines(text).forEach((line) => {
    const i = line.indexOf('=>');
    if (i < 0) { out.push({ id: line }); return; }
    const id = line.slice(0, i).trim();
    const alias = line.slice(i + 2).trim();
    if (id) out.push(alias ? { id, alias } : { id });
  });
  return out;
}

function modelsToText(list) {
  return (list || []).map((m) => (m.alias ? `${m.id} => ${m.alias}` : m.id)).join('\n');
}

function parseKV(text, sep) {
  const o = {};
  lines(text).forEach((line) => {
    const i = line.indexOf(sep);
    if (i > 0) {
      const k = line.slice(0, i).trim();
      if (k) o[k] = line.slice(i + 1).trim();
    }
  });
  return o;
}

function parseHeaders(text) { return parseKV(text, ':'); }
function headersToText(h) {
  return Object.entries(h || {}).map(([k, v]) => `${k}: ${v}`).join('\n');
}
function parseEnv(text) { return parseKV(text, '='); }
function envToText(e) {
  return Object.entries(e || {}).map(([k, v]) => `${k}=${v}`).join('\n');
}

function maskKey(k) {
  const s = String(k || '');
  if (!s) return '—';
  if (s.length <= 12) return '●●●●';
  return s.slice(0, 6) + '●●●●' + s.slice(-4);
}

async function api(path, opts) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), (opts && opts.timeout) || 10000);
  try {
    const r = await fetch(path, Object.assign({}, opts, { signal: ctl.signal }));
    const txt = await r.text();
    let d = {};
    try { d = txt ? JSON.parse(txt) : {}; } catch (e) { d = { raw: txt }; }
    if (!r.ok) {
      const err = new Error(d.error || ('HTTP ' + r.status));
      err.payload = d;
      throw err;
    }
    return d;
  } finally { clearTimeout(timer); }
}

async function post(path, body, timeoutMs) {
  return api(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body === undefined ? {} : body),
    timeout: timeoutMs || 30000,
  });
}

async function del(path) {
  return api(path, { method: 'DELETE' });
}

async function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.className = 'offscreen';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    ok ? resolve() : reject(new Error('复制失败'));
  });
}

/* 桌面桥：与原生窗口交互（DSH 用 preload/contextBridge，这里用 WebView2 桥） */
function desktopBridge() {
  return (window.chrome && window.chrome.webview) || null;
}
function desktopShowWindow() {
  // 优先用 Go 侧 Bind 注入的原生函数；没有（浏览器里打开面板）再退回 postMessage
  if (typeof window.mmShowWindow === 'function') { window.mmShowWindow(); return true; }
  const b = desktopBridge();
  if (b && b.postMessage) { b.postMessage({ type: 'show-window' }); return true; }
  return false;
}
function desktopOpenExternal(url) {
  if (!url) return false;
  if (typeof window.mmOpenExternal === 'function') { window.mmOpenExternal(url); return true; }
  const b = desktopBridge();
  if (b && b.postMessage) { b.postMessage({ type: 'open-external', url }); return true; }
  window.open(url, '_blank', 'noopener');
  return false;
}

/* ── 导航（slot 模式，无路由） ─────────────────────────── */

const VIEW_META = {
  'overview':    { t: '概览', sub: () => (S.status ? `已运行 ${S.status.uptime_sec}s` : '-') },
  'api':         { t: 'API 型平台', sub: () => `${S.channels.filter((c) => c.source !== 'managed').length} 个` },
  'acct':        { t: '积分型平台', sub: () => `${S.channels.filter((c) => c.source === 'managed').length} 个` },
  // 新建渠道的独立视图。副标题不再报数量：这里回答的是「怎么加」，
  // 与「已经有哪些」是两个问题，堆数量只会让人以为这是第三个列表页。
  'add':         { t: '添加平台', sub: () => '新建渠道 · 先选类型' },
  'up-accounts': { t: '账号池', sub: () => UP.chan },
  'up-tasks':    { t: '任务中心', sub: () => UP.chan },
  'up-models':   { t: '模型与档位', sub: () => UP.chan },
  'up-packages': { t: '积分构成', sub: () => UP.chan },
  'up-usage':    { t: '用量', sub: () => UP.chan },
  'up-config':   { t: '上游配置', sub: () => UP.chan },
  'up-logs':     { t: '上游运行日志', sub: () => UP.chan },
  'up-zc-accounts': { t: '账号池', sub: () => UP.chan },
  'up-zc-monitor':  { t: '运行监控', sub: () => UP.chan },
  'up-zc-settings': { t: '网关设置', sub: () => UP.chan },
  'logs':        { t: 'Mergence 运行日志', sub: () => '本进程结构化日志' },
  'settings':    { t: '设置', sub: () => '端口 / 密钥 / 窗口' },
};

/* ── 侧栏分组折叠 ───────────────────────────────────────
   状态存 localStorage：这是纯展示偏好，存进 Store 反而会在每次
   Store.set 时被无谓广播一遍。 */
const NAVSEC_KEY = 'mm_navsec';

function readNavSec() {
  try { return JSON.parse(localStorage.getItem(NAVSEC_KEY) || '{}'); } catch (e) { return {}; }
}

function writeNavSec(o) {
  try { localStorage.setItem(NAVSEC_KEY, JSON.stringify(o)); } catch (e) { /* 隐私模式忽略 */ }
}

/* 视图 → 所属分组。收起的分组里若藏着当前视图，用户会以为导航丢了。 */
function navSecOf(view) {
  if (view === 'api' || view === 'acct' || view === 'add') return 'chan';
  if (view.indexOf('up-') === 0) return 'up';
  // 「设置」是侧栏里的独立入口（不在任何 .nav-sec 内），返回 ''。
  // 把它塞回 'ops' 会让 revealNavSection 去展开运维分组——可它压根不在里面。
  if (view === 'logs') return 'ops';
  return '';
}

function revealNavSection(view) {
  const sec = navSecOf(view);
  document.querySelectorAll('.nav-sec').forEach((el) => {
    el.classList.toggle('has-active', el.dataset.sec === sec);
    if (el.dataset.sec === sec) el.classList.remove('closed');
  });
  if (sec) {
    const st = readNavSec();
    delete st[sec];
    writeNavSec(st);
  }
}

/* 不参与折叠的侧栏分组。**现在是空的**。
 *
 * 「控制台」曾在这里：当时的理由是「入口要随时可见」。但那是入口只有一份
 * 的时候——现在同样的入口在主面板控制台的页签栏里都有一份，侧栏这份只是
 * 快捷跳转，渠道一多反而把侧栏撑得半屏都是链接，所以改回可折叠。
 *
 * 常量保留而不是删掉：bindNavSections 仍需要「哪些分组要跳过折叠状态恢复」
 * 这个概念（旧版本往 localStorage 存过 up:1，升级后必须先清掉残留，
 * 否则分组会在启动时被旧状态收起来）。以后再加常驻分组，往这里塞名字即可。
 */
const NAVSEC_FIXED = [];

/* NAVSEC_PURGE 一次性的折叠状态清理。
 *
 * 「控制台」分组在上一版里是强制常驻的，那时期它每次启动都会把 localStorage
 * 里的 up 键删掉；但再往前的老版本往那里写过 up:1。直接从老版本升上来的用户，
 * 第一次启动会看到控制台分组是收起的——而「入口不见了」正是这次要修的问题，
 * 不能让它换个形式复现。所以清一次，之后用户的折叠选择照常保存。 */
const NAVSEC_PURGE = { key: 'mm_navsec_purge_v1', drop: ['up'] };

function bindNavSections() {
  const st = readNavSec();
  try {
    if (!localStorage.getItem(NAVSEC_PURGE.key)) {
      NAVSEC_PURGE.drop.forEach((k) => delete st[k]);
      localStorage.setItem(NAVSEC_PURGE.key, '1');
    }
  } catch (e) { /* 隐私模式忽略 */ }
  document.querySelectorAll('.nav-sec').forEach((el) => {
    if (NAVSEC_FIXED.indexOf(el.dataset.sec) >= 0) {
      el.classList.remove('closed');
      delete st[el.dataset.sec];
      return;
    }
    if (st[el.dataset.sec]) el.classList.add('closed');
  });
  writeNavSec(st);
  document.querySelectorAll('[data-sec-toggle]').forEach((b) => {
    b.onclick = () => {
      const el = b.closest('.nav-sec');
      const closed = el.classList.toggle('closed');
      const cur = readNavSec();
      if (closed) cur[b.dataset.secToggle] = 1;
      else delete cur[b.dataset.secToggle];
      writeNavSec(cur);
    };
  });
  // 首次进入时，当前视图所在分组不能是收起的
  revealNavSection(Store.get().view || 'overview');
}

function setView(name) {
  const meta = VIEW_META[name] ? name : 'overview';
  document.querySelectorAll('.views > .view').forEach((v) => {
    v.hidden = (v.id !== 'view-' + meta);
  });
  // 控制台工具条（平台选择器 + 页签栏）只在控制台视图下出现。
  // 它不随视图显隐地挂在 .views 里，而不是塞进某个视图内部：这样切页签时
  // 它原地不动，十个 up-* 视图用起来像一个控制台。
  $('conHead').hidden = meta.indexOf('up-') !== 0;
  $('ttl').textContent = VIEW_META[meta].t;
  $('subMeta').textContent = VIEW_META[meta].sub();
  Store.set({ view: meta });
  // 高亮与侧栏入口都要在「视图已确定」之后算：渠道卡片的选中态
  // 依赖 视图 + 渠道 两个维度。
  applyNavActive();
  renderUpConsoleNav();

  const primary = $('btnPrimary');
  primary.hidden = true; primary.onclick = null;
  // 渠道类的「添加」统一收进侧栏「添加平台」视图，这里不再挂按钮。
  // 挂上的话就等于又开了一个入口：按钮已经替你选好类型，表单第一项却还是
  // 「渠道类型」二选一，同一件事被问两遍。
  if (meta === 'up-accounts') { primary.hidden = false; primary.textContent = '添加账号'; primary.onclick = openAddAccount; }

  revealNavSection(meta);
  // 渠道表单是单例，跟着「渠道」分组走：
  //   add      —— 进这个视图就是来新建的，直接把表单挂进来；
  //   api/acct —— 只有点某个渠道的「编辑」才挂进右侧编辑槽（在这里无条件
  //               挂会弹出一张空表单，那不是这两个视图该有的默认样子）；
  //   离开分组  —— 收回表单。同一分组内 api/acct/add 互切时表单不动，
  //               填了一半的草稿不会因为切过去看一眼就没了。
  if (meta === 'add') openAddView();
  else if (meta === 'api' || meta === 'acct') loadChannels();
  else unmountChannelForm();
  if (meta === 'overview') loadMetrics(true);
  if (meta === 'logs') startLogPoll();
  if (meta === 'settings') loadSettings();
  if (meta.indexOf('up-') === 0) {
    onUpView(meta);
    // zcode2api 的原生控制台视图在 zcode.js；upstream.js 的 onUpView 是
    // 表驱动（UP_LOADERS），给它加分支就得改 upstream.js。这里在它之后再补
    // 一次分派，保持 upstream.js 不动；zcode.js 未加载时安全跳过。
    if (typeof onZcodeView === 'function') onZcodeView(meta);
  }
}

/* ── 上游控制台入口 ─────────────────────────────────────
 *
 * 入口表只在这里定义一次，**侧栏卡片与主面板的页签栏共用同一份**。
 * 顺序按使用频率排：账号池/任务/模型是日常，配置与日志属于排查时才看。
 */
const UP_VIEWS = [
  ['up-accounts', '账号池', '◍'],
  ['up-tasks', '任务中心', '☰'],
  ['up-models', '模型与档位', '⬡'],
  ['up-packages', '积分构成', '▤'],
  ['up-usage', '用量', '◫'],
  ['up-config', '配置', '⚙'],
  ['up-logs', '运行日志', '≡'],
];

/* zcode2api 的原生视图（控制台形态 zcode）。
 *
 * 与 UP_VIEWS 是**两套不能混用**的表：它的管理 API 在 /admin/api，接口结构与
 * 集成面板不兼容，能列的只有这三个专属视图。给不匹配的那类列出整套视图，
 * 点进去只会 404 —— 入口看着有、实际不能用，比没有入口更糟。
 *
 * 放在这里而不是 zcode.js：侧栏卡片与主面板页签栏都要用它，两处口径必须一致。 */
const ZCODE_VIEWS = [
  ['up-zc-accounts', '账号池', '◍'],
  ['up-zc-monitor', '运行监控', '▤'],
  ['up-zc-settings', '网关设置', '⚙'],
];

/* upViewDefs 一个渠道的控制台入口表；null = 没有可嵌入的控制台。
 *
 * 「有哪些入口」完全由后端探测出的 console_kind 决定，前端不猜：
 *   gateway -> 集成面板那 7 个视图（接口签名一致，直接复用）
 *   zcode   -> 上面 3 个原生视图（专属适配）
 *   web     -> 接不进来，只给一个外链；none -> 连外链都没有
 *   unknown -> 子进程刚起、还没探出结果，调用方要「不下结论」而不是当成没有
 * 判错的代价只是列错入口，不碰转发——但 404 的入口很伤，所以宁可慢一步。 */
function upViewDefs(c) {
  if (!c) return null;
  if (c.console_kind === 'gateway') return UP_VIEWS;
  if (c.console_kind === 'zcode') return ZCODE_VIEWS;
  return null;
}

/* conViewFor 在主面板换平台时该落到哪个页签。
 *
 * 优先保留当前视图——用户是在「同一个控制台里换渠道对比数据」，把他扔回
 * 第一个页签等于每换一次渠道都要重新点一遍。
 *
 * 但两个渠道的控制台形态可能不同（gateway 有 7 个视图、zcode 只有 3 个），
 * 当前视图在新渠道上**不存在**时必须落到它的第一项：不然会切到一个没有
 * 数据的页签，而内容区还留着上一个渠道的数字，比报错更容易让人看错。
 * 形态还没探出来（defs 为 null）时不动视图——那时也说不清该落到哪。 */
function conViewFor(name, view) {
  const c = (Store.get().channels || []).find((x) => x.name === name);
  const defs = upViewDefs(c);
  if (!defs) return view;
  return defs.some(([v]) => v === view) ? view : defs[0][0];
}

const UPCHAN_KEY = 'mm_upchan';
const UPCARD_KEY = 'mm_upcards';

function readUpChan() {
  try { return localStorage.getItem(UPCHAN_KEY) || ''; } catch (e) { return ''; }
}

function rememberUpChan(name) {
  try {
    if (name) localStorage.setItem(UPCHAN_KEY, name);
    else localStorage.removeItem(UPCHAN_KEY);
  } catch (e) { /* 隐私模式忽略 */ }
}

function readUpCards() {
  try { return JSON.parse(localStorage.getItem(UPCARD_KEY) || '{}'); } catch (e) { return {}; }
}

function writeUpCards(o) {
  try { localStorage.setItem(UPCARD_KEY, JSON.stringify(o)); } catch (e) { /* 忽略 */ }
}

/* renderUpConsoleNav 把托管渠道渲染成侧栏的子卡片。
 *
 * 用签名比对跳过无变化的渲染：渠道列表每 3 秒轮询一次，
 * 每次都重建 DOM 会把卡片的折叠状态弄丢，也会让正在停留的链接闪一下。
 */
function renderUpConsoleNav() {
  const box = $('upChanList');
  const group = $('upGroup');
  if (!box || !group) return;

  const chans = (Store.get().channels || []).filter((c) => c.source === 'managed');

  // 这个分组**不随视图显隐**：控制台入口随时要用，让它们「先点一下某个
  // 视图才出现」等于把入口藏起来（旧版就是这样）。
  //
  // 但它**可以被收起**：渠道一多，每张卡片平铺 7 个入口会把侧栏撑满，
  // 而这些入口在主面板控制台的页签栏里都有一份。折叠状态由 bindNavSections
  // 统一管；这里只保证它不被 hidden，绝不碰 closed —— 之前那句
  // `group.classList.remove('closed')` 会让折叠彻底失效。
  group.hidden = false;

  // 选中的渠道被删掉后不能继续挂着：否则侧栏高亮不到任何一条，
  // 而上游请求还会按旧渠道名发出去。
  if (UP.chan && !chans.some((c) => c.name === UP.chan)) {
    UP.chan = '';
    Store.set({ upChan: '' });
    rememberUpChan('');
  }

  // 主面板的两处（控制台工具条、积分型平台页的入口区块）与侧栏同源，
  // 必须每次都刷：它们各自有签名守卫，不会因为渠道列表每 3 秒轮询一次
  // 就重建 DOM。放在下面那个提前返回**之前**，否则侧栏签名没变时
  // 主面板这两处就永远不刷新了。
  renderConHead(chans);
  renderAcctConsole(chans);

  // 没渠道时也占位并说明原因：「这里本该有什么」比一块空白好懂。
  // 还没拿到状态时不下结论，否则会先闪一句「还没有积分型平台」。
  const loaded = !!Store.get().status;
  const sig = chans.length
    ? chans.map((c) => [c.name, c.display_name, c.ready ? 1 : 0, c.enabled ? 1 : 0].join('~')).join('|')
    : ('empty:' + (loaded ? 1 : 0));
  if (box.dataset.sig === sig) { applyNavActive(); return; }
  box.dataset.sig = sig;

  if (!chans.length) {
    box.innerHTML = '<div class="nav-note">' + (loaded
      ? '还没有积分型平台。创建后它的控制台入口会出现在这里。'
      : '正在读取…') + '</div>';
    applyNavActive();
    return;
  }

  const closed = readUpCards();
  box.innerHTML = chans.map((c) => {
    const st = !c.enabled ? 'off' : (c.ready ? 'ok' : 'warn');
    const items = upConsoleItems(c);
    return `<div class="upcard${closed[c.name] ? ' closed' : ''}" data-chan-card="${esc(c.name)}">
      <button class="upcard-hd" data-chan-toggle="${esc(c.name)}"
              title="${esc(c.display_name || c.name)}${c.ready ? '' : '（未就绪）'}">
        <span class="dot ${st}"></span>
        <span class="upcard-t">${esc(c.display_name || c.name)}</span>
        <i class="caret">▾</i>
      </button>
      <div class="upcard-bd">${items}</div>
    </div>`;
  }).join('');

  bindUpCards();
  applyNavActive();
}

/* upConsoleItems 一个渠道的控制台入口。
 *
 * 形态由后端探测（console_kind）：各 2api 的管理 API 结构差别很大，
 * 给不支持的那类列一整套视图，点进去只会是 404——入口看着有、实际不能用，
 * 比没有入口更糟。
 */
function upConsoleItems(c) {
  const defs = upViewDefs(c);
  if (defs) {
    // 密码由 Mergence 服务端注入（zcode 那套也是），浏览器不接触密码，
    // 所以查看无需输入面板密码。
    return defs.map(([v, label, icon]) =>
      `<a class="nav-i sub" data-view="${v}" data-chan="${esc(c.name)}">
        <i>${icon}</i><span>${esc(label)}</span></a>`).join('');
  }
  if (c.console_kind === 'web') {
    return c.panel_url
      ? `<a class="nav-i sub" data-panel-url="${esc(c.panel_url)}">
          <i>↗</i><span>打开管理面板</span></a>`
      : '<div class="nav-note">该渠道没有可嵌入的控制台</div>';
  }
  if (c.console_kind === 'none') {
    return '<div class="nav-note">该渠道没有可接入的控制台</div>';
  }
  // 还没探出结果（子进程刚起）：不下结论，免得入口闪一下就消失
  return '<div class="nav-note">正在探测控制台…</div>';
}

/* renderConHead 刷新主面板控制台的工具条：平台选择器 + 页签栏 + 添加账号。
 *
 * 这是「把控制台合并成一个视图」的落点：无论切到哪个 up-* 视图，这一条
 * 都在原处，页签只换高亮，所以十个视图用起来像一个控制台。
 *
 * 页签表与侧栏卡片取自同一个 upViewDefs，两处不可能各说各话；真的不一致时
 * 用户会以为「有 4 个页签没显示出来」，比少列几个入口更难排查。
 *
 * 两个签名守卫是必需的：渠道列表每 3 秒轮询一次，无脑重建 innerHTML 会把
 * 下拉框的展开态与页签的 hover 打断。
 */
function renderConHead(chans) {
  const sel = $('conChan');
  const tabs = $('conTabs');
  const add = $('btnConAdd');
  if (!sel || !tabs || !add) return;

  // 当前平台：UP.chan 优先，它为空（刚启动、还没选过）时落到列表第一项。
  // 只用于**显示**，不在这里改 UP.chan —— 渲染函数改全局状态的话，
  // 「渠道列表一到就自动选中」会连带触发上游请求，副作用太隐蔽。
  const cur = chans.find((c) => c.name === UP.chan) || chans[0] || null;

  const selSig = chans.map((c) => [c.name, c.display_name, c.account_count].join('~')).join('|');
  if (sel.dataset.sig !== selSig) {
    sel.dataset.sig = selSig;
    sel.innerHTML = chans.map((c) => {
      const n = c.account_count;
      const tag = n > 0 ? n + ' 个账号' : (n === 0 ? '尚未添加账号' : '账号数未知');
      return `<option value="${esc(c.name)}">${esc(c.display_name || c.name)}（${tag}）</option>`;
    }).join('');
  }
  if (cur) sel.value = cur.name;

  const view = Store.get().view || '';
  const defs = upViewDefs(cur);
  const tabSig = [cur ? cur.name : '', view, cur ? cur.console_kind : '', cur ? (cur.panel_url || '') : ''].join('~');
  if (tabs.dataset.sig !== tabSig) {
    tabs.dataset.sig = tabSig;
    if (!cur) {
      tabs.innerHTML = '<span class="con-none">还没有积分型平台，'
        + '到「添加平台」里创建后控制台入口会出现在这里</span>';
    } else if (defs) {
      tabs.innerHTML = defs.map(([v, label, icon]) =>
        `<button type="button" data-con-view="${v}" class="${v === view ? 'on' : ''}">`
        + `<i>${icon}</i><span>${esc(label)}</span></button>`).join('');
    } else if (cur.console_kind === 'web') {
      tabs.innerHTML = cur.panel_url
        ? `<button type="button" data-con-ext="${esc(cur.panel_url)}">`
          + '<i>↗</i><span>打开管理面板</span></button>'
        : '<span class="con-none">该渠道没有可嵌入的控制台</span>';
    } else if (cur.console_kind === 'none') {
      tabs.innerHTML = '<span class="con-none">该渠道没有可接入的控制台</span>';
    } else {
      tabs.innerHTML = '<span class="con-none">正在探测控制台…</span>';
    }
  }

  // 「添加账号」只对 gateway 形态有意义：zcode 的账号在它自己的账号池页里加
  // （设备码登录 / 导入，接口完全不同），web / none 更没有账号池可加。
  // 给它们挂一个点了会报错的按钮，比不挂更糟。
  add.hidden = !(cur && cur.console_kind === 'gateway');
  add.title = cur ? `向「${cur.display_name || cur.name}」的账号池添加账号` : '';
}

/* renderAcctConsole 「积分型平台」页顶部的控制台入口区块。
 *
 * 它存在的唯一理由是**入口可见**：原先点开一个渠道后，这一页上没有任何
 * 通向账号池的路，用户只能去侧栏找——侧栏那一栏还可能被收起，
 * 「添加积分型平台账号的入口找不到」就是这么来的。
 *
 * 显隐由设置里的「显示积分型平台控制台入口」(config.UI.AcctConsole) 决定，
 * 缺省开。关掉只收这一块，**侧栏分组不受影响**：入口必须至少留一条可达路径，
 * 否则「隐藏」就变成「功能没了」。
 */
function renderAcctConsole(chans) {
  const box = $('acctConsole');
  const list = $('acctConsoleList');
  if (!box || !list) return;

  const want = !(S.status && S.status.acct_console === false) && chans.length > 0;
  box.hidden = !want;
  if (!want) return;

  const view = Store.get().view || '';
  const sig = chans.map((c) => [c.name, c.display_name, c.ready ? 1 : 0, c.enabled ? 1 : 0,
    c.account_count, c.console_kind, c.panel_url || ''].join('~')).join('|') + '#' + view;
  if (list.dataset.sig === sig) return;
  list.dataset.sig = sig;

  list.innerHTML = chans.map((c) => {
    const st = !c.enabled ? '' : (c.ready ? 'ok' : 'warn');
    const n = c.account_count;
    const tag = n > 0 ? n + ' 个账号' : (n === 0 ? '尚未添加账号' : '账号数未知');
    const defs = upViewDefs(c);
    // 入口按钮指向什么，与侧栏卡片同一套判断；没有控制台的渠道给一句实话
    // 而不是一个点不动的按钮。
    if (defs) {
      const opened = defs.some(([v]) => v === view);
      return `<div class="ce-row">
        <span class="ce-dot ${st}"></span>
        <span class="nm">${esc(c.display_name || c.name)}</span>
        <span class="dim">${esc(tag)}</span>
        <span class="grow"></span>
        <button class="ghost" data-con-open="${esc(c.name)}">${
          opened ? '控制台已打开' : '打开控制台'}</button>
      </div>`;
    }
    if (c.console_kind === 'web' && c.panel_url) {
      return `<div class="ce-row">
        <span class="ce-dot ${st}"></span>
        <span class="nm">${esc(c.display_name || c.name)}</span>
        <span class="dim">${esc(tag)} · 自带网页面板</span>
        <span class="grow"></span>
        <button class="ghost" data-con-ext="${esc(c.panel_url)}">打开管理面板</button>
      </div>`;
    }
    const why = c.console_kind === 'none' ? '该渠道没有可接入的控制台' : '正在探测控制台…';
    return `<div class="ce-row">
      <span class="ce-dot ${st}"></span>
      <span class="nm">${esc(c.display_name || c.name)}</span>
      <span class="dim">${esc(tag)} · ${esc(why)}</span>
    </div>`;
  }).join('');
}

function bindUpCards() {
  document.querySelectorAll('[data-chan-toggle]').forEach((b) => {
    b.onclick = () => {
      const card = b.closest('.upcard');
      const closed = card.classList.toggle('closed');
      const cur = readUpCards();
      if (closed) cur[b.dataset.chanToggle] = 1;
      else delete cur[b.dataset.chanToggle];
      writeUpCards(cur);
    };
  });
}

/* applyNavActive 统一算导航高亮。
 *
 * 上游控制台现在每个渠道都有一份入口指向同一个视图，
 * 只按 data-view 匹配会一次点亮好几条——必须同时比渠道。
 */
function applyNavActive() {
  const view = Store.get().view || 'overview';
  document.querySelectorAll('#nav .nav-i').forEach((a) => {
    const ch = a.dataset.chan;
    a.classList.toggle('on', a.dataset.view === view && (!ch || ch === UP.chan));
  });
}

/* openUpView 一步切到「某渠道的某视图」——这是本次改动的全部意义。 */
function openUpView(chan, view) {
  if (UP.chan !== chan) {
    UP.chan = chan;
    Store.set({ upChan: chan });
    rememberUpChan(chan);
  }
  setView(view);
}

/* selectUpstream 保留给「只知道渠道、不知道视图」的调用点。 */
function selectUpstream(name) { openUpView(name, 'up-accounts'); }

/* ── 状态渲染 ─────────────────────────────────────────── */

function renderStatus(st) {
  if (!st) return;
  Store.set({ status: st });
  $('navSub').textContent = `已运行 ${st.uptime_sec}s`;
  $('navState').textContent = (st.channels_ready || 0) > 0 ? '服务就绪' : '无可用渠道';
  $('navStat').textContent = `${st.channels_ready || 0}/${st.channels_total || 0} 渠道 · ${st.models_total || 0} 模型`;
  $('navPulse').className = 'pulse ' + ((st.channels_ready || 0) > 0 ? 'ok' : 'warn');

  // 概览的顶部计数：只放「网关自身状态」，
  // 用量数字在上面的「用量指标」区（那里才有 token 与费用口径）。
  $('ovCards').innerHTML = [
    kcard('就绪渠道', st.channels_ready, 'ok'),
    kcard('可用模型', st.models_total),
    kcard('渠道总数', st.channels_total),
    kcard('托管进程', st.running),
  ].join('');

  $('epURL').textContent = (st.base_url || '') + '/v1';
  $('epKey').textContent = maskKey(st.access_key || '');
  $('epModels').textContent = String(st.models_total || 0);
  $('epHint').textContent = st.api_ready
    ? '把地址与 Key 填进任意 OpenAI 兼容客户端'
    : '还没有可用模型：先在「积分型平台」或「API 型平台」里添加并启用渠道';

  const chs = st.channels || [];
  // 顺手把渠道列表喂给侧栏的「上游控制台」入口。这一步是**必需**的：
  // 侧栏原先只由 loadChannels() 驱动，而它只在 api/acct 视图被调用，
  // 于是入口要等用户点一下「积分型平台」才出现。状态轮询本来就带回了
  // 渠道列表（下面的「渠道概况」就用它渲染），在这里同步一次，
  // 侧栏就与真实渠道状态始终一致 —— 顺带还能反映就绪状态的变化。
  Store.set({ channels: chs });
  renderUpConsoleNav();
  $('chHint').textContent = `${st.channels_ready || 0} / ${chs.length} 就绪`;
  $('chCards').innerHTML = chs.length ? chs.map((c) => `
    <div class="prov" data-goto="${c.source === 'managed' ? 'acct' : 'api'}">
      <div class="ph">
        <span class="dot ${!c.enabled ? 'off' : (c.ready ? 'on' : 'warn')}"></span>
        <span class="nm">${esc(c.display_name)}</span>
        <span class="badge">${c.source === 'managed' ? '积分型' : 'API 型'}</span>
      </div>
      <div class="kv">${esc(c.model_prefix)} · ${c.model_count} 模型</div>
      <div class="kv">${c.enabled ? (c.ready ? '就绪' : '未就绪') : '已停用'}</div>
    </div>`).join('') : '<div class="prov"><div class="kv">还没有渠道</div></div>';
}

function kcard(label, v, cls) {
  return `<div class="card-k"><b class="${cls || ''}">${v == null ? 0 : v}</b><span>${esc(label)}</span></div>`;
}

/* ── 渠道列表（两大区块） ─────────────────────────────── */

/* renderAcctHiddenNote 说明「有平台但都还没加账号，所以没显示」。
 *
 * 为什么要单独说一声：把零账号平台隐藏是用户要求，但「列表空着」与
 * 「一个平台都没有」在界面上长得一模一样——用户会以为平台被删了。
 * 说清楚它只是被隐藏、且告诉他下一步点哪里，隐藏才不构成困惑。
 *
 * account_count === -1（上游还问不到）的平台不算被隐藏：那是「还没探测出来」，
 * 平台照常显示，所以这里的数字只数真正被藏起来的那些。
 */
function renderAcctHiddenNote(n) {
  const box = $('acctHiddenNote');
  if (!box) return;
  box.hidden = n <= 0;
  if (n > 0) {
    box.textContent = `有 ${n} 个平台尚未添加账号，已暂时隐藏。添加账号后会自动显示。`;
  }
}
function chCardHTML(c) {
  const managed = c.source === 'managed';
  const state = !c.enabled ? '<span class="badge muted">已停用</span>'
    : (c.ready ? '' : '<span class="badge warn">未就绪</span>');
  const stats = (c.req_ok || c.req_err)
    ? `<span class="badge ${c.req_err ? 'warn' : 'ok'}">成功 ${c.req_ok} · 失败 ${c.req_err}${
        c.last_ms ? ' · ' + c.last_ms + 'ms' : ''}</span>` : '';

  return `<div class="chcard ${c.enabled ? '' : 'off'}" data-name="${esc(c.name)}">
    <div class="ch-hd">
      <span class="dot ${!c.enabled ? 'off' : (c.ready ? 'on' : 'warn')}"></span>
      <span class="nm">${esc(c.display_name)}</span>
      <span class="badge muted">${esc(c.protocol)}</span>
      <span class="badge accent">${esc(c.model_prefix)}</span>
      ${state}
      <div class="ch-act">
        <label class="switch" title="启用 / 停用（积分型平台会同时启停子进程）">
          <input type="checkbox" data-act="toggle" ${c.enabled ? 'checked' : ''}><span></span>
        </label>
        ${managed ? '<button class="ghost" data-act="restart">重启</button>' : ''}
        <button class="ghost" data-act="test">测试</button>
        <button class="ghost" data-act="edit">编辑</button>
        <button class="ghost danger" data-act="del">删除</button>
      </div>
    </div>
    <div class="ch-meta"><span class="kv">${esc(c.base_url || '—')}</span></div>
    <div class="ch-meta">
      <span class="badge muted">${c.model_count} 个模型</span>
      ${c.key_count ? `<span class="badge muted">${c.key_count} 个 Key</span>` : ''}
      <span class="badge muted">优先级 ${c.priority}</span>
      <span class="badge muted">权重 ${c.weight}</span>
      ${stats}
    </div>
    ${c.ready_reason ? `<div class="err">未就绪：${esc(c.ready_reason)}</div>` : ''}
    ${c.last_err ? `<div class="err">最近错误：${esc(c.last_err)}</div>` : ''}
  </div>`;
}

async function loadChannels() {
  try {
    const d = await api('/api/channels');
    const list = d.channels || [];
    Store.set({ channels: list });
    renderUpConsoleNav();
    // 局部变量禁叫 api：const 的 TDZ 会让整个函数作用域的 api() 失效
    const apiChs = list.filter((c) => c.source !== 'managed');
    // 托管型按账号数过滤：
    //   account_count === 0  -> 没账号，平台整体隐藏（用户要求）
    //   account_count < 0   -> 上游还问不到（子进程没起/没配密码），
    //                         此时**留空**而不是当成 0 把平台藏掉——
    //                         「还没探测出来」与「确实没有」是两回事。
    const acctAll = list.filter((c) => c.source === 'managed');
    const acctChs = acctAll.filter((c) => c.account_count !== 0);
    const acctHidden = acctAll.length - acctChs.length;
    $('apiList').innerHTML = apiChs.length ? apiChs.map(chCardHTML).join('')
      : '<div class="empty">还没有 API 型平台。左侧「添加平台」里选「API 型平台」即可开始。</div>';
    $('acctList').innerHTML = acctChs.length ? acctChs.map(chCardHTML).join('')
      : (acctHidden > 0
        ? '<div class="empty">还没有已添加账号的平台。到上面「控制台」的账号池里添加账号。</div>'
        : '<div class="empty">还没有积分型平台。左侧「添加平台」里选「积分型平台」即可开始。</div>');
    renderAcctHiddenNote(acctHidden);
  } catch (e) {
    toast('渠道加载失败：' + e.message, 'err');
  }
}

async function channelAction(name, act, card) {
  if (act === 'console') { openUpView(name, 'up-accounts'); return; }
  if (act === 'edit') {
    try {
      const raw = await api('/api/channels/raw?name=' + encodeURIComponent(name));
      openChannelForm(raw);
    } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
    return;
  }
  if (act === 'test') {
    $('trTitle').textContent = '连接测试 · ' + name;
    $('trBody').innerHTML = '<div class="tstep"><span class="ic">…</span><span class="tx">测试中…</span></div>';
    $('trModal').hidden = false;
    try {
      const tr = await post('/api/channels/test', { name }, 120000);
      $('trBody').innerHTML = renderTest(tr);
    } catch (e) {
      $('trBody').innerHTML = `<div class="tstep bad"><span class="ic">✕</span>
        <span class="tx"><span class="tn">测试失败</span><div class="td">${esc(e.message)}</div></span></div>`;
    }
    return;
  }
  if (act === 'restart') {
    try { await post('/api/channels/action', { name, action: 'restart' }, 90000); }
    catch (e) { toast('重启失败：' + e.message, 'err'); }
    await loadChannels();
    return;
  }
  if (act === 'toggle') {
    const input = card.querySelector('input[data-act="toggle"]');
    try {
      const d = await post('/api/channels/toggle', { name, enabled: input.checked }, 90000);
      if (d.start_error) toast('子进程操作失败：' + d.start_error, 'err');
    } catch (e) {
      input.checked = !input.checked;
      toast(e.message, 'err');
    }
    await loadChannels();
    return;
  }
  if (act === 'del') {
    if (!confirm(`删除渠道「${name}」？\n\nAPI 型平台会一并删除已保存的 Key；积分型平台会先停掉子进程。`)) return;
    try { await del('/api/channels?name=' + encodeURIComponent(name)); }
    catch (e) { toast('删除失败：' + e.message, 'err'); }
    if (UP.chan === name) {
      UP.chan = ''; Store.set({ upChan: '' }); rememberUpChan('');
    }
    await loadChannels();
  }
}

function renderTest(tr) {
  const steps = (tr.steps || []).map((s) => {
    const cls = s.ok ? 'ok' : (s.blocking ? 'bad' : 'warn');
    const ic = s.ok ? '✓' : (s.blocking ? '✕' : '!');
    return `<div class="tstep ${cls}"><span class="ic">${ic}</span>
      <span class="tx"><span class="tn">${esc(s.name)}</span>
      ${s.detail ? `<div class="td">${esc(s.detail)}</div>` : ''}</span>
      ${s.latency_ms ? `<span class="tl">${s.latency_ms}ms</span>` : ''}</div>`;
  }).join('');
  return steps + (tr.ok
    ? `<div class="tstep ok"><span class="ic">✓</span><span class="tx">测试通过${
        tr.models ? `，识别到 ${tr.models.length} 个模型` : ''}</span></div>`
    : '<div class="tstep bad"><span class="ic">✕</span><span class="tx">测试未通过</span></div>');
}

/* ── 渠道表单（三个宿主视图共用的单例） ──────────────────
 *
 * 表单 DOM 是单例（#chModal），三个视图各有一个挂载槽：
 *   API 型 / 积分型 —— 列表右侧的内联编辑槽（点渠道的「编辑」才挂）；
 *   添加平台        —— 整页就是这张表单（进视图即挂）。
 * 为什么不是「每个视图各复制一份」：字段有几十个，多份必然出现
 * 「这份改了那份还留着旧值」的错误，而这种错要到保存时才发现。
 */
function chEditorSlot() {
  const view = Store.get().view;
  if (view === 'add') return $('addEditor');
  return document.querySelector(`#view-${view === 'api' ? 'api' : 'acct'} .ch-editor`);
}

/* openAddView 进入「添加平台」时保证表单在位。
 * 已经挂在这个槽里、且处于「新建」态，就只当作重新可见——保留用户填了一半的
 * 草稿：切走看一眼别处再回来（或点「刷新」）就把表单清空，等于让人重填一遍。
 * 反之（上一次是编辑某个渠道，或表单停在别的槽里）就换成一张干净的新表单。 */
function openAddView() {
  const modal = $('chModal');
  if (modal && modal.parentElement === $('addEditor') && !editingName) {
    mountChannelForm();
    return;
  }
  openChannelForm(null);
}

function mountChannelForm() {
  const slot = chEditorSlot();
  const modal = $('chModal');
  if (!slot || !modal) return;
  const view = Store.get().view;
  // 先把别的视图留下的「编辑中」两栏态撤掉。不清的话，从 api 的编辑态直接
  // 切到「添加平台」时表单被搬走了，可 #apiSplit 还带着 .editing——回头再看
  // 那个视图，左边列表被挤窄、右边空空如也。
  document.querySelectorAll('.chsplit.editing').forEach((el) => {
    if (el.id !== view + 'Split') el.classList.remove('editing');
  });
  if (modal.parentElement !== slot) slot.appendChild(modal);
  modal.hidden = false;
  modal.classList.add('inline');
  // 两栏布局只有 api/acct 有；「添加平台」没有列表，不需要。
  if (view === 'api' || view === 'acct') {
    const split = $(view + 'Split');
    if (split) split.classList.add('editing');
  }
  syncFormChrome();
}

function unmountChannelForm() {
  const modal = $('chModal');
  if (modal) {
    modal.hidden = true;
    // 关闭要做三件事，缺一件都会留下可见残留：
    //  1) hidden=true        —— 逻辑上标记为关闭；
    //  2) 移除 .inline        —— 这个类把全屏遮罩改回文档流布局（display:block）。
    //                         它是类选择器，特异性压得过 [hidden] 的 display:none，
    //                         不摘掉的话面板仍会显示在渠道列表正下方；
    //  3) 送回 body 原址      —— 表单单例原本挂在 body 下。留在编辑槽里会让
    //                         `.ch-editor:empty { display:none }` 匹配不上，槽位
    //                         不再塌陷，列表下方白留一段行间距。
    modal.classList.remove('inline');
    if (modal.parentElement && modal.parentElement !== document.body) {
      document.body.appendChild(modal);
    }
  }
  const close = $('btnCloseCh');
  if (close) close.hidden = false;
  document.querySelectorAll('.chsplit.editing').forEach((el) => el.classList.remove('editing'));
}

/* syncFormChrome 按「表单挂在哪儿」调整外框。
 * 「添加平台」是独立视图：标题跟着视图叫「添加平台」，也没有「关闭」——
 * 关掉只会把当前这个空视图留在原地，不像弹层那样有关掉的对象。
 * 编辑态要带上是哪个渠道：分栏编辑时列表还在旁边，标题不说清就容易改错行。 */
function syncFormChrome() {
  const modal = $('chModal');
  const inAdd = !!(modal && modal.parentElement && modal.parentElement.id === 'addEditor');
  const close = $('btnCloseCh');
  if (close) close.hidden = inAdd;
  $('chFormTitle').textContent = editingName
    ? '编辑渠道 · ' + (editingLabel || editingName)
    : (inAdd ? '添加平台' : '添加渠道');
}

let editingName = '';
let editingLabel = '';
let formKind = 'embedded';

function setKind(kind) {
  formKind = kind === 'managed' ? 'managed' : 'embedded';
  document.querySelectorAll('#chKind .seg-b').forEach((b) => {
    b.classList.toggle('on', b.dataset.kind === formKind);
  });
  document.querySelectorAll('#chModal .konly').forEach((box) => {
    box.hidden = box.dataset.kind !== formKind;
  });
  $('kindHint').textContent = formKind === 'managed'
    ? '积分型平台：上游跑在 Mergence 进程内，账号在「控制台」里管理。'
    : 'API 型平台：直接向 OpenAI 兼容端点发请求，不额外起进程。';
  fillPresets();
}

function currentPresets() {
  return formKind === 'managed' ? S.managedPresets : S.presets;
}

function fillPresets() {
  // 托管型渠道的 preset 允许缺省（配置里没记、后端也推断不出来）。
  // 此时绝不能回落到第一个模板：那会让用户误以为渠道是用别的平台建的，
  // 保存时还会把这个错值写进配置。宁可不显示，也不要显示错的。
  // 内嵌型的 preset 一直是持久化的，不需要占位项。
  const placeholder = formKind === 'managed'
    ? '<option value="">未记录模板（自定义配置）</option>' : '';
  $('fPreset').innerHTML = placeholder + currentPresets()
    .map((p) => `<option value="${esc(p.id)}">${esc(p.label)}</option>`).join('');
}

function applyPreset(id) {
  const p = currentPresets().find((x) => x.id === id);
  if (!p) return;
  if (formKind === 'managed') {
    // 网关种类由模板带出来：后端据此知道装配哪个内置实现，它同时也是控制台
    // 形态与管理 API 前缀的契约依据（见后端 kindOfUpstream / panelAPIPrefixFor）。
    gatewayKind = p.kind || '';
    $('fHealthM').value = p.health_path || '';
    $('fPrefix').value = p.model_prefix || '';
    if (!$('fName').value.trim()) $('fName').value = p.label || '';
    $('keyHint2').textContent = p.route_key_hint ? 'Key 来源：' + p.route_key_hint : '';
    // 积分平台预填官方折算价（如 WorkBuddy 0.05 元/积分），用户可改
    $('fCreditValue').value = p.credit_value > 0 ? p.credit_value : '';
    $('presetHint').textContent = [p.note, p.doc_url ? '项目：' + p.doc_url : ''].filter(Boolean).join(' · ');
  } else {
    $('fBase').value = p.base_url || '';
    $('fProtocol').value = p.protocol || 'chat';
    $('fPrefix').value = p.model_prefix || '';
    if (!$('fName').value.trim()) $('fName').value = p.label || '';
    $('keyHint').textContent = p.needs_key ? '该上游通常需要 API Key' : '该上游通常不需要 Key';
    $('presetHint').textContent = [p.key_hint ? 'Key 形态：' + p.key_hint : '', p.note].filter(Boolean).join(' · ');
  }
}

function openChannelForm(raw, forceKind) {
  editingName = raw ? raw.name : '';
  editingLabel = raw ? (raw.display_name || raw.name) : '';
  // 标题的最终文案由 syncFormChrome 定：此刻还不知道表单将挂到哪个槽
  // （「添加平台」里要显示成「添加平台」），挂载完成时它会再算一次。
  syncFormChrome();
  $('formMsg').textContent = '';
  $('testBox').hidden = true;
  $('btnForceSave').hidden = true;
  $('chips').innerHTML = '';
  $('fetchHint').textContent = '—';
  Store.set({ fetched: [] });
  document.querySelectorAll('#chModal .field.err').forEach((f) => f.classList.remove('err'));

  setKind(raw ? (raw.kind || 'embedded') : (forceKind || 'embedded'));
  $('chKind').classList.toggle('locked', !!raw);

  if (raw) {
    $('fName').value = raw.display_name || '';
    $('fPrefix').value = raw.model_prefix || '';
    $('fBase').value = raw.base_url || '';
    $('fProtocol').value = raw.protocol || 'chat';
    $('fModels').value = modelsToText(raw.models);
    $('fModelsPath').value = raw.models_path || '';
    $('fHeaders').value = headersToText(raw.headers);
    $('fWeight').value = raw.weight || 1;
    $('fPriority').value = raw.priority || 0;
    $('fRetries').value = raw.retries != null ? raw.retries : 1;
    $('fTimeout').value = raw.timeout || '';
    $('fProxy').value = raw.proxy || '';
    const pr = raw.price || {};
    $('fPriceIn').value = pr.input_per_m != null ? pr.input_per_m : '';
    $('fPriceCached').value = pr.cached_per_m != null ? pr.cached_per_m : '';
    $('fPriceOut').value = pr.output_per_m != null ? pr.output_per_m : '';
    $('fEnabled').checked = !!raw.enabled;
    $('fDataDir').value = raw.data_dir || '';
    $('fCreditValue').value = raw.credit_value > 0 ? raw.credit_value : '';
    $('fHealthM').value = raw.health_path || '';
    $('fPanelAPI').value = raw.panel_api_prefix || '/panel/api';
    $('fPathPrefix').value = raw.path_prefix == null ? '/v1' : raw.path_prefix;
    $('fEnv').value = envToText(raw.env);
    $('fExpose').checked = raw.expose !== false;
    if (raw.preset) $('fPreset').value = raw.preset;
    // 回填已识别的网关种类：保存时带回去。不回填的话，任何一次编辑都会把
    // kind 清空，平台唯一性校验随即失效（用户改个显示名就能绕过）。
    gatewayKind = raw.gateway_kind || '';
    if (raw.preset_inferred) {
      // 该回填分支不调用 applyPreset，所以这行不会被覆盖。
      // 放在这里（本分支其它 presetHint 写入之后）追加一句来源说明。
      const hint = $('presetHint').textContent;
      $('presetHint').textContent = (hint ? hint + ' · ' : '')
        + '模板按当前启动命令与探活路径推断，保存后会记入配置';
    }
    const masked = (raw.api_keys || []).map(maskKey);
    const keyBox = formKind === 'managed' ? $('fKeysM') : $('fKeys');
    keyBox.value = '';
    keyBox.placeholder = masked.length ? masked.join(', ')
      : (formKind === 'managed' ? '本地 Key（可留空）' : 'sk-xxxx');
    $('keyHint').textContent = masked.length ? `留空沿用已保存的 ${masked.length} 个 Key` : '';
    $('keyHint2').textContent = masked.length ? '留空沿用已保存的 Key' : '';
    document.querySelectorAll('#chModal .sec').forEach((d) => { d.open = true; });
  } else {
    ['fName', 'fPrefix', 'fBase', 'fKeys', 'fKeysM', 'fModels', 'fModelsPath',
      'fHeaders', 'fTimeout', 'fProxy', 'fDataDir',
      'fHealthM', 'fEnv', 'fPriceIn', 'fPriceCached', 'fPriceOut']
      .forEach((id) => { $(id).value = ''; });
    $('fProtocol').value = 'chat';
    $('fWeight').value = 1;
    $('fPriority').value = 0;
    $('fRetries').value = 1;
    $('fCreditValue').value = '';
    $('fPanelAPI').value = '/panel/api';
    $('fPathPrefix').value = '/v1';
    $('fEnabled').checked = true;
    $('fExpose').checked = true;
    document.querySelectorAll('#chModal .sec').forEach((d, i) => { d.open = i === 0; });
    // 只有内嵌型还有预设可选。托管型不自动套用：它没有预设下拉了，
    // 套用会把上一个渠道的启动命令/端口环境变量带进新渠道。
    if (formKind === 'embedded') {
      const list = currentPresets();
      if (formKind === 'embedded' && list.length) { $('fPreset').value = list[0].id; applyPreset(list[0].id); }
    }
  }
  // 只有新建才清空「网关种类」——它对新建而言尚未确定。
  // 这个重置以前是无条件执行的（放错位置），会把编辑时的回显值一并清掉：
  // 保存后 kind 丢失（平台唯一性校验失效）。
  if (!raw) {
    gatewayKind = '';
  }
  mountChannelForm();
}

// gatewayKind 当前表单识别出的网关种类（'' = 未知/不适用）。
// 编辑既有渠道时由 openChannelForm 从后端回显值填入；新建时为空，
// 由后端 kindOfUpstream 按名称推断后落盘。
let gatewayKind = '';

function collectForm() {
  const managed = formKind === 'managed';
  const body = {
    kind: managed ? 'managed' : 'embedded',
    // gateway_kind 带上，后端据此判重并落盘识别结果。
    // 编辑时由openChannelForm 从 raw 里回填（见下方 gatewayKind 变量），
    // 新建时留空由后端按名称推断。
    gateway_kind: gatewayKind,
    name: editingName || '',
    original_name: editingName || '',
    display_name: $('fName').value.trim(),
    protocol: $('fProtocol').value,
    model_prefix: $('fPrefix').value.trim(),
    model_source: 'auto',
    models: parseModels($('fModels').value),
    models_path: $('fModelsPath').value.trim(),
    headers: parseHeaders($('fHeaders').value),
    weight: intOf($('fWeight').value, 1),
    priority: intOf($('fPriority').value, 0),
    retries: intOf($('fRetries').value, 1),
    timeout: $('fTimeout').value.trim(),
    enabled: $('fEnabled').checked,
    preset: $('fPreset').value,
  };
  if (managed) {
    Object.assign(body, {
      data_dir: $('fDataDir').value.trim(),
      credit_value: floatOf($('fCreditValue').value) || 0,
      health_path: $('fHealthM').value.trim(),
      panel_api_prefix: $('fPanelAPI').value.trim(),
      path_prefix: $('fPathPrefix').value.trim(),
      env: parseEnv($('fEnv').value),
      expose: $('fExpose').checked,
    });
  } else {
    Object.assign(body, { base_url: $('fBase').value.trim(), proxy: $('fProxy').value.trim() });
    // 自定义单价：三个维度要么全填要么全不填。
    // 只填一半会让费用变成「一半用自定义价、一半用内置价」的混合口径，
    // 那个数字比明确显示「价格未知」更难解释，所以留空就整组不提交。
    const pin = floatOf($('fPriceIn').value);
    const pcached = floatOf($('fPriceCached').value);
    const pout = floatOf($('fPriceOut').value);
    if (pin != null && pout != null) {
      body.price = {
        input_per_m: pin,
        output_per_m: pout,
        // 缓存价留空时按全价的 10% 估算——绝大多数厂商的缓存价就是这个量级。
        cached_per_m: pcached != null ? pcached : 0,
        cached_ratio: pcached != null ? 0 : 0.1,
      };
    }
  }
  const keyText = lines(managed ? $('fKeysM').value : $('fKeys').value);
  if (!editingName || keyText.length) body.api_keys = keyText;
  return body;
}

function formMsg(text, isErr) {
  $('formMsg').textContent = text || '';
  $('formMsg').classList.toggle('err-text', !!isErr);
}

async function runTest() {
  document.querySelectorAll('#chModal .field.err').forEach((f) => f.classList.remove('err'));
  $('testBox').hidden = false;
  $('testSteps').innerHTML = '<div class="tstep"><span class="ic">…</span><span class="tx">测试中…</span></div>';
  try {
    const tr = await post('/api/channels/test', collectForm(), 120000);
    $('testSteps').innerHTML = renderTest(tr);
    if (tr.ok) { formMsg('测试通过，可以保存'); $('btnForceSave').hidden = true; }
    else {
      (tr.fields || []).forEach((f) => {
        document.querySelectorAll(`#chModal .field[data-f="${f}"]`).forEach((el) => {
          const box = el.closest('.konly');
          if (box && box.hidden) return;
          el.classList.add('err');
        });
      });
      formMsg('测试未通过，已阻止保存', true);
      $('btnForceSave').hidden = false;
    }
    return tr;
  } catch (e) {
    $('testSteps').innerHTML = `<div class="tstep bad"><span class="ic">✕</span>
      <span class="tx"><span class="tn">测试请求失败</span><div class="td">${esc(e.message)}</div></span></div>`;
    formMsg('测试失败：' + e.message, true);
    $('btnForceSave').hidden = false;
    return { ok: false };
  }
}

async function saveChannel(force) {
  if (!force && formKind === 'embedded') {
    const tr = await runTest();
    if (!tr || !tr.ok) return;
  }
  formMsg('正在保存…');
  try {
    const d = await post('/api/channels', collectForm(), 90000);
    // 收表单之前先记下这两件事：等会儿要判断「是不是从添加平台来的」、
    // 以及刚加的是哪一类（unmount 本身不清 formKind，但读一眼更直白）。
    const wasAdding = Store.get().view === 'add';
    const kind = formKind;
    unmountChannelForm();
    // 从「添加平台」加完就把人送到对应的列表去看新渠道：留在新建页
    // 只能看到一张空表单，用户会怀疑到底存进去没有。setView 内部会重新
    // 拉一次渠道列表，所以这一支不必再拉。
    if (wasAdding) setView(kind === 'managed' ? 'acct' : 'api');
    else await loadChannels();
    const notes = [];
    if ((d.warns || []).length) notes.push('提示：\n· ' + d.warns.join('\n· '));
    if (d.start_error) notes.push('渠道已保存，但子进程启动失败：\n' + d.start_error);
    if (notes.length) alert(notes.join('\n\n'));
  } catch (e) {
    formMsg(e.message, true);
  }
}

async function fetchModels() {
  const body = collectForm();
  if (formKind === 'embedded' && !body.base_url) { $('fetchHint').textContent = '请先填写 Base URL'; return; }
  $('fetchHint').textContent = '正在拉取…';
  try {
    const d = await post('/api/channels/models', body, 60000);
    Store.set({ fetched: d.models || [] });
    renderChips();
    $('fetchHint').textContent = `已拉取 ${S.fetched.length} 个模型`;
  } catch (e) {
    Store.set({ fetched: [] });
    renderChips();
    $('fetchHint').textContent = '拉取失败：' + e.message;
  }
}

// freeBadge 免费徽标：按类型给不同文案。
//
// 「夜间」与「限时」必须分开：都是免费，但时段不同。混成一个 Free 标签，
// 用户会以为白天也能免费用，踩坑后才发现。夜间模型在非夜间时段明确标出
// 「已结束」——不给提示等于让人以为随时都能用。
function freeBadge(m) {
  if (m.free_kind === 'nightly') {
    return m.nightly_now
      ? '<span class="badge accent" title="夜间免费：每晚 23:00 至次日 08:00，现在可用">夜间</span>'
      : '<span class="badge muted" title="夜间免费：每晚 23:00 至次日 08:00，现在不在时段内">夜间·已结束</span>';
  }
  if (m.free_kind === 'limited') {
    return '<span class="badge accent" title="限时免费">限时</span>';
  }
  if (m.free_kind === 'open') {
    return '<span class="badge accent">Free</span>';
  }
  // 兼容旧响应（只有 free 布尔、没有类型）
  return m.free ? '<span class="badge accent">Free</span>' : '';
}

function pickedIds() {
  // 已在「模型列表」里的真实 ID
  return new Set(parseModels($('fModels').value).map((m) => m.id));
}

function renderChips() {
  const filter = $('fFilter').value.trim().toLowerCase();
  const have = pickedIds();
  // 过滤同时看展示名与真实 ID：用户可能按其中任一边找。
  const list = S.fetched.filter((m) => !filter
    || m.name.toLowerCase().includes(filter) || m.id.toLowerCase().includes(filter));
  $('chips').innerHTML = S.fetched.length
    ? list.slice(0, 500).map((m) => `<label class="chipsel">
        <input type="checkbox" value="${esc(m.id)}" data-name="${esc(m.name)}"
          data-free="${esc(m.free_kind || '')}" data-nightly="${m.nightly_now ? '1' : ''}">${esc(m.name)}${
        freeBadge(m)}${
        // 已加入的**继续显示**（隐藏会让用户以为它没了），
        // 只加一个标记说明状态，可取消勾选但不会重复写入。
        have.has(m.id) ? '<span class="badge muted">已加入</span>' : ''}</label>`).join('')
    : '';
  syncSelectAll();
}

// syncSelectAll 让「全选」复选框反映当前可见列表的勾选状态。
//
// 三态：全选 / 部分选中（indeterminate）/ 全不选。
// 状态失真比没有这个控件更糟——用户会以为全选了却只加进一部分。
function syncSelectAll() {
  const box = $('fSelectAll');
  const all = Array.from($('chips').querySelectorAll('input[type=checkbox]'));
  const on = all.filter((b) => b.checked).length;
  box.checked = all.length > 0 && on === all.length;
  box.indeterminate = on > 0 && on < all.length;
  // 计数随过滤变化，文案跟着变，避免「全选」被误解成「全选全部 19 个」
  $('selectAllLabel').textContent = all.length
    ? `全选（当前 ${all.length} / 共 ${S.fetched.length}）`
    : '全选';
  box.disabled = all.length === 0;
}

function toggleSelectAll(on) {
  $('chips').querySelectorAll('input[type=checkbox]').forEach((b) => { b.checked = on; });
  syncSelectAll();
}

// 手动改单个勾选也要同步「全选」的三态
$('chips').addEventListener('change', syncSelectAll);

function addPicked() {
  const boxes = Array.from($('chips').querySelectorAll('input:checked'));
  if (!boxes.length) {
    showPickResult([], S.fetched, [], true);
    return;
  }
  const have = pickedIds();
  const modelOf = (b) => ({
    id: b.value, name: b.dataset.name || b.value,
    free_kind: b.dataset.free || '', nightly_now: b.dataset.nightly === '1',
  });
  const added = [];   // 本次新加入的 model
  const skipped = []; // 勾了但已在列表里的 model
  boxes.forEach((b) => {
    const m = modelOf(b);
    if (have.has(m.id)) { skipped.push(m); return; }
    have.add(m.id);
    added.push(m);
  });

  if (added.length) {
    // 展示名与真实 ID 不同时写成 `真实ID => 展示名`：
    // 上游只认左边那个，右边纯粹给人看。
    const lines = added.map((a) => (a.name !== a.id ? `${a.id} => ${a.name}` : a.id));
    const text = $('fModels').value.trim();
    $('fModels').value = (text ? text + '\n' : '') + lines.join('\n');
  }
  // 本次没加入的 = 已勾但早就在列表里 + 压根没勾
  //
  // 传 model 对象而不是名字：showPickResult 要渲染免费徽标，
  // 传字符串会让那一区的条目全渲染成空（只有名字，没有类型信息）。
  const pickedSet = new Set(boxes.map((b) => b.value));
  const unchecked = S.fetched.filter((m) => !pickedSet.has(m.id));

  $('fetchHint').textContent = added.length
    ? `已加入 ${added.length} 个模型`
    : '没有新模型可加入（所选模型均已在列表中）';
  showPickResult(added, skipped, unchecked, false);
  renderChips();
}

// showPickResult 弹窗列出「本次加入」与「尚未加入」的明细。
//
// 为什么用弹窗而不是一行 hint：上游动辄十几个模型，
// 「哪些漏了」是用户点这个按钮后最需要确认的事，挤在一行里会看漏。
function showPickResult(added, skipped, unchecked, noSelection) {
  // 条目带上免费徽标：这个弹窗是用户判断「哪些免费、什么时段」的场合，
  // 只列名字等于把最关键的信息留在了上一层。
  const line = (m) => `<span class="pick-i">${esc(m.name)}${freeBadge(m)}</span>`;
  const section = (title, list, cls) => (list.length ? `<div class="pick-sec ${cls}">
      <div class="pick-t">${title}（${list.length}）</div>
      <div class="pick-list">${list.map(line).join('')}</div>
    </div>` : '');

  let head;
  if (noSelection) {
    head = '<div class="empty">没有勾选任何模型，无法加入。<br>可用上方「全选」一次性勾选当前列表。</div>';
  } else if (!added.length) {
    head = '<div class="empty">所选模型都已在列表中，无需重复加入。</div>';
  } else {
    head = '';
  }

  $('pickBody').innerHTML = head
    + section('本次已加入', added, 'ok')
    + section('已在列表中（本次跳过）', skipped, 'muted')
    + section('尚未加入（未勾选）', unchecked, 'warn');

  const total = S.fetched.length;
  $('pickSummary').textContent = noSelection
    ? `共 ${total} 个模型`
    : `共 ${total} 个 · 加入 ${added.length} · 跳过 ${skipped.length} · 未勾选 ${unchecked.length}`;
  $('pickModal').hidden = false;
}

/* ── 限时套餐自动领取 ─────────────────────────────────── */

let claimState = null;   // /api/claim 的响应
let claimChans = [];     // 候选托管渠道（供下拉）

async function loadClaim() {
  try {
    const [st, chans] = await Promise.all([
      api('/api/claim'),
      api('/api/channels').catch(() => ({ channels: [] })),
    ]);
    claimState = st;
    // 只列托管渠道：内嵌型渠道背后没有「网关」这个概念
    claimChans = (chans.channels || []).filter((c) => c.source === 'managed');
    renderClaim();
  } catch (e) {
    $('claimHint').textContent = '读取失败：' + e.message;
  }
}

function renderClaim() {
  const st = claimState;
  if (!st) return;
  $('fClaimOn').checked = !!st.enabled;
  $('claimWindowTo').textContent = st.window_to || '—';
  $('claimHint').textContent = st.configured
    ? `每天 ${st.window_from}–${st.window_to} 自动领取一次`
    : '未填写后台密码，暂时无法领取';

  if (document.activeElement !== $('fClaimAt')) $('fClaimAt').value = st.at || '12:01';

  // 渠道下拉：保留「自动识别」这个默认项
  const sel = $('fClaimChannel');
  const want = ['', ...claimChans.map((c) => c.name)];
  const have = Array.from(sel.options).map((o) => o.value);
  if (JSON.stringify(want) !== JSON.stringify(have)) {
    sel.innerHTML = '<option value="">自动识别</option>'
      + claimChans.map((c) => `<option value="${esc(c.name)}">${esc(c.display_name || c.name)}${
        c.ready ? '' : '（未就绪）'}</option>`).join('');
  }
  sel.value = st.channel || '';

  // 状态一句话说清：能不能领、今天领没领
  let state;
  if (!st.enabled) state = '未启用';
  else if (!st.configured) state = '缺少后台密码';
  else if (st.today_done) state = '今天已领取';
  else if (st.in_window) state = '可领取（窗口内）';
  else state = `等待 ${st.window_from}`;
  $('claimState').textContent = state;
  $('btnClaimNow').disabled = !st.configured;

  renderClaimLast();
}

function renderClaimLast() {
  const box = $('claimLast');
  const r = claimState && claimState.last;
  if (!r || !r.at || r.at.startsWith('0001-01-01')) { box.innerHTML = ''; return; }
  const when = new Date(r.at);
  const time = `${when.getFullYear()}-${String(when.getMonth() + 1).padStart(2, '0')}-${String(when.getDate()).padStart(2, '0')} `
    + `${String(when.getHours()).padStart(2, '0')}:${String(when.getMinutes()).padStart(2, '0')}`;
  const how = r.trigger === 'manual' ? '手动' : '自动';
  let head;
  if (r.skipped) head = `上次（${time} · ${how}）未领取：${esc(r.skipped)}`;
  else if (r.error) head = `上次（${time} · ${how}）失败：${esc(r.error)}`;
  else head = `上次（${time} · ${how}）成功 ${r.ok} 个、失败 ${r.fail} 个`;
  const rows = (r.outcomes || []).map((o) => `<tr>
    <td class="nm">${esc(o.account)}</td>
    <td>${o.ok ? '<span class="badge ok">成功</span>' : '<span class="badge err">失败</span>'}</td>
    <td class="sub">${esc(o.plan || '')}</td>
    <td class="sub">${esc(o.message || '')}</td>
    <td class="sub">${esc(o.next_at || '')}</td></tr>`).join('');
  box.innerHTML = `<div class="mt-sec">领取记录</div>
    <div class="fhint">${head}</div>
    ${rows ? `<div class="tablewrap"><table class="dt">
      <thead><tr><th>账号</th><th>结果</th><th>套餐</th><th>说明</th><th>名额恢复</th></tr></thead>
      <tbody>${rows}</tbody></table></div>` : ''}`;
}

async function saveClaim() {
  const body = {
    enabled: $('fClaimOn').checked,
    at: $('fClaimAt').value.trim(),
    channel: $('fClaimChannel').value,
  };
  // 密码框留空表示「不改」，而不是清空——否则每次改时段都要重填一遍
  const key = $('fClaimKey').value.trim();
  if (key) body.admin_key = key;
  try {
    claimState = await post('/api/claim/config', body);
    $('fClaimKey').value = '';
    toast('领取设置已保存');
    renderClaim();
  } catch (e) {
    toast('保存失败：' + e.message, true);
  }
}

async function claimNow() {
  const btn = $('btnClaimNow');
  btn.disabled = true;
  btn.textContent = '领取中…';
  try {
    const r = await post('/api/claim/now', {}, 120000);
    const msg = r.skipped ? '未领取：' + r.skipped
      : r.error ? '失败：' + r.error
      : `完成：成功 ${r.ok} 个、失败 ${r.fail} 个`;
    toast(msg, !!(r.error || r.skipped));
    await loadClaim();
  } catch (e) {
    toast('领取失败：' + e.message, true);
  } finally {
    btn.textContent = '立即领取一次';
    btn.disabled = false;
  }
}

/* ── Mergence 日志 ────────────────────────────────────── */

let logs = [];
let logSeq = 0;
let logTimer = null;

function levelRank(l) {
  return { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 }[String(l).toUpperCase()] ?? 1;
}

function renderLogs() {
  const min = levelRank($('fLevel').value);
  const prov = $('fProv').value.trim().toLowerCase();
  const q = $('fQ').value.trim().toLowerCase();
  const rows = logs.filter((e) => {
    if (levelRank(e.level) < min) return false;
    if (prov && !(e.provider || '').toLowerCase().includes(prov)) return false;
    if (q) {
      const hay = [e.msg, e.raw, e.req_id, e.provider].join(' ') + ' ' + JSON.stringify(e.fields || {});
      if (!hay.toLowerCase().includes(q)) return false;
    }
    return true;
  });
  $('mmBox').innerHTML = rows.length ? rows.slice(-400).map((e) => `
    <div class="logline lvrow-${esc(e.level)}">
      <span class="ct">${when(e.time)}</span>
      <span class="lv lv-${esc(e.level)}">${esc(e.level)}</span>
      <span class="dim">[${esc(e.provider || 'core')}]</span>
      <span class="rq">${esc(e.req_id || '')}</span>
      ${esc(e.msg)}${e.raw ? ' · ' + esc(e.raw) : ''}
    </div>`).join('') : '<div class="empty">暂无日志</div>';
  if ($('fFollow').checked) {
    const b = $('mmBox'); b.scrollTop = b.scrollHeight;
  }
}

async function pollLogs() {
  try {
    const d = await api('/api/logs?since=' + logSeq + '&limit=500');
    if (d.entries && d.entries.length) {
      logSeq = d.seq;
      logs = logs.concat(d.entries).slice(-800);
      if (Store.get().view === 'logs') renderLogs();
    } else if (typeof d.seq === 'number') logSeq = d.seq;
  } catch (e) { /* 退出过程中会失败，下一轮自愈 */ }
  logTimer = setTimeout(pollLogs, 1400);
}

function startLogPoll() {
  renderLogs();
  if (!logTimer) pollLogs();
}

/* ── 设置 ─────────────────────────────────────────────── */

function loadSettings() {
  const st = S.status;
  if (!st) return;
  $('setURL').textContent = (st.base_url || '') + '/v1';
  if (document.activeElement !== $('fPort')) $('fPort').value = st.panel_port || 0;
  renderKey(st.access_key || '');
  renderCloseBehavior(st.minimize_to_tray !== false);
  renderAcctConsolePref(st.acct_console !== false);
  loadClaim();
}

function renderKey(key) {
  const shown = Store.get().keyShown;
  $('setKey').textContent = shown ? key : maskKey(key);
  $('btnKeyShow').textContent = shown ? '隐藏' : '显示';
}

async function savePort() {
  const port = intOf($('fPort').value, 0);
  if (port < 0 || port > 65535) { toast('端口需在 1–65535（0 = 动态）', 'err'); return; }
  $('btnSavePort').disabled = true;
  try {
    const d = await post('/api/settings/port', { port }, 30000);
    const cur = (S.status && S.status.panel_port) || 0;
    if (d.port !== cur && confirm(`端口已切换到 ${d.port}。\n\n原地址的连接将在完成剩余请求后关闭。\n是否立即跳转到新地址？`)) {
      location.href = d.base_url + '/';
      location.reload();
      return;
    }
    $('setURL').textContent = d.base_url + '/v1';
    toast('端口已保存', 'ok');
  } catch (e) {
    toast('切换失败：' + e.message, 'err');
  } finally {
    $('btnSavePort').disabled = false;
  }
}

/* renderCloseBehavior 回显「关窗行为」。
 *
 * 默认 true 而不是「取反」：服务端老版本没有 minimize_to_tray 字段，
 * 缺字段必须落到「最小化到托盘」——那才是历史行为。写成反向推断的话，
 * 老版本/字段缺失会悄悄变成「关窗即退出」，属于静默改变用户既有行为。
 */
function renderCloseBehavior(on) {
  $('fCloseTray').checked = !!on;
  $('fCloseExit').checked = !on;
}

/* saveCloseBehavior 改关窗行为，选完即存。
 *
 * 存失败必须把界面拨回原值：否则用户看到的是「我选了但其实没生效」，
 * 而配置里的真值与他眼前的 radio 不一致——这种不一致最难排查。
 */
async function saveCloseBehavior(on) {
  const want = !!on;
  try {
    const d = await post('/api/settings/close', { minimize_to_tray: want }, 15000);
    const v = typeof d.minimize_to_tray === 'boolean' ? d.minimize_to_tray : want;
    if (S.status) S.status.minimize_to_tray = v;
    renderCloseBehavior(v);
    toast(d.note || '已保存', 'ok');
  } catch (e) {
    renderCloseBehavior(S.status ? S.status.minimize_to_tray !== false : true);
    toast('保存失败：' + e.message, 'err');
  }
}

/* renderAcctConsolePref 回显「显示积分型平台控制台入口」开关。
 *
 * 判据写成 `!== false` 而不是直接取值：老配置里没有 ui.acct_console 字段，
 * 缺字段必须落到「显示」——那是这一块自诞生以来的行为，也是入口可见的前提。
 */
function renderAcctConsolePref(on) {
  $('fAcctConsole').checked = !!on;
  $('acctConsoleState').textContent = on ? '在「积分型平台」页显示' : '已在「积分型平台」页收起';
}

/* saveAcctConsole 改「控制台入口」显隐，切换即存。
 *
 * 存完立刻重画那一块：用户不必刷新、也不用重启就能看到结果。存失败要把开关
 * 拨回原值——否则界面显示「已收起」而配置里还是开，两边不一致最难排查。
 */
async function saveAcctConsole(on) {
  const want = !!on;
  try {
    const d = await post('/api/settings/console', { acct_console: want }, 15000);
    const v = typeof d.acct_console === 'boolean' ? d.acct_console : want;
    if (S.status) S.status.acct_console = v;
    renderAcctConsolePref(v);
    renderAcctConsole((Store.get().channels || []).filter((c) => c.source === 'managed'));
    toast(d.note || '已保存', 'ok');
  } catch (e) {
    renderAcctConsolePref(S.status ? S.status.acct_console !== false : true);
    toast('保存失败：' + e.message, 'err');
  }
}

async function regenKey() {
  if (!confirm('重新生成对外 API Key？\n\n旧 Key 立即失效，所有外部客户端都需要更新。')) return;
  try {
    const d = await post('/api/access-key/regenerate', {}, 15000);
    Store.set({ keyShown: true });
    if (S.status) S.status.access_key = d.access_key;
    renderKey(d.access_key);
    renderStatus(S.status);
    toast('新 Key 已生效，请更新外部客户端', 'ok');
  } catch (e) { toast('生成失败：' + e.message, 'err'); }
}

/* ── 事件绑定 ─────────────────────────────────────────── */

function bind() {
  $('nav').addEventListener('click', (ev) => {
    const a = ev.target.closest('.nav-i');
    if (!a) return;
    // 「打开管理面板」是外链：用系统浏览器打开，应用内 WebView 打不了它
    if (a.dataset.panelUrl) { desktopOpenExternal(a.dataset.panelUrl); return; }
    // 上游控制台的入口带 data-chan：点一下就同时选定渠道与视图
    if (a.dataset.chan) openUpView(a.dataset.chan, a.dataset.view);
    else setView(a.dataset.view);
  });
  bindNavSections();
  $('btnRefresh').onclick = () => {
    setView(Store.get().view);
    pollStatus();
    // 概览的指标来自 /api/metrics，与状态接口不同源，必须单独刷。
    if (Store.get().view === 'overview') loadMetrics(true);
  };
  $('btnTheme').onclick = () => {
    const cur = document.documentElement.dataset.theme;
    const next = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('mm-theme', next); } catch (e) { /* 忽略 */ }
  };
  $('chCards').addEventListener('click', (ev) => {
    const p = ev.target.closest('[data-goto]');
    if (p) setView(p.dataset.goto);
  });

  [$('apiList'), $('acctList')].forEach((list) => {
    list.addEventListener('click', (ev) => {
      const card = ev.target.closest('.chcard');
      if (!card) return;
      const btn = ev.target.closest('[data-act]');
      if (!btn || btn.dataset.act === 'toggle') return;
      channelAction(card.dataset.name, btn.dataset.act, card);
    });
    list.addEventListener('change', (ev) => {
      if (ev.target.dataset.act !== 'toggle') return;
      const card = ev.target.closest('.chcard');
      if (card) channelAction(card.dataset.name, 'toggle', card);
    });
  });
  // 「添加渠道」的入口只剩侧栏那个「添加平台」视图，这里不再有按钮要绑。

  // 概览指标
  $('btnMetricsReload').onclick = () => loadMetrics(true);
  $('mtRange').addEventListener('change', () => loadMetrics(true));

  $('chKind').addEventListener('click', (ev) => {
    const b = ev.target.closest('.seg-b');
    if (!b || editingName) return;
    setKind(b.dataset.kind);
    const list = currentPresets();
    if (formKind === 'embedded' && list.length) { $('fPreset').value = list[0].id; applyPreset(list[0].id); }
  });
  $('btnCloseCh').onclick = () => unmountChannelForm();
  $('fPreset').onchange = () => applyPreset($('fPreset').value);
  $('btnFetch').onclick = fetchModels;
  $('fFilter').addEventListener('input', renderChips);
  $('btnAddPicked').onclick = addPicked;
  $('fSelectAll').addEventListener('change', (e) => toggleSelectAll(e.target.checked));
  $('btnClosePick').onclick = () => { $('pickModal').hidden = true; };
  $('btnPickOk').onclick = () => { $('pickModal').hidden = true; };
  $('btnTest').onclick = runTest;
  $('btnSaveCh').onclick = () => saveChannel(false);
  $('btnForceSave').onclick = () => saveChannel(true);
  $('btnCloseTr').onclick = () => { $('trModal').hidden = true; };
  $('btnCloseAdd').onclick = () => { $('addModal').hidden = true; };
  $('btnCloseDrawer').onclick = () => { $('drawer').hidden = true; };
  // zcode 账号面板的四个弹窗也走同一套「点遮罩 / ESC 关闭」——
  // 不在这里登记的话它们关不掉，用户只能刷新页面。
  // chModal 是例外：它挂在视图里当内联面板，关闭必须走 unmountChannelForm，
  // 才能同时撤销 .inline 与 .chsplit.editing。只设 hidden 的话，面板会
  // 继续占用槽位、显示在渠道列表正下方（.inline 的 display 压过 [hidden]）。
  ['trModal', 'addModal', 'zcAccModal', 'zcClaimModal', 'zcImportModal', 'zcSetModal'].forEach((id) => {
    $(id).addEventListener('click', (ev) => { if (ev.target === $(id)) $(id).hidden = true; });
  });
  // 「添加平台」里没有遮罩可点也没层可关：那张表单就是页面本体，
  // 收掉它只会留下一个空视图。所以两条习惯路径在 add 视图里都不收表单。
  const dismissableForm = () => Store.get().view !== 'add';
  $('chModal').addEventListener('click', (ev) => {
    if (ev.target === $('chModal') && dismissableForm()) unmountChannelForm();
  });
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    ['trModal', 'addModal', 'drawer', 'zcAccModal', 'zcClaimModal', 'zcImportModal', 'zcSetModal']
      .forEach((id) => { $(id).hidden = true; });
    if (dismissableForm()) unmountChannelForm();
  });

  ['fLevel', 'fProv', 'fQ'].forEach((id) => {
    $(id).addEventListener('input', renderLogs);
    $(id).addEventListener('change', renderLogs);
  });
  $('btnClear').onclick = () => { logs = []; renderLogs(); };

  $('btnSavePort').onclick = savePort;
  $('btnClaimSave').onclick = saveClaim;
  $('btnClaimNow').onclick = claimNow;
  $('fClaimOn').addEventListener('change', saveClaim);
  // 二选一，没有「填错值」的空间，所以不设保存按钮：选中即存。
  ['fCloseTray', 'fCloseExit'].forEach((id) => {
    $(id).addEventListener('change', () => saveCloseBehavior($('fCloseTray').checked));
  });
  // 同上，只有一个开关，切换即存。
  $('fAcctConsole').addEventListener('change', () => saveAcctConsole($('fAcctConsole').checked));
  $('btnKeyShow').onclick = () => {
    Store.set({ keyShown: !Store.get().keyShown });
    renderKey((S.status && S.status.access_key) || '');
  };
  $('btnKeyCopy').onclick = async () => {
    try { await copyText((S.status && S.status.access_key) || ''); toast('已复制', 'ok'); }
    catch (e) { toast('复制失败，请先「显示」', 'err'); }
  };
  $('btnKeyRegen').onclick = regenKey;
  $('btnShowWin').onclick = () => {
    if (!desktopShowWindow()) toast('该功能需要在桌面窗口内使用', 'err');
  };

  // ── 控制台工具条：平台选择器 + 页签栏 ────────────────────
  $('conChan').addEventListener('change', (ev) => {
    const name = ev.target.value;
    if (!name) return;
    openUpView(name, conViewFor(name, Store.get().view));
  });
  $('conTabs').addEventListener('click', (ev) => {
    const ext = ev.target.closest('[data-con-ext]');
    if (ext) { desktopOpenExternal(ext.dataset.conExt); return; }
    const b = ev.target.closest('[data-con-view]');
    // 用下拉框当前显示的值而不是 UP.chan：UP.chan 在「刚进控制台还没点过
    // 任何入口」时是空的，而那时页签已经可点，按 UP.chan 会切到空渠道。
    if (b) openUpView($('conChan').value || UP.chan, b.dataset.conView);
  });
  $('btnConAdd').onclick = () => {
    const name = $('conChan').value || UP.chan;
    if (!name) return;
    // 必须先把渠道落到全局再开弹层：弹层里的授权 / 导入都是发给 UP.chan 的，
    // 只改下拉框的值而不改它，请求会发到上一个渠道去。
    if (UP.chan !== name) openUpView(name, conViewFor(name, Store.get().view));
    openAddAccount();
  };
  // 「积分型平台」页顶部的入口区块：点渠道进它的第一个控制台视图（账号池）
  // —— 那一页才有「添加账号」，这是这一块最主要的用途。
  $('acctConsoleList').addEventListener('click', (ev) => {
    const ext = ev.target.closest('[data-con-ext]');
    if (ext) { desktopOpenExternal(ext.dataset.conExt); return; }
    const b = ev.target.closest('[data-con-open]');
    if (!b) return;
    const defs = upViewDefs((Store.get().channels || []).find((x) => x.name === b.dataset.conOpen));
    if (defs) openUpView(b.dataset.conOpen, defs[0][0]);
  });
  $('btnCopyURL').onclick = async () => {
    try { await copyText($('epURL').textContent); toast('已复制', 'ok'); }
    catch (e) { toast('复制失败', 'err'); }
  };
  $('btnCopyKey').onclick = async () => {
    try { await copyText((S.status && S.status.access_key) || ''); toast('已复制', 'ok'); }
    catch (e) { toast('复制失败', 'err'); }
  };

  // 桌面桥：接收原生通知（主题、端口变化）
  const b = desktopBridge();
  if (b && b.addEventListener) {
    b.addEventListener('message', (ev) => {
      const m = ev.data || {};
      if (m.type === 'theme-changed') document.documentElement.dataset.theme = m.theme;
      else if (m.type === 'port-changed') {
        toast('端口已切换，正在跳转到新地址…');
        setTimeout(() => { location.href = m.url + '/'; location.reload(); }, 600);
      }
    });
  }

  bindUpstream();
}

/* ── 启动 ─────────────────────────────────────────────── */

async function pollStatus() {
  try { renderStatus(await api('/api/status')); }
  catch (e) { $('navState').textContent = '连接中断'; }
}

(async function boot() {
  try {
    // 与 head 里的首帧脚本同一套白名单：那里已经设过一次，这里是兜底
    // （比如页面被别的入口重新载入）。脏值一律不认，否则两个主题块都不匹配。
    const t = localStorage.getItem('mm-theme');
    if (t === 'dark' || t === 'light') document.documentElement.dataset.theme = t;
  } catch (e) { /* 忽略 */ }

  bind();
  // 先把静态卡片（对外出口 / 渠道概况）包成可收起面板，再进场
  setupStaticFolds();
  // 恢复上次选中的控制台渠道：渠道列表会在首次轮询后校验它是否还在
  UP.chan = readUpChan();
  setView('overview');

  try {
    const d = await api('/api/presets');
    Store.set({ presets: d.presets || [], managedPresets: d.managed_presets || [] });
    fillPresets();
  } catch (e) { /* 模板为空不影响 */ }

  await pollStatus();
  setInterval(pollStatus, 3000);
  pollLogs();
})();
