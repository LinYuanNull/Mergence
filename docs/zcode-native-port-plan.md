# 网关原生化总计划

> 文件名沿用 `zcode-native-port-plan.md`（历史原因，避免断掉既有引用）。
> 内容已从「zcode 专项」扩展为 **ModelMux 四个托管型渠道的完整处置计划**。
>
> **一句话**：把 ModelMux 的四个托管型子进程渠道，按各自的**法律条件**与**技术条件**逐个处置 ——
> 能照搬的照搬，能重写的重写，都不能的明确保留 —— 最终 ModelMux 仍是**单可执行文件**、仍是**纯 MIT**。
>
> **三条轨道 + 一个例外**：
> Track 1 workbuddy 内嵌（**先行**，架构试验田）→ Track 2 A 线 `zcode2api-go`（全新纯 MIT 仓库，**单独发版**）
> → Track 3 zcode 内嵌进 ModelMux；Track 4 trae 内嵌（上游换成 `connectedGraph/trae2api-web`，**MIT + Go ⇒ 照搬**，可并行）；
> **new-api 有意保留为子进程**（唯一例外，理由见第二节）。
>
> **四渠道的处置只有三种走法**：MIT 同语言 ⇒ **照搬**（workbuddy / trae）；AGPL 或无许可 ⇒ **独立重写**（zcode）；
> AGPL + 十万行级 ⇒ **明确保留子进程**（new-api）。
>
> 本文是**执行计划**，不是决策讨论。决策已在第〇节定案，开工后不重开。

---

## 〇、已定决策（不再重开）

| # | 决策 | 定于 |
|---|---|---|
| 1 | 路线：**P2 源码级合并**（不是进程级调用） | 第 3 轮 |
| 2 | ModelMux **保持 MIT**；目标态是**全部 MIT** | 第 3 轮 |
| 3 | **A 线不 fork 上游，建全新空仓库**（不带任何上游文件） | 第 6 轮 |
| 4 | 顺序：**先写完独立项目并发版 → 再改写合并进 ModelMux** | 第 4 轮 |
| 5 | 领取：**无人值守自动定时**（Chromium 保留为浏览器资源） | 第 3 轮 |
| 6 | Chromium 来源：**复用系统已装 Edge**，新增分发体积 **0** | 第 3 轮 |
| 7 | Node 层：**Go 自写极简 CDP 客户端**替代 `solver_pw.js` | 第 1 / 3 轮 |
| 8 | 登录：**WorkBuddy 式 OAuth**（打印 URL → 用户浏览器 → poll） | 第 3 轮 |
| 9 | README 加一句「由 dengyie/zcode2api 用 Go 语言重写而来」+ 链接 | 第 3 轮 |
| 10 | A 线许可：**纯 MIT**（不再需要双许可） | 第 6 轮 |
| 11 | **workbuddy 也要内嵌，并建 fork 仓库** | 第 7 轮 |
| 12 | 执行顺序：**workbuddy 内嵌（架构试验田）→ A 线 → zcode 内嵌** | 第 7 轮 |
| 13 | **四个托管预设全部要有明确处置**（含明确保留的例外） | 第 7 轮 |
| 14 | ~~trae 上游是 `linqiu919/trae2api`~~ **作废**：trae 渠道必须**自带账号池 + 领取 + 验证码**，而 `linqiu919/trae2api` 三者皆无（只有 OAuth refresh + 2 个端点） | 第 8 轮 → **第 9 轮作废** |
| 15 | trae 新上游定为 **`connectedGraph/trae2api-web`**（Go **纯标准库** + **MIT**；多账号池 + 每日签到 + Web 管理面板 + OpenAI 兼容 + 登录闭环） | 第 9 轮 |
| 16 | trae 与 workbuddy **同路**：**fork + 照搬内嵌**（不是重写）；fork 副本命名 **`trae2api-web-panel`**，内嵌模块 `src/internal/provider/trae/` | 第 9 轮 |
| 17 | **第三类渠道类型正式定名 `NativeProvider`（原生型）**（用户授权「你自己定名就行」） | 第 9 轮 |
| 18 | trae 的「验证码」在**登录环节**（OAuth 本地回调 + 浏览器内验证码/滑块 + 9074 设备风控），**不需要** zcode 式的无头求解器 | 第 9 轮 |

---

## 一、先解决一个命名歧义：第三种渠道类型

动手前必须先定名，否则代码与文档会双轨。

现状（`src/internal/config/config.go:323`、`src/internal/provider/upstream.go:32-35`）：

| 类型 | 含义 | 出口怎么实现 |
|---|---|---|
| `ManagedProvider` **托管型** | 上游是 ModelMux **拉起的独立子进程** | 子进程自己实现 |
| `EmbeddedProvider` **内嵌型** | **跑在 ModelMux 进程内**，由本进程直接实现对外出口 | **通用 HTTP 转发**（用户填 `base_url` + `api_keys`） |

⚠️ **「内嵌型」这个词已经被占用**。而本计划要做的是第三类：

> **进程内 + 按特定上游协议专用实现 + 自带管理面（账号池 / 领取 / 验证码 / 设置页可写）**

两者都叫「内嵌」必然混淆。**定名：`NativeProvider`（原生型）** —— 定于第 9 轮（用户授权「你自己定名就行」）。

选它而非备选，理由三条：

- `EmbeddedProvider`（内嵌型）**已被占用**，必须有区分度；
- `Native` 直白表达「**进程内原生实现 + 自带管理面**」，与 `Managed`（托管型 / 子进程）形成干净对仗；
- 备选都不够：`Builtin`（内置型）偏「随包附带」、`InProc`（进程内型）只描述**位置**，
  两者都丢了本类最关键的语义 —— **自带管理面（账号池 / 领取 / 验证码 / 可写设置页）**。

**代码落点**：各渠道子包 `src/internal/provider/{workbuddy,zcode,trae}/`，统一实现 `NativeProvider` 接口面
（`internal/provider` 现有的 `Upstream` / `Channel` / `Registry`）。

> 命名一并落定的还有两个名字：trae 上游 fork 副本 **`trae2api-web-panel`**（对齐既有 `workbuddy2api-panel` 的命名习惯）、
> trae 内嵌模块 **`src/internal/provider/trae/`**（见第〇节决策 16）。

---

## 二、四渠道处置矩阵

| 渠道 | 上游项目 | 语言 | 许可 | 源码可得性 | **处置** |
|---|---|---|---|---|---|
| **workbuddy** | `linguo2625469/workbuddy2api-panel` | Go | **MIT** | 本机 `D:/workbuddy2api-panel` | **fork + 照搬内嵌** |
| **zcode** | `dengyie/zcode2api` | Python | **AGPL-3.0** | 本机（仅作契约靶机） | **独立重写 + 内嵌**（Track 2 → 3） |
| **trae** | `connectedGraph/trae2api-web` | Go（**纯标准库**） | **MIT** | 需拉取（fork） | **fork + 照搬内嵌**（Track 4） |
| **new-api** | `QuantumNous/new-api` | Go | **AGPL-3.0** | 无 | **保留托管型子进程**（有意例外） |

### 2.1 为什么 new-api 是唯一例外

