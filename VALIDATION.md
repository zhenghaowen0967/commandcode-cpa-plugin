# 本地与官方模拟联调验证记录（2026-10-08）

## 生产切换完成：0.1.2-local（2026-10-09 08:19 UTC，最新）

验证全部通过后按用户明确授权完成生产热切换，全程无重启、无 unban、无取消在途流：

- 切换前隔离原生验收（同生产二进制 a4eaa1c1，公开合成账号）：happy path / after_pause /
  after_delete 三周期全过，含 guard-only 独立管理地址不被旧 alias 抢占、真实 mock 429
  形成原生 Usage 回执、hold maxmerge 覆盖不翻倍、暂停准入不取消在途流自然 Settle、
  删除后 recover 前向续接不重复 hold import、最终单组两菜单/两账号原 cap。
- 生产切换：guard80 暂存 → 升110 → 暂停准入（只改 enabled）→ 自然排空 → DELETE 旧 next →
  同一 Manager 激活 full → 恢复账号；`hold_imports=1`（当日 hold=0 为外部重启后空导入）、
  其他配置逐字节保护、PID 1948812 全程不变。
- 切后读回：唯一 `commandcode-pool-update` 0.1.2 + 两菜单；两真实账号 enabled/原 Group/cap10；
  pool_active；CPAMP 一组菜单。有界真实模型验证 1 次 POST 200（finish=stop），请求事件视图
  恰好一行三事件，噪声隔离。
- 切换链路中发现并修复：settle 同义词 UI 误判（独立审查）、`_production_pid` ss 解析严重缺陷
  （独立审查，正则修复+单测）、imported_bans 身份泄漏（摘要化）、夹具 failpoint 接线。
- 证据：`.scratch/commandcode-dev/request-summary-native-handoff-20261009.json`、
  `request-summary-production-cutover-20261009.json`、`request-summary-preflight-final-20261009.json`。
- 边界：未做生产浏览器真实凭据界面验证（不把真实管理 Key 送浏览器工具）、大容量压测、
  多实例共享 cap。生产基线 2026-10-08 晚被外部更换（二进制 a4eaa1c1、非 systemd 进程、
  hold 已随重启清零）；隔离验证基于同版新二进制。

## 当前源码候选：0.1.2-local（请求汇总，本地通过、已生产安装）

用户授权“只显示真实请求，一条请求一行”。本次新增独立请求事件缓存，背景额度刷新、
账号编辑及普通完成通知不再挤占请求缓存；`GET /events?scope=requests` 返回实际选号、
准入、拒绝、结算和隔离事件，默认 raw 接口保持兼容，未知/空/重复 scope 返回400。
页面按 request_id 汇总中文结果，候选评分与原始过程展开；只选号不表示成功，重试不拆行，
取消和清理未确认分别显示，截断过程明确标为历史信息不完整。
本页最多500个请求、每请求最多50条技术记录；后端是500条请求过程记录，不保证保留500个完整请求。

- 普通与 race：`./internal/pool ./internal/plugin ./resources` 三个包全部通过。
- vet：上述三个包退出0；本地 c-shared 构建及0.1.2候选包生成成功。
- 回归覆盖：600条后台噪声不能挤走真实请求；原始事件/terminal晚RPC保护不变；
  ring有界、游标及脱敏/deep copy；页面分组、乱序、去重、重试、拒绝、取消、quarantine、
  截断、详情上限、uint64精度、展开状态、清空与断开清数据。
- 未完成：浏览器桌面/mobile与同 binary 隔离加载。两次 preview_start 被安全分类服务
  暂不可用阻断，未通过其他路径启动服务器。独立Sonnet审查因模型网关返回
  `codex_quota_hold` 中断，未取得完成报告，不能记为审查通过。
- 新版生产写入与真实模型请求均为0。现用仍为next497/0.1.1；最后已验两账号enabled、
  cap10/10且只有一组菜单。更新操作器只留下未验收草稿，没有运行stage/finish。

证据：当前工作区 `.scratch/commandcode-dev/request-summary-tests.txt`、
`request-summary-race.txt`、`request-summary-vet.txt`、`request-summary-build.txt`。
历史0.1.1实际运行与真实验收不继承为0.1.2验收。

