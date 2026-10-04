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
	plan       *Plan

	mu      sync.RWMutex
	records map[string]Record
	attempt map[string]int
	genseq  map[string]int64
	// inflight 记录“结论已出但读取尚未收回”的检查项（如超时后仍无响应的读取），
	// 用于：1) 向外界暴露清理进展；2) 同一检查重跑前等待旧读取收尾，避免重叠。
	inflight map[string]inflightRead
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
	var initial []Check
	for _, id := range order {
		item, _ := set.Get(id)
		if !set.Conditional(id) {
			initial = append(initial, item)
		}
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
		plan:       newPlan(initial, time.Now()),
		records:    map[string]Record{},
		attempt:    map[string]int{},
		genseq:     map[string]int64{},
		inflight:   map[string]inflightRead{},
	}, nil
}

type inflightRead struct {
	done    chan struct{}
	gen     int64
	runID   int
	attempt int
	started time.Time
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
		chans := make([]chan struct{}, 0, len(e.inflight))
		for _, read := range e.inflight {
			chans = append(chans, read.done)
		}
		e.mu.RUnlock()
		if len(chans) == 0 {
			return nil
		}
		for _, done := range chans {
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
	base := make([]string, 0, len(e.order))
	for _, id := range e.order {
		if !e.set.Conditional(id) {
			base = append(base, id)
		}
	}
	return e.run(ctx, base, nil, false)
}

// Rerun 只重跑指定检查及其受影响的后续检查。
// 与该检查无关的既有结论保持不变；旧报告仍保留在历史中。
func (e *Executor) Rerun(ctx context.Context, checkID string) (*Report, error) {
	if _, ok := e.set.Get(checkID); !ok {
		return nil, fmt.Errorf("检查项 %q 不存在", checkID)
	}
	affected := e.descendants(checkID, false)
	return e.run(ctx, affected, map[string]bool{checkID: true}, true)
}

// descendants 返回包含 root 自身在内、依赖链可达的全部检查项，按执行顺序排列。
func (e *Executor) descendants(root string, includeConditional bool) []string {
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
		if affected[id] && (includeConditional || !e.set.Conditional(id)) {
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
	return e.descendants(checkID, false), nil
}

func (e *Executor) run(parent context.Context, targets []string, roots map[string]bool, isRerun bool) (*Report, error) {
	runID := e.history.NextRunID()
	targetSet := map[string]bool{}
	queue := append([]string(nil), targets...)
	for _, id := range targets {
		targetSet[id] = true
	}
	started := time.Now()
	planBefore := clonePlan(e.plan)
	var invalidations []Invalidation
	dynamicFromRevision := 0

	if isRerun {
		invalidations = append(invalidations, e.invalidateTargets(targets)...)
		dynamicFromRevision = e.planRevision()
	}

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		item, _ := e.set.Get(id)
		rec := Record{
			CheckID:   id,
			Name:      item.Name(),
			Deps:      append([]string(nil), item.Deps()...),
			StartedAt: time.Now(),
		}

		if err := parent.Err(); err != nil {
			rec.EndedAt = time.Now()
			rec.Status, rec.Code, rec.Message = fromContextErr(err)
			rec.RunID = runID
			rec = e.store(rec)
			continue
		}
		if e.set.Conditional(id) && isRerun && !e.conditionSatisfiedByEvidence(id) {
			e.markStaleIfPresent(id, "触发该补充检查的失败结果已经改变，旧补充结论失效")
			continue
		}

		blockedDeps := e.blockedFor(id, item.Deps())
		runDespiteBlockedDeps := e.set.Conditional(id) && e.conditionSatisfiedByEvidence(id)
		if len(blockedDeps) > 0 && !runDespiteBlockedDeps {
			rec.EndedAt = time.Now()
			rec.Status = Blocked
			rec.Code = "upstream_not_passed"
			why := blockedDeps[0]
			rec.Message = fmt.Sprintf("前置检查 %q 状态为 %s：%s", why.CheckID, why.Status, why.Message)
			rec.RunID = runID
			rec = e.store(rec)
			e.processRules(rec, &queue, targetSet)
			continue
		}

		limit := item.Timeout()
		if limit <= 0 {
			limit = DefaultTimeout
		}
		if read, ok := e.inflightDone(id); ok {
			select {
			case <-read.done:
			case <-parent.Done():
				rec.EndedAt = time.Now()
				rec.Status, rec.Code, rec.Message = fromContextErr(parent.Err())
				rec.Message += "（等待前次读取收尾期间被取消）"
				rec.RunID = runID
				e.store(rec)
				continue
			}
		}

		itemCtx, cancelItem := context.WithTimeout(parent, limit)
		resCh := make(chan Result, 1)
		done := make(chan struct{})
		gen := e.nextGeneration(id)
		e.trackInflight(id, inflightRead{done: done, gen: gen, started: time.Now()})
		checkID := id
		checkItem := item
		go func(gen int64) {
			defer e.untrackInflight(checkID, inflightRead{done: done, gen: gen})
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					select {
					case resCh <- Fault("check_panic", "检查自身异常: %v", r):
					default:
					}
				}
			}()
			resCh <- checkItem.Run(&Context{Context: itemCtx, Device: e.device})
		}(gen)

		select {
		case res := <-resCh:
			if err := itemCtx.Err(); err != nil {
				rec.Status, rec.Code, rec.Message = fromContextErr(err)
			} else {
				rec.Status = res.Status
				rec.Code = res.Code
				rec.Message = res.Message
			}
		case <-itemCtx.Done():
			rec.Status, rec.Code, rec.Message = fromContextErr(itemCtx.Err())
			if read, ok := e.inflightDone(id); ok && read.gen == gen {
				rec.ReadStillOpen = true
			}
		}
		rec.EndedAt = time.Now()
		rec.CleanupPending = rec.ReadStillOpen
		cancelItem()
		rec.RunID = runID
		rec = e.store(rec)
		e.processRules(rec, &queue, targetSet)
	}

	if isRerun {
		e.invalidateChangedConditional()
	}

	all := e.currentRecords()
	e.annotatePlanRun(runID)
	invalidations = append(invalidations, planInvalidationsAfter(e.plan, dynamicFromRevision)...)
	report := &Report{
		DeviceID:      e.device.ID(),
		Model:         e.device.Model(),
		StartedAt:     started,
		EndedAt:       time.Now(),
		Overall:       overall(all, len(e.order)),
		Records:       all,
		PlanBefore:    planBefore,
		PlanAfter:     clonePlan(e.plan),
		Invalidations: append([]Invalidation(nil), invalidations...),
	}
	report.RunID = runID
	e.history.Save(report)
	return report, nil
}

