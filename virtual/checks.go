package virtual

import (
	"context"
	"errors"
	"time"

	"devicecheck/check"
)

// ModelID 是演示虚拟设备的型号标识。
const ModelID = "virt-env-1000"

// 检查项 ID：电源 → 温度（依赖电源）→ 校准（依赖温度）。
const (
	CheckPower = "power"
	CheckTemp  = "temperature"
	CheckCalib = "calibration"
)

// CheckTempProbe 是 temperature 未通过时由条件规则追加的补充检查 ID。
const CheckTempProbe = "temp_probe"

// SensorCheck 把一次虚拟传感器读数封装成一个 check.Check。
// 三类失败严格区分：
//   - 传感器不存在 → Error / sensor_missing；
//   - 读取自身报错 → Error / read_error；
//   - 读数越界     → Failed / out_of_range。
type SensorCheck struct {
	check.FuncCheck
}

// NewSensorCheck 构造传感器检查。thresholdMin/Max 为业务合格区间。
func NewSensorCheck(id, name string, deps []string, timeout time.Duration, sensor string, thresholdMin, thresholdMax float64) *SensorCheck {
	return &SensorCheck{FuncCheck: check.FuncCheck{
		CheckID:   id,
		CheckName: name,
		Requires:  deps,
		Limit:     timeout,
		Fn: func(ctx *check.Context) check.Result {
			dev, _ := ctx.Device.(*Device)
			value, err := dev.ReadSensor(ctx, sensor)
			if err != nil {
				switch {
				case errors.Is(err, ErrSensorMissing):
					return check.Fault("sensor_missing", "传感器 %q 不存在于设备 %s", sensor, dev.ID())
				case errors.Is(err, context.DeadlineExceeded):
					// 由执行器按 timeout 落状态，这里兜底
					return check.Fault("timeout", "读取 %q 超时: %v", sensor, err)
				case errors.Is(err, context.Canceled):
					return check.Fault("canceled", "读取 %q 被取消: %v", sensor, err)
				case errors.Is(err, ErrSensorFault):
					return check.Fault("read_error", "传感器 %q 读取失败: %v", sensor, err)
				default:
					return check.Fault("check_error", "检查 %q 自身报错: %v", sensor, err)
				}
			}
			if value < thresholdMin || value > thresholdMax {
				return check.Fail("out_of_range",
					"传感器 %q 读数 %.1f 超出合格区间 [%.1f, %.1f]", sensor, value, thresholdMin, thresholdMax)
			}
			return check.Pass("传感器 %q 读数 %.1f 在合格区间 [%.1f, %.1f] 内", sensor, value, thresholdMin, thresholdMax)
		},
	}}
}

// NewSet 为演示设备构建检查集：
// temperature 依赖 power；calibration 依赖 temperature。
func NewSet() (*check.CheckSet, error) {
	return check.NewSet(ModelID,
		NewSensorCheck(CheckPower, "电源电压检测", nil, 500*time.Millisecond, "power", 11.0, 13.0),
		NewSensorCheck(CheckTemp, "舱内温度检测", []string{CheckPower}, 500*time.Millisecond, "temp", 10.0, 40.0),
		NewSensorCheck(CheckCalib, "校准一致性检查", []string{CheckTemp}, 500*time.Millisecond, "calib", 0.0, 1.0),
	)
}

// CondRules 返回演示用的条件计划规则：
// temperature 一旦未通过（越界/故障/超时等），即追加“温度探头深度诊断”。
// 补充检查不声明图依赖，其触发依据完整记录在计划项的 Evidence 中，
// 因此修复 temperature 后它不会被自动重跑，旧结论会被标记失效，
// 由现场决定是否单独重跑。
func CondRules() []check.Rule {
	return []check.Rule{{
		Trigger: CheckTemp,
		Code:    "out_of_range",
		Adds: []check.Check{
			NewSensorCheck(CheckTempProbe, "温度探头深度诊断", nil, 500*time.Millisecond, "probe", 0.0, 100.0),
		},
		Reason: "temperature 读数越界，追加探头深度诊断以定位探头本体或安装问题",
	}}
}

// NewDeviceWithSensors 按场景组装演示设备。
func NewDeviceWithSensors(id string, power, temp, calib *Sensor) *Device {
	sensors := map[string]*Sensor{}
	if power != nil {
		sensors["power"] = power
	}
	if temp != nil {
		sensors["temp"] = temp
	}
	if calib != nil {
		sensors["calib"] = calib
	}
	return NewDevice(id, ModelID, sensors)
}
