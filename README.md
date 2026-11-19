# Claude Tier Router

Claude Code 的本地双模型网关，以 Go 实现，在 Opus 和 DeepSeek Flash 两组 Anthropic Messages 接口之间逐请求评估路由，并记录决策、缓存用量、费用和人工任务反馈。

支持代理、规则与可选 Flash 分类器、缓存成本估计、观察模式、分页统计和按问题反馈。更换供应商后，需要用你的 URL、准确模型 ID 和计费方式验证兼容性。

## 快速体验

需要 Go 1.23 或更新版本。SQLite 驱动为纯 Go，不需要安装数据库服务。

```sh
git clone https://github.com/chenhaitao888/claude-tier-router.git
cd claude-tier-router
go build -o bin/router ./cmd/router
./bin/router demo
```

打开终端显示的本地地址，默认是 http://127.0.0.1:8787 ，无需登录。演示只连接本机模拟上游，生成四条请求和一条人工反馈，不需要真实 API Key。演示价格、token 和任务结果均为模拟数据，记录在 `data/demo.db`。按 Ctrl+C 停止。

## 接入你的 Claude Code

```sh
cp config.example.json config.json
```

编辑两个 provider 的 `base_url`、`api_key`、`model`、`auth`、`context_tokens` 和 `prices`。密钥与接口地址一样直接写入本地 `config.json`，不需要设置环境变量。`base_url` 是 API 根地址，可以包含 `/anthropic` 或 `/v1`，不能包含 `/messages`。模型 ID 会显式改写，不依赖供应商对 Claude 模型别名的隐式映射。上下文长度使用实际模型和中转服务的限制；示例值只是占位配置。

连接本机 HTTP 反代时，`base_url` 可以填写 `http://localhost:8000`、`http://127.0.0.1:8000` 或 `http://[::1]:8000`，端口按实际服务修改。非本机接口需使用 HTTPS。这里的 `base_url` 是上游反代地址，网关自己的 `listen` 仍使用 `127.0.0.1:8787` 等回环 IP。

在示例文件中填好 `high.api_key` 和 `low.api_key` 后直接启动：

```sh
./bin/router check -config config.json
./bin/router claude -config config.json
```

`check` 只验证配置结构，不发送模型请求。`claude` 启动本地代理和 Claude Code，只修改子进程环境，不修改 `~/.claude`。附加 Claude Code 参数放在 `--` 后面，例如 `./bin/router claude -config config.json -- --continue`。现有受管理设置和 apiKeyHelper 仍可能影响客户端身份认证，需要在 Claude Code `/status` 中确认最终连接。

统计页打开即用，无需本地网关凭证，也无需设置 `ROUTER_TOKEN`。启动器会自动给 Claude Code 填入固定的 `local-router` 占位值，满足客户端的认证参数要求；网关不校验这个值，不把它转发给供应商。上游仍使用 `high.api_key` 和 `low.api_key`。旧配置中的 `gateway_token` / `gateway_token_env` 会被忽略，可以删除。

provider 仍可使用 `api_key_env`；非空 `api_key` 优先。`config.json` 已被 Git 忽略，程序不会把上游密钥写入日志或统计接口。网关只允许本机 loopback 监听，拒绝跨来源浏览器请求，不用于共享网络部署。

在公司电脑测试时，先按上述步骤克隆并编译，在本机新建配置并填写公司的两组接口。公司套餐价格未知时，把对应 `prices` 设为 `null`；把 `mode` 改为 `auto`。希望在列表中识别问题，可启用 `store_task_summaries: true`，会在本机保存提问前 80 字。

从你要测试的业务项目目录启动（替换为实际克隆路径）：

```sh
cd /path/to/your/work-project
/path/to/claude-tier-router/bin/router claude -config /path/to/claude-tier-router/config.json
```

打开 http://127.0.0.1:8787 查看统计。路由器跟随 Claude Code 退出；修改配置后重新启动。每台电脑使用独立的本地配置与数据库，仓库不携带密钥、历史提问或调用数据。

也可独立启动代理：

```sh
./bin/router serve -config config.json
```