三条理由，任一条都足以否决内嵌：

1. **AGPL-3.0** —— 与 zcode 同命，源码不可复制进 MIT 项目。
2. **规模** —— one-api 衍生的完整聚合平台，十万行级；独立重写不现实（zcode 才 ~6,400 行 Python）。重写它就是重写一整个产品。
3. **职责重叠** —— new-api 本身就是「多渠道聚合底座」，而 ModelMux **已经是**聚合器；把聚合器内嵌进聚合器在架构上无收益。

**保留子进程是合法且无疑问的**：进程级调用不构成衍生作品，这正是 ModelMux 现有的既定规则
（`presets.go:18x` 该条 `Note` 已写明「只以独立进程方式调用、不随包分发，以免影响本项目自身的许可」）。

若目标必须是「零子进程」，只有两条路，**都不推荐**：

- **(a) 删除 new-api 预设**，同时评估「ModelMux 自身的多渠道聚合（`embedded_providers`，用户直填 `base_url`）是否已覆盖真实需求」。
- **(b) 换用 MIT 的 `songquanpeng/one-api`** 再独立重写 —— 同样是十万行级项目，只是没了许可障碍。

### 2.2 trae 上游更换（第 9 轮修正）

**触发条件（用户第 9 轮原话）**：「trae 要找一个有**账号池、有领取、有验证码**的项目，不要使用 `linqiu919/trae2api`。」

`linqiu919/trae2api` **三项全不满足** —— 它只有 OAuth refresh + `/v1/models` + `/v1/chat/completions`，
**无账号池、无领取、无验证码、无持久化**，且**无 LICENSE 文件**、**已停更**。⇒ 弃用。

**筛选结果**（按「三项能力 + 语言 + 许可」逐项核对）：

| 候选 | 语言 | 许可 | 账号池 | 领取 | 验证码 | 结论 |
|---|---|---|---|---|---|---|
| **`connectedGraph/trae2api-web`** | Go（**纯标准库**） | **MIT** | ✅ 积分降序调度 + 冷却/禁用状态机 | ✅ 每日定时签到 | ✅ 登录闭环（OAuth 回调 + 浏览器内验证） | **采用** |
| `lg996/cpa-multi-plugins` | Go | **MIT** | ✅ credit-aware 挑号 + 4 档 cooldown | ✅ 每日 09:00 签到 | ✅ 侧车 + 9074 限流识别 | **备选 · 协议补充** |
| `Maquer/trae-signin` | Go | — | ✗（多账号但不调度） | ✅ | ✗ | 不采用（只签到，非网关） |
| `L0NE-6/Trae-AutoCheckin` | Python | MIT | 多账号配置 | ✅ | ✅ 短信验证码登录 | 不采用（签到工具，非网关） |
| `kggzs/TraeAccountRegister`、`911218sky/Trae-Account-Creator` | Python | AGPLv3 / — | ✗ | ✅（注册送礼包） | ✅ 邮箱验证码 | 不采用（注册工具，非网关） |
| `Shivam990q/Trae-GodMode` | Python | MIT | ✅（池的是第三方 key） | ✗ | ✗ | 不采用（池的不是 Trae 账号） |
| `ZedeX/trae-local-api`、`Wang-JQ77/dsh-trae-api` | TS | 声明式 | ✗ | ✗ | ✗ | 不采用（依赖本机 IDE 登录态 + 破解 `tc` 加密） |
| `Sliverkiss/traework2api` | Go | — | — | — | — | **已 404**（`trae2api-web` 是它的继承者） |
| ~~`linqiu919/trae2api`~~ | Go | **无 LICENSE** | ✗ | ✗ | ✗ | **弃用** |

判据一句话：**要的是「网关」，不是「注册器」也不是「签到脚本」** ——
只有前两个候选同时具备「账号池 + 领取 + 登录/验证 + OpenAI 兼容出口」。

### 2.3 为什么 `trae2api-web` 是正确选择（形态与 zcode2api 同构）

它和 zcode2api 是**同一类东西**，只是语言与平台不同：

| 维度 | `dengyie/zcode2api`（zcode） | `connectedGraph/trae2api-web`（trae） |
|---|---|---|
| 出口 | OpenAI 兼容 + 管理 API（22 路由） | OpenAI 兼容（`/v1/models`、`/v1/chat/completions`）+ `/status` |
| 管理面 | 自带 `frontend/` 面板，前缀 `/admin/api` | 自带 Web 控制台（`GET /admin`） |
| 账号池 | SQLite `accounts.db` + 状态机 | `auths/` + `data/state.json` + **积分降序调度 + 冷却/禁用/解冻状态机** |
| 领取 | `claim.py`（556 行）+ 验证码 | **每日定时签到** + Token 到期前 24h 预刷新 |
| 登录 | OAuth（URL + poll） | **本地回调链接登录闭环**（浏览器完成 → 回调回传 → 换 token 热加载） |
| 语言/依赖 | Python（FastAPI 等） | **Go 纯标准库，零第三方运行时依赖** |
| 许可 | **AGPL-3.0** | **MIT** |

**结论**：trae 这一条与 workbuddy **完全同类**（MIT + Go）⇒ 走**照搬**，成本远低于 zcode。

**许可结论（与旧上游的关键差别）**：`trae2api-web` 是 **MIT**（`LICENSE` 由 `docs: add MIT license` 提交引入，
README 明写「本项目基于 MIT License 许可发布」）⇒ **源码可以复制**，只需**保留版权声明与许可文本**。
这正是它和 `linqiu919/trae2api` 的分水岭 —— 后者「无 LICENSE = 默认保留所有权利」，一行都不能复制。

⚠️ **继承链要记录**：`trae2api-web` 自述「基于 `Sliverkiss/traework2api` 改进」，而 **`traework2api` 已 404**。
许可上我们只认 `trae2api-web` 自己的 MIT；但要在 `PROVENANCE.md` 里写明这层来源声明，避免溯源断层。

### 2.4 trae 的「验证码」与 zcode 不是一回事（重要，别套错方案）

用户要求「有验证码」，但要分清它落在**哪一环**：

| | zcode | trae |
|---|---|---|
| 验证码位置 | **领取**环节 —— 领取前需解阿里滑块，产出 `captchaVerifyParam`（寿命 95 秒） | **登录**环节 —— OAuth 本地回调，用户在浏览器完成登录（短信验证码 / 滑块都在浏览器里） |
| 是否需要无头求解器 | **需要**（上游 `captcha_node/solver_pw.js` → 我们自写 CDP 客户端） | **不需要** |
| 真正的技术难点 | 无头浏览器过滑块（A6；对应风险登记 #3 / #4） | **设备号风控**：业务码 **9074**（同一设备号一天只能签一个账号）、**1105**（机房 IP 风控） |

⇒ **trae 的 Track 4 不需要 A6 那套 CDP 求解器**，难度显著低于 zcode；
但必须实现**设备号管理**（每账号绑定稳定设备号，9074 命中后换号重试）与**签到定时器**。

---

## 三、许可纪律（四个 Track 通用）

### 3.1 为什么 MIT 能保住

