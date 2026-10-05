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
	checks     map[string]Check // 当前已知检查：初始集 + 条件规则追加的补充项
	order      []string
	dependents map[string][]string
	history    *History
	rules      []Rule
	planLog    *PlanLog

	mu      sync.RWMutex
	records map[string]Record
	attempt map[string]int
	// stale 记录“结论已失效但尚未重跑”的检查项及失效原因：
	// 上游结论发生变化时，其下游旧结论即不可再信，直到该项被重新执行。
	stale map[string]string
	// evidenceDeps 记录“依据依赖”：条件规则补充项的加入依据引用了哪些检查结论，
	// 这些结论变化时，补充项旧结论同样失效（即使它没有声明图依赖）。
	evidenceDeps map[string][]string
	// inflight 记录“结论已出但读取尚未收回”的检查项（如超时后仍无响应的读取），
	// 用于：1) 向外界暴露清理进展；2) 同一检查重跑前等待旧读取收尾，避免重叠。
	inflight map[string]chan struct{}
	// commitments 记录条件规则追加、且尚未真正取得资源开始执行的检查。
	commitments map[string]bool
	// canceledCommitments 标记已取消、等待阻塞获取返回后从计划移除的承诺。
	canceledCommitments map[string]bool
	// canceledRuleCommitments 防止被取消的条件项在同轮规则评估中再次加入。
	canceledRuleCommitments map[string]bool
	// scheduler 管理多设备共享仪器。为 nil 时保留单设备执行器原行为。
	scheduler *Scheduler
	risk      int
}

// NewExecutor 为某台设备及其型号检查集构建执行器。
func NewExecutor(device Device, set *CheckSet, history *History) (*Executor, error) {
	if set.Model != device.Model() {
		return nil, fmt.Errorf("检查集型号 %q 与设备型号 %q 不匹配", set.Model, device.Model())
	}
	if _, err := set.Order(); err != nil {
		return nil, err
	}
	if history == nil {
		history = &History{}
	}
	checks := make(map[string]Check, len(set.IDs()))
	for _, id := range set.IDs() {
		item, _ := set.Get(id)
		checks[id] = item
	}
	order, dependents, err := topoOrder(checks)
	if err != nil {
		return nil, err
	}
	return &Executor{
		device:                  device,
		set:                     set,
		checks:                  checks,
		order:                   order,
		dependents:              dependents,
		history:                 history,
		planLog:                 NewPlanLog(order),
		records:                 map[string]Record{},
		attempt:                 map[string]int{},
		stale:                   map[string]string{},
		evidenceDeps:            map[string][]string{},
		inflight:                map[string]chan struct{}{},
		commitments:             map[string]bool{},
		canceledCommitments:     map[string]bool{},
		canceledRuleCommitments: map[string]bool{},
	}, nil
}

// SetRules 注册条件计划规则：某检查得出匹配结论时，自动把补充检查项
// 追加进计划并执行。规则引用的检查 ID 不得与既有检查重复；
// 补充检查的依赖必须指向已知检查（初始集或任一规则提供的检查）。
func (e *Executor) SetRules(rules ...Rule) error {
	known := map[string]bool{}
	for id := range e.checks {
		known[id] = true
	}
	for _, rule := range rules {
		if !known[rule.Trigger] {
			isRuleCheck := false
			for _, r2 := range rules {
				for _, c := range r2.Adds {
					if c.ID() == rule.Trigger {
						isRuleCheck = true
					}
				}
			}
			if !isRuleCheck {
				return fmt.Errorf("规则触发检查 %q 不存在", rule.Trigger)
			}
		}
		for _, c := range rule.Adds {
			if known[c.ID()] {
				return fmt.Errorf("规则补充检查 %q 与既有检查重复", c.ID())
			}
			known[c.ID()] = true
		}
	}
	for _, rule := range rules {
		for _, c := range rule.Adds {
			for _, dep := range c.Deps() {
				if !known[dep] {
					return fmt.Errorf("规则补充检查 %q 依赖不存在的 %q", c.ID(), dep)
				}
			}
		}
	}
	e.rules = append(e.rules, rules...)
	return nil
}

// PlanLog 返回执行器的计划日志：初始计划为版本 1，
// 每次条件规则触发追加补充项都会生成一个新版本，旧版本永不改写。
func (e *Executor) PlanLog() *PlanLog { return e.planLog }

