/* zcode.js zcode2api 控制台：账号池 / 运行监控 / 网关设置。
 *
 * 为什么是原生视图而不是 iframe：
 *   1. ModelMux 首页的 CSP 是 `default-src 'none'`，没有 `frame-src`——
 *      iframe 会被浏览器直接拦掉，根本不会加载；
 *   2. 上游面板（zcode2api）自带前端 localStorage 登录门（auth.js 把后台
 *      密钥存在浏览器里），即使能嵌也过不了鉴权，除非把密码交给浏览器。
 * 所以这里改成「原生视图 + 由 ModelMux 服务端代理上游管理 API」：
 * 后端把 zcode 系列的请求转到上游 `<root>/admin/api/<path>` 并注入后台密码，
 * 浏览器全程不接触密码——用户「查看不需要输入面板密码」是天然成立的，
 * 而不是靠前端绕过登录门。代理只开放 GET，写请求会返回 405，本文件也只用 GET。
 *
 * 取数沿用 upstream.js 的约定：UP.chan 是当前渠道；requireChan() 前置校验；
 * upApi(path) 走 /api/channels/<chan>/upstream/<path>；格式化尽量复用
 * upstream.js / app.js 的全局函数（num / fmtTok / trimFixed / card / esc 等）。
 */

const ZC = { status: null, accounts: null, monitoring: null, settings: null };

/* ── 本地格式化 ─────────────────────────────────────────
 * 上游的时间字段（expires_at / starts_at / ends_at / effective_at / ts）都是
 * Unix 秒；upstream.js 的 when()/dayOf() 收的是毫秒，口径不同，容易差 1000 倍，
 * 所以这里就地把「秒」换算成本地时间，不借用那两个函数。
 */

function zcPad(n) { return String(n).padStart(2, '0'); }

/* zcWhen 秒 → `MM-DD HH:MM`（本地时区）。 */
function zcWhen(sec) {
  const n = Number(sec);
  if (!Number.isFinite(n) || n <= 0) return '—';
  const d = new Date(n * 1000);
  if (isNaN(d.getTime())) return '—';
  return `${zcPad(d.getMonth() + 1)}-${zcPad(d.getDate())} ${zcPad(d.getHours())}:${zcPad(d.getMinutes())}`;
}

/* zcDay 秒 → `YYYY-MM-DD`（本地时区）。 */
function zcDay(sec) {
  const n = Number(sec);
  if (!Number.isFinite(n) || n <= 0) return '—';
  const d = new Date(n * 1000);
  if (isNaN(d.getTime())) return '—';
  return `${d.getFullYear()}-${zcPad(d.getMonth() + 1)}-${zcPad(d.getDate())}`;
}

/* zcSec 秒 → 人类可读时长（首字/总耗时，上游 monitoring 用秒）。 */
function zcSec(v) {
  const n = Number(v);
  if (v == null || !Number.isFinite(n)) return '—';
  return n < 1 ? Math.round(n * 1000) + 'ms' : n.toFixed(2) + 's';
}

/* zcCard 与 upstream.js 的 card() 同形，但多一个 k-sub 说明行。
   card() 只吃「标签 + 数值」，这里块的副标题（提供方列表、额度池口径）要单独放。 */
function zcCard(label, value, cls, sub) {
  return `<div class="card-k"><b class="${cls || ''}">${esc(String(value))}</b>`
    + `<span>${esc(label)}</span>`
    + (sub ? `<span class="k-sub">${esc(sub)}</span>` : '') + '</div>';
}

/* zcKV 只读描述行，复用设置的 .ep / .ep-row / .ep-k 结构。 */
function zcKV(k, valHtml, title) {
  return `<div class="ep-row"><span class="ep-k">${esc(k)}</span>`
    + `<span class="value"${title ? ` title="${esc(title)}"` : ''}>${valHtml}</span></div>`;
}

/* ── 账号池 ───────────────────────────────────────────── */

const ZC_STATUS = { active: '正常', exhausted: '用完', cooling: '限流', invalid: '异常', disabled: '禁用' };
const ZC_STATUS_CLS = { active: 'ok', exhausted: 'warn', cooling: 'warn', invalid: 'err', disabled: 'muted' };