**AGPL 管的是「代码表达」，不是「功能」；而许可的选择权属于著作权人。**
上游代码的许可你无权改动；但**你独立新写的实现**著作权归你，可任选许可（含 MIT）。

### 3.2 三条铁律

1. **新实现只依据对外可观测行为**：HTTP 契约、公开文档、真实请求/响应样本。
   **不逐行翻译**、不照抄上游的代码组织 / 命名 / 注释 / 函数划分。
2. **许可分区声明**：每个仓库的 `LICENSE` 与其内容一致；README 写明来源与分区。
3. **ModelMux 只吸收自己拥有的代码**（自写实现、或 MIT 项目如 workbuddy），**绝不带入 AGPL 代码**。

### 3.3 契约样本只保留结构（重要）

上游的**错误文案属于「表达」，不属于接口**。契约样本应**只保留结构**：

- 保留：路径、方法、字段名、类型、状态码、必填性、枚举值。
- 不保留原话：长文案截断，或替换为语义标签。
  例：`{"code":1005,"msg_kind":"quota_exhausted"}` 而不是抄它的中文句子。

这样样本是「接口事实记录」，不是「上游文案的复制」。字段名、路由、状态码属接口性事实，不受版权保护。

### 3.4 提交纪律

每次实现提交写明依据，例如：

```
依据：docs/contract/admin_api.accounts.json 第 12 条样本
```

这是将来主张「独立实现」的证据链，**不是可选的形式**。

### 3.5 ⚠️ ModelMux 侧必须同步改写的文档

- `README.md:122`「不 `import` AGPL 代码」
- `README.md:183`「AGPL 项目仅以独立进程方式调用，不构成衍生作品」

这两句在 Track 1/3/4 落地后**必须同步改写**，否则文档与事实不符。
改法：说明工作方式分两类 —— **workbuddy / trae 是 MIT 来源照搬**（保留版权声明与许可文本）、
**zcode 是自己独立重写** —— 两者都**不是上游 AGPL 代码**，因此可内嵌且不改变 ModelMux 的 MIT；
**new-api 仍走独立进程**，原规则对它继续适用。

### 3.6 上游许可文本集中存放（用户定于第 10 轮）

照搬 MIT 上游要求**保留版权声明与许可文本**。这些文本**不散落在各代码目录里**，统一放一个单独文件夹：

```
src/THIRD-PARTY-LICENSES/
├─ README.md                     说明：这里是上游许可原文，不是本项目代码
└─ <上游名>/
   └─ LICENSE                    与该目录代码一一对应的许可原文
```

- **落点选 `src/` 的原因**：发布导出白名单是 `src web docs tools test` ⇒ 只有放这里**才会被导出**，
  公开仓库里也带着上游许可，合规链不断。
- **与 ModelMux 自己的 `LICENSE` 分区明确**：仓库根 `LICENSE` = 本项目（MIT）；
  `src/THIRD-PARTY-LICENSES/` = 上游。
- **需要登记的**：`workbuddy2api-panel/`（Track 1）、`trae2api-web/`（Track 4）。
  **zcode 不需要** —— 它是独立重写，仓库里没有任何上游代码。
- 各 Track 在 **W1 / T1 源码落位时同步建立**对应子目录。

---

## 四、Track 1：workbuddy 内嵌（**先行** · 架构试验田）

### 4.1 为什么先做这条

- **不依赖 Track 2**（A 线是独立仓库开发，互不阻塞）。
- **许可零障碍**（MIT，可直接复制）。
- **有 16,250 行测试兜底**，风险低、可回退。
- 目的：把「进程内 dispatch」这套接缝架构，**在最容易的地方先跑通**，再套到 zcode。

> ⚠️ 它与 Track 2 不争资源，但**都要碰 `proxy.go` 与 `presets.go`** ⇒ 这两块必须串行。

### 4.2 内嵌成本（已逐项核实）

| 项 | 结论 |
|---|---|
| 许可 | **MIT**（`Copyright (c) 2026 Sliverkiss (original project: Sliverkiss/workbuddy2api)`；但**上游仓库现为 `linguo2625469/workbuddy2api-panel`**，Sliverkiss 原仓库已 404） |
| 语言 | **纯 Go** |
| 生产规模（**W1 实测**） | **17,833 行 / 58 个文件**（另 14,057 行测试 / 46 文件）—— 少于估算的 23,467/74，是因为 W1 已剔除 `cmd/`（5 个入口）与 `internal/panel`（3,502 行自带面板） |
| **外部进程** | **零** —— `exec.Command` 只出现在 `internal/panel/frontend_test.go`（node 做 JS 语法检查），**生产代码不拉起任何外部进程** |
| **外部服务** | **Redis 可选** —— `internal/redisstore/redisstore.go` 有 Noop 降级：「未配置 url / 连接失败时降级为 Noop，一切功能照常工作（纯内存模式）」；持久化走 `data/state.json` + `auths/`。**内嵌不会要求用户装 Redis** |
| 新引入依赖 | `go-redis v9.22.0` + 4 个间接（全纯 Go）⇒ ModelMux 直接依赖从 **1 个变 2 个**（现仅 `go-webview2`）。**W1 实测 `go build` 已自动并入 `go.mod`**，并把 `golang.org/x/sys` 从 `v0.0.0-20210218145245` 升到 `v0.30.0` |
| 附带可弃 | `internal/panel` 自带的 `index.html` + `app.js`（go:embed）—— ModelMux 已有 `upstream.js`（W1 已剔除） |
| `cmd/` 五入口 | `server` / `login` / `signin` / `credit` / `trial`；内嵌只需 `server`（`login` 的 OAuth 能力已是 HTTP 端点，`upstream.js` 已在调）（W1 已剔除） |
| **需保留的 embed 资源** | `prompt/defaultprompt.md`（2,172 B）+ `upstream/model.json`（2,992 B）—— 落位脚本**必须一并复制**，否则编译报 `pattern xxx: no matching files found` |

### 4.3 阶段

> **进度（第 10 轮末）**：W0 ✅ / W1 ✅ / W2 ⏳进行中 / W3 · W4 待做。
> 验收全绿：`go build -C src ./...` 零输出 · `go vet -C src ./...` 零输出 · `go test -C src ./...` 全 ok · `gofmt -l src` 空 · `build.py --check` 前端逐字节一致。

- **W0｜fork 仓库** ✅ **已完成**：fork **`linguo2625469/workbuddy2api-panel`**（**与 zcode 相反，这条应该 fork** —— MIT + 同语言 ⇒ 将来可直接 merge 上游修 bug）。
  ⚠️ 计划原写的 `Sliverkiss/workbuddy2api` 经 2026-10-03 实测**已 404**（与 `Sliverkiss/traework2api` 同命）；
  现存的、仍在更新的同一上游是 **`linguo2625469/workbuddy2api-panel`**（Go · MIT · 12 MB · pushed 2026-10-01）。
  目标 fork：**`LinYuanNull/workbuddy2api-panel`**（实测 `POST /forks -> 202`，fork=True · MIT · main · 12,153 KB）。
  本机 `git push` 不通 ⇒ 走 GitHub API（`_pubtools/fork_repo.py`，见 `PUBLISHING.md`）。
