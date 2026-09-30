package check

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultTimeout 是检查项未单独设置超时时的默认时限。
const DefaultTimeout = 2 * time.Second

// Executor 按依赖关系执行检查集，维护当前结论与历史报告。
type Executor struct {
	device  Device
	set     *CheckSet
	order   []string
	history *History

	mu      sync.RWMutex
	records map[string]Record
	attempt map[string]int
}

// NewExecutor 为某台设备及其型号检查集构建执行器。
func NewExecutor(device Device, set *CheckSet, history *History) (*Executor, error) {
	if set.Model != device.Model() {
		return nil, fmt.Errorf("检查集型号 %q 与设备型号 %q 不匹配", set.Model, device.Model())
	}
	order, err := set.Order()
	if err != nil {
		return nil, err
	}
	if history == nil {
		history = &History{}
	}
	return &Executor{
		device:  device,
		set:     set,
		order:   order,
		history: history,
		records: map[string]Record{},
		attempt: map[string]int{},
	}, nil
}

// RunAll 在给定 ctx 下执行整轮自检；ctx 取消时后续项记为 canceled。
func (e *Executor) RunAll(ctx context.Context) (*Report, error) {
	return e.run(ctx, e.order)
}

// Rerun 重跑指定的失败检查及其受影响下游；可一次选择多个失败项。
// 与所选检查无关的既有结论保持不变；旧报告仍原样保留在历史中。
func (e *Executor) Rerun(ctx context.Context, checkIDs ...string) (*Report, error) {
	if len(checkIDs) == 0 {
		return nil, fmt.Errorf("未指定需要重跑的检查项")
	}
	roots := map[string]bool{}
	for _, checkID := range checkIDs {
		if _, ok := e.set.Get(checkID); !ok {
			return nil, fmt.Errorf("检查项 %q 不存在", checkID)
		}
		roots[checkID] = true
	}
	affected := map[string]bool{}
	for root := range roots {
		for _, id := range e.descendants(root) {
			affected[id] = true
		}
	}
	targets := make([]string, 0, len(affected))
	for _, id := range e.order {
		if affected[id] {
			targets = append(targets, id)
		}
	}
	return e.run(ctx, targets)
}

// descendants 返回包含 root 自身在内、依赖链可达的全部检查项，按执行顺序排列。
func (e *Executor) descendants(root string) []string {
	affected := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		queue = queue[1:]
		for _, id := range e.order {
			if affected[id] {
				continue
			}
			c, _ := e.set.Get(id)
			for _, dep := range c.Deps() {
				if affected[dep] {
					affected[id] = true
					queue = append(queue, id)
					break
				}
			}
		}
	}
	out := make([]string, 0, len(affected))
	for _, id := range e.order {
		if affected[id] {
			out = append(out, id)
		}
	}
	return out
}

