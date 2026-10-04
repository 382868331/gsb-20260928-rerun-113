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
//
// 执行器持有一份可查看的条件检查计划（Plan）：初始由检查集播种，
// 运行中通过 OnResult 注册的规则按已完成检查的结论临时补充检查项；
// 计划可保存后重开（AdoptPlan），历史结论与新发现项互不覆盖。
type Executor struct {
	device Device

	mu         sync.RWMutex
	checks     map[string]Check // 动态注册表：初始检查集 + 运行中发现的补充检查
	order      []string
	dependents map[string][]string
	plan       *Plan
	rules      []Rule
	history    *History
	records    map[string]Record
	attempt    map[string]int
	// inflight 记录“结论已出但读取尚未收回”的检查项（如超时后仍无响应的读取），
	// 用于：1) 向外界暴露清理进展；2) 同一检查重跑前等待旧读取收尾，避免重叠。
	inflight map[string]chan struct{}
}

// Rule 是一条条件分支规则：当某条检查结论满足 When 时，
// 把 Add 返回的补充检查项加入计划（同 ID 不覆盖），并尽量在当轮执行。
type Rule struct {
	// Reason 写入计划条目，说明补充检查的由来。
	Reason string
	When   func(rec Record) bool
	Add    func() []Check
}

// NewExecutor 为某台设备及其型号检查集构建执行器。
func NewExecutor(device Device, set *CheckSet, history *History) (*Executor, error) {
	if set.Model != device.Model() {
		return nil, fmt.Errorf("检查集型号 %q 与设备型号 %q 不匹配", set.Model, device.Model())
	}
	if history == nil {
		history = &History{}
	}
	e := &Executor{
		device:   device,
		checks:   map[string]Check{},
		plan:     NewPlan(),
		history:  history,
		records:  map[string]Record{},
		attempt:  map[string]int{},
		inflight: map[string]chan struct{}{},
	}
	checks := make([]Check, 0, len(set.IDs()))
	for _, id := range set.IDs() {
		item, _ := set.Get(id)
		checks = append(checks, item)
	}
	if _, err := e.discover(checks, "初始检查集"); err != nil {
		return nil, err
	}
	return e, nil
}

// Plan 返回执行器持有的条件检查计划，可随时查看各项状态与加入原因。
func (e *Executor) Plan() *Plan { return e.plan }

// OnResult 注册条件分支规则：某条检查结论满足规则时，自动补充检查项。
func (e *Executor) OnResult(rules ...Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, rules...)
}

// Register 显式注册补充检查的定义（如计划重开后恢复其检查逻辑）。
// 已存在于计划的条目保留原有结论与加入原因，不被覆盖。
func (e *Executor) Register(reason string, checks ...Check) error {
	_, err := e.discover(checks, reason)
	return err
}

// AdoptPlan 重开一份（通常是从文件加载的）历史计划：
// 已结论条目的记录与依据回填到当前执行器，新发现的检查项追加进计划；
// 当前已有结论一律不被覆盖。重开后可直接 Rerun 受影响部分。
func (e *Executor) AdoptPlan(saved *Plan) {
	added := e.plan.Merge(saved)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, item := range saved.Items() {
		if item.Record == nil {
			continue
		}
		if _, exists := e.records[item.CheckID]; exists {
			continue // 当前已有更新的结论，不覆盖
		}
		rec := *item.Record
		e.records[item.CheckID] = rec
		if rec.Attempt > e.attempt[item.CheckID] {
			e.attempt[item.CheckID] = rec.Attempt
		}
	}
	_ = added
}

