# CommandCode 原生 CPA 账号池插件

## 当前已上线：0.1.9-local（2026-10-10）

本轮修复 Claude 源请求里 `tool_result.content` 携带 image 块时，Chat Completions
与 Responses 两条目标路由一律 HTTP400（`unsupported tool_result block type
"image"`）的问题。共享内核改为 `ToolResultParts`：文本、`tool_reference` 与图片块
分别解析为结果文本和 http(s)/data 图片 URL；CC 路由把图片转存到该回合全部 tool 消息
之后的 user 载体消息（CC 上游仅接受 user 消息带图，且并行工具调用的 tool 消息必须
连续，载体不插入其间），Responses 路由把图片作为
`function_call_output` 输出的 `input_image` 部件原位携带，会话派生摘要纳入图片
URL。`is_error` 前缀、块顺序、未知块类型（如 video）的描述性拒绝全部保留；
非法图片 source 仍为 HTTP400。deepseek 等视觉模型由此可以真正读到工具返回的图片。

2026-10-10 15:21（北京时间）已无重启冲突上线为 `commandcode-pool-next` /
`0.1.9-local`：备份目录 `toolresult-image-migration-20261010T071334Z/`（旧库、
config、state 快照）；配置/库文件热载不替换已加载 Manager（行为探针证明旧实例仍在
翻译），最终经 `systemctl --user restart cpa-core` 完成切换，切换前核实 Codex bans
为空、无在途请求。真实验收：`cc-deepseek-v4.1-flash` 带 base64 图片 tool_result
的非流与流式请求均 HTTP200，模型真实描述图片内容（浅粉纯色 1×1 像素），结束
inflight=0。测试与验证边界见 [VALIDATION.md](VALIDATION.md)。

## 历史候选：0.1.8-local（2026-10-10）

本版增加可复用的有界回归工具与可信逐请求追踪，不改变账号排序、真实额度来源、
共享硬 cap、Codex 保护或 owner 自然结算。

- 回归入口默认离线，不读取生产凭据、不请求模型。离线证据分析与有界真实探针均有独立
  入口；真实探针必须显式授权、确认运行身份和预算，既有 journal 不覆盖、不重发。
- 事件兼容追加 `trace_id`，连接宿主提供的入站 TraceID 与原有 `request_id`、`attempt_id`。
  两类 ID 不互相替代；TraceID 缺失或格式异常时保持旧执行行为，不猜测客户端关联。
- 页面仍按宿主请求一行、主层显示账号名称；TraceID 和过程记录放在技术详情。
  客户端同名私有头先清除再由宿主字段注入，内部头不转发上游。

使用与验证边界见 [REGRESSION.md](REGRESSION.md)、[TRACE.md](TRACE.md)
和 [VALIDATION.md](VALIDATION.md)。

## 历史已上线版本：0.1.7-local（2026-10-10，北京时间）

本轮修复 Claude `ToolSearch` 返回的 `tool_result.content` 内 `tool_reference` 兼容。
引用转换为工具结果文本，保留工具名称；当前请求有对应声明时保留描述和完整参数定义，
没有声明时明确提示不可用，不捏造定义。文本与引用混合保持顺序，不把工具结果提升为
系统指令；工具调用 ID 和 `is_error` 保留。Chat Completions、Responses 和会话派生
共用转换规则，畸形引用以及其他不支持的结果块仍在上游 I/O 前拒绝为 HTTP400。

本轮不修改延迟工具声明策略、账号池排序、共享 cap、Codex 保护、配额横条或六名称路由。
2026-10-09 17:19 UTC（北京时间 2026-10-10 01:19）已无重启上线为
`commandcode-pool-next` / `0.1.7-local`；真实 Claude 当前配置网关的非流式和流式各一次
均 HTTP200、正文“兼容成功”、正常结束，两侧日志按 session 精确关联。测试、原生验收、
生产读回和验证边界见 [VALIDATION.md](VALIDATION.md)；构建本身仍不自动部署。

## 历史已上线版本：0.1.6-local（2026-10-09）

