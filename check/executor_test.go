package check

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeDevice struct{ id, model string }

func (f fakeDevice) ID() string    { return f.id }
func (f fakeDevice) Model() string { return f.model }

func funcCheck(id string, deps []string, limit time.Duration, fn func(ctx *Context) Result) *FuncCheck {
	return &FuncCheck{CheckID: id, CheckName: id, Requires: deps, Limit: limit, Fn: fn}
}

func mustSet(t *testing.T, items ...Check) *CheckSet {
	t.Helper()
	set, err := NewSet("m", items...)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

func newExec(t *testing.T, set *CheckSet) *Executor {
	t.Helper()
	ex, err := NewExecutor(fakeDevice{id: "d1", model: "m"}, set, nil)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return ex
}

// 三类问题必须分别表示：传感器缺失=error/sensor_missing；
// 读数越界=failed/out_of_range；检查自身报错=error/selftest_error。
func TestThreeFailureKinds(t *testing.T) {
	set := mustSet(t,
		funcCheck("missing", nil, time.Second, func(ctx *Context) Result {
			return Fault("sensor_missing", "传感器不存在")
		}),
		funcCheck("range", nil, time.Second, func(ctx *Context) Result {
			return Fail("out_of_range", "读数 95 越界")
		}),
		funcCheck("selftest", nil, time.Second, func(ctx *Context) Result {
			return Fault("selftest_error", "检查逻辑坏了")
		}),
	)
	rep, err := newExec(t, set).RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		status Status
		code   string
	}{
		"missing":  {Error, "sensor_missing"},
		"range":    {Failed, "out_of_range"},
		"selftest": {Error, "selftest_error"},
	}
	for id, w := range want {
		rec, ok := rep.Record(id)
		if !ok {
			t.Fatalf("缺少记录 %s", id)
		}
		if rec.Status != w.status || rec.Code != w.code {
			t.Errorf("%s = %s/%s, 期望 %s/%s", id, rec.Status, rec.Code, w.status, w.code)
		}
	}
}