- **W1｜源码落位** ✅ **已完成**：复制进 `src/internal/provider/workbuddy/`，去掉 `cmd/` 中不需要的入口与自带面板；
  **保留上游文件头与许可声明**（MIT 允许复制，但要求保留版权声明）。
  实测落位 **104 个 Go 文件（13 个包）** + 2 个 embed 资源；改写 **91 处导入路径**
  （`github.com/linguo2625469/workbuddy2api-panel/internal/*` → `modelmux/internal/provider/workbuddy/*`）；
  **CRLF → LF 归一**（上游全 CRLF，Go 标准是 LF，否则 `gofmt -l` 全红）；
  `gofmt -w` 收尾（导入改写打乱了字典序）。
  许可文本落 `src/THIRD-PARTY-LICENSES/workbuddy2api-panel/LICENSE`（见 3.6）。
  脚本：`_pubtools/port_workbuddy.py`（幂等 + `--dry`）。
- **W2｜接线（架构试验田的核心）** ✅ **已完成**：四条接缝（① 前端 `web/upstream.js` ② `proxy.go` 控制台代理
  ③ 领取执行器 ④ 数据面转发）从「HTTP 打子进程」改为「进程内 dispatch」。
  **保留 `/api/channels/wb/upstream/<path>` 这个 URL 形态**，只换 handler 的实现 ⇒ `upstream.js` **零改动**。
  落地形态：
  - `config.ManagedProvider.Mode`（`process` / `native`，**空值 = process**）+ `Native()` / `Process()`；
  - `provider.SourceNative` + `Hosted()`（托管型/原生型都满足）与 `Family()`（对外仍只暴露 内嵌/托管 两类）；
  - `internal/native` 注册表（`Register`/`Lookup`）+ `internal/native/workbuddy` 的 `Boot`（重建上游对象图）；
  - `internal/orchestrator/native.go`（绑 `127.0.0.1:0`、**不进端口表**、产出与子进程逐字段一致的 `RootURL + 就绪状态`）；
  - `presets.go` 的 `workbuddy` 模板改为 `Mode: native, Kind: workbuddy`，用 `LegacyCommands` 认老配置的 `wb2api.exe`；
  - 面板侧：`web/app.js` 回传 `mode` / `gateway_kind` / `data_dir`，原生型自动隐藏子进程专有字段。
- **W3｜回归** ✅ **已完成**（结果见 4.5）。
- **W4｜发版**：并入 ModelMux 的一次常规发版（不单独发版；单独发版的是 Track 2 的 A 线）。

### 4.4 W2 必须验证的四件事

1. 保留 URL 形态 + 进程内 dispatch ⇒ `upstream.js`（63,766 字节）**真的零改动**？
2. workbuddy 的 `config.json` → ModelMux 配置项映射（保留「不覆盖已有 env」语义）。
3. `presets.go` 摘掉 workbuddy 托管预设后，注册表与预设识别的语义变化。
4. `test/verify_upstream_ui.py`（51 项）+ `test/verify_side_console.py`（59 项）**全绿**。

### 4.5 W2/W3 验收结论（2026-10-03 实测）

| # | 验证项 | 结论 |
|---|---|---|
| 1 | `upstream.js` 零改动 | ✅ **63,766 字节，`web/` 与 `src/internal/assets/` 两份 sha256 一致**（`c98dd389…`），全程未编辑过该文件 |
| 2 | `config.json` 映射保留「不覆盖已有文件」语义 | ✅ `Boot` 的 `loadFileConfig` 缺键留默认、文件存在则原样不动；`workbuddy_test.go` 的 `TestBootKeepsExistingConfigFile` 锁住 |
| 3 | 预设识别语义变化 | ✅ 原生型没有 command 锚点，改走 `ManagedPresetByKind`；老配置仍由 `LegacyCommands` 认回 `workbuddy`（`InferManagedPreset` 全套既有用例**未改动仍全绿**） |
| 4 | `verify_upstream_ui.py` | ✅ **51/51** |
| 4 | `verify_side_console.py` | ⚠️ **57/59** —— 2 条失败均指向 **zcode 账号面板**（`#zcAccBody` 的 `rows=0`、额度条 `ratios=[]`）。根因是**数据条件**：`/api/channels/zcode/upstream/accounts` 权威返回 `{"accounts":[]}`（zcode 账号池为空，要加账号只能用户自己做 OAuth）。同批的「账号池有汇总卡」PASS，说明渲染链路完好，缺的只是数据 |
| — | `verify.py` | ✅ 121/121 |
| — | `verify_layout_ui.py` | ⚠️ **32/33** —— 唯一失败是「zcode 渠道存在且可编辑」，同一根因：`account_count === 0` 的托管渠道**按设计整体隐藏**（见 `PITFALLS.md`）。该分支被跳过时连带少 6 项断言（33+6≈39 基线） |
| — | `verify_claim.py` / `verify_zcode_accounts.py` / `verify_pick_ui.py` / `verify_theme.py` / `verify_silent_minimize.py` / `gui_check.py` / `check_sources.py` | ✅ 11/11 · 44/44 · 22/22 · 16/16 · 8/8 · 全通过 · 8/8 |
| — | `go build` / `go vet` / `go test ./...` / `gofmt -l` / `build.py --check` | ✅ 零输出 / 零输出 / 全 ok / 空 / 7 个文件逐字节一致 |

**回归验证的关键前提**：这轮跑的是**真实配置**（`modelmux/config/modelmux.json`），它的 `workbuddy`
与 `zcode` 都**没有 `mode` 字段** ⇒ 被归一成子进程形态 ⇒ 这三条 UI 套件实际验证的是
「老配置升级后行为不变」这条最要紧的回归线，而不是新功能。原生形态由
`internal/native/workbuddy` 的 e2e（起真监听 + 真发 HTTP）与 `internal/web` 的
`native_channel_test.go` 覆盖。

---

## 五、Track 2：A 线 `zcode2api-go`（**全新纯 MIT 仓库** · 单独发版）

**目标**：一个完整可用、可独立部署的 ZCode 网关，纯 Go，无 Python、无 Node，**纯 MIT**。

### 5.1 仓库形态（已更新：不 fork）

```
zcode2api-go/                     ← 全新空仓库，上游文件一个都不进来
├─ go.mod  go.sum                 module github.com/LinYuanNull/zcode2api-go
├─ cmd/zcode2api-go/              main：serve（默认）/ login / claim / set-admin-key
├─ internal/                      ← 全部 Go 实现（本项目核心产出）
│  ├─ store/ models/ settings/ constants/
│  ├─ fingerprint/ identity/ bodytransform/ agent/ compat/
│  ├─ quota/ claim/ oauth/ install/ telemetry/ reqlog/
│  ├─ adminapi/ gateway/ pages/ authadmin/
│  └─ captcha/                    池 + 求解调度
│     └─ cdp/                     自写 CDP 客户端（WebSocket + 6 条命令）
├─ docs/contract/                 契约样本（只保留结构，见 3.3）
├─ PROVENANCE.md                  实现依据登记（靶机版本 / 采样时间 / 样本编号）
├─ LICENSE                        **MIT**（只有这一份）
├─ README.md                      溯源句 + 许可说明
└─ .github/workflows/             CI：go vet / go test / gofmt
```

