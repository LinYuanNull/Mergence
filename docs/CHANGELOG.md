# 更新日志

本项目的版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [0.4.0] - 2026-10-04

### 破坏性变更：项目更名为 Mergence（模渊）

原名 `ModelMux` / `模汇` 全部更名为 `Mergence` / `模渊`，对外可见的标识符一并变更：

| 项 | 原 | 现 |
|---|---|---|
| 可执行文件 | `ModelMux.exe` | `Mergence.exe` |
| 环境变量 | `MODELMUX_*` | `MERGENCE_*` |
| Go module | `modelmux` | `mergence`（含全部 import 路径） |
| 配置文件 | `modelmux.json` | `mergence.json` |
| 日志文件 | `modelmux.log` | `mergence.log` |
| 运行根目录 | `%LOCALAPPDATA%\ModelMux` | `%LOCALAPPDATA%\Mergence` |
| 单实例互斥体 / 托盘窗口类 | `ModelMux*` | `Mergence*` |
| PE 资源（产品名 / 原始文件名 / 描述） | `ModelMux` | `Mergence` |

**升级注意**：环境变量与配置文件名已变，外部脚本需同步更新；旧位置的既有配置不会被自动迁移。
文档文件名（`docs/Mergence-*.md`）与备份归档目录（`_backups_mergence`）一并更名。

### 架构梳理与代码清理

**不改运行时行为，对外契约、配置格式、目录结构均不变。**

### 移除（未引用代码与废弃物）

- **19 个非冻结区的 Go 死符号**（`deadcode -test ./...` 判定 + 逐条 grep 复核，
  剔除冻结区与同名误报）：
  `desktop` 的 `Shell.Title`、`Shell.title` 字段、`ParsePort`、`unregisterWnd`；
  `logging.WithReqID`；`orchestrator` 的 `nativeHandle.Wait`、`Orchestrator.Get`、
  `EnsureAddr`、`PortAllocator.Snapshot`；`provider` 的 `extractTotalTokens`、
  `ExtractStreamTokens`、`ApplyPreset`；`zcode` 的 `fingerprint.Default`、
  `Gateway.hasUsableAccount`、`ModelsFromList`、`FingerprintOf`、
  `OrderedObject.Has/.Raw/.Take`、`isClientError`、`Patch.IsEmpty`；
  以及 `zcode/models` 测试里从未被调用的 `probeString`。
  连带清掉因此变成孤儿的 import（`orchestrator` 的 `net`、`shell` 的 `strconv`）。
- **前端 `web/upstream.js` 的 `cfgIsSecret()`**：全仓唯一引用就是它自己的定义，
  现役实现改为按 `ct === 'secret'` 判断。⇒ 该文件字节数**不再是 63,766 B**，
  但 v0.3.0 那条「四条接缝一行不改」的结论不受影响——删的是一处从未被调用的
  私有函数，接缝契约本身没有改动。
- **两个只有 `doc.go` 的空包**：`zcode/install`、`zcode/telemetry`（零符号、零 import；
  注释写「对应计划中的 A5 阶段」，而 A5 实现落在 `zcode/agent/a5.go`）。
- **失效的截图脚本 `test/shot_tools/shots_newui.py`**：它调用前端已不存在的
  `showPage(...)`（现役是 `setView`），跑起来什么都截不到；`shots_desktop.py` 是替代品。
- **无引用的开发脚本**：`tools/icon/gen_icon.py`、`tools/icon/build_icon.py`
  （均为 WorkBuddy2API 时代产物；前者还默认输出到 `src/app.ico`，误跑会覆盖真图标）、
  `tools/win/mklnk.vbs`、`tools/win/ls_lnk.py`。

### 修正

- `.gitignore` 与 `tools/release/backup_project.py` 补上 `test/zcacc_home`、
  `test/zcnative_home` 两个测试隔离目录——它们此前既没被忽略（可能被误提交），
  也会被整棵打进全量快照。
