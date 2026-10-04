package check

import (
	"fmt"
	"sort"
	"time"
)

// PlanOrigin 说明一个检查项为什么会进入当前计划。
type PlanOrigin string

const (
	PlanInitial     PlanOrigin = "initial"
	PlanConditional PlanOrigin = "conditional"
	PlanRerun       PlanOrigin = "rerun"
)

// ResultEvidence 是触发计划调整时依赖的检查结果快照。
type ResultEvidence struct {
	CheckID string    `json:"check_id"`
	Attempt int       `json:"attempt"`
	RunID   int       `json:"run_id"`
	Status  Status    `json:"status"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
	EndedAt time.Time `json:"ended_at"`
}

// PlanEntry 记录检查项加入计划的原因和当时依赖的检查结果。
type PlanEntry struct {
	CheckID      string           `json:"check_id"`
	Origin       PlanOrigin       `json:"origin"`
	Reason       string           `json:"reason"`
	Trigger      *ResultEvidence  `json:"trigger,omitempty"`
	Dependencies []ResultEvidence `json:"dependencies,omitempty"`
	AddedAt      time.Time        `json:"added_at"`
	RunID        int              `json:"run_id"`
	Revision     int              `json:"revision"`
}

// Invalidation 表示上游结果改变后，旧结论已经不能继续复用。
type Invalidation struct {
	CheckID         string    `json:"check_id"`
	Because         string    `json:"because"`
	Reason          string    `json:"reason"`
	Previous        Status    `json:"previous_status"`
	PreviousCode    string    `json:"previous_code"`
	PreviousAttempt int       `json:"previous_attempt"`
	PreviousRun     int       `json:"previous_run"`
	At              time.Time `json:"invalidated_at"`
}

// PlanRevision 是一次不可变计划调整；执行记录不会被它覆盖。
type PlanRevision struct {
	ID          int            `json:"id"`
	At          time.Time      `json:"at"`
	Reason      string         `json:"reason"`
	Added       []PlanEntry    `json:"added"`
	Invalidated []Invalidation `json:"invalidated,omitempty"`
}

// Plan 保存当前计划及其完整修订历史。
type Plan struct {
	Revision int            `json:"revision"`
	Entries  []PlanEntry    `json:"entries"`
	History  []PlanRevision `json:"history"`
}

func newPlan(initial []Check, at time.Time) *Plan {
	p := &Plan{Revision: 1}
	entries := make([]PlanEntry, 0, len(initial))
	for _, item := range initial {
		entries = append(entries, PlanEntry{
			CheckID:  item.ID(),
			Origin:   PlanInitial,
			Reason:   "型号基础检查项",
			AddedAt:  at,
			Revision: 1,
		})
	}
	p.Entries = entries
	p.History = append(p.History, PlanRevision{ID: 1, At: at, Reason: "初始化基础检查计划", Added: entries})
	return p
}

func (p *Plan) has(id string) bool {
	for _, entry := range p.Entries {
		if entry.CheckID == id {
			return true
		}
	}
	return false
}

func (p *Plan) add(entries []PlanEntry, reason string, invalidated []Invalidation) []PlanEntry {
	if len(entries) == 0 && len(invalidated) == 0 {
		return nil
	}
	p.Revision++
	rev := PlanRevision{ID: p.Revision, At: time.Now(), Reason: reason, Invalidated: invalidated}
	for i := range entries {
		entries[i].Revision = p.Revision
		rev.Added = append(rev.Added, entries[i])
		p.Entries = append(p.Entries, entries[i])
	}
	p.History = append(p.History, rev)
	return entries
}

// ConditionalRule 描述某检查结果出现后需要追加的补充检查。
type ConditionalRule struct {
	From   string
	Status Status
	Code   string
	Add    []string
	Reason string
	Match  func(Record) bool
}

func (r ConditionalRule) matches(rec Record) bool {
	if r.From != "" && r.From != rec.CheckID {
		return false
	}
	if r.Status != "" && r.Status != rec.Status {
		return false
	}
	if r.Code != "" && r.Code != rec.Code {
		return false
	}
	if r.Match != nil && !r.Match(rec) {
		return false
	}
	return true
}

// PlanDifference 描述两个计划版本之间的差别。
type PlanDifference struct {
	FromRevision int
	ToRevision   int
	Added        []PlanEntry
	Removed      []PlanEntry
	Invalidated  []Invalidation
}

// DiffPlans 比较两个历史计划。条件计划当前只追加，但重开/旧版本比较仍保留删除语义。
func DiffPlans(from, to *Plan) PlanDifference {
	if from == nil || to == nil {
		return PlanDifference{}
	}
	diff := PlanDifference{FromRevision: from.Revision, ToRevision: to.Revision}
	fromSet := map[string]PlanEntry{}
	toSet := map[string]PlanEntry{}
	for _, entry := range from.Entries {
		fromSet[entry.CheckID] = entry
	}
	for _, entry := range to.Entries {
		toSet[entry.CheckID] = entry
	}
	for _, entry := range to.Entries {
		if _, ok := fromSet[entry.CheckID]; !ok {
			diff.Added = append(diff.Added, entry)
		}
	}
	for _, entry := range from.Entries {
		if _, ok := toSet[entry.CheckID]; !ok {
			diff.Removed = append(diff.Removed, entry)
		}
	}
	seen := map[string]bool{}
	for _, rev := range to.History {
		if rev.ID <= from.Revision {
			continue
		}
		for _, inv := range rev.Invalidated {
			key := fmt.Sprintf("%s:%s", inv.CheckID, inv.Because)
			if !seen[key] {
				seen[key] = true
				diff.Invalidated = append(diff.Invalidated, inv)
			}
		}
	}
	sort.Slice(diff.Added, func(i, j int) bool { return diff.Added[i].CheckID < diff.Added[j].CheckID })
	sort.Slice(diff.Removed, func(i, j int) bool { return diff.Removed[i].CheckID < diff.Removed[j].CheckID })
	return diff
}

func planInvalidationsSince(from, to *Plan) []Invalidation {
	if from == nil || to == nil {
		return nil
	}
	var out []Invalidation
	for _, rev := range to.History {
		if rev.ID <= from.Revision {
			continue
		}
		out = append(out, rev.Invalidated...)
	}
	return out
}

func planInvalidationsAfter(plan *Plan, revision int) []Invalidation {
	if plan == nil {
		return nil
	}
	var out []Invalidation
	for _, rev := range plan.History {
		if rev.ID <= revision {
			continue
		}
		out = append(out, rev.Invalidated...)
	}
	return out
}