/* zcPlans 上游既可能给全量 plans 数组，旧数据只有单个 plan 对象。 */
function zcPlans(a) {
  if (Array.isArray(a.plans) && a.plans.length) return a.plans;
  const p = a.plan;
  return (p && Object.keys(p).length) ? [p] : [];
}

/* zcQuotaTip 全部额度窗口的悬停明细（多模型时全塞进 tooltip，行内只放主要一条）。 */
function zcQuotaTip(q) {
  const keys = Object.keys(q || {});
  if (!keys.length) return '上游未返回额度窗口';
  return '额度窗口：\n' + keys.map((k) => {
    const w = q[k] || {};
    return `  ${k}：剩余 ${num(w.remaining)} / 总额 ${num(w.total)}`
      + `（已用 ${num(w.used)}）` + (w.expires_at ? `，至 ${zcDay(w.expires_at)}` : '');
  }).join('\n');
}

/* zcQuotaCell 额度列：取总额度最大的一条画进度条，其余进 tooltip。
   进度条用 used/total（上游口径是「用掉多少」），括号里补 remaining。 */
function zcQuotaCell(a) {
  const q = a.quota || {};
  const keys = Object.keys(q);
  if (!keys.length) return '<td class="credits"><span class="dim">—</span></td>';
  let mainKey = keys[0];
  keys.forEach((k) => {
    if ((Number(q[k].total) || 0) > (Number(q[mainKey].total) || 0)) mainKey = k;
  });
  const w = q[mainKey] || {};
  const total = Number(w.total) || 0;
  const used = Number(w.used) || 0;
  const remaining = Number(w.remaining) || 0;
  const pct = total > 0 ? Math.max(0, Math.min(100, Math.round(used / total * 100))) : 0;
  const more = keys.length > 1 ? ` <span class="dim">共 ${keys.length} 个</span>` : '';
  const exp = w.expires_at ? `<span class="dim"> · 至 ${zcDay(w.expires_at)}</span>` : '';
  return `<td class="credits" title="${esc(zcQuotaTip(q))}">
    <div class="zc-qn">${esc(mainKey)}${more}</div>
    <div class="zbar"><i data-style="width:${pct}%"></i></div>
    <div class="sub">剩余 ${num(remaining)} / ${num(total)}（${pct}%）${exp}</div>
  </td>`;
}

/* zcPlanCell 套餐列：取最靠前的一个 plan 显示名称，条数放小字。 */
function zcPlanCell(a) {
  const plans = zcPlans(a);
  if (!plans.length) return '<td><span class="dim">—</span></td>';
  const p = plans[0] || {};
  const name = p.name || p.plan_id || p.show_name || '—';
  const desc = p.description ? `<div class="sub">${esc(p.description)}</div>` : '';
  return `<td><div class="nm">${esc(name)}</div>`
    + (plans.length > 1 ? `<div class="sub">共 ${plans.length} 个套餐</div>` : '')
    + desc + '</td>';
}

/* zcEndsCell 到期时间列：取所有套餐里最晚的 ends_at；已过期要一眼看得出。 */
function zcEndsCell(a) {
  const plans = zcPlans(a);
  const ends = Math.max(0, ...plans.map((p) => Number(p.ends_at) || Number(p.expires_at) || 0));
  if (!ends) return '<td><span class="dim">—</span></td>';
  const past = ends * 1000 < Date.now();
  const soon = !past && ends * 1000 - Date.now() < 7 * 86400000;
  const badge = past ? ' <span class="badge err">已过期</span>'
    : (soon ? ' <span class="badge warn">即将到期</span>' : '');
  return `<td class="sub"><span class="${soon ? 'warn-txt' : ''}">${zcDay(ends)}</span>${badge}</td>`;
}

function zcAccRow(a) {
  const st = a.status || '';
  const badge = `<span class="badge ${ZC_STATUS_CLS[st] || 'muted'}">${esc(ZC_STATUS[st] || st || '未知')}</span>`;
  const err = a.last_error
    ? `<div class="sub err-text" title="${esc(a.last_error)}">${esc(String(a.last_error).slice(0, 30))}</div>` : '';
  const on = a.enabled !== false && st !== 'disabled';
  return `<tr>
    <td><div class="nm">${esc(a.name || '未命名')}</div>
        <div class="sub mono">${esc(a.provider || '—')} / ${a.mode === 'jwt' ? 'JWT' : 'API Key'}
          · ${esc(a.token_masked || '')}</div></td>
    <td>${badge}${err}</td>
    <td>${on ? '<span class="badge ok">启用</span>' : '<span class="badge muted">停用</span>'}</td>
    ${zcQuotaCell(a)}
    ${zcPlanCell(a)}
    ${zcEndsCell(a)}
  </tr>`;
}