// Stale 返回当前已过期的检查项：其结论依据的上游执行次数
// 与上游最新执行次数不一致（上游已再次变化，下游结论失效）。
func (e *Executor) Stale() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []string
	for id, rec := range e.records {
		for dep, basis := range rec.Basis {
			if e.attempt[dep] != basis {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// discover 把检查项注册进动态注册表并追加到计划（同 ID 不覆盖），
// 随后重算拓扑序。返回本次新加入计划的检查 ID。
func (e *Executor) discover(checks []Check, reason string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 先校验：本批及已有注册表必须能解析全部依赖。
	available := map[string]bool{}
	for id := range e.checks {
		available[id] = true
	}
	for _, item := range checks {
		if item.ID() == "" {
			return nil, &ErrCheckSet{msg: "检查项 ID 不能为空"}
		}
		available[item.ID()] = true
	}
	for _, item := range checks {
		for _, dep := range item.Deps() {
			if !available[dep] {
				return nil, &ErrCheckSet{msg: fmt.Sprintf("检查项 %q 依赖不存在的 %q", item.ID(), dep)}
			}
		}
	}
	var added []string
	for _, item := range checks {
		e.checks[item.ID()] = item // 重开计划后重新提供检查定义也走这里
		if e.plan.Add(PlanItem{
			CheckID: item.ID(),
			Name:    item.Name(),
			Deps:    append([]string(nil), item.Deps()...),
			Reason:  reason,
		}) {
			added = append(added, item.ID())
		}
	}
	order, err := TopoOrder(e.checks)
	if err != nil {
		return nil, err
	}
	dependents := map[string][]string{}
	for _, id := range order {
		item := e.checks[id]
		for _, dep := range item.Deps() {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	e.order = order
	e.dependents = dependents
	return added, nil
}

// snapshot 返回当前拓扑序的副本。
func (e *Executor) snapshot() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.order...)
}

// checkOf 按 ID 取检查定义；计划重开后可能暂时没有对应定义。
func (e *Executor) checkOf(id string) (Check, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	item, ok := e.checks[id]
	return item, ok
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
// 运行中由规则新发现的补充检查项会加入计划并尽量在当轮执行。
func (e *Executor) RunAll(ctx context.Context) (*Report, error) {
	return e.run(ctx, e.snapshot(), nil)
}

// Rerun 只重跑指定检查及其受影响的后续检查。
// 与该检查无关的既有结论保持不变；旧报告仍保留在历史中。
func (e *Executor) Rerun(ctx context.Context, checkID string) (*Report, error) {
	if _, ok := e.checkOf(checkID); !ok {
		return nil, fmt.Errorf("检查项 %q 不存在", checkID)
	}
	affected := e.descendants(checkID)
	return e.run(ctx, affected, map[string]bool{checkID: true})
}

// descendants 返回包含 root 自身在内、依赖链可达的全部检查项，按执行顺序排列。
func (e *Executor) descendants(root string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
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
	if _, ok := e.checkOf(checkID); !ok {
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
	processed := map[string]bool{}
	var invalidated []string
	seenInvalid := map[string]bool{}

	// 外层循环：规则可能在运行中发现新检查项，发现后并入本轮继续执行。
	for {
		progressed := false
		for _, id := range e.snapshot() {
			if !target[id] || processed[id] {
				continue
			}
			processed[id] = true
			progressed = true
			item, ok := e.checkOf(id)
			if !ok {
				// 计划重开后该项可能暂时没有检查定义：保持 pending，不得当作通过。
				continue
			}
			rec := e.execOne(parent, item)
			var stale []string
			rec, stale = e.store(rec)
			for _, dep := range stale {
				if !seenInvalid[dep] {
					seenInvalid[dep] = true
					invalidated = append(invalidated, dep)
				}
			}
			e.plan.complete(rec)
			// 条件分支：按本条结论决定是否补充检查项。
			e.mu.RLock()
			rules := append([]Rule(nil), e.rules...)
			e.mu.RUnlock()
			for _, rule := range rules {
				if !rule.When(rec) {
					continue
				}
				added, err := e.discover(rule.Add(), rule.Reason)
				if err != nil {
					return nil, err
				}
				for _, newID := range added {
					if !processed[newID] {
						target[newID] = true
					}
				}
			}
		}
		if !progressed {
			break
		}
	}

	e.mu.RLock()
	all := make([]Record, 0, len(e.order))
	inOrder := map[string]bool{}
	for _, id := range e.order {
		inOrder[id] = true
		if rec, ok := e.records[id]; ok {
			all = append(all, rec)
			continue
		}
		// 计划内但尚未结论的项：如实记为 pending，不得当成通过跳过。
		name := id
		var deps []string
		if item, ok := e.checks[id]; ok {
			name = item.Name()
			deps = append([]string(nil), item.Deps()...)
		} else if pi, ok := e.plan.Get(id); ok {
			name = pi.Name
			deps = pi.Deps
		}
		all = append(all, Record{
			CheckID: id,
			Name:    name,
			Deps:    deps,
			Status:  Pending,
			Code:    "pending",
			Message: "尚未执行，结论待定（不计为通过）",
		})
	}
	// 计划重开后可能存在尚未注册检查定义的条目，同样如实列出。
	for _, pi := range e.plan.Items() {
		if inOrder[pi.CheckID] {
			continue
		}
		if pi.Record != nil {
			all = append(all, *pi.Record)
			continue
		}
		all = append(all, Record{
			CheckID: pi.CheckID,
			Name:    pi.Name,
			Deps:    pi.Deps,
			Status:  Pending,
			Code:    "pending",
			Message: "尚未执行，结论待定（不计为通过）",
		})
	}
	e.mu.RUnlock()

	report := &Report{
		DeviceID:    e.device.ID(),
		Model:       e.device.Model(),
		StartedAt:   started,
		EndedAt:     time.Now(),
		Overall:     overall(all, len(all)),
		Records:     all,
		Invalidated: invalidated,
	}
	e.history.Save(report)
	return report, nil
}

// execOne 执行单个检查项并返回记录（含依赖依据、取消/超时与收尾标记）。
func (e *Executor) execOne(parent context.Context, item Check) Record {
	id := item.ID()
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
		return rec
	}

	// 2) 前置条件：任一前置非通过（失败/故障/阻塞/超时/取消）则无法执行。
	if blocked, why := e.blockedBy(item.Deps()); blocked != "" {
		rec.EndedAt = time.Now()
		rec.Status = Blocked
		rec.Code = "upstream_not_passed"
		rec.Message = fmt.Sprintf("前置检查 %q 状态为 %s：%s", blocked, why.status, why.message)
		rec.Basis = e.basisOf(item.Deps())
		return rec
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
			return rec
		}
	}

	// 记录结论依据：各前置当前的执行次数，供日后判断下游结论是否过期。
	rec.Basis = e.basisOf(item.Deps())

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
	return rec
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

// basisOf 记录各前置当前的执行次数，作为本结论的依据。
func (e *Executor) basisOf(deps []string) map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(deps) == 0 {
		return nil
	}
	basis := make(map[string]int, len(deps))
	for _, dep := range deps {
		basis[dep] = e.attempt[dep]
	}
	return basis
}

// store 存入一条结论，并返回因此过期的下游检查项
// （其既有结论依据的上游执行次数已与最新不一致）。
func (e *Executor) store(rec Record) (Record, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempt[rec.CheckID]++
	rec.Attempt = e.attempt[rec.CheckID]
	e.records[rec.CheckID] = rec
	var stale []string
	for _, dep := range e.dependents[rec.CheckID] {
		down, ok := e.records[dep]
		if !ok || down.Basis == nil {
			continue
		}
		if down.Basis[rec.CheckID] != rec.Attempt {
			stale = append(stale, dep)
		}
	}
	return rec, stale
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
