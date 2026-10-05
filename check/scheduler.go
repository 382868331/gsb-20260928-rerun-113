package check

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ResourceNeed 声明一个检查项需要占用的共享仪器与预计占用时长。
// 仪器容量由调度器统一管理，同一仪器的并发占用不得超过其容量。
type ResourceNeed struct {
	Instrument string
	Hold       time.Duration
}

// ResourceAware 由需要共享仪器的检查项可选实现，声明所需仪器与预计占用。
type ResourceAware interface {
	Resources() []ResourceNeed
}

// ResourcesOf 提取检查项声明的共享仪器需求；未声明时返回 nil。
func ResourcesOf(c Check) []ResourceNeed {
	if ra, ok := c.(ResourceAware); ok {
		return ra.Resources()
	}
	return nil
}

// Policy 是多设备共用仪器时的调度策略。
type Policy int

const (
	// PolicyHighRiskFirst 优先处理高风险设备：风险分高的设备先占用仪器。
	PolicyHighRiskFirst Policy = iota
	// PolicyMinTotalWait 减少总等待：预计占用短的承诺先排（SJF），最小化总等待。
	PolicyMinTotalWait
)

func (p Policy) String() string {
	if p == PolicyMinTotalWait {
		return "减少总等待"
	}
	return "高风险设备优先"
}

// CommitmentState 是承诺的生命周期状态。
type CommitmentState string

const (
	// CommitPending 已登记，等待仪器与前置检查就绪。
	CommitPending CommitmentState = "pending"
	// CommitRunning 已占用仪器开始执行；此时已无法及时取消。
	CommitRunning CommitmentState = "running"
	// CommitDone 执行完成且底层读取已收回，仪器已归还。
	CommitDone CommitmentState = "done"
	// CommitCanceled 尚未开始即被取消（上游修复使补充项失效）。
	CommitCanceled CommitmentState = "canceled"
)

// Commitment 是一条“补充检查待执行”的承诺：
// 声明所需仪器、预计占用与必须先完成的检查（Deps），
// 由调度器在共享容量下统一排序兑现。
type Commitment struct {
	DeviceID string
	CheckID  string
	Needs    []ResourceNeed
	Prereqs  []string // 同设备上必须先完成（已有结论）的检查
	Risk     int      // 设备风险分，越大越优先
	Reason   string

	run        func(ctx context.Context) error
	drain      func(ctx context.Context) error
	prereqDone func(checkID string) bool

	seq   int64
	state CommitmentState
}

// State 返回承诺当前状态。
func (c *Commitment) State() CommitmentState { return c.state }

// estimate 返回承诺的预计占用总时长（各仪器占用之和）。
func (c *Commitment) estimate() time.Duration {
	var total time.Duration
	for _, n := range c.Needs {
		total += n.Hold
	}
	return total
}

// Event 是调度审计日志的一条记录，用于核对实际执行次序与资源占用。
type Event struct {
	At         time.Time
	Kind       string // "start" / "finish" / "cancel"
	DeviceID   string
	CheckID    string
	Instrument string
	InUse      int // 事件发生时该仪器的占用数
}

// Arrangement 是某种策略下对当前待执行承诺的模拟安排。
type Arrangement struct {
	Policy    Policy
	Order     []string // "设备/检查" 执行次序
	TotalWait time.Duration
}

// Scheduler 协调多台设备共用检测仪器的条件计划调度：
// 各设备的补充检查由自身结果触发并登记为承诺，调度器在共享容量下
// 按策略决定兑现次序。仪器从检查开始一直占用到其底层读取真正退出
// （超时但尚未退出的读取不视为空闲），迟到结果只归属原轮次。
type Scheduler struct {
	policy   Policy
	capacity map[string]int

	mu      sync.Mutex
	inUse   map[string]int
	pending []*Commitment
	running []*Commitment
	events  []Event
	seq     int64
	wake    chan struct{}
}

// NewScheduler 按策略与仪器容量表构建调度器。
func NewScheduler(policy Policy, instruments map[string]int) *Scheduler {
	cap := make(map[string]int, len(instruments))
	for name, n := range instruments {
		if n <= 0 {
			n = 1
		}
		cap[name] = n
	}
	return &Scheduler{
		policy:   policy,
		capacity: cap,
		inUse:    map[string]int{},
		wake:     make(chan struct{}, 1),
	}
}