- README 的 PE 资源一节原本给的命令是 `akavel/rsrc -ico app.ico -o appres.syso`，
  **这条指令会让 DPI 清单与版本信息静默丢失**。改为 go-winres 的正确命令
  （`--in winres/winres.json --out appres.syso --no-suffix --arch amd64`），
  并说明 `src/app.ico` 是图标母版、`go build` 并不消费它。
- README 的测试前置条件表补齐 `verify_zcode_accounts.py`，并写明
  `verify_silent_minimize.py` 可用 `MERGENCE_PID` 锁定目标进程。

## [0.3.0] - 2026-10-04

**托管渠道新增「进程内原生」运行方式，workbuddy 渠道不再需要外部 exe。**
对外契约与配置格式**向后兼容** —— 升级后旧配置原样可用，未改过的渠道行为不变。

### 新增

- **`mode` 字段：托管渠道的两种运行方式**
  - `mode: "process"`（**空值等价**）：保持原行为，由 Mergence 拉起独立子进程。
  - `mode: "native"`：Mergence 自己装配上游的 `http.Handler`，在本进程内起一个
    **只绑 `127.0.0.1:0`** 的服务。端口由内核分配，不进 `ports.json` 端口表
    （该表是给「用户可能手工填进别处的外链地址」用的）。
  - 原生型**仍然绕回环 HTTP** 而不拆成函数调用：上游是一整套 `http.ServeMux` 应用，
    拆函数等于重写上游；而「能 merge 上游修复、能对照上游排查」这两项收益都建立在
    上游代码逐字不动之上。四条接缝因此一行不改，`web/upstream.js` 全程 **63,766 B 零改动**。
- **workbuddy 渠道改为进程内原生**（`linguo2625469/workbuddy2api-panel`，MIT 照搬内嵌）。
  面板新增 **数据目录** 入口，指到原 wb2api 目录（含 `auths/` 与 `config.json`）
  即可**沿用已登录的账号**，不需要重新登录。
- **新增 trae 渠道（进程内原生）**（`connectedGraph/trae2api-web`，MIT 照搬内嵌）。
  账号池调度、冷却状态机、每日签到与设备码/回调登录闭环都由内置实现承担，
  不再需要 `node server.js`。管理面固定在 `/admin/api`，凭据取渠道的**路由密钥**
  （与 zcode 用后台密码不同，见下）；登录回调需要一个固定端口（默认 `18080`，
  见数据目录下的 `config.json`）。
  「限时套餐自动领取」会定时触发它内部的签到 —— 上游原本只在进程内按整点跑
  `RunCheckinNow()`、**没有 HTTP 入口**，由原生装配层在 handler 外面补出
  `POST /admin/api/checkin`，因此复用同一条代理路径与同一份凭据。
- 第三方许可原文集中存放于 `src/THIRD-PARTY-LICENSES/`，与本项目自身的 MIT `LICENSE` 分区。
- **zcode 渠道改为进程内原生，且是「独立重写」而非照搬**。
  上游 `dengyie/zcode2api`（AGPL-3.0）**不能照搬**，因此先把它的可观测行为固化成
  三类契约（HTTP 25 路由 / SQLite 落盘 / 出站端点），再按契约**从零重写**成 Go
  （`src/internal/provider/zcode/`，装配层在 `src/internal/native/zcode/`）。
  仓库里**没有任何上游代码**，所以 `src/THIRD-PARTY-LICENSES/` 里没有 zcode 目录。
  覆盖能力：账号池（SQLite，落盘契约逐字节兼容）、额度查询、限时套餐领取、
  设备码登录、验证码（自写 CDP 客户端 + 用时现解）、Anthropic Messages 与
  OpenAI Chat Completions 双协议。不再需要 Python 环境。

  > **密码只剩一处**（本次最大的简化收益）：独立部署时后台密码在网关库与本机
  > 配置各存一份，「只改一边就整块 401 + 定时领取失效」是它的经典故障。
  > 内嵌后**渠道的「路由密钥」就是唯一真源**（留空用默认 `zcode`），
  > 原生装配层在每次启动时用它覆盖账号库里的 `admin_key`，改一处即全生效。

### 变更

