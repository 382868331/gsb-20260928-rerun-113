package check

// Status 表示一次检查（或一轮自检整体）的结论。
type Status string

const (
	// Passed 检查通过。
	Passed Status = "passed"
	// Failed 检查执行完成，但读数等业务结果不满足要求。
	Failed Status = "failed"
	// Error 检查无法完成，例如传感器缺失、读取报错。
	Error Status = "error"
	// Blocked 前置检查未通过，本检查无法执行；不能计为通过。
	Blocked Status = "blocked"
	// Timeout 检查超过时限。
	Timeout Status = "timeout"
	// Canceled 自检被取消，检查尚未完成。
	Canceled Status = "canceled"
	// Pending 检查已列入计划但尚未得出任何结论；不得当作通过跳过。
	Pending Status = "pending"
	// Stale 检查重跑后，旧的相关结论已过期（仅保留在历史报告中）。
	Stale Status = "stale"
)

// Counts 按状态汇总检查项数量。
type Counts map[Status]int

// rank 给出整体结论的严重程度优先级，数值越大越严重。
func (s Status) rank() int {
	switch s {
	case Passed:
		return 0
	case Blocked, Stale, Pending:
		return 1
	case Failed:
		return 2
	case Timeout:
		return 3
	case Canceled:
		return 4
	case Error:
		return 5
	default:
		return 6
	}
}

// terminal 表示检查自身已得出确定结论。
func (s Status) terminal() bool {
	switch s {
	case Passed, Failed, Error, Blocked, Timeout, Canceled:
		return true
	default:
		return false
	}
}