## 最新授权验证增量（2026-10-08 14:40 UTC之后）

用户明确要求继续验证，通过后上线正式CPA/CPAMP。14:40UTC只读前置核验PASS：
CPA PID1329426/000204二进制与保护配置一致，next0.1.1完整模式，两账号enabled/cap10，
CPAMP18317一组菜单；当前有效Codex hold数为1，不能用历史hold0假设迁移。

主实读宿主management.go:46–80确认冲突管理地址由更高优先级插件抢先注册，
原draft使用低优先级original会被next已有original alias抢占。新增允许的构建HostID
`commandcode-pool-update`，guard-only只注册自身地址，full保原管理别名兼容；
next/update地址及原生页面回归通过。新增暂停准入回归证明Upsert enabled=false
不取消active owner，后续Acquire拒绝；owner正常Settle归零后恢复原enabled/group/cap10。
第一次新增测试误用Settle无返回值导致编译失败，已修测试签名并保留原日志；三个受影响包
race最终通过，不能描述为首轮全绿。

浏览器启动本轮又两次被安全分类服务不可用阻断；原两Sonnet代理恢复仍被codex_quota_hold
中断，针对1条生产hold连续性/路由冲突派的Opus审查同样未能执行。
以上不能视为独立审查通过或同binary原生热切换通过。新版仍生产写入0、未安装；
运维草稿production拒绝门保留，不自动清hold、换用户网关或重启绕过。

证据：`.scratch/commandcode-dev/request-summary-authorized-preflight.json`、
`request-summary-authorized-race.txt`（首FAIL）、`request-summary-authorized-race-fixed.txt`。

## 已运行版本：0.1.1-local（历史真实测试；当前仅一组入口）

旧pure-view兼容入口已按用户要求删除，最后已验当前cap为10/10且两个账号保持启用。
下面cap1和双入口属于各自标注的历史验收阶段，不是当前配置指令。


最新真人授权“不要禁用啊你去测试然后没问题交付给我”后，两个已有真实账号均启用，真实Group不变、各cap=1，最终eligible/inflight0。新池模型 `commandcode/deepseek/deepseek-v4.1-flash` 完成HTTP200非流与流式正文验证：两空闲候选按真实headroom/credits评分选账号2；其inflight1时账号1实际准入，两组同时inflight1；第三请求HTTP500、两候选均concurrency_limit、无新增acquire。事件配对证明各组峰值1、正常完成和取消均由owner settle后归零。6次客户端模型POST中5次实际准入，1次满cap拒绝。首次长流512 token全用于reasoning、正文0的整阶段FAIL原样保留；后续只补账号1短流，200/正文2字符/stop/[DONE]/settle通过，不把原FAIL改成PASS。结果 `.scratch/commandcode-dev/production-real-acceptance-results.json` 的最终阶段PASS，当前私有持久化及无关配置保护通过，CPA PID1329426保持。目录最终采样87个模型，目录会变化，不表示所有模型已测试。

本次不改Go库、不重新部署、不unban或重启，不迁旧cc-deepseek直连。下面两禁用账号、真实模型0等均是前次上线阶段的历史证据；本次范围为单模型Chat非流/流、小量逐组cap及释放，不冒称高容量、跨实例或厂商后台取消ACK。

当前新版代码已加入统一 `/alpha/` 额度请求 User-Agent、插件代理配置、严格 Codex bans 导入、guard-only 单实例接管，以及旧 API/页面兼容。请求修复、同一 Guard 激活 full 和兼容角色分工已完成局部测试、独立审查与实际隔离加载；不能把这些检查合并成生产切换或真实业务验收。

