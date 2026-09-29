package check

import (
	"context"
	"fmt"
	"time"
)

// Result 是检查函数返回的结论。
type Result struct {
	Status  Status
	Code    string
	Message string
}

// Pass 构造通过结论。
func Pass(format string, args ...any) Result {
	return Result{Status: Passed, Code: "ok", Message: fmt.Sprintf(format, args...)}
}

// Fail 表示通信/流程正常，但读数越界等业务不达标。
func Fail(code, format string, args ...any) Result {
	return Result{Status: Failed, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Fault 表示设备故障导致检查无法完成，例如传感器缺失。
func Fault(code, format string, args ...any) Result {
	return Result{Status: Error, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Context 一次检查执行时拿到的上下文，携带设备与运行环境。
type Context struct {
	context.Context
	Device Device
}

// Check 是一个可执行的诊断项。
type Check interface {
	ID() string
	Name() string
	Deps() []string
	Timeout() time.Duration
	Run(ctx *Context) Result
}

// FuncCheck 用函数字面量快速定义诊断项。
type FuncCheck struct {
	CheckID   string
	CheckName string
	Requires  []string
	Limit     time.Duration
	Fn        func(ctx *Context) Result
}

func (f *FuncCheck) ID() string             { return f.CheckID }
func (f *FuncCheck) Name() string           { return f.CheckName }
func (f *FuncCheck) Deps() []string         { return f.Requires }
func (f *FuncCheck) Timeout() time.Duration { return f.Limit }
func (f *FuncCheck) Run(ctx *Context) Result {
	return f.Fn(ctx)
}