// Submit 登记一条承诺。引用的仪器必须已注册；登记后由 Run 统一兑现。
func (s *Scheduler) Submit(c *Commitment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range c.Needs {
		if _, ok := s.capacity[n.Instrument]; !ok {
			return fmt.Errorf("仪器 %q 未注册", n.Instrument)
		}
	}
	s.seq++
	c.seq = s.seq
	c.state = CommitPending
	s.pending = append(s.pending, c)
	s.notifyLocked()
	return nil
}

// SubmitCheck 为某设备执行器登记一条补充检查承诺：
// 所需仪器与预计占用取自检查的 ResourceAware 声明，
// 必须先完成的检查取自检查的 Deps；轮到它时才把检查注册进
// 执行器（AddChecks）并执行（Rerun），仪器一直占用到该检查的
// 底层读取真正退出为止。
func (s *Scheduler) SubmitCheck(exec *Executor, risk int, reason string, evidence []Evidence, c Check) error {
	needs := ResourcesOf(c)
	cm := &Commitment{
		DeviceID: exec.DeviceID(),
		CheckID:  c.ID(),
		Needs:    needs,
		Prereqs:  append([]string(nil), c.Deps()...),
		Risk:     risk,
		Reason:   reason,
		run: func(ctx context.Context) error {
			if _, err := exec.AddChecks(reason, evidence, c); err != nil {
				return err
			}
			_, err := exec.Rerun(ctx, c.ID())
			return err
		},
		// 超时/取消结论先于读取退出给出：仪器须占用到读取真正收回。
		drain: func(ctx context.Context) error { return exec.WaitCheckIdle(ctx, c.ID()) },
		prereqDone: func(id string) bool {
			_, ok := exec.RecordOf(id)
			return ok
		},
	}
	return s.Submit(cm)
}

// Cancel 取消尚未开始的承诺；已在执行或已完成的承诺无法及时取消，
// 返回 false，其执行与结果归属不受影响。
func (s *Scheduler) Cancel(deviceID, checkID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.pending {
		if c.DeviceID == deviceID && c.CheckID == checkID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			c.state = CommitCanceled
			s.recordLocked("cancel", c)
			s.notifyLocked()
			return true
		}
	}
	return false
}

// Compare 对当前待执行承诺分别按两种策略模拟安排，
// 返回各策略的执行次序与总等待，供系统权衡后选择。
func (s *Scheduler) Compare() []Arrangement {
	s.mu.Lock()
	pend := append([]*Commitment(nil), s.pending...)
	capacity := make(map[string]int, len(s.capacity))
	for k, v := range s.capacity {
		capacity[k] = v
	}
	s.mu.Unlock()
	return []Arrangement{
		simulate(PolicyHighRiskFirst, pend, capacity),
		simulate(PolicyMinTotalWait, pend, capacity),
	}
}

// simulate 按策略排序并在仪器容量上推演各承诺的开始时刻。
func simulate(policy Policy, pend []*Commitment, capacity map[string]int) Arrangement {
	ordered := orderBy(policy, pend)
	free := map[string][]time.Duration{} // 仪器 -> 各容量槽位的空闲时刻
	var total time.Duration
	arr := Arrangement{Policy: policy}
	for _, c := range ordered {
		var start time.Duration
		for _, n := range c.Needs {
			slots := free[n.Instrument]
			for len(slots) < capacity[n.Instrument] {
				slots = append(slots, 0)
			}
			if slots[0] > start {
				start = slots[0]
			}
		}
		for _, n := range c.Needs {
			slots := free[n.Instrument]
			for len(slots) < capacity[n.Instrument] {
				slots = append(slots, 0)
			}
			// 占用该仪器最早空闲的一个槽位。
			i := 0
			for j := range slots {
				if slots[j] < slots[i] {
					i = j
				}
			}
			if slots[i] < start {
				slots[i] = start
			}
			slots[i] += n.Hold
			free[n.Instrument] = slots
		}
		total += start
		arr.Order = append(arr.Order, c.DeviceID+"/"+c.CheckID)
	}
	arr.TotalWait = total
	return arr
}