| 项目 | 当前状态 |
| --- | --- |
| 新版局部检查 | UA、Proxy、bans import 相关局部测试通过；最新源码候选普通/race 全量测试通过。Opus 增量复核 guard-only full 后失败 reconfigure 修复，无新增否决；不等于生产接管验收。 |
| 构建身份 | 单实例接管库 `f0a6984…` 已完成构建及原生接管检查；加入兼容别名后的 next 库为 `49766793…`。纯页面 view 库为 `00c9aed6…`，以 `commandcode-pool-v0.1.1-compatview.so` 成功加载；文件版本 `0.1.1-compatview` 与其 runtime metadata `0.1.1-view` 分开验证。 |
| SDK binary | 当前隔离使用的 CPA SDK/toolsearch 二进制版本为 `8.0.20`、SHA256 `000204…`，不同于官方原版 `6efbd386…`；证据仅适用于当前 binary 隔离验证，不声称对应源码 patch 已逐项实核。 |
| 原生及部署 | 单实例接管候选 `f0a6984…` 在隔离环境原生 9 项 handoff 检查通过，证据 `.scratch/commandcode-dev/request-compat-native-handoff-results.json`：GlobalUsage mock marker 验证新 receipt 与历史 fallback/hold merge；guard-only 不持 pool 锁，锁冲突时仍保持 guard 注册；卸载旧插件后同一 Manager 激活 full，保留 accounts=2、disabled 与 usage counter=1；full 状态下非法切回 guard-only 仍保持 full 且 counter=2；全 hold 返回500且 mock calls 前后均为4；新 full quota=75/headroom=0.75。三次 fixture 错误尝试保留在 JSON 中，不描述为首轮一次通过。该隔离阶段生产 writes=0；未做真实模型操作。真实禁用账号导入与现场接管证据见后文。 |
| 真实服务只读 GET | 2026-10-08 10:06 UTC，两条原始 Key 的 `whoami`、`credits`、`subscriptions` 各返回200；只读探测，不导入插件、不调用模型。 |

真实只读 GET 解析结果：两条 Key 对应 payload 的 `group` count 均为2、`identity_kind=user`；余额分别为 `7.2268866748`、`43.1058383575`；5小时窗口 used/cap 为 `0/14`，两周窗口分别为 `28.9673538019/35`、`26.8941616425/35`。Go 字段解析与观测到的真实响应合同吻合。原始证据 `.scratch/commandcode-dev/request-compat-real-get-probe.json` 不含原始 identity 或 Key。该结果只覆盖真实服务只读 GET 和响应字段解析；不表示真实账号已经导入池、真实模型已验或生产已切换。上述 9 项是隔离 native handoff 验证，不是“现用接管完成”或生产接管通过。

2026-10-08 11:30 UTC，最新 next `49766793…` 与纯页面 view `00c9aed6…` 在同现用 binary 的隔离 `18645` 注册生效。原/新两套页面均返回200；原管理 API 导入的公开 disabled/cap1 账号与 next 列表一致，原额度刷新读回75，原 bans 导入与 next 使用同一 Guard。匿名管理 API 返回401；静态 HTML 本身按宿主资源合同匿名200，不含账号状态或管理凭据。首轮 `-v0.1.1-view.so` 被错误解析为另一 ID 的失败保留，修正为 `-v0.1.1-compatview.so` 后通过；还保留了把静态页面误当鉴权 API 的错误断言。证据 `.scratch/commandcode-dev/request-compat-url-results.json`，只用公开合成凭据，生产写入与真实模型请求均为0。

用户明确要求直接上线后，现用 `49766793…` 完成同Manager完整接管（优先级100），原HostID已换为 `00c9aed6…` 纯页面兼容入口（优先级0）。旧完整库已卸载，受限备份保留，CPA PID1329426未重启。两真实账号通过CPAMP18317导入、身份与单ID额度刷新全200，Group2/disabled/cap1/inflight0；API与磁盘Key/身份/分组/cap一致，state/auth0600、目录0700。原/新四资源200、两账号API一致，匿名管理401，无关配置语义保护通过。Guard计数最终采样1050，无清hold或重复import。本任务真实模型0。

首次授权finish在旧库DELETE之后因SDK补60个空ModelMatcher字段触发保护门；只在比较副本中删除新增空字段、将runtime空集合归一回nil后，两个投影均精确等原保护摘要，证明没有其他业务变化。保留原配置、归档原私有baseline，从删除后的位置恢复开池，没有重新移交Guard。`request-compat-authorized-finish-result.json`、`request-compat-authorized-resume-result.json`、`request-compat-production-acceptance-results.json`保留失败与最终证据。新增恢复/view安装/验收脚本经Sonnet独立增量审查，无确认缺陷。

