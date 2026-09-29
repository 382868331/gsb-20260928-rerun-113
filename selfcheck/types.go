package selfcheck

import (
	"context"
	"time"
)

type Status string

const (
	StatusPassed   Status = "passed"
	StatusFailed   Status = "failed"
	StatusBlocked  Status = "blocked"
	StatusCanceled Status = "canceled"
	StatusTimedOut Status = "timed_out"
	StatusExpired  Status = "expired"
)

type Result struct {
	Status  Status
	Code    string
	Message string
	Details map[string]any
}

type Check[T any] struct {
	ID           string
	Name         string
	Timeout      time.Duration
	DependsOn    []string
	Precondition func(T) (bool, string)
	Run          func(context.Context, T) Result
}

type CheckSet[T any] struct {
	Model  string
	Checks []Check[T]
}

type Report struct {
	ID                  string
	CheckID             string
	Model               string
	Attempt             int
	Status              Status
	Code                string
	Message             string
	Details             map[string]any
	StartedAt           time.Time
	FinishedAt          time.Time
	DependencyReportIDs map[string]string
	PreviousReportID    string
	SourceReportID      string
}

func Passed(code, message string, details map[string]any) Result {
	return Result{Status: StatusPassed, Code: code, Message: message, Details: details}
}

func Failed(code, message string, details map[string]any) Result {
	return Result{Status: StatusFailed, Code: code, Message: message, Details: details}
}