// Invalidated 返回当前已失效但尚未重跑的旧结论及失效原因。
// 上游检查结果发生变化时，其下游既有结论即被标记失效，
// 直到对应检查被重新执行为止。
func (e *Executor) Invalidated() []Invalidation {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return invalidatedSorted(e.stale)
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

func (e *Executor) setScheduler(scheduler *Scheduler, risk int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scheduler = scheduler
	e.risk = risk
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
	e.mu.RLock()
	_, ok := e.checks[checkID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("检查项 %q 不存在", checkID)
	}
	affected := e.descendants(checkID)
	return e.run(ctx, affected, map[string]bool{checkID: true})
}

// descendants 返回包含 root 自身在内、依赖链可达的全部检查项，按执行顺序排列。
func (e *Executor) descendants(root string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.descendantsLocked(root)
}

// descendantsLocked 是 descendants 的无锁版本，调用方须已持有锁。
func (e *Executor) descendantsLocked(root string) []string {
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
	e.mu.RLock()
	_, ok := e.checks[checkID]
	e.mu.RUnlock()
	if !ok {
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
	done := map[string]bool{}

	for {
		e.mu.RLock()
		order := append([]string(nil), e.order...)
		e.mu.RUnlock()
		for _, id := range order {
			if !target[id] || done[id] {
				continue
			}
			done[id] = true
			e.mu.RLock()
			item := e.checks[id]
			e.mu.RUnlock()
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

			lease, err := e.acquireResources(parent, item)
			if err != nil {
				e.mu.Lock()
				canceledCommitment := e.canceledCommitments[id]
				if canceledCommitment {
					delete(e.canceledCommitments, id)
					delete(e.commitments, id)
					delete(e.checks, id)
					e.rebuildLocked()
					e.planLog.removeLocked([]string{id})
				}
				e.mu.Unlock()
				if canceledCommitment {
					continue
				}
				rec.EndedAt = time.Now()
				rec.Status, rec.Code, rec.Message = fromContextErr(parent.Err())
				rec.Message += "（等待共享仪器期间被取消）"
				e.store(rec, roots[id])
				newRecords = append(newRecords, rec)
				continue
			}
			e.markCommitmentStarted(id)

			itemCtx, cancelItem := context.WithTimeout(parent, limit)
			resCh := make(chan Result, 1)
			done := make(chan struct{})
			e.trackInflight(id, done)
			go func() {
				defer e.releaseResources(lease)
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
		// 每轮执行后评估条件规则：失败/异常结论可能触发补充检查加入计划。
		// 新加入的项纳入本次执行范围，直到没有新项为止。
		added := e.applyRules()
		for _, id := range added {
			target[id] = true
		}
		if len(added) == 0 {
			break
		}
	}

	e.mu.RLock()
	all := make([]Record, 0, len(e.order))
	for _, id := range e.order {
		if rec, ok := e.records[id]; ok {
			all = append(all, rec)
		}
	}
	staleNow := staleIDs(e.stale)
	e.mu.RUnlock()

	report := &Report{
		DeviceID:  e.device.ID(),
		Model:     e.device.Model(),
		StartedAt: started,
		EndedAt:   time.Now(),
		Overall:   overall(all, len(e.order)),
		Records:   all,
		Stale:     staleNow,
	}
	e.history.Save(report)
	return report, nil
}

func (e *Executor) acquireResources(ctx context.Context, item Check) (*resourceLease, error) {
	e.mu.RLock()
	scheduler := e.scheduler
	deviceID := e.device.ID()
	e.mu.RUnlock()
	if scheduler == nil {
		return &resourceLease{}, nil
	}
	return scheduler.acquire(ctx, deviceID, item.ID(), item)
}

func (e *Executor) releaseResources(lease *resourceLease) {
	e.mu.RLock()
	scheduler := e.scheduler
	e.mu.RUnlock()
	if scheduler != nil {
		scheduler.release(lease)
	}
}

// applyRules 根据当前已落记录评估条件规则，把新触发的补充检查
// 注册进检查表、追加计划版本，并返回新加入的检查 ID（按加入顺序）。
func (e *Executor) applyRules() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var added []string
	for _, rule := range e.rules {
		rec, ok := e.records[rule.Trigger]
		if !ok || !rule.matches(rec) {
			continue
		}
		var items []PlanItem
		for _, c := range rule.Adds {
			id := c.ID()
			if e.checkAlreadyResolved(id) {
				continue
			}
			e.checks[id] = c
			// 记录加入原因与当时依赖的检查结论：触发项 + 新检查的各前置。
			evidence := []Evidence{evidenceOf(rec)}
			for _, dep := range c.Deps() {
				if depRec, ok := e.records[dep]; ok && dep != rule.Trigger {
					evidence = append(evidence, evidenceOf(depRec))
				}
			}
			for _, ev := range evidence {
				e.evidenceDeps[ev.CheckID] = append(e.evidenceDeps[ev.CheckID], id)
			}
			items = append(items, PlanItem{CheckID: id, Reason: rule.Reason, Evidence: evidence})
			e.commitments[id] = true
			added = append(added, id)
		}
		if len(items) > 0 {
			e.rebuildLocked()
			e.planLog.appendLocked(items)
		}
	}
	return added
}

func (e *Executor) checkAlreadyResolved(id string) bool {
	_, inPlan := e.checks[id]
	return inPlan || e.canceledRuleCommitments[id]
}

// rebuildLocked 在检查表变化后重算拓扑顺序与下游表，调用方须已持有写锁。
func (e *Executor) rebuildLocked() {
	order, dependents, err := topoOrder(e.checks)
	if err != nil {
		return // SetRules 已校验无环无缺失，此处不应失败
	}
	e.order = order
	e.dependents = dependents
}

// staleIDs 返回失效检查 ID 的有序列表。
func staleIDs(stale map[string]string) []string {
	inv := invalidatedSorted(stale)
	ids := make([]string, 0, len(inv))
	for _, v := range inv {
		ids = append(ids, v.CheckID)
	}
	return ids
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
	_ = isRoot
	if e.canceledCommitments[rec.CheckID] {
		delete(e.canceledCommitments, rec.CheckID)
		delete(e.canceledRuleCommitments, rec.CheckID)
		delete(e.commitments, rec.CheckID)
		delete(e.checks, rec.CheckID)
		e.rebuildLocked()
		e.planLog.removeLocked([]string{rec.CheckID})
		e.mu.Unlock()
		return
	}
	old, existed := e.records[rec.CheckID]
	e.attempt[rec.CheckID]++
	rec.Attempt = e.attempt[rec.CheckID]
	e.records[rec.CheckID] = rec
	// 本项重新执行后，其失效标记自然清除。
	delete(e.stale, rec.CheckID)
	// 上游结论发生变化：其下游既有结论全部失效，直到被重新执行。
	// 失效只是“不可再信”的标记，不改写任何历史记录。
	var pendingCancellations []string
	if existed && (old.Status != rec.Status || old.Code != rec.Code) {
		affected := append(e.descendantsLocked(rec.CheckID), e.evidenceDeps[rec.CheckID]...)
		for _, id := range affected {
			if id == rec.CheckID {
				continue
			}
			if e.commitments[id] {
				e.canceledCommitments[id] = true
				e.canceledRuleCommitments[id] = true
				pendingCancellations = append(pendingCancellations, id)
				continue
			}
			if current, ok := e.records[id]; ok && current.Status == Passed {
				continue
			}
			if _, ok := e.records[id]; ok {
				e.stale[id] = fmt.Sprintf("上游 %q 结论由 %s/%s 变为 %s/%s",
					rec.CheckID, old.Status, old.Code, rec.Status, rec.Code)
			}
		}
	}
	deviceID := e.device.ID()
	scheduler := e.scheduler
	e.mu.Unlock()
	for _, id := range pendingCancellations {
		if scheduler != nil {
			scheduler.CancelWaiting(deviceID, id)
		}
	}
}

func (e *Executor) markCommitmentStarted(id string) {
	e.mu.Lock()
	delete(e.commitments, id)
	e.mu.Unlock()
}

// CancelCommitment 取消尚未取得共享资源、尚未开始的条件补充承诺。
// 已经占用仪器或已有独立结论的检查不会被回滚。
func (e *Executor) CancelCommitment(id string) bool {
	e.mu.Lock()
	if !e.commitments[id] {
		e.mu.Unlock()
		return false
	}
	deviceID := e.device.ID()
	scheduler := e.scheduler
	e.mu.Unlock()
	cancelled := scheduler == nil
	if scheduler != nil {
		cancelled = scheduler.CancelWaiting(deviceID, id)
	}
	if !cancelled {
		return false
	}
	e.mu.Lock()
	e.canceledCommitments[id] = true
	e.canceledRuleCommitments[id] = true
	e.mu.Unlock()
	return true
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