此前Guard-only80/旧90、Usage1/31与缺窗口拒绝属于已完成的前一阶段；下方“现用未安装”等历史采样亦只描述标注时间，不覆盖最新现场结果。

## 历史候选：0.1.0-local / f34842e7…（结果不继承至 0.1.1）

旧版统一调度库 SHA256 为 `f34842e74d5b02a249a2ef0e75af84ff7202c5a6dc44618a73cfc47b5290c247`。本地唯一 Scheduler 同时处理 CommandCode 余额选号和 Codex 429 窗口保护，不改 CPA 核心。该旧库曾通过官方 CPA `8.0.20` / CPAMP `1.14.4` 隔离模拟与原生页面验收；这些证据仅属于旧库。旧版当时未用真实账号/真实服务验收；2026-10-08 的真实只读 GET 是之后独立完成的有限接口探测，不等同于旧版额度准入验收或新版生产部署。真实模型、生产容量与切流仍未验收。更早的 `49fc0d92…` 和权限候选 `95e9c1c9…` 仅保留历史证据。

### 旧版历史验证结果（非 0.1.1 证据）

| 项目 | 历史结果（对象为 f348 / 0.1.0-local） |
| --- | --- |
| 普通 Go | `go test ./... -count=1 -timeout=120s`：13 个测试 package 全部通过；plugin 15.244s、pool 0.417s、resources 0.667s。 |
| race | `go test -race ./internal/... ./resources -count=1 -timeout=180s`：13 个 package 全部通过；plugin 17.966s、pool 1.645s、resources 1.889s。 |
| 静态与构建 | `go vet ./...` 退出0；Go1.26.8 c-shared 构建通过，ABI1/schema6。原生包9个 payload 加校验清单，共10成员，含完整第三方 MIT 声明。 |
| 独立审查 | Sonnet 增量审查后无确认项；复核混合路由与实际 Acquire 的区别。 |
| 官方加载 | 当时隔离 CPA 正常重载，加载库与 f348 摘要一致，既有模型/账号恢复。 |
| CommandCode 模拟回归 | 假上游额度翻转选 A/B；耗尽及非法额度拒绝且上游新增调用0；同账号双 Key 共享 cap1；流结束及断连后 I/O 与插件在途归零；私有头未外传。Chat/Responses/Messages 非流及 Chat 流通过。 |
| Codex 模拟链路 | 假配置失败429经 `usage.handle` 记录5h hold；健康账号回退、全 hold 拒绝、手动解禁及自然到期通过；缺 reset 回退五小时通过。 |
| 原版 CPAMP 页面 | 同一插件菜单提供账号池与 Codex 保护；Codex 页经隔离管理代理验证，鉴权错误/缺失拒绝；暂停、解禁、断开和401清会话通过。 |
| 主题与手机 | 原版主题浅/深色与375手机布局通过；整页无横向溢出，仅局部表格横滚。 |
| 仓库检查 | 当时 `check.sh changed/docs` 退出1：文档链接1018通过、0失败、712跳过；指导合同5通过/2失败，因既有 AGENTS.md 文件缺失。未修无关基线、未豁免。 |

旧版证据位于当时工作区 `.scratch/commandcode-dev/`：`unified-final-test/race/vet.txt`、`unified-build.txt`、`official-unified-business-results.json`、`official-unified-concurrency-results.json`、`official-unified-codex-results.json`、`official-unified-theme-results.json`、`unified-changed/docs.txt`。旧版模拟证据包不含 auth/state/运行配置/真实凭据/数据库/官方二进制或工具链。

### 旧版模拟边界与失败闭环