function renderZcAccCards(st, accts) {
  const enabled = accts.filter((a) => a.enabled !== false && a.status !== 'disabled').length;
  const cards = [zcCard('账号总数', accts.length), zcCard('启用', enabled, 'ok')];
  const pool = (st && st.quota_pool) || {};
  Object.keys(pool).forEach((p) => cards.push(zcCard('额度池 · ' + p, pool[p])));
  if (st && Array.isArray(st.providers) && st.providers.length) {
    cards.push(zcCard('提供方', st.providers.length, '', st.providers.join(' / ')));
  }
  $('zcAccCards').innerHTML = cards.join('');
  // data-style 要在 innerHTML 之后写进 CSSOM 才生效（CSP 无 'unsafe-inline'，内联 style 会被丢弃）。
  // paintStyles 由 app.js 定义；加 typeof 保护，避免脚本加载顺序被后人调整后静默失效。
  if (typeof paintStyles === 'function') paintStyles($('zcAccCards'));
}

async function loadZcAccounts() {
  if (!requireChan()) return;
  const body = $('zcAccBody');
  body.innerHTML = '<tr><td colspan="6" class="sub">加载中…</td></tr>';
  $('zcAccEmpty').hidden = true;
  try {
    // status 顺带取回额度池；失败不拖垮账号表，所以单独 catch
    const [st, d] = await Promise.all([
      upApi('status').catch(() => null),
      upApi('accounts'),
    ]);
    const accts = d.accounts || [];
    ZC.status = st;
    ZC.accounts = accts;
    renderZcAccCards(st, accts);
    const stats = d.stats || {};
    $('zcAccHint').textContent = `${accts.length} 个账号`
      + (stats.active != null ? ` · 正常 ${stats.active}` : '')
      + (stats.exhausted != null ? ` · 用完 ${stats.exhausted}` : '');
    body.innerHTML = accts.map(zcAccRow).join('');
    // data-style 要在 innerHTML 之后写进 CSSOM 才生效（CSP 无 'unsafe-inline'，内联 style 会被丢弃）。
    // paintStyles 由 app.js 定义；加 typeof 保护，避免脚本加载顺序被后人调整后静默失效。
    if (typeof paintStyles === 'function') paintStyles(body);
    $('zcAccEmpty').hidden = accts.length > 0;
    $('zcAccEmpty').textContent = '账号为空，去网关自己的面板添加账号';
  } catch (e) {
    body.innerHTML = '';
    $('zcAccEmpty').hidden = false;
    $('zcAccEmpty').textContent = '加载失败：' + e.message;
  }
}

/* ── 运行监控 ─────────────────────────────────────────── */

/* zcMonRow monitoring.entries 一行。
   字段取自上游 app/reqlog.py：ts / account / mode / model / endpoint / stream /
   ok(三态 None=在途) / status / error / t_first / t_total / input_tokens /
   output_tokens / preview。 */
function zcMonRow(e) {
  const ok = e.ok;
  const cls = ok === true ? 'ok' : (ok === false ? 'err' : 'warn');
  const label = ok === true ? '成功'
    : (ok === null || ok === undefined) ? '进行中'
      : (e.status === 499 ? '客户端断开' : '失败' + (e.status ? ' · HTTP ' + e.status : ''));
  const modeTag = e.mode ? ` <span class="badge muted">${e.mode === 'jwt' ? 'JWT' : 'KEY'}</span>` : '';
  const content = ok === false && e.error
    ? `<span class="err-text" title="${esc(e.error)}">${esc(e.error)}</span>`
    : esc(e.preview || '—');
  const stream = e.stream ? ' <span class="badge muted">流</span>' : '';
  return `<tr>
    <td class="sub">${zcWhen(e.ts)}</td>
    <td><div class="nm">${esc(e.account || '—')}${modeTag}</div></td>
    <td class="sub" title="${esc(e.model || '')}">${esc(e.model || '—')}</td>
    <td class="sub mono">${esc(e.endpoint || '—')}</td>
    <td><span class="badge ${cls}" title="${esc(e.error || '')}">${esc(label)}</span></td>
    <td class="num">${zcSec(e.t_first)}</td>
    <td class="num">${zcSec(e.t_total)}</td>
    <td class="num">${fmtTok(e.input_tokens)}</td>
    <td class="num">${fmtTok(e.output_tokens)}</td>
    <td class="sub">${content}${stream}</td>
  </tr>`;
}

