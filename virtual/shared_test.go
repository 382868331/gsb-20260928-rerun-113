package virtual_test

import (
	"context"
	"testing"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

// 多设备共用仪器：两台设备争用同一台 analyzer，补充检查由各自结果触发，
// 声明所需仪器、预计占用与前置检查；调度器比较两种策略后按高风险优先兑现。
// 超时但尚未退出的读取继续占用仪器；迟到结果只归属原轮次；
// 上游修复仅取消尚未开始的承诺，独立有效结果继续复用。
func TestSharedInstrumentScheduling(t *testing.T) {
	const analyzer = "analyzer"
	// 设备 A：高风险；其探头读取对取消无响应（超时后仍占用仪器 400ms）。
	tempA := virtual.NewSensor("temp", 55, 10, 40)
	probeA := virtual.NewSensor("probeA", 3, 0, 100)
	probeA.SetDelay(400 * time.Millisecond)
	probeA.SetIgnoreContext(true)
	devA := virtual.NewDevice("dev-a", virtual.ModelID, map[string]*virtual.Sensor{
		"power":  virtual.NewSensor("power", 12, 11, 13),
		"temp":   tempA,
		"calib":  virtual.NewSensor("calib", .2, 0, 1),
		"probeA": probeA,
	})
	// 设备 B：低风险；探头正常快速。
	devB := virtual.NewDevice("dev-b", virtual.ModelID, map[string]*virtual.Sensor{
		"power":    virtual.NewSensor("power", 12, 11, 13),
		"temp":     virtual.NewSensor("temp", 55, 10, 40),
		"calib":    virtual.NewSensor("calib", .2, 0, 1),
		"probeB":   virtual.NewSensor("probeB", 3, 0, 100),
		"humidity": virtual.NewSensor("humidity", 40, 0, 80),
	})
	execA := buildExec(t, devA, nil)
	execB := buildExec(t, devB, nil)

	// 各自整轮自检：temperature 越界触发各自的补充检查承诺。
	rA, err := execA.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rB, err := execB.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, rA, "temperature", check.Failed, "out_of_range")
	assertRec(t, rB, "temperature", check.Failed, "out_of_range")
	evA := []check.Evidence{{CheckID: "temperature", Status: check.Failed, Code: "out_of_range"}}
	evB := []check.Evidence{{CheckID: "temperature", Status: check.Failed, Code: "out_of_range"}}

	sched := check.NewScheduler(check.PolicyHighRiskFirst, map[string]int{analyzer: 1})
	mustSubmit := func(exec *check.Executor, risk int, ev []check.Evidence, c check.Check) {
		t.Helper()
		if err := sched.SubmitCheck(exec, risk, "temperature 越界，追加探头深度诊断", ev, c); err != nil {
			t.Fatal(err)
		}
	}
	// 补充检查声明：所需仪器 analyzer、预计占用、必须先完成的检查 power。
	mustSubmit(execA, 10, evA, virtual.NewInstrumentedCheck("probe_a", "A 探头深度诊断",
		[]string{virtual.CheckPower}, 100*time.Millisecond, "probeA", 0, 100,
		check.ResourceNeed{Instrument: analyzer, Hold: 500 * time.Millisecond}))
	mustSubmit(execB, 1, evB, virtual.NewInstrumentedCheck("probe_b", "B 探头深度诊断",
		[]string{virtual.CheckPower}, 100*time.Millisecond, "probeB", 0, 100,
		check.ResourceNeed{Instrument: analyzer, Hold: 100 * time.Millisecond}))

	// 共享容量下比较两种安排：高风险优先让 A 先上，但总等待更高。
	arr := sched.Compare()
	if len(arr) != 2 {
		t.Fatalf("应给出两种安排: %+v", arr)
	}
	highRisk, minWait := arr[0], arr[1]
	if highRisk.Policy != check.PolicyHighRiskFirst || minWait.Policy != check.PolicyMinTotalWait {
		t.Fatalf("安排策略不对: %+v", arr)
	}
	if len(highRisk.Order) != 2 || highRisk.Order[0] != "dev-a/probe_a" {
		t.Fatalf("高风险优先应让 dev-a 先执行: %v", highRisk.Order)
	}
	if len(minWait.Order) != 2 || minWait.Order[0] != "dev-b/probe_b" {
		t.Fatalf("减少总等待应让 dev-b 先执行: %v", minWait.Order)
	}
	if minWait.TotalWait >= highRisk.TotalWait {
		t.Fatalf("减少总等待的总等待应更小: 高风险=%v 总等待=%v", highRisk.TotalWait, minWait.TotalWait)
	}

	// 按高风险优先兑现；运行期间核对资源占用与动态追加。
	runDone := make(chan error, 1)
	go func() { runDone <- sched.Run(context.Background()) }()

	// 200ms 时：A 的补充检查已超时定论（100ms 时限），但其读取要等满 400ms 才退出，
	// 仪器必须仍被 A 占用，不能当作空闲派给 B；此时取消 A 已来不及。
	time.Sleep(200 * time.Millisecond)
	if got := sched.InUse(analyzer); got != 1 {
		t.Fatalf("超时未退出的读取仍应占用仪器，InUse=%d", got)
	}
	if sched.Cancel("dev-a", "probe_a") {
		t.Fatal("已在执行的承诺不应被取消")
	}
	// 动态追加：B 的湿度扫描在调度进行中登记，应被纳入同一调度。
	mustSubmit(execB, 1, nil, virtual.NewInstrumentedCheck("humidity_scan", "B 湿度扫描",
		[]string{virtual.CheckPower}, 100*time.Millisecond, "humidity", 0, 80,
		check.ResourceNeed{Instrument: analyzer, Hold: 50 * time.Millisecond}))

	if err := <-runDone; err != nil {
		t.Fatal(err)
	}

	// 实际执行次序：A 先（高风险优先），且 B 只在 A 的读取真正退出后才拿到仪器。
	events := sched.Events()
	var startA, finishA, startB, finishB, startH time.Time
	maxInUse := 0
	for _, ev := range events {
		if ev.InUse > maxInUse {
			maxInUse = ev.InUse
		}
		switch {
		case ev.Kind == "start" && ev.CheckID == "probe_a":
			startA = ev.At
		case ev.Kind == "finish" && ev.CheckID == "probe_a":
			finishA = ev.At
		case ev.Kind == "start" && ev.CheckID == "probe_b":
			startB = ev.At
		case ev.Kind == "finish" && ev.CheckID == "probe_b":
			finishB = ev.At
		case ev.Kind == "start" && ev.CheckID == "humidity_scan":
			startH = ev.At
		}
	}
	for name, ts := range map[string]time.Time{"startA": startA, "finishA": finishA, "startB": startB, "finishB": finishB, "startH": startH} {
		if ts.IsZero() {
			t.Fatalf("缺少事件 %s: %+v", name, events)
		}
	}
	if maxInUse > 1 {
		t.Fatalf("仪器占用不得超过容量 1，实际峰值 %d", maxInUse)
	}
	if startB.Before(finishA) {
		t.Fatalf("B 在 A 的读取退出前就拿到的仪器: startB=%v finishA=%v", startB, finishA)
	}
	if startH.Before(finishB) {
		t.Fatalf("动态追加项应排在 B 探头之后: startH=%v finishB=%v", startH, finishB)
	}
	if got := sched.InUse(analyzer); got != 0 {
		t.Fatalf("调度结束后仪器应全部归还，InUse=%d", got)
	}

	// A 的补充检查超时定论且迟到结果只归属原轮次：记录仍是 timeout，未被改写。
	recA, ok := execA.RecordOf("probe_a")
	if !ok || recA.Status != check.Timeout || recA.Attempt != 1 {
		t.Fatalf("probe_a 应保持超时结论且 Attempt=1: %+v", recA)
	}
	if !recA.CleanupPending {
		t.Fatal("超时且读取未退出应标记 CleanupPending")
	}
	if got := execA.InFlight(); len(got) != 0 {
		t.Fatalf("调度归还仪器后在途应为空: %v", got)
	}
	assertRec(t, mustRecord(t, execB, "probe_b"), "probe_b", check.Passed, "ok")
	assertRec(t, mustRecord(t, execB, "humidity_scan"), "humidity_scan", check.Passed, "ok")

	// 上游修复：A 的 temperature 恢复，仅 probe_a 旧结论失效；
	// power 等独立有效结果继续复用（Attempt 不增加），B 完全不受影响。
	tempA.SetValue(25)
	if _, err := execA.Rerun(context.Background(), virtual.CheckTemp); err != nil {
		t.Fatal(err)
	}
	inv := execA.Invalidated()
	if len(inv) != 1 || inv[0].CheckID != "probe_a" {
		t.Fatalf("失效范围应只含 probe_a: %+v", inv)
	}
	if pow, _ := execA.RecordOf("power"); pow.Attempt != 1 {
		t.Fatalf("power 结论应继续复用，Attempt=%d", pow.Attempt)
	}
	if got := execB.Invalidated(); len(got) != 0 {
		t.Fatalf("设备 B 不应受 A 修复影响: %+v", got)
	}

	// 补充项失效后：仅取消尚未开始的承诺；已登记的检查不注册、不执行。
	if err := sched.SubmitCheck(execA, 10, "探头复核", nil, virtual.NewInstrumentedCheck(
		"probe_a_extra", "A 探头复核", []string{virtual.CheckPower},
		100*time.Millisecond, "probeA", 0, 100,
		check.ResourceNeed{Instrument: analyzer, Hold: 100 * time.Millisecond})); err != nil {
		t.Fatal(err)
	}
	if !sched.Cancel("dev-a", "probe_a_extra") {
		t.Fatal("尚未开始的承诺应可取消")
	}
	if err := sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := execA.RecordOf("probe_a_extra"); ok {
		t.Fatal("已取消的承诺不应注册或执行")
	}
	if item, _ := execA.PlanLog().Current().Item("probe_a_extra"); item.CheckID != "" {
		t.Fatal("已取消的承诺不应进入计划历史")
	}

	// 迟到结果不污染后续轮次：修复探头响应后单独重跑 probe_a 通过，
	// 旧的超时批次原样保留在历史中。
	probeA.SetIgnoreContext(false)
	probeA.SetDelay(0)
	rFix, err := execA.Rerun(context.Background(), "probe_a")
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, rFix, "probe_a", check.Passed, "ok")
	if got := execA.Invalidated(); len(got) != 0 {
		t.Fatalf("重跑后不应再有失效结论: %+v", got)
	}
	if err := devA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := devB.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustRecord(t *testing.T, exec *check.Executor, id string) *check.Report {
	t.Helper()
	r, ok := exec.History().Latest()
	if !ok {
		t.Fatal("缺少历史报告")
	}
	if _, ok := r.Record(id); !ok {
		t.Fatalf("报告中缺少 %q", id)
	}
	return r
}
