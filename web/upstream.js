/* upstream.js 上游控制台：积分型平台的完整管理功能。
 *
 * 这里实现了原 WorkBuddy 网关面板（wb2api）的全部功能入口，通过 Mergence 的
 * 管理代理调用（密钥由服务端注入，浏览器不接触上游凭证）：
 *
 *   账号池    overview / 批量签到·旅行·活跃·保活·余额 / 单账号启停·签到·余额·任务·移除
 *   任务中心  tasks/queue · scan_all · run_queue · school/vouchers · 账号任务 accept·claim·auto
 *   模型档位  models（积分倍率/默认档/思考档位/上下文/最大输出 + 能力徽标 + 域/能力/档位/价格筛选）
 *   积分构成  packages（到期分布 + 账号对比 + 批次明细）
 *   用量      usage（总量 KPI + 花费估算 + Token 时序 + 账号/模型/域三视 + 积分扣除历史）
 *   配置      config 读写（按上游分组的结构化表单，60 个字段全覆盖）
 *   日志      logs / request_metrics / request_logs
 *   添加账号  login/start · login/poll · import/cockpit
 *
 * 字段读取一律防御式：不同 2api 实现的字段命名有差异，缺字段就显示「—」，
 * 不能因为一个可选字段缺失就整页报错。
 */

const UP = {
  chan: '',        // 当前选中的托管渠道名
  overview: null,  // 账号池概览缓存
  models: null,    // 模型与档位缓存（上游原始顺序，绝不原地排序）
  usage: null,     // 用量缓存
  pk: null,        // 积分构成缓存
  cfg: null,       // 上游配置缓存（原始 JSON 副本）
  cfgDirty: false, // 配置是否被改动
  login: { state: '', url: '', realm: 'cn' },
  tasks: [],       // 任务列表缓存
  usDim: 'account',       // 用量明细维度
  usCreditDim: 'account', // 积分扣除维度
};

/* ── 代理调用 ─────────────────────────────────────────── */

async function upApi(path, opts) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), (opts && opts.timeout) || 30000);
  try {
    const r = await fetch('/api/channels/' + encodeURIComponent(UP.chan) + '/upstream/' + path,
      Object.assign({}, opts, { signal: ctl.signal }));
    const txt = await r.text();
    let d = {};
    try { d = txt ? JSON.parse(txt) : {}; } catch (e) { d = { raw: txt }; }
    if (!r.ok) {
      const err = new Error(d.error || d.message || ('HTTP ' + r.status));
      err.code = d.code;
      err.payload = d;
      throw err;
    }
    return d;
  } finally {
    clearTimeout(timer);
  }
}

function upPost(path, body, timeout) {
  return upApi(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body === undefined ? {} : body),
    timeout: timeout || 30000,
  });
}

function upPut(path, body, timeout) {
  return upApi(path, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body === undefined ? {} : body),
    timeout: timeout || 30000,
  });
}

/* upDelete 带 body 的 DELETE。
 *
 * 上游 zcode2api 的 DELETE /accounts 收的是 JSON 数组 body（要删的 id 列表），
 * 不是 query 参数——所以不能退化成无 body 的 del()，那样上游拿不到 id，
 * 会安静地一个都不删（返回 deleted:0），看起来像「删了但没生效」。 */
function upDelete(path, body, timeout) {
  return upApi(path, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body === undefined ? {} : body),
    timeout: timeout || 30000,
  });
}

/* 需要先选中托管渠道的视图调用 */
function requireChan() {
  if (UP.chan) return true;
  toast('请先在侧栏「控制台」里选一个渠道', 'err');
  return false;
}

/* ── 格式化 ───────────────────────────────────────────── */

function num(v) {
  if (v === null || v === undefined || v === '') return '—';
  if (typeof v !== 'number') {
    const n = Number(v);
    if (!Number.isFinite(n)) return String(v);
    v = n;
  }
  return v.toLocaleString('zh-CN');
}

/* fmtTok 大数压缩：账号列表里出现 8 位数会把整行撑开。 */
function fmtTok(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v === 0) return n === 0 ? '0' : '—';
  if (v >= 1e8) return (v / 1e8).toFixed(2) + '亿';
  if (v >= 1e4) return (v / 1e4).toFixed(1) + '万';
  return String(v);
}

function fmtMs(ms) {
  const v = Number(ms);
  if (!Number.isFinite(v) || v <= 0) return '—';
  return v >= 1000 ? (v / 1000).toFixed(2) + 's' : Math.round(v) + 'ms';
}

function fmtRate(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n.toFixed(1) + ' tok/s';
}

function dur(sec) {
  if (!sec && sec !== 0) return '—';
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + ' 秒';
  if (s < 3600) return Math.round(s / 60) + ' 分钟';
  if (s < 86400) return (s / 3600).toFixed(1) + ' 小时';
  return (s / 86400).toFixed(1) + ' 天';
}

