package check

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Rule 是一条条件计划规则：当 Trigger 检查得出匹配结论时，
// 把 Adds 中的补充检查项追加进执行计划，并以 Reason 说明加入原因。
// Status 为空表示匹配任何非 passed 结论；Code 为空表示不限定具体代码。
type Rule struct {
	Trigger string
	Status  Status
	Code    string
	Adds    []Check
	Reason  string
}

// matches 判断一条检查记录是否触发本规则。
func (r Rule) matches(rec Record) bool {
	if r.Status != "" {
		if rec.Status != r.Status {
			return false
		}
	} else if rec.Status == Passed {
		return false
	}
	if r.Code != "" && rec.Code != r.Code {
		return false
	}
	return true
}

// Evidence 是补充检查加入计划瞬间、其所依赖检查结论的快照，
// 用于事后解释“当时凭什么把该项加入计划”。
type Evidence struct {
	CheckID string `json:"check_id"`
	Status  Status `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func evidenceOf(rec Record) Evidence {
	return Evidence{CheckID: rec.CheckID, Status: rec.Status, Code: rec.Code, Message: rec.Message}
}

// PlanItem 是计划中的一项：检查 ID、加入原因、加入时的依据快照。
type PlanItem struct {
	CheckID  string     `json:"check_id"`
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence,omitempty"`
	Since    int        `json:"since"` // 首次出现的计划版本号
}

// Plan 是某一版本的执行计划快照，生成后不可变。
type Plan struct {
	Version int        `json:"version"`
	Items   []PlanItem `json:"items"`
}

// Item 按检查 ID 查找计划项。
func (p *Plan) Item(id string) (PlanItem, bool) {
	for _, it := range p.Items {
		if it.CheckID == id {
			return it, true
		}
	}
	return PlanItem{}, false
}

// PlanLog 以追加方式保存计划历史：每次调整生成一个新版本，
// 旧版本永不改写，可随时取回任意历史版本与当前版本对比。
type PlanLog struct {
	mu    sync.RWMutex
	items map[string]PlanItem // 当前计划全集（按 CheckID）
	order []string            // 加入顺序
	plans []*Plan             // 全部历史版本，plans[0] 为版本 1
}

// NewPlanLog 以初始检查集创建计划日志（版本 1，原因均为“初始检查集”）。
func NewPlanLog(initialIDs []string) *PlanLog {
	l := &PlanLog{items: map[string]PlanItem{}}
	items := make([]PlanItem, 0, len(initialIDs))
	for _, id := range initialIDs {
		items = append(items, PlanItem{CheckID: id, Reason: "初始检查集", Since: 1})
	}
	l.append(items)
	return l
}

// append 把一批新计划项并入当前计划并生成新版本（重复 ID 自动忽略）。
func (l *PlanLog) append(items []PlanItem) *Plan {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(items)
}

func (l *PlanLog) appendLocked(items []PlanItem) *Plan {
	version := len(l.plans) + 1
	for _, it := range items {
		if it.Since == 0 {
			it.Since = version
		}
		if _, dup := l.items[it.CheckID]; dup {
			continue
		}
		l.items[it.CheckID] = it
		l.order = append(l.order, it.CheckID)
	}
	snap := &Plan{Version: version, Items: make([]PlanItem, 0, len(l.order))}
	for _, id := range l.order {
		snap.Items = append(snap.Items, l.items[id])
	}
	l.plans = append(l.plans, snap)
	return snap
}

// Current 返回当前（最新）计划版本。
func (l *PlanLog) Current() *Plan {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.plans) == 0 {
		return &Plan{}
	}
	return l.plans[len(l.plans)-1]
}

// Get 按版本号（从 1 开始）取历史计划。
func (l *PlanLog) Get(version int) (*Plan, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if version < 1 || version > len(l.plans) {
		return nil, false
	}
	return l.plans[version-1], true
}

// Len 返回计划版本数。
func (l *PlanLog) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.plans)
}

// planLogJSON 是 PlanLog 的持久化形式。
type planLogJSON struct {
	Plans []*Plan `json:"plans"`
}

// Save 把全部计划版本写入 path（JSON），旧版本一并保存，不会被截断。
func (l *PlanLog) Save(path string) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	data, err := json.MarshalIndent(planLogJSON{Plans: l.plans}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadPlanLog 从 path 读回计划日志，恢复全部历史版本，
// 之后可继续追加新版本（版本号接续，不覆盖已读回的历史）。
func LoadPlanLog(path string) (*PlanLog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var payload planLogJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("解析计划日志 %s: %w", path, err)
	}
	l := &PlanLog{items: map[string]PlanItem{}}
	for _, plan := range payload.Plans {
		if plan == nil {
			return nil, fmt.Errorf("解析计划日志 %s: 存在空版本", path)
		}
		l.plans = append(l.plans, plan)
	}
	if len(l.plans) > 0 {
		cur := l.plans[len(l.plans)-1]
		for _, it := range cur.Items {
			l.items[it.CheckID] = it
			l.order = append(l.order, it.CheckID)
		}
	}
	return l, nil
}

// PlanDiff 描述两个计划版本之间的差别。
type PlanDiff struct {
	From    int
	To      int
	Added   []PlanItem // 新版本中多出的项（含加入原因与当时依据）
	Removed []PlanItem // 新版本中不再出现的项
}

// DiffPlans 比较两个计划版本，解释从 old 到 cur 计划发生了什么变化。
func DiffPlans(old, cur *Plan) PlanDiff {
	diff := PlanDiff{}
	if old != nil {
		diff.From = old.Version
	}
	if cur != nil {
		diff.To = cur.Version
	}
	inOld := map[string]bool{}
	if old != nil {
		for _, it := range old.Items {
			inOld[it.CheckID] = true
		}
	}
	inCur := map[string]bool{}
	if cur != nil {
		for _, it := range cur.Items {
			inCur[it.CheckID] = true
			if !inOld[it.CheckID] {
				diff.Added = append(diff.Added, it)
			}
		}
	}
	if old != nil {
		for _, it := range old.Items {
			if !inCur[it.CheckID] {
				diff.Removed = append(diff.Removed, it)
			}
		}
	}
	return diff
}

// String 以可读形式说明计划差别，包含每项加入原因与当时依赖的检查结论。
func (d PlanDiff) String() string {
	if len(d.Added) == 0 && len(d.Removed) == 0 {
		return fmt.Sprintf("计划 v%d → v%d：无变化", d.From, d.To)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "计划 v%d → v%d：", d.From, d.To)
	for _, it := range d.Added {
		fmt.Fprintf(&b, "\n  + %s（%s，自 v%d 起）", it.CheckID, it.Reason, it.Since)
		for _, ev := range it.Evidence {
			fmt.Fprintf(&b, "\n      依据: %s=%s/%s %s", ev.CheckID, ev.Status, ev.Code, ev.Message)
		}
	}
	for _, it := range d.Removed {
		fmt.Fprintf(&b, "\n  - %s（%s）", it.CheckID, it.Reason)
	}
	return b.String()
}

// Invalidation 描述一条已失效的旧结论及其失效原因。
type Invalidation struct {
	CheckID string
	Reason  string
}

// invalidatedSorted 把失效表整理为按 ID 排序的切片，便于稳定展示。
func invalidatedSorted(stale map[string]string) []Invalidation {
	ids := make([]string, 0, len(stale))
	for id := range stale {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Invalidation, 0, len(ids))
	for _, id := range ids {
		out = append(out, Invalidation{CheckID: id, Reason: stale[id]})
	}
	return out
}
