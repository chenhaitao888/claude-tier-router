# Claude Tier Router

Claude Code 的本地双模型网关，以 Go 实现，在 Opus 和 DeepSeek Flash 两组 Anthropic Messages 接口之间逐请求评估路由，并记录决策、缓存用量、费用和人工任务反馈。

当前是可运行的第一版：支持代理、规则与可选 Flash 分类器、缓存成本估计、观察模式和本地统计页。真实接口兼容性与模型质量尚需用你的 URL、准确模型 ID 和价格进行验证。

## 快速体验

需要 Go 1.23 或更新版本。SQLite 驱动为纯 Go，不需要安装数据库服务。

```sh
go build -o bin/router ./cmd/router
./bin/router demo
```

打开终端显示的本地地址，默认是 http://127.0.0.1:8787 。演示登录凭证为 `local-demo-token-not-a-real-key`。演示只连接本机模拟上游，生成四条请求和一条人工反馈，不需要真实 API Key。演示价格、token 和任务结果均为模拟数据，记录在 `data/demo.db`。按 Ctrl+C 停止。

## 接入你的 Claude Code

```sh
cp config.example.json config.json
```

编辑两个 provider 的 `base_url`、`model`、`auth`、`context_tokens` 和 `prices`。`base_url` 是 API 根地址，可以包含 `/anthropic` 或 `/v1`，不能包含 `/messages`。模型 ID 会显式改写，不依赖供应商对 Claude 模型别名的隐式映射。上下文长度使用实际模型和中转服务的限制；示例值只是占位配置。

在本地环境中设置下面三个变量，密钥由进程读取，配置文件只保存变量名：

```sh
export ROUTER_TOKEN='替换为至少16字符的本地随机凭证'
export OPUS_API_KEY='你的高级模型密钥'
export DEEPSEEK_API_KEY='你的廉价模型密钥'
./bin/router check -config config.json
./bin/router claude -config config.json
```

`check` 只验证配置结构，不发送模型请求。`claude` 启动本地代理和 Claude Code，只修改子进程环境，不修改 `~/.claude`。附加 Claude Code 参数放在 `--` 后面，例如 `./bin/router claude -config config.json -- --continue`。现有受管理设置和 apiKeyHelper 仍可能影响客户端身份认证，需要在 Claude Code `/status` 中确认最终连接。

也可独立启动代理：

```sh
./bin/router serve -config config.json
```

再为 Claude Code 设置 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN` 并启用 `CLAUDE_CODE_GATEWAY_HINT_HEADERS=1`。统计页面使用同一个本地网关凭证登录。当前只允许 loopback 监听，不用于共享网络部署。

## 路由模式

| mode | 行为 |
| --- | --- |
| shadow | 默认。记录建议和候选成本，实际全部使用 Opus |
| auto | 每次推理重新评估，按质量建议、能力和缓存成本选择模型 |
| high | 固定 Opus，适合基线及接入验证 |
| low | 固定 Flash，适合基线及接入验证 |

`classifier.enabled` 默认 false。打开后，不确定请求会额外调用 Flash 分类，输入有长度上限和超时，调用独立记账，不递归经过网关。分类器调用会产生真实费用；超时后不能假定供应商未收费。规则和置信度是初始启发式，不是训练或校准后的复杂度模型。

`policy.allow_text_switch` 默认 false。在真实上游双向接续验证通过后，可设置 true，放行普通文本和客户端工具历史切换。thinking、受保护内容及服务端状态仍保持来源模型。来源未知的恢复会话在 auto 下返回 409；使用明确的固定模型模式重新建立来源。固定模式代表你确认了历史兼容性。

## 价格与缓存

`prices: null` 表示价格未知，费用显示未知，auto 模式不会声称找到了最优价格。填写价格时必须明确提供所有字段；不收费项显式填 0。下面是结构示例，数字仅用于说明，不能当作真实报价：

```json
{
  "version": "your-provider-2026-11-18",
  "currency": "USD",
  "input_per_million": 1,
  "cache_read_per_million": 0.1,
  "cache_write_5m_per_million": 1.25,
  "cache_write_1h_per_million": 2,
  "output_per_million": 5
}
```

DeepSeek 自动缓存通常没有单独的缓存写入加价，仍需按你实际供应商的计费和 usage 契约填写。原生 Anthropic 的普通输入、缓存读取和缓存写入是分离项；本实现使用该契约。中转服务如果把缓存 tokens 包含在 input_tokens 内，必须先扩展适配，不能直接使用当前账本。

`cache.kind` 为 explicit、automatic 或 none。`ttl_seconds` 是本地保守预测窗口，特别是 automatic 模式下，不代表上游提供固定 TTL 保证。预测只使用本进程成功请求中观察到的缓存证据，按模型和对话分支隔离；重启后预测从冷缓存开始。移动缓存标记不会改写转发内容。

费用是 usage 乘以价格快照的核算值，尚未与账单对账。不完整响应、缺少 usage、缓存写入 TTL 不明、额外服务端工具费用或上游报告不同模型时，总费用保持未知。流式累计 output_tokens 只取最新值。第一版不支持自动拉取价格、分时价格或阶梯计费。

## 统计和反馈

页面展示已知费用小计、未知费用数量、两档请求比例、缓存读取量、分类器开销及人工任务结果。请求详情提供推荐、拟执行和实际模型，候选成本、决策理由、价格与策略版本、usage 和错误。

点击请求的“评价”，记录同一 session_id 和 prompt_id 的通过、失败或未知；重复评价会更新任务结果。人工反馈独立于 HTTP 成功。缺失 prompt_id 的请求不会被伪造为独立任务。页面时间线显示最近 500 条，概览统计整个数据库。

默认不保存提示词正文、模型回答或工具参数，保留标识和结构化统计。生产记录位于配置中的数据库，演示使用单独数据库。当前尚未实现测试结果 hooks、任务级自动验收、离线重放、双策略对照实验、账单对账和真实节省率。

## 验证与开发

```sh
go test -race ./...
go vet ./...
```

测试使用本机模拟 HTTP 上游及临时 SQLite，覆盖 SSE 即时转发、累计用量、断流、取消、认证替换、请求字节保留、分类开销、缓存决策、历史锁定、压缩归属、并发分支和反馈。测试不会调用真实模型。

项目结构：`cmd/router` 为命令行，`internal/router` 为代理、路由与存储，`internal/router/web` 为内嵌页面。完整产品方案见 [设计文档](docs/design.md)，真实联调步骤见 [接口验证](docs/provider-validation.md)。
