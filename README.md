# ModelMux（模汇）

把多个上游模型服务聚合成**本地一份 OpenAI Chat Completions 契约**的 Windows 桌面网关。

自带面板（内嵌 WebView2 桌面壳 + 系统托盘），负责渠道管理、用量计量与状态查看。
上游的协议差异全部在适配层里消化，对外只暴露一种协议。

```
                     ┌────────────────────────────┐
  OpenAI 客户端 ────▶│  /v1/chat/completions      │
  （任意 SDK）       │  /v1/models   ← 唯一对外契约│
                     └─────────────┬──────────────┘
                                   │ 按 model 前缀路由
                     ┌─────────────▼──────────────┐
                     │   src/internal/provider    │
                     │  协议适配 · Key 池 · 保真转发 │
                     └───┬────────────────────┬───┘
          ┌──────────────┘                    └──────────────┐
          ▼                                                  ▼
   ┌──────────────┐                              ┌────────────────────┐
   │  内嵌型渠道   │                              │   托管型 provider   │
   │ 本进程内转发  │                              │ 独立子进程 + 动态端口│
   │ 自定义/预设端点│                              │ 探活 · 断线重拉 · 杀树│
   └──────────────┘                              └────────────────────┘
```

## 快速开始

```bash
# 1) 构建（Windows / Go 1.25+ / 需 WebView2 运行时）
#    脚本会先把 web/ 同步到 src/internal/assets/ 再编译，保证外置与内嵌一致
python tools/release/build.py

# 2) 运行（桌面壳 + 托盘）
./ModelMux.exe
```

也可以直接调 Go（此时 `src/internal/assets/` 用的是上次同步的副本）：

```bash
go build -C src -trimpath -ldflags="-s -w -H windowsgui" -o ../ModelMux.exe .
```

启动后托盘图标右键 →「打开面板」，或在浏览器里访问启动日志打印的地址（默认 `http://127.0.0.1:1234/`）。
在面板「渠道」里添加你的上游，然后就能用 `access_key` 调 `/v1` 了：

```bash
curl http://127.0.0.1:1234/v1/chat/completions \
  -H "Authorization: Bearer <面板里复制的 access_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"<你的模型名>","messages":[{"role":"user","content":"hi"}]}'
```

## 仓库结构

根目录只有 exe 本体与按职责分开的文件夹——一个文件夹只放一类东西：

```
modelmux/
├─ ModelMux.exe          构建产物（go build 生成，唯一散落在根的文件）
├─ README.md             ← 你正在读的
├─ LICENSE
├─ .gitignore
│
├─ web/                  面板前端资源（唯一真源：HTML / JS / CSS）
├─ config/               用户配置（modelmux.json，首次启动自动生成）
├─ data/                 运行期数据（logs/ usage/ cache/ instances/ ports.json）
│
├─ src/                  Go 源码
│  ├─ main.go            启动顺序：配置 → 日志 → 单实例 → 端口 → 编排 → 服务 → 窗口/托盘
│  ├─ go.mod / go.sum
│  ├─ app.ico / appres.syso / winres/   PE 资源：图标、DPI 清单、版本信息
│  └─ internal/
│     ├─ config/         配置读写与归一化（未知协议明确报错，不猜默认值）
│     ├─ logging/        结构化日志：内存环 + 滚动文件
│     ├─ orchestrator/   托管型 provider 的子进程生命周期与端口分配（杀树、先等真退出）
│     ├─ provider/       内嵌型渠道：路由、协议适配、Key 池、连接测试
│     │  ├─ workbuddy/   内置原生实现（MIT 上游照搬内嵌）
│     │  ├─ trae/        内置原生实现（MIT 上游照搬内嵌）
│     │  └─ zcode/       内置原生实现（按契约**独立重写**，无上游代码）
│     ├─ native/         进程内原生装配层：按 kind 把上面的实现装进本进程
│     ├─ metrics/        用量计量与定价
│     ├─ claim/          限时套餐的定时领取
│     ├─ desktop/        WebView2 壳、托盘、单实例、有序退出
│     ├─ assets/         前端资源的**构建镜像**（由 web/ 同步而来，不要手改）
│     └─ web/            内置 HTTP 服务：对外 /v1 出口 + 对内 /api 控制面
│
├─ test/                 端到端验证脚本（假上游 + CDP 驱动真实界面）
│  └─ shot_tools/        界面截图（给人看的，不含断言）
├─ tools/                开发与发布期脚本
│  ├─ release/           构建、全量快照、公开副本导出
│  ├─ icon/              图标生成与比对
│  └─ win/               桌面集成：快捷方式、窗口截图
└─ docs/                 设计与改造记录
```

