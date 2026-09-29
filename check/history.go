package check

import ("sync")

// History 保存历轮报告；重跑产生新报告，旧报告原样留存。
type History struct {
	mu      sync.RWMutex
	reports []*Report
}

// Save 追加一轮报告并回填其轮次编号。
func (h *History) Save(report *Report) {
	h.mu.Lock()
	defer h.mu.Unlock()
	report.RunID = len(h.reports) + 1
	h.reports = append(h.reports, report)
}

// Latest 返回最近一轮报告。
func (h *History) Latest() (*Report, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.reports) == 0 {
		return nil, false
	}
	return h.reports[len(h.reports)-1], true
}

// Get 按轮次编号（从 1 开始）取报告。
func (h *History) Get(runID int) (*Report, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if runID < 1 || runID > len(h.reports) {
		return nil, false
	}
	return h.reports[runID-1], true
}

// Len 返回已保存的报告轮数。
func (h *History) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.reports)
}

