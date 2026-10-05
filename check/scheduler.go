package check

import (
	"context"
	"sort"
	"sync"
	"time"
)

// SchedulePolicy 决定共享仪器争用时的放行优先级。
type SchedulePolicy string

const (
	// PolicyHighRiskFirst 按设备风险降序放行。
	PolicyHighRiskFirst SchedulePolicy = "high_risk_first"
	// PolicyMinimizeWaiting 按预计占用时长升序放行。
	PolicyMinimizeWaiting SchedulePolicy = "minimize_waiting"
)

// Instrument 声明一台共享仪器及其可并发容量。
type Instrument struct {
	ID       string
	Capacity int
}

// InstrumentRequirement 声明一次检查对某台仪器的容量需求。
type InstrumentRequirement struct {
	InstrumentID string
	Capacity     int
}

// ResourceCheck 由需要共享真实仪器的检查实现。
type ResourceCheck interface {
	InstrumentRequirements() []InstrumentRequirement
	EstimatedOccupancy() time.Duration
}

// ScheduleEvent 是一次排队、放行或释放事件。
type ScheduleEvent struct {
	Time         time.Time
	Kind         string
	DeviceID     string
	CheckID      string
	InstrumentID string
	Capacity     int
	Risk         int
	Estimate     time.Duration
}

type resourceRequest struct {
	deviceID string
	checkID  string
	risk     int
	estimate time.Duration
	uses     []InstrumentRequirement
	ready    chan struct{}
	enqueued time.Time
	cancel   context.CancelFunc
}

type resourceLease struct{ req *resourceRequest }

type Scheduler struct {
	policy SchedulePolicy

	mu         sync.Mutex
	capacity   map[string]int
	used       map[string]int
	waiters    map[string][]*resourceRequest
	holdings   map[string][]*resourceRequest
	deviceRisk map[string]int
	events     []ScheduleEvent
}

func NewScheduler(policy SchedulePolicy, instruments ...Instrument) *Scheduler {
	s := &Scheduler{
		policy:     policy,
		capacity:   map[string]int{},
		used:       map[string]int{},
		waiters:    map[string][]*resourceRequest{},
		holdings:   map[string][]*resourceRequest{},
		deviceRisk: map[string]int{},
	}
	for _, instrument := range instruments {
		s.capacity[instrument.ID] = effectiveCapacity(instrument.Capacity)
	}
	return s
}

func (s *Scheduler) Attach(executor *Executor, risk int) {
	s.mu.Lock()
	s.deviceRisk[executor.device.ID()] = risk
	s.mu.Unlock()
	executor.setScheduler(s, risk)
}

func (s *Scheduler) acquire(parent context.Context, deviceID, checkID string, item Check) (*resourceLease, error) {
	request := &resourceRequest{
		deviceID: deviceID,
		checkID:  checkID,
		estimate: item.Timeout(),
		ready:    make(chan struct{}),
		enqueued: time.Now(),
	}
	if request.estimate <= 0 {
		request.estimate = DefaultTimeout
	}
	if resource, ok := item.(ResourceCheck); ok {
		request.uses = append([]InstrumentRequirement(nil), resource.InstrumentRequirements()...)
		if estimate := resource.EstimatedOccupancy(); estimate > 0 {
			request.estimate = estimate
		}
	}
	if len(request.uses) == 0 {
		return &resourceLease{req: request}, nil
	}

	ctx, cancel := context.WithCancel(parent)
	request.cancel = cancel
	s.mu.Lock()
	request.risk = s.deviceRisk[deviceID]
	for _, use := range request.uses {
		if _, ok := s.capacity[use.InstrumentID]; !ok {
			s.capacity[use.InstrumentID] = 1
		}
		s.waiters[use.InstrumentID] = append(s.waiters[use.InstrumentID], request)
		s.recordEventLocked("enqueue", use, request)
	}
	s.promoteLocked()
	s.mu.Unlock()

	select {
	case <-request.ready:
		request.cancel()
		return &resourceLease{req: request}, nil
	case <-ctx.Done():
		s.removeRequest(request)
		return nil, ctx.Err()
	}
}