- 旧版 `commandcode/` 独立路由保留额度选号；无 Codex hold 的跨 provider 混合路由保留 CPA 内建策略，不保证余额择优。实际 CommandCode I/O 一律经过 `Pool.Acquire`，混路由不能绕准入。
- Codex 状态仅在当时进程内存中，重配置保留、重启丢失。手动 unban 只清本插件，不清 CPA 内建冷却或 CPAMP auth 文件禁用。
- 首轮 guard 测试曾修复缺 `math` import、错误重试时刻断言及极值 Unix reset JSON 失败；这些属于旧版结果。
- 假 OAuth 文件的 `base_url` 不被官方 Codex 执行器用于路由；旧版改用官方原生 API-key 配置及 loopback 模拟上游，没有修改 CPA 核心或调用厂商服务。
- 初轮官方脚本曾遇到宿主 `model_cooldown`，保留失败证据并等待冷却后复测通过；未清宿主状态或放宽全 hold 拒绝断言。
- 页面“全部解禁”确认曾以临时调试替身模拟并恢复；实际隔离 CPAMP 管理 API 返回200。浅/深色、手机布局、预期401行为均为旧版证据。
- 旧版只读终采 `2026-10-08T06:30:40Z` 记录 PID1050902 active/running、CPA摘要官方8.0.20、`pool_config_present=false`、`pool_library_present=false`；这是有时间戳的旧采样，不覆盖当前已知旧 f348 安装信息，也不证明新版生产部署状态。

## 以下为历史账号池验收与接入调查（非当前统一库证据）

## 现用接入调查与后续源码修复

2026-10-08 核查现用 CPA `8317`：实际二进制与上述官方 `8.0.20` 摘要相同；CPAMP `18317`
镜像标签为 `1.14.4`，尚未核容器内二进制完整摘要。现用已启用 `codex-429-autoban 0.2.2`
(priority100)、`cpa-quota-estimator`、`quota-pacer`；另有 CommandCode 两 Key 的旧直连。

**现用未安装：** CPA `8.0.20` 的 `schedulerRecord()` 仅选择一个 Scheduler。
现用 Codex 插件与官方 `v0.2.2` 发布库逐字节一致，源码声明 Scheduler 且对非 Codex 候选也
返回 Handled/内建轮询委托。因此本插件低优先级时无法执行余额选号；提高优先级会取代原
Codex 自动禁用调度。当时尚未解决共存；当前已在统一插件内实现两套规则，仍未改现用
优先级、停旧插件或迁移 Key。

调查还发现 `pool.New` 强制 chmod 共享 auth 根的副作用；已在后续源码中修复：已有根保留
权限和所有权，新建根 `0700`；StatePath 直接位于 auth 根时也不收紧共享目录，私有子目录
仍 `0700`，自有文件/锁/journal 仍 `0600`，拒绝根 symlink/非目录。该候选的全部普通及
race 测试通过，vet 退出0；证据为 `current-compatibility-final-test/race/vet.txt`。

**对象区分：** 下文官方模拟/浏览器证据均对应 `49fc0d92…` 历史验收库。后续权限修复候选
不继承“已经加载/已经原版复验”的结论；旧 dist 归档保留不覆盖，不用于现用安装。
本会话没有修改或重启现用服务，没有调用真实模型。核查期间现用 PID/配置摘要发生了外部
变化，最新采样2026-10-08T05:13:05Z PID1050902、库仍8.0.20；不能冒称旧基线全程不变。

## 结论与边界

`commandcode-pool 0.1.0-local` 已完成本地实现、独立审查和**官方 CPA/CPAMP 的本轮模拟联调验收**。用户明确批准停止并重启隔离 CPA 后，主题修复库已通过正常 `preview_stop/start` 重载；原版 iframe 浅色、深色、手机布局与关键业务复验通过，本轮模拟范围无剩余阻断。

