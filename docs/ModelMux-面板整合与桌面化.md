# ModelMux 面板整合与桌面化改造说明

> 版本 v1.0 · 日期 2026-10-02
> 范围：① 对照原 WorkBuddy 2 API 面板补全全部功能 ② 网页式界面改为桌面应用式形态

---

## 一、功能对照表（35 个接口逐一核对）

原 `workbuddy2api-panel` 面板共 **37 条路由**（2 条是页面本身，**35 条 API**）。
改造前 ModelMux 只覆盖 **6 条**；本次全部补齐，并新增代理层统一转发。

### 1.1 账号池（`/panel/api/overview` + 账号操作）

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 1 | `GET overview` | 上游控制台 → 账号池（统计卡 + 账号表） | ✅ 已有，保留 |
| 2 | `POST accounts/{uid}/disable` | 账号表行内「停用」 | ✅ 已有 |
| 3 | `POST accounts/{uid}/revive` | 账号表行内「启用」（停用态才显示） | ✅ 已有 |
| 4 | `POST accounts/{uid}/checkin` | 账号表行内「签到」/「今日已签」徽章 | ✅ 已有 |
| 5 | `POST accounts/{uid}/balance` | 账号表行内「刷新余额」 | ✅ 已有 |
| 6 | `POST accounts/{uid}/remove` | 账号表行内「移除」（带确认） | ✅ 已有 |
| 7 | `POST checkin_all` | 账号池工具条「全部签到」 | 🆕 补回 |
| 8 | `POST travel_all` | 账号池工具条「旅行巡检」 | 🆕 补回 |
| 9 | `POST activity_all` | 账号池工具条「活跃上报」 | 🆕 补回 |
| 10 | `POST keepalive_all` | 账号池工具条「全部保活」 | 🆕 补回 |
| 11 | `POST balance_all` | 账号池工具条「刷新全部余额」 | 🆕 补回 |
| 12 | `GET accounts/{uid}/tasks` | 账号表行内「任务」→ 右侧抽屉 | 🆕 补回 |
| 13 | `POST accounts/{uid}/tasks/accept` | 任务抽屉内操作 | 🆕 补回 |
| 14 | `POST accounts/{uid}/tasks/accept_all` | 任务抽屉内操作 | 🆕 补回 |
| 15 | `POST accounts/{uid}/tasks/claim` | 任务抽屉内操作 | 🆕 补回 |
| 16 | `POST accounts/{uid}/tasks/auto` | 任务抽屉内操作 | 🆕 补回 |
| 17 | `POST accounts/{uid}/tasks/auto_all` | 任务抽屉内操作 | 🆕 补回 |

账号表列：账号 / 状态 / 积分 / 成功·失败 / 在途 / 最近成功 / 操作（与原面板一致，另加「任务」入口）。

### 1.2 任务中心

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 18 | `GET tasks/queue` | 上游控制台 → 任务中心（队列表 + 统计卡） | 🆕 补回 |
| 19 | `POST tasks/scan_all` | 「扫描待办」 | 🆕 补回 |
| 20 | `POST tasks/run_queue` | 「执行全部待办」（带进度条） | 🆕 补回 |
| 21 | `GET school/vouchers` | 「查询券码」→ 抽屉 | 🆕 补回 |

### 1.3 模型与档位

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 22 | `GET models` | 上游控制台 → 模型与档位（表格 + 过滤 + 排序） | 🆕 补回 |
| 23 | `GET model_probes` | 模型页 hint 里的探测结果 | 🆕 补回 |

列：模型 / 积分倍率 / 默认档 / 思考档位 / 上下文 / 最大输出。
字段做了多实现兼容：倍率取 `rate`｜`multiplier`｜`credits`（`"x0.29"` 字符串），思考档取 `supported_efforts`｜`efforts`。

### 1.4 积分构成

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 24 | `GET packages` | 上游控制台 → 积分构成（统计卡 + 套餐表） | 🆕 补回 |

### 1.5 用量

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 25 | `GET usage` | 上游控制台 → 用量（按账号/模型/域 + 排序 + 时间范围） | 🆕 补回 |
| 26 | `POST usage/save` | 「保存快照」 | 🆕 补回 |

### 1.6 上游配置

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 27 | `GET config` | 上游控制台 → 配置（结构化表单：标量 + 嵌套 JSON） | 🆕 补回 |
| 28 | `POST config` | 「保存配置」（带确认，失败定位到字段） | 🆕 补回 |

密钥默认掩码，「显示密钥」开关控制；未修改的掩码字段提交时沿用原值。
返回结构 `{config:{}, ok, path}` 已正确解包。

### 1.7 运行日志与请求记录

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 29 | `GET logs` | 上游控制台 → 运行日志（频道筛选 + 自动滚动） | 🆕 补回 |
| 30 | `GET request_metrics` | 请求指标（hint 区展示） | 🆕 补回 |
| 31 | `GET request_logs` | 请求记录表（q/outcome/limit 筛选） | 🆕 补回 |

