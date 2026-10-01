package main

import (
	"context"
	"fmt"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

// 场景四：读取对取消无响应 —— 超时结论与资源收尾分离，重跑不与旧读取重叠。
func runStuck() {
	fmt.Println("\n########## 场景 stuck：读取取消无响应 + 清理进展 + 重跑去重 ##########")
	power := virtual.NewSensor("power", 12.0, 11.0, 13.0)
	temp := virtual.NewSensor("temp", 25.0, 10.0, 40.0)
	calib := virtual.NewSensor("calib", 0.2, 0.0, 1.0)
	temp.SetDelay(1500 * time.Millisecond) // 远超检查项 500ms 时限
	temp.SetIgnoreContext(true)            // 读取不响应取消/超时，必须等满延迟
	dev := virtual.NewDeviceWithSensors("dev-stuck", power, temp, calib)

	exec, _ := newExecutor(dev)
	r1, err := exec.RunAll(context.Background())
	must(err)
	printReport("首轮：temperature 读取无响应，超时定论", r1)
	must(assertStatus(r1, "temperature", check.Timeout, "timeout"))
	must(assertStatus(r1, "calibration", check.Blocked, "upstream_not_passed"))
	tRec, _ := r1.Record("temperature")
	if !tRec.CleanupPending {
		must(fmt.Errorf("无响应读取应标记 CleanupPending"))
	}
	fmt.Printf("检查结论已出，但资源未收回：在途=%v（不能宣称本轮已彻底结束）\n", exec.InFlight())
	if err := dev.Close(); err != nil {
		fmt.Println("Close 如实报告未收尾:", err)
	} else {
		must(fmt.Errorf("读取仍在途，Close 不应通过"))
	}

	// 无关设备不受拖累，照常完成整轮自检。
	devOK := virtual.NewDeviceWithSensors("dev-healthy",
		virtual.NewSensor("power", 12.0, 11.0, 13.0),
		virtual.NewSensor("temp", 25.0, 10.0, 40.0),
		virtual.NewSensor("calib", 0.2, 0.0, 1.0))
	execOK, _ := newExecutor(devOK)
	rOK, err := execOK.RunAll(context.Background())
	must(err)
	printReport("并行：无关设备 dev-healthy 整轮自检", rOK)
	must(assertStatus(rOK, "calibration", check.Passed, "ok"))
	must(devOK.Close())

	// 修复设备（恢复响应、延迟清零），在旧读取仍在途时发起重跑：
	// 重跑会等待旧读取收尾后才发起新读取，二者绝不重叠；期间展示清理进展。
	temp.SetIgnoreContext(false)
	temp.SetDelay(0)
	type rerunOut struct {
		r   *check.Report
		err error
	}
	outCh := make(chan rerunOut, 1)
	go func() {
		r, err := exec.Rerun(context.Background(), virtual.CheckTemp)
		outCh <- rerunOut{r, err}
	}()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var out rerunOut
	for done := false; !done; {
		select {
		case out = <-outCh:
			done = true
		case <-ticker.C:
			fmt.Printf("清理进展：在途=%v（重跑已就绪，等待旧读取收尾）\n", exec.InFlight())
		}
	}
	must(out.err)
	r2 := out.r
	printReport("重跑：旧读取收尾后 temperature 及下游恢复", r2)
	must(assertStatus(r2, "temperature", check.Passed, "ok"))
	must(assertStatus(r2, "calibration", check.Passed, "ok"))
	must(exec.WaitIdle(context.Background()))
	must(dev.Close()) // 此刻才真正全部收尾
	reads, opened, closed := temp.Stats()
	fmt.Printf("temperature 读取 %d 次，句柄 open=%d close=%d（旧读取未丢失、未重叠）\n", reads, opened, closed)
	if reads != 2 || opened != closed {
		must(fmt.Errorf("资源使用状态不真实: reads=%d open=%d close=%d", reads, opened, closed))
	}

	// 历史独立：首轮超时批次原样保留，迟到结果未污染任何记录。
	old, ok := exec.History().Get(1)
	if !ok {
		must(fmt.Errorf("首轮历史丢失"))
	}
	must(assertStatus(old, "temperature", check.Timeout, "timeout"))
	printHistory(exec.History())
}