function renderZcMonCards(st, entries) {
  const cards = [];
  // 块一：提供方
  const providers = (st && st.providers) || [];
  cards.push(zcCard('提供方', providers.length, '',
    providers.length ? providers.join(' / ') : '上游未声明'));
  // 块二：网关密钥
  const gset = !!(st && st.gateway_key_set);
  cards.push(zcCard('网关 API Key', gset ? '已设置' : '未设置', gset ? 'ok' : 'warn',
    gset ? '调用 /v1 需携带密钥' : '网关当前不校验调用方密钥'));
  // 块三：额度池
  const pool = (st && st.quota_pool) || {};
  Object.keys(pool).forEach((p) => cards.push(zcCard('额度池 · ' + p, pool[p])));

  // 请求观测汇总，口径与上游监控页一致
  const total = entries.length;
  const succ = entries.filter((e) => e.ok === true).length;
  const fail = entries.filter((e) => e.ok === false).length;
  const inflight = entries.filter((e) => e.ok === null || e.ok === undefined).length;
  const tok = entries.reduce((x, e) => x + (Number(e.input_tokens) || 0) + (Number(e.output_tokens) || 0), 0);
  const rates = entries.filter((e) => e.t_first != null).map((e) => Number(e.t_first));
  const ttfb = rates.length ? rates.reduce((a, b) => a + b, 0) / rates.length : null;
  cards.push(zcCard('调用', total, '', `进行中 ${inflight}`));
  cards.push(zcCard('成功率', (succ + fail) ? (succ / (succ + fail) * 100).toFixed(1) + '%' : '—', 'ok', `成功 ${succ}`));
  cards.push(zcCard('失败', fail, fail ? 'err' : ''));
  cards.push(zcCard('Token 总量', (Number.isFinite(tok) && tok) ? fmtTok(tok) : '—', 'accent'));
  cards.push(zcCard('平均首字', ttfb != null ? zcSec(ttfb) : '—', '', rates.length ? `样本 ${rates.length}` : ''));
  $('zcMonCards').innerHTML = cards.join('');
  // data-style 要在 innerHTML 之后写进 CSSOM 才生效（CSP 无 'unsafe-inline'，内联 style 会被丢弃）。
  // paintStyles 由 app.js 定义；加 typeof 保护，避免脚本加载顺序被后人调整后静默失效。
  if (typeof paintStyles === 'function') paintStyles($('zcMonCards'));
}

async function loadZcMonitor() {
  if (!requireChan()) return;
  const body = $('zcMonBody');
  body.innerHTML = '<tr><td colspan="10" class="sub">加载中…</td></tr>';
  $('zcMonEmpty').hidden = true;
  try {
    const [st, d] = await Promise.all([
      upApi('status').catch(() => null),
      upApi('monitoring'),
    ]);
    const entries = (d && d.entries) || [];
    const keep = (d && d.keep) || 0;
    ZC.status = st;
    ZC.monitoring = d;
    renderZcMonCards(st, entries);
    $('zcMonHint').textContent = '网关请求实时观测 · 内存环形日志，重启清零';
    $('zcMonReqHint').textContent = `${entries.length} 条` + (keep ? ` / 上限 ${keep}` : '');
    body.innerHTML = entries.map(zcMonRow).join('');
    // data-style 要在 innerHTML 之后写进 CSSOM 才生效（CSP 无 'unsafe-inline'，内联 style 会被丢弃）。
    // paintStyles 由 app.js 定义；加 typeof 保护，避免脚本加载顺序被后人调整后静默失效。
    if (typeof paintStyles === 'function') paintStyles(body);
    $('zcMonEmpty').hidden = entries.length > 0;
    $('zcMonEmpty').textContent = '最近没有请求记录（网关内的环形日志为空）';
  } catch (e) {
    body.innerHTML = '';
    $('zcMonEmpty').hidden = false;
    $('zcMonEmpty').textContent = '加载失败：' + e.message;
  }
}