### 1.8 添加账号

| # | 上游 API | 面板入口 | 状态 |
|---|---|---|---|
| 32 | `POST login/start` | 添加账号 → 浏览器登录 → 「获取授权链接」 | 🆕 补回 |
| 33 | `GET login/poll` | 「检查登录状态」（可重复点击） | 🆕 补回 |
| 34 | `GET login/regions` | 区域下拉（cn / global） | 🆕 补回 |
| 35 | `POST import/cockpit` | 添加账号 → 导入 JSON → 「导入」 | 🆕 补回 |

**免密钥**：授权链接必须用系统浏览器打开（应用内 WebView 打不了第三方登录页），
为此新增桌面桥 `window.mmOpenExternal(url)`。

### 1.9 代理层（ModelMux 侧新增，非上游接口）

| 接口 | 作用 |
|---|---|
| `GET|POST /api/channels/{name}/upstream/{path...}` | 统一转发到托管型上游的管理 API，**服务端注入渠道密钥** |
| `GET /api/access-key` · `POST /api/access-key/regenerate` | 对外 API Key 查看/再生成 |
| `POST /api/settings/port` | 监听端口热切换 |

---

## 二、原代码的保留与移除

### 保留（ModelMux 独有，不动）

| 位置 | 内容 | 理由 |
|---|---|---|
| `internal/provider/*` | 路由、Key 池、协议转换（chat/anthropic）、保真直通 | 聚合网关的核心，与面板无关 |
| `internal/web/v1.go` | OpenAI 兼容出口 | 对外契约 |
| `internal/desktop/*` | 托盘、窗口生命周期、单实例、WebView2 壳 | 桌面形态基础 |
| `internal/orchestrator/*` | 子进程生命周期、动态端口 | 托管型渠道的运行时 |
| `internal/logging/*` | slog 双通道 + 结构化环形缓冲 | 日志基建 |
| `internal/web/upstreams.go` | 配置 + 子进程状态 → 可路由上游 | 2s 指纹同步 |

### 移除 / 替换

| 原形态 | 现在 | 理由 |
|---|---|---|
| 顶栏 `<nav class="tabs">` + `location.hash` 路由 | 侧栏 `data-view` ↔ `#view-x` **slot 绑定** | 去掉浏览器页��语义（见 §3） |
| `history.replaceState('#'+name)` | 删除 | 桌面应用不需要历史记录 |
| `showPage()` 里 `fetch('/api/...')` 相对路径 | 保留（仍是同源本地服务） | 这是**本地服务**不是远程网站；DSH 也用 `dsh-app://` 同源转发 |
| 单文件大 `app.js`（1158 行，含全部 UI 逻辑） | 拆成 `app.js`（框架/渠道/日志/设置）+ `upstream.js`（上游控制台六视图） | 单一职责，便于维护 |
| 渠道行内「打开面板」跳转上游 | **删除**，改为「控制台」进入内置上游控制台 | 消除「面板里再开面板」 |
| 概览页里的渠道管理区 | 移到「平台账号型」区块 + 上游控制台 | 概览只做总览 |
| 内嵌 `<style>` / `style="..."` | 全部 class + CSS 变量 | CSP `style-src 'self'` 会静默丢弃内联样式 |

### 未被采纳的做法（及原因）

- **直接搬用 wb2api 的 3853 行前端**：它的登录态、主题、i18n 都与 ModelMux 重复，且要直连上游密钥。改为「结构对齐 + 数据经代理」，UI 复用 ModelMux 现有设计令牌。
- **把上游面板 HTML iframe 进来**：那仍是「面板里开面板」，且同源策略下需要额外放行。

---

## 三、架构改造：各模块调整范围

参照 **DeepSeek Harness（dsh）** 桌面端（Electron 44 + Cordis，调研结论附后）。

### 3.1 窗口与布局

| 项 | 改造前 | 改造后 |
|---|---|---|
| 结构 | 顶栏 tabs + 整页 main | **侧栏 268px（可收 56px）+ 主内容区**，DSH 同款 |
| 侧栏 | 无 | 品牌区 / 分组导航（渠道·上游控制台·运维）/ 底部状态灯 |
| 折叠 | 无 | `btnCollapse` 切换 `.collapsed` |
| 顶栏 | 品牌 + tabs + stats | 折叠钮 + 标题 + meta + 主题 + 刷新 + **随视图变化的主操作钮** |
| 状态指示 | 顶部数字 | 侧栏底部 pulse 灯（就绪/告警）+ `n/m 渠道 · x 模型` |

### 3.2 导航（去路由）

- 侧栏 `<a class="nav-i" data-view="x">` ↔ 主面板 `<section id="view-x">` **同 id 双向绑定**
- `setView(name)` 只切 `hidden` + 侧栏高亮，**不写 history/hash**
- 视图懒加载：切到某视图才拉该视图的数据
- 实测 `location.hash === ''`

### 3.3 状态管理