**为什么不再 fork**（三个理由）：

1. fork 会把 AGPL Python **物理带进仓库** ⇒ 混合许可，读者默认整仓 AGPL，MIT 主张更难讲清。
2. 更要紧的是：**源码同仓会持续诱惑你去对照** —— 而「照着 Python 写 Go」正是翻译风险的唯一来源。
   让上游代码**不在仓库里**，是最省力的结构性保险。
3. fork 原本的理由「便于 diff / 便于回贡」**本来就不成立** —— 上游是 Python 项目，Go 重写无法回贡。

**代价（明确接受）**：失去与上游 diff、失去回贡能力、失去 GitHub 的「forked from」标识、
失去自动跟随上游更新（风险登记 #8 的缓解手段相应改为「定期比对上游 release notes，人工决定是否跟」）。

**上游 Python 的角色**：留在本机 `D:/AiWork/ZCode/zcode2api` 当**契约采样靶机**，不进仓库。

**命名与定位**：`zcode2api-go` 是「zcode2api 的**独立 Go 实现**」，不是「zcode2api 的分支」。
README 里的溯源句照写（事实陈述，不涉及代码复制）。

### 5.2 阶段

- **A0｜立项与骨架**：建空仓库、`go.mod`、`cmd/`、`internal/`、CI、`LICENSE`(MIT)、`PROVENANCE.md` 骨架、README 溯源句。
  **验收**：`go build ./...` 通过（空实现）；CI 绿灯；README 与许可就位。
- **A1｜契约固化（必做第一顺位）**：对着本机靶机跑一遍上游的 22 个管理 API + 3 个网关 API，录真实请求/响应（**含错误分支**）
  → `docs/contract/*.json`（只保留结构）；写 `PROVENANCE.md`；建立提交纪律（3.4）。
  **验收**：契约覆盖全部 **25 个路由**，每个样本都能用现有 Python 复现。
  **双重意义**：① 后续每一期的判据来源；② 独立实现的**唯一证据链**。
- **A2｜账号池与存储**：`store` + `models` + `fingerprint` + `settings` + `constants`。
  **验收**：单测绿；能读现有 `data/accounts.db` 并产出与 Python 侧**逐字段一致**的账号快照（双向读校验）。
- **A3｜管理 API（22 路由）**：路由 + 鉴权（常数时间比较 + IP 失败锁定）+ settings 读写。
  **验收（强契约测试）**：**上游自带的 `frontend/` 面板（本机跑）直接接 Go 后端能正常使用** ——
  这是最划算的端到端契约验证。同时把 `test/verify_zcode_accounts.py`（44 项）指向 Go 实现，**全绿**。
  （注：上游前端不进仓库，只在本机当验收手段。）
- **A4｜转发链路（最难的一期）**：调度器（轮询 / 并发槽 / 跳过策略）、`body_transform`、`identity`、`agent`、
  `openai_compat`、SSE 透传与转换、错误分类与冷却（验证码挑战 / 风控退避 / 额度耗尽 / 401 / 429 / 5xx）。
  **验收**：假上游覆盖全部错误分支；同一份请求序列下与 Python 行为**逐项一致**；SSE 逐 chunk 对齐。
  **风险**：`gateway.py` 811 行里的 asyncio 单线程原子语义（`_inflight` 计数、槽位 park/reacquire）
  在 Go 里必须**重新论证**，不能凭感觉移植。**先写行为对照测试，再写实现。**
  **必须逐字保留**：出站代理语义 —— messages 走环境代理，**billing / 验证码强制直连**（否则触发风控）。
- **A5｜额度、领取、登录**：`quota` + `claim` + `oauth` + `install` + `telemetry`。
  登录按 WorkBuddy 形态实现 `login url` / `login poll`（纯 HTTP，用户浏览器完成）；
  领取两条通路：自动（走验证码求解）+ 手动（收外部 param）。
  **验收**：OAuth 全流程走通；手动领取可用；`verify_claim.py` 的 11 项语义在 Go 侧复现。
- **A6｜验证码（Go 自写 CDP）**：自写极简 CDP 客户端（约 300 行）替代 `node solver_pw.js`（208 行）。
  定位 Chromium（**优先系统 Edge**）→ 反探测补丁（`Page.addScriptToEvaluateOnNewDocument`）→
  打开 `file://solver-page.html` → `Emulation.setUserAgentOverride` → `Runtime.evaluate(awaitPromise)`
  调 `startTracelessVerification()` → 取 `captchaVerifyParam` → 关目标退出。
  **只用到 6 条命令**：`Target.createTarget`、`Page.navigate`、`Page.addScriptToEvaluateOnNewDocument`、
  `Emulation.setUserAgentOverride`、`Runtime.evaluate`、`Target.closeTarget`。
  - **Windows 参数必须重调**：`solver_pw.js` 的 `--single-process` / `--no-zygote` / oom 看门狗是
    **Linux 小内存容器专用**；`headless:true`（old headless）在 Chrome 132+ **已被移除**，
    需改用 `--headless=new` 或 `chrome-headless-shell`。**这两点直接决定能否过无痕验证，必须实测。**
  - **SDK 加载**：上游 `page.html` 从 `https://o.alicdn.com/...` 远程加载，浏览器需能直连阿里 CDN。
  - **池**：`CAPTCHA_TOKEN_TTL = 95 秒`，池 `MIN=1 / MAX=2`。领取频率为**每天一次**，
    所以求解量 1–2 次/天，**Chromium 不必常驻**，可实现为「用时现解」。
  - **兜底（必做）**：同时实现 `GET /claim/captcha-config` + `POST /claim/manual` 人工路径。
  - **验收**：能解出**真实** `verifyParam` 并成功用于一次真实领取，且连续多次成功。
- **A7｜发布 v0.1.0（独立里程碑）**：打 tag、走 GitHub Git Data API 发布。
  Release 资产：源码 zip + `zcode2api-go-windows-amd64.exe` / `linux-amd64`。**不含 Chromium**。
  **验收**：全新机器上「下载 exe → `serve` → 面板可用 → 登录账号 → 领取成功」全流程走通。

---

## 六、Track 3：zcode 内嵌进 ModelMux

**前提**：Track 1 的接缝架构已验证 + Track 2 的 v0.1.0 已发版并稳定。

- **B0｜改写为 provider 形态（是「改写」，不是「复制」）**：把 A 线的核心逻辑按 ModelMux 现有接口面
  （`internal/provider` 的 `Upstream` / `Channel` / `Registry`）**重新组织**成 `src/internal/provider/zcode/`。
  **去掉**：独立 HTTP server、CLI。**保留并改写**：store / models / 调度器 / body 变换 / identity /
  `openai_compat` / quota / claim / captcha。
  顺带收益：这次「按新接口面重组」本身**进一步强化了 MIT 主张**（不是原样搬运）。
