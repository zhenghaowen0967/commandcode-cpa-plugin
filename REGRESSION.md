# 有界回归工具与请求追踪

工具位于独立插件仓的 `scripts/regression/`，只依赖 Python 标准库。默认离线；不会安装、启动、重启 CPA/CPAMP，不修改调度、账号或并发上限。`live` 会读取明确指定的凭据文件，并访问明确授权的直接 CPA；不能把它描述成“不读生产”。

| 入口 | 行为 | 证明范围 |
|---|---|---|
| `offline`（默认） | 自包含合成夹具单测 | 工具逻辑及反例，不是运行环境或真实模型验收 |
| `analyze` | 离线读取显式 JSON 证据，独占创建报告 | 集合归属、显式容量合同、事件支持的 TraceID 连接 |
| `live` | 显式授权后，直接回环 CPA 顺序发送 1–4 次固定探针 | 最小完整响应及可选逐请求追踪，不是全容量 |

## 默认离线

```bash
bash scripts/regression.sh
```

```bash
bash scripts/regression.sh offline
```

`PYTHON_BIN` 可指定 Python 3 解释器，默认 `python3`。不需要 `.env`、生产配置或真实账号。夹具仅包含公开合成 UUID、摘要与数值。

离线入口在本进程安装 `sys.addaudithook`，阻断 `socket.connect`（含回环）以及命中生产路径标记的 `open`，记录违规并使检查失败。路径标记包括 `/proc/`、CPA 核心、管理密钥、cc-switch/usage 数据库等。它不是操作系统沙箱，不穷举所有可能的生产路径，也不继承到子进程；CLI 测试子进程只处理显式临时合成文件。不能用审计摘要声称任意外部代码都无法联网或读取生产。

输出包含测试数、失败、错误、跳过及违规摘要。`scripts/test.sh` 先运行该入口，再运行既有 Go 普通/race 检查；`regression.sh go` 转发 `scripts/test.sh`。

## 离线证据分析

```bash
bash scripts/regression.sh analyze --cases CASES.json --observations OBSERVATIONS.json --completeness COMPLETENESS.json --prior-cursor 100 --out REPORT.json
```

若完整性回执已内嵌在 observations，可省略 `--completeness`。`--baseline` 可追加身份基线核对；`--trace-id` 可查询一个入站 UUID 对应的事件宿主集合。

### 输入合同

`CASES.json` 是数组或含 `cases` 的对象。每条记录包含：

- `attempt_status="verified"`、`http=200`、带时区的 `started_at_utc` / `finished_at_utc`。
- `server_execution.account_ordinal`：正整数账号序号；账号明文和 Key 不属于证据格式。
- 新宿主模式：每条明确提供 `host_request_id`。可选 `client_trace_id` 是客户端响应/日志读出的标准 UUID，不是宿主生命周期 ID。
- 旧 0.1.7 集合模式：可以没有 host ID，必须保留唯一客户端 `session_id`、`association="exact_session"`、最终唯一成功 `cpa_usage` 与执行账号序号。其余 usage 尝试只能是已证实的本地 cap 拒绝，不能把未知失败吞掉。不得补造 host UUID 或拿 usage 的 `request_id` 冒充它。
- 若要求身份或 token 预算验收，逐条提供相应 `identity` / `max_tokens` 证据；缺失不能以默认零代替。

`OBSERVATIONS.json` 包含完整 `events` 与全池 `samples`：

- 事件至少保留 `sequence`、`request_id`、`action`，Acquire/Settle 保留 `attempt_id`、账号序号和 reason；可选 `trace_id` 必须是宿主可信字段。选号候选供满载重选验证使用。
- 采样包含 `at` 与非空 `accounts`，每账号 `account_ordinal` / `inflight` / `cap`。首采样早于首 POST、末采样晚于末响应；两边在途都为零，账号集合稳定。当前提取合同验证每组共享 cap10，不据此支持不同 cap 或跨实例总上限。
- 完整性回执包含 `initial_cursor`、`last_test_event_sequence`、`period_request_events`、`oldest_retained_request_sequence`，以及 `actual_buffer_retained_entire_period=true`、`period_events_equal_exactly=true`。最老保留序号必须不晚于初始游标，sequence 跨度须小于当前 500 条缓存容量。不能只相信默认请求条数或两个布尔。

### 三类结论必须区分

1. **集合证明**：每个真实成功执行必须有独立 lease、客户端不缓存响应；成功数与新 Acquire 数等量、账号分布守恒、事件完整、自然 Settle、无残留。`collection_proven` 只说明整批归属，不等价于逐条配对或容量通过。
2. **容量合同**：必须显式给出 `--expected-peak` 和/或 `--require-busy-switch`。满载重选需同宿主拒绝/换组后实际 Acquire，不把一次 Pick 当成功。未声明容量门时 `capacity_acceptance_passed=false`，不是容量失败，也不能报容量通过。
3. **TraceID 连接**：依据 `events.trace_id` 精确相等及该 host 的关键 owner 事件一致，不能只信 cases 提示、时间接近或 UUID 长相。同一入站 trace 可有多个 host；返回集合，不强行唯一配对。旧无 trace 证据只保留集合结论，不倒填逐条追踪。

例如，显式验收某一已授权历史轮次的峰值与满载重选：

```bash
bash scripts/regression.sh analyze --cases CASES.json --observations OBSERVATIONS.json --completeness COMPLETENESS.json --prior-cursor 100 --expected-peak 11 --require-busy-switch --max-posts 17 --out CAPACITY_REPORT.json
```