// orderBy 按策略给承诺排序：高风险优先按风险分降序，
// 减少总等待按预计占用升序（SJF）；同分按登记先后。
func orderBy(policy Policy, pend []*Commitment) []*Commitment {
	out := append([]*Commitment(nil), pend...)
	sort.SliceStable(out, func(i, j int) bool {
		if policy == PolicyMinTotalWait {
			if out[i].estimate() != out[j].estimate() {
				return out[i].estimate() < out[j].estimate()
			}
		} else if out[i].Risk != out[j].Risk {
			return out[i].Risk > out[j].Risk
		}
		return out[i].seq < out[j].seq
	})
	return out
}

// Run 兑现承诺直到队列清空（无待执行且无在途）或 ctx 结束。
// 运行期间可随时 Submit 追加承诺（动态补充检查）或 Cancel 撤回；
// Run 返回后新登记的承诺需再次调用 Run 兑现。
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		s.mu.Lock()
		c := s.pickLocked()
		if c != nil {
			s.startLocked(c)
			go s.execute(ctx, c)
			s.mu.Unlock()
			continue
		}
		idle := len(s.pending) == 0 && len(s.running) == 0
		s.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pickLocked 选出下一条可兑现的承诺：前置检查已完成、
// 同设备无在途承诺（同一设备的检查不并发）、各仪器有空闲容量。
func (s *Scheduler) pickLocked() *Commitment {
	for _, c := range orderBy(s.policy, s.pending) {
		ready := true
		for _, pre := range c.Prereqs {
			if c.prereqDone == nil || !c.prereqDone(pre) {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		busy := false
		for _, r := range s.running {
			if r.DeviceID == c.DeviceID {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		fits := true
		for _, n := range c.Needs {
			if s.inUse[n.Instrument] >= s.capacity[n.Instrument] {
				fits = false
				break
			}
		}
		if fits {
			return c
		}
	}
	return nil
}

// startLocked 把承诺从待执行移到在途并占用仪器，记录 start 事件。
func (s *Scheduler) startLocked(c *Commitment) {
	for i, p := range s.pending {
		if p == c {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	c.state = CommitRunning
	s.running = append(s.running, c)
	for _, n := range c.Needs {
		s.inUse[n.Instrument]++
	}
	s.recordLocked("start", c)
}

// execute 兑现一条承诺：先执行检查，再等其底层读取真正退出，
// 最后才归还仪器 —— 超时但尚未退出的读取期间仪器不算空闲。
func (s *Scheduler) execute(ctx context.Context, c *Commitment) {
	runErr := c.run(ctx)
	if runErr == nil && c.drain != nil {
		_ = c.drain(ctx)
	}
	s.mu.Lock()
	for i, r := range s.running {
		if r == c {
			s.running = append(s.running[:i], s.running[i+1:]...)
			break
		}
	}
	for _, n := range c.Needs {
		s.inUse[n.Instrument]--
	}
	c.state = CommitDone
	s.recordLocked("finish", c)
	s.notifyLocked()
	s.mu.Unlock()
}

// recordLocked 追加审计事件（每个涉及仪器一条），调用方须已持有锁。
func (s *Scheduler) recordLocked(kind string, c *Commitment) {
	if len(c.Needs) == 0 {
		s.events = append(s.events, Event{At: time.Now(), Kind: kind, DeviceID: c.DeviceID, CheckID: c.CheckID})
		return
	}
	for _, n := range c.Needs {
		s.events = append(s.events, Event{
			At: time.Now(), Kind: kind, DeviceID: c.DeviceID, CheckID: c.CheckID,
			Instrument: n.Instrument, InUse: s.inUse[n.Instrument],
		})
	}
}

func (s *Scheduler) notifyLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// InUse 返回某仪器当前占用数（含结论已出但读取尚未退出的占用）。
func (s *Scheduler) InUse(instrument string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse[instrument]
}

// Events 返回调度审计日志（按发生先后），用于核对实际执行次序与资源占用。
func (s *Scheduler) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Pending 返回当前待执行承诺（按登记先后）。
func (s *Scheduler) Pending() []*Commitment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Commitment(nil), s.pending...)
}