func (e *Executor) planRevision() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.plan.Revision
}

func (e *Executor) blockedFor(checkID string, deps []string) []Record {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var blocked []Record
	for _, dep := range deps {
		rec, ok := e.records[dep]
		if !ok {
			blocked = append(blocked, Record{CheckID: dep, Status: Status("missing"), Message: "前置尚未执行"})
			continue
		}
		if rec.Status != Passed {
			blocked = append(blocked, rec)
		}
	}
	return blocked
}

func (e *Executor) store(rec Record) Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempt[rec.CheckID]++
	rec.Attempt = e.attempt[rec.CheckID]
	e.records[rec.CheckID] = rec
	return rec
}

// inflightDone 返回某检查项尚未收回的前次读取的完成信号；无则返回 nil。
func (e *Executor) inflightDone(id string) (inflightRead, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	read, ok := e.inflight[id]
	return read, ok
}

func (e *Executor) nextGeneration(id string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.genseq[id]++
	return e.genseq[id]
}

func (e *Executor) trackInflight(id string, read inflightRead) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight[id] = read
}

func (e *Executor) untrackInflight(id string, read inflightRead) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 迟到结果只清理自己的登记，不触碰已有结论，避免污染后续自检。
	if cur, ok := e.inflight[id]; ok && cur.gen == read.gen && cur.done == read.done {
		delete(e.inflight, id)
	}
}

func (e *Executor) currentRecords() []Record {
	e.mu.RLock()
	defer e.mu.RUnlock()
	all := make([]Record, 0, len(e.order))
	for _, id := range e.order {
		if rec, ok := e.records[id]; ok {
			all = append(all, rec)
		}
	}
	return all
}

func evidence(rec Record) ResultEvidence {
	return ResultEvidence{
		CheckID: rec.CheckID,
		Attempt: rec.Attempt,
		RunID:   rec.RunID,
		Status:  rec.Status,
		Code:    rec.Code,
		Message: rec.Message,
		EndedAt: rec.EndedAt,
	}
}

