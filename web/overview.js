/* overview.js 概览面板：只放「本机转发」的用量。
 *
 * 口径：每次 /v1 转发都记了 token 与缓存，费用 = token × 价格表。
 * 这是**估算**——ModelMux 只是网关，不知道上游实际扣了多少
 * （中转站倍率、协议价、赠送额度都会让两者不等）。
 * 拿不到价格时显示「价格未知」而不是 $0.00。
 *
 * 为什么不在这里放平台账号（积分型平台）的真实用量：那是**另一个口径**
 * 的数据（上游自己的积分账面，不需要我们估算），而且它是「某个渠道」的
 * 细节，属于控制台的职责。概览只回答「经过我的流量有多少」这一个问题；
 * 把两种口径并排放在首屏，最容易让人误以为可以把它们相加。
 * 某个平台的积分消耗与花费，去侧栏「控制台 → 用量」看。
 *
 * 缓存命中率在分母为 0 时显示「—」——那是「没数据」，
 * 与「命中率真的是 0」必须区分开。
 */

const MT = {
  data: null,      // /api/metrics 的响应
  loading: false,
  err: '',
  fx: 6.71,        // 美元→人民币折算率（取自响应，缺省用最近采集值）
};

/* ── 取数 ─────────────────────────────────────────────── */

async function loadMetrics(force) {
  const days = $('mtRange').value;
  if (MT.loading && !force) return;
  MT.loading = true;
  MT.err = '';
  renderMetrics();
  try {
    MT.data = await api('/api/metrics?days=' + encodeURIComponent(days), { timeout: 20000 });
    if (MT.data.local && Number(MT.data.local.usd_cny) > 0) {
      MT.fx = Number(MT.data.local.usd_cny);
    }
  } catch (e) {
    MT.data = null;
    MT.err = e.message;
  }
  MT.loading = false;
  renderMetrics();
}

/* ── 渲染 ─────────────────────────────────────────────── */

/* ── 面板折叠 ─────────────────────────────────────────── */

const FOLD_KEY = 'mm_folds';

function readFolds() {
  try { return JSON.parse(localStorage.getItem(FOLD_KEY) || '{}'); } catch (e) { return {}; }
}

function writeFolds(o) {
  try { localStorage.setItem(FOLD_KEY, JSON.stringify(o)); } catch (e) { /* 隐私模式忽略 */ }
}

/* foldCard 把一个用量块包成独立可收起面板。
   key 必须稳定（同渠道每次渲染都相同），否则收起状态记不住。 */
function foldCard(key, tagCls, tagText, subText, bodyHtml) {
  return `<section class="card fold${readFolds()[key] ? ' closed' : ''}" data-section="${key}">
    <div class="card-hd fold-hd" data-fold-toggle="${key}">
      <span class="mt-tag ${tagCls}">${esc(tagText)}</span>
      <span class="mt-sub">${esc(subText)}</span>
      <i class="caret">▾</i>
    </div>
    <div class="fold-bd">${bodyHtml}</div>
  </section>`;
}

/* bindFolds 每次重渲染后重新绑定：面板内容由 innerHTML 重写，
   旧节点上的监听器随节点一起没了。 */
function bindFolds(root) {
  (root || document).querySelectorAll('[data-fold-toggle]').forEach((hd) => {
    hd.onclick = () => {
      const card = hd.closest('.fold, .card');
      const key = hd.dataset.foldToggle;
      if (!card || !key) return;
      const closed = card.classList.toggle('closed');
      const cur = readFolds();
      if (closed) cur[key] = 1; else delete cur[key];
      writeFolds(cur);
    };
  });
}

/* 静态卡片（对外出口 / 渠道概况）：把 card-hd 变成折叠头，
   它之后的所有兄弟节点收进 body。在 DOM 上做而不是改渲染代码，
   因为这两张卡的内部结构由 renderStatus 直接写 innerHTML。 */
function setupStaticFolds() {
  document.querySelectorAll('[data-collapsible]').forEach((card) => {
    if (card.dataset.foldReady) return;
    card.dataset.foldReady = '1';
    const hd = card.querySelector(':scope > .card-hd');
    if (!hd) return;
    const body = document.createElement('div');
    body.className = 'fold-bd';
    while (hd.nextSibling) body.appendChild(hd.nextSibling);
    card.appendChild(body);
    card.classList.add('fold');
    hd.classList.add('fold-hd');
    hd.dataset.foldToggle = card.dataset.fold || '';
    hd.insertAdjacentHTML('beforeend', '<i class="caret">▾</i>');
    if (readFolds()[card.dataset.fold]) card.classList.add('closed');
  });
  bindFolds(document);
}

