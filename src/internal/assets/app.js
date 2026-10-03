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
  'logs':        { t: 'ModelMux 运行日志', sub: () => '本进程结构化日志' },
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
  if (view === 'api' || view === 'acct') return 'chan';
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

/* 常驻分组：不参与折叠（上游控制台的入口要随时可见）。
   旧版本往 localStorage 存过 up:1，所以既要跳过恢复、也要清掉残留，
   否则升级后这个分组会在启动时又被收起来。 */
const NAVSEC_FIXED = ['up'];

function bindNavSections() {
  const st = readNavSec();
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
  $('ttl').textContent = VIEW_META[meta].t;
  $('subMeta').textContent = VIEW_META[meta].sub();
  Store.set({ view: meta });
  // 高亮与侧栏入口都要在「视图已确定」之后算：渠道卡片的选中态
  // 依赖 视图 + 渠道 两个维度。
  applyNavActive();
  renderUpConsoleNav();

  const primary = $('btnPrimary');
  primary.hidden = true; primary.onclick = null;
  if (meta === 'api') { primary.hidden = false; primary.textContent = '添加 API 平台'; primary.onclick = () => openChannelForm(null, 'embedded'); }
  if (meta === 'acct') { primary.hidden = false; primary.textContent = '添加积分型平台'; primary.onclick = () => openChannelForm(null, 'managed'); }
  if (meta === 'up-accounts') { primary.hidden = false; primary.textContent = '添加账号'; primary.onclick = openAddAccount; }

  revealNavSection(meta);
  // 离开渠道视图就收起编辑区：表单是单例，留着会跟着跑到别的视图里
  if (meta !== 'api' && meta !== 'acct') unmountChannelForm();
  if (meta === 'api' || meta === 'acct') loadChannels();
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

/* ── 上游控制台入口（侧栏） ─────────────────────────────
 *
 * 入口表只在这里定义一次，渲染与高亮共用。顺序按使用频率排：
 * 账号池/任务/模型是日常，配置与日志属于排查时才看。
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

  // 这个分组**常驻**：不随视图显隐，也不允许被收起。
  //
  // 之前是 `hidden = chans.length === 0`，而渠道列表只由 loadChannels()
  // 塞进 Store，那个函数又只在 api/acct 视图被调用 —— 于是启动后必须先
  // 点一下「积分型平台」入口，控制台入口才出现。入口要「先点某处」才可见，
  // 等于把它藏起来了。
  group.hidden = false;
  group.classList.remove('closed');

  // 选中的渠道被删掉后不能继续挂着：否则侧栏高亮不到任何一条，
  // 而上游请求还会按旧渠道名发出去。
  if (UP.chan && !chans.some((c) => c.name === UP.chan)) {
    UP.chan = '';
    Store.set({ upChan: '' });
    rememberUpChan('');
  }
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
  if (c.console_kind === 'gateway') {
    return UP_VIEWS.map(([v, label, icon]) =>
      `<a class="nav-i sub" data-view="${v}" data-chan="${esc(c.name)}">
        <i>${icon}</i><span>${esc(label)}</span></a>`).join('');
  }
  if (c.console_kind === 'zcode') {
    // zcode2api：原生视图（账号池 / 运行监控 / 网关设置）。密码由 ModelMux
    // 服务端注入，浏览器不接触密码，所以查看无需输入面板密码。
    return [
      ['up-zc-accounts', '账号池', '◍'],
      ['up-zc-monitor', '运行监控', '▤'],
      ['up-zc-settings', '网关设置', '⚙'],
    ].map(([v, label, icon]) =>
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
      : '<div class="empty">还没有 API 型平台。点右上角「添加 API 平台」开始。</div>';
    $('acctList').innerHTML = acctChs.length ? acctChs.map(chCardHTML).join('')
      : (acctHidden > 0
        ? '<div class="empty">还没有已添加账号的平台。点右上角「添加账号」开始。</div>'
        : '<div class="empty">还没有积分型平台。点右上角「添加积分型平台」开始。</div>');
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

/* ── 渠道表单（嵌在大面板里的编辑区） ───────────────────
 *
 * 表单 DOM 是单例（#chModal），两个渠道视图各有一个挂载槽。
 * 为什么不是「每个视图各复制一份」：字段有几十个，两份必然出现
 * 「这份改了那份还留着旧值」的错误，而这种错要到保存时才发现。
 */
function chEditorView() {
  return Store.get().view === 'api' ? 'api' : 'acct';
}

function mountChannelForm() {
  const view = chEditorView();
  const slot = document.querySelector(`#view-${view} .ch-editor`);
  const modal = $('chModal');
  if (!slot || !modal) return;
  if (modal.parentElement !== slot) slot.appendChild(modal);
  modal.hidden = false;
  modal.classList.add('inline');
  const split = $(view + 'Split');
  if (split) split.classList.add('editing');
}

function unmountChannelForm() {
  const modal = $('chModal');
  if (modal) modal.hidden = true;
  document.querySelectorAll('.chsplit.editing').forEach((el) => el.classList.remove('editing'));
}

let editingName = '';
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
    ? '积分型平台：拉起进程或在进程内运行，账号在「控制台」里管理。'
    : 'API 型平台：直接向 OpenAI 兼容端点发请求，不额外起进程。';
  fillPresets();
  // 切到托管型时，若上一次选的是内置原生模板，子进程专有字段要立刻隐藏。
  syncModeFields();
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
    // 运行方式与网关种类也由模板带出来。原生型模板没有 command，全靠 mode+kind
    // 让后端知道「装配哪个内置实现」，而不是去拉起一个不存在的可执行文件。
    managedMode = p.mode || '';
    gatewayKind = p.kind || '';
    $('fCmd').value = p.command || '';
    $('fArgs').value = (p.args || []).join('\n');
    $('fPortEnv').value = p.port_env_var || '';
    $('fHealthM').value = p.health_path || '';
    $('fPrefix').value = p.model_prefix || '';
    if (!$('fName').value.trim()) $('fName').value = p.label || '';
    $('keyHint2').textContent = p.route_key_hint ? 'Key 来源：' + p.route_key_hint : '';
    // 积分平台预填官方折算价（如 WorkBuddy 0.05 元/积分），用户可改
    $('fCreditValue').value = p.credit_value > 0 ? p.credit_value : '';
    $('presetHint').textContent = [p.note, p.doc_url ? '项目：' + p.doc_url : ''].filter(Boolean).join(' · ');
    syncModeFields();
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
  $('chFormTitle').textContent = raw ? '编辑渠道 · ' + (raw.display_name || raw.name) : '添加渠道';
  $('formMsg').textContent = '';
  $('testBox').hidden = true;
  $('btnForceSave').hidden = true;
  $('chips').innerHTML = '';
  $('fetchHint').textContent = '—';
  Store.set({ fetched: [] });
  document.querySelectorAll('#chModal .field.err').forEach((f) => f.classList.remove('err'));

  setKind(raw ? (raw.kind || (raw.command ? 'managed' : 'embedded')) : (forceKind || 'embedded'));
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
    $('fCmd').value = raw.command || '';
    $('fArgs').value = (raw.args || []).join('\n');
    $('fDir').value = raw.dir || '';
    $('fDataDir').value = raw.data_dir || '';
    $('fPortEnv').value = raw.port_env_var || '';
    $('fFixedPort').value = raw.fixed_port || 0;
    $('fCreditValue').value = raw.credit_value > 0 ? raw.credit_value : '';
    $('fHealthM').value = raw.health_path || '';
    $('fReady').value = raw.ready_timeout || '';
    $('fPanelAPI').value = raw.panel_api_prefix || '/panel/api';
    $('fPathPrefix').value = raw.path_prefix == null ? '/v1' : raw.path_prefix;
    $('fEnv').value = envToText(raw.env);
    $('fExpose').checked = raw.expose !== false;
    if (raw.preset) $('fPreset').value = raw.preset;
    // 回填已识别的网关种类：保存时带回去。不回填的话，任何一次编辑都会把
    // kind 清空，平台唯一性校验随即失效（用户改个显示名就能绕过）。
    gatewayKind = raw.gateway_kind || '';
    // 运行方式同样必须回显。不回显的话，任何一次编辑都会把原生渠道变回
    // 「去拉起一个空 command」的子进程，渠道再也起不来。
    managedMode = raw.mode || '';
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
      'fHeaders', 'fTimeout', 'fProxy', 'fCmd', 'fArgs', 'fDir', 'fDataDir', 'fPortEnv',
      'fHealthM', 'fReady', 'fEnv', 'fPriceIn', 'fPriceCached', 'fPriceOut']
      .forEach((id) => { $(id).value = ''; });
    $('fProtocol').value = 'chat';
    $('fWeight').value = 1;
    $('fPriority').value = 0;
    $('fRetries').value = 1;
    $('fFixedPort').value = 0;
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
  // 只有新建才清空「网关种类 / 运行方式」——它们对新建而言尚未确定。
  // 这一对重置以前是无条件执行的（放错位置），会把编辑时的回显值一并清掉：
  // 保存后 kind 丢失（平台唯一性校验失效），原生渠道的 mode 丢失（退化成子进程）。
  if (!raw) {
    gatewayKind = '';
    managedMode = '';
  }
  syncModeFields();
  mountChannelForm();
}