- 集中 `Store`（快照 + 订阅），视图只读快照
- `const S` 是 **Proxy 活视图**而非快照（否则 `Store.set` 替换对象后 `S.xxx` 读到旧值——表现为侧栏计数恒为 0）
- 日志缓冲仍是本地定长 + 增量拉取

### 3.4 本地资源加载

- 三个前端文件全部 **`go:embed`** 进 exe：无外部文件依赖、无 `file://`、无 CDN
- 仍走 `http://127.0.0.1:<动态端口>/` —— 这是**本机回环服务**不是网页站点，
  与 DSH 的 `dsh-app://app/` + 转发本地 HTTP 是同一思路（静态与 API 同源，无 CORS）
- 窗口内不存在 `<a href>` 页面跳转；外部链接一律走桌面桥

### 3.5 桌面桥（新增）

| 注入 | 用途 | 回退 |
|---|---|---|
| `window.mmShowWindow()` | 唤出驻留托盘时的主窗口 | `chrome.webview.postMessage` → `window.open` |
| `window.mmOpenExternal(url)` | 系统浏览器打开授权链接 | 同上 |

实现：`Shell.BindNative()`（go-webview2 `Bind`，须在 `Navigate` 之前调用）。

### 3.6 文件与模块调整范围

| 文件 | 变化 | 规模 |
|---|---|---|
| `internal/web/index.html` | 全面重写：slot 导航 + 12 视图 + 3 弹层 | 15.8K → 26K |
| `internal/web/app.js` | 重写：Store/slot/渠道/日志/设置/桌面桥 | 44K → 40K |
| `internal/web/upstream.js` | **新增**：上游控制台六视图逻辑 | 26K |
| `internal/web/app.css` | 追加桌面布局 + 表格 + 日志盒 + 配置表单 + toast | 17K → 33K |
| `internal/web/server.go` | embed upstream.js + 路由 | +12 行 |
| `internal/desktop/shell.go` | **新增** `BindNative()` | +25 行 |
| `main.go` | 调 `BindNative()` | +2 行 |
| `_e2e/fake_managed_gateway.py` | 补 11 个只读管理接口（供断言） | +55 行 |
| `_e2e/verify.py` | 补 12 条接口全覆盖断言 | 72 → 84 |

> **注（2026-10-03）**：本仓库当日做过两轮目录整理，本文（含上表与下节）的路径都是
> **当次改造**的实际路径，故保留原样：
> 1. 前端资源先从 `internal/web/` 迁到 `internal/assets/`（该目录只做 `go:embed`）；
>    随后又外置为仓库根的 `web/`（唯一真源），`src/internal/assets/` 降级为构建镜像，
>    由 `tools/release/build.py` 在编译前同步。
> 2. e2e 脚本从 `_e2e/` 迁到 `test/`；Go 源码从仓库根迁到 `src/`。
>
> 当前结构以仓库根 `README.md` 的「仓库结构」为准。

### 3.7 不变的部分（刻意）

- **仍在 127.0.0.1 上起 HTTP 服务**：这是 DSH 同款做法（它也转发到 `127.0.0.1:<随机端口>`）。
  去掉的不是「本地服务」，而是「网页路由 + 页面跳转 + 远程资源」这些浏览器语义。
- `/v1` 出口、鉴权、托盘、退出时序、单实例：全部不变。

---

## 四、验证

```
go build / go vet / go test      全干净（web 包 6 个集成测试）
_e2e/verify.py                   84/84（含 12 条上游接口全覆盖断言）
_e2e/verify_proxy_real.py        真实 wb2api：账号/概览/模型/配置实测通过
截图（真实数据）                 账号表 2 行 · 模型表 19 行 · 配置 13 项
界面自检                         location.hash === '' · 12 个 slot · 原生桥在 WebView2 内可用
```

实测覆盖到的真实数据：账号 2 个（终渊Null 2400/2400 已签、林渊NullPilirihm 911/1225）、
模型 19 个（倍率 ×0.06–×0.79、上下文最高 1,000,000）、配置 13 项可编辑。

---

## 五、DSH 架构调研要点（改造依据）

| DSH 做法 | ModelMux 的对应 |
|---|---|
| `dsh-app://app/` 自定义协议：静态本地 + API 转发同源 | `go:embed` 静态 + 127.0.0.1 同源 API |
| panel/slot 代替路由（侧栏 id ↔ main keyed slot） | `data-view` ↔ `#view-x` |
| preload + contextBridge 暴露原生能力 | go-webview2 `Bind` 注入 `mmShowWindow` / `mmOpenExternal` |
| 自研 store（快照 + 订阅，rAF 节流） | `Store` 快照 + 订阅 |
| 三栏 AppFrame + 侧栏可收起 56px | 侧栏 268→56px |
| 窗口固定尺寸、不持久化几何 | 保留 DPI 自适应（不做位置记忆） |
| 托盘常驻 + 二次启动聚焦 | 已实现（P1） |