- **B1｜接线**：套用 Track 1 已验证的接缝方案。
  | 事项 | 变化 |
  |---|---|
  | `presets.go` | zcode 从**托管型**预设改为**原生型** provider；`managed_providers` 里的 zcode 条目下线 |
  | 代理路径 | **保留** `/api/channels/zcode/upstream/<rest>` 的 URL 形态，只换实现（**不是删除**，前端零改动） |
  | 前端 | `web/zcode.js` 目标是**零改动** |
  | **密码** | `config.Claim.AdminKey` 与网关侧 admin_key 的**两处同步取消** —— 合并后只剩一处。**这是最大的简化收益**，同时消掉「只改一边就整块 401」的经典故障 |
  | settings | 上游 `.env` 那套改为 ModelMux 配置项（保留「不覆盖已有 env」语义） |
- **B2｜回归**：全量 e2e（第九节基线）+ `build.py` + `go vet/test` + `gofmt`。
  **验收**：现有 11 个脚本全绿；删除 Python 目录后功能不受影响。
- **B3｜发布 ModelMux v0.3.0**：同步改写 `README.md:122/183`（见 3.5）与「已并入的 Provider」表。
  **验收**：发布物里不含任何 AGPL 代码；`github.com/LinYuanNull/modelmux` 仍是 MIT。

---

## 七、Track 4：trae 内嵌（上游 `connectedGraph/trae2api-web` · 可并行）

**前提**：无（不依赖 Track 1/2/3）。**建议位置**：与 Track 3 并行；因为它比 zcode 简单得多，也可插在 Track 2 之前热身。

**性质**：与 Track 1 **同类** —— **MIT + Go ⇒ fork + 照搬内嵌**（**不是重写**）。这是第 9 轮换上游后最大的变化。

**规模**：代码量比 zcode 小，但能力面**远大于**旧上游 `linqiu919/trae2api` ——
它**自带管理面**（账号池调度 / 冷却状态机 / 每日签到 / Web 控制台 / 登录闭环），不是「只有 2 个端点」的裸转发。

### 7.1 阶段

- **T0｜fork + 契约采样**：fork `connectedGraph/trae2api-web`，本机副本命名 **`trae2api-web-panel`**
  （对齐既有 `workbuddy2api-panel` 的命名习惯；本机 `git push` 不通 ⇒ 走 GitHub Git Data API，见 `PUBLISHING.md`）。
  对着它跑一遍并录制：`/v1/models`、`/v1/chat/completions`（流式 + 非流式）、`/status`、
  `/admin` 的写操作（导入 / 启停 / 删除）、`signin` 签到（**含 1005 / 429 / 401 / 5xx 冷却分支**）
  → `docs/contract/*.json`（只保留结构，见 3.3）。
- **T1｜源码落位**：照搬进 `src/internal/provider/trae/`，**保留上游文件头与 MIT 声明**。
  **去掉**：`cmd/` 中不需要的入口、`Dockerfile` / `docker-compose.yml` / `login.sh` / `signin.sh` / `credit.sh`
  （这些运维职责改由 ModelMux 面板与接口承担）。
  **保留并接线**：账号池调度器、冷却/禁用状态机、签到 scheduler、凭证 store（`auths/` + `state.json`）、
  OpenAI 兼容层、登录闭环回调（`TW2A_CALLBACK_PORT`）。
- **T2｜接线**：套用 Track 1 已验证的四条接缝（`presets.go` / `proxy.go` / `claim_api.go` / 数据面转发）。

  | 事项 | 变化 |
  |---|---|
  | `presets.go` | trae 从**托管型**预设改为**原生型** provider；`managed_providers` 里的 trae 条目下线 |
  | 代理路径 | **保留** `/api/channels/trae/upstream/<rest>` 的 URL 形态，只换实现（前端零改动） |
  | **签到** | **等同 zcode 的 claim，属接缝 ③**（`claim_api.go`）——**独立第二条路、最容易漏改**，必须一起接 |
  | 配置映射 | `TW2A_API_KEY` / `TW2A_AUTH_DIR` / `TW2A_STATE_FILE` / `TW2A_ERR_THRESHOLD` / `TW2A_ERR_COOLDOWN` 等 → ModelMux 配置项 |
  | **设备号** | **新增**：每账号绑定稳定设备号；9074 命中后换号重试（trae 特有风控，见 2.4） |

- **T3｜回归 + 发布**：全量 e2e（第九节基线）+ `build.py` + `go vet/test` + `gofmt`；并入 ModelMux 发版（不单独发版）。

### 7.2 同步修正

- `presets.go:207` 的 `Label: "Trae 本地网关"` 改为不叫「本地网关」（建议「Trae（原生）」）；
  上游 `DocURL` 从 `linqiu919/trae2api` 指向 **`connectedGraph/trae2api-web`**。
- `README.md` 的「已并入的 Provider」表补 trae 一行（MIT 来源 · 照搬）。

### 7.3 备选与后续

`lg996/cpa-multi-plugins`（MIT · Go）作**协议补充**保留：它覆盖 Trae **三变体（CN / SOLO CN / Intl）**与
Intl Web SOLO remote 协议，并独有 **9074 限流识别**与更细的模型表对齐；
若 `trae2api-web` 对 Intl / SOLO 覆盖不足，按同一「照搬」路径补入（许可同为 MIT，无额外障碍）。

⚠️ **串行约束**：Track 4 与 Track 1 / 3 **都要碰 `proxy.go` / `presets.go` / `claim_api.go`** ⇒
这三处必须串行改（同一轮只允许一个改动者），不可并行写同一文件。

---

## 八、模块映射（zcode Python → Go）

| 原文件 | 行数 | Go 目标 | 风险 |
|---|---|---|---|
| `constants.py` | 135 | `zcode/constants.go` | 低 |
| `settings.py` | 157 | `zcode/settings.go`（保留「不覆盖已有 env」语义） | 低 |
| `models.py` | 222 | `zcode/account.go`（状态机 + 可选中/冷却/风控） | 低 |
| `store.py` | 320 | `zcode/store.go`（纯 Go SQLite + 内存常驻 + 线程安全重做） | 中 |
| `fingerprint.py` | 210 | `zcode/fingerprint.go` | 低 |
| `identity.py` | 125 | `zcode/identity.go`（`pio` 身份头 + 追踪头） | 低 |
| `body_transform.py` | 154 | `zcode/body.go`（system 身份块 / cache_control / user_id） | 低 |
| `agent.py` | 127 | `zcode/agent.go`（端点选择 / 鉴权头 / 透传清洗表） | 低 |
| `openai_compat.py` | 305 | `zcode/compat.go`（双向转换 + 流式转换器） | 低 |
| `auth_admin.py` | 103 | `zcode/auth.go`（常数时间比较 + IP 失败锁定） | 低 |
| `quota.py` | 287 | `zcode/quota.go` | 中 |
| `claim.py` | 556 | `zcode/claim.go` | 中 |
| `oauth.py` | 124 | `zcode/oauth.go` | 低 |
| `install.py` | 181 | `zcode/install.go` | 低 |
| `telemetry.py` | 75 | `zcode/telemetry.go` | 低 |
| `reqlog.py` / `logs.py` | 159 | `zcode/reqlog.go`（环形缓冲） | 低 |
| `routes/admin_api.py` | 639 | `zcode/adminapi.go`（22 路由） | 中 |
| `routes/gateway.py` | 811 | `zcode/gateway.go`（**重灾区**） | **高** |
| `routes/pages.py` | 59 | `zcode/pages.go`（`/meta` 等） | 低 |
| `captcha.py` | 277 | `zcode/captcha.go`（实时求解 + 人工 param 兜底两条通路） | 中 |
| `captcha_node/solver_pw.js` | 208 | `zcode/cdp/solver.go`（**Go 自写 CDP 客户端**，约 300 行） | **高** |
| `cli.py` | 262 | 由 ModelMux 面板/接口取代，仅保留必要运维子命令 | 低 |

