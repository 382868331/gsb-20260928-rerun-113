package check

import "time"

// Record 是单个检查项的一次执行记录。
type Record struct {
	CheckID   string
	Name      string
	Deps      []string
	Status    Status
	Code      string
	Message   string
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
