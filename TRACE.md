# 可信逐请求追踪合同（0.1.8-local）

本版仅追加观测能力，现用生产 0.1.7 不因源码或安装包交付自动升级。

## 三个 ID 的职责

| 字段 | 来源与用途 |
|---|---|
| `trace_id` | 宿主 `RequestInterceptRequest.TraceID`：入站 HTTP 追踪。用于与 CPA usage trace 或响应 `X-CPA-TRACE-ID` 内的 UUID 精确连接。 |
| `request_id` | 宿主 `RequestInterceptRequest.RequestID`：一次模型执行的生命周期，仍用于选号、取消和事件分组。 |
| `attempt_id` | Pool 独立创建的执行 owner，Acquire 与 Settle 使用同一值。 |

多个宿主执行可以来自同一个入站 trace。相同 trace 不合并 owner、不共享一次释放，也不改变并发统计。
客户端 session 与 trace 的关系应从真实客户端日志或响应建立；插件不解析或猜测客户端 session。

## 可信来源与传递

1. before 拦截器清除所有大小写变体的 `X-Commandcode-Pool-Request-Id` 和
   `X-Commandcode-Pool-Trace-Id`，只从宿主 typed 字段重新注入。
2. after 拦截器按可信池凭据确认渠道，重新清除并注入。非池渠道不获得这些内部头。
   RequestID 缺失仍遵循原 fail-closed 准入；TraceID 缺失不阻止旧宿主正常调用。
3. scheduler 从单值内部头调用 `PickWithTrace`。实际执行调用 `AcquireWithTrace`，
   移除内部头后才进入上游 I/O。执行 owner 保存 trace，后续 quarantine、Settle 沿用它。
4. request.complete 可记录宿主 trace，但仍仅取消，不代替 owner 释放占位。

不从 metadata、原始 payload、客户端同名头、其他 trace header 或时间接近程度回退。
TraceID 只接受标准 36 字符 UUID（版本 1–8、RFC 变体），保持原大小写；格式校验不赋予信任。
不是 UUID 的旧宿主值视为不可直接关联，事件省略该字段，不擅自截短或扩写。
当前 CPA 宿主生成 UUIDv7；宿主生命周期 RequestID 独立生成 UUIDv4，两者不能假定相等。
已知账号秘密即使碰巧是合法 UUID，也经事件写入和读取两次脱敏，不出现在追踪字段中。

## 兼容与页面

- `Event` 兼容追加 `trace_id,omitempty`；原 JSON 字段、事件 sequence、缓存范围、游标、
  `scope=requests` 与原接口不变。无 trace 的事件不增加空字段。
- 原 `Pick`、`Acquire`、`AbortRequest` 保留，通过空 trace 走同一行为。
- 新接口不会保存无界 RequestID→TraceID 表；只有已存在 owner 持有 trace。
- 选号、真实额度新鲜度、组共享 cap、terminal 标记、取消和 fail-closed 清理合同不变。
- 页面继续按 host request_id 一请求一行，账号名称在主层；两个 ID 和逐事件记录放技术详情。
  缺失 trace 明确显示“不能直接关联”；同一 host 的多个不同 trace 显示关联信息不一致。
  不将同一 trace 的不同 host 请求合并，外来文本只用 textContent 展示。

## 验证边界

相关 Go/资源回归覆盖 typed 来源、头清除、歧义/坏格式、旧事件、秘密脱敏、自然释放、
相同 trace 的独立 owner 以及上游不收到内部头。原生隔离验证使用公开合成账号与本地上游，
将客户端响应的 UUID 精确对到 pick/acquire/settle；不冒充真实模型或生产部署。

有界回归工具、离线证据格式与真实探针授权门见 [REGRESSION.md](REGRESSION.md)。
历史 0.1.7 的并发结论仍是整批集合归属；新字段不能倒填旧证据或把历史测试改称逐条配对。
