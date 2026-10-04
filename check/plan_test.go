package check_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"devicecheck/check"
)

type fakeDevice struct{ id string }

func (f fakeDevice) ID() string    { return f.id }
func (f fakeDevice) Model() string { return "fake-1" }

// resultCheck 的结果可由测试随时调整。
type resultCheck struct {
	id   string
	deps []string
	fn   func() check.Result
}

func (c *resultCheck) ID() string             { return c.id }
func (c *resultCheck) Name() string           { return c.id }
func (c *resultCheck) Deps() []string         { return c.deps }
func (c *resultCheck) Timeout() time.Duration { return time.Second }
func (c *resultCheck) Run(ctx *check.Context) check.Result {
	return c.fn()
}

func pass() check.Result { return check.Pass("ok") }
func fail() check.Result { return check.Fail("out_of_range", "bad") }

func newExec(t *testing.T, checks ...check.Check) *check.Executor {
	t.Helper()
	set, err := check.NewSet("fake-1", checks...)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := check.NewExecutor(fakeDevice{"dev-1"}, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func recOf(t *testing.T, r *check.Report, id string) check.Record {
	t.Helper()
	rec, ok := r.Record(id)
	if !ok {
		t.Fatalf("报告缺少 %q", id)
	}
	return rec
}

// 计划只增不改：同 ID 再加入不覆盖；合并双向都不覆盖已有结论。
func TestPlanAddAndMergeNeverOverwrite(t *testing.T) {
	p := check.NewPlan()
	if !p.Add(check.PlanItem{CheckID: "a", Name: "A", Reason: "初始"}) {
		t.Fatal("首次加入应成功")
	}
	if p.Add(check.PlanItem{CheckID: "a", Name: "A2", Reason: "重复"}) {
		t.Fatal("同 ID 不应覆盖")
	}
	item, _ := p.Get("a")
	if item.Reason != "初始" || item.Status != check.Pending {
		t.Fatalf("已有条目被改写: %+v", item)
	}

	// 另一份计划中 a 已结论、b 是新发现项。
	other := check.NewPlan()
	other.Add(check.PlanItem{CheckID: "a", Name: "A", Reason: "初始",
		Status: check.Passed, Record: &check.Record{CheckID: "a", Status: check.Passed, Attempt: 1}})
	other.Add(check.PlanItem{CheckID: "b", Name: "B", Reason: "补充"})
	added := p.Merge(other)
	if len(added) != 1 || added[0] != "b" {
		t.Fatalf("应只新发现 b，得到 %v", added)
	}
	// a 回填了结论；b 保持 pending。
	item, _ = p.Get("a")
	if item.Record == nil || item.Status != check.Passed {
		t.Fatalf("a 应回填结论: %+v", item)
	}
	if item.Record.Attempt != 1 {
		t.Fatalf("a 的依据应保留 Attempt=1，得到 %d", item.Record.Attempt)
	}
	// 反向合并：other 中的 a 不被 p 覆盖，b 不被清空。
	back := other.Merge(p)
	if len(back) != 0 {
		t.Fatalf("反向合并不应有新项: %v", back)
	}
	oa, _ := other.Get("a")
	if oa.Record.Attempt != 1 {
		t.Fatal("历史计划中的结论被覆盖")
	}
}

// 计划保存/重开：JSON 往返后结论、依据、原因完整保留。
func TestPlanSaveLoadRoundTrip(t *testing.T) {
	p := check.NewPlan()
	p.Add(check.PlanItem{CheckID: "up", Name: "上游", Reason: "初始检查集",
		Status: check.Passed, Record: &check.Record{CheckID: "up", Status: check.Passed, Code: "ok", Attempt: 2}})
	p.Add(check.PlanItem{CheckID: "down", Name: "下游", Reason: "初始检查集", Deps: []string{"up"},
		Status: check.Failed, Record: &check.Record{CheckID: "down", Status: check.Failed, Code: "out_of_range",
			Attempt: 1, Basis: map[string]int{"up": 2}}})
	var buf bytes.Buffer
	if err := p.Save(&buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := check.LoadPlan(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	down, ok := loaded.Get("down")
	if !ok || down.Record == nil {
		t.Fatal("重开后 down 结论丢失")
	}
	if down.Record.Basis["up"] != 2 || down.Reason != "初始检查集" {
		t.Fatalf("重开后依据/原因丢失: %+v", down)
	}
}

// 某项失败触发补充检查：补充项当轮执行，计划可查看加入原因。
func TestConditionalDiscoveryOnFailure(t *testing.T) {
	up := &resultCheck{id: "up", fn: fail}
	exec := newExec(t, up)
	exec.OnResult(check.Rule{
		Reason: "up 未通过，触发补充诊断",
		When:   func(rec check.Record) bool { return rec.CheckID == "up" && rec.Status != check.Passed },
		Add: func() []check.Check {
			return []check.Check{
				&resultCheck{id: "diag", fn: pass},
				&resultCheck{id: "deep", deps: []string{"diag"}, fn: pass},
			}
		},
	})
	r, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := recOf(t, r, "up").Status; got != check.Failed {
		t.Fatalf("up 应 failed，得到 %s", got)
	}
	if got := recOf(t, r, "diag").Status; got != check.Passed {
		t.Fatalf("补充检查 diag 应当轮执行并通过，得到 %s", got)
	}
	if got := recOf(t, r, "deep").Status; got != check.Passed {
		t.Fatalf("deep 应在 diag 之后执行，得到 %s", got)
	}
	item, ok := exec.Plan().Get("diag")
	if !ok || item.Reason != "up 未通过，触发补充诊断" {
		t.Fatalf("计划中应可查看补充原因: %+v", item)
	}
	// 恢复后规则不再触发，计划不重复添加。
	up.fn = pass
	if _, err := exec.Rerun(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if exec.Plan().Len() != 3 {
		t.Fatalf("计划不应重复添加，得到 %d 项", exec.Plan().Len())
	}
}

// 未结论项不得被当成通过跳过：整体结论不得为 passed，依赖它的项被 blocked。
func TestPendingItemsAreNotPassed(t *testing.T) {
	up := &resultCheck{id: "up", fn: pass}
	down := &resultCheck{id: "down", deps: []string{"ghost"}, fn: pass}
	set, err := check.NewSet("fake-1", up, down)
	if err == nil {
		t.Fatal("依赖不存在的检查项应在构建检查集时报错")
	}
	_ = set

	// 通过计划重开引入一个尚无检查定义、也未结论的项。
	exec := newExec(t, up)
	saved := check.NewPlan()
	saved.Add(check.PlanItem{CheckID: "ghost", Name: "幽灵检查", Reason: "历史计划遗留"})
	exec.AdoptPlan(saved)

	r, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ghost := recOf(t, r, "ghost")
	if ghost.Status != check.Pending {
		t.Fatalf("未执行项应记 pending，得到 %s", ghost.Status)
	}
	if r.Overall == check.Passed {
		t.Fatal("存在未结论项时整体不得为 passed")
	}
	if pend := exec.Plan().Pending(); len(pend) != 1 || pend[0].CheckID != "ghost" {
		t.Fatalf("Pending() 应列出 ghost，得到 %v", pend)
	}
}

// 修复后仅重测受影响部分：无关项保留原结论与依据；
// 上游再次变化时，报告指出哪些下游结论已过期。
func TestRerunKeepsBasisAndReportsStale(t *testing.T) {
	up := &resultCheck{id: "up", fn: pass}
	down := &resultCheck{id: "down", deps: []string{"up"}, fn: pass}
	other := &resultCheck{id: "other", fn: pass}
	exec := newExec(t, up, down, other)

	r1, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r1.Overall != check.Passed {
		t.Fatalf("首轮应全部通过，得到 %s", r1.Overall)
	}
	if basis := recOf(t, r1, "down").Basis["up"]; basis != 1 {
		t.Fatalf("down 的依据应为 up 第 1 次执行，得到 %d", basis)
	}

	// 上游再次变化：up 由通过变为失败。
	up.fn = fail
	r2, err := exec.Rerun(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if got := recOf(t, r2, "down").Status; got != check.Blocked {
		t.Fatalf("up 失败后 down 应 blocked，得到 %s", got)
	}
	if len(r2.Invalidated) != 1 || r2.Invalidated[0] != "down" {
		t.Fatalf("应指出 down 结论已过期，得到 %v", r2.Invalidated)
	}
	// 无关项不被重测，原结论与次数保留。
	if rec := recOf(t, r2, "other"); rec.Attempt != 1 || rec.Status != check.Passed {
		t.Fatalf("other 不应被重测: %+v", rec)
	}
	if stale := exec.Stale(); len(stale) != 0 {
		t.Fatalf("down 本轮已重测，不应再过期: %v", stale)
	}
}

// 计划保存重开：历史结论与依据恢复到新执行器，
// 只重测受影响部分；历史计划文件不被新发现项覆盖。
func TestPlanSaveReopenAndRerun(t *testing.T) {
	up := &resultCheck{id: "up", fn: fail}
	down := &resultCheck{id: "down", deps: []string{"up"}, fn: pass}
	exec := newExec(t, up, down)
	h := exec.History()

	r1, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := recOf(t, r1, "down").Status; got != check.Blocked {
		t.Fatalf("down 应 blocked，得到 %s", got)
	}

	// 保存计划；随后“进程重启”：新执行器 + 同一历史，重开计划。
	var buf bytes.Buffer
	if err := exec.Plan().Save(&buf); err != nil {
		t.Fatal(err)
	}
	saved, err := check.LoadPlan(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	exec2 := newExec(t, up, down)
	// 新执行器共用同一历史，轮次延续。
	exec2 = nil
	set, err := check.NewSet("fake-1", up, down)
	if err != nil {
		t.Fatal(err)
	}
	exec2, err = check.NewExecutor(fakeDevice{"dev-1"}, set, h)
	if err != nil {
		t.Fatal(err)
	}
	exec2.AdoptPlan(saved)

	// 重开后结论已恢复：up 仍是 failed/Attempt=1。
	r2pre, err := exec2.Rerun(context.Background(), "down")
	if err != nil {
		t.Fatal(err)
	}
	if got := recOf(t, r2pre, "down").Status; got != check.Blocked {
		t.Fatalf("重开后 up 结论应恢复为 failed，down 仍 blocked，得到 %s", got)
	}
	if rec := recOf(t, r2pre, "up"); rec.Attempt != 1 || rec.Status != check.Failed {
		t.Fatalf("重开后 up 应保留原结论与次数: %+v", rec)
	}

	// 修复上游，仅重测 up 及下游；历史轮次接续。
	up.fn = pass
	r3, err := exec2.Rerun(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if got := recOf(t, r3, "down").Status; got != check.Passed {
		t.Fatalf("修复后 down 应通过，得到 %s", got)
	}
	if rec := recOf(t, r3, "up"); rec.Attempt != 2 {
		t.Fatalf("up 应执行 2 次，得到 %d", rec.Attempt)
	}
	if h.Len() != 3 {
		t.Fatalf("历史应接续为 3 轮，得到 %d", h.Len())
	}

	// 历史计划文件不被后来的执行覆盖：重新加载仍是首轮结论。
	saved2, err := check.LoadPlan(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	item, _ := saved2.Get("up")
	if item.Record == nil || item.Record.Status != check.Failed {
		t.Fatal("已保存的历史计划被新结论覆盖")
	}
	// 新发现的检查项也不被历史计划覆盖。
	exec2.Plan().Add(check.PlanItem{CheckID: "extra", Name: "新发现", Reason: "后续发现"})
	exec2.AdoptPlan(saved2)
	if _, ok := exec2.Plan().Get("extra"); !ok {
		t.Fatal("新发现的检查项被历史计划覆盖")
	}
}

// 重开的计划中若下游依据与上游最新次数不符，Stale 应指出过期项。
func TestStaleAfterAdopt(t *testing.T) {
	up := &resultCheck{id: "up", fn: pass}
	down := &resultCheck{id: "down", deps: []string{"up"}, fn: pass}
	exec := newExec(t, up, down)

	saved := check.NewPlan()
	saved.Add(check.PlanItem{CheckID: "up", Name: "up", Status: check.Passed,
		Record: &check.Record{CheckID: "up", Status: check.Passed, Attempt: 2}})
	saved.Add(check.PlanItem{CheckID: "down", Name: "down", Status: check.Passed,
		Record: &check.Record{CheckID: "down", Status: check.Passed, Attempt: 1,
			Basis: map[string]int{"up": 1}}}) // 依据停留在 up 第 1 次，已过期
	exec.AdoptPlan(saved)
	stale := exec.Stale()
	if len(stale) != 1 || stale[0] != "down" {
		t.Fatalf("应指出 down 已过期，得到 %v", stale)
	}
}