function renderMetrics() {
  const body = $('mtBody');
  if (!body) return;

  if (MT.loading && !MT.data) {
    body.innerHTML = '<div class="empty">正在读取用量…</div>';
    $('mtHint').textContent = '读取中';
    return;
  }
  if (MT.err) {
    body.innerHTML = `<div class="empty err-state">
      <div>指标读取失败：${esc(MT.err)}</div>
      <div class="fhint">这不影响转发——概览只是读不到统计，不是网关出错。</div>
      <button class="ghost" onclick="loadMetrics(true)">重试</button></div>`;
    $('mtHint').textContent = '读取失败';
    return;
  }
  if (!MT.data) {
    body.innerHTML = '<div class="empty">尚未加载</div>';
    return;
  }

  const d = MT.data;
  const local = d.local || {};
  const blocks = [];

  // 本机转发没有记录 → 直接说「还没有用量」，而不是渲染一堆 0。
  // 不再看上游有没有数据：概览只管转发口径，上游有量不代表
  // 「经过本机的流量」有量。
  const hasAny = !!local.has_data;
  if (!hasAny) {
    body.innerHTML = `<div class="empty">
      <div>所选时间范围内还没有调用记录</div>
      <div class="fhint">用任意 OpenAI 兼容客户端调用 <code>${esc((S.status && S.status.base_url) || '')}/v1</code> 后点「刷新指标」。</div>
    </div>`;
    $('mtHint').textContent = '暂无数据';
    return;
  }

  // 只有本机转发一块。平台账号的指标在控制台「用量」视图里，
  // 这里不再重复——首屏并排两个口径，最容易让人把它们相加。
  blocks.push(localBlock(local, d.account_spend, d.local ? d.local.usd_cny : 0));

  body.innerHTML = blocks.join('');
  paintStyles(body);
  bindFolds(body);
  $('mtHint').textContent = metaHint(local);
}

/* metaHint 顶部一句话说明：数据覆盖范围 + 价格表时间。
   只说本机转发口径——概览不再展示平台账号的数据，
   在这里提「几个平台有数据」会让人去找并不存在的区块。 */
function metaHint(local) {
  const parts = [];
  const days = (local.days || 0);
  if (local.range_start) {
    parts.push(local.range_start === local.range_end
      ? local.range_start
      : `${local.range_start} ~ ${local.range_end}`);
  } else if (days) {
    parts.push(`近 ${days} 天`);
  }
  if (local.restored) parts.push('含重启前历史');
  if (local.price_date) parts.push(`价格表 ${local.price_date}`);
  if (local.usd_cny) parts.push(`汇率 1USD≈${local.usd_cny}（${local.fx_date || ''}）`);
  return parts.join(' · ') || '-';
}

/* ── 本机转发指标块 ───────────────────────────────────── */

