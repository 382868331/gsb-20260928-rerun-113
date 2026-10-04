package check

import "time"

// Record 是单个检查项的一次执行记录。
type Record struct {
	CheckID   string    `json:"check_id"`
	Name      string    `json:"name"`
	Deps      []string  `json:"deps,omitempty"`
	Status    Status    `json:"status"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Attempt   int       `json:"attempt"`
	// Basis 记录本结论所依赖的上游检查项及其当时的执行次数（Attempt），
	// 即“结论的依据”。上游再次执行后，依据不再匹配的下游结论即为过期。
	Basis map[string]int `json:"basis,omitempty"`
	// CleanupPending 为 true 表示检查结论已出（如超时/取消），
	// 但其底层读取尚未收回，不能视为“完全结束”。
	// 可通过 Executor.InFlight / WaitIdle 观察与等待收尾进展。
	CleanupPending bool `json:"cleanup_pending,omitempty"`
}

func (r Record) Duration() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// Report 是一轮自检（可由整轮或单项重跑产生）的不可变快照。
type Report struct {
	RunID     int
	DeviceID  string
	Model     string
	StartedAt time.Time
	EndedAt   time.Time
	Overall   Status
	Records   []Record
	// Invalidated 列出本轮中因上游重新执行而过期的既有下游结论
	// （过期后本轮可能已重测，也可能仍待重测，见 Executor.Stale）。
	Invalidated []string
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