func (e *Executor) run(parent context.Context, targets []string) (*Report, error) {
	target := map[string]bool{}
	for _, id := range targets {
		target[id] = true
	}
	started := time.Now()
	for _, id := range e.order {
		if !target[id] {
			continue
		}
		item, _ := e.set.Get(id)
		rec := Record{
			CheckID:   id,
			Name:      item.Name(),
			Deps:      append([]string(nil), item.Deps()...),
			StartedAt: time.Now(),
		}

		// 1) 整轮已被取消/超时：尚未执行的项分别留痕。
		if err := parent.Err(); err != nil {
			rec.EndedAt = time.Now()
			rec.Status, rec.Code, rec.Message = fromContextErr(err)
			e.store(rec)
			continue
		}

		// 2) 前置条件：任一前置未通过（失败/故障/阻塞/超时/取消/未执行）则不得执行，
		//    也绝不能按通过处理。
		if blocked, why := e.blockedBy(item.Deps()); blocked != "" {
			rec.EndedAt = time.Now()
			rec.Status = Blocked
			if why.code == "upstream_not_run" {
				rec.Code = "upstream_not_run"
			} else {
				rec.Code = "upstream_not_passed"
			}
			rec.Message = fmt.Sprintf("前置检查 %q 状态为 %s：%s", blocked, why.status, why.message)
			e.store(rec)
			continue
		}

		// 3) 单项超时与取消由独立 ctx 区分。
		limit := item.Timeout()
		if limit <= 0 {
			limit = DefaultTimeout
		}
		itemCtx, cancelItem := context.WithTimeout(parent, limit)
		resCh := make(chan Result, 1)
		var runners sync.WaitGroup
		runners.Add(1)
		go func() {
			defer runners.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					select {
					case resCh <- Result{
						Status:  Error,
						Code:    "check_panic",
						Message: fmt.Sprintf("检查执行崩溃: %v", recovered),
					}:
					case <-itemCtx.Done():
					}
				}
			}()
			select {
			case resCh <- item.Run(&Context{Context: itemCtx, Device: e.device}):
			case <-itemCtx.Done():
			}
		}()

		select {
		case res := <-resCh:
			rec.EndedAt = time.Now()
			rec.Status = res.Status
			rec.Code = res.Code
			rec.Message = res.Message
		case <-itemCtx.Done():
			rec.EndedAt = time.Now()
			if parent.Err() != nil {
				rec.Status, rec.Code, rec.Message = fromContextErr(parent.Err())
			} else {
				rec.Status, rec.Code, rec.Message = fromContextErr(itemCtx.Err())
			}
		}
		cancelItem()
		runners.Wait()
		e.store(rec)
	}

	e.mu.RLock()
	all := make([]Record, 0, len(e.order))
	for _, id := range e.order {
		if rec, ok := e.records[id]; ok {
			if rec.Deps == nil {
				rec.Deps = []string{}
			}
			all = append(all, rec)
		}
	}
	e.mu.RUnlock()

	report := &Report{
		DeviceID:  e.device.ID(),
		Model:     e.device.Model(),
		StartedAt: started,
		EndedAt:   time.Now(),
		Overall:   overall(all, len(e.order)),
		Records:   append([]Record(nil), all...),
	}
	report.Records = report.Records[:len(all):len(all)]
	e.history.Save(report)
	return report, nil
}

type upstreamInfo struct {
	status  Status
	code    string
	message string
}

// blockedBy 返回第一个未通过的前置检查。重跑时若前置尚无记录也视为阻塞。
func (e *Executor) blockedBy(deps []string) (string, upstreamInfo) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, dep := range deps {
		rec, ok := e.records[dep]
		if !ok {
			return dep, upstreamInfo{status: Status("missing"), code: "upstream_not_run", message: "前置尚未执行"}
		}
		if rec.Status != Passed {
			return dep, upstreamInfo{status: rec.Status, code: "upstream_not_passed", message: rec.Message}
		}
	}
	return "", upstreamInfo{}
}

func (e *Executor) store(rec Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempt[rec.CheckID]++
	rec.Attempt = e.attempt[rec.CheckID]
	rec.Deps = append([]string(nil), rec.Deps...)
	e.records[rec.CheckID] = rec
}

func fromContextErr(err error) (Status, string, string) {
	if err == context.DeadlineExceeded {
		return Timeout, "timeout", "检查超过时限"
	}
	return Canceled, "canceled", "自检被取消"
}

// overall 汇总当前全部检查项结论；未执行项记为阻塞。
func overall(records []Record, total int) Status {
	if len(records) == 0 {
		return Blocked
	}
	worst := Passed
	if len(records) < total {
		worst = Blocked
	}
	for _, rec := range records {
		if rec.Status.rank() > worst.rank() {
			worst = rec.Status
		}
	}
	return worst
}

// History 返回执行器使用的历史存储。
func (e *Executor) History() *History { return e.history }

// Restore 用既往报告恢复执行器当前结论（例如跨进程重跑前）。
// 只载入属于当前检查集的记录；历史报告本身不被修改。
func (e *Executor) Restore(report *Report) error {
	if report == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range report.Records {
		if _, ok := e.set.Get(rec.CheckID); !ok {
			continue
		}
		rec.Deps = append([]string(nil), rec.Deps...)
		e.records[rec.CheckID] = rec
		if rec.Attempt > e.attempt[rec.CheckID] {
			e.attempt[rec.CheckID] = rec.Attempt
		}
	}
	return nil
}
