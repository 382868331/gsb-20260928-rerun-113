package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

// runDemo 运行内置场景，演示依赖/取消/超时/历史/重跑的完整链路。
func runDemo(args []string) error {
	rest, jsonOut := parseJSONFlag(args)
	if len(rest) != 1 {
		return fmt.Errorf("用法: demo <missing|slow|recover>")
	}
	os.Remove(stateFile)
	switch rest[0] {
	case "missing":
		return demoMissing(jsonOut)
	case "slow":
		return demoSlow(jsonOut)
	case "recover":
		return demoRecover(jsonOut)
	default:
		return fmt.Errorf("未知场景 %q（可选 missing|slow|recover）", rest[0])
	}
}

func newDemo() (*virtual.Device, *check.CheckSet, *check.History, *check.Executor) {
	temp := virtual.NewSensor(42.5)
	fans := virtual.NewSensor(1800)
	dev := virtual.NewDevice("vm-device-01", modelID, map[string]*virtual.Sensor{
		"temp": temp,
		"fans": fans,
	})
	set, err := virtual.NewSet(virtual.SetConfig{
		Model:   modelID,
		Timeout: 300 * time.Millisecond,
		Rules: []virtual.RangeRule{
			{Sensor: "temp", Min: 0, Max: 80},
			{Sensor: "fans", Min: 1000, Max: 2500},
		},
		Aggregates: map[string][]string{
			"thermal_summary": {"temp_range", "fans_range"},
		},
	})
	if err != nil {
		panic(err)
	}
	h := &check.History{}
	ex, err := check.NewExecutor(dev, set, h)
	if err != nil {
		panic(err)
	}
	return dev, set, h, ex
}

func sensorOf(dev *virtual.Device, name string) *virtual.Sensor {
	s, ok := dev.Sensor(name)
	if !ok {
		panic("传感器 " + name + " 未配置")
	}
	return s
}

func demoEnd(h *check.History, rep *check.Report, jsonOut string) error {
	fmt.Printf("历史轮数: %d（旧批次原样保留，未被改写）\n", h.Len())
	return saveOnly(h, rep, jsonOut)
}

// demoMissing：温度传感器缺失 -> error(sensor_missing)；fans 读数越界 -> failed；
// 综合项因上游未通过而 blocked。恢复后多选重跑失败项及其下游。
func demoMissing(jsonOut string) error {
	dev, _, h, ex := newDemo()
	sensorOf(dev, "temp").Missing()
	sensorOf(dev, "fans").SetValue(9000)

	fmt.Println("=== 第 1 轮：temp 传感器缺失，fans 读数越界 ===")
	rep, err := ex.RunAll(context.Background())
	if err != nil {
		return err
	}
	printReport(rep)

	fmt.Println("\n=== 第 2 轮：装回 temp=42.5、fans 调回 1800，多选重跑两个失败项及下游 ===")
	sensorOf(dev, "temp").Install(42.5)
	sensorOf(dev, "fans").SetValue(1800)
	rep, err = ex.Rerun(context.Background(), "temp_range", "fans_range")
	if err != nil {
		return err
	}
	printReport(rep)
	return demoEnd(h, rep, jsonOut)
}

// demoSlow：先单项超时并验证在途读取收尾，再演示整轮取消，最后恢复重跑。
func demoSlow(jsonOut string) error {
	dev, _, h, ex := newDemo()
	slow := sensorOf(dev, "temp")
	slow.Slow(2 * time.Second)

	fmt.Println("=== 第 1 轮：temp 读取需 2s，单项时限 300ms ===")
	rep, err := ex.RunAll(context.Background())
	if err != nil {
		return err
	}
	printReport(rep)
	slow.Slow(0)
	dev.WaitIdle()
	fmt.Println("资源收尾: 超时产生的在途读取已全部退出")

	fmt.Println("\n=== 第 2 轮：再次慢响应，整轮在 150ms 时被外部取消 ===")
	slow.Slow(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	rep, err = ex.RunAll(ctx)
	if err != nil {
		return err
	}
	printReport(rep)
	slow.Slow(0)
	dev.WaitIdle()
	fmt.Println("资源收尾: 取消产生的在途读取已全部退出")

	fmt.Println("\n=== 第 3 轮：只重跑 temp_range 及受影响下游，恢复通过 ===")
	rep, err = ex.Rerun(context.Background(), "temp_range")
	if err != nil {
		return err
	}
	printReport(rep)
	return demoEnd(h, rep, jsonOut)
}

// demoRecover：读数越界(failed) 与自检逻辑报错(error) 分开演示；
// 自检未修好时，下游重跑不得通过。
func demoRecover(jsonOut string) error {
	dev, _, h, ex := newDemo()
	sensorOf(dev, "temp").SetValue(95)
	dev.BreakSelftest()

	fmt.Println("=== 第 1 轮：temp 读数越界(failed) + 自检逻辑坏(error) ===")
	rep, err := ex.RunAll(context.Background())
	if err != nil {
		return err
	}
	printReport(rep)

	fmt.Println("\n=== 第 2 轮：temp 调回 42.5，但自检逻辑仍坏；重跑 selftest 及下游 ===")
	sensorOf(dev, "temp").SetValue(42.5)
	rep, err = ex.Rerun(context.Background(), "device_selftest")
	if err != nil {
		return err
	}
	printReport(rep)

	fmt.Println("\n=== 第 3 轮：修复自检逻辑后重跑，全部恢复 ===")
	dev.RepairSelftest()
	rep, err = ex.Rerun(context.Background(), "device_selftest")
	if err != nil {
		return err
	}
	printReport(rep)
	return demoEnd(h, rep, jsonOut)
}
