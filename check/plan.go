package check

import (
	"encoding/json"
	"io"
	"sync"
)

// PlanItem 是条件检查计划中的一个条目。
// 计划执行前即可查看：每项为何加入（Reason）、当前状态与已有结论。
type PlanItem struct {
	CheckID string   `json:"check_id"`
	Name    string   `json:"name"`
	Deps    []string `json:"deps,omitempty"`
	Reason  string   `json:"reason"`
	Status  Status   `json:"status"`
	// Record 为已得出的结论；未结论时为 nil，Status 为 Pending。
	Record *Record `json:"record,omitempty"`
}

// Plan 是一轮自检的条件检查计划：初始由检查集播种，
// 执行过程中可按规则临时补充检查项（如某项失败触发补充检查）。
//
// 计划只增不改：同一 ID 再次加入时保留原有条目与结论，互不覆盖；
// 可保存为 JSON 后重开，重开合并且集，历史计划与后来新发现的检查项互不改写。
type Plan struct {
	mu    sync.RWMutex
	items []*PlanItem
	index map[string]*PlanItem
}

// NewPlan 构造空计划。
func NewPlan() *Plan {
	return &Plan{index: map[string]*PlanItem{}}
}

// Add 追加一个待执行条目（状态 Pending）。同 ID 已存在时不覆盖，返回 false。
func (p *Plan) Add(item PlanItem) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.index[item.CheckID]; ok {
		return false
	}
	if item.Status == "" {
		item.Status = Pending
	}
	cp := item
	p.items = append(p.items, &cp)
	p.index[cp.CheckID] = &cp
	return true
}

// Get 按检查 ID 查看计划条目。
func (p *Plan) Get(id string) (PlanItem, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	item, ok := p.index[id]
	if !ok {
		return PlanItem{}, false
	}
	return copyItem(item), true
}

// Items 返回计划全部条目的快照（按加入顺序），供查看/展示。
func (p *Plan) Items() []PlanItem {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]PlanItem, 0, len(p.items))
	for _, item := range p.items {
		out = append(out, copyItem(item))
	}
	return out
}

// Pending 返回尚未得出结论的条目；这些项不得被当成通过跳过。
func (p *Plan) Pending() []PlanItem {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []PlanItem
	for _, item := range p.items {
		if item.Record == nil {
			out = append(out, copyItem(item))
		}
	}
	return out
}

// Len 返回计划条目数。
func (p *Plan) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.items)
}

// complete 由执行器在结论产生时回填条目。
func (p *Plan) complete(rec Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, ok := p.index[rec.CheckID]
	if !ok {
		return
	}
	cp := rec
	item.Status = rec.Status
	item.Record = &cp
}

// restore 用于计划重开：条目不存在则追加；存在但本计划尚无结论而
// 对方已结论的，回填其结论。已有结论一律不被覆盖。
func (p *Plan) restore(item PlanItem) (added bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.index[item.CheckID]
	if !ok {
		cp := item
		if cp.Status == "" {
			cp.Status = Pending
		}
		p.items = append(p.items, &cp)
		p.index[cp.CheckID] = &cp
		return true
	}
	if cur.Record == nil && item.Record != nil {
		cp := *item.Record
		cur.Record = &cp
		cur.Status = item.Status
		cur.Reason = item.Reason
	}
	return false
}

// Merge 把另一份计划并入本计划：只追加/回填空缺，
// 双方已有结论的条目各自保留，互不覆盖。返回新加入的检查 ID。
func (p *Plan) Merge(other *Plan) []string {
	var added []string
	for _, item := range other.Items() {
		if p.restore(item) {
			added = append(added, item.CheckID)
		}
	}
	return added
}

type planJSON struct {
	Items []PlanItem `json:"items"`
}

// Save 把计划（含各项结论与依据）序列化为 JSON，供稍后重开。
func (p *Plan) Save(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(planJSON{Items: p.Items()})
}

// LoadPlan 从 JSON 重开一份计划。重开后可用 Merge/AdoptPlan 并入
// 当前执行器：历史结论保留，新发现的检查项追加，互不覆盖。
func LoadPlan(r io.Reader) (*Plan, error) {
	var pj planJSON
	if err := json.NewDecoder(r).Decode(&pj); err != nil {
		return nil, err
	}
	plan := NewPlan()
	for _, item := range pj.Items {
		plan.restore(item)
	}
	return plan, nil
}

func copyItem(item *PlanItem) PlanItem {
	cp := *item
	cp.Deps = append([]string(nil), item.Deps...)
	if item.Record != nil {
		rec := *item.Record
		cp.Record = &rec
	}
	return cp
}