- 面板的渠道编辑表单会**回显并提交** `mode` / `gateway_kind` / `data_dir`
  三要素（缺任一项都会导致「编辑一次原生渠道就退化成去拉起空 command」）。
  表单在原生型下自动隐藏子进程专有字段（启动命令、参数、工作目录、端口环境变量、固定端口、就绪超时）。
- `web/upstream.js` 仍按 `source === 'managed'` 判定视图，因此原生型在 UI 上归一为托管型，
  面板形态与子进程型完全一致。
- **管理面/领取凭据的来源按运行形态区分**（`panelAuthKey` / `claimKeyFor`）：
  原生型 zcode 取渠道 route key（空则默认 `zcode`）；托管型子进程仍取
  `config.Claim.AdminKey` —— 老配置里 `python cli.py serve` 的渠道尚未迁移，
  它的管理面认的是自己的 `ZCODE_ADMIN_KEY`，拿 route key 去会 401。
- **改后台密码**：原生型只改一次（面板代理 `PUT /admin/api/settings` 后同步
  渠道 route key）；老式子进程保留原「改两处」语义。

### 修复

- 渠道编辑弹窗在打开时会无条件清空 `gateway_kind`，导致唯一性校验在编辑态失效 —— 已修。

### 配置归一化（禁而不猜）

会导致路由歧义或运行方式冲突的配置一律**禁用并给出告警**，不猜默认值：
原生型同时填 `command`（两种运行方式只能选一种）、原生型缺 `kind`（不拿渠道名猜）、
非法 `mode`（禁用并回落 `process`）。原生型的 `fixed_port` / `port_env_var` 会被清零/清空并告警。

### 工程

- 新增 `tools/release/scan_secrets.py`：提交前扫描导出范围内的个人路径痕迹、
  疑似密钥与误纳入的数据文件；豁免按「文件 + 行内占位符」双条件，放行测试夹具里的
  假 key 而不会放过仿真 key。
- 全量回归：`verify.py` 121 项、上游控制台 51 项、侧栏控制台 59 项、布局 38 项、
  zcode 账号 44 项、拉取模型 22 项、首帧主题 16 项、限时领取 11 项、
  GUI 生命周期 全部通过、静默最小化 8 项、源码不变式 8 项、
  **原生 zcode 专项 29 项（本次新增 `test/verify_zcode_native.py`）**。
  侧栏与布局各有 1~2 项依赖 **zcode 账号池非空**（账号须用户自己做 OAuth），
  在空池下按设计隐藏对应卡片，属数据条件而非回归。
- 验收基线：`go build` / `go vet` / `gofmt -l` 零输出，`go test ./...` 全通过，
  `build.py --check` 确认 `web/` 与 `src/internal/assets/` 逐字节一致（7 个文件）。
- **前端零改动**：Track 3 全程未编辑 `web/` 下任何文件（`web/zcode.js` 仍为 48,956 B），
  证明「保留 `/api/channels/zcode/upstream/<rest>` URL 形态、只换实现」这一设计成立。

## [0.2.1] - 2026-10-03

**目录与发布形态整理。对外契约、配置格式与 `/v1` 行为完全不变**——升级可直接覆盖 exe，
已有的 `config/mergence.json` 原样可用。

### 变更

- **根目录按职责拆开**：根目录现在只剩 `Mergence.exe` 与 `README.md` / `LICENSE` / `.gitignore`，
  其余各归其位 —— `src/`（Go 源码，模块根）、`web/`（面板前端）、`config/`（用户配置）、
  `data/`（日志 / 用量 / 缓存 / 托管实例）、`test/`（端到端验证）、`tools/`（开发与发布脚本）、
  `docs/`（设计与改造记录）。
- **前端资源外置 + 内嵌兜底**：面板前端以仓库根的 `web/` 为**唯一真源**，运行时优先读取
  exe 同级的 `web/`（改完即生效，便于调试），读不到时回落到编进 exe 的内嵌副本。
  **删掉 `web/` 应用照常工作**，只是前端不能外置调整。