**规模**：Python 生产代码 ~6,400 行；Go 侧预计 **6,000–8,000 行**（含测试）。

---

## 九、验收基线（Track 1 / 3 回归用）

| 脚本 | 项数 | 覆盖 |
|---|---|---|
| `test/verify.py` | 121 | 端到端：托管型全生命周期、端口热切换、静态资源逐字节一致 |
| `test/verify_upstream_ui.py` | 51 | 上游控制台逐视图 |
| `test/verify_side_console.py` | 59 | 侧栏渠道卡与常驻控制台入口 + 网关设置可写 |
| `test/verify_zcode_accounts.py` | 44 | zcode 账号面板端到端 |
| `test/verify_layout_ui.py` | 38 | 侧栏 / 概览拆分 / 渠道嵌套编辑 |
| `test/verify_pick_ui.py` | 22 | 拉取模型全选与结果弹窗 |
| `test/verify_theme.py` | 16 | 首帧主题（防亮暗闪烁） |
| `test/verify_claim.py` | 11 | 限时套餐定时领取链路 |
| `test/gui_check.py` | 9 | GUI 生命周期（托盘 / 窗口） |
| `test/verify_silent_minimize.py` | 8 | 最小化 / 隐藏不产生系统通知 |
| `test/check_sources.py` | 8 | 源码级不变式 |

外加：`python tools/release/build.py`（同步 `web/` → `src/internal/assets/` 并编译）、
`go vet -C src ./...`、`go test -C src ./...`、`gofmt -l src`。

> Track 2 自带的 `go test ./...` 全绿是 A7 发版的前置条件。

---

## 十、风险登记

| # | 风险 | 影响 | 缓解 |
|---|---|---|---|
| 1 | 新实现实为上游的「翻译」⇒ 衍生作品，MIT 主张失效 | 项目落入 AGPL | **契约固化 + `PROVENANCE.md` + 提交纪律**（唯一凭据）；**上游源码不入仓库**；实现时不打开上游源码 |
| 2 | A4 并发语义重写（asyncio 单线程原子语义） | 工期最大变量 | 先写行为对照测试再写实现；允许拆得更细 |
| 3 | Windows 上 headless 形态变化（old headless 已被移除） | A6 可能跑不通 | 一开始就实测 `--headless=new` 与 `chrome-headless-shell` 两条路；保留人工兜底 |
| 4 | 阿里云风控升级导致自动求解失效 | 无人值守断掉 | 保留人工兜底路径（上游本来就备着两条路） |
| 5 | 本机 `git push` 不通 | 无法常规发版 | 全走 GitHub Git Data API（`PUBLISHING.md` 已有 runbook） |
| 6 | `data/accounts.db` 兼容 | 用户账号丢失 | A2 做双向读校验；迁移前先备份 |
| 7 | SQLite 驱动体积 | 打包体积 | `modernc.org/sqlite`（纯 Go，无 CGO） |
| 8 | 上游演进，独立实现落后 | 契约漂移 | 定期对照上游 release notes，人工决定是否跟（**不 fork 后失去自动跟随能力，已接受**） |
| 9 | **命名冲突**：新的「原生型」与既有 `EmbeddedProvider`（也叫内嵌型）混淆 | 代码与文档双轨 | **Track 1 开工前先定名**（见第一节） |
| 10 | workbuddy fork 后上游分叉 | merge 冲突 | 保持 `internal/` 内改动集中、少碰上游文件；定期 merge |
| 11 | workbuddy 内嵌引入 `go-redis` | 依赖面从 1 变 2 | 已核实 Redis 可选（Noop 降级），不影响零依赖体验 |
| 12 | **上游继承链断裂**：`traework2api` 已 404，`trae2api-web` 是其继承者 | 溯源断层 | 许可上只认 `trae2api-web` 自己的 MIT；在 `PROVENANCE.md` 记录继承声明与采样时间（见 2.3） |
| 13 | new-api 无法内嵌，与「零子进程」目标不一致 | 目标打折 | 已明确列为**有意例外**；另给两条替代路（见 2.1） |
| 14 | trae 上游是 MIT，误以为「可以复制、不必保留声明」 | 违反 MIT 条款 | 照搬必须**保留版权声明 + 许可文本**（见 2.3） |
| 15 | trae 上游以「TRAE SOLO CN」为主，Intl / SOLO 覆盖可能不足 | 版本覆盖受限 | 备选 `lg996/cpa-multi-plugins`（MIT Go，覆盖 CN / SOLO CN / Intl）作协议补充（见 7.3） |
| 16 | trae 设备号风控（9074 / 1105）导致签到失败 | 领取链路断 | 每账号绑定稳定设备号；9074 命中换号重试；1105 提示换网络（见 2.4） |

---

## 十一、调研证据链（保留，避免重复调研）

**许可与形态：**

| 事实 | 出处 |
|---|---|
| workbuddy 上游是 **MIT 纯 Go** | `D:/workbuddy2api-panel/LICENSE`（`Copyright (c) 2026 Sliverkiss`）、`go.mod`（module `github.com/linguo2625469/workbuddy2api-panel`） |
| workbuddy 的**真实上游**是 `linguo2625469/workbuddy2api-panel` | 2026-10-03 GitHub API 实测：`200 / Go / MIT / fork=false / pushed 2026-10-01`；而 `Sliverkiss/workbuddy2api` **404** |
| workbuddy **零外部进程** | `exec.Command` 仅出现在 `internal/panel/frontend_test.go` |
| workbuddy **Redis 可选，有 Noop 降级** | `internal/redisstore/redisstore.go:1,7,56,59` |
| workbuddy 规模 | 生产 23,467 行 / 74 文件；测试 16,250 行 / 56 文件 |
| zcode 上游是 **AGPL-3.0 Python** | `presets.go` zcode 条 `Note`；上游仓库 LICENSE |
| new-api 上游是 **AGPL-3.0** | `presets.go:18x` 该条 `Note`「AGPL-3.0。只以独立进程方式调用、不随包分发」；DocURL `QuantumNous/new-api` |
| trae 上游是 **`connectedGraph/trae2api-web`**，**MIT 纯 Go** | 仓库 `LICENSE`（`docs: add MIT license` 提交，2026-08-24）；README「本项目基于 MIT License 许可发布」 |
| `trae2api-web` **三项能力齐备** | README：多账号智能调度池（积分降序 + 1005/429/401/5xx 冷却）+ 每日定时签到 + `/admin` 管理面板 + 登录闭环 |
| `trae2api-web` **纯标准库、零第三方运行时依赖** | README「纯 Go 标准库开发，零第三方运行时依赖」；`go.mod` |
| `trae2api-web` 继承自 `Sliverkiss/traework2api` | README 首行「基于上游 Sliverkiss/traework2api 改进」；该上游 **已 404** |
| `lg996/cpa-multi-plugins` 是 MIT Go 备选 | README「License: MIT」；覆盖 Trae CN / SOLO CN / Intl 三变体，含 9074 限流识别 |
| trae 风控码 **9074 / 1105** | `L0NE-6/Trae-AutoCheckin` README「9074 自动换号」「机房 / 代理 / VPN 可能触发 1105 风控和滑块验证」 |
| ~~`linqiu919/trae2api`~~ **弃用** | 无账号池 / 无领取 / 无验证码 + 无 LICENSE 文件 + 已停更 |
| `trae-local-api` / `dsh-trae-api` **不采用** | 依赖本机 IDE 登录态、破解 Trae CN 的 `tc` 加密（AES-128-CBC + SHA-512） |

