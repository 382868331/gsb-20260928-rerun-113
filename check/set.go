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
	Model  string
	checks map[string]Check
}

// NewSet 构建并校验一个检查集。
func NewSet(model string, checks ...Check) (*CheckSet, error) {
	set := &CheckSet{Model: model, checks: make(map[string]Check, len(checks))}
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
	order, _, err := topoOrder(s.checks)
	if err != nil {
		return nil, &ErrCheckSet{msg: fmt.Sprintf("型号 %q 的检查依赖存在环", s.Model)}
	}
	return order, err
}

// topoOrder 对给定检查表做 Kahn 拓扑排序，同时返回每个检查的直接下游表。
// 依赖图成环时返回错误。
func topoOrder(checks map[string]Check) ([]string, map[string][]string, error) {
	indegree := make(map[string]int, len(checks))
	dependents := make(map[string][]string, len(checks))
	for id, item := range checks {
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
	order := make([]string, 0, len(checks))
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
	if len(order) != len(checks) {
		return nil, nil, &ErrCheckSet{msg: "检查依赖存在环"}
	}
	return order, dependents, nil
}
