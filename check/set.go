package check

import (
	"fmt"
	"sort"
)

// ErrCheckSet 描述检查集定义本身的问题（重复 ID、依赖缺失、成环）。
type ErrCheckSet struct{ msg string }

func (e *ErrCheckSet) Error() string { return e.msg }

// CheckSet 是某个型号提供的一组诊断项及其依赖关系。
type CheckSet struct {
	Model          string
	checks         map[string]Check
	conditional    map[string]bool
	conditionRules []ConditionalRule
}

// NewSet 构建并校验一个检查集。
func NewSet(model string, checks ...Check) (*CheckSet, error) {
	set := &CheckSet{Model: model, checks: make(map[string]Check, len(checks)), conditional: map[string]bool{}}
	for _, item := range checks {
		if item.ID() == "" {
			return nil, &ErrCheckSet{msg: "检查项 ID 不能为空"}
		}
		if _, exists := set.checks[item.ID()]; exists {
			return nil, &ErrCheckSet{msg: fmt.Sprintf("检查项 %q 重复定义", item.ID())}
		}
		set.checks[item.ID()] = item
	}
	for _, item := range checks {
		for _, dep := range item.Deps() {
			if _, ok := set.checks[dep]; !ok {
				return nil, &ErrCheckSet{msg: fmt.Sprintf("检查项 %q 依赖不存在的 %q", item.ID(), dep)}
			}
		}
	}
	return set, nil
}

// ConditionalCheck 包装一个默认不执行、由失败结果追加的补充检查。
type ConditionalCheck struct {
	Check
}

// NewConditionalCheck 标记一个检查项只能由条件规则加入计划。
func NewConditionalCheck(item Check) ConditionalCheck {
	return ConditionalCheck{Check: item}
}

// IsConditional 表明该检查由条件规则触发。
func (ConditionalCheck) IsConditional() bool { return true }

type conditionalMarker interface{ IsConditional() bool }

// RegisterConditionalRule 添加失败结果触发补充检查的规则。
func (s *CheckSet) RegisterConditionalRule(rule ConditionalRule) error {
	if _, ok := s.Get(rule.From); !ok {
		return &ErrCheckSet{msg: fmt.Sprintf("条件规则来源 %q 不存在", rule.From)}
	}
	if rule.Status == "" && rule.Code == "" && rule.Match == nil {
		return &ErrCheckSet{msg: fmt.Sprintf("条件规则 %q 必须指定状态、代码或匹配函数", rule.From)}
	}
	for _, id := range rule.Add {
		item, ok := s.Get(id)
		if !ok {
			return &ErrCheckSet{msg: fmt.Sprintf("条件规则追加的 %q 不存在", id)}
		}
		if marker, ok := item.(conditionalMarker); !ok || !marker.IsConditional() {
			return &ErrCheckSet{msg: fmt.Sprintf("条件规则追加的 %q 必须是条件检查", id)}
		}
		s.conditional[id] = true
	}
	s.conditionRules = append(s.conditionRules, rule)
	return nil
}

// Conditional 报告检查项是否默认不在基础计划中。
func (s *CheckSet) Conditional(id string) bool { return s.conditional[id] }

// Rules 返回条件追加规则。
func (s *CheckSet) Rules() []ConditionalRule {
	return append([]ConditionalRule(nil), s.conditionRules...)
}

// Get 按 ID 取检查项。
func (s *CheckSet) Get(id string) (Check, bool) {
	c, ok := s.checks[id]
	return c, ok
}

// IDs 返回按字典序排列的全部检查项 ID。
func (s *CheckSet) IDs() []string {
	ids := make([]string, 0, len(s.checks))
	for id := range s.checks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Order 返回满足依赖先后关系的执行顺序（Kahn 拓扑排序）。
// 依赖图成环时返回错误。
func (s *CheckSet) Order() ([]string, error) {
	indegree := make(map[string]int, len(s.checks))
	dependents := make(map[string][]string, len(s.checks))
	for id, item := range s.checks {
		indegree[id] = len(item.Deps())
		for _, dep := range item.Deps() {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	ready := make([]string, 0)
	for id, n := range indegree {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(s.checks))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string(nil), dependents[id]...)
		sort.Strings(next)
		for _, depID := range next {
			indegree[depID]--
			if indegree[depID] == 0 {
				ready = append(ready, depID)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(s.checks) {
		return nil, &ErrCheckSet{msg: fmt.Sprintf("型号 %q 的检查依赖存在环", s.Model)}
	}
	return order, nil
}