function localBlock(local, accountSpend, fxCny) {
  const t = local.total || {};
  const cost = t.cost || {};
  const known = cost.known === true;
  const fx = Number(fxCny) > 0 ? Number(fxCny) : 6.71;

  // 费用拆成两张卡，口径不同不能合并：
  //   API 型费用  = token × 厂商公开 API 价（美元） × 汇率 → 人民币估算
  //   积分型费用  = 积分 × 平台积分单价 → 人民币（真实账面折算）
  // 两者相加没有意义：前者是「直连要花多少」的对照，后者是「实际花了多少」。
  const hasApiUsage = (local.by_channel || []).some((c) => c.source !== 'managed');
  const apiCostValue = !hasApiUsage
    ? '<b class="na">—</b>'
    : (!known ? '<b class="na">价格未知</b>' : `<b>¥${flt(cost.total * fx)}</b>`);
  const apiCostSub = !hasApiUsage
    ? '当前只有积分型平台的用量'
    : (!known
      ? 'API 型平台的模型不在价格表内，可在渠道里填自定义单价'
      : (cost.complete === false
        ? `部分模型价格未知；按 1USD≈${fx} 折算，只是已知部分`
        : `按 1USD≈${fx} 折算 · 输入 ¥${flt(cost.input * fx)} · 输出 ¥${flt(cost.output * fx)}`));

  const ac = accountSpend || {};
  const acctCostValue = !ac.known
    ? '<b class="na">—</b>'
    : `<b>¥${flt(ac.spend)}</b>`;
  const acctCostSub = !ac.known
    ? '暂无积分型用量'
    : (!ac.complete
      ? `部分渠道未取到积分单价；已汇总 ${ac.channels}/${ac.channels_total} 个渠道`
      : `积分 ${flt(ac.credits)} × 平台单价 · ${ac.channels} 个渠道`);

  const rate = t.cache_hit_rate_known
    ? pct(t.cache_hit_rate)
    : '<b class="na">—</b>';
  const rateSub = t.cache_hit_rate_known
    ? `命中 ${n0(t.cached_tokens)} / 输入 ${n0(t.prompt_total)}`
    : '上游未回报缓存用量';

  const cards = [
    { label: 'API 型费用（估算）', value: apiCostValue, sub: apiCostSub, cls: known ? '' : 'na' },
    { label: '积分型费用（估算）', value: acctCostValue, sub: acctCostSub, cls: ac.known ? 'accent' : 'na' },
    { label: '调用次数', value: `<b>${n0(t.requests)}</b>`,
      sub: t.errors ? `失败 ${n0(t.errors)}` : '本机转发口径', cls: t.errors ? 'warn' : '' },
    { label: '缓存命中率', value: rate, sub: rateSub,
      cls: t.cache_hit_rate_known ? 'ok' : 'na' },
    { label: 'Token 总量', value: `<b>${n0(t.total_tokens)}</b>`,
      sub: `输出 ${n0(t.output_tokens)} · 平均 ${n0(t.avg_latency_ms)}ms` },
  ];

  const unknown = local.unknown_models || [];
  const warn = unknown.length
    ? `<div class="warn-line">以下模型没有价格，其用量未计入费用：${unknown.map(esc).join('、')}</div>`
    : '';

  const chRows = (local.by_channel || []).map(chRow).join('');
  const daily = (local.daily || []);
  const trend = daily.length > 1 ? trendBlock(daily) : '';
  const recent = recentBlock(local.recent || []);

  // 标题按「这里到底有哪些渠道」说话，而不是写死「API 型平台」。
  //
  // 之前写死是有害的：经本进程转发的也包括积分型平台（那是它对外的入口），
  // 把它们标成「API 型平台」会让人以为这个费用是上游直接收的，
  // 而实际上积分型平台的真实积分消耗在控制台「用量」视图里。
  const srcs = (local.by_channel || []).map((c) => c.source);
  const hasEmbedded = srcs.indexOf('embedded') >= 0;
  const hasManaged = srcs.indexOf('managed') >= 0;
  const tag = hasEmbedded && hasManaged ? '本机转发'
    : (hasManaged ? '本机转发（积分型平台）' : 'API 型平台');
  const sub = hasEmbedded
    ? '经 ModelMux 转发的用量 · 费用按 token 估算'
    : '经 ModelMux 转发的用量 · 费用按 token 估算（这些是积分型平台的对外入口）';

  return foldCard('mt-local', '', tag, sub, `
    <div class="cards">${cards.map(mtCard).join('')}</div>
    ${warn}
    ${trend}
    ${chRows ? `<div class="tablewrap"><table class="dt">
      <thead><tr><th>渠道</th><th>类型</th><th>调用</th><th>Token</th><th>缓存命中</th><th>API 费用</th></tr></thead>
      <tbody>${chRows}</tbody></table></div>` : ''}
    ${recent}`);
}

