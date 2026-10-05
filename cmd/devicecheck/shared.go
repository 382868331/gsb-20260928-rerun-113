package main

import (
	"context"
	"fmt"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

// 场景六：多设备共用仪器 —— 两台设备争用同一台 analyzer，
// 补充检查声明所需仪器/预计占用/前置检查，调度器比较两种策略后兑现；
// 超时未退出的读取继续占用仪器；上游修复仅取消尚未开始的承诺。
func runShared() {
	fmt.Println("\n########## 场景 shared：多设备共用仪器 + 条件计划调度 ##########")
	const analyzer = "analyzer"
	tempA := virtual.NewSensor("temp", 55.0, 10.0, 40.0) // 越界，触发补充检查
	probeA := virtual.NewSensor("probeA", 3.0, 0.0, 100.0)
	probeA.SetDelay(400 * time.Millisecond) // 读取 400ms 才退出
	probeA.SetIgnoreContext(true)           // 且对取消/超时无响应
	devA := virtual.NewDevice("dev-a", virtual.ModelID, map[string]*virtual.Sensor{
		"power":  virtual.NewSensor("power", 12.0, 11.0, 13.0),
		"temp":   tempA,
		"calib":  virtual.NewSensor("calib", 0.2, 0.0, 1.0),
		"probeA": probeA,
	})
	devB := virtual.NewDevice("dev-b", virtual.ModelID, map[string]*virtual.Sensor{
		"power":  virtual.NewSensor("power", 12.0, 11.0, 13.0),
		"temp":   virtual.NewSensor("temp", 55.0, 10.0, 40.0),
		"calib":  virtual.NewSensor("calib", 0.2, 0.0, 1.0),
		"probeB": virtual.NewSensor("probeB", 3.0, 0.0, 100.0),
	})
	execA, _ := newExecutor(devA)
	execB, _ := newExecutor(devB)

	rA, err := execA.RunAll(context.Background())
	must(err)
	rB, err := execB.RunAll(context.Background())
	must(err)
	printReport("dev-a 整轮：temperature 越界", rA)
	printReport("dev-b 整轮：temperature 越界", rB)

	// 各自结果触发补充检查承诺：声明仪器、预计占用与前置检查。
	sched := check.NewScheduler(check.PolicyHighRiskFirst, map[string]int{analyzer: 1})
	ev := []check.Evidence{{CheckID: "temperature", Status: check.Failed, Code: "out_of_range"}}
	must(sched.SubmitCheck(execA, 10, "temperature 越界，追加探头深度诊断", ev,
		virtual.NewInstrumentedCheck("probe_a", "A 探头深度诊断", []string{virtual.CheckPower},
			100*time.Millisecond, "probeA", 0, 100,
			check.ResourceNeed{Instrument: analyzer, Hold: 500 * time.Millisecond})))
	must(sched.SubmitCheck(execB, 1, "temperature 越界，追加探头深度诊断", ev,
		virtual.NewInstrumentedCheck("probe_b", "B 探头深度诊断", []string{virtual.CheckPower},
			100*time.Millisecond, "probeB", 0, 100,
			check.ResourceNeed{Instrument: analyzer, Hold: 100 * time.Millisecond})))

	// 共享容量下比较两种安排。
	fmt.Println("\n两种安排对比（共享 analyzer，容量 1）：")
	for _, arr := range sched.Compare() {
		fmt.Printf("  %-10s 次序=%v 总等待=%v\n", arr.Policy, arr.Order, arr.TotalWait)
	}
	fmt.Println("选定策略：高风险设备优先（dev-a 风险分 10 > dev-b 风险分 1）")

	runDone := make(chan error, 1)
	go func() { runDone <- sched.Run(context.Background()) }()

	// A 的补充检查 100ms 超时定论，但其读取 400ms 才退出：
	// 期间仪器仍被 A 占用，不能当作空闲派给 B；取消 A 也已来不及。
	time.Sleep(200 * time.Millisecond)
	fmt.Printf("\nA 超时定论后、读取退出前：analyzer 占用=%d（仍归 A，不能派给 B）\n", sched.InUse(analyzer))
	if sched.Cancel("dev-a", "probe_a") {
		must(fmt.Errorf("已在执行的承诺不应被取消"))
	}
	fmt.Println("尝试取消执行中的 dev-a/probe_a：已来不及，承诺继续兑现")
	must(<-runDone)

	fmt.Println("\n调度审计（实际执行次序与资源占用）：")
	for _, e := range sched.Events() {
		fmt.Printf("  %-6s %-16s %-9s 占用=%d\n", e.Kind, e.DeviceID+"/"+e.CheckID, e.Instrument, e.InUse)
	}
	if got := sched.InUse(analyzer); got != 0 {
		must(fmt.Errorf("调度结束后仪器应全部归还，InUse=%d", got))
	}
	recA, _ := execA.RecordOf("probe_a")
	fmt.Printf("dev-a/probe_a 结论 %s/%s（迟到结果只归属原轮次，记录未被改写）\n", recA.Status, recA.Code)
	recB, _ := execB.RecordOf("probe_b")
	fmt.Printf("dev-b/probe_b 结论 %s/%s（在 A 的读取退出后才拿到仪器）\n", recB.Status, recB.Code)

	// 上游修复：仅 probe_a 旧结论失效；尚未开始的承诺可取消，独立结果继续复用。
	tempA.SetValue(25.0)
	rFix, err := execA.Rerun(context.Background(), virtual.CheckTemp)
	must(err)
	printReport("dev-a 修复 temperature 后重跑", rFix)
	for _, inv := range execA.Invalidated() {
		fmt.Printf("旧结论失效: %s（%s）\n", inv.CheckID, inv.Reason)
	}
	must(sched.SubmitCheck(execA, 10, "探头复核", nil,
		virtual.NewInstrumentedCheck("probe_a_extra", "A 探头复核", []string{virtual.CheckPower},
			100*time.Millisecond, "probeA", 0, 100,
			check.ResourceNeed{Instrument: analyzer, Hold: 100 * time.Millisecond})))
	if !sched.Cancel("dev-a", "probe_a_extra") {
		must(fmt.Errorf("尚未开始的承诺应可取消"))
	}
	fmt.Println("补充项失效后：仅取消尚未开始的承诺 probe_a_extra（不注册、不执行）")
	must(sched.Run(context.Background()))
	if _, ok := execA.RecordOf("probe_a_extra"); ok {
		must(fmt.Errorf("已取消的承诺不应执行"))
	}
	if pow, _ := execA.RecordOf("power"); pow.Attempt != 1 {
		must(fmt.Errorf("power 独立有效结果应继续复用，Attempt=%d", pow.Attempt))
	}
	fmt.Println("power 等独立有效结果继续复用（Attempt 未增加），dev-b 完全不受影响")

	// 探头恢复响应后单独重跑，失效标记清除；历史批次不被改写。
	probeA.SetIgnoreContext(false)
	probeA.SetDelay(0)
	rRe, err := execA.Rerun(context.Background(), "probe_a")
	must(err)
	printReport("dev-a 探头恢复后重跑 probe_a", rRe)
	must(assertStatus(rRe, "probe_a", check.Passed, "ok"))
	if got := execA.Invalidated(); len(got) != 0 {
		must(fmt.Errorf("重跑后不应再有失效结论: %+v", got))
	}
	must(execA.WaitIdle(context.Background()))
	must(devA.Close())
	must(devB.Close())
	printHistory(execA.History())
}
