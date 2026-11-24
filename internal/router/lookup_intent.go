package router

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var lookupAction = regexp.MustCompile(`\b(find|list|locate|look up|which|what)\b`)
var lookupArtifact = regexp.MustCompile(`\b(table names?|tables|fields|constants|definitions?|file paths?|config(uration)? keys|event (keys|names)|function names?|class names?)\b`)
var lookupReasoning = regexp.MustCompile(`\b(` +
	`analy[sz]\w*|analysis|design\w*|review\w*|audit\w*|debug\w*|diagnos\w*|why|how|caus\w*|reason\w*|compar\w*|impact\w*|risk\w*|tradeoffs?|trade-offs?|recommend\w*|choos\w*|` +
	`deadlocks?|race conditions?|data races?|inconsisten\w*|conflict\w*|fail\w*|lose|losing|lost|security|permissions?|authentica\w*|authori[sz]\w*|attack\w*|inject\w*|vulnerabilit\w*|` +
	`fix\w*|implement\w*|optimi[sz]\w*|refactor\w*|migrat\w*|chang\w*|modif\w*|delet\w*|creat\w*|rewrit\w*|replac\w*|deploy\w*|validat\w*|verif\w*|prov\w*|ensur\w*|` +
	`explain\w*|summari[sz]\w*|acceptance|correctness|should|ought|need\w*|whether)\b`)

// Recognize only explicit retrieval of existing names/definitions. A topic noun
// such as "architecture" is not itself evidence of a design task. Mixed tasks
// and ambiguous wording fall through to the existing rules/classifier; this is
// a conservative heuristic, not a semantic parser or a history-switch override.
func isLookupTask(text string) bool {
	if text == "" || utf8.RuneCountInString(text) >= 1800 {
		return false
	}
	if containsLookupTerm(text, []string{
		"分析", "设计", "评估", "推断", "判断", "比较", "对比", "区别", "关系", "影响", "风险",
		"为什么", "为何", "原因", "根因", "排查", "诊断", "调试", "优化", "修复", "修改",
		"改造", "新增", "删除", "重构", "迁移", "实现", "验证", "证明", "保证", "确保",
		"检查", "审计", "审查", "是否", "怎么", "如何", "又错", "仍然失败",
		"导致", "死锁", "漏洞", "权限", "认证", "鉴权", "攻击", "注入", "权衡", "建议",
		// Retrieval verbs do not make a mixed request mechanical. These also
		// cover design questions phrased as "which fields should ... use" and
		// searches that require diagnosing a failure rather than finding a name.
		"验收", "重写", "改写", "改动", "替换", "部署", "上线", "发布", "补齐",
		"说明", "解释", "总结", "推导", "推演", "应该", "应当", "需要", "选型",
		"数据竞争", "竞态", "不一致", "冲突", "丢失", "隐患", "不安全", "故障", "异常",
	}) || lookupReasoning.MatchString(text) {
		return false
	}
	action := containsLookupTerm(text, []string{"查找", "查询", "查下", "查一下", "找出", "找到", "列出", "看下", "看一下", "哪些", "是什么", "在哪里"}) || lookupAction.MatchString(text)
	artifact := containsLookupTerm(text, []string{"表名", "埋点表", "字段", "常量", "定义", "文件路径", "配置项", "事件名", "埋点key", "埋点 key", "函数名", "类名"}) || lookupArtifact.MatchString(text)
	return action && artifact
}

func containsLookupTerm(text string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}
