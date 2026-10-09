# 来源与许可声明

## 复用基座

本地 `commandcode-pool`（候选版本 `0.1.2-local`）基于
[mczhoucn/commandcode-go-cliproxyapi](https://github.com/mczhoucn/commandcode-go-cliproxyapi)
修改，固定来源提交为
[`ea84cdd799564f644c6f9f39c7dc356d0f013526`](https://github.com/mczhoucn/commandcode-go-cliproxyapi/tree/ea84cdd799564f644c6f9f39c7dc356d0f013526)。

原始版权为 `Copyright (c) 2026 mczhoucn`，许可为 MIT。本目录的
`LICENSE` 保留原始版权与完整许可文本；构建包必须包含它，不能只保留此说明。
本地变更包括账号池、真实额度准入、真实账号共享并发、请求清理确认、
持久化与管理页面。本项目不是基座作者发布的官方版本。

## Codex 429 保护规则

统一插件的 Codex 窗口识别与临时禁用规则参考并适配现用
`ysxk/codex-429-autoban` 的固定 `v0.2.2` MIT 源码；具体仓库、版权及完整许可见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)，源码和原生交付包均须保留。
统一插件使用一份禁用状态并修正全禁用时的内建回落、双窗口恢复时间与重试缩短期限边界。

`peach0x33a/cmdcode2api` 固定 `2f30fbf…` 未发现明确许可，仅参考功能设计，
没有复制其源码，也未因此加入额外 OAuth 或网关功能。

## 直接及间接依赖

版本以随源码交付的 `go.mod` / `go.sum` 为准。当前固定依赖包括：

| 依赖 | 版本 | 版权 / 许可来源 |
| --- | --- | --- |
| `github.com/router-for-me/CLIProxyAPI/v8` | `v8.0.20` | MIT；Luis Pater、Router-For.ME；[原始 LICENSE](https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.20/LICENSE) |
| `github.com/tidwall/gjson` | `v1.19.0` | MIT；Copyright (c) 2016 Josh Baker |
| `github.com/tidwall/sjson` | `v1.2.5` | MIT；Copyright (c) 2016 Josh Baker |
| `github.com/tidwall/match` | `v1.1.1` | MIT；Copyright (c) 2016 Josh Baker |
| `github.com/tidwall/pretty` | `v1.2.0` | MIT；Copyright (c) 2017 Josh Baker |
| `gopkg.in/yaml.v3` | `v3.0.1` | 部分 libyaml 移植文件 MIT，其余 Apache-2.0；Kirill Simonov、Canonical Ltd |

CLIProxyAPI 版权原文：

```text
Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME
```

上述 MIT 依赖的许可条款如下（对应版权声明见上表）：

```text
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to
do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```

YAML 的 MIT 部分适用于 `apic.go emitterc.go parserc.go readerc.go scannerc.go
writerc.go yamlh.go yamlprivateh.go`，版权原文为：

```text
Copyright (c) 2006-2010 Kirill Simonov
Copyright (c) 2006-2011 Kirill Simonov
```

其余 YAML 文件的声明为：

```text
Copyright (c) 2011-2019 Canonical Ltd

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

Go 工具链与运行时的许可见 [Go LICENSE](https://go.dev/LICENSE)。
本次只生成本地验证包，不构成对外发布；若后续获准分发，仍须核对最终二进制
包含的运行时、第三方组件及完整许可文本，并随分发包附上相应材料。

## 非背书与版本范围

CPA SDK `v8.0.20` 和 CPAMP `v1.14.4` 是参考和隔离模拟版本，
不表示这些项目作者背书。现用 CPA 的 SDK/toolsearch 二进制摘要不同于官方发行版，
历史验证以实际被测二进制为准；`0.1.2-local` 尚未进行隔离原生或生产加载验收。
分阶段结果见 [VALIDATION.md](VALIDATION.md)。
