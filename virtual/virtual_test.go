package virtual_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

func buildExec(t *testing.T, dev *virtual.Device, h *check.History) *check.Executor {
	t.Helper()
	set, err := virtual.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	exec, err := check.NewExecutor(dev, set, h)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func healthyDevice(id string, power, temp, calib *virtual.Sensor) *virtual.Device {
	if power == nil {
		power = virtual.NewSensor("power", 12.0, 11.0, 13.0)
	}
	if temp == nil {
		temp = virtual.NewSensor("temp", 25.0, 10.0, 40.0)
	}
	if calib == nil {
		calib = virtual.NewSensor("calib", 0.2, 0.0, 1.0)
	}
	return virtual.NewDeviceWithSensors(id, power, temp, calib)
}

// 传感器缺失、读数越界、检查自身报错必须是三种不同结论。
func TestFailureKindsAreDistinct(t *testing.T) {
	// 1) 传感器不存在
	dev := virtual.NewDeviceWithSensors("d", nil,
		virtual.NewSensor("temp", 25, 10, 40), virtual.NewSensor("calib", .2, 0, 1))
	r, err := buildExec(t, dev, nil).RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r, "power", check.Error, "sensor_missing")
	assertRec(t, r, "temperature", check.Blocked, "upstream_not_passed")
	assertRec(t, r, "calibration", check.Blocked, "upstream_not_passed")
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}

	// 2) 读数越界
	dev2 := healthyDevice("d2", nil,
		virtual.NewSensor("temp", 55, 10, 40), nil)
	r2, err := buildExec(t, dev2, nil).RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r2, "temperature", check.Failed, "out_of_range")
	if err := dev2.Close(); err != nil {
		t.Fatal(err)
	}

	// 3) 检查自身报错（传感器读取故障）
	s := virtual.NewSensor("temp", 25, 10, 40)
	s.SetFault(true)
	dev3 := healthyDevice("d3", nil, s, nil)
	exec := buildExec(t, dev3, nil)
	r3, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r3, "temperature", check.Error, "read_error")
	if err := dev3.Close(); err != nil {
		t.Fatal(err)
	}

	// 原始虚拟错误仍可用 errors.Is 区分
	if _, err := dev3.ReadSensor(context.Background(), "temp"); !errors.Is(err, virtual.ErrSensorFault) {
		t.Fatalf("期望 ErrSensorFault，得到 %v", err)
	}
}

// 上游不通过时依赖检查必须 blocked，不能假装通过。
func TestUpstreamFailureBlocksDownstream(t *testing.T) {
	dev := virtual.NewDeviceWithSensors("d", virtual.NewSensor("power", 12, 11, 13),
		virtual.NewSensor("temp", 200, 10, 40), virtual.NewSensor("calib", .2, 0, 1))
	r, err := buildExec(t, dev, nil).RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r, "temperature", check.Failed, "out_of_range")
	assertRec(t, r, "calibration", check.Blocked, "upstream_not_passed")
}

// 重跑只覆盖失败项及受影响下游，且保留前次结果、不改写历史批次。
func TestRerunScopeAndHistory(t *testing.T) {
	power := virtual.NewSensor("power", 12, 11, 13)
	temp := virtual.NewSensor("temp", 55, 10, 40) // 首轮越界
	calib := virtual.NewSensor("calib", .2, 0, 1)
	dev := healthyDevice("d", power, temp, calib)
	exec := buildExec(t, dev, nil)

	r1, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r1, "power", check.Passed, "ok")
	assertRec(t, r1, "temperature", check.Failed, "out_of_range")
	assertRec(t, r1, "calibration", check.Blocked, "upstream_not_passed")

	affected, err := exec.Affected(virtual.CheckTemp)
	if err != nil || len(affected) != 2 || affected[0] != "temperature" || affected[1] != "calibration" {
		t.Fatalf("temperature 受影响范围期望 [temperature calibration]，得到 %v err=%v", affected, err)
	}

	// 恢复温度，重跑 temperature：power 不应再次执行。
	temp.SetValue(25)
	r2, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r2, "power", check.Passed, "ok")
	assertRec(t, r2, "temperature", check.Passed, "ok")
	assertRec(t, r2, "calibration", check.Passed, "ok")
	pow, _ := r2.Record("power")
	if pow.Attempt != 1 {
		t.Fatalf("power 被多余重跑，Attempt=%d", pow.Attempt)
	}
	tmp, _ := r2.Record("temperature")
	if tmp.Attempt != 2 {
		t.Fatalf("temperature 应执行 2 次，Attempt=%d", tmp.Attempt)
	}

	// 历史批次原样留存：第 1 轮仍是失败结论，共 2 轮。
	if exec.History().Len() != 2 {
		t.Fatalf("历史轮数期望 2，得到 %d", exec.History().Len())
	}
	old, ok := exec.History().Get(1)
	if !ok {
		t.Fatal("第 1 轮历史丢失")
	}
	assertRec(t, old, "temperature", check.Failed, "out_of_range")
	assertRec(t, r1, "temperature", check.Failed, "out_of_range") // 旧报告对象未被改写
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
}