/* 秒级相对时间：账号表的「最近成功」看的是「多久没动了」，绝对时刻反而要心算。 */
function ago(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

function when(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function dayOf(t) {
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function pct(part, total) {
  const t = Number(total);
  if (!Number.isFinite(t) || t <= 0) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

function trimFixed(s) {
  return String(s).replace(/\.?0+$/, '');
}

function showDrawer(title, result, raw) {
  $('drawerTitle').textContent = title;
  const body = $('drawerJson');
  const mode = document.querySelector('#drawerTabs .seg-b.on');
  body.textContent = (mode && mode.dataset.dt === 'raw')
    ? JSON.stringify(raw, null, 2)
    : (typeof result === 'string' ? result : JSON.stringify(result, null, 2));
  $('drawer').hidden = false;
}

function card(label, value, cls) {
  return `<div class="card-k"><b class="${cls || ''}">${num(value)}</b><span>${esc(label)}</span></div>`;
}

/* ── 账号池 ───────────────────────────────────────────── */

/* accState 账号状态标签。
 *
 * 冷却有四种来源，混成一个「冷却中」会让人分不清「等一会儿就好」和
 * 「这个号出问题了」——熔断要人工介入，限流等几分钟自己会恢复。
 */
function accState(a) {
  if (a.disabled) return '<span class="badge muted">已禁用</span>';
  const now = Date.now();
  const bl = (new Date(a.breaker_until || 0) - now) / 1000;
  const dg = (new Date(a.degrade_until || 0) - now) / 1000;
  const soft = a.cool_remaining_sec || 0;
  const cool = Math.max(soft, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  if (cool <= 0) return '<span class="badge ok">可用</span>';
  const kind = bl >= Math.max(soft, dg) ? '熔断'
    : (dg >= soft ? '连败降权' : (a.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
  return `<span class="badge warn">${kind} ${dur(cool)}</span>`;
}

/* accUsage 用量列：最近一次请求的次数/token/延迟/速率四个小项。
   合成一列而不是四列——这是「一眼看这个号最近干活怎么样」的读数，
   拆开后每列都只有一两个字符，反而更难扫。 */
function accUsage(a) {
  const tu = a.token_usage || {};
  const req = tu.request_count || 0;
  const tok = fmtTok(tu.total_tokens);
  const lat = fmtMs(tu.last_latency_ms);
  const rate = fmtRate(tu.last_tokens_per_second);
  const title = `最近一次：${req} 次 / ${tok} tok / 延迟 ${lat} / ${rate}`;
  return `<td class="num usage-cell" title="${esc(title)}"><span class="usage-line">
      <span class="usage-item usage-count"><b>${req}</b><em>次</em></span>
      <span class="usage-item usage-total"><b>${tok}</b>${tok === '—' ? '' : '<em>tok</em>'}</span>
      <span class="usage-item usage-latency"><b>${lat}</b></span>
      <span class="usage-item usage-rate"><b>${rate}</b></span>
    </span></td>`;
}

function accRow(a) {
  const uid = esc(a.uid || '');
  const ops = [];
  const frozen = a.disabled || (a.cool_remaining_sec || 0) > 0;
  ops.push(`<button class="ghost" data-uid="${uid}" data-op="checkin">${a.checkin_done ? '已签' : '签到'}</button>`);
  ops.push(`<button class="ghost" data-uid="${uid}" data-op="balance">余额</button>`);
  ops.push(`<button class="ghost" data-uid="${uid}" data-op="tasks">任务</button>`);
  ops.push(a.disabled
    ? `<button class="ghost" data-uid="${uid}" data-op="revive">解冻</button>`
    : `<button class="ghost" data-uid="${uid}" data-op="disable">禁用</button>`);
  ops.push(`<button class="ghost danger" data-uid="${uid}" data-op="remove">移除</button>`);

  // 积分：有总额就显示 剩余/总额 与进度条；旧数据没有总额则退回相对池内最高值
  const maxCred = Math.max(1, ...((UP.overview && UP.overview.accounts) || []).map((s) => s.credits || 0));
  const p = a.credits_total > 0
    ? Math.min(100, Math.round((a.credits || 0) / a.credits_total * 100))
    : Math.round((a.credits || 0) / maxCred * 100);
  let credTip = a.credits_total > 0
    ? `剩余 ${a.credits} / 总额 ${a.credits_total}（${p}%）`
    : '积分（相对池内最高）';
  const costs = (a.model_costs || []).filter((c) => c.model);
  if (costs.length) {
    credTip += '\n实测单价（credits/1K）：\n' + costs.map((c) =>
      '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
  }
  const credNum = a.credits == null ? '—'
    : (a.credits_total > 0
      ? `${num(a.credits)}<span class="of">/${num(a.credits_total)}</span>`
      : num(a.credits));

  const realmTag = a.realm === 'global' ? ' <span class="badge muted">国际版</span>' : '';
  const note = a.reason ? `<div class="sub">${esc(a.reason)}</div>` : '';
  const short = String(a.uid || '').length > 16 ? String(a.uid).slice(0, 16) + '…' : (a.uid || '');

  return `<tr title="uid: ${uid}">
    <td><div class="nm">${esc(a.nickname || '未命名')}${realmTag}</div>
        <div class="sub mono">${esc(short)}</div></td>
    <td>${accState(a)}${note}</td>
    <td class="credits" title="${esc(credTip)}"><div class="cred-n">${credNum}</div>
        <div class="cred-bar"><i data-style="width:${isFinite(p) ? p : 0}%"></i></div></td>
    <td class="credits">${num(a.success_count || 0)} <span class="dim">/</span>
        <span class="err-text">${num(a.err_total || 0)}</span></td>
    <td>${a.in_flight ? `<span class="badge warn">${a.in_flight}</span>` : '0'}</td>
    ${accUsage(a)}
    <td class="sub">${ago(a.last_success)}</td>
    <td class="c-ops">${ops.join(' ')}</td>
  </tr>`;
}

async function loadUpAccounts() {
  if (!requireChan()) return;
  const body = $('accBody');
  body.innerHTML = '<tr><td colspan="8" class="sub">加载中…</td></tr>';
  try {
    const d = await upApi('overview');
    UP.overview = d;
    const accts = d.accounts || [];
    // 头部统计：与上游面板同口径（含积分合计、在途占满、运行时长、版本）
    const remSum = accts.reduce((x, s) => x + (s.credits || 0), 0);
    const totSum = accts.reduce((x, s) => x + (s.credits_total || 0), 0);
    $('upStat').innerHTML = [
      card('账号总数', d.total),
      card('可用', d.healthy, 'ok'),
      card('冷却中', d.cooling, 'warn'),
      card('已禁用', d.disabled),
      card('积分剩余 / 总额', totSum > 0 ? `${num(remSum)} / ${num(totSum)}` : num(remSum)),
      card('粘性会话', d.sticky_sessions),
    ].join('');
    const up = Math.floor(d.uptime_sec || 0);
    const uptxt = up >= 86400
      ? `${Math.floor(up / 86400)} 天 ${Math.floor(up % 86400 / 3600)} 时`
      : `${Math.floor(up / 3600)} 时 ${Math.floor(up % 3600 / 60)} 分`;
    $('upAccHint').textContent = [
      `共 ${d.total || accts.length} 个账号`, `可用 ${d.healthy || 0}`,
      `冷却 ${d.cooling || 0}`, `停用 ${d.disabled || 0}`,
      d.in_flight_full ? `${d.in_flight_full} 个在途占满` : '',
      `运行 ${uptxt}`, `v${d.version || '?'}`,
      d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存',
    ].filter(Boolean).join(' · ');
    body.innerHTML = accts.length ? accts.map(accRow).join('') : '';
    paintStyles(body);
    $('accEmpty').hidden = accts.length > 0;
    $('accEmpty').textContent = '池里还没有账号，点右上角「添加账号」';
  } catch (e) {
    body.innerHTML = '';
    $('accEmpty').hidden = false;
    $('accEmpty').textContent = '加载失败：' + e.message;
  }
}

async function accOp(uid, op) {
  if (op === 'remove' && !confirm('移除账号将删除池状态与凭证文件，且不可恢复。确认移除？')) return;
  if (op === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
  try {
    const r = await upPost('accounts/' + encodeURIComponent(uid) + '/' + op, {}, 60000);
    if (op === 'tasks') { showTasks(r); return; }
    toast('操作完成', 'ok');
  } catch (e) {
    toast('操作失败：' + e.message, 'err');
  }
  await loadUpAccounts();
}

async function batchOp(op, label) {
  if (!requireChan()) return;
  try {
    const r = await upPost(op, {}, 180000);
    showDrawer(label, r);
    toast(label + ' 已执行', 'ok');
  } catch (e) {
    toast(label + ' 失败：' + e.message, 'err');
  }
  await loadUpAccounts();
}

/* showTasks 单账号任务明细（点账号行「任务」）。 */
function showTasks(r) {
  const list = r.tasks || r.items || (Array.isArray(r) ? r : []);
  if (!list.length) { showDrawer('账号任务', r); return; }
  const html = list.map((t) =>
    `${esc(t.name || t.title || t.id || '—')}　${t.done ? '已完成' : (t.progress != null ? t.progress : '待处理')}`
  ).join('\n');
  showDrawer('账号任务', html, r);
}

/* ── 任务中心 ─────────────────────────────────────────── */

async function loadTasks() {
  if (!requireChan()) return;
  const body = $('tcBody');
  try {
    const d = await upApi('tasks/queue');
    const list = d.items || d.tasks || d.queue || [];
    UP.tasks = Array.isArray(list) ? list : [];
    $('tcCards').innerHTML = [
      card('队列长度', d.total != null ? d.total : UP.tasks.length),
      card('可执行', UP.tasks.filter((t) => t.runnable !== false && !t.done).length, 'ok'),
      card('已完成', UP.tasks.filter((t) => t.done).length),
      card('进行中', d.running ? 1 : 0, 'warn'),
    ].join('');
    const parts = [];
    if (d.running) parts.push('正在执行');
    if (d.conc) parts.push(`并发 ${d.conc}`);
    if (d.started_at && d.started_at.slice(0, 4) !== '0001') parts.push(`开始于 ${when(d.started_at)}`);
    $('tcHint').textContent = parts.length ? parts.join(' · ')
      : (UP.tasks.length ? '' : '队列为空，点「扫描待办」拉取');
    body.innerHTML = UP.tasks.length ? UP.tasks.map(taskRow).join('') : '';
    $('tcEmpty').hidden = UP.tasks.length > 0;
  } catch (e) {
    body.innerHTML = '';
    $('tcEmpty').hidden = false;
    $('tcEmpty').textContent = '读取队列失败：' + e.message;
  }
}

function taskRow(t) {
  const id = esc(t.id || t.task_id || t.key || '');
  const prog = t.total ? `${t.done || 0}/${t.total}` : (t.progress != null ? t.progress : '—');
  const reward = t.reward != null ? num(t.reward)
    : (t.credits != null ? num(t.credits) : '—');
  const ops = [];
  if (!t.done) {
    ops.push(`<button class="ghost" data-task="${id}" data-top="accept">接受</button>`);
    ops.push(`<button class="ghost" data-task="${id}" data-top="claim">领取</button>`);
    ops.push(`<button class="ghost" data-task="${id}" data-top="auto">自动</button>`);
  }
  return `<tr>
    <td><div class="nm">${esc(t.name || t.title || t.type || id)}</div>
        <div class="sub">${esc(t.desc || t.description || '')}</div></td>
    <td class="credits">${esc(String(prog))}</td>
    <td class="credits">${reward}</td>
    <td>${t.done ? '<span class="badge ok">已完成</span>'
      : (t.running ? '<span class="badge warn">进行中</span>' : '<span class="badge">待处理</span>')}</td>
    <td class="c-ops">${ops.join(' ')}</td>
  </tr>`;
}

async function taskOp(taskId, op) {
  try {
    const r = await upPost('tasks/' + op, { id: taskId, task_id: taskId }, 120000);
    showDrawer('任务 ' + op, r);
  } catch (e) {
    toast('任务操作失败：' + e.message, 'err');
  }
  await loadTasks();
}

async function scanTasks() {
  if (!requireChan()) return;
  $('tcHint').textContent = '正在扫描…';
  try {
    const r = await upPost('tasks/scan_all', {}, 180000);
    showDrawer('扫描待办结果', r);
  } catch (e) {
    toast('扫描失败：' + e.message, 'err');
  }
  await loadTasks();
}

async function runQueue() {
  if (!requireChan()) return;
  const conc = ($('qcConc') && $('qcConc').value) || '1';
  const prog = $('tcProg');
  prog.hidden = false;
  $('tcProgText').textContent = `正在执行队列（并发 ${conc}）…`;
  $('tcProgFill').className = 'prog-fill';
  try {
    const r = await upPost('tasks/run_queue', { conc: Number(conc) }, 600000);
    showDrawer('执行队列结果', r);
  } catch (e) {
    toast('执行失败：' + e.message, 'err');
  } finally {
    $('tcProgText').textContent = '完成';
    $('tcProgFill').className = 'prog-fill done';
  }
  await loadTasks();
}

async function queryVouchers() {
  if (!requireChan()) return;
  try {
    const r = await upApi('school/vouchers');
    showDrawer('学校券码', r);
  } catch (e) {
    toast('查询失败：' + e.message, 'err');
  }
}

/* ── 模型与档位 ───────────────────────────────────────── */

const MD = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };

/* mdRate 从 credits 字符串里抽出数值。
 * 上游两种写法：`x0.29` 与 `x0.03 credits`（后者多带单位），
 * 直接取值会把 "0.03 credits" 渲染到界面上。 */
function mdRate(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const s = String(raw == null ? '' : raw);
  const n = parseFloat(s.replace(/[^\d.]/g, ''));
  return { text: s.trim(), value: Number.isFinite(n) ? n : null };
}

/* mdRateValue 排序用：促销价优先；没有倍率记 Infinity（「没有价格」不该
   冒充最便宜排到第一）。 */
function mdRateValue(m) {
  const r = mdRate(m);
  return r.value == null ? Infinity : r.value;
}

function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

function mdMatch(m) {
  if (MD.q) {
    const text = mdSearchText(m);
    for (const kw of MD.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  // 域看的是 ID 前缀（`cn:` / `global:`），模型对象里没有独立的 realm 字段
  if (MD.realm && !String(m.id || '').startsWith(MD.realm + ':')) return false;
  if (MD.cap === 'tool' && !m.supports_tool_call) return false;
  if (MD.cap === 'vision' && !m.supports_images) return false;
  if (MD.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (MD.cap === 'default' && !m.is_default) return false;
  if (MD.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (MD.effort && !(m.supported_efforts || []).includes(MD.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (MD.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (MD.promo === 'free' && factor !== 0) return false;
  if (MD.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

function mdSortList(list) {
  const out = list.slice(); // 不改动缓存：切回「上游默认顺序」要能还原
  const n = (v) => { const x = Number(v || 0); return Number.isFinite(x) ? x : 0; };
  if (MD.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (MD.sort === 'context') out.sort((a, b) => n(b.context_length) - n(a.context_length));
  else if (MD.sort === 'output') out.sort((a, b) => n(b.max_output_tokens) - n(a.max_output_tokens));
  else if (MD.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

/* mdRow 模型行：ID + 展示名 + 能力徽标，倍率/默认档/思考档位/上下文/最大输出。 */
function mdRow(m) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length
    ? eff.map((e) => `<span class="badge warn">${esc(e)}</span>`).join('')
    : `<span class="dim">${m.supports_reasoning
      ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考'}</span>`;

  // 能力徽标：全字段透出，缺失就不显示（不编造）
  const caps = [];
  if (m.is_default) caps.push('<span class="badge ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="badge warn">工具</span>');
  if (m.supports_images) caps.push('<span class="badge warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="badge warn">思考常开</span>');
  const capHtml = caps.length ? `<div class="caps">${caps.join('')}</div>` : '';

  // 上游 tags 里有 `badge:文案:#颜色` 这种自定义徽标（如夜间免费）
  const tagHtml = (m.tags || []).map((t) => {
    const mm = /^badge:([^:]+):(#[0-9a-fA-F]{3,8})$/.exec(String(t));
    if (mm) return `<span class="badge" data-style="color:${mm[2]};border-color:${mm[2]}">${esc(mm[1])}</span>`;
    return '';
  }).join('');

  const r = mdRate(m);
  let rateCell = '—';
  if (r.value != null) {
    rateCell = r.value === 0 ? '<span class="badge ok">免费</span>' : `×${trimFixed(r.value)}`;
  } else if (r.text) {
    rateCell = esc(r.text);
  }
  if (m.promo_label && r.value !== 0) rateCell += ` <span class="badge warn">${esc(m.promo_label)}</span>`;

  const tip = m.description ? ` title="${esc(m.description)}"` : '';
  return `<tr>
    <td${tip}><div class="nm">${esc(m.id)}</div>
        ${m.name && m.name !== m.id ? `<div class="sub">${esc(m.name)}</div>` : ''}
        ${capHtml}${tagHtml ? `<div class="caps">${tagHtml}</div>` : ''}</td>
    <td class="credits">${rateCell}</td>
    <td>${m.default_effort ? `<span class="badge ok">${esc(m.default_effort)}</span>` : '<span class="dim">—</span>'}</td>
    <td class="efs">${effs}</td>
    <td class="credits">${m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—'}</td>
    <td class="credits">${fmtTok(m.max_output_tokens)}</td>
  </tr>`;
}

async function loadModels() {
  if (!requireChan()) return;
  const body = $('mdBody');
  body.innerHTML = '<tr><td colspan="6" class="sub">正在向上游查询…</td></tr>';
  try {
    const [d, pr] = await Promise.all([
      upApi('models'),
      upApi('model_probes').catch(() => ({})),
    ]);
    const list = d.models || d.data || d.items || [];
    UP.models = Array.isArray(list) ? list : [];
    UP.probes = pr.probes || {};
    const hit = Object.keys(UP.probes).length;
    $('mdHint').textContent = `${UP.models.length} 个模型`
      + (hit ? ` · ${hit} 个有实测上限` : '')
      + (Array.isArray(d.tags) ? '' : '');
    renderModels();
  } catch (e) {
    body.innerHTML = '';
    $('mdEmpty').hidden = false;
    $('mdEmpty').textContent = '加载失败：' + e.message;
  }
}

function renderModels() {
  const list = mdSortList((UP.models || []).filter(mdMatch));
  const all = UP.models || [];
  $('mdBody').innerHTML = list.length ? list.map(mdRow).join('') : '';
  paintStyles($('mdBody'));
  $('mdEmpty').hidden = list.length > 0;
  $('mdEmpty').textContent = all.length ? '没有符合当前筛选条件的模型' : '上游未返回模型列表';
  const filtered = list.length !== all.length;
  $('mdCount').textContent = !all.length ? '' : (filtered
    ? `命中 ${list.length} / ${all.length} 个模型` : `${all.length} 个模型`);
  $('mdCount').className = filtered ? 'fhint warn-txt' : 'fhint';
}

function resetModelFilter() {
  MD.q = ''; MD.realm = ''; MD.cap = ''; MD.effort = ''; MD.promo = ''; MD.sort = 'default';
  ['mdQ', 'mdRealm', 'mdCap', 'mdEffort', 'mdPromo', 'mdSort'].forEach((id) => {
    if ($(id)) $(id).value = id === 'mdSort' ? 'default' : '';
  });
  renderModels();
}

/* ── 积分构成 ─────────────────────────────────────────── */

/* pkBySource 把「账号 → 套餐批次」摊平成批次列表，用于到期分布。
   上游按账号分组返回，做分布要按到期日重排，因此先扁平化。 */
function pkRows(d) {
  const accts = d.accounts || [];
  const out = [];
  accts.forEach((a) => {
    (a.packages || []).forEach((p) => {
      out.push({
        acct: a.nickname || a.uid,
        uid: a.uid,
        realm: a.realm,
        name: p.name || p.id || '—',
        remain: Number(p.remain || 0),
        used: Number(p.used || 0),
        size: Number(p.size || 0),
        end: p.end_time || p.expiry || p.expires_at,
      });
    });
  });
  return out;
}

/* pkColor 稳定的账号配色：同一个账号在分布图和图例里必须是同一个颜色，
   否则「看颜色对账号」这个设计就失效了。 */
const PK_COLORS = ['#6aa9ff', '#5fd39a', '#f2b95c', '#e8798a', '#a98bf0', '#57c9d4', '#dba05b', '#8fc76a'];
function pkColor(uid) {
  let h = 0;
  for (const c of String(uid || '')) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return PK_COLORS[h % PK_COLORS.length];
}

async function loadPackages() {
  if (!requireChan()) return;
  try {
    const d = await upApi('packages');
    UP.pk = d;
    const accts = d.accounts || [];
    const rows = pkRows(d);
    const remainSum = accts.reduce((x, a) => x + (Number(a.remain) || 0), 0);
    const sizeSum = accts.reduce((x, a) => x + (Number(a.size) || 0), 0);
    $('pkNote').textContent = `${accts.length} 个账号 · ${rows.length} 个批次 · 剩余 ${num(remainSum)}`
      + (sizeSum ? ` / 总额 ${num(sizeSum)}` : '');

    renderExpiry(rows);
    renderPkSummary(accts, rows);
    renderPkDetail(rows, d.package_detail_limit || d.limit || 0);
  } catch (e) {
    $('pkDetail').innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
  }
}

/* renderExpiry 积分到期分布：按到期日聚合，条内按账号切色。
   为什么值得单独一块：积分过期是「不花就没了」，只看剩余总量看不出
   「下周三会丢掉多少」。 */
function renderExpiry(rows) {
  const box = $('pkExpiry');
  const now = Date.now();
  const groups = new Map();
  rows.forEach((r) => {
    const t = new Date(r.end || 0);
    const key = (isNaN(t.getTime()) || t.getFullYear() < 2000) ? '长期有效' : dayOf(r.end);
    if (!groups.has(key)) groups.set(key, { total: 0, parts: new Map() });
    const g = groups.get(key);
    g.total += r.remain;
    g.parts.set(r.acct, (g.parts.get(r.acct) || 0) + r.remain);
  });
  const list = [...groups.entries()].filter(([, g]) => g.total > 0);
  if (!list.length) { box.innerHTML = '<div class="empty">没有可用的积分批次</div>'; return; }
  list.sort((a, b) => (a[0] === '长期有效' ? 1e15 : new Date(a[0]).getTime())
    - (b[0] === '长期有效' ? 1e15 : new Date(b[0]).getTime()));
  const max = Math.max(...list.map(([, g]) => g.total));
  box.innerHTML = list.map(([day, g]) => {
    const segs = [...g.parts.entries()].map(([acct, v]) =>
      `<i data-style="width:${(v / g.total * 100).toFixed(2)}%;background:${pkColor(acct)}"></i>`).join('');
    const past = day !== '长期有效' && new Date(day).getTime() < now;
    return `<div class="exp-row" title="${esc(day)}：${num(g.total)} 积分">
      <span class="exp-d">${esc(day)}${past ? ' <span class="badge err">已过期</span>' : ''}</span>
      <span class="exp-bar" data-style="width:${Math.max(4, g.total / max * 100)}%">${segs}</span>
      <span class="exp-v">${num(g.total)}</span>
    </div>`;
  }).join('');
  paintStyles(box);
}

/* renderPkSummary 账号对比：每个账号一张卡，条内按批次切色。 */
function renderPkSummary(accts, rows) {
  const box = $('pkSummary');
  if (!accts.length) { box.innerHTML = '<div class="empty">上游未返回账号积分</div>'; return; }
  box.innerHTML = accts.map((a) => {
    const mine = rows.filter((r) => r.uid === a.uid && r.remain > 0);
    const total = mine.reduce((x, r) => x + r.remain, 0);
    const size = Number(a.size) || total || 1;
    const segs = mine.map((r) =>
      `<i data-style="width:${(r.remain / size * 100).toFixed(2)}%;background:${pkColor(a.uid)}"></i>`).join('');
    const legend = mine.slice(0, 4).map((r) =>
      `<span><b data-style="background:${pkColor(a.uid)}"></b>${esc(r.name)} ${num(r.remain)}</span>`).join('');
    const earliest = mine.map((r) => r.end).filter(Boolean).sort()[0];
    return `<div class="pk-acct">
      <div class="n">${esc(a.nickname || a.uid)}${a.realm === 'global' ? ' <span class="badge muted">国际版</span>' : ''}</div>
      <div class="r">剩余 ${num(total)}${a.size ? ` <span class="dim">/ ${num(a.size)}</span>` : ''}
        · 已用 ${num(a.used || 0)}</div>
      <div class="bar">${segs}</div>
      <div class="lg">${legend || '<span>无剩余批次</span>'}</div>
      <div class="lg">${earliest ? `最早到期 ${dayOf(earliest)}` : ''}</div>
    </div>`;
  }).join('');
  paintStyles(box);
}

/* renderPkDetail 批次明细表；limit=0 表示不截断。 */
function renderPkDetail(rows, limit) {
  const list = limit > 0 ? rows.slice(0, limit) : rows;
  const rest = rows.length - list.length;
  if (!list.length) { $('pkDetail').innerHTML = '<div class="empty">上游未返回套餐信息</div>'; return; }
  const body = list.map((r) => `<tr>
    <td><div class="nm">${esc(r.name)}</div><div class="sub">${esc(r.acct)}</div></td>
    <td class="credits">${num(r.size)}</td>
    <td class="credits">${num(r.used)}</td>
    <td class="credits">${num(r.remain)}</td>
    <td class="sub">${r.end ? dayOf(r.end) : '长期有效'}</td>
  </tr>`).join('');
  $('pkDetail').innerHTML = `<section class="card">
    <div class="card-hd"><h2>批次明细</h2>
      <span class="hint">${rows.length} 个批次${rest > 0 ? `（按上游设置只列前 ${list.length} 条，其余 ${rest} 条见「账号对比」）` : ''}</span></div>
    <div class="tablewrap"><table class="dt"><thead><tr>
      <th>套餐</th><th>总量</th><th>已用</th><th>剩余</th><th>到期</th>
    </tr></thead><tbody>${body}</tbody></table></div>
  </section>`;
}

/* ── 用量 ─────────────────────────────────────────────── */

async function loadUsage() {
  if (!requireChan()) return;
  const range = $('usRange').value;
  try {
    const d = await upApi('usage' + (range !== '0' ? '?days=' + range : ''));
    UP.usage = d;
    renderUsageKpi(d);
    renderUsageChart(d.series || []);
    renderUsageDim();
    renderCreditDim();
  } catch (e) {
    $('usDimBody').innerHTML = '';
    $('usEmpty').hidden = false;
    $('usEmpty').textContent = '加载失败：' + e.message;
  }
}

/* renderUsageKpi 总量指标：14 个口径一次铺开（含积分换算与缓存命中）。 */
/* upCreditValue 当前渠道的每积分价值（元/积分）。
 *
 * 取自渠道列表里的 credit_value——后端已经把「配置值 || 平台默认价」
 * 算好，并用 credit_value_default 标明来源，所以这里不再自己判断平台。
 * 两者都没有时返回 0，此时花费显示「—」而不是 ¥0.00：
 * 0 会被读成「没花钱」，而实际是「不知道」。
 */
function upCreditValue() {
  const c = (Store.get().channels || []).find((x) => x.name === UP.chan);
  return {
    cv: c ? (Number(c.credit_value) || 0) : 0,
    isDefault: !!(c && c.credit_value_default),
  };
}

/* money 积分 × 单价 → 人民币。
 *
 * 分以下多留几位：平台上一天的用量常常只有几分钱，一律 toFixed(2)
 * 会全显示成 ¥0.00，反而看不出差别。
 */
function money(credits, cv) {
  const n = (Number(credits) || 0) * (Number(cv) || 0);
  if (!(n > 0)) return '';
  if (n < 0.01) return '¥' + n.toFixed(4);
  if (n < 1) return '¥' + n.toFixed(3);
  return '¥' + n.toLocaleString('zh-CN',
    { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function renderUsageKpi(d) {
  const t = d.totals || {};
  $('usNote').textContent = d.generated
    ? `生成于 ${when(d.generated)}${d.since ? ` · 自 ${d.since}` : ''}` : '—';
  const kpi = (label, value, cls, sub) =>
    `<div class="card-k"><b class="${cls || ''}">${esc(String(value))}</b><span>${esc(label)}</span>
      ${sub ? `<span class="k-sub">${esc(sub)}</span>` : ''}</div>`;
  const { cv, isDefault } = upCreditValue();
  const spend = money(t.credits, cv);
  $('usStats').innerHTML = [
    kpi('请求数', num(t.requests)),
    kpi('失败数', num(t.errors), t.errors ? 'err' : ''),
    kpi('合计 Token', fmtTok(t.total_tokens), 'accent',
      `prompt ${fmtTok(t.prompt_tokens)} / completion ${fmtTok(t.completion_tokens)}`),
    kpi('积分消耗', trimFixed(Number(t.credits || 0).toFixed(4))),
    kpi('积分 / 1M Token', t.credits_per_1m_tokens != null
      ? trimFixed(Number(t.credits_per_1m_tokens).toFixed(4)) : '—',
      '', `${t.credit_samples || 0} 个有效样本`),
    // 花费估算紧挨着「积分消耗」：积分是平台口径，人民币才是能对比的数。
    // 未配单价且平台无默认价时给「—」并写明去哪儿填，不给 0。
    kpi('实际花费估算', spend || '—', spend ? 'accent' : '',
      !cv ? '在渠道设置里填写「每积分价值」后可估算'
        : (isDefault ? `按平台默认价 ¥${cv}/积分（渠道设置里可改）`
          : `积分单价 ¥${cv}`)),
    kpi('缓存命中率', t.cache_hit_rate != null
      ? trimFixed(Number(t.cache_hit_rate).toFixed(1)) + '%' : '—', '',
      `命中 ${fmtTok(t.cache_hit_tokens)} / 未命中 ${fmtTok(t.cache_miss_tokens)}`),
    kpi('平均延迟', fmtMs(t.avg_latency_ms)),
    kpi('平均速率', fmtRate(t.avg_tokens_per_second)),
  ].join('');
}

/* renderUsageChart Token 时序：prompt/completion 堆叠柱 + 均值参考线。 */
function renderUsageChart(series) {
  const box = $('usChart');
  if (!series || !series.length) {
    box.innerHTML = '<div class="empty">该时间范围内没有请求记录</div>';
    $('usChartNote').textContent = '—';
    return;
  }
  const list = series.slice(-72);
  const max = Math.max(1, ...list.map((s) => Number(s.total_tokens) || 0));
  const avg = list.reduce((x, s) => x + (Number(s.total_tokens) || 0), 0) / list.length;
  // 柱子高度直接算像素而不是交给 CSS 百分比：这段结构是
  // 「容器 → 列 → 柱」三层，列的高度本身就是百分比，再往下一层用
  // 百分比 height / flex-basis 都会被当成「父高度不确定」而解析为 0，
  // 结果是整张图空白但元素都在（连断言都容易骗过去）。
  // 容器高度由 CSS 决定，这里读实际值换算，改样式也不会失配。
  const boxH = box.clientHeight || 124;
  $('usChartNote').textContent = `${list.length} 个时间桶 · 峰值 ${fmtTok(max)} · 均值 ${fmtTok(Math.round(avg))}`;
  // 单根柱子至少 2px：用量分布极不均衡时（常见：一两小时有量、其余几乎为零，
  // 或某桶只有几十 token），按比例算出来的高度不到 1px，整张图看起来是空的——
  // 「这一格有没有请求」是这张图最基本的读法，不能因为比例小而丢掉。
  const bar = (v, total, span) => (v > 0 ? Math.max(2, span / total * v) : 0);
  box.innerHTML = list.map((s) => {
    const p = Number(s.prompt_tokens) || 0;
    const c = Number(s.completion_tokens) || 0;
    const t = Math.max(1, p + c);
    const h = (Number(s.total_tokens) || 0) / max * boxH;
    return `<div class="uschart-col" title="${esc(s.t || '')}｜prompt ${fmtTok(p)} / completion ${fmtTok(c)}｜${s.requests || 0} 请求">
      <i class="c" data-style="height:${bar(c, t, h).toFixed(1)}px"></i>
      <i class="p" data-style="height:${bar(p, t, h).toFixed(1)}px"></i>
    </div>`;
  }).join('') + `<div class="us-avg" data-style="bottom:${(avg / max * 100).toFixed(2)}%"></div>`;
  paintStyles(box);
}

const US_DIMS = {
  account: { key: 'by_account', title: '账号', realm: true, perf: true, span: 10 },
  model: { key: 'by_model', title: '模型', realm: false, perf: false, span: 8 },
  realm: { key: 'by_realm', title: '域', realm: false, perf: false, span: 8 },
};

function usSortField() {
  const v = $('usSort').value;
  return v === 'requests' ? 'requests' : v === 'errors' ? 'errors'
    : v === 'latency' ? 'avg_latency_ms' : 'total_tokens';
}

/* renderUsageDim 用量明细：账号/模型/域共用一张表（列数不同，表头一起换）。 */
function renderUsageDim() {
  const d = UP.usage || {};
  const dim = US_DIMS[UP.usDim] || US_DIMS.account;
  const field = usSortField();
  const rows = (d[dim.key] || []).slice()
    .sort((a, b) => (Number(b[field]) || 0) - (Number(a[field]) || 0));

  const tabs = [['account', '按账号', 'by_account'], ['model', '按模型', 'by_model'], ['realm', '按域', 'by_realm']];
  $('usDimTabs').innerHTML = tabs.map(([k, label, key]) =>
    `<button data-dim="${k}"${k === UP.usDim ? ' class="on"' : ''}>${label}<span class="cnt">${(d[key] || []).length}</span></button>`
  ).join('');

  $('usDimHead').innerHTML = `<tr><th>${esc(dim.title)}</th>`
    + (dim.realm ? '<th>域</th>' : '')
    + '<th class="num">请求</th><th class="num">失败</th>'
    + '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>'
    + (dim.perf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '')
    + '</tr>';

  $('usDimBody').innerHTML = rows.length ? rows.map((x) => {
    const p = Number(x.prompt_tokens) || 0;
    const c = Number(x.completion_tokens) || 0;
    const tot = Number(x.total_tokens) || 0;
    const name = UP.usDim === 'account' ? String(x.key || '').slice(0, 8) : (x.key || '—');
    return `<tr>
      <td><div class="nm">${esc(name)}</div>
        ${x.extra ? `<div class="sub">${esc(x.extra)}</div>` : ''}</td>
      ${dim.realm ? `<td class="sub">${esc(x.realm || '—')}</td>` : ''}
      <td class="num">${num(x.requests)}</td>
      <td class="num">${x.errors ? `<span class="err-text">${num(x.errors)}</span>` : '—'}</td>
      <td class="num">${fmtTok(p)}</td>
      <td class="num">${fmtTok(c)}</td>
      <td class="num">${fmtTok(tot)}${tot ? `<span class="mix"><i class="p" data-style="width:${(p / tot * 100).toFixed(1)}%"></i><i class="c" data-style="width:${(c / tot * 100).toFixed(1)}%"></i></span>` : ''}</td>
      ${dim.perf ? `<td class="num">${fmtMs(x.avg_latency_ms)}</td>
        <td class="num">${fmtRate(x.avg_tokens_per_second)}</td>` : ''}
    </tr>`;
  }).join('') : '';
  paintStyles($('usDimBody'));
  $('usEmpty').hidden = rows.length > 0;
  $('usEmpty').textContent = '该维度暂无用量数据';
  $('usDimNote').textContent = rows.length ? `${rows.length} 行 · 请求数含失败尝试` : '';
}

/* cacheRate 缓存命中率：颜色即健康度（≥90 绿 / 80–90 黄 / <80 红）。 */
function cacheRate(hit, miss) {
  const h = Number(hit) || 0;
  const m = Number(miss) || 0;
  const total = h + m;
  if (!total) return '<span class="dim">—</span>';
  const p = h / total * 100;
  const cls = p >= 90 ? 'ok' : (p >= 80 ? 'warn' : 'err');
  return `<span class="${cls === 'ok' ? '' : cls}-text" title="命中 ${fmtTok(h)} / 未命中 ${fmtTok(m)} tok"
    data-style="color:var(--${cls === 'err' ? 'err' : cls})">${trimFixed(p.toFixed(1))}%</span>`;
}

/* renderCreditDim 积分扣除历史：按账号 / 按模型两视。 */
function renderCreditDim() {
  const d = UP.usage || {};
  const byModel = UP.usCreditDim === 'model';
  // 配了单价才加这一列：没有时整列都是「—」，白占宽度。
  const cv = upCreditValue().cv;
  const spendTh = cv ? '<th class="num">花费估算</th>' : '';
  $('usCreditTabs').innerHTML = [['account', '按账号'], ['model', '按模型']].map(([k, label]) =>
    `<button data-cdim="${k}"${k === UP.usCreditDim ? ' class="on"' : ''}>${label}</button>`).join('');

  $('usCreditHead').innerHTML = byModel
    ? `<tr><th>模型</th><th class="num">请求</th><th class="num">扣除积分</th>
       <th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th>
       <th class="num">缓存命中率</th>${spendTh}</tr>`
    : `<tr><th>账号</th><th class="num">请求</th><th class="num">扣除积分</th>
       <th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th>
       <th class="num">缓存命中率</th>${spendTh}</tr>`;

  const list = byModel ? (d.credit_by_model || []) : (d.credit_by_account || []);
  const tot = list.reduce((x, r) => x + (Number(r.credits) || 0), 0);
  const reqs = list.reduce((x, r) => x + (Number(r.requests) || 0), 0);
  const toks = list.reduce((x, r) => x + (Number(r.credit_tokens) || 0), 0);
  $('usCreditStats').innerHTML = [
    card('维度条目', list.length),
    card('合计请求', num(reqs)),
    card('合计扣除积分', trimFixed(tot.toFixed(4))),
    // 与上方 KPI 同源（积分 × 单价），这里是按维度汇总后的口径
    card('合计花费估算', money(tot, cv) || '—'),
    card('合计有效样本 Token', fmtTok(toks)),
  ].join('');
  const ratio = (v, samples, tokens) => (!samples || !tokens) ? '—'
    : trimFixed(Number(v || 0).toFixed(4)) + ' / 1M';

  $('usCreditBody').innerHTML = list.length ? list.map((r) => `<tr>
    <td><div class="nm">${esc(UP.usCreditDim === 'account' ? String(r.key || '').slice(0, 8) : (r.key || '—'))}</div>
      ${r.nickname ? `<div class="sub">${esc(r.nickname)}</div>` : ''}
      ${r.realm ? `<div class="sub">${esc(r.realm)}</div>` : ''}</td>
    <td class="num">${num(r.requests)}</td>
    <td class="num">${trimFixed(Number(r.credits || 0).toFixed(4))}</td>
    <td class="num">${fmtTok(r.credit_tokens)}</td>
    <td class="num">${ratio(r.credits_per_1m_tokens, r.credit_samples, r.credit_tokens)}</td>
    <td class="num">${cacheRate(r.cache_hit_tokens, r.cache_miss_tokens)}</td>
    ${cv ? `<td class="num">${money(r.credits, cv) || '—'}</td>` : ''}
  </tr>`).join('') : '';
  paintStyles($('usCreditBody'));
  $('usCreditNote').textContent = list.length ? `${list.length} 行` : '暂无积分扣除记录';
}

async function saveUsageSnapshot() {
  if (!requireChan()) return;
  try {
    const r = await upPost('usage/save', {});
    toast('快照已保存', 'ok');
    showDrawer('保存快照结果', r);
  } catch (e) {
    toast('保存失败：' + e.message, 'err');
  }
}

/* ── 上游配置（结构化表单） ───────────────────────────── */

/* cfgGet/cfgSet 按点路径读写嵌套配置（`pool.max_in_flight`）。
   表单是扁平的 data-cfg 路径，配置是嵌套对象，中间需要这一层。 */
function cfgGet(o, path) {
  return path.split('.').reduce((x, k) => (x == null ? x : x[k]), o);
}

function cfgSet(o, path, val) {
  const ks = path.split('.');
  let x = o;
  for (let i = 0; i < ks.length - 1; i++) {
    if (typeof x[ks[i]] !== 'object' || x[ks[i]] === null) x[ks[i]] = {};
    x = x[ks[i]];
  }
  x[ks[ks.length - 1]] = val;
}

async function loadConfig(force) {
  if (!requireChan()) return;
  if (UP.cfg && !force) { renderConfig(); return; }
  try {
    const d = await upApi('config');
    // 上游把配置本体包在 config 键里（另带 ok / path）；兼容「直接就是配置对象」的其它实现
    UP.cfg = (d && d.config && typeof d.config === 'object') ? d.config : d;
    UP.cfgDirty = false;
    $('cfgPath').textContent = d.path || '上游配置文件';
    renderConfig();
  } catch (e) {
    $('cfgPath').textContent = '读取配置失败：' + e.message;
  }
}

/* renderConfig 把缓存里的配置值灌进表单。
   只改值与勾选状态，不重建 DOM——重建会丢掉用户正在输入的焦点。 */
function renderConfig() {
  const d = UP.cfg || {};
  const showEye = $('cfgEye').checked;
  let n = 0;
  // 作用域是整个视图：表单按上游分成 5 组卡片，cfgForm 只是其中一组的容器
  document.querySelectorAll('#view-up-config [data-cfg]').forEach((el) => {
    const path = el.dataset.cfg;
    const ct = el.dataset.ct;
    const v = cfgGet(d, path);
    n++;
    if (ct === 'bool') { el.checked = !!v; return; }
    if (ct === 'hours') {
      el.value = Array.isArray(v) ? v.join(', ') : (v == null ? '' : String(v));
      return;
    }
    if (v == null) { el.value = ''; return; }
    if (ct === 'secret' && !showEye) { el.value = '••••••••'; return; }
    el.value = String(v);
  });
  const miss = [];
  Object.keys(d).forEach((k) => {
    if (typeof d[k] === 'object' && d[k] !== null && !Array.isArray(d[k])) {
      Object.keys(d[k]).forEach((kk) => {
        const p = `${k}.${kk}`;
        if (!document.querySelector(`#view-up-config [data-cfg="${p}"]`)) miss.push(p);
      });
    }
  });
  $('cfgHint').textContent = miss.length
    ? `已加载 ${n} 项 · 上游另有 ${miss.length} 项本表单未覆盖：${miss.slice(0, 6).join('、')}`
    : `已加载 ${n} 项配置（与上游配置字段一一对应）`;
}

async function saveConfig() {
  if (!UP.cfg) return;
  const draft = JSON.parse(JSON.stringify(UP.cfg));
  let bad = null;
  document.querySelectorAll('#view-up-config [data-cfg]').forEach((el) => {
    const path = el.dataset.cfg;
    const ct = el.dataset.ct;
    if (ct === 'bool') { cfgSet(draft, path, el.checked); return; }
    const raw = el.value;
    if (ct === 'secret' && /^•+$/.test(raw)) return; // 没改掩码 → 沿用原值
    if (ct === 'num' || ct === 'float') {
      if (String(raw).trim() === '') return;
      const x = ct === 'num' ? parseInt(raw, 10) : parseFloat(raw);
      if (!Number.isFinite(x)) { bad = bad || path; return; }
      cfgSet(draft, path, x);
    } else if (ct === 'hours') {
      const arr = String(raw).split(/[,，\s]+/).filter(Boolean).map((s) => parseInt(s, 10));
      if (arr.some((x) => !Number.isFinite(x))) { bad = bad || path; return; }
      cfgSet(draft, path, arr);
    } else {
      cfgSet(draft, path, raw);
    }
  });
  if (bad) { toast('字段 ' + bad + ' 的值不合法', 'err'); return; }
  if (!confirm('保存上游配置？部分字段需重启上游进程才生效。')) return;
  try {
    const r = await upPost('config', draft, 60000);
    UP.cfg = draft; UP.cfgDirty = false;
    toast('配置已保存', 'ok');
    showDrawer('保存配置返回', r);
    renderConfig();
  } catch (e) {
    toast('保存失败：' + e.message, 'err');
  }
}

/* ── 日志 / 请求监控 / 请求记录 ───────────────────────── */

async function loadUpLogs() {
  if (!requireChan()) return;
  const ch = $('ulCh').value;
  try {
    const d = await upApi('logs' + (ch ? '?channel=' + encodeURIComponent(ch) : ''));
    const list = d.entries || d.items || [];
    // 上游日志条目是 {ts, ch, text}：频道叫 ch 不叫 channel，正文叫 text 不叫 msg
    $('ulBox').innerHTML = list.length
      ? list.map((e) => {
        const lv = /error|失败|错误/i.test(e.text || '') ? 'err'
          : (/warn|冷却|熔断/i.test(e.text || '') ? 'warn' : 'info');
        return `<div class="logline"><span class="c-time">${when(e.ts || e.time)}</span>
          <span class="lv lv-${lv}">${lv === 'err' ? 'ERR' : lv === 'warn' ? 'WARN' : 'INFO'}</span>
          <span class="dim">[${esc(e.ch === 'task' ? '任务' : e.ch === 'chat' ? '对话' : e.ch === 'sys' ? '系统' : (e.ch || '-'))}]</span>
          ${esc(e.text || e.msg || '')}</div>`;
      }).join('')
      : '<div class="empty">暂无日志</div>';
    $('ulHint').textContent = `${list.length} 条`;
    if ($('ulFollow').checked) { const b = $('ulBox'); b.scrollTop = b.scrollHeight; }
  } catch (e) {
    $('ulBox').innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
  }
}

/* loadRequestMetrics 请求监控：进程内的实时计数与归档状态。 */
async function loadRequestMetrics() {
  if (!requireChan()) return;
  try {
    const m = await upApi('request_metrics');
    const a = m.archive || {};
    $('rmSummary').textContent = '服务启动于 ' + when(m.started_at);
    $('rmArchive').textContent = a.enabled
      ? `归档开启 · ${a.files || 0} 个文件 / ${fmtTok(a.bytes)} 字节` +
        (a.dropped_writes ? ` · 丢弃 ${a.dropped_writes} 次` : '')
      : '归档关闭 · 仅内存最近记录';
    $('rmCards').innerHTML = [
      card('已完成', m.completed),
      card('成功', m.succeeded, 'ok'),
      card('失败', m.failed, m.failed ? 'warn' : ''),
      card('在途', m.in_flight),
      card('成功率', m.success_rate != null ? trimFixed(Number(m.success_rate).toFixed(1)) + '%' : '—'),
      card('HTTP 成功率', m.http_success_rate != null ? trimFixed(Number(m.http_success_rate).toFixed(1)) + '%' : '—'),
      card('平均耗时', fmtMs(m.avg_duration_ms)),
    ].join('');
  } catch (e) {
    $('rmCards').innerHTML = `<div class="empty">请求监控不可用：${esc(e.message)}</div>`;
  }
}

async function loadUpRequests() {
  if (!requireChan()) return;
  const q = $('ulQ').value.trim();
  const outcome = $('ulOutcome').value;
  const limit = $('ulLimit').value || 200;
  const qs = new URLSearchParams({ limit });
  if (q) qs.set('q', q);
  if (outcome) qs.set('outcome', outcome);
  try {
    const d = await upApi('request_logs?' + qs.toString());
    const list = d.entries || d.items || d.requests || [];
    // 字段名以 wb2api 为准：duration_ms / total_tokens / request_id / client_ip
    $('ulReqBody').innerHTML = list.length ? list.map((r) => {
      const ok = r.ok !== false && r.outcome !== 'error';
      const o = r.outcome || (ok ? 'success' : 'error');
      const cls = o === 'success' || ok ? 'ok' : (o === 'interrupted' ? 'warn' : 'err');
      return `<tr>
        <td class="sub">${when(r.time || r.ts)}</td>
        <td><span class="badge ${cls}">${esc(o === 'success' ? '成功' : o === 'http_error' ? 'HTTP 错误'
          : o === 'stream_error' ? '流错误' : o === 'interrupted' ? '中断' : '失败')}</span>
          ${r.status ? `<span class="sub">${r.status}</span>` : ''}</td>
        <td class="sub mono">${esc(r.path || '—')}</td>
        <td class="nm">${esc(r.model || '—')}</td>
        <td class="sub">${esc(r.account || '—')}</td>
        <td class="sub mono">${esc(r.client_ip || '—')}</td>
        <td class="sub" title="${esc(r.user_agent || '')}">${esc((r.user_agent || '—').slice(0, 28))}</td>
        <td class="num">${r.duration_ms != null ? r.duration_ms + 'ms' : (r.dur_ms != null ? r.dur_ms + 'ms' : '—')}</td>
        <td class="num">${num(r.total_tokens != null ? r.total_tokens : r.tokens)}</td>
        <td class="num">${num(r.prompt_tokens)}</td>
        <td class="num">${num(r.completion_tokens)}</td>
        <td class="num">${r.credit_known === false ? '—' : num(r.credits)}</td>
        <td class="sub mono" title="${esc(r.request_id || '')}">${esc(String(r.request_id || r.id || '').slice(0, 14))}</td>
      </tr>`;
    }).join('') : '';
    $('ulReqEmpty').hidden = list.length > 0;
    $('ulReqEmpty').textContent = '暂无请求记录';
    $('ulReqHint').textContent = `${list.length} 条`
      + (d.limit ? `（上限 ${d.limit}）` : '');
  } catch (e) {
    $('ulReqBody').innerHTML = '';
    $('ulReqEmpty').hidden = false;
    $('ulReqEmpty').textContent = '查询失败：' + e.message;
  }
}

/* ── 添加账号（登录 / 导入） ──────────────────────────── */

/* fillAddAcctChan 填「目标平台」下拉。
 *
 * 列的是**全部**托管型平台，包括 account_count === 0（还没加账号、
 * 因而在列表里被隐藏）的那些——它们正是此刻最需要被选中的目标。
 * 拿不到就提示一句，不静默失败。
 */
function fillAddAcctChan() {
  const sel = $('addAcctChan');
  const managed = (Store.get().channels || []).filter((c) => c.source === 'managed');
  if (!managed.length) {
    sel.innerHTML = '<option value="">（还没有平台，请先新建）</option>';
    return;
  }
  const cur = sel.value;
  sel.innerHTML = managed.map((c) => {
    const n = c.account_count;
    const tag = n > 0 ? c.account_count + ' 个账号' : '尚未添加账号';
    return `<option value="${esc(c.name)}">${esc(c.display_name || c.name)}（${tag}）</option>`;
  }).join('');
  if (cur && managed.some((c) => c.name === cur)) sel.value = cur;
}
/* openAddAccount 打开「添加账号」弹层。
 *
 * 刻意**不再**要求先选中渠道：平台列表按「有没有账号」显示（无账号就隐藏），
 * 若这里还要求requireChan，用户就陷入死锁——平台看不见 -> 选不中 -> 加不了账号
 * -> 平台永远不出现。改成在弹层里用下拉选目标平台。
 */
function openAddAccount() {
  fillAddAcctChan();
  // 优先沿用当前已选中的渠道；没有或它已不在列表里，就用下拉的第一项。
  const sel = $('addAcctChan');
  if (UP.chan && [...sel.options].some((o) => o.value === UP.chan)) sel.value = UP.chan;
  if (sel.value) openUpView(sel.value, Store.get().view || 'up-accounts');
  UP.login = { state: '', url: '', realm: 'cn' };
  $('loginState').textContent = '未开始';
  $('loginState').className = 'login-state';
  $('loginUrl').value = '';
  $('btnLoginCopy').disabled = true;
  $('btnLoginOpen').disabled = true;
  $('btnLoginPoll').hidden = true;
  $('btnImportDo').hidden = true;
  $('addMsg').textContent = '';
  $('addModal').hidden = false;
}

async function loginStart() {
  if (!UP.chan) return;
  const realm = $('loginRealm').value;
  try {
    const d = await upPost('login/start', { realm }, 30000);
    UP.login = { state: d.state || '', url: d.url || d.auth_url || '', realm };
    $('loginUrl').value = UP.login.url;
    $('btnLoginCopy').disabled = !UP.login.url;
    $('btnLoginOpen').disabled = !UP.login.url;
    $('btnLoginPoll').hidden = !UP.login.state;
    $('loginState').textContent = '等待你在浏览器完成登录…';
    $('loginState').className = 'login-state waiting';
  } catch (e) {
    $('loginState').textContent = '失败：' + e.message;
    $('loginState').className = 'login-state err';
  }
}

async function loginPoll() {
  if (!UP.login.state) return;
  try {
    const d = await upApi('login/poll?state=' + encodeURIComponent(UP.login.state), { timeout: 30000 });
    if (d.done) {
      $('loginState').textContent = '登录成功，账号已加入池';
      $('loginState').className = 'login-state ok';
      toast('账号已添加', 'ok');
      $('btnLoginPoll').hidden = true;
      $('addMsg').textContent = '';
      setTimeout(() => { $('addModal').hidden = true; }, 900);
      await loadUpAccounts();
    } else {
      $('loginState').textContent = '等待中：' + (d.message || '尚未完成登录');
      $('loginState').className = 'login-state waiting';
    }
  } catch (e) {
    $('loginState').textContent = '轮询失败：' + e.message;
    $('loginState').className = 'login-state err';
  }
}

async function doImport() {
  if (!UP.chan) return;
  const text = $('importText').value.trim();
  if (!text) { toast('请先粘贴要导入的 JSON', 'err'); return; }
  let payload;
  try { payload = JSON.parse(text); } catch (e) {
    toast('JSON 解析失败：' + e.message, 'err'); return;
  }
  try {
    const r = await upPost('import/cockpit', payload, 60000);
    showDrawer('导入结果', r);
    toast('导入完成', 'ok');
    $('addModal').hidden = true;
    await loadUpAccounts();
  } catch (e) {
    toast('导入失败：' + e.message, 'err');
  }
}

/* ── 视图加载分发 ─────────────────────────────────────── */

const UP_LOADERS = {
  'up-accounts': loadUpAccounts,
  'up-tasks': loadTasks,
  'up-models': loadModels,
  'up-packages': loadPackages,
  'up-usage': loadUsage,
  'up-config': loadConfig,
  'up-logs': async () => { await loadRequestMetrics(); await loadUpLogs(); await loadUpRequests(); },
};

function onUpView(view) {
  const fn = UP_LOADERS[view];
  if (fn) fn(true);
}

/* 上游视图内的所有事件（一次绑定，委托） */
function bindUpstream() {
  // ── 账号表操作
  $('accBody').addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-op]');
    if (b) accOp(b.dataset.uid, b.dataset.op);
  });
  $('btnCheckinAll').onclick = () => batchOp('checkin_all', '全部签到');
  $('btnTravelAll').onclick = () => batchOp('travel_all', '旅行巡检');
  $('btnActivityAll').onclick = () => batchOp('activity_all', '活跃上报');
  $('btnKeepaliveAll').onclick = () => batchOp('keepalive_all', '全部保活');
  $('btnBalanceAll').onclick = () => batchOp('balance_all', '刷新全部余额');
  $('btnAddAccount').onclick = openAddAccount;
  // 目标平台下拉：改选就切渠道，后续授权/导入都发给它
  $('addAcctChan').onchange = (e) => {
    if (e.target.value) openUpView(e.target.value, Store.get().view || 'up-accounts');
  };

  // ── 任务
  $('btnScanAll').onclick = scanTasks;
  $('btnRunQueue').onclick = runQueue;
  $('btnVouchers').onclick = queryVouchers;
  $('tcBody').addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-top]');
    if (b) taskOp(b.dataset.task, b.dataset.top);
  });

  // ── 模型（筛选即时、输入防抖）
  let mdTimer = null;
  $('mdQ').addEventListener('input', () => {
    clearTimeout(mdTimer);
    mdTimer = setTimeout(() => { MD.q = $('mdQ').value.trim(); renderModels(); }, 120);
  });
  [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]
    .forEach(([id, key]) => {
      if (!$(id)) return;
      $(id).addEventListener('change', () => { MD[key] = $(id).value; renderModels(); });
    });
  $('mdReset').onclick = resetModelFilter;
  $('btnModelsReload').onclick = () => loadModels();

  // ── 积分构成
  $('btnPkReload').onclick = loadPackages;

  // ── 用量
  $('btnUsageReload').onclick = loadUsage;
  $('btnUsageSave').onclick = saveUsageSnapshot;
  $('usRange').addEventListener('change', loadUsage);
  $('usSort').addEventListener('change', renderUsageDim);
  $('usDimTabs').addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-dim]');
    if (!b) return;
    UP.usDim = b.dataset.dim;
    renderUsageDim();
  });
  $('usCreditTabs').addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-cdim]');
    if (!b) return;
    UP.usCreditDim = b.dataset.cdim;
    renderCreditDim();
  });

  // ── 配置
  ['btnCfgReload', 'btnCfgReset', 'btnCfgReload2'].forEach((id) => {
    if ($(id)) $(id).onclick = () => loadConfig(true);
  });
  ['btnCfgSave', 'btnCfgSave2'].forEach((id) => {
    if ($(id)) $(id).onclick = saveConfig;
  });
  $('cfgEye').addEventListener('change', renderConfig);
  $('view-up-config').addEventListener('change', () => { UP.cfgDirty = true; });

  // ── 日志
  $('btnUlReload').onclick = loadUpLogs;
  $('btnReqReload').onclick = loadUpRequests;
  $('ulCh').addEventListener('change', loadUpLogs);

  // ── 添加账号弹层
  $('addTabs').addEventListener('click', (ev) => {
    const b = ev.target.closest('.seg-b');
    if (!b) return;
    document.querySelectorAll('#addTabs .seg-b').forEach((x) => x.classList.toggle('on', x === b));
    $('addTabLogin').hidden = b.dataset.at !== 'login';
    $('addTabImport').hidden = b.dataset.at !== 'import';
    $('btnLoginPoll').hidden = b.dataset.at !== 'login' || !UP.login.state;
    $('btnImportDo').hidden = b.dataset.at !== 'import';
  });
  $('btnLoginStart').onclick = loginStart;
  $('btnLoginPoll').onclick = loginPoll;
  $('btnImportDo').onclick = doImport;
  $('btnLoginCopy').onclick = async () => {
    try { await copyText(UP.login.url); toast('链接已复制', 'ok'); }
    catch (e) { toast('复制失败', 'err'); }
  };
  $('btnLoginOpen').onclick = () => {
    // 授权链接必须用系统浏览器打开（应用内 WebView 打不了第三方登录页）
    desktopOpenExternal(UP.login.url);
  };

  // ── 抽屉：结果 / 原始返回 切换
  $('drawerTabs').addEventListener('click', (ev) => {
    const b = ev.target.closest('.seg-b');
    if (!b) return;
    document.querySelectorAll('#drawerTabs .seg-b').forEach((x) => x.classList.toggle('on', x === b));
  });
}
