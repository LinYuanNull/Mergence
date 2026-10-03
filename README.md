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
                     │      internal/provider     │
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
go build -trimpath -ldflags="-s -w -H windowsgui" -o ModelMux.exe .

# 2) 运行（桌面壳 + 托盘）
./ModelMux.exe
```

启动后托盘图标右键 →「打开面板」，或在浏览器里访问启动日志打印的地址（默认 `http://127.0.0.1:1234/`）。
在面板「渠道」里添加你的上游，然后就能用 `access_key` 调 `/v1` 了：

```bash
curl http://127.0.0.1:1234/v1/chat/completions \
  -H "Authorization: Bearer <面板里复制的 access_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"<你的模型名>","messages":[{"role":"user","content":"hi"}]}'
```

## 目录

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

1. **对外契约只有一份**——OpenAI Chat Completions。任何上游的协议差异都在 `internal/provider` 的适配层吸收，不对外暴露第二套格式。
2. **不 `import` AGPL 代码**。AGPL 项目只允许**进程级**调用：它是独立子进程，不构成衍生作品。
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
| **托管型** | `managed_providers` | 独立子进程。由编排器动态分配端口、注入环境变量、探活、断线重拉、退出时按树杀干净。 |

托管型渠道的管理面板由网关**反向代理**到面板里，`Authorization` 由服务端注入——
面板侧不接触上游密钥。

## 已并入的 Provider

预设只是**预填了一组字段的模板**：选中后每一项都能改，保存后就是一个普通渠道，
不参与路由 / 额度 / 日志里的任何特殊分支。

### 托管型预设（拉起独立子进程）