- CPA：官方 `v8.0.20`，提交 `0f96f568e4dbf6f84ad7399a74b78344c5eac7e6`；SDK ABI **1**、RPC schema **6**。
- CPAMP：官方 `v1.14.4`，提交 `166262365d506157b99968105c890f480cc7c07c`。
- MIT 基座：`mczhoucn/commandcode-go-cliproxyapi`，提交 `ea84cdd799564f644c6f9f39c7dc356d0f013526`。
- 官方 CPA 二进制 SHA256：`6efbd3861bde4f166d5536c40e6307541ae9bd954a960c9ce2636f0b8006bd37`。
- 官方 CPAMP 二进制 SHA256：`0588b7f28010825cde26511dfecafbb4a70f6c26d35f4cc4998b2f90434b3a98`。
- 官方 CPAMP 管理 HTML SHA256：`98476e174f8f74001c2d1b576091106a49f00b476843ed2a54d38a9f9f04e4ae`。
- 用户实际部署版本未知。仅支持 **Linux amd64、glibc ≥2.34、单个 CPA 进程、本地文件存储**；不支持 Home 或跨实例共享 cap。
- 未提交、推送、发布、部署、重启生产服务、读取真实凭据或调用真实模型。

## 验证对象与源码身份

| 对象 | SHA256 | 验证范围 |
| --- | --- | --- |
| 历史初版实跑库 | `508920df141d4370b77c081db1a8ebad5e6b1696f1abeb8f8878fc9fcec7d0ad` | 官方加载、空池导入注册、业务与原生页面管理；发现主题冲突。仅作为历史证据。 |
| 当前交付和实际加载库 | `49fc0d92137a6c3d4853b5d194d15a383f2e24c661f69eebe4f13857ee305889` | 普通测试/race/vet/构建通过；官方重载后模型、额度翻转、共享 cap、流与断连、额度失效拒绝、协议、代理鉴权、原版浅/深色及手机均复验通过。 |

CSS 使用 CPAMP 的 `--app-bg`、`--app-surface`、`--app-surface-muted`、`--text-primary` 等既有变量，独立打开保留深色 fallback。未修改调度、存储、鉴权或 executor，也未修改官方 CPAMP 源码。当前库通过 CPA 重启后实际加载，CPAMP 返回的 `/pool` 资源与当前源码 HTML **逐字节一致**，SHA256 为 `f8022e753d0ef2681a00c6bc576bf1fbf70a346ea6b6aa39bf62e1ed86f59958`。

工作区为 `tweet-overview-bfc9d0`，分支 `haowen/tweet-overview-bfc9d0`，基准 HEAD 为 `ba506e9e665addd34977e9613fbc575519098102`。插件是未提交增量，**HEAD 不单独代表插件源码**。源码归档的 `SOURCE_SHA256SUMS` 排除自身、构建产物、auth/state、运行目录、缓存与工具链。归档摘要另列在 `ARTIFACT_SHA256SUMS`，不形成自引用。

## 官方 CPA/CPAMP 的实际模拟结果

运行拓扑为 CPA `127.0.0.1:18633`、CPAMP `127.0.0.1:18634`、当前模拟上游 `127.0.0.1:18637`。旧模拟上游 18635 与独立 UI 18636 不作为当前业务上游证据。

官方进程通过 `preview_start` 启动，独立运行目录与 `env -i` 隔离真实配置。插件业务请求全部指向 loopback 假上游；官方 CPA 日志另有公开版本刷新，**不声称整个官方进程绝无外部 HTTP**。所有账号和管理凭据都是公开测试值。

