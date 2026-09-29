package virtual

import (
	"context"
	"fmt"
	"time"

	"device-selfcheck/selfcheck"
)

type Device struct {
	ID            string
	CommOK        bool
	HasSensor     bool
	SensorReading float64
	MinReading    float64
	MaxReading    float64
}

const (
	CheckCommLink       = "comm_link"
	CheckSensorPresence = "sensor_presence"
	CheckSensorReading  = "sensor_reading"
	CheckCalibration    = "calibration"
	CheckHealthSummary  = "health_summary"
)

func CheckSet() selfcheck.CheckSet[Device] {
	return selfcheck.CheckSet[Device]{
		Model: "virtual-v1",
		Checks: []selfcheck.Check[Device]{
			{
				ID:        CheckCommLink,
				Name:      "通信链路",
				Timeout:   100 * time.Millisecond,
				DependsOn: nil,
				Run: func(_ context.Context, device Device) selfcheck.Result {
					if !device.CommOK {
						return selfcheck.Failed(
							"communication_unavailable",
							"device is not reachable",
							map[string]any{"device_id": device.ID},
						)
					}
					return selfcheck.Passed("communication_ok", "device responds normally", map[string]any{"device_id": device.ID})
				},
			},
			{
				ID:        CheckSensorPresence,
				Name:      "传感器在位",
				Timeout:   100 * time.Millisecond,
				DependsOn: []string{CheckCommLink},
				Run: func(_ context.Context, device Device) selfcheck.Result {
					if !device.HasSensor {
						return selfcheck.Failed("sensor_missing", "expected sensor is not present", nil)
					}
					return selfcheck.Passed("sensor_present", "sensor is present", nil)
				},
			},
			{
				ID:        CheckSensorReading,
				Name:      "传感器读数",
				Timeout:   100 * time.Millisecond,
				DependsOn: []string{CheckCommLink, CheckSensorPresence},
				Precondition: func(device Device) (bool, string) {
					if !device.HasSensor {
						return false, "sensor must be present before reading its value"
					}
					return true, ""
				},
				Run: func(_ context.Context, device Device) selfcheck.Result {
					if device.SensorReading < device.MinReading || device.SensorReading > device.MaxReading {
						return selfcheck.Failed(
							"sensor_reading_out_of_range",
							"sensor communication is normal but reading is outside limits",
							map[string]any{
								"reading": device.SensorReading,
								"min":     device.MinReading,
								"max":     device.MaxReading,
							},
						)
					}
					return selfcheck.Passed("sensor_reading_ok", "reading is within limits", map[string]any{"reading": device.SensorReading})
				},
			},
			{
				ID:        CheckCalibration,
				Name:      "标定有效性",
				Timeout:   100 * time.Millisecond,
				DependsOn: []string{CheckSensorReading},
				Run: func(_ context.Context, device Device) selfcheck.Result {
					midpoint := device.MinReading + (device.MaxReading-device.MinReading)/2
					if device.SensorReading > midpoint {
						return selfcheck.Passed("calibration_ok", "calibration reference is valid", nil)
					}
					return selfcheck.Failed("calibration_bad", "calibration reference is too low", nil)
				},
			},
			{
				ID:        CheckHealthSummary,
				Name:      "健康汇总",
				Timeout:   100 * time.Millisecond,
				DependsOn: []string{CheckCommLink, CheckSensorPresence, CheckSensorReading, CheckCalibration},
				Run: func(_ context.Context, device Device) selfcheck.Result {
					return selfcheck.Passed("healthy", fmt.Sprintf("device %s passed self-check", device.ID), nil)
				},
			},
		},
	}
}
