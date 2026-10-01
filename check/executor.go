package check

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultTimeout 是检查项未单独设置超时时的默认时限。
const DefaultTimeout = 2 * time.Second

// Executor 按依赖关系执行检查集，维护当前结论与历史报告。
type Executor struct {
	device     Device
	set        *CheckSet
	order      []string
	dependents map[string][]string
	history    *History

	mu      sync.RWMutex
	records map[string]Record
	attempt map[string]int
	// inflight 记录“结论已出但读取尚未收回”的检查项（如超时后仍无响应的读取），
	// 用于：1) 向外界暴露清理进展；2) 同一检查重跑前等待旧读取收尾，避免重叠。
	inflight map[string]chan struct{}
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
	dependents := map[string][]string{}
	for _, id := range order {
		item, _ := set.Get(id)
		for _, dep := range item.Deps() {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	return &Executor{
		device:     device,
		set:        set,
		order:      order,
		dependents: dependents,
		history:    history,
		records:    map[string]Record{},
		attempt:    map[string]int{},
		inflight:   map[string]chan struct{}{},
	}, nil
}

// InFlight 返回结论已出但底层读取尚未收回的检查项（按 ID 排序）。
// 非空即表示对应资源尚未完全收尾，不能宣称自检已彻底结束。
func (e *Executor) InFlight() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.inflight))
	for id := range e.inflight {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// WaitIdle 阻塞等待全部在途读取收尾（或 ctx 结束）。
// 超时/取消轮次之后、关闭设备或宣告资源已释放之前应调用它。
func (e *Executor) WaitIdle(ctx context.Context) error {
	for {
		e.mu.RLock()
		dones := make([]chan struct{}, 0, len(e.inflight))
		for _, done := range e.inflight {
			dones = append(dones, done)
		}
		e.mu.RUnlock()
		if len(dones) == 0 {
			return nil
		}
		for _, done := range dones {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
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
		for _, id := range e.dependents[cur] {
			if !affected[id] {
				affected[id] = true
				queue = append(queue, id)
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

// Affected 返回重跑 checkID 时会被重新执行的检查项（自身及受影响下游），按执行顺序排列。
func (e *Executor) Affected(checkID string) ([]string, error) {
	if _, ok := e.set.Get(checkID); !ok {
		return nil, fmt.Errorf("检查项 %q 不存在", checkID)
	}
	return e.descendants(checkID), nil
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

		// 同一检查的前次读取可能尚未收回（如超时后仍无响应）：
		// 先等它收尾再发起新读取，避免新旧读取在同一设备上重叠。
		if done := e.inflightDone(id); done != nil {
			select {
			case <-done:
			case <-parent.Done():
				rec.EndedAt = time.Now()
				rec.Status, rec.Code, rec.Message = fromContextErr(parent.Err())
				rec.Message += "（等待前次读取收尾期间被取消）"
				e.store(rec, roots[id])
				newRecords = append(newRecords, rec)
				continue
			}
		}

		itemCtx, cancelItem := context.WithTimeout(parent, limit)
		resCh := make(chan Result, 1)
		done := make(chan struct{})
		e.trackInflight(id, done)
		go func() {
			defer e.untrackInflight(id, done)
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					resCh <- Fault("check_panic", "检查自身异常: %v", r)
				}
			}()
			resCh <- item.Run(&Context{Context: itemCtx, Device: e.device})
		}()

		select {
		case res := <-resCh:
			// 超时/取消与检查返回可能同时发生，以上游 ctx 结论为准，避免误判通过。
			if err := itemCtx.Err(); err != nil {
				rec.Status, rec.Code, rec.Message = fromContextErr(err)
				break
			}
			rec.EndedAt = time.Now()
			rec.Status = res.Status
			rec.Code = res.Code
			rec.Message = res.Message
		case <-itemCtx.Done():
			rec.EndedAt = time.Now()
			rec.Status, rec.Code, rec.Message = fromContextErr(itemCtx.Err())
			// 结论已出，但读取可能尚未收回：如实标记，不得报告为完全结束。
			select {
			case <-done:
			default:
				rec.CleanupPending = true
			}
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
	_ = isRoot
	e.attempt[rec.CheckID]++
	rec.Attempt = e.attempt[rec.CheckID]
	e.records[rec.CheckID] = rec
}

// inflightDone 返回某检查项尚未收回的前次读取的完成信号；无则返回 nil。
func (e *Executor) inflightDone(id string) chan struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.inflight[id]
}

func (e *Executor) trackInflight(id string, done chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight[id] = done
}

func (e *Executor) untrackInflight(id string, done chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 迟到结果只清理自己的登记，不触碰已有结论，避免污染后续自检。
	if cur, ok := e.inflight[id]; ok && cur == done {
		delete(e.inflight, id)
	}
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