func (s *Scheduler) CancelWaiting(deviceID, checkID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, waiters := range s.waiters {
		for _, request := range waiters {
			if request.deviceID != deviceID || request.checkID != checkID {
				continue
			}
			select {
			case <-request.ready:
				continue
			default:
			}
			request.cancel()
			return true
		}
	}
	return false
}

func (s *Scheduler) removeRequest(request *resourceRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, use := range request.uses {
		waiters := s.waiters[use.InstrumentID]
		for i, waiter := range waiters {
			if waiter == request {
				s.waiters[use.InstrumentID] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
	}
	s.promoteLocked()
}

func (s *Scheduler) release(lease *resourceLease) {
	if lease == nil || lease.req == nil || len(lease.req.uses) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	request := lease.req
	for _, use := range request.uses {
		s.used[use.InstrumentID] -= effectiveCapacity(use.Capacity)
		if s.used[use.InstrumentID] < 0 {
			s.used[use.InstrumentID] = 0
		}
		holdings := s.holdings[use.InstrumentID]
		for i, held := range holdings {
			if held == request {
				s.holdings[use.InstrumentID] = append(holdings[:i], holdings[i+1:]...)
				break
			}
		}
		s.recordEventLocked("release", use, request)
	}
	s.promoteLocked()
}

func (s *Scheduler) promoteLocked() {
	for {
		candidates := s.eligibleRequestsLocked()
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			if s.policy == PolicyMinimizeWaiting && left.estimate != right.estimate {
				return left.estimate < right.estimate
			}
			if left.risk != right.risk {
				return left.risk > right.risk
			}
			return left.enqueued.Before(right.enqueued)
		})
		progressed := false
		for _, request := range candidates {
			if !s.canGrantLocked(request) {
				continue
			}
			select {
			case <-request.ready:
				continue
			default:
			}
			for _, use := range request.uses {
				s.used[use.InstrumentID] += effectiveCapacity(use.Capacity)
				s.removeWaiterLocked(use.InstrumentID, request)
				s.holdings[use.InstrumentID] = append(s.holdings[use.InstrumentID], request)
				s.recordEventLocked("acquire", use, request)
			}
			close(request.ready)
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

func (s *Scheduler) eligibleRequestsLocked() []*resourceRequest {
	seen := map[*resourceRequest]bool{}
	var requests []*resourceRequest
	for _, waiters := range s.waiters {
		for _, request := range waiters {
			if !seen[request] {
				seen[request] = true
				requests = append(requests, request)
			}
		}
	}
	return requests
}

func (s *Scheduler) canGrantLocked(request *resourceRequest) bool {
	for _, use := range request.uses {
		if s.used[use.InstrumentID]+effectiveCapacity(use.Capacity) > s.capacity[use.InstrumentID] {
			return false
		}
	}
	return true
}

func (s *Scheduler) removeWaiterLocked(instrumentID string, target *resourceRequest) {
	waiters := s.waiters[instrumentID]
	for i, request := range waiters {
		if request == target {
			s.waiters[instrumentID] = append(waiters[:i], waiters[i+1:]...)
			return
		}
	}
}

func (s *Scheduler) recordEventLocked(kind string, use InstrumentRequirement, request *resourceRequest) {
	s.events = append(s.events, ScheduleEvent{
		Time:         time.Now(),
		Kind:         kind,
		DeviceID:     request.deviceID,
		CheckID:      request.checkID,
		InstrumentID: use.InstrumentID,
		Capacity:     effectiveCapacity(use.Capacity),
		Risk:         request.risk,
		Estimate:     request.estimate,
	})
}

func (s *Scheduler) Events() []ScheduleEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ScheduleEvent(nil), s.events...)
}

func (s *Scheduler) InUse(instrumentID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used[instrumentID]
}

func effectiveCapacity(capacity int) int {
	if capacity <= 0 {
		return 1
	}
	return capacity
}
