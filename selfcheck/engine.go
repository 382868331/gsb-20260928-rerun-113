package selfcheck

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Engine[T any] struct {
	set    CheckSet[T]
	store  *Store
	order  []string
	checks map[string]Check[T]
}

func NewEngine[T any](set CheckSet[T]) (*Engine[T], error) {
	checks := make(map[string]Check[T], len(set.Checks))
	for _, check := range set.Checks {
		if check.ID == "" {
			return nil, fmt.Errorf("check id is empty")
		}
		if _, exists := checks[check.ID]; exists {
			return nil, fmt.Errorf("duplicate check id %q", check.ID)
		}
		checks[check.ID] = check
	}

	order, err := topoOrder(set.Checks, checks)
	if err != nil {
		return nil, err
	}

	return &Engine[T]{
		set:    set,
		store:  NewStore(),
		order:  order,
		checks: checks,
	}, nil
}

func (e *Engine[T]) Model() string {
	return e.set.Model
}

func (e *Engine[T]) Store() *Store {
	return e.store
}

func (e *Engine[T]) Run(ctx context.Context, device T) ([]*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	current := make(map[string]*Report, len(e.order))
	created := make([]*Report, 0, len(e.order))
	for _, id := range e.order {
		report := e.runOne(ctx, e.checks[id], device, current, true)
		current[id] = report
		e.store.Save(report)
		created = append(created, report)
	}
	return created, nil
}

func (e *Engine[T]) Rerun(ctx context.Context, checkID string, device T) ([]*Report, error) {
	check, ok := e.checks[checkID]
	if !ok {
		return nil, fmt.Errorf("unknown check %q", checkID)
	}

	latest := e.store.LatestMap()
	report := e.runOne(ctx, check, device, latest, true)
	e.store.Save(report)
	latest[checkID] = report

	created := []*Report{report}
	now := time.Now()
	for _, dependentID := range e.dependents(checkID) {
		previous := latest[dependentID]
		if previous == nil || previous.Status == StatusExpired {
			continue
		}
		expired := &Report{
			ID:               e.store.nextID(),
			CheckID:          dependentID,
			Model:            e.set.Model,
			Attempt:          previous.Attempt,
			Status:           StatusExpired,
			Code:             "upstream_reran",
			Message:          fmt.Sprintf("upstream check %q was rerun", checkID),
			StartedAt:        now,
			FinishedAt:       now,
			PreviousReportID: previous.ID,
			SourceReportID:   previous.ID,
		}
		e.store.Save(expired)
		latest[dependentID] = expired
		created = append(created, expired)
	}
	return created, nil
}

func (e *Engine[T]) runOne(ctx context.Context, check Check[T], device T, latest map[string]*Report, incrementAttempt bool) *Report {
	now := time.Now()
	previous := latest[check.ID]
	attempt := 0
	if previous != nil {
		attempt = previous.Attempt
	}
	if incrementAttempt {
		attempt++
	}

	base := &Report{
		CheckID:   check.ID,
		Model:     e.set.Model,
		Attempt:   attempt,
		StartedAt: now,
	}
	if previous != nil {
		base.PreviousReportID = previous.ID
	}

	dependencyReports := make(map[string]string, len(check.DependsOn))
	for _, dependencyID := range check.DependsOn {
		dependency := latest[dependencyID]
		if dependency == nil {
			return e.finish(base, Result{
				Status:  StatusBlocked,
				Code:    "dependency_missing",
				Message: fmt.Sprintf("dependency %q has no result", dependencyID),
			}, dependencyReports)
		}
		dependencyReports[dependencyID] = dependency.ID
		if dependency.Status != StatusPassed {
			return e.finish(base, Result{
				Status:  StatusBlocked,
				Code:    "dependency_not_passed",
				Message: fmt.Sprintf("dependency %q is %s", dependencyID, dependency.Status),
				Details: map[string]any{
					"dependency_check_id": dependencyID,
					"dependency_status":   string(dependency.Status),
					"dependency_code":     dependency.Code,
				},
			}, dependencyReports)
		}
	}

	if check.Precondition != nil {
		if ok, message := check.Precondition(device); !ok {
			return e.finish(base, Result{
				Status:  StatusBlocked,
				Code:    "precondition_not_met",
				Message: message,
			}, dependencyReports)
		}
	}

	if err := ctx.Err(); err != nil {
		return e.finish(base, canceledResult(err), dependencyReports)
	}
	if check.Run == nil {
		return e.finish(base, Passed("skipped", "check has no logic", nil), dependencyReports)
	}

	runCtx := ctx
	var cancel context.CancelFunc
	timedOut := false
	if check.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, check.Timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	type runResult struct {
		result Result
	}
	completed := make(chan runResult, 1)
	go func() {
		completed <- runResult{result: check.Run(runCtx, device)}
	}()

	select {
	case item := <-completed:
		return e.finish(base, normalize(item.result), dependencyReports)
	case <-runCtx.Done():
		var result Result
		if ctx.Err() != nil {
			result = canceledResult(ctx.Err())
		} else {
			timedOut = true
			result = Result{
				Status:  StatusTimedOut,
				Code:    "timeout",
				Message: fmt.Sprintf("check did not finish within %s", check.Timeout),
			}
		}
		report := e.finish(base, result, dependencyReports)
		go func() { <-completed }()
		_ = timedOut
		return report
	}
}

func (e *Engine[T]) finish(base *Report, result Result, dependencies map[string]string) *Report {
	return &Report{
		ID:                  e.store.peekID(),
		CheckID:             base.CheckID,
		Model:               base.Model,
		Attempt:             base.Attempt,
		Status:              result.Status,
		Code:                result.Code,
		Message:             result.Message,
		Details:             cloneDetails(result.Details),
		StartedAt:           base.StartedAt,
		FinishedAt:          time.Now(),
		DependencyReportIDs: cloneDependencies(dependencies),
		PreviousReportID:    base.PreviousReportID,
	}
}

func (e *Engine[T]) dependents(rootID string) []string {
	direct := make(map[string][]string)
	for id, check := range e.checks {
		for _, dependencyID := range check.DependsOn {
			direct[dependencyID] = append(direct[dependencyID], id)
		}
	}

	seen := map[string]bool{rootID: true}
	queue := append([]string(nil), direct[rootID]...)
	var result []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
		queue = append(queue, direct[id]...)
	}
	return result
}

func normalize(result Result) Result {
	if result.Status == "" {
		result.Status = StatusPassed
	}
	return result
}

func canceledResult(err error) Result {
	return Result{
		Status:  StatusCanceled,
		Code:    "canceled",
		Message: err.Error(),
	}
}

func cloneDetails(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneDependencies(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

type Store struct {
	mu       sync.RWMutex
	sequence uint64
	history  []*Report
	latest   map[string]*Report
}

func NewStore() *Store {
	return &Store{latest: make(map[string]*Report)}
}

func (s *Store) nextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextIDLocked()
}

func (s *Store) peekID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("report-%06d", s.sequence+1)
}

func (s *Store) nextIDLocked() string {
	s.sequence++
	return fmt.Sprintf("report-%06d", s.sequence)
}

func (s *Store) Save(report *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if report.ID == "" {
		report.ID = s.nextIDLocked()
	}
	s.history = append(s.history, report)
	s.latest[report.CheckID] = report
}

func (s *Store) History() []*Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Report(nil), s.history...)
}

func (s *Store) Latest(checkID string) *Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest[checkID]
}

func (s *Store) LatestMap() map[string]*Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	output := make(map[string]*Report, len(s.latest))
	for id, report := range s.latest {
		output[id] = report
	}
	return output
}