// 上游未通过时，下游必须 blocked 且绝不计为通过；缺失前置记录也不允许放行。
func TestUpstreamNotPassedBlocksDownstream(t *testing.T) {
	set := mustSet(t,
		funcCheck("a", nil, time.Second, func(ctx *Context) Result {
			return Fail("out_of_range", "a 越界")
		}),
		funcCheck("b", []string{"a"}, time.Second, func(ctx *Context) Result {
			t.Fatal("上游 a 未通过时 b 不应被执行")
			return Pass("不应到达")
		}),
		funcCheck("c", []string{"b"}, time.Second, func(ctx *Context) Result {
			t.Fatal("c 不应被执行")
			return Pass("不应到达")
		}),
	)
	rep, _ := newExec(t, set).RunAll(context.Background())
	b, _ := rep.Record("b")
	c, _ := rep.Record("c")
	if b.Status != Blocked || b.Code != "upstream_not_passed" {
		t.Errorf("b = %s/%s, 期望 blocked/upstream_not_passed", b.Status, b.Code)
	}
	if c.Status != Blocked {
		t.Errorf("c = %s, 期望 blocked", c.Status)
	}
	if rep.Overall == Passed {
		t.Error("整体结论不能是 passed")
	}

	// 全新执行器只重跑 b：前置 a 无记录，必须以 upstream_not_run 阻塞，不能假通过。
	rep2, err := newExec(t, set).Rerun(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := rep2.Record("b")
	if b2.Status != Blocked || b2.Code != "upstream_not_run" {
		t.Errorf("b = %s/%s, 期望 blocked/upstream_not_run", b2.Status, b2.Code)
	}
}

// 超时后执行器返回，慢检查 goroutine 必须完成收尾，不泄漏、不回写。
func TestTimeoutReapsRunner(t *testing.T) {
	var done sync.WaitGroup
	done.Add(1)
	set := mustSet(t,
		funcCheck("slow", nil, 50*time.Millisecond, func(ctx *Context) Result {
			<-ctx.Done()
			done.Done()
			return Pass("不应被采用")
		}),
		funcCheck("down", []string{"slow"}, time.Second, func(ctx *Context) Result {
			t.Fatal("down 不应执行")
			return Pass("x")
		}),
	)
	rep, _ := newExec(t, set).RunAll(context.Background())
	slow, _ := rep.Record("slow")
	if slow.Status != Timeout {
		t.Errorf("slow = %s, 期望 timeout", slow.Status)
	}
	if down, _ := rep.Record("down"); down.Status != Blocked {
		t.Errorf("down = %s, 期望 blocked", down.Status)
	}
	finishCh := make(chan struct{})
	go func() { done.Wait(); close(finishCh) }()
	select {
	case <-finishCh:
	case <-time.After(time.Second):
		t.Fatal("超时检查的 goroutine 未在取消后退出，资源泄漏")
	}
}

// 整轮取消：执行中的检查与后续检查分别记为 canceled。
func TestCancelPropagation(t *testing.T) {
	proceed := make(chan struct{})
	set := mustSet(t,
		funcCheck("a", nil, time.Second, func(ctx *Context) Result {
			select {
			case <-ctx.Done():
				return Pass("x") // 结果不应被采用，ctx 分支优先
			case <-proceed:
				return Pass("x")
			}
		}),
		funcCheck("b", nil, time.Second, func(ctx *Context) Result { return Pass("b") }),
	)
	ex := newExec(t, set)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	rep, _ := ex.RunAll(ctx)
	a, _ := rep.Record("a")
	b, _ := rep.Record("b")
	if a.Status != Canceled {
		t.Errorf("a = %s, 期望 canceled", a.Status)
	}
	if b.Status != Canceled {
		t.Errorf("b = %s, 期望 canceled", b.Status)
	}
}

// 重跑只触及所选失败项及其下游；无关项保留前次结果，旧历史批次不改写。
func TestRerunScopeAndHistoryRetention(t *testing.T) {
	state := map[string]Result{
		"a": Fail("out_of_range", "a 越界"),
		"b": Pass("ok"),
		"d": Pass("ok"),
	}
	var mu sync.Mutex
	get := func(id string) Result {
		mu.Lock()
		defer mu.Unlock()
		return state[id]
	}
	set := mustSet(t,
		funcCheck("a", nil, time.Second, func(ctx *Context) Result { return get("a") }),
		funcCheck("b", []string{"a"}, time.Second, func(ctx *Context) Result { return get("b") }),
		funcCheck("c", []string{"b"}, time.Second, func(ctx *Context) Result { return Pass("c") }),
		funcCheck("d", nil, time.Second, func(ctx *Context) Result { return get("d") }),
	)
	ex := newExec(t, set)
	first, err := ex.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Overall != Failed {
		t.Fatalf("首轮整体 = %s, 期望 failed", first.Overall)
	}

	mu.Lock()
	state["a"] = Pass("fixed")
	mu.Unlock()
	second, err := ex.Rerun(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[string]int{}
	for _, rec := range second.Records {
		attempts[rec.CheckID] = rec.Attempt
	}
	if attempts["a"] != 2 || attempts["b"] != 2 || attempts["c"] != 2 {
		t.Errorf("重跑范围错误，attempts=%v（a/b/c 应为 2）", attempts)
	}
	if attempts["d"] != 1 {
		t.Errorf("无关项 d 不应重跑，attempt=%d", attempts["d"])
	}
	if second.Overall != Passed {
		t.Errorf("恢复后整体 = %s, 期望 passed", second.Overall)
	}

	// 历史第一批必须原样保留。
	if ex.History().Len() != 2 {
		t.Fatalf("历史轮数 = %d, 期望 2", ex.History().Len())
	}
	old, _ := ex.History().Get(1)
	oldA, _ := old.Record("a")
	if oldA.Status != Failed {
		t.Errorf("旧批次 a 被改写为 %s", oldA.Status)
	}
	oldC, _ := old.Record("c")
	if oldC.Status != Blocked {
		t.Errorf("旧批次 c = %s, 期望仍为 blocked", oldC.Status)
	}
}

// 支持一次选择多个失败项重跑。
func TestRerunMultipleRoots(t *testing.T) {
	state := map[string]bool{"a": false, "d": false}
	set := mustSet(t,
		funcCheck("a", nil, time.Second, func(ctx *Context) Result {
			if state["a"] {
				return Pass("ok")
			}
			return Fail("out_of_range", "a")
		}),
		funcCheck("b", []string{"a"}, time.Second, func(ctx *Context) Result { return Pass("b") }),
		funcCheck("d", nil, time.Second, func(ctx *Context) Result {
			if state["d"] {
				return Pass("ok")
			}
			return Fault("sensor_missing", "d")
		}),
	)
	ex := newExec(t, set)
	if _, err := ex.RunAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state["a"], state["d"] = true, true
	rep, err := ex.Rerun(context.Background(), "a", "d")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "d"} {
		rec, _ := rep.Record(id)
		if rec.Status != Passed {
			t.Errorf("%s = %s, 期望 passed", id, rec.Status)
		}
	}
	if _, err := ex.Rerun(context.Background(), "unknown"); err == nil {
		t.Error("重跑不存在的检查项应当报错")
	}
}

// 检查自身 panic 归为 error/check_panic，而不是拖垮执行器。
func TestCheckPanicRecordedAsError(t *testing.T) {
	set := mustSet(t, funcCheck("boom", nil, time.Second, func(ctx *Context) Result {
		panic(errors.New("kaboom"))
	}))
	rep, _ := newExec(t, set).RunAll(context.Background())
	rec, _ := rep.Record("boom")
	if rec.Status != Error || rec.Code != "check_panic" {
		t.Errorf("boom = %s/%s, 期望 error/check_panic", rec.Status, rec.Code)
	}
}

func TestSetRejectsUnknownDepAndCycle(t *testing.T) {
	if _, err := NewSet("m", funcCheck("a", []string{"x"}, 0, nil)); err == nil {
		t.Error("依赖不存在时应报错")
	}
	s1 := funcCheck("a", []string{"b"}, 0, func(ctx *Context) Result { return Pass("") })
	s2 := funcCheck("b", []string{"a"}, 0, func(ctx *Context) Result { return Pass("") })
	set, _ := NewSet("m", s1, s2)
	if _, err := set.Order(); err == nil {
		t.Error("依赖成环时应报错")
	}
}