| 预设 | 项目 | 说明 | 许可 |
|---|---|---|---|
| `workbuddy` | **[WorkBuddy 2 API](https://github.com/Thelatter666/workbuddy2api)**（wb2api） | 把 CodeBuddy 账号变成 OpenAI 兼容接口的多账号网关：OAuth 登录、账号池三因子加权轮转、分级熔断与冷却、会话粘性。启动命令 `wb2api.exe`，端口环境变量 `WB2A_LISTEN`。 | MIT |
| `zcode` | **[zcode2api](https://github.com/dengyie/zcode2api)** | ZCode 账号运营 + 双协议网关一体机：账号池轮询、额度监控、限时套餐领取，同时对外提供 Anthropic Messages 与 OpenAI Chat Completions。Python 项目，启动命令 `python cli.py serve`，端口环境变量 `ZCODE_PORT`。 | AGPL-3.0 |
| `new-api` | **[new-api](https://github.com/QuantumNous/new-api)** | 多渠道聚合与分发底座。启动命令 `new-api.exe`，端口环境变量 `PORT`。 | AGPL-3.0 |
| `trae` | **[Trae](https://www.trae.ai)** 本地网关 | 把 Trae IDE 的模型能力暴露成本地 OpenAI 兼容端点。启动命令 `node server.js`；具体网关实现由使用者自行选择。 | 随所选项目 |
| `custom-managed` | 自定义进程 | 任何能用环境变量指定端口、且暴露 OpenAI 兼容端点的可执行程序。 | — |

> `workbuddy` 预设指向的是派生副本。它的原项目 `Sliverkiss/workbuddy2api` 仓库已不可访问，
> 链接给出的是仍在维护的那一份（两者的 MIT 版权声明与 NOTICE 一并在仓库内保留）。

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
| `vllm` | 本地 vLLM / [LM Studio](https://lmstudio.ai) | OpenAI | `http://127.0.0.1:8000/v1` | 免 Key |
| `custom` | 任意 OpenAI 兼容端点 | OpenAI | 自填 | — |

上表里只有 `anthropic` 一家协议不同：上游说 Anthropic Messages，由适配层转成 OpenAI 格式对外。
`openai` 的部分新模型只在新版 Responses 协议下可用，需要时可在渠道里改选协议。

托管型预设对应的都是**独立项目，由使用者自行获取与部署**。ModelMux 只把已存在于本机的
程序作为子进程拉起，**不包含也不分发**这些程序的二进制，也不代其上游服务授予任何权利。
AGPL 项目仅以独立进程方式调用，不构成衍生作品；各项目自身的免责声明同样适用。

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

```bash
go build -trimpath -ldflags="-s -w -H windowsgui" -o ModelMux.exe .
```

`-H windowsgui` 必须有，否则会多出一个黑色控制台窗口。

图标资源 `appres.syso` 已随仓库提供，`go build` 会自动拾取。需要重建时：

```bash
go run github.com/akavel/rsrc@v0.10.2 -ico app.ico -o appres.syso
```

## 运行与配置

```bash
ModelMux.exe                 # 桌面壳 + 托盘
ModelMux.exe -headless       # 只跑服务（等价于 MODELMUX_HEADLESS=1）
```

数据目录默认 `%LOCALAPPDATA%\ModelMux`，可用环境变量 `MODELMUX_HOME` 覆盖：

| 文件 | 说明 |
|---|---|
| `modelmux.json` | 配置（`access_key` 首次启动自动生成） |
| `ports.json` | 上次用过的端口，重启优先复用 |
| `logs/` | 结构化日志 |
| `instances/` | 托管型 provider 的实例数据 |

面板监听在 `127.0.0.1`，端口默认动态分配；可在面板「设置」里固定为指定端口（如 `1234`）。
实际地址以启动日志为准。

## 对外接口

| 路径 | 说明 |
|---|---|
| `/` | 面板（静态资源经 `//go:embed` 编进 exe） |
| `/v1/chat/completions`、`/v1/models` | OpenAI 兼容出口，需 `access_key` |
| `/api/status`、`/api/healthz`、`/api/logs` | 状态与日志 |
| `/api/channels*` | 渠道管理（增删改查 / 启停 / 测试 / 拉模型） |
| `/api/channels/{name}/upstream/{path...}` | 托管型渠道的管理面板反代 |
| `/api/metrics` | 用量与请求计量 |
| `/api/claim*` | 限时套餐领取 |
| `/api/settings/port`、`/api/settings/close` | 端口与关窗行为 |
| `/api/quit` | 有序退出 |

## 端到端验证

`_e2e/` 下是**真实运行的**验证脚本（需要 Python 3）：用假上游覆盖协议分支，用 CDP 驱动真实界面点击。

```bash
python _e2e/verify.py              # 全链路：托管型 provider 全生命周期、端口热切换
python _e2e/verify_upstream_ui.py  # 上游控制台逐视图
python _e2e/verify_side_console.py # 侧栏渠道卡与常驻控制台入口
python _e2e/verify_layout_ui.py    # 侧栏 / 概览拆分 / 渠道嵌套编辑
python _e2e/verify_pick_ui.py      # 拉取模型全选与结果弹窗
python _e2e/verify_theme.py        # 首帧主题（防亮暗闪烁）
python _e2e/verify_claim.py        # 限时套餐定时领取链路
python _e2e/gui_check.py           # 托盘与窗口生命周期
```

跑之前先看清楚：

| 脚本 | 前置条件 |
|---|---|
| `verify.py` / `verify_claim.py` / `verify_pick_ui.py` / `gui_check.py` | 使用隔离的 `MODELMUX_HOME`，**不会动你的真实配置** |
| `verify_layout_ui.py` / `verify_side_console.py` / `verify_upstream_ui.py` / `verify_theme.py` | 需要**本机已有一个实例在跑**（默认 `http://127.0.0.1:1234/`） |
| `gui_check.py` | 需要**独占**：机器上不能有其它 ModelMux 实例，否则会被单实例逻辑唤出并退出 |
| `verify_workbuddy.py` | ⚠️ 会**写你的真实配置**，慎跑 |

## 维护脚本

```bash
python tools/backup_project.py      # 全量快照（源码 + 配置 + 编译产物），默认输出到仓库同级的 _backups/
python tools/export_for_github.py   # 导出可公开的干净副本（白名单式，自动挡掉 _ref/ 与测试残留配置）
```

`export_for_github.py` 是**白名单式**的：只列进清单的文件才会出去，因此新增文件时不会
不小心把 `_ref/`（含真实账号 token）或 `internal/web/.tmp`（测试写出的残留配置，可能带真实
`access_key`）这样的东西带进公开仓库。目标目录已存在时默认中止，确认要同步进已有仓库时加
`GITHUB_EXPORT_INTO=1`——该模式下只覆盖同名文件、不动 `.git`，但会**清掉「源里已删除、
目标里还留着」的文件**（否则从白名单撤下的文件会永久留在公开仓库里），要保留它们设
`GITHUB_EXPORT_NO_PRUNE=1`。

两个脚本的路径都自动推导，也可用 `MODELMUX_SRC` / `BACKUP_DIR` / `GITHUB_EXPORT_DIR` 覆盖。

## 第三方与许可

Go 依赖：

| 模块 | 许可 |
|---|---|
| `github.com/jchv/go-webview2` | MIT |
| `github.com/jchv/go-winloader` | ISC |
| `golang.org/x/sys` | BSD-3-Clause |

**托管型 provider 与面板里出现的上游服务**都是独立程序，由使用者自行获取，本项目
**不包含也不分发**其二进制。各项目的链接、许可与说明见[已并入的 Provider](#已并入的-provider)；
其许可与使用合规性由各自项目及使用者自行负责。

## License

MIT，见 [LICENSE](LICENSE)。