- **数据目录改为便携形态**：默认跟 exe 同级（`config/mergence.json` + `data/`），
  整个文件夹拷到哪、配置与数据就跟到哪。exe 所在目录**实测不可写**时（只读介质 /
  受控文件夹访问 / 组策略），按 `MERGENCE_HOME` → `%LOCALAPPDATA%\Mergence` → `%TEMP%\Mergence` 依次回落。
- WebView2 用户数据目录由 `webview2/` 更名为 `data/cache/`。

### 工程

- 新增 `tools/release/build.py`：编译前把 `web/` 按 sha256 同步到 `src/internal/assets/`
  （`go:embed` 不允许 `..`，两份物理上无法合并成一处），支持 `--check`（只校验）与 `--no-build`（只同步）。
- 导出脚本（白名单式）的清理范围扩到**目标根目录**，避免重构后旧路径的文件滞留在公开仓库。
- 搬迁零破坏是**字节级**验证的：搬迁前后的源码用同一 Go 工具链各构建一次，产物 SHA256 完全相同。
- 全量回归：`verify.py` 121 项、上游控制台 51 项、侧栏控制台 57 项、布局 38 项、拉取模型 22 项、
  首帧主题 16 项、限时领取 11 项、GUI 生命周期 9 项、静默最小化 8 项、源码不变式 8 项，全部通过。

[0.2.1]: https://github.com/LinYuanNull/mergence/releases/tag/v0.2.1

## [0.1.0] - 2026-10-03

首个公开版本。核心是把多个上游模型服务聚合成**一份 OpenAI Chat Completions 契约**，
并提供一个能日常用的 Windows 桌面面板。

### 新增

**网关与协议**

- 对外唯一契约：`POST /v1/chat/completions`、`GET /v1/models`，含流式直通
- 内嵌型渠道（`embedded_providers`）：自定义 OpenAI 兼容端点 + 内置预设，支持多 Key 轮询、权重、模型前缀
- 托管型渠道（`managed_providers`）：把上游服务作为独立子进程拉起，动态分配端口、注入环境变量、探活、断线重拉、退出时按树杀干净
- 协议适配层：上游协议差异全部在 `internal/provider` 内部消化，未实现的协议明确报错而非近似回落
- 上游错误原样透传（状态码 + 错误体）
- 转发保真：上下游格式一致时按字节直通；改 JSON 用 `map[string]json.RawMessage`
- 对外出口鉴权：`access_key` 首次启动自动生成，面板可查看 / 复制 / 重新生成

**面板与桌面壳**

- 内嵌 WebView2 桌面壳，静态资源经 `//go:embed` 编进 exe
- 系统托盘驻留；关窗行为可选「最小化到托盘」或「直接退出应用」，选完即存无需重启
- 单实例：重复启动会唤出已有窗口
- 按所在显示器真实 DPI 换算窗口物理尺寸
- 侧栏导航 + 概览 / 渠道 / 上游控制台 / 运维分区
- 上游控制台逐视图：账号池、任务中心、模型与档位、积分构成、用量、请求监控、网关设置、运行日志
- 托管型渠道的管理面板经网关反代，`Authorization` 由服务端注入，面板侧不接触上游密钥
- 面板端口运行中热切换，无需重启

**计量与运维**

- 用量与请求计量（`/api/metrics`）、请求日志（含来源 IP 与 User-Agent）、转发链路完整 `usage` 透传
- 结构化日志（环形缓冲 + 文件轮转）
- 限时套餐定时领取（默认关闭，可设时间窗口自动领取，也可手动触发）
- 有序退出：托盘图标移除 → 窗口关闭 → 托管子进程按树回收 → 状态落盘，不留幽灵图标与孤儿进程
- 无界面模式（`-headless` / `MERGENCE_HEADLESS=1`）：把「服务可用」与「窗口可开」解耦，供自动化与 CI 使用

### 工程

- `_e2e/` 端到端验证：假上游覆盖协议分支 + CDP 驱动真实界面点击 + 断言渲染输出
- 覆盖托管型 provider 全生命周期、端口热切换、托盘与窗口生命周期、首帧主题、限时领取链路

[0.1.0]: https://github.com/LinYuanNull/mergence/releases/tag/v0.1.0
