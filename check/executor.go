package check

import (
	"context"
	"fmt"
	"time"
)

// DefaultTimeout 是检查项未单独设置超时时的默认时限。
const DefaultTimeout = 2 * time.Second

// Executor 按依赖关系执行检查集，维护当前结论与历史报告。
type Executor struct {
	device Device
	set    *CheckSet
	order  []string
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
	return e.run(ctx, e.order, nil)
}

// Rerun 只重跑指定检查及其受影响的后续检查。
// 与该检查无关的既有结论保持不变；旧报告仍保留在历史中。
func (e *Executor) Rerun(ctx context.Context, checkID string) (*Report, error) {
	if _, ok := e.set.Get(checkID); !ok {
		return nil, fmt.Errorf("检查项 %q 不存在", checkID)
	}
	affected := e.descendants(checkID)
	return e.run(ctx, affected, map[string]bool{checkID: true})
}

// descendants 返回包含 root 自身在内、依赖链可达的全部检查项，按执行顺序排列。
func (e *Executor) descendants(root string) []string {
	affected := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
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

func (e *Executor) run(parent context.Context, targets []string, roots map[string]bool) (*Report, error) {
	target := map[string]bool{}
	for _, id := range targets {
		target[id] = true
	}
	started := time.Now()
	newRecords := make([]Record, 0, len(targets))

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
			e.store(rec, roots[id])
			newRecords = append(newRecords, rec)
			continue
		}

		// 2) 前置条件：任一前置非通过（失败/故障/阻塞/超时/取消）则无法执行。
		if blocked, why := e.blockedBy(item.Deps()); blocked != "" {
			rec.EndedAt = time.Now()
			rec.Status = Blocked
			rec.Code = "upstream_not_passed"
			rec.Message = fmt.Sprintf("前置检查 %q 状态为 %s：%s", blocked, why.status, why.message)
			e.store(rec, roots[id])
			newRecords = append(newRecords, rec)
			continue
		}

		// 3) 单项超时与取消由独立 ctx 区分。
		limit := item.Timeout()
		if limit <= 0 {
			limit = DefaultTimeout
		}
		itemCtx, cancelItem := context.WithTimeout(parent, limit)
		resCh := make(chan Result, 1)
		go func() {
			resCh <- item.Run(&Context{Context: itemCtx, Device: e.device})
		}()

		select {
		case res := <-resCh:
			rec.EndedAt = time.Now()
			rec.Status = res.Status
			rec.Code = res.Code
			rec.Message = res.Message
		case <-itemCtx.Done():
			rec.EndedAt = time.Now()
			rec.Status, rec.Code, rec.Message = fromContextErr(itemCtx.Err())
		}
		cancelItem()
		e.store(rec, roots[id])
		newRecords = append(newRecords, rec)
	}

	e.mu.RLock()
	all := make([]Record, 0, len(e.order))
	for _, id := range e.order {
		if rec, ok := e.records[id]; ok {
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
		Records:   all,
	}
	e.history.Save(report)
	return report, nil
}

type upstreamInfo struct {
	status  Status
	message string
}

// blockedBy 返回第一个未通过的前置检查。重跑时若前置尚无记录也视为阻塞。
func (e *Executor) blockedBy(deps []string) (string, upstreamInfo) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, dep := range deps {
		rec, ok := e.records[dep]
		if !ok {
			return dep, upstreamInfo{status: Status("missing"), message: "前置尚未执行"}
		}
		if rec.Status != Passed {
			return dep, upstreamInfo{status: rec.Status, message: rec.Message}
		}
	}
	return "", upstreamInfo{}
}

func (e *Executor) store(rec Record, isRoot bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if isRoot {
		e.attempt[rec.CheckID]++
	}
	rec.Attempt = e.attempt[rec.CheckID]
	if rec.Attempt == 0 {
		rec.Attempt = 1
	}
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