### 前端资源为什么有两份

`go:embed` **不允许 `..`**，而面板资源要放进 `internal/assets` 这个包才能被内嵌——
所以「根目录的 `web/`」和「`src/internal/assets/`」在物理上必须是两处。约定：

- **`web/` 是唯一真源**，改前端只改这里。
- `src/internal/assets/` 是**构建镜像**，由 `tools/release/build.py` 在编译前用 sha256 逐文件同步，不手工改。
- 运行时**优先读 exe 同级的 `web/`**（便于热改调试），读不到才回落到 exe 内嵌副本。
  也就是说：删掉 `web/` 应用照常工作，只是前端不能外置调整。

## 目录

- [仓库结构](#仓库结构)
- [设计约束](#设计约束)
- [两类渠道](#两类渠道)
- [已并入的 Provider](#已并入的-provider)
- [主要能力](#主要能力)
- [构建](#构建)
- [运行与配置](#运行与配置)
- [对外接口](#对外接口)
- [端到端验证](#端到端验证)
- [维护脚本](#维护脚本)
- [第三方与许可](#第三方与许可)

## 设计约束

这三条是硬约束，改动时不能破：

1. **对外契约只有一份**——OpenAI Chat Completions。任何上游的协议差异都在 `src/internal/provider` 的适配层吸收，不对外暴露第二套格式。
2. **工作方式分两类，都不引入第三方许可负担**。
   - **照搬 MIT 上游源码**（`workbuddy` / `trae`）：须保留其版权声明，许可原文集中放在
     `src/THIRD-PARTY-LICENSES/`；照搬来的那部分不改变本项目自身的 MIT。
   - **独立重写**（`zcode`）：上游 `dengyie/zcode2api` 是 AGPL-3.0，**不能照搬**，
     因此按实测契约**从零重写**成 Go（`src/internal/provider/zcode/`）。
     本仓库里没有任何上游代码，所以也不需要它的许可原文。

   两种方式都**不是**上游 AGPL 代码，因此都可内嵌且不改变 ModelMux 的 MIT。
   AGPL 项目（`new-api`）仍只允许**进程级**调用：它是独立子进程，不构成衍生作品。
3. **不随包分发第三方二进制**。托管型 provider 由用户自行获取，本项目不分发。

配套的工程约定：

| 约定 | 原因 |
|---|---|
| 未实现的协议**明确报错并附原因**，不回落近似协议「试一下」 | 静默回落会把上游的报错变成用户看不懂的谜题 |
| 上游错误**原样透传**（状态码 + 错误体） | 客户端要靠状态码决定重试策略 |
| 转发**保真优先**：格式一致时一个字节都不碰 | 改 JSON 用 `map[string]json.RawMessage`，`map[string]any` 经 `float64` 往返会破坏大整数与高精度小数 |
| **区分「字段缺失」与「空值」** | Go 里 `nil` slice = 字段缺失，空数组 = 显式清空 |
| 会导致路由歧义的配置**禁止 + 报错**，不猜默认值 | 猜错会把请求静默送到错误的上游 |

## 两类渠道

| 类型 | 配置键 | 运行形态 |
|---|---|---|
| **内嵌型** | `embedded_providers` | 跑在网关进程内。自定义 OpenAI 兼容端点，或选用内置预设（见下一节）。支持多 Key 轮询、权重、模型前缀。 |
| **托管型** | `managed_providers` | 由 ModelMux 托管：进程内原生（内置实现，不起外部进程）或独立子进程（动态分配端口、注入环境变量、探活、按树杀干净）。由 `mode` 字段决定。 |

托管型渠道的管理面板由网关**反向代理**到面板里，`Authorization` 由服务端注入——
面板侧不接触上游密钥。

## 已并入的 Provider

预设只是**预填了一组字段的模板**：选中后每一项都能改，保存后就是一个普通渠道，
不参与路由 / 额度 / 日志里的任何特殊分支。

### 托管型预设

托管型渠道有**两种运行方式**，由渠道的 `mode` 决定，模板会替你选好：

- **进程内原生**（`mode=native`）：ModelMux 自己装配内置实现，在本进程内起一个只绑 `127.0.0.1`
  的服务。**不需要任何外部可执行文件**，面板与控制台都照常使用。
- **独立子进程**（`mode=process`，缺省）：编排器动态分配端口、注入环境变量、探活、退出时按树杀干净。
  升级前的配置没有 `mode` 字段，一律按子进程解释，行为与旧版完全一致。

| 预设 | 项目 | 运行方式 | 说明 | 许可 |
|---|---|---|---|---|
| `workbuddy` | **[WorkBuddy 2 API](https://github.com/linguo2625469/workbuddy2api-panel)** | **进程内原生** | 把 CodeBuddy 账号变成 OpenAI 兼容接口的多账号网关：OAuth 登录、账号池三因子加权轮转、分级熔断与冷却、会话粘性。**已内置**，不需要 `wb2api.exe`；把「数据目录」指到原 wb2api 目录即可沿用已登录的账号。 | MIT |
| `zcode` | **[zcode2api](https://github.com/dengyie/zcode2api)** | **进程内原生** | ZCode 账号运营 + 双协议网关一体机：账号池轮询、额度监控、限时套餐领取、设备码登录，同时对外提供 Anthropic Messages 与 OpenAI Chat Completions。**已内置**（按实测契约**独立重写**，见下方说明），不需要 Python 环境；把「数据目录」指到原 zcode2api 的 `data/` 即可沿用已登录的账号。 | AGPL-3.0（上游）/ 本项目为独立实现 |
| `new-api` | **[new-api](https://github.com/QuantumNous/new-api)** | 独立子进程 | 多渠道聚合与分发底座。启动命令 `new-api.exe`，端口环境变量 `PORT`。 | AGPL-3.0 |
| `trae` | **[trae2api-web](https://github.com/connectedGraph/trae2api-web)** | **进程内原生** | 把 Trae IDE 的模型能力暴露成本地 OpenAI 兼容端点：账号池调度、冷却状态机、每日签到与设备码/回调登录闭环。**已内置**，不需要 `node server.js`；登录回调需要一个固定端口（默认 `18080`，见数据目录下的 `config.json`）。 | MIT |
| `custom-managed` | 自定义进程 | 独立子进程 | 任何能用环境变量指定端口、且暴露 OpenAI 兼容端点的可执行程序。 | — |

> `workbuddy` 与 `trae` 都是**照搬上游 MIT 源码**内嵌的实现（上游文件头与版权声明逐字保留），
> 许可原文见 [`src/THIRD-PARTY-LICENSES/`](src/THIRD-PARTY-LICENSES/)。
> `zcode` 是**独立重写**：上游 `dengyie/zcode2api`（AGPL-3.0）只作为**契约来源**
> （在其上采样出 HTTP / 落盘 / 出站三类契约），本项目按契约从零实现，
> 仓库里没有任何上游代码，因此 `src/THIRD-PARTY-LICENSES/` 里**没有** zcode 目录。
> 表中其余项目都是**独立程序，由使用者自行获取与部署**：ModelMux **不包含也不分发**它们的二进制，
> AGPL 上游（`new-api`）仅以独立进程方式调用、不构成衍生作品。

### 内嵌型预设（进程内转发）

| 预设 | 上游 | 协议 | 默认接入点 | 获取 Key |
|---|---|---|---|---|
| `openrouter` | [OpenRouter](https://openrouter.ai) | OpenAI | `https://openrouter.ai/api/v1` | [keys](https://openrouter.ai/keys) |
| `zhipu` | [智谱 BigModel](https://open.bigmodel.cn) | OpenAI | `https://open.bigmodel.cn/api/paas/v4` | [apikeys](https://open.bigmodel.cn/usercenter/apikeys) |
| `deepseek` | [DeepSeek 官方](https://platform.deepseek.com) | OpenAI | `https://api.deepseek.com/v1` | [api_keys](https://platform.deepseek.com/api_keys) |
| `siliconflow` | [SiliconFlow 硅基流动](https://cloud.siliconflow.cn) | OpenAI | `https://api.siliconflow.cn/v1` | [ak](https://cloud.siliconflow.cn/account/ak) |
| `moonshot` | [Moonshot / Kimi](https://platform.moonshot.cn) | OpenAI | `https://api.moonshot.cn/v1` | [api-keys](https://platform.moonshot.cn/console/api-keys) |
| `anthropic` | [Anthropic（Claude）](https://console.anthropic.com) | **Anthropic Messages** | `https://api.anthropic.com` | [keys](https://console.anthropic.com/settings/keys) |
| `openai` | [OpenAI 官方](https://platform.openai.com) | OpenAI | `https://api.openai.com/v1` | [api-keys](https://platform.openai.com/api-keys) |
| `ollama` | [本地 Ollama](https://ollama.com) | OpenAI | `http://127.0.0.1:11434/v1` | 免 Key |
| `vllm` | 本地 [vLLM](https://github.com/vllm-project/vllm) / [LM Studio](https://lmstudio.ai) | OpenAI | `http://127.0.0.1:8000/v1` | 免 Key |
| `custom` | 任意 OpenAI 兼容端点 | OpenAI | 自填 | — |

上表里只有 `anthropic` 一家协议不同：上游说 Anthropic Messages，由适配层转成 OpenAI 格式对外。
`openai` 的部分新模型只在新版 Responses 协议下可用，需要时可在渠道里改选协议。

托管型渠道有三种来源方式，与许可无关、只取决于上游的获取成本：

- **MIT 上游照搬源码内嵌**（如 `workbuddy`、`trae`）：逐字保留上游文件头与版权声明，许可原文集中放在
  [`src/THIRD-PARTY-LICENSES/`](src/THIRD-PARTY-LICENSES/)，照搬部分不改变本项目自身的 MIT。
- **自己独立重写**（如 `zcode`）：按实测契约从零实现，仓库里不含任何上游代码，可内嵌。
- **AGPL 上游仅进程级调用**（当前的 `new-api`）：ModelMux 只把已存在于本机的程序
  作为子进程拉起，**不包含也不分发**其二进制，也不代其上游服务授予任何权利，
  不构成衍生作品；各项目自身的免责声明同样适用。

## 主要能力

- 渠道增删改查、启停、连通性测试、从上游拉取模型列表
- 对外 `/v1` 出口带 `access_key` 鉴权（首次启动自动生成，面板可查看 / 复制 / 重新生成）
- 用量与请求计量（`/api/metrics`）、请求日志（含来源 IP 与 User-Agent）
- 限时套餐定时领取（默认关闭；开启后按时间窗口自动领取，也可手动触发）
- 面板端口运行中热切换（`/api/settings/port`）
- 关窗行为可选：最小化到托盘继续运行 / 直接退出应用（选完即存，无需重启）
- 托盘驻留、单实例（重复启动会唤出已有窗口）、按显示器 DPI 换算窗口尺寸
- **无界面模式**：`-headless`，只跑内置服务不建窗口不建托盘

## 构建

要求：**Windows 10/11**、**Go 1.25+**、**WebView2 运行时**（Windows 11 自带，Windows 10 需[单独安装](https://developer.microsoft.com/microsoft-edge/webview2/)）。

推荐用发布脚本（它会先同步 `web/` → `src/internal/assets/` 再编译，产物落在仓库根）：

```bash
python tools/release/build.py            # 同步 + 编译
python tools/release/build.py --check    # 只校验两份前端是否一致，不编译
python tools/release/build.py --no-build # 只同步，不编译
```

直接调 Go 也可以，但要**自己保证** `src/internal/assets/` 是最新的：

```bash
go build -C src -trimpath -ldflags="-s -w -H windowsgui" -o ../ModelMux.exe .
```

`-H windowsgui` 必须有，否则会多出一个黑色控制台窗口。

图标资源 `src/appres.syso` 已随仓库提供，`go build` 会自动拾取（同目录的 `*.syso` 会被自动收集）。
需要重建时：

```bash
cd src && go run github.com/akavel/rsrc@v0.10.2 -ico app.ico -o appres.syso
```

## 运行与配置

```bash
ModelMux.exe                 # 桌面壳 + 托盘
ModelMux.exe -headless       # 只跑服务（等价于 MODELMUX_HEADLESS=1）
```

**数据目录默认跟 exe 同级**，也就是「绿色 / 便携」形态：把整个文件夹拷到哪，配置和数据就跟到哪。

```
modelmux/
├─ ModelMux.exe
├─ config/
│  └─ modelmux.json      配置（access_key 首次启动自动生成）
├─ data/
│  ├─ ports.json         上次用过的端口，重启优先复用
│  ├─ logs/              结构化日志
│  ├─ usage/             用量计量累积
│  ├─ cache/             WebView2 用户数据目录
│  └─ instances/         托管型 provider 的实例数据
└─ web/                  （可选）外置前端，见上文「前端资源为什么有两份」
```

查找顺序（第一个成立者胜出）：

1. 环境变量 `MODELMUX_HOME`（显式指定，最高优先级）
2. **exe 所在目录**——但必须**实测可写**（会在该目录建临时文件再删掉验证），
   避免 exe 放在 `Program Files`、只读介质或受控文件夹访问拦截时静默失败
3. `%LOCALAPPDATA%\ModelMux`（回落，Windows 上的常规选择）
4. 系统临时目录下的 `ModelMux`（最后的兜底）

`config/` 与 `data/` 缺失时会在启动时自动创建，不需要手工准备。

面板监听在 `127.0.0.1`，端口默认动态分配；可在面板「设置」里固定为指定端口（如 `1234`）。
实际地址以启动日志为准。

## 对外接口

| 路径 | 说明 |
|---|---|
| `/` | 面板（优先读 exe 同级 `web/`，读不到用 exe 内嵌副本） |
| `/v1/chat/completions`、`/v1/models` | OpenAI 兼容出口，需 `access_key` |
| `/api/status`、`/api/healthz`、`/api/logs` | 状态与日志 |
| `/api/channels*` | 渠道管理（增删改查 / 启停 / 测试 / 拉模型） |
| `/api/channels/{name}/upstream/{path...}` | 托管型渠道的管理面板反代 |
| `/api/metrics` | 用量与请求计量 |
| `/api/claim*` | 限时套餐领取 |
| `/api/settings/port`、`/api/settings/close` | 端口与关窗行为 |
| `/api/quit` | 有序退出 |

## 端到端验证

`test/` 下是**真实运行的**验证脚本（需要 Python 3）：用假上游覆盖协议分支，用 CDP 驱动真实界面点击。

```bash
python test/verify.py              # 全链路：托管型 provider 全生命周期、端口热切换
python test/verify_upstream_ui.py  # 上游控制台逐视图
python test/verify_side_console.py # 侧栏渠道卡与常驻控制台入口
python test/verify_layout_ui.py    # 侧栏 / 概览拆分 / 渠道嵌套编辑
python test/verify_pick_ui.py      # 拉取模型全选与结果弹窗
python test/verify_theme.py        # 首帧主题（防亮暗闪烁）
python test/verify_claim.py        # 限时套餐定时领取链路
python test/verify_zcode_accounts.py  # zcode 账号面板（托管型子进程 + 假网关）
python test/verify_zcode_native.py    # zcode 内置原生（ModelMux 自己装配，无外部进程）
python test/gui_check.py           # 托盘与窗口生命周期
python test/check_sources.py       # 源码级不变式（内联 style、通知 API 是否被重新引入）
python test/verify_silent_minimize.py  # 最小化 / 隐藏不得产生系统通知
python test/verify_proxy_real.py   # 真实 wb2api 账号管理代理（用你的真实配置）
```

跑之前先看清楚：

| 脚本 | 前置条件 |
|---|---|
| `verify.py` / `verify_claim.py` / `verify_pick_ui.py` / `gui_check.py` / `verify_zcode_native.py` | 使用隔离的 `MODELMUX_HOME`，**不会动你的真实配置** |
| `verify_layout_ui.py` / `verify_side_console.py` / `verify_upstream_ui.py` / `verify_theme.py` | 需要**本机已有一个实例在跑**（默认 `http://127.0.0.1:1234/`） |
| `gui_check.py` | 需要**独占**：机器上不能有其它 ModelMux 实例，否则会被单实例逻辑唤出并退出 |
| `verify_workbuddy.py` | ⚠️ 会**写你的真实配置**，慎跑 |
| `verify_proxy_real.py` | ⚠️ 用你的**真实配置**启动，会拉起真实托管子进程；需要本机真有一个可用的 wb2api 渠道 |
| `verify_silent_minimize.py` | 需要**本机已有一个实例在跑**（按 exe 名找 PID，并会最小化该窗口） |
| `check_sources.py` | 不需要实例，纯源码检查 |

`test/shot_tools/` 下是**截图工具**（`shots*.py`、`panel_shot.py`、`shot_settings.py`），
只产出给人看的 PNG、不含任何断言，所以不在上面的验证清单里。

## 维护脚本

```bash
python tools/release/build.py               # 构建：同步 web/ → src/internal/assets/，再编出根目录的 ModelMux.exe
python tools/release/backup_project.py      # 全量快照（源码 + 配置 + 编译产物），默认输出到仓库同级的 _backups/
python tools/release/export_for_github.py   # 导出可公开的干净副本（白名单式）
```

`export_for_github.py` 是**白名单式**的：只列进清单的文件才会出去（`src/` `web/` `docs/` `tools/` `test/`
与 `README.md` `LICENSE` `.gitignore`），因此新增文件时不会不小心把含真实密钥的 `config/`
或运行期 `data/` 带进公开仓库。含真实账号 token 的 `_ref/` 与历史快照已移出仓库，
`.gitignore` 里的规则保留作兜底。目标目录已存在时默认中止，确认要同步进已有仓库时加
`GITHUB_EXPORT_INTO=1`——该模式下只覆盖同名文件、不动 `.git`，但会**清掉「源里已删除、
目标里还留着」的文件**（否则从白名单撤下的文件会永久留在公开仓库里），要保留它们设
`GITHUB_EXPORT_NO_PRUNE=1`。

三个脚本的路径都自动推导，也可用 `MODELMUX_SRC` / `BACKUP_DIR` / `GITHUB_EXPORT_DIR` / `GO` 覆盖。

## 第三方与许可

Go 依赖：

| 模块 | 许可 |
|---|---|
| `github.com/jchv/go-webview2` | MIT |
| `github.com/jchv/go-winloader` | ISC |
| `golang.org/x/sys` | BSD-3-Clause |

**托管型 provider 与面板里出现的上游服务**分两类：MIT 上游照搬源码内嵌（`workbuddy`、`trae`，
版权声明逐字保留），其余是独立程序、由使用者自行获取，本项目**不包含也不分发**其二进制。
各项目的链接、许可与说明见[已并入的 Provider](#已并入的-provider)；
照搬部分的许可原文见 [`src/THIRD-PARTY-LICENSES/`](src/THIRD-PARTY-LICENSES/)，
其余项目的许可与使用合规性由各自项目及使用者自行负责。

## License

MIT，见 [LICENSE](LICENSE)。
