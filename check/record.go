package check

import "time"

// Record 是单个检查项的一次执行记录。
type Record struct {
	RunID     int
	CheckID   string
	Name      string
	Deps      []string
	Status    Status
	Code      string
	Message   string
	StartedAt time.Time
	EndedAt   time.Time
	Attempt   int
	// ReadStillOpen 区分检查已超时/取消，但底层读取直到报告产生时仍未退出。
	ReadStillOpen bool
	// CleanupPending 为 true 表示检查结论已出（如超时/取消），
	// 但其底层读取尚未收回，不能视为“完全结束”。
	// 可通过 Executor.InFlight / WaitIdle 观察与等待收尾进展。
	CleanupPending bool
	// Stale 表示当前结果已经被后续执行或上游变化废弃；历史报告仍保留当时结论。
	Stale             bool
	InvalidatedReason string
}

func (r Record) Duration() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// Report 是一轮自检（可由整轮或单项重跑产生）的不可变快照。
type Report struct {
	RunID         int
	DeviceID      string
	Model         string
	StartedAt     time.Time
	EndedAt       time.Time
	Overall       Status
	Records       []Record
	PlanBefore    *Plan
	PlanAfter     *Plan
	Invalidations []Invalidation
}

// Counts 汇总本轮报告中各状态的检查项数量。
func (r *Report) Counts() Counts {
	c := Counts{}
	for _, rec := range r.Records {
		c[rec.Status]++
	}
	return c
}

// Record 按检查 ID 查找记录。
func (r *Report) Record(id string) (Record, bool) {
	for _, rec := range r.Records {
		if rec.CheckID == id {
			return rec, true
		}
	}
	return Record{}, false
}

// PendingCleanup 返回本轮结论已出但资源尚未收回的检查项。
func (r *Report) PendingCleanup() []string {
	var ids []string
	for _, rec := range r.Records {
		if rec.CleanupPending {
			ids = append(ids, rec.CheckID)
		}
	}
	return ids
}
