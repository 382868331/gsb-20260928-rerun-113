// Package virtual 提供用于演示与测试的虚拟自检设备。
//
// 虚拟设备可以模拟三类典型现场情况：
//   - 传感器未安装（ErrSensorMissing）
//   - 读取耗时长（Sleep），用于验证超时、取消与 goroutine 收尾
//   - 恢复（重新安装传感器、消除延迟/故障），用于验证重跑范围
package virtual

import (
	"errors"
	"sync"
	"time"
)

// 传感器读取错误；与“读数越界”区分：读数越界是检查项的业务判定，
// 而这里是设备侧无法给出读数。
var (
	ErrSensorMissing = errors.New("sensor_missing")
	ErrSensorFault   = errors.New("sensor_fault")
)

// Sensor 是一个可配置的虚拟传感器。
type Sensor struct {
	mu        sync.Mutex
	installed bool
	faulty    bool
	value     float64
	delay     time.Duration
	reads     int
}

// NewSensor 安装一只初始读数为 value 的传感器。
func NewSensor(value float64) *Sensor {
	return &Sensor{installed: true, value: value}
}

// Missing 模拟传感器缺失。
func (s *Sensor) Missing() {
	s.mu.Lock()
	s.installed = false
	s.mu.Unlock()
}

// Install 以给定读数重新安装传感器（恢复场景）。
func (s *Sensor) Install(value float64) {
	s.mu.Lock()
	s.installed = true
	s.faulty = false
	s.value = value
	s.mu.Unlock()
}

// SetValue 更新读数。
func (s *Sensor) SetValue(value float64) {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
}

// Fault 让读取返回传感器自身故障（例如总线校验错误）。
func (s *Sensor) Fault() {
	s.mu.Lock()
	s.faulty = true
	s.mu.Unlock()
}

// Slow 让每次读取至少等待 delay；设 0 取消慢响应。读取会响应取消。
func (s *Sensor) Slow(delay time.Duration) {
	s.mu.Lock()
	s.delay = delay
	s.mu.Unlock()
}

// Read 执行一次可取消的读取，返回读数或设备侧错误。
func (s *Sensor) Read(abort <-chan struct{}) (float64, error) {
	s.mu.Lock()
	installed := s.installed
	faulty := s.faulty
	value := s.value
	delay := s.delay
	s.reads++
	s.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-abort:
			return 0, abortErr(abort)
		}
	}
	if !installed {
		return 0, ErrSensorMissing
	}
	if faulty {
		return 0, ErrSensorFault
	}
	return value, nil
}

// Reads 返回累计读取次数。
func (s *Sensor) Reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func abortErr(abort <-chan struct{}) error {
	select {
	case <-abort:
		return contextCanceled
	default:
		return nil
	}
}

var contextCanceled = errors.New("read_canceled")

// Device 是由若干命名传感器组成的虚拟设备。
type Device struct {
	id      string
	model   string
	mu      sync.Mutex
	sensors map[string]*Sensor
	// selftestFault 为 true 时设备自检逻辑自身报错（区别于传感器问题）。
	selftestFault bool
	active        sync.WaitGroup
}

// NewDevice 创建虚拟设备并装入传感器。
func NewDevice(id, model string, sensors map[string]*Sensor) *Device {
	clone := make(map[string]*Sensor, len(sensors))
	for name, s := range sensors {
		clone[name] = s
	}
	return &Device{id: id, model: model, sensors: clone}
}

func (d *Device) ID() string    { return d.id }
func (d *Device) Model() string { return d.model }

// Sensor 按名取传感器。
func (d *Device) Sensor(name string) (*Sensor, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sensors[name]
	return s, ok
}

// BreakSelftest 让设备自检逻辑自身报错。
func (d *Device) BreakSelftest() {
	d.mu.Lock()
	d.selftestFault = true
	d.mu.Unlock()
}

// RepairSelftest 恢复设备自检逻辑。
func (d *Device) RepairSelftest() {
	d.mu.Lock()
	d.selftestFault = false
	d.mu.Unlock()
}

// SelftestFaulted 返回设备自检逻辑是否处于故障状态。
func (d *Device) SelftestFaulted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.selftestFault
}

// readSensor 跟踪一次在途读取，供资源收尾验证使用。
func (d *Device) readSensor(name string, abort <-chan struct{}) (float64, error) {
	s, ok := d.Sensor(name)
	if !ok {
		return 0, ErrSensorMissing
	}
	d.active.Add(1)
	defer d.active.Done()
	return s.Read(abort)
}

// WaitIdle 等待全部在途读取结束（超时/取消后用于验证 goroutine 收尾）。
func (d *Device) WaitIdle() {
	d.active.Wait()
}