| 验收项 | 实际结果 |
| --- | --- |
| 动态库与模型注册 | 初版从空池导入 3 Key/2 分组，无需重启出现 `commandcode/glm-5.2` 并推理；当前库重载后日志确认加载/注册、watcher 恢复三个 auth，模型接口与推理正常。 |
| 额度进入实际选号 | 当前库 A=90 credits/0.9 headroom、B=30/0.3 时返回 `selected a`；翻转 A=10/0.1、B=80/0.8 后返回 `selected b`。额度刷新经真实 CPAMP 管理代理，响应来自实际模拟上游；完成后恢复初始值。 |
| 多 Key 共享硬 cap | 当前库 A 的两个 Key 共享 cap=1，两个视图均返回组 inflight=1；A/B 各一条保持流时，上游 active 为 A=1/B=1。第三请求 500 且新增上游调用 0，峰值均为 1。 |
| 正常流完成 | 上游发送 `finish_reason=stop` 与 `[DONE]` 后，实际 active 与插件各视图 inflight 均归零。 |
| 客户端断连 | 关闭真实客户端 socket 后，等待实际上游 handler 与插件占位均归零；没有强制释放或 TTL 回收。 |
| 私有头防伪/剥除 | 客户端传入伪造 RequestID 头；模拟上游记录的 private_header 均为 null。 |
| 额度失效拒绝 | 当前库额度耗尽后推理 500 且上游调用不增加；非法 credits 的刷新返回 502，随后推理 500 且不调用上游。 |
| 协议 | 当前库 Chat Completions 非流/流通过；Responses 与 Messages 非流返回 200。未实跑后两者的 streaming。 |
| CPAMP 管理代理 | 既有官方 `/setup` 返回 200 并保存 CPA 连接；当前库重载后正确 CPAMP admin key 访问插件管理返回 200，缺失/错误 key 返回 401。静态插件资源公开 200，无凭据。 |
| 原生页面 | 官方侧栏与同源 iframe，显示 3 Key/2 分组、额度、共享 cap/在途及实际候选评分，不是外部网关嵌入。 |
| 管理操作 | 初版原版 UI 新增临时账号、清空 Key 输入、HTML 名称按文本显示；编辑/删除通过实际 CPAMP 代理 API 完成。原版 UI 部分导入成功 1/提交 2，网络响应含 partial_success；临时记录已删除。当前库手机新增表单成功打开/关闭，未添加额外记录。 |
| 页面会话 | 当前库 iframe 连接后管理 Key 输入为空；断开后账号/事件/Key 均清空；错误 Key 的 401 后停止轮询并清空会话。父页面自身有官方 storage 项，不声称整个宿主 storage 为空。 |

此 CPA 版本把 `pool_unavailable` 映射为 **HTTP 500**，而非 429：cap 满、额度耗尽的拒绝均如此。上游没有新增调用，因此硬准入测试通过；不能描述为已实现 429/Retry-After 排队合同。

首次流夹具仅发 `[DONE]`、未发 finish_reason，被插件正确拒绝。修正模拟协议后复跑通过，未放宽插件验证。初次文件改端口并未热加载，核运行配置后用官方 PATCH 切到 18637；最终证据不混用两个上游实例。

### 原版主题与手机布局验收

CPAMP 注入 `cpamp-plugin-host-style` 与主题变量。旧页面硬深色背景与宿主文字冲突，已用最小变量映射修复；通过 CPAMP 自己的主题菜单切换，**没有用临时 DOM/CSS 实现修复**。

- 桌面预览 1440×1000，iframe 可用宽 1230、文档宽 1215，无整页横向溢出。
- 浅色：宿主/iframe 均 `white`，body 背景 `rgb(239,242,247)`，标题/表格文字 `rgb(44,62,80)`，color-scheme 为 light。
- 深色：宿主/iframe 均 `dark`，body 背景 `rgb(10,10,10)`，标题/表格文字 `rgb(229,229,229)`，color-scheme 为 dark；账号与候选仍可操作。
- 手机浅/深色：375×812，宿主与 iframe 文档宽均 375。账号表容器宽 341、表宽 1080；分组表宽 620；候选容器宽 307、表宽 860，仅局部横滚。实际 scrollLeft=500 后整页仍宽 375。
- 手机新增 dialog 宽 341，left=17/right=358，成功打开/关闭；标题为 24px。
- 无运行异常。主动错误 Key 测试后出现预期 HTTP401，并取消另外两路并发 fetch；没有把这些预期拒绝报告为“零错误请求”。
- 完成后恢复原主题选择“自动”和 desktop viewport，iframe 已断开，轮询停止，管理 Key 清空。
- 截图为实际官方 CPAMP，文件 `official-reloaded-light.jpg`、`official-reloaded-dark.jpg`、`official-reloaded-mobile.jpg`，不是独立 UI mock。

### 历史权限阻断已解除

此前正常停止隔离 CPA 被权限检查拒绝，未绕过。用户随后明确“允许停止并重启隔离CPA，把剩余联调跑完”，正常 preview_stop 成功；确认无在途后替换库，通过 preview_start 重启。当前 CPA serverId 为 `dc93a88a-35bf-4189-9273-96d8e38d3e0d`，旧 ID 已停止。本轮重载与剩余模拟验收已完成，不再把历史拒绝作为当前阻碍。

