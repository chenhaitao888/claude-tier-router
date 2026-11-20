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

配置完成后，在 Router 项目目录安装命令（macOS / Linux，只需一次）：

```sh
./install.sh
```

安装脚本编译 `bin/router` 并将 `~/.local/bin/router` 链接到它。原有 `config.json` 和数据库仍保留在 Router 项目中。若该目录尚未加入 PATH，脚本为 zsh / bash 追加 PATH 设置，按提示打开新终端即可；其他 shell 按提示加入 PATH。不会覆盖已有的其他 `router` 命令文件。可用 `./install.sh --bin-dir /your/bin --no-path-update` 指定安装目录并自行管理 PATH。

以后在任意业务项目目录启动：

```sh
router check
router claude
```

`check` 只验证配置结构，不发送模型请求。`claude` 自动启动或复用独立运行的本地共享网关，再启动当前目录的 Claude Code；使用子进程环境及本次会话的 `--settings` 固定 Router 地址、模型别名和本地占位认证，不修改 Claude Code 的全局或项目设置文件。会话设置保存在权限为 0600 的临时文件中，子进程退出时清理。插件、hooks、权限等普通设置继续加载。附加 Claude Code 参数放在 `--` 后面，例如 `router claude -- --continue`。

省略 `-config` 时，优先读取 Router 可执行文件所属项目的 `config.json`，再查找可执行文件同目录、`~/.config/claude-tier-router/config.json`，最后兼容当前目录的 `config.json`；启动时会打印实际配置路径。通过安装链接启动也能找到原项目配置，数据库相对路径仍以配置文件目录为基准。需要使用其他配置时，继续使用 `router claude -config /path/to/config.json`。无需安装也可使用原来的 `./bin/router claude -config config.json`。

如果另传一个 `--settings` 文件或 JSON，启动器会保留其中其他字段及环境变量，再覆盖连接 Router 所需的字段；不会把上游密钥传入 Claude Code。原 provider 的 `apiKeyHelper`、API Key、OAuth token 与云 provider 开关仅在这次会话中停用。企业 managed settings 仍遵循 Claude Code 自身的更高优先级。

统计页打开即用，无需本地网关凭证，也无需设置 `ROUTER_TOKEN`。启动器会自动给 Claude Code 填入固定的 `local-router` 占位值，满足客户端的认证参数要求；网关不校验这个值，不把它转发给供应商。上游仍使用 `high.api_key` 和 `low.api_key`。旧配置中的 `gateway_token` / `gateway_token_env` 会被忽略，可以删除。

provider 仍可使用 `api_key_env`；非空 `api_key` 优先。`config.json` 已被 Git 忽略，程序不会把上游密钥写入日志或统计接口。网关只允许本机 loopback 监听，拒绝跨来源浏览器请求，不用于共享网络部署。

在公司电脑测试时，先按上述步骤克隆、配置并运行 `./install.sh`，在本机配置中填写公司的两组接口。公司套餐价格未知时，把对应 `prices` 设为 `null`；把 `mode` 改为 `auto`。希望在列表中识别问题，可启用 `store_task_summaries: true`，会在本机保存提问前 80 字。

从你要测试的业务项目目录启动：

```sh
cd /path/to/your/work-project
router claude
```

打开 http://127.0.0.1:8787 查看统一统计。每个终端都可在不同业务目录运行 `router claude`，共享同一个网关，按各自会话 ID 隔离路由状态和问题统计。关闭任意 Claude 会话都会释放连接，网关继续运行；关闭第一个窗口也不会影响其他窗口。每台电脑使用独立的本地配置与数据库，仓库不携带密钥、历史提问或调用数据。

更新代码时，在 Router 项目目录执行以下命令。后台网关没有活动会话时，下次 `router claude` 会自动加载变更后的配置或程序；仍有会话或请求在运行时，会提示先结束会话再重启，不会中途替换。安装链接依赖这个目录，请保留它；移动目录后重新处理旧链接并安装。

```sh
git pull --ff-only
./install.sh
```

共享服务管理命令：

```sh
router status
router stop
router restart
```

`status` 显示服务地址、PID、活动 Claude 会话数和在途请求数。`stop` / `restart` 默认拒绝打断活动会话或请求；确实要中断全部会话时可以显式加 `-force`。这些命令也支持 `-config`，不需要额外配置本地凭证。后台日志和运行状态位于数据库目录下的 `.router-*` 文件，权限为 0600；不会保存 API Key。