在另一个终端、业务项目目录运行：

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 \
ANTHROPIC_AUTH_TOKEN=local-router \
ANTHROPIC_MODEL=router-auto \
CLAUDE_CODE_GATEWAY_HINT_HEADERS=1 claude
```

`local-router` 只是固定占位值，不是密钥。推荐使用 `router claude`，它还会设置子代理模型、模型别名和上下文窗口，并清除子进程中冲突的 provider 环境变量。

## 路由模式

| mode | 行为 |
| --- | --- |
| shadow | 默认。记录建议和候选成本，实际全部使用 Opus |
| auto | 每次推理重新评估，按质量建议、能力和缓存成本选择模型 |
| high | 固定 Opus，适合基线及接入验证 |
| low | 固定 Flash，适合基线及接入验证 |

`classifier.enabled` 默认 false。打开后，仅当规则不确定且两个模型仍可选择时，才额外调用 Flash 分类，输入有长度上限和超时，调用独立记账，不递归经过网关。分类器调用会产生真实费用；超时后不能假定供应商未收费。规则和置信度是初始启发式，不是训练或校准后的复杂度模型。

`policy.allow_text_switch` 默认 false。在真实上游双向接续验证通过后，可设置 true，放行普通文本和客户端工具历史切换。thinking、受保护内容及服务端状态仍保持来源模型。来源未知的恢复会话在 auto 下返回 409；使用明确的固定模型模式重新建立来源。固定模式代表你确认了历史兼容性。

## 价格与缓存

v4 策略中，`prices: null` 适用于价格未知或公司套餐：费用显示未知，使用占比照常统计，auto 按能力建议选模并跳过金额比较，不会把未知价格视作免费。填写价格时必须明确提供所有字段；不收费项显式填 0。下面是结构示例，数字仅用于说明，不能当作真实报价：

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

费用是 usage 乘以请求发出时价格快照的核算值，尚未与账单对账。支持 USD 和 CNY，按币种分别记账。配置 `policy.exchange_rate` 后，跨币种候选费用折算为 USD 比较，汇率快照随决策保存；未配置汇率时按能力建议选择并跳过跨币种金额比较。不完整响应、缺少 usage、缓存写入 TTL 不明、额外服务端工具费用或上游报告不同模型时，总费用保持未知。流式累计 output_tokens 只取最新值。支持高峰/空闲分时价格，尚不支持自动拉取价格或阶梯计费。

## 路由与诊断

- 提取最后一条有效用户任务，忽略 Claude Code 注入的 `<system-reminder>`，工具结果和纯提醒不会覆盖任务。规则长度上限按 Unicode 字符计算。上游请求内容保持原样。
- 分类器输入独立保留任务，再填充有界近期上下文；接受单个 JSON 对象或单个 JSON 代码围栏，拒绝重复字段、缺失字段、非法枚举、越界置信度、矛盾决策和额外内容。输出截断也保守回退。详细原因、停止原因、输出长度会入账，正文不记录；不自动付费重试。DeepSeek Anthropic 接口仅支持 `output_config.effort`，所以没有依赖不受支持的强制 JSON schema 参数。
- 已有 thinking / redacted thinking / 服务端状态历史时留在来源模型，直接跳过付费分类器。来源未知时 auto 返回 409。换一个 prompt_id 不能清除历史；新会话或真正不含受保护块的压缩后历史才重新具备切换条件。**Flash 已产生 thinking 历史后，后续复杂问题也可能被锁在 Flash；想独立测试简单题和复杂题，请分别开启新会话。** 不会擅自删除 thinking 签名或历史来强行切换。
- 缓存前缀按 tools → system → message 内容块构建，支持消息增长、标记移动和未缓存尾部修改。显式缓存考虑断点及 20 块回看范围；自动缓存从完整响应的输入 usage 建立预测。各模型与分支隔离、TTL 过期失效，覆盖量由 usage 按前缀相对大小分摊，再保守打八折。未缓存尾部按普通输入费率估算。
- “路由诊断”展示分类器结果、跳过原因、切换限制，按预测版本分别汇总缓存预测值、实际值、漏预测次数和平均绝对误差。未知价格仍不影响请求和 token 占比。

v4 针对 Claude Code 交互模式补充了缓存元数据适配：已识别的 `safeguards[].type=dangerous_tool_use` / `classifier_context.v=1` 不参与生成提示的缓存指纹和输入长度估算。工具执行后追加的 `prior_turn_context` 因而不会把整个前缀判为变化。该字段仍逐字转发给上游；其他 safeguards 字段、未知类型和未来版本仍保守参与指纹。

请求详情新增两档缓存诊断：本地是否有证据、是否过期、是否超出断点回看范围、生成参数 / 工具 / 系统提示 / 消息历史哪部分变化，以及工具安全上下文是否变化。诊断保存原因和计数，内容哈希只在内存比较，不保存正文、路径或权限上下文。历史记录没有这些字段时显示“旧记录未采集原因”。

示例使用固定汇率；请按自己的成本规划调整，它不是实时汇率，也不改变原币种账目：

```json
"exchange_rate": {
  "version": "user-fixed-2026-11-18",
  "cny_per_usd": 6.8
}
```

把以上对象放在 `policy` 内；删除或设为 `null` 即关闭跨币种比较。修改配置和升级二进制后都需重启。

## 已配置的价格

以下为示例价格，按每百万 token 计；请按实际供应商或套餐修改：

| 模型及币种 | 普通输入 | 缓存读取 | 缓存写入 5 分钟 / 1 小时 | 输出 |
| --- | ---: | ---: | ---: | ---: |
| Opus 5.5 · USD | 4 | 0.20 | 5 / 8 | 20 |
| DeepSeek Flash 空闲 · CNY | 1 | 0.02 | 0 / 0 | 4 |
| DeepSeek Flash 高峰 · CNY | 2 | 0.04 | 0 / 0 | 8 |

Opus 的输入、输出和缓存读取价格采用用户报价；缓存写入按 [OpenRouter 官方价格](https://openrouter.ai/anthropic/claude-opus-5.5/providers)补齐。DeepSeek 价格采用用户报价及[官方分时规则](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)：北京时间周一至周五 09:00–12:00、14:00–18:00 为高峰，周末和节假日为空闲。区间含起点，不含终点，按请求发出时间选择价格。DeepSeek 自动缓存没有单独写入加价，正常未命中输入仍按普通输入费计费。

`prices.peak` 保存时区、星期、时间窗、假期日期、日历有效范围和高峰费率。节假日当前根据[2026 年官方安排](https://www.beijing.gov.cn/zhengce/zhengcefagui/202511/t20251104_4258873.html)和[2027 年官方安排](https://www.sdxc.gov.cn/jryw/gnwzdxw/202611/t20261106_18279442.htm)配置，供应商实际账单口径仍需对账；超过日历覆盖范围时显示价格未知，避免把未收录假期算成高峰。无需在每次请求时联网查询日历。

## 统计和反馈

页面展示分币种费用、历史估算、未知费用数量、两档请求次数占比和总 token 占比、缓存读取量、分类器开销及人工任务结果。模型使用占比可按全部任务请求、主任务与子代理、后台辅助与压缩、路由分类器分别查看。请求详情提供推荐、拟执行和实际模型，候选成本、决策理由、价格与策略版本、usage、停止原因和错误。达到 `max_tokens` 或上下文上限会单独标记；`complete` 只代表传输完整，费用照常核算，不代表任务完成。

总 token = 普通输入 + 缓存读取 + 缓存写入 + 输出，各输入分项只计一次。请求次数占比基于已分配模型的请求（包含失败和重试）；未派发的请求单列。token 占比只采用响应完整且四项 token 齐全的记录，页面显示覆盖率及排除数量。没有数据时占比显示“—”，已知用量的零占比显示 0%。不同模型的分词方式可能不同，占比不代表费用或公司套餐扣减额度。

在“我的问题”列表中，每次提问只显示一行和一个“评价此问题”按钮。问题由完整的 `session_id + prompt_id` 关联，同名问题在不同会话中分别展示。点击后，表单顶部固定显示问题名称、开始时间与会话；提交目标绑定于所选问题，重复评价会更新该问题的反馈。不同问题的未保存草稿分别保留。逐请求明细默认折叠，只有详情入口，可按问题查看。后台/压缩请求不会独立成为可评价问题。

“问题验收”可以选择函数/API 解释、并发/死锁分析、代码修改三套证据清单，也保留快速人工评价。清单逐项填写通过、失败、未确定及证据；通过或失败必须提供文档/代码位置、测试命令和结果等依据。所有项通过才通过；任一项失败则失败；有未验证项且无失败则未确定。后端验证完整性并计算结果，不能只把总结果改成“通过”绕过检查。

清单和证据属于**人工评审记录**，并不自动执行命令、核验网址内容或调用另一个模型打分。示例未编译、风险未复现时应保留未确定。页面单独展示带证据清单的通过、失败、未确定数量。`GET /api/rubrics` 返回标准与版本；`POST /api/feedback` 可增加 `rubric`、`rubric_version`、`checks`（每项包含 `id`、`outcome`、`evidence`），总 `outcome` 可省略由服务端计算。旧反馈接口与历史记录保持兼容。

默认不保存提示词正文、模型回答或工具参数，保留标识和结构化统计。用户可显式启用顶层 `store_task_summaries: true`，在本地 `task_labels` 表记录去除系统提醒后的提问前 80 个 Unicode 字符；不会保存完整提问或回答。示例配置默认关闭。摘要按任务首次记录，后续工具调用或压缩不会覆盖；可以手动改名称。关闭开关只停止自动采集，已有标题仍保留。历史问题仅通过会话 ID 和提问 ID 精确匹配本地 transcript 补齐，无法对应时显示“历史问题（未记录摘要）”，先补名称再评价。生产记录位于配置中的数据库，演示使用单独数据库。历史价格未知的完整请求，可按当前配置和当时请求时间给出“历史估算”；估算仅在查询结果中展示，不修改原始数据库记录或已有价格快照。仅对模型 ID 与当前配置一致的记录估算。提供本地请求样本的离线缓存复查测试；尚未实现测试结果 hooks、任务级自动验收、自动化完整任务双策略对照、账单对账和真实节省率。

## 验证与开发

```sh
go test -race ./...
go vet ./...
```

测试使用本机模拟 HTTP 上游及临时 SQLite，覆盖 SSE 即时转发、累计用量、断流、取消、认证替换、请求字节保留、分类开销、缓存决策、历史锁定、压缩归属、并发分支和反馈。测试不会调用真实模型。

项目结构：`cmd/router` 为命令行，`internal/router` 为代理、路由与存储，`internal/router/web` 为内嵌页面。完整产品方案见 [设计文档](docs/design.md)，真实联调步骤见 [接口验证](docs/provider-validation.md)，前期真实对照见 [v3 验证报告](docs/v3-validation.md)，交互请求缓存修复与证据验收见 [v4 验证报告](docs/v4-validation.md)。

“我的问题”和“逐请求明细”分别分页，每页可选 **10 / 20 / 50** 条，默认 20 条。单个问题的请求明细也支持分页；切换每页数量或请求筛选时回到第一页，刷新保留当前页。问题统计始终覆盖其完整账本，翻页不会改变验收表单绑定的问题或混用草稿。

`GET /api/tasks?page=1&page_size=20` 返回 `tasks`；`GET /api/records?page=1&page_size=20` 和 `GET /api/task-records?session_id=...&prompt_id=...&page=1&page_size=20` 返回 `records`。响应均包含 `page`、`page_size`、`total`、`total_pages`；超出末页时返回末页，空列表使用第 1 页。请求明细按完整会话和提问标识筛选，不限制在最近 500 / 1000 条。问题页按开始时间倒序，全部请求按时间倒序，指定问题的请求按时间正序。摘要与反馈不受分页影响；任务费用包含已关联的推理、后台和分类调用，按币种汇总。

`POST /api/task-title` 可保存最多 80 字的手动名称。接口不需要本地凭证；标题表与请求账本分开，补标题不改写历史调用或计费。
