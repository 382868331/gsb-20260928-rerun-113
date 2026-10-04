package check

import (
	"encoding/json"
	"io"
	"os"
	"time"
)

// PersistedState 是执行器可重新打开的完整当前状态。历史批次单独保留，不被计划调整覆盖。
type PersistedState struct {
	DeviceID string         `json:"device_id"`
	Model    string         `json:"model"`
	SavedAt  time.Time      `json:"saved_at"`
	Records  []Record       `json:"records"`
	Attempts map[string]int `json:"attempts"`
	Plan     *Plan          `json:"plan"`
}

// SaveState 将当前结论、计划与执行次数写入 JSON。历史报告由 History.SaveState 保存。
func (e *Executor) SaveState(w io.Writer) error {
	e.mu.RLock()
	state := PersistedState{
		DeviceID: e.device.ID(),
		Model:    e.device.Model(),
		SavedAt:  time.Now(),
		Records:  make([]Record, 0, len(e.records)),
		Attempts: cloneIntMap(e.attempt),
		Plan:     clonePlan(e.plan),
	}
	for _, rec := range e.records {
		state.Records = append(state.Records, rec)
	}
	e.mu.RUnlock()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(state)
}

// SaveStateFile 保存到指定路径。
func (e *Executor) SaveStateFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return e.SaveState(f)
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneEvidence(in []ResultEvidence) []ResultEvidence {
	if in == nil {
		return nil
	}
	return append([]ResultEvidence(nil), in...)
}

func cloneEntry(in PlanEntry) PlanEntry {
	out := in
	if in.Trigger != nil {
		v := *in.Trigger
		out.Trigger = &v
	}
	out.Dependencies = cloneEvidence(in.Dependencies)
	return out
}

func clonePlan(in *Plan) *Plan {
	if in == nil {
		return nil
	}
	out := &Plan{Revision: in.Revision}
	for _, entry := range in.Entries {
		out.Entries = append(out.Entries, cloneEntry(entry))
	}
	for _, rev := range in.History {
		cp := PlanRevision{ID: rev.ID, At: rev.At, Reason: rev.Reason}
		for _, entry := range rev.Added {
			cp.Added = append(cp.Added, cloneEntry(entry))
		}
		cp.Invalidated = append([]Invalidation(nil), rev.Invalidated...)
		out.History = append(out.History, cp)
	}
	return out
}

func (h *History) reportsLocked() []*Report {
	out := make([]*Report, 0, len(h.reports))
	for _, report := range h.reports {
		r := &Report{
			RunID:         report.RunID,
			DeviceID:      report.DeviceID,
			Model:         report.Model,
			StartedAt:     report.StartedAt,
			EndedAt:       report.EndedAt,
			Overall:       report.Overall,
			Records:       append([]Record(nil), report.Records...),
			PlanBefore:    clonePlan(report.PlanBefore),
			PlanAfter:     clonePlan(report.PlanAfter),
			Invalidations: append([]Invalidation(nil), report.Invalidations...),
		}
		out = append(out, r)
	}
	return out
}

// SaveState 保存全部不可变历史批次。
func (h *History) SaveState(w io.Writer) error {
	h.mu.RLock()
	reports := h.reportsLocked()
	h.mu.RUnlock()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(reports)
}

// LoadState 从 JSON 恢复历史；不会与已有历史合并。
func (h *History) LoadState(r io.Reader) error {
	var reports []*Report
	if err := json.NewDecoder(r).Decode(&reports); err != nil {
		return err
	}
	h.mu.Lock()
	h.reports = reports
	h.mu.Unlock()
	return nil
}

// RestoreExecutor 使用保存的当前状态恢复执行器。跨进程后无法继续等待旧 goroutine，
// ReadStillOpen 会保留“检查超时但读取退出未知”的语义。
func RestoreExecutor(device Device, set *CheckSet, history *History, state PersistedState) (*Executor, error) {
	if state.Model != "" && state.Model != device.Model() {
		return nil, errStateMismatch("设备型号", state.Model, device.Model())
	}
	if set.Model != device.Model() {
		return nil, errStateMismatch("检查集型号", set.Model, device.Model())
	}
	e, err := NewExecutor(device, set, history)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = map[string]Record{}
	for _, rec := range state.Records {
		if _, ok := set.Get(rec.CheckID); !ok {
			continue
		}
		// 进程重启后原读取句柄已脱离本执行器管理：保留“已超时、退出未知”的事实，
		// 但不冒充当前仍可等待的在途读取。
		rec.CleanupPending = false
		e.records[rec.CheckID] = rec
	}
	if state.Attempts != nil {
		e.attempt = cloneIntMap(state.Attempts)
	} else {
		for _, rec := range state.Records {
			if rec.Attempt > e.attempt[rec.CheckID] {
				e.attempt[rec.CheckID] = rec.Attempt
			}
		}
	}
	if state.Plan != nil {
		e.plan = clonePlan(state.Plan)
	}
	return e, nil
}

func errStateMismatch(kind, saved, actual string) error {
	return &ErrCheckSet{msg: kind + "不匹配：保存值 " + saved + "，当前值 " + actual}
}

// LoadExecutorState 读取执行器状态 JSON。
func LoadExecutorState(r io.Reader) (PersistedState, error) {
	var state PersistedState
	err := json.NewDecoder(r).Decode(&state)
	return state, err
}

// CurrentPlan 返回当前计划副本。
func (e *Executor) CurrentPlan() *Plan {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return clonePlan(e.plan)
}

// ComparePlan 比较当前计划与指定历史批次时的计划。
func (e *Executor) ComparePlan(runID int) (PlanDifference, bool) {
	report, ok := e.history.Get(runID)
	if !ok || report.PlanAfter == nil {
		return PlanDifference{}, false
	}
	return DiffPlans(report.PlanAfter, e.CurrentPlan()), true
}