第一次升级到共享版本时，请先退出旧版 Router 及其 Claude 会话，然后再运行新版 `router claude`。旧版进程没有共享协议，占用端口时会明确报错；其他程序占用端口也不会被当作 Router 复用。配置的端口为 0 时，并发启动仍通过运行状态文件共享同一个实际端口。

也可独立启动代理：

```sh
router serve
```

`router serve` 在前台运行，也能被相同配置的 `router claude` 复用；服务生命周期由这个前台终端控制，启动器不会自动替换它。也可在另一个终端、业务项目目录直接运行：

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 \
ANTHROPIC_AUTH_TOKEN=local-router \
ANTHROPIC_MODEL=router-auto \
CLAUDE_CODE_GATEWAY_HINT_HEADERS=1 claude
```

`local-router` 只是固定占位值，不是密钥。推荐使用 `router claude`，它还会设置子代理模型、模型别名和上下文窗口，并清除子进程中冲突的 provider 环境变量。

## 上下文与自动压缩

启动器将 `router-auto` 的上下文容量设为两档 `context_tokens` 的较小值（`CLAUDE_CODE_MAX_CONTEXT_TOKENS`），自动压缩窗口仍取其 75%（`CLAUDE_CODE_AUTO_COMPACT_WINDOW`）。两档均填 1,000,000 时对应 750,000 的窗口；这些是配置值，不能代替供应商实际容量验证。Claude Code 会按自身版本、模型容量和有效配置处理窗口值。启动器保留用户的自动压缩开关；在 `/config` 查看 Auto-compact，`DISABLE_AUTO_COMPACT` / `DISABLE_COMPACT` 等环境变量也可能关闭压缩。

Router 本地估算超限现在返回 HTTP 400 / `invalid_request_error`，消息包含 `prompt is too long`，让 Claude Code 能识别并尝试自动压缩恢复；不再用通用 409 混淆上下文和内容能力错误。错误明确注明本地估算，并在请求详情的 `decision.context_budget` 中记录估算输入、输出预留、合计和两档配置上限。该预检查未派发模型，不产生该次上游费用；由客户端发起的后续压缩调用正常计费，Router 不自行重试。

内容能力 / 历史兼容冲突仍返回 409，详情错误码为 `selected_model_content_unsupported`；输出预留本身达到配置窗口则返回 `output_budget_exceeds_context`，需调整输出预算或配置，不能靠压缩历史解决。原始上游错误继续原样转发。本地 Claude Code 2.1.329 的隔离模拟对照中，旧 409 格式未自动压缩，新 400 格式触发一次自动压缩并完成后续回答；测试不调用真实模型，真实接口与其他客户端版本仍需实际确认。

## 与 CC Switch 共存及连接验证

CC Switch 切换供应商时可能把 `ANTHROPIC_BASE_URL`、认证和模型写入 `~/.claude/settings.json`。Claude Code 的配置文件 `env` 可以覆盖进程环境，因此仅设置启动环境不能保证请求进入 Router；退出 CC Switch 也不代表已保存配置消失。

`router claude` 现在会通过会话级 `--settings` 覆盖普通设置中的连接字段。无需删除 CC Switch 配置，也不用切换配置目录或禁用所有插件。关闭 Router 后，直接运行 `claude` 仍使用原来的配置。

启动后输入 `/status`：默认 `Anthropic base URL` 应显示 `http://127.0.0.1:8787`。发送一条测试问题并刷新统计页，应出现对应时间的请求。`http://localhost:8000` 属于上游反代地址；如果 `/status` 仍显示它且 Router 没有新增记录，当前请求仍未进入 Router。先确认使用的是新编译的 `router claude`；若企业 managed settings 强制指定了地址，需要按组织管理方式处理。

本地验证使用 Claude Code 2.1.328、隔离配置和模拟上游：旧方式请求进入配置文件中的地址；新启动器请求进入 Router 账本并完成转发，全局与项目设置内容均未变化。该测试不调用真实模型，也不代表所有 CC Switch / Claude Code 版本都已验证。