账号池页面按真实账号分组展示名称与 **5 小时、周、月剩余配额横条**。各窗口的重置时间直接显示，
订阅周期结束独立显示，统一使用北京时间；指纹、ID、分组、套餐和并发等技术信息收进“详情”。
原有逐凭据编辑、启用、刷新、删除、批量导入以及真实请求追踪保留。同一真实账号的多个 Key
不会重复相加额度，图条仅表示展示比例，不参与账号调度。

5 小时和周额度来自上游窗口。月额度仅使用 `monthlyCredits`，总额按现有套餐表推导，
购买额度与免费额度不混算进月条；未知套餐没有可信分母时只显示月度余额，不显示 0%。
`currentPeriodEnd` 只表示订阅本期结束，不等于永久失效或已确认的月额度重置；没有重置时间时
明确显示待确认。过期或刷新失败的读数不能视作实时额度，图条减淡并显示异常说明。

本版本在已合并的 0.1.5 上增量实现，保持原有额度准入、共享硬 cap、Codex 可信凭据保护及
Anthropic 历史兼容。构建仍不自动安装；本轮验证及部署边界见 [VALIDATION.md](VALIDATION.md)。
2026-10-09 14:56 UTC 已无重启接管为 `commandcode-pool-update` / `0.1.6-local`，15:01 UTC
最终读回通过：两真实账号仍启用、各共享并发 cap10，两菜单正常；月重置时间仍待确认。
实际活动 ID 和后续变化仍须以现场读回及交付报告为准。

## 历史已上线版本：0.1.5-local（2026-10-09）

`0.1.5-local` 在已验收的 0.1.4 可信凭据身份保护之上，修复 CommandCode 的
Anthropic 历史消息级 `system` 兼容：顶层 `system` 仍是系统指令；历史提醒包装为
`user` 的 `<system-reminder>`，不会提升权限。并行工具结果可分多条用户消息返回，
结果完整对应后按调用 ID 对齐，再输出提醒及普通内容；不完整结果不伪造、不丢失。
Chat Completions 与 Responses 共用该历史处理，保留工具参数、结果及错误标记。
上游 I/O 前的坏输入返回 400、不可重试并释放 owner，不改真正上游错误的分类。

2026-10-09 07:56 UTC 已无重启接管到 `commandcode-pool-next` / `0.1.5-local`，
保全全部未过期 hold，无 Unban、取消在途请求或业务路由删除；两个账号原 Group、
enabled、各 cap10 与六名称保持。真实 CLI 非流/流各一次均返回“兼容成功”、200/end_turn，
CC Switch 与 CPA usage 按各自 session 精确关联；没有改默认模型映射或重发成功请求。
完整边界与证据见 [VALIDATION.md](VALIDATION.md)。

## 历史已上线版本：0.1.4-local（2026-10-09）

源码版本 `0.1.4-local` 修复第三方 Responses API key 被 Codex quota guard 误封的问题。
只对宿主确认的 Codex OAuth 429 建立配额 hold；BigModel 等 API key 的正常 429 仍由
CPA 原有冷却和退避处理。历史 API key hold 不再阻塞可信的 API key 候选，但不会自动
清空其他 hold。判断不依赖模型名、上游域名、客户端 Key 或错误正文。

Go 回归、race、vet 和同生产二进制的隔离原生 A/B 检查通过，独立 Sonnet 审查无确认缺陷。
2026-10-09 06:17 UTC 已无重启热切换到 `commandcode-pool-update` / `0.1.4-local`；
真实 Desktop 专属链路分别调用 Flash/GLM5.3，均 HTTP200、正文“验证成功”、正常 end_turn，
CC Switch 与 CPA usage 均按 session 精确关联，验收后无 BigModel hold。
生产 PID1948812 保持不变；接管脚本的补充独立 Sonnet 审查已完成，无确认缺陷。
上述 0.1.4 当时不包含 Anthropic 历史消息级 system 提醒兼容修复；当前 0.1.5 已补实现与回归。