function chRow(c) {
  const cost = c.cost || {};
  // 费用列的三种状态必须可区分：
  //   API 型有价格  → 显示美元估算
  //   API 型无价格  → 「价格未知」（不显示 $0.00）
  //   积分型        → 「积分口径」——它的花费本就不用美元计，
  //                   真实账面在控制台「用量」视图里（积分 + 花费估算）
  //
  // 原先写「积分口径 ↓」并指向概览下方的分区；那个分区已移进控制台，
  // 箭头方向不再成立。指引改放进 tooltip：单元格里塞整句话会撑乱表格，
  // 而「这条为什么没有金额」是看到时才需要的解释。
  const costTxt = c.source === 'managed'
    ? '<span class="na" title="平台账号的花费以积分计，见侧栏「控制台 → 用量」">积分口径</span>'
    : (cost.known ? `¥${cny(cost.total, MT.fx)}` : '<span class="na">价格未知</span>');
  const rate = c.cache_hit_rate_known ? pct(c.cache_hit_rate) : '<span class="na">—</span>';
  return `<tr>
    <td class="nm">${esc(c.name)}</td>
    <td><span class="badge muted">${c.source === 'managed' ? '积分型' : 'API 型'}</span></td>
    <td class="credits">${n0(c.requests)}${c.errors ? ` <span class="warn-txt">/${n0(c.errors)}</span>` : ''}</td>
    <td class="credits">${n0(c.total_tokens)}</td>
    <td class="credits">${rate}</td>
    <td class="credits">${costTxt}</td>
  </tr>`;
}

/* trendBlock 简易柱状趋势（纯 CSS，不引图表库）。 */
function trendBlock(daily) {
  const max = Math.max.apply(null, daily.map((d) => d.total_tokens || 0));
  if (max <= 0) return '';
  const bars = daily.map((d) => {
    const h = Math.max(3, Math.round(((d.total_tokens || 0) / max) * 100));
    const tip = `${d.date} · ${n0(d.total_tokens)} token · ${n0(d.requests)} 次调用`;
    return `<div class="trend-col" title="${esc(tip)}">
      <div class="trend-bar" data-style="height:${h}%"></div>
      <span class="trend-lbl">${esc(d.date.slice(5))}</span></div>`;
  }).join('');
  return `<div class="trend">${bars}</div>`;
}

function recentBlock(list) {
  if (!list.length) return '';
  const rows = list.slice(0, 8).map((r) => `<tr>
    <td class="sub">${esc(r.time)}</td>
    <td>${r.ok ? '<span class="badge ok">成功</span>'
      : `<span class="badge err">失败</span>`}</td>
    <td class="nm">${esc(r.model)}</td>
    <td class="sub">${esc(r.channel)}</td>
    <td class="credits">${n0(r.total_tokens)}</td>
    <td class="credits">${r.cost_known ? '¥' + cny(r.cost, MT.fx) : '<span class="na">—</span>'}</td>
  </tr>`).join('');
  return `<div class="mt-sec">最近调用</div>
    <div class="tablewrap"><table class="dt">
      <thead><tr><th>时间</th><th>结果</th><th>模型</th><th>渠道</th>
        <th>Token</th><th>费用</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

/* ── 小组件 ───────────────────────────────────────────── */

function mtCard(c) {
  return `<div class="card-k ${c.cls || ''}">
    <span>${esc(c.label)}</span>
    ${c.value}
    <div class="k-sub">${c.sub || ''}</div>
  </div>`;
}

/* cny 把美元费用按汇率折成人民币显示。
 *
 * 为什么在显示层折算而不是后端直接算：价格表以美元报价是厂商的事实，
 * 汇率是另一个会变的量。分开存，面板能分别标注「价格表 2026-10」
 * 与「汇率 2026-10-02」，费用不对时用户能分清该怪哪个。
 */
function cny(usd, fx) {
  return flt((Number(usd) || 0) * (Number(fx) > 0 ? Number(fx) : 6.71));
}

/* 费用显示：小于 1 分钱时保留更多位，避免全部显示 $0.00。 */
function flt(v) {
  const n = Number(v) || 0;
  if (n === 0) return '0.00';
  if (Math.abs(n) < 0.01) return n.toFixed(4);
  if (Math.abs(n) < 1) return n.toFixed(3);
  return n.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function pct(v) {
  const n = Number(v) || 0;
  // 上游可能给 0–1 的比例，也可能给 0–100 的百分数。
  const r = n > 1 ? n : n * 100;
  return `<b>${r.toFixed(1)}%</b>`;
}

/* n0 是本文件专用的整数格式化。
 *
 * 不复用 upstream.js 的 n0()：那边对 null 返回「—」（用于表格里
 * 「这个字段上游没给」），而概览的计数类字段缺失就该显示 0——
 * 「0 次调用」和「不知道」在计数语境下不是一回事，
 * 把它们混成同一个符号会让人以为数据没加载出来。
 */
function n0(v) {
  const n = Number(v);
  if (!Number.isFinite(n)) return '0';
  return Math.round(n).toLocaleString('zh-CN');
}
