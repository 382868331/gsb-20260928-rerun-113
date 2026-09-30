package virtual

import (
	"context"
	"runtime"
	"testing"
	"time"

	"devicecheck/check"
)

func testRig(t *testing.T) (*Device, *check.CheckSet, *check.Executor) {
	t.Helper()
	temp := NewSensor(42.5)
	fans := NewSensor(1800)
	dev := NewDevice("vm-device-01", "VM-THERMO-1", map[string]*Sensor{
		"temp": temp,
		"fans": fans,
	})
	set, err := NewSet(SetConfig{
		Model:   "VM-THERMO-1",
		Timeout: 200 * time.Millisecond,
		Rules: []RangeRule{
			{Sensor: "temp", Min: 0, Max: 80},
			{Sensor: "fans", Min: 1000, Max: 2500},
		},
		Aggregates: map[string][]string{
			"thermal_summary": {"temp_range", "fans_range"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := check.NewExecutor(dev, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dev, set, ex
}

func TestMissingSensorThenRecover(t *testing.T) {
	dev, _, ex := testRig(t)
	s, _ := dev.Sensor("temp")
	s.Missing()
	fans, _ := dev.Sensor("fans")
	fans.SetValue(9000)

	rep, err := ex.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := rep.Record("temp_range")
	if tr.Status != check.Error || tr.Code != CodeSensorMissing {
		t.Fatalf("temp_range = %s/%s", tr.Status, tr.Code)
	}
	fr, _ := rep.Record("fans_range")
	if fr.Status != check.Failed || fr.Code != CodeOutOfRange {
		t.Fatalf("fans_range = %s/%s, 期望 failed/out_of_range", fr.Status, fr.Code)
	}
	sum, _ := rep.Record("thermal_summary")
	if sum.Status != check.Blocked {
		t.Fatalf("thermal_summary = %s, 期望 blocked", sum.Status)
	}

	s.Install(42.5)
	fans.SetValue(1800)
	rep2, err := ex.Rerun(context.Background(), "temp_range", "fans_range")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Overall != check.Passed {
		t.Fatalf("恢复后整体 = %s, 期望 passed", rep2.Overall)
	}
	for _, id := range []string{"temp_range", "fans_range", "thermal_summary"} {
		rec, _ := rep2.Record(id)
		if rec.Attempt != 2 {
			t.Errorf("%s attempt=%d, 期望 2", id, rec.Attempt)
		}
	}
	self, _ := rep2.Record("device_selftest")
	if self.Attempt != 1 {
		t.Errorf("无关的 device_selftest 被重跑, attempt=%d", self.Attempt)
	}
}

func TestSlowSensorTimeoutCleansUp(t *testing.T) {
	dev, _, ex := testRig(t)
	s, _ := dev.Sensor("temp")
	s.Slow(2 * time.Second)

	before := runtime.NumGoroutine()
	rep, err := ex.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := rep.Record("temp_range")
	if tr.Status != check.Timeout {
		t.Fatalf("temp_range = %s, 期望 timeout", tr.Status)
	}
	if tr.Duration() >= 2*time.Second {
		t.Fatalf("执行器被慢读取拖住: %s", tr.Duration())
	}

	// 在途慢读取必须随取消退出；WaitIdle 应在很短时间内返回。
	s.Slow(0)
	done := make(chan struct{})
	go func() { dev.WaitIdle(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("超时后在途读取未收尾，设备资源泄漏")
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Fatalf("goroutine 数量异常增长: before=%d after=%d", before, got)
	}

	// 恢复后仅重跑 temp 及其下游，全部通过。
	s.Install(42.5)
	rep2, err := ex.Rerun(context.Background(), "temp_range")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Overall != check.Passed {
		t.Fatalf("恢复后整体 = %s", rep2.Overall)
	}
}

func TestCancelMidRunCleansUp(t *testing.T) {
	dev, _, ex := testRig(t)
	s, _ := dev.Sensor("temp")
	s.Slow(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	rep, err := ex.RunAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := rep.Record("temp_range")
	if tr.Status != check.Canceled {
		t.Fatalf("temp_range = %s, 期望 canceled", tr.Status)
	}
	s.Slow(0)
	done := make(chan struct{})
	go func() { dev.WaitIdle(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消后在途读取未收尾")
	}
}

func TestSelftestFaultIsDistinctAndBlocksAll(t *testing.T) {
	dev, _, ex := testRig(t)
	dev.BreakSelftest()
	rep, _ := ex.RunAll(context.Background())
	self, _ := rep.Record("device_selftest")
	if self.Status != check.Error || self.Code != CodeSelftestError {
		t.Fatalf("device_selftest = %s/%s", self.Status, self.Code)
	}
	tr, _ := rep.Record("temp_range")
	if tr.Status != check.Blocked {
		t.Fatalf("temp_range = %s, 自检坏时必须 blocked", tr.Status)
	}

	// 只恢复读数不修自检：重跑 selftest 仍失败，下游不得通过。
	rep2, _ := ex.Rerun(context.Background(), "device_selftest")
	self2, _ := rep2.Record("device_selftest")
	if self2.Status != check.Error {
		t.Fatalf("self test 仍坏，得到 %s", self2.Status)
	}
	tr2, _ := rep2.Record("temp_range")
	if tr2.Status != check.Blocked {
		t.Fatalf("下游得到 %s, 必须 blocked", tr2.Status)
	}

	dev.RepairSelftest()
	rep3, _ := ex.Rerun(context.Background(), "device_selftest")
	if rep3.Overall != check.Passed {
		t.Fatalf("修复后整体 = %s, 期望 passed", rep3.Overall)
	}
}

func TestSensorFaultDistinctFromMissing(t *testing.T) {
	dev, _, ex := testRig(t)
	s, _ := dev.Sensor("temp")
	s.Fault()
	rep, _ := ex.RunAll(context.Background())
	tr, _ := rep.Record("temp_range")
	if tr.Status != check.Error || tr.Code != CodeSensorFault {
		t.Fatalf("temp_range = %s/%s, 期望 error/sensor_fault", tr.Status, tr.Code)
	}
}
