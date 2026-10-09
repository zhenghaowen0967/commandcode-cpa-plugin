# CPA 全局 Scheduler 共存与受控切换

本文规定调度与切换边界，不自动授权任何生产变更。历史 0.1.5 已上线为 `commandcode-pool-next`；本轮 0.1.6 候选为 `commandcode-pool-update`，实际活动版本以现场读回及交付报告为准，源码验证见 [VALIDATION.md](VALIDATION.md) 最新一节。方案由**一个有效的账号池插件 Scheduler** 同时承接 CommandCode 调度与 Codex 429 保护；受控切换时旧、新实例可暂时并存，但宿主仅选择一个有效 Scheduler，不是多 Scheduler 回退链。下方初版迁移清单保留历史前提，不代表当前还须重新迁移。

## 目标行为与限制

- CPA v8 只选择一个有效插件 Scheduler。正常运行时只保留本方案一个有效账号池插件，活动 ID 须由插件管理接口核实（本轮候选为 `commandcode-pool-update`）；受控接管期间按已验证优先级选择旧、新实例，不依赖多 Scheduler 回退。停用/移除其他插件的冲突声明须另行获准。插件通过 `UsagePlugin` 仅记录宿主确认 `provider=codex` 且 `AuthType=oauth` 的失败 429，按 auth ID 维护本进程内的到期禁用状态。
- `provider=codex` 表示执行通道，不等同 Codex OAuth 凭据。第三方 Responses API key 不新建 Codex 配额 hold，正常 429 退避仍归宿主。调度候选用可信 `auth_kind=apikey/api_key` 区分历史误封；kind 为空才兼容旧 `api_key` 属性，显式 OAuth 不被 key 覆盖。模型名、域名、客户端声明和错误正文不能作身份依据。历史 API-key hold 不挡该候选，但保留记录；确认误封后的单条解禁不清其他 hold。
- 没有候选因 Codex hold 被过滤时，纯 CommandCode 走原有真实额度选号，其他或混合 provider 路由交给 CPA 已配置的内建策略。发生过滤时由本插件在过滤后的候选中选择；**不得委托 CPA 内建 picker**，因为宿主给它的是完整候选集，可能重新选中被 hold 的 Codex auth。若候选全部被 hold，插件明确 Reject，不做绕过或回退；不要把该拒绝描述成上游 HTTP 429，CPA 最终错误码须按对应官方错误分类核实后再说明。
- 新插件的 Codex hold 是单 CPA 进程内存状态，重启即丢失；管理入口为 `GET /v0/management/plugins/commandcode-pool/codex/bans`、`POST .../codex/unban` 和 `POST .../codex/unban-all`。`unban`/`unban-all` 是明确的人工操作，不属于切换步骤的自动清理。
- 固定旧源码 `ysxk/codex-429-autoban v0.2.2` 的只读 bans 响应包含 `plugin`、`version`、`count`、`bans`；每项含 `auth_id`、`window`、RFC3339 `reset_at`、`reset_at_unix`、`remaining_seconds`，可能还含 `banned_at` / `banned_at_unix`。旧插件也是进程内 map、重启丢失、按读取/调度惰性清除过期项；新插件不会自动导入。迁移前保存完整字段而非只存 ID。旧 API 的 `/unban` 也支持清空全部状态的参数，不得在快照步骤调用写接口。
- 新版候选提供 `POST /v0/management/plugins/<PLUGIN_ID>/codex/bans/import`，可提交 `{"bans":[{"auth_id":"...","reset_at":"<RFC3339>"}]}`；`reset_at` 也接受整数 Unix 秒，也可将旧 `/bans` 响应原样转交。只使用 `auth_id` 和 `reset_at`，不会沿用旧 window 分类；任一条非法会拒绝整批，过期条目忽略，已有 hold 只会被更新为更晚的 reset、不会缩短。导入只影响本进程内存，不自动 unban，也不持久化。该接口是受控迁移工具，不替代授权、快照与逐项核对；实际切换前须用本次最终候选复验。
- CommandCode 应使用独立 `commandcode/` 模型路由。无 Codex hold 的跨 provider 同名混合路由保留 CPA 内建策略，不保证余额评分；实际 CommandCode I/O 仍必须通过 `Pool.Acquire` 的额度和共享 cap 检查，不能绕准入。此限制不适用于纯 CommandCode 余额选号。
- CommandCode 的真实额度准入、账号共享并发 cap 与硬上限保持原状，不由 Codex hold 改写。CPAMP 菜单是同一个插件 ID 下的“CommandCode 账号池”和“Codex 429 保护”两个资源页面。
- CPAMP 1.14.4 另有 `USAGE_QUOTA_COOLDOWN_ENABLED` / 原生额度冷却及账号自动禁用、到期恢复和持久 ownership 能力；本方案不假设该功能关闭，也不接管它。2026-10-08 的接入检查已核实相关开关开启；后续变更前仍须重新确认当前状态。新插件手动 unban 不会清除 CPAMP 自己的 cooldown 或其 disabled/ownership 状态；切换前须识别并分别处理，未经用户决定不得擅自恢复。

