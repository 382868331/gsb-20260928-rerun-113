// Command devicecheck 是设备自检执行器的可运行演示入口。
//
// 用法：
//
//	go run ./cmd/devicecheck [场景]
//
// 场景（默认 all）：
//
//	missing  电源传感器缺失：power=error/sensor_missing，下游 temperature、
//	         calibration 记为 blocked（不会假装通过）；随后安装传感器重跑 power，
//	         受影响下游自动重新检查，历史保留两个批次。
//	slow     温度传感器慢响应：首轮 temperature=timeout、calibration=blocked；
//	         整轮可被 Ctrl-C 取消（演示 canceled 与资源收尾）；恢复后只重跑
//	         temperature 及其下游，power 的前次通过结论保留。
//	range    温度读数越界（failed/out_of_range）与传感器恢复正常读数的对比，
//	         演示“读数越界”与“传感器不存在/检查报错”是不同结论。
//	all      依次演示上述场景。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: devicecheck [missing|slow|range|all]")
		flag.PrintDefaults()
	}
	flag.Parse()
	scene := flag.Arg(0)
	if scene == "" {
		scene = "all"
	}
	switch scene {
	case "missing":
		runMissing()
	case "slow":
		runSlow()
	case "range":
		runRange()
	case "all":
		runMissing()
		runSlow()
		runRange()
	default:
		fmt.Fprintf(os.Stderr, "未知场景 %q（可选 missing|slow|range|all）\n", scene)
		os.Exit(2)
	}
}

// printReport 以表格打印一轮报告，并给出各状态计数。
func printReport(title string, r *check.Report) {
	fmt.Printf("\n=== %s（批次 #%d，整体 %s）===\n", title, r.RunID, r.Overall)
	fmt.Printf("%-14s %-9s %-20s %-9s %s\n", "检查项", "状态", "代码", "次数", "说明")
	for _, rec := range r.Records {
		fmt.Printf("%-14s %-9s %-20s %-9d %s\n", rec.CheckID, rec.Status, rec.Code, rec.Attempt, rec.Message)
	}
	c := r.Counts()
	fmt.Printf("计数: passed=%d failed=%d error=%d blocked=%d timeout=%d canceled=%d\n",
		c[check.Passed], c[check.Failed], c[check.Error], c[check.Blocked], c[check.Timeout], c[check.Canceled])
}

func printHistory(h *check.History) {
	fmt.Printf("历史批次共 %d 轮：", h.Len())
	for i := 1; i <= h.Len(); i++ {
		r, _ := h.Get(i)
		fmt.Printf("#%d=%s ", i, r.Overall)
	}
	fmt.Println()
}