// 慢响应超时：后续项 blocked/timeout 正确落状态，资源全部收尾。
func TestSlowSensorTimeoutReleasesResources(t *testing.T) {
	power := virtual.NewSensor("power", 12, 11, 13)
	temp := virtual.NewSensor("temp", 25, 10, 40)
	temp.SetDelay(5 * time.Second)
	calib := virtual.NewSensor("calib", .2, 0, 1)
	dev := healthyDevice("d", power, temp, calib)
	exec := buildExec(t, dev, nil)

	start := time.Now()
	r, err := exec.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时项没有在单项时限内被放弃")
	}
	assertRec(t, r, "temperature", check.Timeout, "timeout")
	assertRec(t, r, "calibration", check.Blocked, "upstream_not_passed")
	if n := temp.InFlight(); n != 0 {
		t.Fatalf("超时后仍有 %d 次在途读取，资源未收尾", n)
	}
	reads, opened, closed := temp.Stats()
	if opened != closed {
		t.Fatalf("句柄未配对 open=%d close=%d", opened, closed)
	}
	if reads != 1 {
		t.Fatalf("慢传感器应只读取 1 次，得到 %d", reads)
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}

	// 恢复后重跑通过，且 power 保留。
	temp.SetDelay(0)
	r2, err := exec.Rerun(context.Background(), virtual.CheckTemp)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r2, "temperature", check.Passed, "ok")
	assertRec(t, r2, "calibration", check.Passed, "ok")
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
}

// 外部取消：未完成项记 canceled，在途读取及时退出并收尾。
func TestCancelStopsAndCleansUp(t *testing.T) {
	power := virtual.NewSensor("power", 12, 11, 13)
	temp := virtual.NewSensor("temp", 25, 10, 40)
	temp.SetDelay(10 * time.Second)
	calib := virtual.NewSensor("calib", .2, 0, 1)
	dev := healthyDevice("d", power, temp, calib)
	exec := buildExec(t, dev, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	r, err := exec.RunAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r, "temperature", check.Canceled, "canceled")
	assertRec(t, r, "calibration", check.Canceled, "canceled")
	deadline := time.Now().Add(2 * time.Second)
	for temp.InFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if temp.InFlight() != 0 {
		t.Fatal("取消后在途读取未退出")
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
}

// 上游未通过时重跑下游，必须保持 blocked，不能因为重跑就放行。
func TestRerunDownstreamStillBlocked(t *testing.T) {
	temp := virtual.NewSensor("temp", 55, 10, 40)
	dev := healthyDevice("d", nil, temp, nil)
	exec := buildExec(t, dev, nil)
	if _, err := exec.RunAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := exec.Rerun(context.Background(), virtual.CheckCalib)
	if err != nil {
		t.Fatal(err)
	}
	assertRec(t, r, "calibration", check.Blocked, "upstream_not_passed")
	if err := dev.Close(); err != nil {
	}
}

func assertRec(t *testing.T, r *check.Report, id string, status check.Status, code string) {
	t.Helper()
	rec, ok := r.Record(id)
	if !ok {
		t.Fatalf("报告缺少 %q", id)
	}
	if rec.Status != status || rec.Code != code {
		t.Fatalf("%q 期望 %s/%s，实际 %s/%s（%s）", id, status, code, rec.Status, rec.Code, rec.Message)
	}
}