**ModelMux 现有接缝（Track 1/3 都要改这四条）：**

| 接缝 | 代码位置 | 备注 |
|---|---|---|
| ① 前端原生视图 | `web/upstream.js`（63,766 B，workbuddy）、`web/zcode.js`（48,956 B，zcode） | **都是 ModelMux 自己写的原生视图，不是 iframe**（首页 CSP `default-src 'none'` 无 `frame-src`） |
| ② 控制台代理 | `src/internal/web/proxy.go` `handleChannelUpstream` | **通用托管渠道代理**，workbuddy 也走它（`zcode_panel_test.go` 有 `TestWorkBuddyPanelProxyUnchanged` 断言）⇒ **不能删路径，只能换实现** |
| ③ **领取执行器（独立第二条路）** | `src/internal/web/claim_api.go` `postClaim`，常量 `claimAdminPath = "/admin/api/claim"` | 直接用 `up.RootURL`，**不走 ②** ⇒ 最容易漏改 |
| ④ 数据面转发 | `internal/provider` forward → `up.BaseURL` | `/v1`，含 SSE |

**验证码与登录（Track 2 用）：**

| 事实 | 出处 |
|---|---|
| 上游作者自述无头求解不可用 | `app/routes/admin_api.py:516`「verify_param 必须来自用户浏览器内阿里 SDK 滑块成功回调（无头环境无法求解）」 |
| 官方人工路径两个接口 | `GET /claim/captcha-config`（`admin_api.py:498`）、`POST /claim/manual`（`:512`） |
| 浏览器侧实现**已存在** | `frontend/admin/accounts.html:654-724` |
| 验证码只出现在领取环节 | `app/claim.py:406` / `:368`，无其他调用点 |
| param 寿命只有 **95 秒** | `app/settings.py:67` `CAPTCHA_TOKEN_TTL = 95_000`；池 `MIN=1` / `MAX=2` ⇒ **人工无法当主路** |
| SDK 初始化参数是静态默认值 | `app/constants.py:72` `CAPTCHA_DEFAULTS` |
| SDK 由远端加载 | `captcha_node/page.html` 的 `<script src="https://o.alicdn.com/...">` |
| 登录是 OAuth（用户浏览器 + poll） | `app/oauth.py`：`/oauth/cli/init` → `authorize_url` → `/oauth/cli/poll/{flow_id}` → `exchange_api_key` |
| WorkBuddy 是同一形态的纯 Go 参考 | `D:/workbuddy2api-panel`（39,717 行 Go）；`cmd/login/main.go` 的 `url` / `poll` 子命令 |
| 代理语义必须保留 | messages 走环境代理；billing / 验证码**强制直连**（`app/captcha.py:_run_solver` 显式剥离 `*_PROXY`） |

**已作废的候选（不要再走一遍）：**

- ~~Miniblink49~~：内核 Chromium **v57 / 2017 年**、单进程、C++ 需 CGO，跑不动现代 SDK。
- ~~按需拉取 `chrome-headless-shell` 作主路~~（zip 88 MB / 解压 189 MB）：仅当系统确实无 Edge/Chrome 时兜底。
- ~~复用桌面壳 WebView2 开 `--remote-debugging-port` 走 CDP~~：单 WebView 模型下 CDP 能力受限，且等于把渲染器暴露给本机任意进程。
- ~~A 线 fork 上游仓库~~：会把 AGPL 代码带进仓库（第 6 轮改为全新空仓库）。
- ~~`trae-local-api` / `dsh-trae-api`~~：依赖本机 IDE 登录态 + 破解商业产品加密格式，形态不干净。
- ~~`linqiu919/trae2api`~~：**第 9 轮弃用** —— 只有 2 个端点，**无账号池 / 无领取 / 无验证码**，且无 LICENSE。
- ~~A 线双许可 `MIT OR AGPL-3.0`~~：不 fork 后不再需要，直接纯 MIT。

---

## 十二、执行顺序总览

```
Track 1   workbuddy 内嵌（先行 · 架构试验田）
  W0 fork 仓库 ─▶ W1 源码照搬落位 ─▶ W2 进程内 dispatch 接线 ─▶ W3 全量回归 ─▶ W4 随版发布
     ✅ 已完成      ✅ 已完成            ⏳ 进行中（先定名：NativeProvider ✅ 已定）
                                   ⬇  接缝架构验证完毕

Track 2   A 线 zcode2api-go（全新纯 MIT 仓库 · 单独发版）
  A0 骨架 ─▶ A1 契约固化 ─▶ A2 账号池 ─▶ A3 管理API ─▶ A4 转发链路 ─▶ A5 领取/登录 ─▶ A6 验证码 ─▶ A7 发 v0.1.0
                                                                        （每期可独立验证、可随时停手）
                                   ⬇  A 线发版并稳定后

Track 3   zcode 内嵌进 ModelMux
  B0 改写 provider ─▶ B1 接线（presets/前端/配置） ─▶ B2 全量回归 ─▶ B3 发 ModelMux v0.3.0

Track 4   trae 内嵌（上游 connectedGraph/trae2api-web · MIT+Go ⇒ 照搬 · 可并行）
  T0 fork+契约采样 ─▶ T1 源码照搬落位 ─▶ T2 四条接缝接线（含签到=接缝③） ─▶ T3 回归+随版发布

例外      new-api —— 有意保留为托管型子进程，不内嵌（理由见 2.1）
```

**当前进度**：① 第三类渠道**已定名 `NativeProvider`**（第一节）✅；② **Track 1 的 W0 / W1 / W2 / W3 已完成**（fork + 104 个 Go 文件落位 + 四条接缝改进程内 dispatch + 全量回归，见 4.5）；③ **下一步 = W4**（并入 ModelMux 的一次常规发版，不单独发版）。

Track 4 因上游换为 **MIT + Go**，与 **Track 1 同路**（照搬，非重写）⇒ 可在 Track 1 的 W2 把接缝架构跑通后**立即并行铺开**。

Track 2 的第一步是 **A1 契约固化** —— 它是后续每一期的判据来源，也是「独立实现」主张的唯一证据链。
