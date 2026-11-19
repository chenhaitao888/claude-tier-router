# v3 路由修复与验证（2026-11-18）

四项修改已实现：任务文本与系统提醒分离、分类器结构校验及诊断、受保护历史提前锁定并跳过付费分类、内容块级缓存预测及显式汇率比较。验证时配置为 `auto` / `v3`，汇率按用户指定固定为 **1 USD = 6.8 CNY**。没有改动业务项目代码和原生产账本。

## 离线复查

从 一段本地 Claude Code 会话 的 request shape、消息 blob 和 transcript 还原 31 条主请求；所有被引用消息均匹配原哈希。使用原账本 usage、请求时间和实际执行模型推进缓存状态，不调用上游。

| 指标 | 原记录 | v3 离线复查 |
| --- | ---: | ---: |
| 主请求 | 31 | 31 |
| 实际缓存命中请求 | 30 | 同一批原 usage |
| 预测为零但实际命中 | 23 | 0 |
| 缓存读取预测总绝对误差 / token | 2,006,589 | 487,314 |
| 解释题的能力规则建议 | 原先未命中简单规则 | 4 条均建议 low |
| 并发题的能力规则建议 | high | 27 条均建议 high |

原请求共读取缓存 2,436,504 token，新预测为 1,949,190 token。保留了 20% 的保守折扣，仍然存在预测误差。这是**保持历史实际模型不变的缓存预测复查**，不能当作真实节省率；改变首条请求的模型后，后续回答、工具行为和缓存都会变化。

原轨迹有 30 条请求携带受保护历史，因此 v3 会保持来源模型。解释题首条可直接建议 Flash，后续能否切换仍取决于实际历史；不能把“4 条规则建议 low”解释为这 4 条历史请求都能安全切成 Flash。此轨迹中的主请求无需再调用付费分类器。

复查测试可用私有本地 fixture 重跑：

```sh
ROUTER_REPLAY_FIXTURE=/private/tmp/tier-router-v3-audit/replay.json \
  go test ./internal/router -run TestLocalCacheReplay -v
```

临时 fixture 含原请求内容，不纳入版本控制；默认测试跳过此项。

## 已授权的真实调用

使用相同的任务及代码上下文，对两个模型各做一次回答；不执行返回的工具或代码。上游采用用户现有 OpenRouter / DeepSeek 配置。账本和响应单独保存在 `data/v3-validation/`，未混入日常统计。

第一批共 5 次主调用及 1 次 Flash 分类调用。两次 Opus 请求因为测试参数 `thinking: disabled` 被端点拒绝，错误为“Reasoning is mandatory for this endpoint and cannot be disabled.”；Flash 并发题在 3,000 输出 token 处截断。用户另行授权 3 次主调用后，Opus 改为 adaptive/high，输出上限提高到 8,000；Flash 并发题保持非 thinking 模式、上限提高到 8,000。

两档的任务文本与代码上下文逐字一致，但 thinking 设置、回答长度和供应商缓存条件不同，因此本次是固定上下文的单步检查，**不是参数完全相同的多轮 Claude Code 对照实验**。

| 任务 / 模型 | 实际路由 | 耗时 | 已核算费用 | 结果 |
| --- | --- | ---: | ---: | --- |
| 解释 ExecuteActivity / Flash | auto → low | 5.92 秒 | ¥0.007419 | end_turn，输出 1,036 token |
| 解释 ExecuteActivity / Opus | fixed high | 28.41 秒 | $0.083540 | end_turn，输出 3,039 token |
| 并发分析 / Flash，补测 | fixed low | 17.09 秒 | ¥0.011817 | end_turn，输出 2,843 token；读取缓存 12,800 token |
| 并发分析 / Opus，补测 | auto → high | 85.48 秒 | $0.243296 | max_tokens；8,000 token 全部为 thinking，无正文 |
| 排序任务 / Flash | 分类器 → low | 2.01 秒 | ¥0.000049 + 分类 ¥0.000285 | 分类 accepted / validation ok，任务 end_turn |

本轮全部调用已核算合计 **$0.326836 + ¥0.044559**，包括最初截断的 Flash 回答。按 6.8 折算约 ¥2.2670。另有两次 HTTP 400 未返回 usage，费用保持未知，不假定免费。金额由配置价格及 usage 核算，尚未对账。

## 回答质量观察

解释题两档都生成了回答，仍需核对 SDK 签名、参数要求以及编译结果。并发题的复杂异常路径需要复现，不能把文字分析当作已验证结论。Opus 并发题本轮在输出上限处截断、没有有效正文，不能用 HTTP 200 或 thinking token 数量声称其质量胜出。私有业务代码、回答内容与项目缺陷细节不随本报告发布。

为避免误读统计，v3 另外记录 JSON/SSE 的 `stop_reason`，页面明确标记 `max_tokens` 和上下文上限。`complete` 继续表示传输完整，保留应计费用；质量仍由任务反馈判断。停止原因只对升级后的新记录采集，旧账本没有的信息不回填猜测。

## 工程验证与使用

`go test -race ./...`、`go vet ./...`、构建及配置校验通过。测试覆盖中文任务与提醒剥离、分类截断/缺字段/重复字段/围栏、低置信回退、历史来源未知、跳过分类、干净会话解锁、缓存标记移动/尾部变化/TTL/模型隔离、自动冷缓存、汇率快照、停止原因及费用。页面已检查数据加载、诊断数据、使用范围切换和 390px 布局。

退出原来的 router/Claude Code 后，仍在你的业务项目目录运行：

```sh
/path/to/claude-tier-router/bin/router claude \
  -config /path/to/claude-tier-router/config.json
```

分别用两个新会话测试解释题和并发题。仅换问题不会移除 thinking 历史；Flash 产生受保护历史后也不能直接切到 Opus。新会话或真正去除受保护历史的压缩边界才恢复切换资格。启动页面应显示 `自动路由 · v3`，路由诊断应显示汇率 6.8。

参考：[Anthropic 缓存规则](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)、[DeepSeek Anthropic 兼容范围](https://api-docs.deepseek.com/guides/anthropic_api/)。前者说明内容层级与断点回看；后者的 `output_config` 只支持 effort，分类器不依赖该接口尚不支持的 JSON schema 参数。
