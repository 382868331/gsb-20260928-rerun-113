package virtual_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"devicecheck/check"
	"devicecheck/virtual"
)

type instrumentCheck struct {
	id            string
	deps          []string
	instrument    string
	estimate      time.Duration
	timeout       time.Duration
	start         chan struct{}
	release       chan struct{}
	result        check.Result
	ignoresCancel bool
}

func (c instrumentCheck) ID() string             { return c.id }
func (c instrumentCheck) Name() string           { return c.id }
func (c instrumentCheck) Deps() []string         { return c.deps }
func (c instrumentCheck) Timeout() time.Duration { return c.timeout }
func (c instrumentCheck) InstrumentRequirements() []check.InstrumentRequirement {
	return []check.InstrumentRequirement{{InstrumentID: c.instrument}}
}
func (c instrumentCheck) EstimatedOccupancy() time.Duration { return c.estimate }
func (c instrumentCheck) Run(ctx *check.Context) check.Result {
	close(c.start)
	if c.ignoresCancel {
		<-c.release
	} else {
		select {
		case <-ctx.Done():
			return check.Fault("canceled", "取消")
		case <-c.release:
		}
	}
	return c.result
}

func TestSharedInstrumentSchedulingPolicies(t *testing.T) {
	t.Run("high risk first", func(t *testing.T) {
		releaseHolder := make(chan struct{})
		holder := instrumentCheck{id: "holder", instrument: "meter", estimate: time.Millisecond,
			timeout: time.Second, start: make(chan struct{}), release: releaseHolder, result: check.Pass("ok")}
		high := instrumentCheck{id: "high", instrument: "meter", estimate: time.Hour,
			timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
		low := instrumentCheck{id: "low", instrument: "meter", estimate: time.Millisecond,
			timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
		scheduler, devices := setupScheduledDevices(t, check.PolicyHighRiskFirst, holder, low, high)
		var wait sync.WaitGroup
		wait.Add(3)
		go runAsync(devices[0], &wait)
		<-holder.start
		go runAsync(devices[1], &wait)
		go runAsync(devices[2], &wait)
		assertAcquireOrder(t, scheduler, "holder", 0)
		waitEnqueue(t, scheduler, "low")
		waitEnqueue(t, scheduler, "high")
		close(releaseHolder)
		assertAcquireOrder(t, scheduler, "high", 1)
		close(high.release)
		assertAcquireOrder(t, scheduler, "low", 2)
		close(low.release)
		wait.Wait()
	})

	t.Run("minimize waiting", func(t *testing.T) {
		releaseHolder := make(chan struct{})
		holder := instrumentCheck{id: "holder", instrument: "meter", estimate: time.Millisecond,
			timeout: time.Second, start: make(chan struct{}), release: releaseHolder, result: check.Pass("ok")}
		long := instrumentCheck{id: "long", instrument: "meter", estimate: time.Hour,
			timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
		short := instrumentCheck{id: "short", instrument: "meter", estimate: time.Millisecond,
			timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
		scheduler, devices := setupScheduledDevices(t, check.PolicyMinimizeWaiting, holder, long, short)
		var wait sync.WaitGroup
		wait.Add(3)
		go runAsync(devices[0], &wait)
		<-holder.start
		go runAsync(devices[1], &wait)
		go runAsync(devices[2], &wait)
		assertAcquireOrder(t, scheduler, "holder", 0)
		waitEnqueue(t, scheduler, "long")
		waitEnqueue(t, scheduler, "short")
		close(releaseHolder)
		assertAcquireOrder(t, scheduler, "short", 1)
		close(short.release)
		assertAcquireOrder(t, scheduler, "long", 2)
		close(long.release)
		wait.Wait()
	})
}

func TestTimeoutKeepsInstrumentUntilReadReturns(t *testing.T) {
	release := make(chan struct{})
	stuckCheck := instrumentCheck{id: "read", instrument: "meter", estimate: time.Millisecond,
		timeout: 20 * time.Millisecond, start: make(chan struct{}), release: release,
		result: check.Pass("late"), ignoresCancel: true}
	waitingCheck := instrumentCheck{id: "read", instrument: "meter", estimate: time.Millisecond,
		timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
	scheduler, devices := setupScheduledDevices(t, check.PolicyHighRiskFirst, stuckCheck, waitingCheck)

	stuckDone := make(chan *check.Report, 1)
	go func() {
		report, err := devices[0].RunAll(context.Background())
		if err != nil {
			t.Error(err)
		}
		stuckDone <- report
	}()
	<-stuckCheck.start
	select {
	case report := <-stuckDone:
		assertRec(t, report, "read", check.Timeout, "timeout")
	case <-time.After(time.Second):
		t.Fatal("超时结论应先返回")
	}
	if scheduler.InUse("meter") != 1 {
		t.Fatal("超时但读取未退出时仪器仍应被占用")
	}
	waitingDone := make(chan struct{}, 1)
	go func() {
		if _, err := devices[1].RunAll(context.Background()); err != nil {
			t.Error(err)
		}
		close(waitingDone)
	}()
	select {
	case <-waitingCheck.start:
		t.Fatal("读取未释放前不能把仪器派给另一设备")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-waitingCheck.start:
	case <-time.After(time.Second):
		t.Fatal("旧读取释放后等待设备应取得仪器")
	}
	close(waitingCheck.release)
	<-waitingDone
	if scheduler.InUse("meter") != 0 {
		t.Fatal("仪器最终应释放")
	}
	old, _ := devices[0].History().Get(1)
	assertRec(t, old, "read", check.Timeout, "timeout")
}

func TestUpstreamFixCancelsOnlyUnstartedCommitment(t *testing.T) {
	temp := virtual.NewSensor("temp", 55, 10, 40)
	probe := virtual.NewSensor("probe", 3, 0, 100)
	dev := virtual.NewDevice("d", virtual.ModelID, map[string]*virtual.Sensor{"temp": temp, "probe": probe})
	set, err := check.NewSet(virtual.ModelID, virtual.NewSensorCheck("temperature", "温度", nil, time.Second, "temp", 10, 40))
	if err != nil {
		t.Fatal(err)
	}
	exec, err := check.NewExecutor(dev, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := check.Rule{Trigger: "temperature", Code: "out_of_range", Reason: "追加探头", Adds: []check.Check{
		virtual.NewInstrumentCheck("temp_probe", "探头", nil, time.Second, "probe", 0, 100, "meter", time.Millisecond),
	}}
	if err := exec.SetRules(rule); err != nil {
		t.Fatal(err)
	}
	holderCheck := instrumentCheck{id: "hold", instrument: "meter", estimate: time.Millisecond,
		timeout: time.Second, start: make(chan struct{}), release: make(chan struct{}), result: check.Pass("ok")}
	holderSet, err := check.NewSet(virtual.ModelID, holderCheck)
	if err != nil {
		t.Fatal(err)
	}
	holderExec, err := check.NewExecutor(dev, holderSet, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := check.NewScheduler(check.PolicyHighRiskFirst, check.Instrument{ID: "meter"})
	scheduler.Attach(exec, 1)
	scheduler.Attach(holderExec, 1)

	holderDone := make(chan *check.Report, 1)
	go func() {
		report, err := holderExec.RunAll(context.Background())
		if err != nil {
			t.Error(err)
		}
		holderDone <- report
	}()
	<-holderCheck.start
	firstDone := make(chan *check.Report, 1)
	go func() {
		report, err := exec.RunAll(context.Background())
		if err != nil {
			t.Error(err)
		}
		firstDone <- report
	}()
	deadline := time.Now().Add(time.Second)
	for !hasEvent(scheduler.Events(), "enqueue", "d", "temp_probe") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hasEvent(scheduler.Events(), "enqueue", "d", "temp_probe") {
		t.Fatal("动态补充检查未进入共享仪器队列")
	}
	temp.SetValue(25)
	if !exec.CancelCommitment("temp_probe") {
		t.Fatal("上游修复后，尚未开始的承诺应可取消")
	}
	first := <-firstDone
	if _, ok := first.Record("temp_probe"); ok {
		t.Fatal("取消的未开始承诺不得生成检查记录")
	}
	if _, ok := exec.PlanLog().Current().Item("temp_probe"); ok {
		t.Fatal("当前计划应移除取消项")
	}
	v2, _ := exec.PlanLog().Get(2)
	if _, ok := v2.Item("temp_probe"); !ok {
		t.Fatal("加入承诺的历史计划版本必须保留")
	}
	reads, _, _ := probe.Stats()
	if reads != 0 {
		t.Fatalf("取消项不得读取仪器，reads=%d", reads)
	}
	close(holderCheck.release)
	<-holderDone
}

func setupScheduledDevices(t *testing.T, policy check.SchedulePolicy, checks ...instrumentCheck) (*check.Scheduler, []*check.Executor) {
	t.Helper()
	scheduler := check.NewScheduler(policy, check.Instrument{ID: "meter"})
	executors := make([]*check.Executor, 0, len(checks))
	for i, item := range checks {
		dev := virtual.NewDevice(string(rune('a'+i)), virtual.ModelID, nil)
		set, err := check.NewSet(virtual.ModelID, item)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := check.NewExecutor(dev, set, nil)
		if err != nil {
			t.Fatal(err)
		}
		risk := 1
		if item.id == "high" {
			risk = 10
		}
		scheduler.Attach(executor, risk)
		executors = append(executors, executor)
	}
	return scheduler, executors
}

func runAsync(executor *check.Executor, wait *sync.WaitGroup) {
	defer wait.Done()
	_, _ = executor.RunAll(context.Background())
}

func assertAcquireOrder(t *testing.T, scheduler *check.Scheduler, expectedCheck string, occurrence int) {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events := scheduler.Events()
		acquires := make([]string, 0, occurrence+1)
		for _, event := range events {
			if event.Kind == "acquire" {
				acquires = append(acquires, event.CheckID)
				if len(acquires) == occurrence+1 {
					if acquires[occurrence] != expectedCheck {
						t.Fatalf("第 %d 个放行期望 %q，实际 %v", occurrence, expectedCheck, acquires)
					}
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %q 被放行超时", expectedCheck)
}

func hasEvent(events []check.ScheduleEvent, kind, deviceID, checkID string) bool {
	for _, event := range events {
		if event.Kind == kind && event.DeviceID == deviceID && event.CheckID == checkID {
			return true
		}
	}
	return false
}

func waitEnqueue(t *testing.T, scheduler *check.Scheduler, checkID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, event := range scheduler.Events() {
			if event.Kind == "enqueue" && event.CheckID == checkID {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %q 入队超时", checkID)
}
