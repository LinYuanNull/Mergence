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
 * 而不是靠前端绕过登录门。
 *
 * 写操作走同一条代理（GET/POST/PUT/DELETE 全开放，见 src/internal/web/proxy.go）：
 *   账号池   POST /accounts · PUT/DELETE /accounts[/{id}] · /enabled · /refresh
 *            · /fingerprint/rotate · GET /export · POST /import
 *   领取     GET /claim/preview · POST /claim
 *   登录     POST /login/start · GET /login/poll/{flow_id}
 * 密钥注入与只读时完全一样——打开写口子没有把密码挪到浏览器里。
 *
 * 取数沿用 upstream.js 的约定：UP.chan 是当前渠道；requireChan() 前置校验；
 * upApi(path) 走 /api/channels/<chan>/upstream/<path>；格式化尽量复用
 * upstream.js / app.js 的全局函数（num / fmtTok / trimFixed / card / esc 等）。
 *
 * 超时必须显式给：upstream.js 的 upApi 默认 30s，而领取要在上游解人机验证
 * （可长达数十秒），导入要落盘+派生任务。所以写操作都带自己的 timeout，
 * 与其在服务端的 upstreamProxyWriteTimeout / upstreamProxyClaimTimeout 对齐。
 */

const ZC = {
  status: null,      // GET /status 缓存（额度池、提供方、网关密钥态）
  accounts: null,    // GET /accounts 的 accounts 数组
  accountsRaw: null, // GET /accounts 的完整响应，供「原始返回」抽屉用
  stats: null,       // GET /accounts 的 stats
  providers: [],     // 上游声明的提供方，填「新增账号」的下拉
  monitoring: null,  // GET /monitoring 缓存
  settings: null,    // GET /settings 缓存
  editId: null,      // 编辑态账号 id；null 表示新增
  preview: null,     // GET /claim/preview 结果；null = 还没查过
  login: { flowId: '', url: '', timer: null, tries: 0 },
};

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
  const id = esc(a.id || '');
  // 行内操作：4 个足够。指纹换发放在编辑弹窗里——它属于账号级维护动作，
  // 挂在表格行上会让这个本来就 6 列的表更难读。
  const ops = [
    `<button class="ghost" data-zop="refresh" data-zid="${id}">刷新</button>`,
    `<button class="ghost" data-zop="toggle" data-zid="${id}" data-zon="${on ? '0' : '1'}">${on ? '停用' : '启用'}</button>`,
    `<button class="ghost" data-zop="edit" data-zid="${id}">编辑</button>`,
    `<button class="ghost danger" data-zop="remove" data-zid="${id}">删除</button>`,
  ].join('');
  return `<tr>
    <td><div class="nm">${esc(a.name || '未命名')}</div>
        <div class="sub mono">${esc(a.provider || '—')} / ${a.mode === 'jwt' ? 'JWT' : 'API Key'}
          · ${esc(a.token_masked || '')}</div></td>
    <td>${badge}${err}</td>
    <td>${on ? '<span class="badge ok">启用</span>' : '<span class="badge muted">停用</span>'}</td>
    ${zcQuotaCell(a)}
    ${zcPlanCell(a)}
    ${zcEndsCell(a)}
    <td class="c-ops zc-ops">${ops}</td>
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
  body.innerHTML = '<tr><td colspan="7" class="sub">加载中…</td></tr>';
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
    ZC.accountsRaw = d;
    ZC.stats = d.stats || {};
    ZC.providers = d.providers || [];
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
    $('zcAccEmpty').textContent = '账号池是空的，点右上角「添加账号」从上游加一个';
  } catch (e) {
    body.innerHTML = '';
    $('zcAccEmpty').hidden = false;
    $('zcAccEmpty').textContent = '加载失败：' + e.message;
  }
}

/* ── 账号池：写操作 ───────────────────────────────────────
 *
 * 全部经服务端代理转发，浏览器不接触后台密码（打开写口子没把密码挪到前端）。
 *
 * 每个写操作完成后都重新拉一次账号列表：上游会顺带刷新额度、状态和 id
 * （新增账号的 id 由上游生成，额度是拉完才有的），本地拼的乐观更新必然对不上。
 */

/* zcBusy 让按钮进入忙碌态。
   写操作可能要等几十秒（领取最久），不置灰用户会反复点 → 重复提交。 */
async function zcBusy(btn, label, fn) {
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; if (label) btn.textContent = label; }
  try {
    return await fn();
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

/* zcMsg 弹窗底部的提示行：成功用 fhint、失败加 err-text。 */
function zcMsg(id, text, isErr) {
  const el = $(id);
  if (!el) return;
  el.textContent = text || '';
  el.className = isErr ? 'fhint err-text' : 'fhint';
}

/* ── 新增 / 编辑账号 ───────────────────────────────────── */

/* zcFillProviders 用上游声明的提供方填下拉。
   不写死成 ['zai']——那是上游自己的常量（PROVIDERS），
   换一版网关多一个提供方时不该回来改前端。 */
function zcFillProviders(cur) {
  const sel = $('zcAccProvider');
  if (!sel) return;
  const list = (ZC.providers && ZC.providers.length) ? ZC.providers : ['zai'];
  sel.innerHTML = list.map((p) => `<option value="${esc(p)}">${esc(p)}</option>`).join('');
  if (cur && list.indexOf(cur) >= 0) sel.value = cur;
}

function zcSetAccTab(name) {
  document.querySelectorAll('#zcAccTabs .seg-b')
    .forEach((b) => b.classList.toggle('on', b.dataset.zt === name));
  $('zcTabPaste').hidden = name !== 'paste';
  $('zcTabLogin').hidden = name !== 'login';
}

function zcStopLogin() {
  if (ZC.login.timer) { clearTimeout(ZC.login.timer); ZC.login.timer = null; }
}

/* zcEditMode 切换「新增 / 编辑」两种形态。
   编辑态：没有页签（不做登录）、隐藏批量粘贴框、露出「新的 Token」与「换发指纹」、
   提供方不可改（上游 PUT 只认 name/token，改了也不生效——不如直接禁掉）。 */
function zcEditMode(on) {
  $('zcAccTabs').hidden = on;
  $('zcAccNewTokField').hidden = !on;
  $('btnZcAccFp').hidden = !on;
  $('zcAccProvider').disabled = !!on;
  if (on) { $('zcTabPaste').hidden = true; $('zcTabLogin').hidden = true; }
}

function zcOpenAdd() {
  if (!requireChan()) return;
  zcStopLogin();
  ZC.editId = null;
  ZC.login = { flowId: '', url: '', timer: null, tries: 0 };
  $('zcAccTitle').textContent = '添加账号';
  $('zcAccName').value = '';
  $('zcAccName').placeholder = '留空按「提供方-序号」自动命名';
  zcFillProviders('');
  $('zcAccTokens').value = '';
  $('zcAccNewToken').value = '';
  $('zcAccTokHint').textContent =
    '三段点分的 JWT 走 Coding Plan 通道；其余按 API Key 处理。重复的 token 由上游自动跳过。';
  $('zcLoginUrl').value = '';
  $('zcLoginState').textContent = '未开始';
  $('zcLoginState').className = 'login-state';
  $('btnZcLoginCopy').disabled = true;
  $('btnZcLoginOpen').disabled = true;
  $('btnZcAccSave').textContent = '添加';
  zcMsg('zcAccMsg', '');
  zcEditMode(false);
  zcSetAccTab('paste');
  $('zcAccModal').hidden = false;
  $('zcAccTokens').focus();
}

function zcOpenEdit(id) {
  const a = (ZC.accounts || []).find((x) => x.id === id);
  if (!a) { toast('账号已不在列表里，请先刷新', 'err'); return; }
  zcStopLogin();
  ZC.editId = id;
  ZC.login = { flowId: '', url: '', timer: null, tries: 0 };
  $('zcAccTitle').textContent = '编辑账号';
  $('zcAccName').value = a.name || '';
  $('zcAccName').placeholder = '';
  zcFillProviders(a.provider || '');
  $('zcAccNewToken').value = '';
  $('zcAccNewToken').placeholder = '粘贴新的 JWT 或 API Key；留空则不改';
  $('zcAccTokens').value = '';
  $('zcAccTokHint').textContent = '';
  $('btnZcAccSave').textContent = '保存';
  zcMsg('zcAccMsg', '');
  zcEditMode(true);
  $('zcAccModal').hidden = false;
  $('zcAccName').focus();
}

async function zcSaveAccount(btn) {
  const name = $('zcAccName').value.trim();

  if (ZC.editId) {
    const body = {};
    if (name) body.name = name;
    const tok = $('zcAccNewToken').value.trim();
    if (tok) body.token = tok;
    if (!Object.keys(body).length) { zcMsg('zcAccMsg', '没有要保存的改动', true); return; }
    await zcBusy(btn, '保存中…', async () => {
      try {
        await upPut('accounts/' + encodeURIComponent(ZC.editId), body, 120000);
        toast('已保存', 'ok');
        $('zcAccModal').hidden = true;
        await loadZcAccounts();
      } catch (e) { zcMsg('zcAccMsg', '保存失败：' + e.message, true); }
    });
    return;
  }

  const provider = $('zcAccProvider').value;
  if (!provider) { zcMsg('zcAccMsg', '请选择提供方', true); return; }
  const tokens = $('zcAccTokens').value.split('\n').map((s) => s.trim()).filter(Boolean);
  if (!tokens.length) { zcMsg('zcAccMsg', '请至少粘贴一个 Token / API Key', true); return; }
  // 批量时不给名字：上游会把同一个 name 套用到每一个，反而分不清谁是谁。
  // 单个才用用户填的名字。
  const payload = { provider, tokens };
  if (name && tokens.length === 1) payload.name = name;

  await zcBusy(btn, '添加中…', async () => {
    try {
      const d = await upPost('accounts', payload, 180000);
      const n = d.count != null ? d.count : tokens.length;
      toast(`已提交 ${n} 个账号`, 'ok');
      $('zcAccModal').hidden = true;
      await loadZcAccounts();
    } catch (e) { zcMsg('zcAccMsg', '添加失败：' + e.message, true); }
  });
}

/* ── 启停 / 刷新 / 指纹 / 删除 ─────────────────────────── */

async function zcToggleEnabled(id, on, btn) {
  await zcBusy(btn, '…', async () => {
    try {
      await upPost('accounts/' + encodeURIComponent(id) + '/enabled', { enabled: on }, 60000);
      toast(on ? '已启用' : '已停用', 'ok');
      await loadZcAccounts();
    } catch (e) { toast('操作失败：' + e.message, 'err'); }
  });
}

async function zcRefreshOne(id, btn) {
  await zcBusy(btn, '…', async () => {
    try {
      const d = await upPost('accounts/' + encodeURIComponent(id) + '/refresh', {}, 120000);
      // 上游对非 JWT 账号返回 ok:false + message（不是 HTTP 错误），要照实说
      if (d && d.ok === false) toast(d.message || '该账号不支持额度刷新', 'err');
      else toast('额度已刷新', 'ok');
      await loadZcAccounts();
    } catch (e) { toast('刷新失败：' + e.message, 'err'); }
  });
}

async function zcRefreshAll(btn) {
  await zcBusy(btn, '刷新中…', async () => {
    try {
      const d = await upPost('accounts/refresh', { all: true }, 180000);
      const bits = ['刷新 ' + (d.count != null ? d.count : '—') + ' 个'];
      if (d.skipped_cooling) bits.push('跳过冷却 ' + d.skipped_cooling);
      if (d.skipped_invalid) bits.push('跳过失效 ' + d.skipped_invalid);
      // 结果只走 toast：提示行是「N 个账号 · 正常 X · 用完 Y」的稳态摘要，
      // 而紧随其后的 loadZcAccounts() 一定会把它改回摘要——写在那里等于当场被抹掉，
      // 用户永远看不到刷新计数。
      toast('刷新完成：' + bits.join(' · '), 'ok');
      await loadZcAccounts();
    } catch (e) { toast('刷新失败：' + e.message, 'err'); }
  });
}

async function zcRotateFp(btn) {
  if (!ZC.editId) return;
  await zcBusy(btn, '换发中…', async () => {
    try {
      const d = await upPost('accounts/' + encodeURIComponent(ZC.editId) + '/fingerprint/rotate', {}, 120000);
      const fp = (d && d.fingerprint) || {};
      zcMsg('zcAccMsg', '指纹已换发：' + (fp.platform || '—') + ' / '
        + String(fp.device_mid || '').slice(0, 8), false);
      toast('指纹已换发', 'ok');
      await loadZcAccounts();
    } catch (e) { zcMsg('zcAccMsg', '换发失败：' + e.message, true); }
  });
}

async function zcRemove(id) {
  const a = (ZC.accounts || []).find((x) => x.id === id) || {};
  // 删除是不可逆的上游写操作，用 confirm 挡一道（与「删除渠道」同一套路）
  if (!confirm(`删除账号「${a.name || id}」？\n\n该账号会从上游账号池移除。已领取的额度不受影响，但账号本身需要重新添加。`)) return;
  try {
    const d = await upDelete('accounts', [id], 60000);
    // deleted=0 说明上游没找到它（可能已被别处删掉），这不算失败但也不能报成功
    toast(d && d.deleted ? '已删除' : '上游没有删除该账号（可能已不存在）',
      d && d.deleted ? 'ok' : 'err');
    await loadZcAccounts();
  } catch (e) { toast('删除失败：' + e.message, 'err'); }
}

/* ── 导出 / 导入 ───────────────────────────────────────── */

/* zcDownload 用 Blob + 临时 <a download> 触发保存。
   不用 dataURL：它有长度上限，账号多了会被截断成半截 JSON。 */
function zcDownload(filename, text) {
  const blob = new Blob([text], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 4000);
}

async function zcExport(btn) {
  await zcBusy(btn, '导出中…', async () => {
    try {
      const d = await upApi('export', { timeout: 60000 });
      const prov = d.providers || {};
      const n = Object.keys(prov).reduce((x, p) => x + ((prov[p] || []).length), 0);
      // 文件名带时间戳：连着导几次不会互相覆盖
      const stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-');
      zcDownload(`zcode-accounts-${stamp}.json`, JSON.stringify(d, null, 2));
      toast(`已导出 ${n} 个账号`, 'ok');
    } catch (e) { toast('导出失败：' + e.message, 'err'); }
  });
}

function zcOpenImport() {
  if (!requireChan()) return;
  $('zcImportFile').value = '';
  $('zcImportText').value = '';
  zcMsg('zcImportMsg', '');
  $('zcImportModal').hidden = false;
}

async function zcImportFilePicked() {
  const f = $('zcImportFile').files && $('zcImportFile').files[0];
  if (!f) return;
  try {
    $('zcImportText').value = await f.text();
    zcMsg('zcImportMsg', `已读入 ${f.name}（${f.size} 字节）`, false);
  } catch (e) { zcMsg('zcImportMsg', '读取文件失败：' + e.message, true); }
}

async function zcImportDo(btn) {
  const text = $('zcImportText').value.trim();
  if (!text) { zcMsg('zcImportMsg', '请先选择文件或粘贴 JSON', true); return; }
  let payload;
  try { payload = JSON.parse(text); } catch (e) {
    zcMsg('zcImportMsg', 'JSON 解析失败：' + e.message, true); return;
  }
  await zcBusy(btn, '导入中…', async () => {
    try {
      const d = await upPost('import', payload, 180000);
      toast(`已导入 ${d.count != null ? d.count : '—'} 个账号`, 'ok');
      $('zcImportModal').hidden = true;
      await loadZcAccounts();
    } catch (e) { zcMsg('zcImportMsg', '导入失败：' + e.message, true); }
  });
}

/* ── 领取套餐 ───────────────────────────────────────────
 *
 * 上游分两步：preview 拉「现在能领什么」，claim 真正领。
 * claim 要在上游解人机验证（自带 Node 求解器要拉浏览器），可长达数十秒——
 * 所以按钮文案写明「要等」，超时给到 300s（与服务端那一档对齐）。
 */

function zcOpenClaim() {
  if (!requireChan()) return;
  ZC.preview = null;
  $('zcClaimBody').innerHTML = '';
  $('zcClaimResult').innerHTML = '';
  $('zcClaimEmpty').hidden = false;
  $('zcClaimEmpty').textContent = '点「预览可领套餐」开始';
  $('zcClaimHint').textContent = '—';
  zcMsg('zcClaimMsg', '');
  $('zcClaimModal').hidden = false;
}

/* zcPlanNames 上游 preview 的 plans 形状随版本变：
   可能是字符串数组，也可能是带 plan_id / name / grants 的对象数组。
   只做「能显示」的降级——不猜业务字段，认不出来就把原名贴出来。 */
function zcPlanNames(plans) {
  if (!Array.isArray(plans) || !plans.length) return '<span class="dim">暂无可领套餐</span>';
  return plans.map((p) => {
    if (typeof p === 'string') return `<span class="badge">${esc(p)}</span>`;
    const name = p.name || p.plan_name || p.plan_id || '未命名套餐';
    const grant = p.grants != null ? ` +${esc(String(p.grants))}` : '';
    return `<span class="badge" title="${esc(JSON.stringify(p))}">${esc(name)}${grant}</span>`;
  }).join(' ');
}

function zcRenderClaimPreview(rows) {
  const body = $('zcClaimBody');
  if (!rows.length) {
    body.innerHTML = '';
    $('zcClaimEmpty').hidden = false;
    $('zcClaimEmpty').textContent = '没有可领取的账号（只有 Coding Plan / JWT 账号能领套餐）';
    return;
  }
  body.innerHTML = rows.map((r) => {
    const err = r.error ? `<div class="sub err-text">${esc(r.error)}</div>` : '';
    const act = r.activated
      ? '<span class="badge ok">已上报</span>'
      : `<span class="badge warn">未上报</span>`
        + (r.activation_error ? `<div class="sub">${esc(r.activation_error)}</div>` : '');
    const id = esc(r.account_id || '');
    return `<tr>
      <td><div class="nm">${esc(r.account_name || r.account_id || '—')}</div></td>
      <td class="sub">${zcPlanNames(r.plans)}${err}</td>
      <td>${act}</td>
      <td class="c-ops"><button class="ghost" data-zclaim="${id}"${r.error ? ' disabled' : ''}>领取</button></td>
    </tr>`;
  }).join('');
  $('zcClaimEmpty').hidden = true;
}

async function zcClaimPreview(btn) {
  await zcBusy(btn, '查询中…', async () => {
    try {
      const d = await upApi('claim/preview', { timeout: 300000 });
      const rows = (d && d.preview) || [];
      ZC.preview = rows;
      zcRenderClaimPreview(rows);
      const n = rows.reduce((x, r) => x + ((r.plans || []).length), 0);
      $('zcClaimHint').textContent = `${rows.length} 个账号 · ${n} 个可领套餐`;
    } catch (e) {
      $('zcClaimBody').innerHTML = '';
      $('zcClaimEmpty').hidden = false;
      $('zcClaimEmpty').textContent = '预览失败：' + e.message;
    }
  });
}

/* zcRenderOutcomes 把领取回执摊开。
   上游每条 outcome 带 account_name / ok / plan_name / grants / message——
   只报「成功 N 失败 M」的话，用户既不知道谁失败了，也不知道领到了什么。 */
function zcRenderOutcomes(d) {
  const rows = (d && d.outcomes) || [];
  const s = (d && d.summary) || {};
  $('zcClaimResult').innerHTML = rows.map((o) => {
    const good = !!o.ok;
    const what = o.plan_name
      ? `领取 ${esc(o.plan_name)}` + (o.grants != null ? `（+${esc(String(o.grants))}）` : '')
      : '';
    const why = o.message ? esc(o.message) : '';
    return `<div class="ep-row"><span class="ep-k">${esc(o.account_name || o.account_id || '—')}</span>
      <span class="value"><span class="badge ${good ? 'ok' : 'err'}">${good ? '成功' : '失败'}</span>
      ${good ? what : why}</span></div>`;
  }).join('');
  const fail = s.fail != null ? s.fail : 0;
  zcMsg('zcClaimMsg', `成功 ${s.ok != null ? s.ok : 0} · 失败 ${fail}`, fail > 0);
}

async function zcClaim(ids, btn) {
  const body = (ids && ids.length) ? { account_ids: ids } : {};
  const label = (ids && ids.length) ? '领取中…' : '全部领取中…';
  let done = false;
  await zcBusy(btn, label, async () => {
    try {
      const d = await upPost('claim', body, 300000);
      zcRenderOutcomes(d);
      toast('领取完成', ((d.summary || {}).fail || 0) > 0 ? 'err' : 'ok');
      done = true;
    } catch (e) {
      zcMsg('zcClaimMsg', '领取失败：' + e.message, true);
    }
  });
  if (!done) return;
  await loadZcAccounts();
  // 领到的套餐不该还挂在「可领」里；只有已经预览过才重查（否则会凭空打一次上游）
  if (ZC.preview !== null) await zcClaimPreview($('btnZcClaimPreview'));
}

/* ── 设备码登录（OAuth） ─────────────────────────────────
 *
 * 上游 POST /login/start 返回 {flow_id, authorize_url, expires_in}；
 * 用户在系统浏览器里完成授权后，上游那侧把账号入池。
 * 前端只负责轮询 GET /login/poll/{flow_id} 直到 ready / failed / expired。
 *
 * 轮询用 setTimeout 串行（不是 setInterval）：等上一次返回再排下一次，
 * 慢响应时不会堆叠请求。并且每轮先看弹窗还在不在——点遮罩/ESC 关闭
 * 走的是 app.js 的统一关闭逻辑，不经过我们的关闭函数，只能这样收尾。
 */
const ZC_LOGIN_INTERVAL = 2000;
const ZC_LOGIN_MAX_TRIES = 150; // 2s × 150 = 300s，与上游 flow TTL 持平

function zcSetLoginState(kind, text) {
  const el = $('zcLoginState');
  if (!el) return;
  el.textContent = text;
  el.className = 'login-state' + (kind ? ' ' + kind : '');
}

function zcPollLogin() {
  zcStopLogin();
  ZC.login.timer = setTimeout(async () => {
    ZC.login.timer = null;
    const box = $('zcAccModal');
    if (!box || box.hidden) return;      // 弹窗关了就不再轮询
    if (!ZC.login.flowId) return;
    if (ZC.login.tries++ >= ZC_LOGIN_MAX_TRIES) {
      zcSetLoginState('err', '授权超时（5 分钟），请重新获取链接');
      return;
    }
    try {
      const d = await upApi('login/poll/' + encodeURIComponent(ZC.login.flowId), { timeout: 60000 });
      const st = d && d.status;
      if (st === 'ready') {
        zcSetLoginState('ok', '授权成功，账号已加入池'
          + ((d.account && d.account.name) ? '：' + d.account.name : ''));
        toast('账号已添加', 'ok');
        setTimeout(() => { $('zcAccModal').hidden = true; }, 1200);
        await loadZcAccounts();
        return;
      }
      if (st === 'failed') { zcSetLoginState('err', '授权失败：' + (d.message || '被上游拒绝')); return; }
      if (st === 'expired') { zcSetLoginState('err', '授权会话已过期，请重新获取链接'); return; }
      zcSetLoginState('waiting', '等待中…' + (d.message ? '（' + d.message + '）' : ''));
      zcPollLogin();
    } catch (e) {
      // 单次网络抖动按「还在等」处理，不把一次失败当终态
      zcSetLoginState('waiting', '轮询失败，重试中：' + e.message);
      zcPollLogin();
    }
  }, ZC_LOGIN_INTERVAL);
}

async function zcLoginStart(btn) {
  await zcBusy(btn, '获取中…', async () => {
    try {
      const label = $('zcAccName').value.trim();
      const d = await upPost('login/start', label ? { label } : {}, 120000);
      ZC.login = { flowId: d.flow_id || '', url: d.authorize_url || '', timer: null, tries: 0 };
      if (!ZC.login.flowId) throw new Error('上游没有返回 flow_id');
      $('zcLoginUrl').value = ZC.login.url;
      $('btnZcLoginCopy').disabled = !ZC.login.url;
      $('btnZcLoginOpen').disabled = !ZC.login.url;
      zcSetLoginState('waiting', '等待你在浏览器完成授权…（链接 '
        + (d.expires_in || 300) + ' 秒内有效）');
      // 直接拉系统浏览器：应用内 WebView 打不了第三方登录页
      if (ZC.login.url) desktopOpenExternal(ZC.login.url);
      zcPollLogin();
    } catch (e) {
      zcSetLoginState('err', '发起失败：' + e.message);
    }
  });
}

/* ── 账号表行操作分发 ───────────────────────────────────
 * 行会被整体重渲染，所以用事件委托（绑定在 tbody 上，见 bindZcode）。 */
function zcAccOp(id, op, btn, on) {
  switch (op) {
    case 'refresh': zcRefreshOne(id, btn); break;
    case 'toggle': zcToggleEnabled(id, on, btn); break;
    case 'edit': zcOpenEdit(id); break;
    case 'remove': zcRemove(id); break;
    default: break;
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
    $('zcSetHint').textContent = '可直接修改 · 保存会同时落到网关与本机';
  } catch (e) {
    $('zcSetBody').innerHTML = `<div class="warn-line">加载失败：${esc(e.message)}</div>`;
    $('zcSetHint').textContent = '网关没应答 · 先确认渠道卡上的子进程已启动';
  }
}

/* ── 修改网关设置：写回上游 + 两处密码同步 ─────────────── */

/* zcSetHintFill 提示行只回显「当前是什么状态」，不回填可提交的值。 */
function zcSetHintFill(id, on, masked) {
  const el = $(id);
  if (el) el.textContent = '当前：' + (on ? (masked || '已设置') : '未设置');
}

/* zcOpenSetEdit 打开修改弹窗。
   数值字段预填当前值（直接改）；两个密钥字段一律留空（留空 = 不改），
   掩码只出现在提示行里，不作为可提交的值——上游虽然会跳过含 `…` 的值，
   但把「猜意图」留给上游不如前端干脆不发这个字段。 */
async function zcOpenSetEdit() {
  if (!requireChan()) return;
  if (!ZC.settings) await loadZcSettings();
  const d = ZC.settings;
  if (!d) { toast('网关设置还没读上来，先点「刷新」', 'err'); return; }
  zcSetHintFill('zcSetAdminHint', d.admin_key_set, d.admin_key_masked);
  zcSetHintFill('zcSetGwHint', d.gateway_key_set, d.gateway_key_masked);
  $('zcSetAdminKey').value = '';
  $('zcSetGwKey').value = '';
  $('zcSetCurKey').value = '';
  $('zcSetQuota').value = d.quota_refresh_interval ?? 0;
  $('zcSetConc').value = d.account_concurrency ?? 0;
  $('zcSetClaim').value = d.claim_round_interval ?? 0;
  zcMsg('zcSetMsg', '', false);
  $('zcSetModal').hidden = false;
}

/* zcSaveSettings 保存网关设置。
   两条通道分工明确：
     ① 非密项（三个数值 + 可选的网关 API Key）→ 面板代理 PUT /settings；
     ② 后台密码 → 原生接口 POST /api/channels/admin-key（它负责「改两处」）。
   只把「与当前值不同」的项放进 payload：全量回写会把用户没碰过的字段也写一遍，
   万一上游某字段有不同的默认值处理，就会变成静默覆盖。 */
async function zcSaveSettings(btn) {
  if (!requireChan()) return;
  const pw = $('zcSetAdminKey').value.trim();
  const gw = $('zcSetGwKey').value.trim();
  const curKey = $('zcSetCurKey').value.trim();
  const now = ZC.settings || {};
  const payload = {};
  let changed = 0;
  const takeNum = (id, key, label) => {
    const raw = $(id).value.trim();
    if (raw === '') return;
    const n = Number(raw);
    if (!Number.isInteger(n) || n < 0) throw new Error(label + '必须是非负整数');
    if (String(n) !== String(now[key])) { payload[key] = n; changed++; }
  };
  try {
    takeNum('zcSetQuota', 'quota_refresh_interval', '额度刷新间隔');
    takeNum('zcSetConc', 'account_concurrency', '单账号并发上限');
    takeNum('zcSetClaim', 'claim_round_interval', '套餐自动领取轮');
  } catch (e) { zcMsg('zcSetMsg', e.message, true); return; }
  if (gw !== '') { payload.gateway_key = gw; changed++; }

  if (!changed && !pw) {
    zcMsg('zcSetMsg', '没有要修改的项（密钥留空表示不改）', false);
    return;
  }

  await zcBusy(btn, '保存中…', async () => {
    const done = [];
    try {
      if (changed) {
        await upPut('settings', payload, 30000);
        done.push(`网关设置 ${changed} 项`);
      }
      if (pw) {
        const r = await post('/api/channels/admin-key',
          { name: UP.chan, admin_key: pw, current_key: curKey }, 60000);
        done.push(`后台密码（已同步 ${(r.synced || []).join(' + ')}）`);
      }
    } catch (e) {
      // 半成品要说清楚做到哪一步了：密码那条失败的语义与普通设置不同。
      const head = done.length ? '已完成：' + done.join('、') + '；' : '';
      zcMsg('zcSetMsg', head + '失败：' + e.message, true);
      toast('保存失败：' + e.message, 'err');
      return;
    }
    await loadZcSettings();
    if (pw) $('zcSetAdminKey').value = '';
    zcMsg('zcSetMsg', '已保存：' + done.join('、'), false);
    toast('网关设置已保存', 'ok');
  });
}

/* zcMonClear 清空上游的请求监控环形日志。
   上游这份日志是内存态（重启即清零），清它不动账号也不动设置，
   所以只挡一道 confirm，不做二次确认。 */
async function zcMonClear(btn) {
  if (!requireChan()) return;
  if (!confirm('清空网关的请求监控记录？\n\n只清掉这份内存里的日志，不影响账号与设置。')) return;
  await zcBusy(btn, '清空中…', async () => {
    try {
      await upPost('monitoring/clear', {}, 30000);
      ZC.monitoring = null;
      toast('监控已清空', 'ok');
      await loadZcMonitor();
    } catch (e) { toast('清空失败：' + e.message, 'err'); }
  });
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

  // ── 账号池（只读）
  on('btnZcAccReload', () => loadZcAccounts());
  on('btnZcAccRaw', () => showDrawer('账号原始返回', ZC.accountsRaw || ZC.accounts, ZC.accounts));

  // ── 账号池（写操作）
  on('btnZcAdd', zcOpenAdd);
  on('btnZcClaim', zcOpenClaim);
  on('btnZcImport', zcOpenImport);
  on('btnZcExport', () => zcExport($('btnZcExport')));
  on('btnZcRefreshAll', () => zcRefreshAll($('btnZcRefreshAll')));

  // 账号表行操作：事件委托（行会被整体重渲染）
  const accBody = $('zcAccBody');
  if (accBody) {
    accBody.addEventListener('click', (ev) => {
      const b = ev.target.closest('[data-zop]');
      if (b) zcAccOp(b.dataset.zid, b.dataset.zop, b, b.dataset.zon === '1');
    });
  }

  // ── 新增 / 编辑弹窗
  on('btnZcAccClose', () => { zcStopLogin(); $('zcAccModal').hidden = true; });
  on('btnZcAccSave', () => zcSaveAccount($('btnZcAccSave')));
  on('btnZcAccFp', () => zcRotateFp($('btnZcAccFp')));
  on('btnZcLoginStart', () => zcLoginStart($('btnZcLoginStart')));
  on('btnZcLoginCopy', async () => {
    try { await copyText(ZC.login.url || ''); toast('链接已复制', 'ok'); }
    catch (e) { toast('复制失败', 'err'); }
  });
  on('btnZcLoginOpen', () => desktopOpenExternal(ZC.login.url || ''));
  const accTabs = $('zcAccTabs');
  if (accTabs) {
    accTabs.addEventListener('click', (ev) => {
      const b = ev.target.closest('.seg-b');
      if (b) zcSetAccTab(b.dataset.zt);
    });
  }

  // ── 领取弹窗
  on('btnZcClaimClose', () => { $('zcClaimModal').hidden = true; });
  on('btnZcClaimOk', () => { $('zcClaimModal').hidden = true; });
  on('btnZcClaimPreview', () => zcClaimPreview($('btnZcClaimPreview')));
  on('btnZcClaimAll', () => zcClaim(null, $('btnZcClaimAll')));
  const claimBody = $('zcClaimBody');
  if (claimBody) {
    claimBody.addEventListener('click', (ev) => {
      const b = ev.target.closest('[data-zclaim]');
      if (b) zcClaim([b.dataset.zclaim], b);
    });
  }

  // ── 导入弹窗
  on('btnZcImportClose', () => { $('zcImportModal').hidden = true; });
  on('btnZcImportDo', () => zcImportDo($('btnZcImportDo')));
  const impFile = $('zcImportFile');
  if (impFile) impFile.addEventListener('change', zcImportFilePicked);

  // ── 运行监控（清空是上游写操作）
  on('btnZcMonReload', () => loadZcMonitor());
  on('btnZcMonRaw', () => showDrawer('运行监控原始返回', ZC.monitoring, ZC.monitoring));
  on('btnZcMonClear', () => zcMonClear($('btnZcMonClear')));

  // ── 网关设置（可在本面板内直接改）
  on('btnZcSetReload', () => loadZcSettings());
  on('btnZcSetRaw', () => showDrawer('网关设置原始返回', ZC.settings, ZC.settings));
  on('btnZcSetEdit', zcOpenSetEdit);
  on('btnZcSetClose', () => { $('zcSetModal').hidden = true; });
  on('btnZcSetSave', () => zcSaveSettings($('btnZcSetSave')));
})();
