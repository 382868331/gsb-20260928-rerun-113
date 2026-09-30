package virtual

import (
	"errors"
	"fmt"
	"time"

	"devicecheck/check"
)

// 检查结论代码：三类问题分别留痕，禁止混为一谈。
const (
	CodeSensorMissing = "sensor_missing" // 传感器不存在，检查无法完成
	CodeSensorFault   = "sensor_fault"   // 传感器/读取链路报错，检查无法完成
	CodeOutOfRange    = "out_of_range"   // 读数越界，通信正常但业务不达标
	CodeSelftestError = "selftest_error" // 检查/设备逻辑自身报错
)

// RangeRule 描述一个传感器的允许读数区间。
type RangeRule struct {
	Sensor string
	Min    float64
	Max    float64
}

// SetConfig 描述一台虚拟设备型号上的检查布局。
type SetConfig struct {
	Model   string
	Timeout time.Duration
	Rules   []RangeRule
	// Aggregates 的值是它依赖的检查项 ID；聚合项只在全部前置通过后执行。
	Aggregates map[string][]string
}

// NewSet 依据配置构建该校验并自校验的检查集。
func NewSet(cfg SetConfig) (*check.CheckSet, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = check.DefaultTimeout
	}
	items := make([]check.Check, 0, len(cfg.Rules)+len(cfg.Aggregates)+1)

	items = append(items, &check.FuncCheck{
		CheckID:   "device_selftest",
		CheckName: "设备自检逻辑",
		Limit:     timeout,
		Fn: func(ctx *check.Context) check.Result {
			dev, ok := ctx.Device.(*Device)
			if !ok || dev == nil {
				return check.Fault(CodeSelftestError, "设备类型不匹配，检查无法完成")
			}
			if dev.SelftestFaulted() {
				return check.Fault(CodeSelftestError, "设备自检逻辑内部报错")
			}
			return check.Pass("设备自检逻辑正常")
		},
	})

	for _, rule := range cfg.Rules {
		rule := rule
		id := rule.Sensor + "_range"
		items = append(items, &check.FuncCheck{
			CheckID:   id,
			CheckName: fmt.Sprintf("传感器 %s 读数区间", rule.Sensor),
			Requires:  []string{"device_selftest"},
			Limit:     timeout,
			Fn: func(ctx *check.Context) check.Result {
				return readAndCheck(ctx, rule)
			},
		})
	}

	for aggID, deps := range cfg.Aggregates {
		aggID, deps := aggID, append([]string(nil), deps...)
		items = append(items, &check.FuncCheck{
			CheckID:   aggID,
			CheckName: "综合判定 " + aggID,
			Requires:  deps,
			Limit:     timeout,
			Fn: func(ctx *check.Context) check.Result {
				return check.Pass("全部前置检查通过，%s 综合判定正常", aggID)
			},
		})
	}
	return check.NewSet(cfg.Model, items...)
}

func readAndCheck(ctx *check.Context, rule RangeRule) check.Result {
	dev := ctx.Device.(*Device)
	value, err := dev.readSensor(rule.Sensor, ctx.Done())
	if err != nil {
		switch {
		case errors.Is(err, ErrSensorMissing):
			return check.Fault(CodeSensorMissing, "传感器 %q 不存在", rule.Sensor)
		case errors.Is(err, ErrSensorFault):
			return check.Fault(CodeSensorFault, "传感器 %q 读取故障", rule.Sensor)
		default:
			// 读取被取消等情况交由执行器的 ctx 判定，不返回业务结论。
			return check.Fault(CodeSensorFault, "传感器 %q 读取异常: %v", rule.Sensor, err)
		}
	}
	if value < rule.Min || value > rule.Max {
		return check.Fail(CodeOutOfRange,
			"传感器 %q 读数 %.2f 超出允许区间 [%.2f, %.2f]", rule.Sensor, value, rule.Min, rule.Max)
	}
	return check.Pass("传感器 %q 读数 %.2f 在区间 [%.2f, %.2f] 内", rule.Sensor, value, rule.Min, rule.Max)
}
