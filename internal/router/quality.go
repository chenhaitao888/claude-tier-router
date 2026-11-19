package router

import (
	"errors"
	"fmt"
	"strings"
)

type Criterion struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
}
type Rubric struct {
	ID       string      `json:"id"`
	Version  string      `json:"version"`
	Label    string      `json:"label"`
	Criteria []Criterion `json:"criteria"`
}
type QualityCheck struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Evidence string `json:"evidence"`
}

func QualityRubrics() []Rubric {
	return []Rubric{
		{ID: "api_explanation", Version: "1", Label: "函数 / API 解释", Criteria: []Criterion{
			{ID: "contract", Label: "签名与参数约束正确", Hint: "记录对应版本的源码或官方文档位置；区分编译期检查与运行时校验。"},
			{ID: "semantics", Label: "超时、重试、取消与错误语义正确", Hint: "逐项核对文档或实现，不能只依据回答自述。"},
			{ID: "example", Label: "示例通过验证", Hint: "记录实际执行的编译或测试命令及结果；未运行选未确定。"},
			{ID: "references", Label: "引用与当前项目一致", Hint: "记录文件、行号或版本，核对调用与注册是否对应。"},
		}},
		{ID: "concurrency_analysis", Version: "1", Label: "并发 / 死锁分析", Criteria: []Criterion{
			{ID: "path", Label: "触发条件与等待路径可追溯", Hint: "记录具体调用链、锁 / 管道两端和等待的资源。"},
			{ID: "failure_paths", Label: "覆盖异常退出与取消路径", Hint: "核对 panic、断流、超时和资源释放；不能只检查正常路径。"},
			{ID: "reproduction", Label: "关键风险有复现或测试证据", Hint: "记录复现条件、测试命令与结果；只有静态推断选未确定。"},
			{ID: "claims", Label: "结论范围与证据一致", Hint: "区分已复现、条件风险与尚未排除，不把未发现当作不存在。"},
		}},
		{ID: "code_change", Version: "1", Label: "代码修改", Criteria: []Criterion{
			{ID: "behavior", Label: "需求行为得到验证", Hint: "记录可复查的输入、预期行为和实际结果。"},
			{ID: "tests", Label: "相关测试通过", Hint: "记录测试命令、结果和代码版本；未执行选未确定。"},
			{ID: "regression", Label: "关键边界未回归", Hint: "记录错误、取消、并发或兼容性等相关检查。"},
		}},
	}
}

// Evidence is supplied by the reviewer, not automatically asserted to be true.
// A missing check or unrun test must never silently become a passing task.
func (f *Feedback) validateQuality() error {
	if f.Rubric == "" {
		if len(f.Checks) > 0 || f.RubricVersion != "" {
			return errors.New("rubric required for evidence checks")
		}
		if f.Outcome != "passed" && f.Outcome != "failed" && f.Outcome != "unknown" {
			return errors.New("outcome must be passed, failed or unknown")
		}
		return nil
	}
	var rubric *Rubric
	for _, candidate := range QualityRubrics() {
		if candidate.ID == f.Rubric {
			copy := candidate
			rubric = &copy
			break
		}
	}
	if rubric == nil {
		return errors.New("unknown quality rubric")
	}
	if f.RubricVersion != "" && f.RubricVersion != rubric.Version {
		return errors.New("rubric version mismatch; reload criteria")
	}
	if len(f.Checks) != len(rubric.Criteria) {
		return errors.New("every rubric criterion needs an outcome")
	}
	required := map[string]bool{}
	for _, c := range rubric.Criteria {
		required[c.ID] = true
	}
	outcome := "passed"
	for i := range f.Checks {
		check := &f.Checks[i]
		if !required[check.ID] {
			return errors.New("unknown or duplicate criterion")
		}
		delete(required, check.ID)
		check.Evidence = strings.TrimSpace(check.Evidence)
		if len(check.Evidence) > 2000 {
			return errors.New("criterion evidence max 2000 bytes")
		}
		switch check.Outcome {
		case "failed":
			outcome = "failed"
		case "unknown":
			if outcome != "failed" {
				outcome = "unknown"
			}
		case "passed":
		default:
			return errors.New("criterion outcome must be passed, failed or unknown")
		}
		if check.Outcome != "unknown" && check.Evidence == "" {
			return fmt.Errorf("evidence required for %s", check.ID)
		}
	}
	if f.Outcome != "" && f.Outcome != outcome {
		return errors.New("task outcome conflicts with criterion outcomes")
	}
	f.Outcome = outcome
	f.RubricVersion = rubric.Version
	return nil
}
