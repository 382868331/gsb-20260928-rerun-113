package virtual_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"devicecheck/check"
	"devicecheck/virtual"
)

// 条件计划：temperature 越界触发补充检查加入计划，记录加入原因与当时依据；
// 修复后只重跑受影响项，无关通过结论复用；上游变化后旧结论被明确标记失效。
func TestConditionalPlanAndInvalidation(t *testing.T) {
	temp := virtual.NewSensor("temp", 55, 10, 40) // 首轮越界
	probe := virtual.NewSensor("probe", 3, 0, 100)
	dev := virtual.NewDevice("d", virtual.ModelID, map[string]*virtual.Sensor{
		"power": virtual.NewSensor("power", 12, 11, 13),
		"temp":  temp,
		"calib": virtual.NewSensor("calib", .2, 0, 1),
		"probe": probe,
	})
	exec := buildExec(t, dev, nil)
	if err := exec.SetRules(virtual.CondRules()...); err != nil {
		t.Fatal(err)
	}

	r1, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r1, "temperature", check.Failed, "out_of_range")
	// 补充检查被自动加入并在同一轮执行。
	assertRec(t, r1, virtual.CheckTempProbe, check.Passed, "ok")

	// 计划从 v1 调整到 v2，每项加入都有原因与当时的依赖结论。
	plans := exec.PlanLog()
	if plans.Len() != 2 {
		t.Fatalf("计划应调整到 2 个版本，得到 %d", plans.Len())
	}
	item, ok := plans.Current().Item(virtual.CheckTempProbe)
	if !ok || item.Since != 2 {
		t.Fatalf("补充项未记录或版本不对: %+v", item)
	}
	if !strings.Contains(item.Reason, "越界") {
		t.Fatalf("补充项缺少加入原因: %+v", item)
	}
	if len(item.Evidence) == 0 || item.Evidence[0].CheckID != "temperature" ||
		item.Evidence[0].Status != check.Failed || item.Evidence[0].Code != "out_of_range" {
		t.Fatalf("补充项未记录当时依赖的检查结论: %+v", item.Evidence)
	}

	// 计划历史保存并重新打开后，能解释当前计划与历史计划的差别。
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plans.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := check.LoadPlanLog(path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := loaded.Get(1)
	diff := check.DiffPlans(v1, loaded.Current())
	if len(diff.Added) != 1 || diff.Added[0].CheckID != virtual.CheckTempProbe {
		t.Fatalf("重开后 diff 应指出新增 %q: %+v", virtual.CheckTempProbe, diff)
	}
	if !strings.Contains(diff.String(), "out_of_range") {
		t.Fatalf("diff 说明应包含当时依据:\n%s", diff)
	}

	// 修复设备：只重跑 temperature 及受影响下游。
	// power 的通过结论复用（Attempt 不增加）；probe 与 temperature 无图依赖，
	// 不在重跑范围内，其旧结论必须被明确标记失效。
	temp.SetValue(25)
	r2, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r2, "temperature", check.Passed, "ok")
	assertRec(t, r2, "calibration", check.Passed, "ok")
	pow, _ := r2.Record("power")
	if pow.Attempt != 1 {
		t.Fatalf("power 不应被重跑，Attempt=%d", pow.Attempt)
	}
	pr, _ := r2.Record(virtual.CheckTempProbe)
	if pr.Attempt != 1 {
		t.Fatalf("probe 不在受影响范围内，不应重跑，Attempt=%d", pr.Attempt)
	}
	inv := exec.Invalidated()
	if len(inv) != 1 || inv[0].CheckID != virtual.CheckTempProbe {
		t.Fatalf("probe 旧结论应被标记失效: %+v", inv)
	}
	if !strings.Contains(inv[0].Reason, "temperature") || !strings.Contains(inv[0].Reason, "failed") {
		t.Fatalf("失效原因应说明上游结论变化: %s", inv[0].Reason)
	}
	if len(r2.Stale) != 1 || r2.Stale[0] != virtual.CheckTempProbe {
		t.Fatalf("报告应列出失效项: %v", r2.Stale)
	}

	// 单独重跑 probe 后失效标记清除；历史批次不被改写。
	r3, err := exec.Rerun(context.Background(), virtual.CheckTempProbe)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r3, virtual.CheckTempProbe, check.Passed, "ok")
	if got := exec.Invalidated(); len(got) != 0 {
		t.Fatalf("重跑后不应再有失效结论: %+v", got)
	}
	old, _ := exec.History().Get(1)
	assertRec(t, old, "temperature", check.Failed, "out_of_range")
	if exec.History().Len() != 3 {
		t.Fatalf("历史应有 3 轮，得到 %d", exec.History().Len())
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
}

// 规则定义校验：触发项不存在、补充项重复、依赖缺失都必须报错。
func TestSetRulesValidation(t *testing.T) {
	dev := healthyDevice("d", nil, nil, nil)
	exec := buildExec(t, dev, nil)
	bad := check.Rule{Trigger: "nope", Adds: nil, Reason: "x"}
	if err := exec.SetRules(bad); err == nil {
		t.Fatal("触发检查不存在应报错")
	}
	dup := check.Rule{
		Trigger: virtual.CheckTemp,
		Adds:    []check.Check{virtual.NewSensorCheck(virtual.CheckPower, "x", nil, 0, "power", 0, 1)},
		Reason:  "x",
	}
	if err := exec.SetRules(dup); err == nil {
		t.Fatal("补充检查与既有检查重复应报错")
	}
	missingDep := check.Rule{
		Trigger: virtual.CheckTemp,
		Adds:    []check.Check{virtual.NewSensorCheck("extra", "x", []string{"ghost"}, 0, "probe", 0, 1)},
		Reason:  "x",
	}
	if err := exec.SetRules(missingDep); err == nil {
		t.Fatal("补充检查依赖不存在应报错")
	}
	if err := exec.SetRules(virtual.CondRules()...); err != nil {
		t.Fatalf("合法规则不应报错: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
}