func (e *Executor) processRules(rec Record, queue *[]string, targetSet map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var added []PlanEntry
	for _, rule := range e.set.Rules() {
		if !rule.matches(rec) {
			continue
		}
		for _, addID := range rule.Add {
			if targetSet[addID] || e.plan.has(addID) {
				continue
			}
			targetSet[addID] = true
			*queue = append(*queue, addID)
			entry := PlanEntry{
				CheckID:      addID,
				Origin:       PlanConditional,
				Reason:       rule.Reason,
				Trigger:      ptrEvidence(evidence(rec)),
				Dependencies: []ResultEvidence{evidence(rec)},
				AddedAt:      time.Now(),
			}
			added = append(added, entry)
		}
	}
	if len(added) > 0 {
		e.plan.add(added, "失败结果触发补充检查", nil)
		sort.Slice(*queue, func(i, j int) bool {
			return e.indexOf((*queue)[i]) < e.indexOf((*queue)[j])
		})
	}
}

func (e *Executor) indexOf(id string) int {
	for i, ordered := range e.order {
		if ordered == id {
			return i
		}
	}
	return len(e.order)
}

func ptrEvidence(v ResultEvidence) *ResultEvidence { return &v }

func (e *Executor) conditionSatisfiedByEvidence(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.conditionSatisfiedByEvidenceLocked(id)
}

func (e *Executor) conditionSatisfiedByEvidenceLocked(id string) bool {
	for _, rule := range e.set.Rules() {
		matched := false
		for _, added := range rule.Add {
			if added == id {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		rec, ok := e.records[rule.From]
		if ok && !rec.Stale && rule.matches(rec) {
			return true
		}
	}
	return false
}

func (e *Executor) markStaleIfPresent(id, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.records[id]
	if !ok || rec.Stale {
		return
	}
	rec.Stale = true
	rec.InvalidatedReason = reason
	e.records[id] = rec
	e.plan.add(nil, "上游结果改变使旧补充结论失效", []Invalidation{{
		CheckID:         id,
		Because:         "trigger_changed",
		Reason:          reason,
		Previous:        rec.Status,
		PreviousCode:    rec.Code,
		PreviousAttempt: rec.Attempt,
		PreviousRun:     rec.RunID,
		At:              time.Now(),
	}})
}

func (e *Executor) invalidateChangedConditional() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for _, entry := range e.plan.Entries {
		if entry.Origin != PlanConditional {
			continue
		}
		if !e.set.Conditional(entry.CheckID) {
			continue
		}
		valid := false
		for _, rule := range e.set.Rules() {
			if !containsString(rule.Add, entry.CheckID) {
				continue
			}
			trigger, ok := e.records[rule.From]
			if ok && !trigger.Stale && rule.matches(trigger) {
				valid = true
			}
		}
		if valid {
			continue
		}
		rec, ok := e.records[entry.CheckID]
		if !ok || rec.Stale {
			continue
		}
		reason := "触发补充检查的失败结果已经改变或不存在"
		rec.Stale = true
		rec.InvalidatedReason = reason
		e.records[entry.CheckID] = rec
		e.plan.add(nil, "上游结果改变使旧补充结论失效", []Invalidation{{
			CheckID:         entry.CheckID,
			Because:         "trigger_changed",
			Reason:          reason,
			Previous:        rec.Status,
			PreviousCode:    rec.Code,
			PreviousAttempt: rec.Attempt,
			PreviousRun:     rec.RunID,
			At:              now,
		}})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (e *Executor) invalidateTargets(targets []string) []Invalidation {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	var invalidated []Invalidation
	var entries []PlanEntry
	for _, id := range targets {
		if rec, ok := e.records[id]; ok && !rec.Stale {
			inv := Invalidation{
				CheckID:         id,
				Because:         "rerun",
				Reason:          "重新执行该检查，旧结论不再作为当前结论复用",
				Previous:        rec.Status,
				PreviousCode:    rec.Code,
				PreviousAttempt: rec.Attempt,
				At:              now,
			}
			invalidated = append(invalidated, inv)
			rec.Stale = true
			rec.InvalidatedReason = inv.Reason
			e.records[id] = rec
		}
		if !e.plan.has(id) {
			entries = append(entries, PlanEntry{
				CheckID: id,
				Origin:  PlanRerun,
				Reason:  "手动重跑指定项",
				AddedAt: now,
			})
		}
	}
	if len(invalidated) > 0 || len(entries) > 0 {
		e.plan.add(entries, "重跑前使受影响旧结论失效", invalidated)
	}
	return invalidated
}

func (e *Executor) annotatePlanRun(runID int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.plan == nil || len(e.plan.History) == 0 {
		return
	}
	for rev := range e.plan.History {
		for i := range e.plan.History[rev].Added {
			entry := &e.plan.History[rev].Added[i]
			if entry.RunID == 0 {
				entry.RunID = runID
			}
		}
	}
	for i := range e.plan.Entries {
		if e.plan.Entries[i].RunID == 0 {
			e.plan.Entries[i].RunID = runID
		}
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