func newExecutor(dev *virtual.Device) (*check.Executor, *check.CheckSet) {
	set, err := virtual.NewSet()
	must(err)
	exec, err := check.NewExecutor(dev, set, nil)
	must(err)
	return exec, set
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// 场景一：传感器缺失 → 安装恢复 → 只重跑失败项及受影响下游。
func runMissing() {
	fmt.Println("\n########## 场景 missing：传感器缺失 + 恢复重跑 ##########")
	power := virtual.NewSensor("power", 12.0, 11.0, 13.0)
	temp := virtual.NewSensor("temp", 25.0, 10.0, 40.0)
	calib := virtual.NewSensor("calib", 0.2, 0.0, 1.0)
	// 设备上不注册 power 传感器，模拟未安装/地址不存在。
	dev := virtual.NewDeviceWithSensors("dev-missing", nil, temp, calib)

	exec, _ := newExecutor(dev)
	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("首轮：power 传感器缺失", r1)
	must(dev.Close())
	must(assertStatus(r1, "power", check.Error, "sensor_missing"))
	must(assertStatus(r1, "temperature", check.Blocked, "upstream_not_passed"))
	must(assertStatus(r1, "calibration", check.Blocked, "upstream_not_passed"))

	// 运维安装电源传感器（恢复），只重跑 power；受影响下游自动纳入。
	dev2 := virtual.NewDeviceWithSensors("dev-missing", power, temp, calib)
	exec2, _ := newExecutorWithHistory(dev2, exec.History())
	affected, err := exec2.Affected(virtual.CheckPower)
	must(err)
	fmt.Printf("重跑 %q 的范围: %v（其余项保留前次结论）\n", virtual.CheckPower, affected)
	r2, err := exec2.Rerun(context.Background(), virtual.CheckPower)
	must(err)
	printReport("重跑：power 及受影响下游", r2)
	must(dev2.Close())
	must(assertStatus(r2, "power", check.Passed, "ok"))
	must(assertStatus(r2, "temperature", check.Passed, "ok"))
	must(assertStatus(r2, "calibration", check.Passed, "ok"))
	printHistory(exec2.History())
}

// 场景二：慢响应导致超时；外部取消；恢复后按受影响范围重跑。
func runSlow() {
	fmt.Println("\n########## 场景 slow：慢响应超时 + 取消 + 恢复重跑 ##########")
	power := virtual.NewSensor("power", 12.0, 11.0, 13.0)
	temp := virtual.NewSensor("temp", 25.0, 10.0, 40.0)
	calib := virtual.NewSensor("calib", 0.2, 0.0, 1.0)
	temp.SetDelay(5 * time.Second) // 远大于检查项 500ms 时限
	dev := virtual.NewDeviceWithSensors("dev-slow", power, temp, calib)

	exec, _ := newExecutor(dev)
	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("首轮：temperature 慢响应超时", r1)
	must(dev.Close()) // 关键：超时后传感器句柄必须全部释放，无在途读取
	must(assertStatus(r1, "power", check.Passed, "ok"))
	must(assertStatus(r1, "temperature", check.Timeout, "timeout"))
	must(assertStatus(r1, "calibration", check.Blocked, "upstream_not_passed"))
	if n := temp.InFlight(); n != 0 {
		must(fmt.Errorf("资源未收尾: temperature 在途读取=%d", n))
	}
	fmt.Println("资源收尾检查通过：temperature 在途读取=0，句柄 open==close")

	// 演示外部取消：慢响应未恢复时主动取消整轮。
	temp.SetDelay(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	r2, err := exec.RunAll(ctx)
	must(err)
	printReport("取消轮：开始 100ms 后外部取消", r2)
	must(dev.Close())
	must(assertStatus(r2, "temperature", check.Canceled, "canceled"))
	must(assertStatus(r2, "calibration", check.Canceled, "canceled"))

	// 恢复：延迟清零。只重跑 temperature 及下游；power 前次结果保留（Attempt 不增加）。
	temp.SetDelay(0)
	affected, err := exec.Affected(virtual.CheckTemp)
	must(err)
	fmt.Printf("重跑 %q 的范围: %v（power 保留前次结果）\n", virtual.CheckTemp, affected)
	r3, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	must(err)
	printReport("重跑：temperature 恢复，仅下游联动", r3)
	must(dev.Close())
	must(assertStatus(r3, "power", check.Passed, "ok"))
	must(assertStatus(r3, "temperature", check.Passed, "ok"))
	must(assertStatus(r3, "calibration", check.Passed, "ok"))
	beforePower, _ := r2.Record("power")
	afterPower, _ := r3.Record("power")
	if afterPower.Attempt != beforePower.Attempt {
		must(fmt.Errorf("重跑 temperature 不应再次执行 power：取消轮后 Attempt=%d，重跑后 %d",
			beforePower.Attempt, afterPower.Attempt))
	}
	tempRecord, _ := r3.Record("temperature")
	if tempRecord.Attempt != 3 {
		must(fmt.Errorf("temperature 应已执行 3 次，实际 %d", tempRecord.Attempt))
	}
	printHistory(exec.History())
}

// 场景三：读数越界(failed) 与读取报错(error) 的区别，以及恢复。
func runRange() {
	fmt.Println("\n########## 场景 range：读数越界 + 读取报错 + 恢复 ##########")
	power := virtual.NewSensor("power", 12.0, 11.0, 13.0)
	temp := virtual.NewSensor("temp", 55.0, 10.0, 40.0) // 55 越界，但通信正常
	calib := virtual.NewSensor("calib", 0.2, 0.0, 1.0)
	dev := virtual.NewDeviceWithSensors("dev-range", power, temp, calib)

	exec, _ := newExecutor(dev)
	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("首轮：temperature 读数 55 越界", r1)
	must(dev.Close())
	must(assertStatus(r1, "temperature", check.Failed, "out_of_range"))
	must(assertStatus(r1, "calibration", check.Blocked, "upstream_not_passed"))

	// 通信层面也坏掉：read_error，与越界区分。
	temp.SetFault(true)
	r2, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	must(err)
	printReport("重跑：temperature 改为读取自身报错", r2)
	must(dev.Close())
	must(assertStatus(r2, "temperature", check.Error, "read_error"))
	must(assertStatus(r2, "calibration", check.Blocked, "upstream_not_passed"))

	// 恢复：故障清除且读数回到区间，下游联动通过。
	temp.SetFault(false)
	temp.SetValue(25.0)
	r3, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	must(err)
	printReport("重跑：temperature 恢复正常", r3)
	must(dev.Close())
	must(assertStatus(r3, "temperature", check.Passed, "ok"))
	must(assertStatus(r3, "calibration", check.Passed, "ok"))
	powRec, _ := r3.Record("power")
	if powRec.Attempt != 1 {
		must(fmt.Errorf("power 不应被重跑，期望 Attempt=1，实际 %d", powRec.Attempt))
	}
	printHistory(exec.History())
}

func assertStatus(r *check.Report, id string, status check.Status, code string) error {
	rec, ok := r.Record(id)
	if !ok {
		return fmt.Errorf("报告中缺少检查项 %q", id)
	}
	if rec.Status != status || rec.Code != code {
		return fmt.Errorf("检查项 %q 期望 %s/%s，实际 %s/%s", id, status, code, rec.Status, rec.Code)
	}
	return nil
}

// newExecutorWithHistory 复用前一执行器的历史（演示中更换设备句柄但保持同一批次序列）。
func newExecutorWithHistory(dev *virtual.Device, h *check.History) (*check.Executor, *check.CheckSet) {
	set, err := virtual.NewSet()
	must(err)
	exec, err := check.NewExecutor(dev, set, h)
	must(err)
	return exec, set
}
