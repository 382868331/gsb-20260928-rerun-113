package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"devicecheck/check"
	"devicecheck/virtual"
)

// 场景五：条件计划 —— 失败结果决定补充检查项，计划调整留痕、可保存重开并对比。
func runCond() {
	fmt.Println("\n########## 场景 cond：条件计划 + 补充检查 + 失效标记 ##########")
	temp := virtual.NewSensor("temp", 55.0, 10.0, 40.0) // 首轮越界
	dev := virtual.NewDevice("dev-cond", virtual.ModelID, map[string]*virtual.Sensor{
		"power": virtual.NewSensor("power", 12.0, 11.0, 13.0),
		"temp":  temp,
		"calib": virtual.NewSensor("calib", 0.2, 0.0, 1.0),
		"probe": virtual.NewSensor("probe", 3.0, 0.0, 100.0),
	})

	exec, _ := newExecutor(dev)
	must(exec.SetRules(virtual.CondRules()...))
	fmt.Println("初始计划（固定检查集）:", planIDs(exec.PlanLog().Current()))

	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("首轮：temperature 越界，触发补充检查", r1)
	must(assertStatus(r1, "temperature", check.Failed, "out_of_range"))
	must(assertStatus(r1, virtual.CheckTempProbe, check.Passed, "ok"))
	fmt.Println("调整后计划:", planIDs(exec.PlanLog().Current()))
	item, _ := exec.PlanLog().Current().Item(virtual.CheckTempProbe)
	fmt.Printf("补充项 %q 加入原因: %s\n", item.CheckID, item.Reason)
	for _, ev := range item.Evidence {
		fmt.Printf("  加入时依据: %s=%s/%s %s\n", ev.CheckID, ev.Status, ev.Code, ev.Message)
	}

	// 计划历史保存到文件并重新打开，解释当前计划与历史计划的差别。
	path := filepath.Join(os.TempDir(), "devicecheck-cond-plan.json")
	must(exec.PlanLog().Save(path))
	loaded, err := check.LoadPlanLog(path)
	must(err)
	v1, _ := loaded.Get(1)
	fmt.Println("保存并重新打开后:", check.DiffPlans(v1, loaded.Current()))

	// 修复设备：只重跑 temperature 及受影响下游；power 通过结论复用；
	// probe 不在受影响范围内，其旧结论被明确标记失效。
	temp.SetValue(25.0)
	affected, err := exec.Affected(virtual.CheckTemp)
	must(err)
	fmt.Printf("修复后重跑 %q 的范围: %v（power 复用前次通过结论）\n", virtual.CheckTemp, affected)
	r2, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	must(err)
	printReport("重跑：temperature 恢复", r2)
	for _, inv := range exec.Invalidated() {
		fmt.Printf("旧结论失效: %s（%s）\n", inv.CheckID, inv.Reason)
	}

	// 单独重跑 probe，失效标记清除；历史批次与计划版本均未被改写。
	r3, err := exec.Rerun(context.Background(), virtual.CheckTempProbe)
	must(err)
	printReport("重跑：probe 单独确认", r3)
	if got := exec.Invalidated(); len(got) != 0 {
		must(fmt.Errorf("probe 重跑后不应再有失效结论: %+v", got))
	}
	must(dev.Close())
	old, ok := exec.History().Get(1)
	if !ok {
		must(fmt.Errorf("首轮历史丢失"))
	}
	must(assertStatus(old, "temperature", check.Failed, "out_of_range"))
	printHistory(exec.History())
}

func planIDs(p *check.Plan) []string {
	ids := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		ids = append(ids, it.CheckID)
	}
	return ids
}