// gatewayKind 当前表单识别出的网关种类（'' = 未知/不适用）。
// 编辑既有渠道时由 openChannelForm 从后端回显值填入；新建时为空，
// 由后端 kindOfUpstream 按名称推断后落盘。
let gatewayKind = '';

// managedMode 当前托管型渠道的运行方式：'' / 'process' = 独立子进程；
// 'native' = 进程内原生（ModelMux 自己装配上游，不拉起任何可执行文件）。
// 与 gatewayKind 一样：编辑时由 openChannelForm 回显，选模板时由 applyPreset 预填。
let managedMode = '';

// syncModeFields 按运行方式显隐「只有子进程才需要」的表单项。
//
// 原生型跑在 ModelMux 进程内：没有可执行文件，也没有端口环境变量可供注入，
// 把「启动命令（标着必填）」「端口环境变量」「固定端口」留在原生渠道的表单里，
// 用户会以为漏填了什么；而这些字段真填了，后端反而会把整个渠道禁用
// （见 config 的 normalize：原生型带 command 直接禁用）。宁可不显示。
function syncModeFields() {
  const native = formKind === 'managed' && managedMode === 'native';
  document.querySelectorAll('#chModal [data-mode]').forEach((el) => {
    el.hidden = native && el.dataset.mode === 'process';
  });
}

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
      // mode 决定后端是拉起子进程还是装配进程内实现；空值 = 子进程。
      // 新建托管渠道时表单默认子进程，选到内置原生模板才会变成 native。
      mode: managedMode,
      command: $('fCmd').value.trim(),
      args: lines($('fArgs').value),
      dir: $('fDir').value.trim(),
      data_dir: $('fDataDir').value.trim(),
      port_env_var: $('fPortEnv').value.trim(),
      fixed_port: intOf($('fFixedPort').value, 0),
      credit_value: floatOf($('fCreditValue').value) || 0,
      health_path: $('fHealthM').value.trim(),
      ready_timeout: $('fReady').value.trim(),
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
    unmountChannelForm();
    await loadChannels();
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

/* ── ModelMux 日志 ────────────────────────────────────── */

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
  $('btnAddApi').onclick = () => openChannelForm(null, 'embedded');
  $('btnAddAcct').onclick = () => openChannelForm(null, 'managed');

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
  ['chModal', 'trModal', 'addModal', 'zcAccModal', 'zcClaimModal', 'zcImportModal', 'zcSetModal'].forEach((id) => {
    $(id).addEventListener('click', (ev) => { if (ev.target === $(id)) $(id).hidden = true; });
  });
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    ['chModal', 'trModal', 'addModal', 'drawer', 'zcAccModal', 'zcClaimModal', 'zcImportModal', 'zcSetModal']
      .forEach((id) => { $(id).hidden = true; });
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