## 初版迁移门槛（历史前提，未来切换须重新核实）

1. 取得切换授权后，先只读核对 CPA/CPAMP 当前版本与二进制身份、插件 ID/启用状态/优先级、Scheduler 声明、路由和配置；保存可恢复的配置快照并妥善保护凭据。核实 CPAMP 原生 cooldown 当前开关及持久状态。不得把历史候选 `49fc0d92…` 的模拟结果当作新候选证据；记录本次实际候选包、`.so` 与 `SHA256SUMS` 身份。
2. 通过已授权且鉴权的 CPA/CPAMP 管理通道，只读调用旧插件 `GET /v0/management/plugins/codex-429-autoban/bans`。保存带采样时间的**完整非密钥 hold 结构**，并确认返回内容含义、active holds 数量及到期时间；不要把响应凭据写入日志。旧插件为内存状态，不能假定新插件会自动导入。
3. 在旧插件仍运行时停止产生新的 Codex 失败、排空相关请求并观察上游 I/O owner 已结束；再次读取旧 bans，确认没有新 hold 或在途失败造成的竞态。若无法证明状态稳定，暂停切换。若有 active hold，只有在完成经核实的逐项迁移，或等待其自然到期并复查后，才可继续；不得直接换掉旧插件造成 hold 丢失。需要提前人工恢复时，先由用户逐项决定，不自动 unban。
4. 确认 CommandCode 旧 `CommandCode2Key` 直连旁路、真实 Group ID 和账号归属。它们尚未确认；不得自动搬 Key、改 Group ID、停用旁路或声称纳入账号池 cap。明确切换覆盖范围并取得相应授权后，才进入配置变更。

## 获准执行与回退

在上述门槛通过且变更单独获准后，按快照将旧 Scheduler 声明隔离，再以唯一插件 ID `commandcode-pool` 加载候选 `.so` 并应用对应插件配置；CPA/CPAMP 支持热载不等于可以省略 drain、状态核对或变更授权。不要整体停掉无关 provider，不要修改共享 auth 根目录权限，也不要擅自变更其他 key、配置或路由。检查唯一 Scheduler、管理页/接口和状态后，再按另行授权逐步恢复流量。此流程仅规定切换顺序，不构成真实账号测试或生产验收承诺。

回退时先停止新流量并确认本插件 CommandCode pool `inflight=0`；若无法确认，等待并查明 owner，不得强制清计数或盲目重启。依据快照恢复旧插件启用状态、优先级与路由，确认新插件不再接收流量后再处理本次新增的自有 `.so`/配置。保留并保护本插件 pool state、包校验材料及旧/新 bans 快照，不因回退删除状态；对新旧 Codex holds 的恢复/等待仍须按用户决定处理。任何删除、权限变更、清空 hold 或生产重启都不由本文自动授权。

版本更新须重新记录新候选与包摘要；单进程限制仍适用。本插件保留 `NOTICE.md` 中 Codex 429 AutoBan v0.2.2 MIT 来源说明；`cmdcode2api` 仅借鉴功能设计，不复制其无许可证源码。