/* ── 网关设置（只读） ─────────────────────────────────── */

/* zcSecMasked 掩码密钥的呈现：明确标注「这是掩码，不是明文」。
   上游 _mask_secret 只在长度 > 8 时给 `前4…后4`，否则整体 `••••`。 */
function zcMasked(set, masked) {
  if (!set) return '<span class="dim">未设置</span>';
  const m = masked || '';
  if (!m) return '<span class="dim">已设置（无掩码）</span>';
  return `<span class="mono">${esc(m)}</span> <span class="badge muted">掩码值</span>`;
}

function renderZcSettings(d) {
  const rows = [
    zcKV('后台密码', zcMasked(d.admin_key_set, d.admin_key_masked)
      + (d.admin_key_is_default ? ' <span class="badge warn">仍是默认口令</span>' : ''),
      '掩码显示，浏览器不接触明文密码'),
    zcKV('网关 API Key', zcMasked(d.gateway_key_set, d.gateway_key_masked),
      '掩码显示，浏览器不接触明文密钥'),
    zcKV('额度刷新间隔', `${esc(String(d.quota_refresh_interval ?? '—'))} 秒`,
      '后台自动刷新账号额度的周期，0 = 关闭'),
    zcKV('单账号并发上限', esc(String(d.account_concurrency ?? '—')),
      '每个账号同时处理的最大请求数，0 = 不限'),
    zcKV('套餐自动领取轮', `${esc(String(d.claim_round_interval ?? '—'))} 秒`,
      '周期对全部 JWT 账号执行活动套餐领取，0 = 关闭'),
  ];
  $('zcSetBody').innerHTML = rows.join('');
  // data-style 要在 innerHTML 之后写进 CSSOM 才生效（CSP 无 'unsafe-inline'，内联 style 会被丢弃）。
  // paintStyles 由 app.js 定义；加 typeof 保护，避免脚本加载顺序被后人调整后静默失效。
  if (typeof paintStyles === 'function') paintStyles($('zcSetBody'));
}

async function loadZcSettings() {
  if (!requireChan()) return;
  $('zcSetBody').innerHTML = '<div class="ep-row"><span class="ep-k">加载中…</span></div>';
  try {
    const d = await upApi('settings');
    ZC.settings = d;
    renderZcSettings(d);
    $('zcSetHint').textContent = '只读视图 · 修改设置请去网关自己的面板';
  } catch (e) {
    $('zcSetBody').innerHTML = `<div class="warn-line">加载失败：${esc(e.message)}</div>`;
    $('zcSetHint').textContent = '只读视图 · 修改设置请去网关自己的面板';
  }
}

/* ── 视图加载分发（由 app.js 的 setView 钩子调用） ─────── */

const ZCODE_LOADERS = {
  'up-zc-accounts': loadZcAccounts,
  'up-zc-monitor': loadZcMonitor,
  'up-zc-settings': loadZcSettings,
};

function onZcodeView(view) {
  const fn = ZCODE_LOADERS[view];
  if (fn) fn();
}

/* 视图内按钮：脚本在 body 末尾加载，DOM 已就绪，直接绑。
   用 $() 取元素并判空，避免别的页面片段缺失时报错拖垮整个脚本。 */
(function bindZcode() {
  const on = (id, fn) => { const el = $(id); if (el) el.onclick = fn; };
  on('btnZcAccReload', () => loadZcAccounts());
  on('btnZcAccRaw', () => showDrawer('账号原始返回', ZC.accounts, ZC.accounts));
  on('btnZcMonReload', () => loadZcMonitor());
  on('btnZcMonRaw', () => showDrawer('运行监控原始返回', ZC.monitoring, ZC.monitoring));
  on('btnZcSetReload', () => loadZcSettings());
  on('btnZcSetRaw', () => showDrawer('网关设置原始返回', ZC.settings, ZC.settings));
})();