此前基线 `commandcode-pool-next` / `0.1.3-local` 的旧 CommandCode 客户端名、裸
上游名和 `commandcode/` 名称继续经过同一账号池。两个真实账号保持启用、原 Group、各
cap10；别名不新建账号、不拆分并发。请求汇总页 `/pool` 一请求一行，
`GET /events?scope=requests` 为请求过程视图，默认 raw 接口兼容。
它复用 [mczhoucn/commandcode-go-cliproxyapi](https://github.com/mczhoucn/commandcode-go-cliproxyapi)
的 MIT 基座，固定提交为
[`ea84cdd799564f644c6f9f39c7dc356d0f013526`](https://github.com/mczhoucn/commandcode-go-cliproxyapi/tree/ea84cdd799564f644c6f9f39c7dc356d0f013526)。
原始 [LICENSE](LICENSE) 保留，来源、修改范围和第三方许可见 [NOTICE.md](NOTICE.md)。

它还在同一个 Scheduler 中提供 Codex 429 窗口临时禁用、到期恢复和手动解禁；
其他 provider 未被过滤时保留 CPA 自身调度，不引入定制 CPA 核心。

## 统一调度（方案一）

- `usage.handle` 仅消费宿主确认 `provider=codex` 且 `AuthType=oauth` 的失败 429，使用
  五小时/周窗口 reset；真正 OAuth 缺少可靠 header 时仍保守禁用五小时。重复 429 不缩短
  有效期限，双窗口都满时取较晚的有效 reset。API key、缺失或未知身份不新建该 hold。
- Codex 过滤、列表与手动解禁使用同一份 Manager 内存状态。配置热更新保留该状态，
  新进程重启后丢失，不写 OAuth 文件或修改 CPAMP 的禁用记录。
- 受过滤的请求不再委托内建调度重新读取全候选；全部禁用时明确拒绝。
  混合候选保留优先级与其他 provider 的可用回退，CommandCode 候选仍检查真实额度。
- **额度评分范围：** CommandCode 应使用独立 `commandcode/` 路由。无 Codex hold 的跨
  provider 混合路由保留 CPA 内建策略，不保证余额择优；实际 CommandCode 请求仍须通过
  `Pool.Acquire` 的额度准入和共享硬 cap，不能靠混合路由绕过。
- 原生资源 `/pool`（CommandCode 账号池）和 `/codex`（Codex 429 保护）属于同一插件。
  手动解禁只清本插件内存，不会清 CPA 内建冷却或 CPAMP 自动禁用的 auth 文件。
- CPAMP `1.14.4` 有独立的配额自动禁用/恢复能力；本次现用接入已只读核实相关开关开启。
  它与本插件的 Codex 内存过滤独立，不共享同一份 hold。
- Codex 规则适配固定 MIT 插件，保留完整第三方许可；cmdcode2api 仅作功能设计参考，
  没有复制其未明确授权的源码，也没有借此新增不在需求内的 OAuth 功能。

实现合同、未来切换/状态迁移及回退边界见 [UNIFIED_SCHEDULER.md](UNIFIED_SCHEDULER.md)。

## 当前交付与验证边界

- CPA SDK 固定 `v8.0.20`，原生 ABI **1**、RPC schema **6**。0.1.7 历史原生检查使用
  CPA `8.0.21` / SHA256 `a4eaa1c1…`；本轮候选隔离目标为 `8.0.23+toolsearch` /
  SHA256 `44600a36…`。两次身份与验证记录分开，不把 SDK 缓存当新 core 的完整源码。
- 本轮源码与产物为 `0.1.8-local`，仅补回归工具及可信请求追踪；不修改账号池排序、cap、
  配额展示、六名称路由或其他渠道配置。0.1.7 的真实 CLI 与 0.1.6 配额界面验收不冒充
  本轮重测；候选验证与生产部署分别记录，见 [VALIDATION.md](VALIDATION.md)。
- 更早的 cap=1 测试、双入口及 `0.1.1` / `0.1.2` 安装信息仅是历史验收阶段。
- 仅支持 **Linux amd64、单个 CPA 进程、本地文件存储**。使用 Linux `syscall.Flock`
  排斥共用 auth/state 路径的另一个池；这不是跨实例协调机制。
  **不支持 Home，不支持多实例共享账号并发，也不声明跨主机安全**。
- 本包不自动部署、提交、发布、重启生产服务或使用真实账号。真实账号、生产部署和发布
  仍需分别授权。

### 自动更新范围（2026-10-09 核验）

现用每日更新脚本只替换 CPA core binary，不覆盖 `plugins/` 或插件配置；CPAMP 自身
更新不自动安装本插件。生产进程未配置 Home JWT，同步远端插件的 Home 启动路径未启用；
独立插件仓已有 `v0.1.5-local` GitHub Release；发布本身不触发部署。上述现有自动更新路径不会把本轮已安装库换回旧版。

这不保证任意未来 CPA 版本的 SDK 兼容，也不防止显式调用插件商店安装其他版本。
版本含 `-local` 后缀，不能仅信“有更新”提示判断升级/降级；手工更新仍须核对
版本、库 SHA256、可信身份回归和生产读回，不触发每日更新来代替验收。

### Guard-only 受控接管模式

候选配置提供 `guard-only`，默认 `false`。仅在经过授权的受控接管场景考虑启用；不要仅为
安装/更新插件就在生产配置中自行打开。初始以 `true` 注册时，只提供 Codex Scheduler、
Usage 记录、原生 Codex 页面与 Codex 管理入口，不加载账号池、不持 pool 锁，也不访问
账号 auth、模型目录或额度。对同一个 Manager 热重配为 `false` 时才激活账号池，同时保留
Codex hold 与 Usage 计数；已有账号池活动时不能反向切回 `true`。已有有效注册后重配置失败
会保留最后有效模式的注册能力、Guard 与计数，并提供脱敏 `activation_error`；不会仅因非法
配置而让宿主移除保护。首次注册失败仍报错。隔离原生验证与本轮生产接管结果见
[VALIDATION.md](VALIDATION.md)，构建此能力本身不自动授权后续生产切换。

## 历史接入限制（2026-10-08，非当前迁入目标）

现用 CPA `8317` / CPAMP `18317` 运行 `0.1.1-local` 完整池，仅保留一组菜单，
两个真实账号保持启用，最后已验当前cap=10/10。请求头/显式代理、真实身份/额度、原生管理API与
私有持久化已验收；更早的真实DeepSeek非流/流、额度参与实际选号、逐组cap1及释放验收通过。
候选 `0.1.2-local` 尚未安装，本地请求汇总测试不等于现用页面验收。
本轮未改其他业务路由，也没有重启服务。旧cc-deepseek直连不受新池cap控制，使用新池应选
`commandcode/` 命名空间中的模型；不是所有目录模型或高容量生产负载都已测试。
旧版与当前源码、模拟检查和现场运行证据分层见 [VALIDATION.md](VALIDATION.md)。

## 旧客户端模型名兼容

在插件配置节点中设置客户端名称到规范上游 ID 的映射：

```yaml
model-aliases:
  cc-deepseek-v4.1-flash: deepseek/deepseek-v4.1-flash
  cc-deepseek-v4.1-flash-fast: deepseek/deepseek-v4.1-flash-fast
  deepseek/deepseek-v4.1-flash: deepseek/deepseek-v4.1-flash
  deepseek/deepseek-v4.1-flash-fast: deepseek/deepseek-v4.1-flash-fast
```

- 原 `commandcode/deepseek/deepseek-v4.1-flash[-fast]` 名称继续发布；旧别名和裸上游名
  也由 `commandcode-pool` 发布，实际请求 body 使用规范上游 ID。
- 所有名称共享现有凭据、真实账号 Group、额度选号和执行前硬 cap；别名不增加并发名额。
- 目标必须在当前可路由上游目录中。缺失目标、别名链及跨模型名称冲突不会被发布；诊断
  会保留原因，且不会覆盖其他模型。未配置映射时维持原行为。
- 完整迁入必须移除旧 `openai-compatibility` CommandCode 直连条目；只增加别名而继续保留
  旧 provider 会留下绕过账号池的候选。这是受控生产变更，不由构建脚本自动执行。
- 不使用 CPA 的 OAuth alias 代替此映射；当前 API-key auth 不走该别名通道。响应转换
  保持现有上游模型回显行为，不额外改写业务响应。

## 构建与测试

需要稳定版 **Go 1.26.7+**（`go.mod` 的实际最低版本）、CGO、gcc、Bash、
Linux coreutils 和 tar。脚本使用 `GOTOOLCHAIN=local`，不自动下载或切换 Go 工具链；
Go 构建自身可能根据 `go.mod` / `go.sum` 下载模块依赖，不下载 CPA/CPAMP 发行包。
脚本通过 `go -C` 固定模块并使用 `-buildvcs=false`，避免嵌套 worktree 自动写入错误的
仓库 revision。库中的版本号不是源码溯源凭证；交付时应另外核对实际工作区 revision、
源码及依赖输入摘要和产物 SHA256。

从本目录先执行测试：

```bash
bash scripts/test.sh
```

再生成默认交付包：

```bash
bash scripts/build.sh
```

也可改为指定新的输出目录：

```bash
bash scripts/build.sh /absolute/path/to/local-build-output
```

`GO_BIN` 可指定已有稳定工具链；从本独立插件仓根目录运行，替换以下路径为实际 Go：

```bash
env GO_BIN=/absolute/path/to/go bash scripts/test.sh
```

构建时单独执行：

```bash
env GO_BIN=/absolute/path/to/go bash scripts/build.sh
```

已有依赖缓存可通过 `GOMODCACHE`、`GOCACHE` 配置；不要沿用旧 doc_mana 嵌入目录的路径。

不需要 `source` 任何 `.env` 或生产凭证。`test.sh` 依次执行有界工具离线回归、
`go test ./...` 与 `go test -race ./internal/...`，遇到失败立即停止；它们测试的是代码与
公开测试夹具，不是启动已安装的 CPA/CPAMP。离线工具仅需 Python 3 标准库：

```bash
bash scripts/regression.sh
```

`PLUGIN_ID` 默认 `commandcode-pool`，只允许 `commandcode-pool`、`commandcode-pool-next`
或 `commandcode-pool-update`。默认构建保持原插件 ID；可用另一 ID 构建隔离候选：

```bash
env PLUGIN_ID=commandcode-pool-next \
  bash scripts/build.sh
```

`PLUGIN_ID` 决定插件/包文件命名及原生 URL。next/update full 使用各自独立管理地址，
并提供原 `commandcode-pool` 管理 API 的兼容别名；guard-only 不提供这些别名，避免与
仍在运行的旧池冲突。本轮 0.1.7 已实际读回为 `commandcode-pool-next` full；guard-only
暂存、hold 移交及无重启接管详见 [VALIDATION.md](VALIDATION.md)。本次生产验收不以
0.1.5 的历史成功代替。本脚本不是自动部署机制，
后续更新仍须核对实际活动 ID、库身份及保护状态。
ProviderID 和客户端模型命名空间仍分别为 `commandcode-pool` /
`commandcode`，不改变客户端模型别名。构建只生成本地产物，不安装或部署。

`PLUGIN_ROLE` 默认 `full`；另可用 `PLUGIN_ID=commandcode-pool PLUGIN_ROLE=view` 构建
仅页面兼容库。它只提供原 HostID 的两页静态资源，页面 API 指向 next，不提供 Scheduler、
Usage、账号池或执行能力，也不会接管第二份 Codex 状态。其产物名为
`commandcode-pool-v0.1.1-compatview.so`，配置 `store.version` 应为 `0.1.1-compatview`；
页面库的运行时 metadata.version 仍为 `0.1.1-view`，与加载器文件版本不是同一字段。
不能改用 `-v0.1.1-view.so`：加载器按最后一个 `-v` 分隔符解析，该名字会被识别成错误 ID。
页面兼容库仅应在旧业务插件已安全移交并卸载后加载，不能覆盖正在使用的旧库。

默认输出到 `dist/`，文件名前缀按 `PLUGIN_ID`，候选版本为 `0.1.8-local`：

- `<PLUGIN_ID>.so` 与 CGO 生成的 `<PLUGIN_ID>.h`；
- `LICENSE`、`NOTICE.md`、`THIRD_PARTY_NOTICES.md`、`UNIFIED_SCHEDULER.md`、
  `README.md`、`VALIDATION.md`、`REGRESSION.md`、`TRACE.md`、`config.example.yaml`；
- `SHA256SUMS`，覆盖以上文件；
- `<PLUGIN_ID>_0.1.8-local_linux_amd64.tar.gz`，含上述文件及校验清单。

输出目录的归属标记只用于安全重建，不是宿主 manifest。脚本不执行 `rm`，保留
无关文件；首次构建遇到同名既有文件会拒绝，只有本脚本标记的自有产物可重建覆盖。
输出不能指向源码目录。校验时在产物目录执行 `sha256sum -c SHA256SUMS`。

## 配置与后续安装准备

[config.example.yaml](config.example.yaml) 是 CPA v8 的**隔离本地 mock 示例**：
CPA 仅绑定 `127.0.0.1:18633`，插件上游仅指向 `127.0.0.1:18635`；目录和密钥均为
必须替换的测试占位符。`allow-http: true` 仅供 loopback mock，不是生产推荐配置。
只有另行获准使用真实服务时才配置真实 HTTPS 地址和凭证。

后续获准加载时，插件文件名须与选定的 `PLUGIN_ID` 一致。CPA v8.0.20 按顺序扫描
`<plugins.dir>/linux/amd64/` 和 `<plugins.dir>/`（不递归扫描其他目录）；单个平台本地包
可放在根目录。启用默认构建时，配置键应为 `commandcode-pool`；next 隔离构建使用
`commandcode-pool-next`。必须同时开启 `plugins.enabled` 和所选插件 ID 的 `enabled`。
切换或并行加载前仍须遵守 [UNIFIED_SCHEDULER.md](UNIFIED_SCHEDULER.md) 的唯一 Scheduler
门槛，构建 next 不代表宿主已安装或获准切换。

本轮隔离实测补记一条**版本命名配对**合同（CPA 8.0.23 共享内核）：加载器把库文件名
解析为 `<插件ID>-v<版本>.so`，并与配置 `plugins.configs.<插件ID>.store.version` 精确比较。
两种可用配置必须成对选择：

- 配置里**不写**（或留空）`store.version`：库文件可命名为 `<插件ID>.so`，按 ID 匹配即加载；
  这是现有 0.1.x 生产库的部署方式。
- 配置里**写了** `store.version: X`：库文件必须命名为 `<插件ID>-vX.so`，否则版本不匹配，
  宿主静默跳过该文件。表现为管理接口 `registered=false`、`effective_enabled=false`、
  `path=""`，且加载日志无任何报错——这是本轮首次原生验收命中并已定位的失败模式。

`build.sh` 产出的 `<PLUGIN_ID>.so` 属于第一种命名；若部署端要写 `store.version`，请把库文件
重命名为对应 `-v<版本>` 形式再放入 `plugins.dir`，两边版本字面量必须完全一致。

宿主直接发现动态库，不需要额外 manifest。若后续添加 `SOURCE_METADATA.json`，
它只能是来源证明，不能充当 CPA 加载 manifest。

### 存储与重配

- `plugins.configs.commandcode-pool.pool.auth-dir` 必须与 CPA 的
  **`oauth.auth-dir` 指向同一个实际目录**。插件会生成 CPA 可消费的本插件 auth 文件；
  不自动扫描或迁移无关凭证。建议使用明确的绝对路径，插件不会代替用户校验宿主路径是否匹配。
- 默认账号状态位于 `<auth-dir>/.commandcode-pool/state.json`；状态、auth 和锁文件
  权限为 **0600**，对应目录为 **0700**。该文件存储上游密钥，**不是加密保险库**；
  不得提交、公开、打包进交付物或粘贴到日志。备份也属于敏感数据操作。
- `pool.auth-dir`、`pool.state-path`、`pool.max-concurrency`（默认 cap）及
  `pool.quota-max-age` 不能在线更换，需要获准重启 CPA。
- 单个账号的名称、启用状态和并发 cap 可通过管理页面/API 热修改；同一 group 的
  cap 是共享限制，更新会保持组内一致。降低 cap 不会提前释放已经在途的请求。
- 旧组有任何 Key 在途时不能迁组；已知厂商身份有 owner 时不能替换身份。
  `name`、`group_id` 和厂商身份最长 256 字节，不能包含任何已知或已删除凭据原文；
  新 Key 也不能与已有控制元数据重叠。旧状态若已含这种秘密元数据，启动拒绝加载，
  不自动重命名真实账号分组。
- auth、state 和受控 staging 通过 journal 恢复；只处理本插件拥有的身份，不清理其他插件凭据。
  模型目录成功刷新后更新非秘密 `catalog_revision`，触发宿主 watcher 重发布模型。
  这条链路已离线测试，并在官方 CPA v8.0.20 上实跑：空池导入后无需重启即可出现模型并推理。

## 真实账号分组与额度准入

1. 优先通过账号池页面添加/导入 Key，由用户填写 `group_id`。
   **同一个真实账号的多个 Key 必须放在同一 group**；不要根据 Key 数量增加账号并发。
   插件不会把不同 Key 自动推断为不同真实账号，group 归属必须由用户确认。
2. 默认共享并发 cap 为 **2**，不是每个 Key 各 2。旧式 YAML `api-keys` 没有真实主体
   信息，全部导入到保守的 `configured-accounts` group，共享同一个 cap。
3. 额度来源是与 `base-url` 同源的 `/alpha/billing/credits`、
   `/alpha/billing/subscriptions` 和 `/alpha/whoami?limits=1`，请求使用 `User-Agent: cli`。
   插件配置的 `proxy-url` 只控制插件发出的上游请求：省略/空值时继承进程代理环境；
   显式设为 `direct` 或 `none` 时绕过环境代理。CPA 自身 `requests.proxy-url` 是独立配置。
   HTTP 请求路径和余额不是客户端估算或填写；不能以 mock 值证明真实账号额度接口已验收。
4. 额度缺失、刷新失败、超过 `pool.quota-max-age`（默认 **5m**）、已耗尽或账号禁用时，
   按 **fail-closed** 拒绝准入，不使用过期额度继续放行。后台刷新默认 **1m**。
   模型目录的 stale-while-unavailable 不改变额度的 fail-closed 规则。
5. 在可准入候选中按 headroom、剩余额度等信息选择，并在实际执行前原子 Acquire；
   调度推荐不是已经占到槽位，真正的上游执行不得绕过原子准入。

批量导入请求结构为：

```json
{
  "accounts": [
    {
      "name": "本地测试账号 Key A",
      "group_id": "one-confirmed-test-account",
      "api_key": "REPLACE_WITH_LOCAL_MOCK_KEY_A",
      "max_concurrency": 2,
      "enabled": true
    },
    {
      "name": "同一测试账号 Key B",
      "group_id": "one-confirmed-test-account",
      "api_key": "REPLACE_WITH_LOCAL_MOCK_KEY_B",
      "max_concurrency": 2,
      "enabled": true
    }
  ]
}
```

批量导入按条处理，后续条目失败时可能返回 `partial_success`，须检查响应，
不要把导入视为整批原子事务。

删除账号会留下**墓碑**，清除其密钥并停止新准入；已经在途的执行保留占位直到完成。
被删 Key **不能重新添加相同 Key**，重启或重新配置不会绕过墓碑。临时停用应使用
`enabled=false`，不要删除再加回。Key 轮换使用一个**新 Key**；删除后建议从 legacy
`api-keys` 中移除对应旧 Key，避免配置继续保留其明文。注册会跳过已存在的墓碑，
不会因这个旧 Key 自动复活账号或导致注册失败。

## CPAMP 原生页面与鉴权

在 CPAMP 完成初始 setup、注册 CPA 上游连接后：

- 插件管理：`/management.html#/plugins`；
- 原生菜单：在侧栏选择“CommandCode 账号池”或“Codex 429 保护”；CPAMP 按资源路径排序。
  0.1.7 本轮已上线使用 `commandcode-pool-next`，加载后的入口为
  `/management.html#/plugin-pages/commandcode-pool-next/1` 和
  `/management.html#/plugin-pages/commandcode-pool-next/0`；0.1.6 的历史部署 ID 为
  `commandcode-pool-update`，实际活动入口以插件管理页面读回为准；
- iframe 资源：`/v0/resource/plugins/<PLUGIN_ID>/pool` 或 `/codex`；
- 管理 API：`/v0/management/plugins/<PLUGIN_ID>/...`。

`<PLUGIN_ID>` 须替换为实际加载的插件 ID；默认构建为 `commandcode-pool`，0.1.7 本轮部署为
`commandcode-pool-next`。

账号池 iframe 第一次需要手动输入当前宿主的管理密钥：直接通过 CPA 打开时使用 CPA
Management Key；通过真正的 CPAMP 打开时使用 **CPAMP 管理员密钥**，由 CPAMP 服务端
验证并换成保存的 CPA Key 代理上游。两类密钥不能混淆。

页面只把手动输入保存在本页 **JavaScript 内存**，不会放进 URL、localStorage 或
sessionStorage；刷新页面需要重新输入。宿主没有自动 token 注入、postMessage token
或 SDK bridge，不应声称 iframe 已自动继承登录态。不要用同源假网关页面替代原版
CPAMP 鉴权验收。

账号接口为 `GET/POST accounts`、`POST accounts/import`、`POST accounts/delete`；额度为
`POST quota/refresh`，状态为 `GET status`。默认 `GET events?after=<cursor>` 保留全部原始
诊断事件；`GET events?scope=requests&after=<cursor>` 读取独立请求事件缓存，均在上述
管理 API 前缀下，返回形状仍为 `events` 与 `next_cursor`。未知、空或重复 scope 返回400。
页面按 request_id 一请求一行，只显示实际账号池请求；后台通知不显示，候选评分和
过程记录可展开。0.1.8 兼容追加可选 `trace_id`：它是宿主提供的入站追踪 UUID，
用于与响应 `X-CPA-TRACE-ID` 中的 UUID 或 usage trace 精确连接；不把它当作宿主
生命周期 `request_id`。旧事件不带该字段仍可显示，详情会明确提示不能直接关联。
只选号不会显示成功，成功指上游正常结算，不等同客户端已收到完整响应。
页面不返回可恢复的 Key。

Codex hold 管理提供 `GET codex/bans`、`POST codex/unban`、`POST codex/unban-all`，
以及 `POST codex/bans/import`，也位于同一插件管理 API 前缀下。导入 body 为
`{"bans":[{"auth_id":"<host auth id>","reset_at":"<RFC3339>"}]}`；`reset_at`
也可用整数 Unix 秒。可转发旧 bans 响应，导入只使用 `auth_id` 和 `reset_at`：任何一项
非法会拒绝整批，不部分写入；已过期项忽略；重复 auth 只延长、不缩短现有 hold。它只修改
当前进程内存，不写持久化状态，也不会自动 unban。管理结果中的 `activation_error` 经脱敏。

## 生命周期、取消与事件

实际执行使用插件自行持有的 **HTTP/1.1 transport**，由 attempt owner 跟踪上游 I/O。
取消、客户端断连、宿主 `request.complete` 或超时只是发出取消/关闭请求，
**不能直接证明上游已经结束，也不能直接退还并发槽位**。
只有 owner 确认上游 I/O 清理完成后才 Settle，一次 attempt 最多释放一次。

若清理失败或无法确认，保留占位并 quarantine 对应 group，拒绝继续准入；
**不按 TTL 强制回收**，以免重叠真实上游请求。需要确认旧上游工作确已结束后，
才考虑经授权的进程恢复，不能靠盲目重启清空计数绕过隔离。

原始诊断事件和真实请求过程事件各自使用最多 **500** 条的内存环，后台通知不会挤走
请求过程；重启会丢失，不能作为持久审计或完整历史账本。两个视图使用同一个递增序号，
游标落后于容量仍可能失去旧过程，页面会标示历史不完整。本页最多保留500个请求汇总、
每请求最多50条技术记录，不意味着后端始终保存500个完整请求。
