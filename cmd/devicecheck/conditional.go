package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

// condRules 返回演示用条件分支规则：temperature 未通过时补充两项诊断，
// temperature 恢复通过后补充一项趋势检查（演示重开后新发现的检查项）。
func condRules() []check.Rule {
	return []check.Rule{
		{
			Reason: "temperature 未通过，触发补充诊断",
			When:   func(rec check.Record) bool { return rec.CheckID == "temperature" && rec.Status != check.Passed },
			Add: func() []check.Check {
				return []check.Check{
					virtual.NewSensorCheck("temp_channel_diag", "温度通道诊断",
						[]string{"power"}, 500*time.Millisecond, "tempdiag", 0, 100),
					virtual.NewSensorCheck("temp_deep_diag", "温度深度诊断",
						[]string{"temperature"}, 500*time.Millisecond, "tempdeep", 0, 100),
				}
			},
		},
		{
			Reason: "temperature 恢复通过，补充趋势检查",
			When:   func(rec check.Record) bool { return rec.CheckID == "temperature" && rec.Status == check.Passed },
			Add: func() []check.Check {
				return []check.Check{
					virtual.NewSensorCheck("temp_trend", "温度趋势检查",
						[]string{"temperature"}, 500*time.Millisecond, "temptrend", 0, 100),
				}
			},
		},
	}
}

// condSet 基础检查集：power / temperature / calibration 主链 + 独立分支 fan。
func condSet() *check.CheckSet {
	set, err := check.NewSet(virtual.ModelID,
		virtual.NewSensorCheck("power", "电源电压检测", nil, 500*time.Millisecond, "power", 11, 13),
		virtual.NewSensorCheck("temperature", "舱内温度检测", []string{"power"}, 500*time.Millisecond, "temp", 10, 40),
		virtual.NewSensorCheck("calibration", "校准一致性检查", []string{"temperature"}, 500*time.Millisecond, "calib", 0, 1),
		virtual.NewSensorCheck("fan", "风扇转速检测", nil, 500*time.Millisecond, "fan", 1000, 3000),
	)
	must(err)
	return set
}

func condDevice(id string, temp *virtual.Sensor) *virtual.Device {
	return virtual.NewDevice(id, virtual.ModelID, map[string]*virtual.Sensor{
		"power":     virtual.NewSensor("power", 12.0, 11.0, 13.0),
		"temp":      temp,
		"calib":     virtual.NewSensor("calib", 0.2, 0.0, 1.0),
		"fan":       virtual.NewSensor("fan", 1500, 1000, 3000),
		"tempdiag":  virtual.NewSensor("tempdiag", 1, 0, 100),
		"tempdeep":  virtual.NewSensor("tempdeep", 2, 0, 100),
		"temptrend": virtual.NewSensor("temptrend", 3, 0, 100),
	})
}

// printPlan 打印可查看的条件检查计划：每项为何加入、当前状态与结论。
func printPlan(title string, p *check.Plan) {
	fmt.Printf("\n--- %s ---\n", title)
	fmt.Printf("%-18s %-9s %-26s %s\n", "检查项", "状态", "加入原因", "结论")
	for _, item := range p.Items() {
		conclusion := "（待执行，不计为通过）"
		if item.Record != nil {
			conclusion = fmt.Sprintf("#%d %s/%s %s", item.Record.Attempt, item.Record.Status, item.Record.Code, item.Record.Message)
		}
		fmt.Printf("%-18s %-9s %-26s %s\n", item.CheckID, item.Status, item.Reason, conclusion)
	}
}