参考：[Claude Code 设置优先级](https://code.claude.com/docs/en/settings#settings-precedence)、[配置文件 env 与环境变量](https://code.claude.com/docs/en/llm-gateway-connect#set-in-a-settings-file)、[CC Switch 配置文件说明](https://github.com/farion1231/cc-switch/blob/main/docs/user-manual/en/5-faq/5.1-config-files.md)。

## 路由模式

| mode | 行为 |
| --- | --- |
| shadow | 默认。记录建议和候选成本，实际全部使用 Opus |
| auto | 每次推理重新评估，按质量建议、能力和缓存成本选择模型 |
| high | 固定 Opus，适合基线及接入验证 |
| low | 固定 Flash，适合基线及接入验证 |

`classifier.enabled` 默认 false。打开后，仅当规则不确定且两个模型仍可选择时，才额外调用 Flash 分类，输入有长度上限和超时，调用独立记账，不递归经过网关。分类器调用会产生真实费用；超时后不能假定供应商未收费。规则和置信度是初始启发式，不是训练或校准后的复杂度模型。

`policy.allow_text_switch` 默认 false。在真实上游双向接续验证通过后，可设置 true，放行普通文本和客户端工具历史切换。thinking 历史通常保留来源模型；auto 命中本地复杂任务规则 `complexity_rule` 或内容能力规则 `content_requires_high_capability` 时优先使用 high，可越过仅由 thinking / redacted_thinking 引起的历史锁（包括来源未知的恢复会话）。服务端工具等其他受保护状态仍保留来源；这些状态来源未知时返回 409。固定模式仍按指定模型执行。

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
- auto 命中本地复杂任务规则 `complexity_rule` 或内容能力规则 `content_requires_high_capability`（image / document / search_result）时，即使历史中有 Flash 的 thinking / redacted_thinking，也直接使用 high 并跳过付费分类器。对应越过限制理由为 `complexity_overrides_thinking` 或 `capability_overrides_thinking`。这项例外独立于 `allow_text_switch`，仍受高级模型上下文限制；工具失败信号、分类器建议或不确定回退本身不触发此例外。服务端工具等其他受保护状态继续限制切换；若来源 Flash 又不满足内容能力要求，则返回 409、不调用模型，不能通过历史兼容例外放行不支持的内容。
- 切换仅替换请求的模型字段，完整保留 thinking 签名、工具调用和结果。**路由放行不代表上游兼容性已验证**；如果高档接口拒绝异源 thinking，原样返回并记录错误，不自动重试或退回 Flash。普通任务仍遵守历史锁；换一个 prompt_id 不能清除历史。`/api/info` 和新请求详情中的 `routing_version: "context-recovery-v1"` 可用于确认已运行这版逻辑，既有 `policy.version` 配置不需要修改。
- 缓存前缀按 tools → system → message 内容块构建，支持消息增长、标记移动和未缓存尾部修改。显式缓存考虑断点及 20 块回看范围；自动缓存从完整响应的输入 usage 建立预测。各模型与分支隔离、TTL 过期失效，覆盖量由 usage 按前缀相对大小分摊，再保守打八折。未缓存尾部按普通输入费率估算。
- “路由诊断”默认展示本次 Router 启动产生的请求，可切换到全部历史；顶部显示当前生效的模式、分类器与文本历史切换开关。分类器结果、跳过原因和切换限制按所选范围统计，缓存预测值、实际值、漏预测次数和平均绝对误差按预测版本分别汇总。未知价格仍不影响请求和 token 占比。

v4 针对 Claude Code 交互模式补充了缓存元数据适配：已识别的 `safeguards[].type=dangerous_tool_use` / `classifier_context.v=1` 不参与生成提示的缓存指纹和输入长度估算。工具执行后追加的 `prior_turn_context` 因而不会把整个前缀判为变化。该字段仍逐字转发给上游；其他 safeguards 字段、未知类型和未来版本仍保守参与指纹。

逐请求明细分别显示请求类别（主请求、子代理、后台辅助、上下文压缩、分类器）和问题归属。缺少问题 ID 只表示“未关联问题”，不能据此推断请求类别。问题行 Opus 0 次不包含未关联或使用独立问题 ID 的其他请求；核对扣费要看同一时段的全部请求。“推荐 Opus；实际 Flash”表示一次请求的推荐和实际执行档位，不表示先后调用两个模型。分类器行单独显示其执行模型。

“全部历史”中的“分类器未启用”“文本历史切换已关闭”可能来自旧配置，不能据此判断当前开关；修改配置并重启不会清空账本。模型分类的“不确定或置信不足”会明确标记保守回退，不再显示成“规则无法确定”。格式无效的分类结果也保守回退，不会自动付费重试。

缓存表为空时会列出尚未派发、进行中、响应不完整或用量字段缺失的请求数量。缺失缓存字段仍保持未知，不按零计费或计入预测误差。

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

`GET /api/routing-health?scope=current` 返回本次启动的诊断；`scope=all` 返回完整历史。响应包含 `current_run_id`、`started_at`、`current_config` 和 `routing_health`。新推理和分类记录均保存由网关生成的 `run_id`，按完整运行标识隔离；旧版本无标识的记录只出现在历史诊断。`/api/summary`、费用、用量、问题列表和反馈仍覆盖完整账本，不随诊断范围改变。