## 当前源码的本地验证

| 项目 | 结果 |
| --- | --- |
| 普通 Go | `go test ./... -count=1 -timeout=120s`：12 个测试 package 全部通过；plugin 15.287s、pool 0.430s、resources 0.669s。 |
| Go race | `go test -race ./internal/... ./resources -count=1 -timeout=180s`：12 个 package 全部通过；plugin 20.724s、pool 4.049s、resources 1.873s。 |
| 静态检查 | `go vet ./...` 退出 0，无诊断。 |
| 主题专项 | resources 普通/race、JavaScript 语法/管理行为/安全/主题合同通过；独立页面 fallback 与本轮原版浅/深色及手机均验收。 |
| 构建 | Go 1.26.8 Linux amd64 c-shared；最低 Go 1.26.7，四个 ABI 导出、7 payload 摘要、8 native 成员逐字节一致。纯源码 83 文件加 manifest，与源码逐字节核验。 |
| 独立审查 | 前阶段两路 Opus 的 Pool/跨模块确认项已修复并增量复核，无剩余确认项；CSS 小改未重开核心整轮审查。本轮未再改业务源码，仅重载验证与更新交付说明。 |

独审修复包括：同组在途禁止迁组、活跃身份禁止替换、控制元数据拒绝秘密、历史事件重新脱敏、journal 先于秘密 staging 与实际 os.Exit 恢复、limited 语义纠正、catalog_revision 重发布、UI 组 inflight 不重复累加。取消/Abort/Delete/Close 不提前 Settle，无法确认 I/O 清理时保留占位并 quarantine。

普通/race/vet 日志为 `.scratch/commandcode-dev/official-final-test.txt`、`official-final-race.txt`、`official-final-vet.txt`。本轮未再改 Go/HTML，不重复执行已通过整轮单测；重载后的证据为 `official-reloaded-business-results.json`、`official-reloaded-concurrency-results.json`、`official-reloaded-theme-results.json`，均显式记录当前库 SHA256。

## 仓库检查：未全通过

`bash scripts/check.sh docs` 与 `changed` 的最近完成结果均退出 **1**：指导合同测试 **5 通过、2 失败**，当前基线缺根 `AGENTS.md` 与 `doc_mana/ocr/AGENTS.md`；docs 子扫描 **1018 通过、0 失败、712 跳过**。本轮说明更新后复跑仍为 **1018/0/712** 与 **5 通过/2 失败**，总退出码均 **1**，日志为 `.scratch/commandcode-dev/official-completed-docs.txt` 和 `official-completed-changed.txt`。子扫描通过不等于总入口通过，未修补无关基线文件、未豁免、未执行 full。Go 测试独立执行。

## 证据与尚未验收范围

受控证据归档只含 loopback 夹具/检查脚本、JSON 结果、文本摘要、实际 CPAMP 截图和本说明，不含官方二进制、运行 auth/state/数据库、数据密钥、生产配置或真实凭据。`official-reloaded-*` 为当前库实证；其余初版文件只作注明身份的历史回证，不据此冒充当前库独立重测。

尚未验收：完整 CommandCode credits/window/identity API 的所有边界行为、所有目录模型、大容量生产负载，以及 Responses/Messages 的真实协议与streaming。本次已完成两真实账号启用、真实DeepSeek Chat非流/流正文、实际额度评分选号、逐真实Group cap1重叠/满额拒绝和正常/取消owner释放。多Key同组硬cap仍只有既有模拟证据，本次每个真实Group各一Key，不伪造真实身份扩大覆盖。`freeCredits` 缺省合同仍未知，保持严格字段检查。真实Group来自whoami稳定身份核验。跨实例共享 cap、Home、持久完整审计不在本版支持范围。

本地 I/O 清理不保证厂商后台计算在取消瞬间停止。本版硬并发证明限定于插件受控连接/读写在途边界，不声称厂商后台执行 ACK。本次真实账号启用/小量测试已有直接授权并完成；后续扩大测试、库更新和发布仍按各自授权边界处理。
