package check

import "time"

// CleanupState 表示一次检查涉及的设备操作（在途读取等）在记录落定时是否已收回。
type CleanupState string

const (
	// CleanupDone 本次检查的设备操作已全部收回，资源真正收尾。
	CleanupDone CleanupState = "done"
	// CleanupPending 检查结论已出（如超时），但仍有在途操作未收回，
	// 不能视为完全结束；待其退出后由 Executor.PendingCleanup/WaitCleanup 跟踪。
	CleanupPending CleanupState = "pending"
)

// Record 是单个检查项的一次执行记录。
type Record struct {
	CheckID   string
	Name      string
	Deps      []string
	Status    Status
	Code      string
	Message   string
	Cleanup   CleanupState
	StartedAt time.Time
	EndedAt   time.Time
	Attempt   int
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

// PendingCleanup 返回本报告中结论已出但设备操作尚未收回的检查项。
func (r *Report) PendingCleanup() []string {
	var ids []string
	for _, rec := range r.Records {
		if rec.Cleanup == CleanupPending {
			ids = append(ids, rec.CheckID)
		}
	}
	return ids
}

// FullySettled 表示本轮报告涉及的全部设备操作都已收回，无任何在途残留。
func (r *Report) FullySettled() bool {
	return len(r.PendingCleanup()) == 0
}