// 场景五：条件检查计划 —— 失败触发补充检查、修复重测、计划保存重开。
func runCond() {
	fmt.Println("\n########## 场景 cond：条件计划 + 修复重测 + 计划保存重开 ##########")
	temp := virtual.NewSensor("temp", 25.0, 10.0, 40.0)
	temp.SetDelay(1500 * time.Millisecond) // 远超 500ms 时限
	temp.SetIgnoreContext(true)            // 读取对取消无响应
	dev := condDevice("dev-cond", temp)

	history := &check.History{}
	exec, err := check.NewExecutor(dev, condSet(), history)
	must(err)
	exec.OnResult(condRules()...)

	// 第一轮：temperature 无响应超时 → 触发补充诊断；无关分支 fan 照常完成。
	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("第 1 轮：temperature 无响应，失败触发补充检查", r1)
	must(assertStatus(r1, "temperature", check.Timeout, "timeout"))
	must(assertStatus(r1, "calibration", check.Blocked, "upstream_not_passed"))
	must(assertStatus(r1, "fan", check.Passed, "ok")) // 无关分支仍可完成
	must(assertStatus(r1, "temp_channel_diag", check.Passed, "ok"))
	must(assertStatus(r1, "temp_deep_diag", check.Blocked, "upstream_not_passed"))
	tRec, _ := r1.Record("temperature")
	if !tRec.CleanupPending {
		must(fmt.Errorf("无响应读取应保留 CleanupPending 收尾标记"))
	}
	fmt.Printf("清理进展：在途=%v（结论已出，资源收尾独立进行）\n", exec.InFlight())
	printPlan("条件检查计划（可查看）", exec.Plan())

	// 保存计划到文件，稍后“重开”。
	planPath := filepath.Join(os.TempDir(), "devicecheck-cond-plan.json")
	f, err := os.Create(planPath)
	must(err)
	must(exec.Plan().Save(f))
	must(f.Close())
	fmt.Printf("计划已保存: %s\n", planPath)

	// 修复设备，立刻重测受影响部分：旧读取未收回前不启动新读取。
	temp.SetIgnoreContext(false)
	temp.SetDelay(0)
	type rerunOut struct {
		r   *check.Report
		err error
	}
	outCh := make(chan rerunOut, 1)
	go func() {
		r, err := exec.Rerun(context.Background(), "temperature")
		outCh <- rerunOut{r, err}
	}()
	ticker := time.NewTicker(300 * time.Millisecond)
	var out rerunOut
	for done := false; !done; {
		select {
		case out = <-outCh:
			done = true
		case <-ticker.C:
			fmt.Printf("清理进展：在途=%v（重跑等待旧读取收尾，绝不重叠）\n", exec.InFlight())
		}
	}
	ticker.Stop()
	must(out.err)
	r2 := out.r
	printReport("第 2 轮：修复后仅重测 temperature 及受影响下游", r2)
	must(assertStatus(r2, "temperature", check.Passed, "ok"))
	must(assertStatus(r2, "calibration", check.Passed, "ok"))
	must(assertStatus(r2, "temp_deep_diag", check.Passed, "ok"))
	must(assertStatus(r2, "temp_trend", check.Passed, "ok")) // 恢复通过后新发现
	fanRec, _ := r2.Record("fan")
	if fanRec.Attempt != 1 {
		must(fmt.Errorf("无关分支 fan 不应被重测，Attempt=%d", fanRec.Attempt))
	}
	must(exec.WaitIdle(context.Background()))
	must(dev.Close())

	// 上游再次变化：温度越界，重测时指出哪些下游结论已过期。
	temp.SetValue(55.0)
	r3, err := exec.Rerun(context.Background(), "temperature")
	must(err)
	printReport("第 3 轮：temperature 再次变化（越界）", r3)
	must(assertStatus(r3, "temperature", check.Failed, "out_of_range"))
	fmt.Printf("上游再次变化，以下下游结论已过期: %v\n", r3.Invalidated)
	if len(r3.Invalidated) == 0 {
		must(fmt.Errorf("应指出过期的下游结论"))
	}
	must(dev.Close())

	// 计划重开：模拟进程重启 —— 新执行器 + 同一历史，从文件恢复计划。
	savedFile, err := os.Open(planPath)
	must(err)
	saved, err := check.LoadPlan(savedFile)
	must(err)
	must(savedFile.Close())
	exec2, err := check.NewExecutor(dev, condSet(), history)
	must(err)
	exec2.OnResult(condRules()...)
	exec2.AdoptPlan(saved)
	printPlan("重开后的计划（历史结论带原依据恢复）", exec2.Plan())
	item, ok := exec2.Plan().Get("temperature")
	if !ok || item.Record == nil || item.Record.Status != check.Timeout || item.Record.Attempt != 1 {
		must(fmt.Errorf("重开后应恢复第 1 轮 temperature 结论: %+v", item))
	}

	// 重开后修复上游，仅重测受影响部分；历史轮次接续。
	temp.SetValue(25.0)
	r4, err := exec2.Rerun(context.Background(), "temperature")
	must(err)
	printReport("第 4 轮（重开后）：修复 temperature，仅重测受影响部分", r4)
	must(assertStatus(r4, "temperature", check.Passed, "ok"))
	must(assertStatus(r4, "calibration", check.Passed, "ok"))
	fanRec2, _ := r4.Record("fan")
	if fanRec2.Attempt != 1 {
		must(fmt.Errorf("重开后 fan 应保留第 1 轮结论，Attempt=%d", fanRec2.Attempt))
	}
	// 重开恢复的旧结论中，依据已过期的下游会被指出，需按需重测。
	if stale := exec2.Stale(); len(stale) > 0 {
		fmt.Printf("重开后发现过期结论（依据的上游已变化）: %v\n", stale)
	}
	staleFound := false
	for _, id := range exec2.Stale() {
		if id == "temp_deep_diag" {
			staleFound = true
		}
	}
	if !staleFound {
		must(fmt.Errorf("temp_deep_diag 的旧结论应被指出已过期"))
	}
	// 重开的执行器还没有该项的检查定义（规则未触发过），先恢复定义再重测。
	must(exec2.Register("重开后恢复检查定义",
		virtual.NewSensorCheck("temp_deep_diag", "温度深度诊断",
			[]string{"temperature"}, 500*time.Millisecond, "tempdeep", 0, 100)))
	r5, err := exec2.Rerun(context.Background(), "temp_deep_diag")
	must(err)
	printReport("第 5 轮：按提示重测过期项 temp_deep_diag", r5)
	must(assertStatus(r5, "temp_deep_diag", check.Passed, "ok"))
	if stale := exec2.Stale(); len(stale) != 0 {
		must(fmt.Errorf("重测后不应再有过期结论: %v", stale))
	}
	must(exec2.WaitIdle(context.Background()))
	must(dev.Close())

	// 历史计划文件不被后来的执行覆盖；新发现的检查项也不被历史计划覆盖。
	again, err := os.Open(planPath)
	must(err)
	reloaded, err := check.LoadPlan(again)
	must(err)
	must(again.Close())
	oldItem, _ := reloaded.Get("temperature")
	if oldItem.Record == nil || oldItem.Record.Status != check.Timeout {
		must(fmt.Errorf("已保存的历史计划被新结论覆盖"))
	}
	exec2.AdoptPlan(reloaded) // 再次合并：互不覆盖
	if _, ok := exec2.Plan().Get("temp_trend"); !ok {
		must(fmt.Errorf("新发现的检查项 temp_trend 被历史计划覆盖"))
	}
	cur, _ := exec2.Plan().Get("temperature")
	if cur.Record == nil || cur.Record.Status != check.Passed {
		must(fmt.Errorf("当前结论不应被历史计划回退"))
	}
	fmt.Println("计划保存重开校验通过：历史计划与新发现项互不覆盖")
	printHistory(history)
}