数字只说明命令格式，必须替换成**该轮真实合同和证据**，不是新的真实请求许可。`--max-posts` 在当前分析格式中限制观测到的 Acquire / 准入拒绝尝试数（`posts_budget_basis=observed_acquire_attempts_not_authorized_posts`），不是网络 POST 次数或实际授权预算证明。`--max-tokens` 核逐条声明值；这些离线门不会批准或发出请求。真实 POST 次数须另核对应 live journal 或调用者发送账本。

报告独立列出集合、容量、预算、身份、完整响应耗时与 TraceID 连接。显式声明的验收门不满足时 overall `passed=false`；集合可以保持 `collection_proven=true`。退出码：`0` 通过、`2` 证据/验收门未通过、`1` 输入或执行错误。报告用 `O_EXCL` 创建，已有目标拒绝，不覆盖原证据。

## 显式直接 CPA 探针

**运行标记不是业务授权。** 调用者须先获得该环境的实际 POST 次数、并发、token 和凭据读取许可。不沿用其他轮次的预算，不从已有 journal 恢复重发。

```bash
bash scripts/regression.sh live --authorized-live --gateway http://127.0.0.1:8317 --key-file /absolute/private/client-key --management-key-file /absolute/private/management-key --identity-baseline BASELINE.json --journal NEW_JOURNAL.json --out NEW_RESULT.json --requests 1 --concurrency 1 --max-tokens 128 --require-trace
```

参数均显式给出；示例端口不是机器身份真源。只支持 `http://127.0.0.1:PORT` 的直接 CPA，不支持 `localhost`、远程/IPv6、URL 凭据、附加路径/查询/fragment，也不冒称验证了 Claude 网关→CPA 的代理链。

### 授权、身份与预算门

- 必须 `--authorized-live`，`requests` 为 1–4、`concurrency=1`、`max_tokens` 为 1–1024；不接受 bool，不静默夹取。
- 客户端与管理 Key 各来自指定的当前用户 **0600 常规文件**。用同一 `O_NOFOLLOW|O_NONBLOCK` 文件描述符完成 `fstat` 和读取，拒绝符号链接、FIFO、错误权限、超长或控制字符。秘密仅在受控内存，不进入参数、日志、报告或浏览器。
- 已有 out/journal（含链接）拒绝运行。out 在模型 POST 前预留；journal 独占创建，预算先持久化；两路径不能相同。
- `BASELINE.json` 必须经过实际只读核对，而非为通过工具而生成任意相同快照。字段如下：

| 字段 | 含义 |
|---|---|
| `identity_confirmed` | 实际身份已核，必须为 true |
| `pid` / `startticks_epoch` | 进程 PID / `pid:startticks`，不只核端口或 PID 数字 |
| `core_path` / `core_sha256` | 绝对 core 路径及实际运行 executable 摘要 |
| `library_path` / `lib_sha256` | 绝对库路径、对应内存映射文件 device/inode 及摘要 |
| `config_path` / `config_sha256` | 绝对配置路径、实际 `--config` 参数及文件摘要 |
| `plugin_version` | 同一 CPA `/status` 读回的版本 |
| `management_boundary` | 同 gateway origin 的插件管理前缀，无 query/fragment |

管理前缀只接受 `/v0/management/plugins/commandcode-pool`、`-next` 或 `-update`。每次发送前实际检查 `/proc/PID/exe`、cmdline、maps、stat，以及进程 fd 对应的 IPv4 LISTEN socket；管理 GET 前后复核进程，随后核各摘要与基线。不是比较两份静态 JSON。配置摘要只说明当前磁盘文件与进程参数一致，不证明任意宿主会热加载该文件。

身份漂移或首次失败**停止追加**，不重试、不取消其他请求、不强回收 owner；已经发出的自身响应尽力完整读完。检查不是原子锁：若进程恰在最后预检之后重启，不能承诺该 POST 一定零发出；之后的身份检查会使结果失败并停止后续请求。

### 请求与结果

请求固定 `cc-deepseek-v4.1-flash`、公开合成提示、无工具、非流式。完整响应须 HTTP200、`type=message`、正常结束、有效文本且含“请求追踪探针成功”；工具/未知块、`max_tokens` 截断、坏正文都不算成功。响应最多 2 MiB，不跟重定向。

只从响应 `X-CPA-TRACE-ID` 提取一个标准 UUID，不把 `X-Request-Id`、私有 request header 或其他 UUID 当父 trace。`--require-trace` 要求对应 Pick/Acquire/自然 Settle 事件精确一致；当前最小探针限定一次直接执行、一个 owner，不支持借它验证重试/多执行全容量。未要求 trace 时，无 trace 的旧宿主仍可验证基本响应，追踪能力单列为未证明。

- `posts_reserved`：在调用传输前持久化的预算预占。
- `posts_sent`：当轮调用一次传输的尝试数，不代表上游一定接受；超时仍计一次，不报告成零。
- 非终态 journal 中 `budget_reserved` 可能已发出；崩溃后**不重发**，不能据未完成的计数推断零 POST。
- 无法确认计数的执行/落盘异常报告 null，而非伪造零。失败退出非零，保留证据。
- `full_capacity_supported=false` 恒定；顺序 1–4 次成功不是并发验收。

## 本轮验证与边界

具体结果见 [VALIDATION.md](VALIDATION.md)，可信来源/旧接口见 [TRACE.md](TRACE.md)。本轮真实生产模型 POST、生产部署及重启均未执行；公开合成原生环境若通过，仅证明被测 core/候选库的路径，不当作生产上线。

未覆盖跨实例共享 cap、每账号全部打满、总30并发、长时间压力、全部模型、真实配额耗尽或上游429。历史0.1.7并发结果仍是集合级历史证据，不会因新字段改称逐条验收。
